package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Package ownership (artifact registry).
//
// The registry keeps ONE global namespace per format (npm-official model):
// names are globally unique, every name belongs to exactly one user, and
// visibility is a per-name flag (public by default; private = owning user
// only). Rows are created on first publish — the "claim".

// PackageOwner is the ownership record of one registry name.
type PackageOwner struct {
	Format      string
	Repository  string
	OwnerUserID int64
	Visibility  string // "public" | "private"
	Created     time.Time
}

// ErrPackageOwned is returned when another user owns the name.
var ErrPackageOwned = errors.New("store: package belongs to another user")

// ErrScopeNotYours is returned when the npm-style scope (@org/…) has no org
// evidence in the caller's namespaces.
var ErrScopeNotYours = errors.New("store: scope does not belong to you (create the namespace/repo first)")

// GetPackageOwner returns the ownership record of a name, or ErrNotFound.
func (s *CentralStore) GetPackageOwner(format, repository string) (*PackageOwner, error) {
	var row packageOwnerRow
	err := s.d.gdb.Where("format=? AND repository=?", format, repository).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &PackageOwner{Format: row.Format, Repository: row.Repository, OwnerUserID: row.OwnerUserID, Visibility: row.Visibility, Created: time.UnixMilli(row.Created)}, nil
}

// ClaimPackage records userID as the owner of a previously unclaimed name.
// Returns ErrPackageOwned when the name is already owned by someone else.
func (s *CentralStore) ClaimPackage(format, repository string, userID int64) error {
	row := &packageOwnerRow{Format: format, Repository: repository, OwnerUserID: userID, Visibility: "public", Created: time.Now().UTC().UnixMilli()}
	res := s.d.gdb.Where("format=? AND repository=?", format, repository).FirstOrCreate(row)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// Found an existing row: only legitimate when it is already ours
		// (idempotent re-claim).
		existing, err := s.GetPackageOwner(format, repository)
		if err != nil {
			return err
		}
		if existing.OwnerUserID != userID {
			return fmt.Errorf("%w: %s/%s", ErrPackageOwned, format, repository)
		}
	}
	return nil
}

