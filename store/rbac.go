package store

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// gormConflictUpdateAll is the ON CONFLICT DO UPDATE clause used by upserts.
func gormConflictUpdateAll() clause.OnConflict {
	return clause.OnConflict{UpdateAll: true}
}

// Role is a user's effective role on a repository. It resolves the namespace
// owner to "owner"; explicit collaborator grants map to "maintainer" /
// "developer"; a public repository grants "developer" to everyone (including
// anonymous readers); otherwise the user has no role.
type Role string

// Effective repository roles.
const (
	RoleOwner Role = "owner"
	RoleNone  Role = ""
)

// rank orders roles for "at least this role" comparisons. -1 = none.
func roleRank(r Role) int {
	switch r {
	case RoleOwner:
		return 3
	case Role(RoleMaintainer):
		return 2
	case Role(RoleDeveloper):
		return 1
	default:
		return -1
	}
}

// AtLeast reports whether r grants at least the privileges of min.
func (r Role) AtLeast(min Role) bool {
	return roleRank(r) >= 0 && roleRank(r) >= roleRank(min)
}

// RoleOf resolves the effective role of userID on a repository. userID 0 is an
// anonymous caller. A public repository grants "developer" to everyone as a
// fallback (read/propose only).
func (s *CentralStore) RoleOf(repoID, userID int64) (Role, error) {
	var row repoRow
	if err := s.d.gdb.Where("id=?", repoID).First(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return RoleNone, ErrNotFound
		}
		return RoleNone, err
	}
	if userID != 0 && userID == row.OwnerUserID {
		return RoleOwner, nil
	}
	if userID != 0 {
		memberRole, err := s.RepoMemberRole(repoID, userID)
		if err != nil {
			return RoleNone, err
		}
		switch memberRole {
		case RoleMaintainer:
			return Role(RoleMaintainer), nil
		case RoleDeveloper:
			return Role(RoleDeveloper), nil
		}
	}
	if row.Visibility != "private" {
		// Public repo: everyone (including anonymous) gets developer-equivalent
		// read / propose access.
		return Role(RoleDeveloper), nil
	}
	return RoleNone, nil
}

// MemberRoleOf is RoleOf WITHOUT the public fallback: it returns a role only
// when the user is the owner or holds an explicit repo_members grant. It is
// used where the public fallback must not leak access (private registry
// packages whose name maps to a public repo).
func (s *CentralStore) MemberRoleOf(repoID, userID int64) (Role, error) {
	var row repoRow
	if err := s.d.gdb.Where("id=?", repoID).First(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return RoleNone, ErrNotFound
		}
		return RoleNone, err
	}
	if userID != 0 && userID == row.OwnerUserID {
		return RoleOwner, nil
	}
	if userID != 0 {
		memberRole, err := s.RepoMemberRole(repoID, userID)
		if err != nil {
			return RoleNone, err
		}
		switch memberRole {
		case RoleMaintainer:
			return Role(RoleMaintainer), nil
		case RoleDeveloper:
			return Role(RoleDeveloper), nil
		}
	}
	return RoleNone, nil
}

// RoleOfRef resolves the effective role of userID on the repo named by ref.
func (s *CentralStore) RoleOfRef(ref RepoRef, userID int64) (Role, error) {
	repo, err := s.OpenRepo(ref)
	if err != nil {
		return RoleNone, err
	}
	return s.RoleOf(repo.repoID, userID)
}

// MemberRoleOfRef resolves the role of userID on a repo without the public
// fallback (owner or explicit repo_members grant only).
func (s *CentralStore) MemberRoleOfRef(ref RepoRef, userID int64) (Role, error) {
	repo, err := s.OpenRepo(ref)
	if err != nil {
		return RoleNone, err
	}
	return s.MemberRoleOf(repo.repoID, userID)
}

// RoleForOwner resolves the role of userID for a resource described only by its
// owner + visibility (no repository row): the owner is "owner", a non-private
// (public) resource grants "developer" to everyone, and a private resource with
// no owner match grants nothing. It is the fallback for registry packages whose
// name maps to no easyvcs repository.
func RoleForOwner(ownerUserID int64, visibility string, userID int64) Role {
	if userID != 0 && userID == ownerUserID {
		return RoleOwner
	}
	if visibility != "private" {
		return Role(RoleDeveloper)
	}
	return RoleNone
}

// CanPush reports whether the role may directly push/set refs (owner only).
func (r Role) CanPush() bool { return r == RoleOwner }

// CanMerge reports whether the role may approve/merge change requests and
// manage tags (owner or maintainer).
func (r Role) CanMerge() bool { return r == RoleOwner || r == Role(RoleMaintainer) }

// CanPropose reports whether the role may open change requests / fork
// (developer and above; anonymous resolves to developer on public repos, so
// callers still require authentication separately for propose actions).
func (r Role) CanPropose() bool { return r.AtLeast(Role(RoleDeveloper)) }

// CanRead reports whether the role may read the repository.
func (r Role) CanRead() bool { return roleRank(r) >= 1 }

// CanOwnSession reports whether the role may drive repository-bound agent
// sessions (owner only).
func (r Role) CanOwnSession() bool { return r == RoleOwner }
