package store

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// NamespaceMember is a user's membership in a namespace (org).
type NamespaceMember struct {
	Namespace string
	UserID    int64
	Role      string // "readonly" | "member" | "admin" | "owner"
}

// Namespace member roles. Write access requires member/admin/owner; readonly
// grants read only.
const (
	RoleReadonly = "readonly"
	RoleMember   = "member"
	RoleAdmin    = "admin"
	RoleOwner    = "owner"
)

// validRole reports whether role is one of the four member roles.
func validRole(role string) bool {
	switch role {
	case RoleReadonly, RoleMember, RoleAdmin, RoleOwner:
		return true
	}
	return false
}

// AddNamespaceMember grants a user membership in a namespace with a role.
func (s *CentralStore) AddNamespaceMember(namespace string, userID int64, role string) error {
	if !validRole(role) {
		return fmt.Errorf("invalid role %q (readonly|member|admin|owner)", role)
	}
	row := &namespaceMemberRow{Namespace: namespace, UserID: userID, Role: role}
	return s.d.gdb.Clauses(clause.OnConflict{UpdateAll: true}).Create(row).Error
}

// RemoveNamespaceMember revokes a user's membership in a namespace.
func (s *CentralStore) RemoveNamespaceMember(namespace string, userID int64) error {
	return s.d.gdb.Where("namespace=? AND user_id=?", namespace, userID).Delete(&namespaceMemberRow{}).Error
}

// GetNamespaceMember returns a user's membership in a namespace.
func (s *CentralStore) GetNamespaceMember(namespace string, userID int64) (*NamespaceMember, error) {
	var row namespaceMemberRow
	err := s.d.gdb.Where("namespace=? AND user_id=?", namespace, userID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &NamespaceMember{Namespace: row.Namespace, UserID: row.UserID, Role: row.Role}, nil
}

// ListNamespaceMembers lists members of a namespace.
func (s *CentralStore) ListNamespaceMembers(namespace string) ([]*NamespaceMember, error) {
	var rows []namespaceMemberRow
	if err := s.d.gdb.Where("namespace=?", namespace).Order("user_id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*NamespaceMember, 0, len(rows))
	for i := range rows {
		out = append(out, &NamespaceMember{Namespace: rows[i].Namespace, UserID: rows[i].UserID, Role: rows[i].Role})
	}
	return out, nil
}

// ListUserNamespaces returns the namespaces a user belongs to.
func (s *CentralStore) ListUserNamespaces(userID int64) ([]*NamespaceMember, error) {
	var rows []namespaceMemberRow
	if err := s.d.gdb.Where("user_id=?", userID).Order("namespace").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*NamespaceMember, 0, len(rows))
	for i := range rows {
		out = append(out, &NamespaceMember{Namespace: rows[i].Namespace, UserID: rows[i].UserID, Role: rows[i].Role})
	}
	return out, nil
}

// IsNamespaceMember reports whether a user belongs to a namespace, regardless of
// role. The user has read access to that namespace's private repositories.
func (s *CentralStore) IsNamespaceMember(namespace string, userID int64) bool {
	var count int64
	s.d.gdb.Model(&namespaceMemberRow{}).Where("namespace=? AND user_id=?", namespace, userID).Count(&count)
	return count > 0
}

// UserCanReadRepo reports whether a user may read a repository. Public
// repositories are always readable; private repositories require namespace
// membership. When no user is supplied the repository is readable only if it is
// public (or the Lab has no users at all, i.e. an open single-user instance).
func (s *CentralStore) UserCanReadRepo(r RepoRef, userID *int64) bool {
	repo, err := s.OpenRepo(r)
	if err != nil {
		return false
	}
	meta, err := repo.RepoMeta()
	if err != nil {
		return false
	}
	if meta.Visibility != "private" {
		return true
	}
	if userID == nil {
		// No authenticated user: private repos are only visible when the entire
		// instance is a single-user open setup (no registered users).
		if s.instanceIsOpen() {
			return true
		}
		return false
	}
	return s.IsNamespaceMember(r.Namespace, *userID)
}

// instanceIsOpen reports whether no users are registered, i.e. a fresh
// single-user fixture where there is nothing to hide and no auth to gate on.
// Callers use it to keep dev flows and unit tests frictionless. The result is
// cached and invalidated whenever a user is created, so per-request ACL checks
// don't rescan the users table.
func (s *CentralStore) instanceIsOpen() bool {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if s.openKnown {
		return s.openValue
	}
	users, err := s.ListUsers()
	if err != nil {
		return false
	}
	s.openValue = len(users) == 0
	s.openKnown = true
	return s.openValue
}

// invalidateOpenCache drops the cached instanceIsOpen result (called whenever
// a user is created, which is the only transition from open to closed).
func (s *CentralStore) invalidateOpenCache() {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	s.openKnown = false
}

// IsOpenInstance is the exported form of instanceIsOpen for callers outside the
// store package (e.g. easyvcs-server). It reports whether the instance has no
// registered users, in which case anonymous access is allowed (CLI default).
func (s *CentralStore) IsOpenInstance() bool { return s.instanceIsOpen() }

// UserCanWriteRepo reports whether a user may write to a repository (push).
// Write access requires the instance to be open OR the user to be a namespace
// member with a write-capable role (member/admin/owner; readonly is denied).
// A nil userID is only allowed when the instance is open.
func (s *CentralStore) UserCanWriteRepo(r RepoRef, userID *int64) bool {
	if s.instanceIsOpen() {
		return true
	}
	if userID == nil {
		return false
	}
	m, err := s.GetNamespaceMember(r.Namespace, *userID)
	if err != nil {
		return false
	}
	return m.Role != RoleReadonly
}