// SetPackageVisibility flips a name's visibility (owning user only).
func (s *CentralStore) SetPackageVisibility(format, repository string, userID int64, visibility string) error {
	if visibility != "public" && visibility != "private" {
		return fmt.Errorf("invalid visibility %q", visibility)
	}
	res := s.d.gdb.Model(&packageOwnerRow{}).
		Where("format=? AND repository=? AND owner_user_id=?", format, repository, userID).
		Update("visibility", visibility)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPackageVisibilityAuthorized flips a name's visibility after checking the
// caller holds maintainer+ on the name's scope (mapped repo, else owning user).
// It is the gateway entry point; the raw owner-only form remains for internal
// callers.
func (s *CentralStore) SetPackageVisibilityAuthorized(format, repository string, userID int64, visibility string) error {
	if visibility != "public" && visibility != "private" {
		return fmt.Errorf("invalid visibility %q", visibility)
	}
	role, err := s.PackageScopeRole(format, repository, userID)
	if err != nil {
		return err
	}
	if !role.CanMerge() {
		return fmt.Errorf("%w: %s/%s needs maintainer", ErrPackageOwned, format, repository)
	}
	res := s.d.gdb.Model(&packageOwnerRow{}).
		Where("format=? AND repository=?", format, repository).
		Update("visibility", visibility)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// RepoForPackage maps a registry name to the easyvcs repository that owns it,
// when one exists. npm "@acme/ui" -> acme/ui; OCI/generic "acme/api" -> acme/api;
// flat names map to no repo. The bool reports whether a repo was found.
func (s *CentralStore) RepoForPackage(format, repository string) (RepoRef, bool) {
	ns := NamespaceOfName(format, repository)
	if ns == "" {
		return RepoRef{}, false
	}
	leaf := packageLeaf(format, repository)
	if leaf == "" {
		return RepoRef{}, false
	}
	ref := RepoRef{Namespace: ns, Name: leaf}
	if _, err := s.OpenRepo(ref); err != nil {
		return RepoRef{}, false
	}
	return ref, true
}

// PackageScopeRole resolves the effective role of userID for a registry name,
// mirroring repository roles: when the name maps to an easyvcs repository the
// repo role applies (owner / maintainer / developer); otherwise the name's
// owner user is "owner" and a public/unclaimed name is readable by everyone.
// An unclaimed name (e.g. a pull-through cache entry) grants developer to all
// and no management rights.
func (s *CentralStore) PackageScopeRole(format, repository string, userID int64) (Role, error) {
	own, oerr := s.GetPackageOwner(format, repository)
	if oerr == nil && own.Visibility == "private" {
		// A private package is readable only by its scope members: the mapped
		// repo's members/owner, else the owning user. No public fallback.
		if ref, ok := s.RepoForPackage(format, repository); ok {
			return s.MemberRoleOfRef(ref, userID)
		}
		return RoleForOwner(own.OwnerUserID, "private", userID), nil
	}
	// Public or unclaimed: repo roles (with public developer fallback), else
	// public-read.
	if ref, ok := s.RepoForPackage(format, repository); ok {
		return s.RoleOfRef(ref, userID)
	}
	if oerr == nil {
		return RoleForOwner(own.OwnerUserID, own.Visibility, userID), nil
	}
	if !errors.Is(oerr, ErrNotFound) {
		return RoleNone, oerr
	}
	return Role(RoleDeveloper), nil
}

// packageLeaf extracts the leaf name of a registry name: npm "@acme/ui" -> "ui",
// OCI/generic "acme/api" -> "api". Flat names -> the name itself.
func packageLeaf(format, repository string) string {
	if i := indexByte(repository, '/'); i >= 0 {
		return repository[i+1:]
	}
	return repository
}

// AuthorizePublish implements the registry Ownership contract, aligned with
// repository roles: a claimed name requires maintainer+ on the name's scope
// (the mapped repository, else the owning user/namespace); an unclaimed name
// requires namespace evidence (the caller owns the scope) so it can be claimed.
func (s *CentralStore) AuthorizePublish(ctx context.Context, format, repository string, userID int64) error {
	own, err := s.GetPackageOwner(format, repository)
	if err == nil {
		role, rerr := s.PackageScopeRole(format, repository, userID)
		if rerr != nil {
			// No repo, unknown owner row: fall back to direct ownership.
			if own.OwnerUserID == userID {
				return nil
			}
			return fmt.Errorf("%w: %s/%s", ErrPackageOwned, format, repository)
		}
		if role.CanMerge() {
			return nil
		}
		// A developer (or a public non-owner) may not publish over a claim.
		return fmt.Errorf("%w: %s/%s needs maintainer", ErrPackageOwned, format, repository)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	// Unclaimed: the name's namespace part must belong to the user (via the
	// mapped repo — the caller must be at least a maintainer — or, when no repo
	// exists, a namespace the user owns).
	ns := NamespaceOfName(format, repository)
	if ns == "" {
		return fmt.Errorf("%w: %s/%s has no namespace", ErrScopeNotYours, format, repository)
	}
	if ref, ok := s.RepoForPackage(format, repository); ok {
		role, rerr := s.RoleOfRef(ref, userID)
		if rerr == nil && role.CanMerge() {
			return s.ClaimPackage(format, repository, userID)
		}
		return fmt.Errorf("%w: %q (maintainer required on %s/%s)", ErrScopeNotYours, ns, ref.Namespace, ref.Name)
	}
	if !s.TenantHasNamespace(userID, ns) {
		return fmt.Errorf("%w: %q", ErrScopeNotYours, ns)
	}
	return s.ClaimPackage(format, repository, userID)
}

// CanRead implements the registry Ownership contract: public and unclaimed
// names are readable by everyone; a private name requires a role on its scope
// (mapped repo or owning user).
func (s *CentralStore) CanRead(ctx context.Context, format, repository string, userID int64) bool {
	role, err := s.PackageScopeRole(format, repository, userID)
	if err != nil {
		return false
	}
	return role.CanRead()
}

// NamespaceOfName extracts the ownership namespace of a registry name per
// its native protocol convention: npm "@org/name" → "org"; OCI and generic
// "a/b/..." paths → the first path segment; flat names → "" (no namespace —
// such protocols cannot be claimed under org evidence and rely on the
// protocol adapter requiring a namespace).
func NamespaceOfName(format, repository string) string {
	if repository == "" {
		return ""
	}
	switch format {
	case "npm":
		if repository[0] == '@' {
			if i := indexByte(repository, '/'); i > 1 {
				return repository[1:i]
			}
		}
		return ""
	default:
		if i := indexByte(repository, '/'); i > 0 {
			return repository[:i]
		}
		return ""
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
