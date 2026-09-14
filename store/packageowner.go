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

// AuthorizePublish implements the registry Ownership contract: a user may
// publish (format, repository) when it already owns the name, or when the name
// is unclaimed AND the name's namespace part is one the user owns (npm @scope
// or OCI namespace mapped onto easyvcs namespaces).
func (s *CentralStore) AuthorizePublish(ctx context.Context, format, repository string, userID int64) error {
	own, err := s.GetPackageOwner(format, repository)
	if err == nil {
		if own.OwnerUserID == userID {
			return nil
		}
		return fmt.Errorf("%w: %s/%s", ErrPackageOwned, format, repository)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	// Unclaimed: the name's namespace part must belong to the user.
	ns := NamespaceOfName(format, repository)
	if ns == "" {
		return fmt.Errorf("%w: %s/%s has no namespace", ErrScopeNotYours, format, repository)
	}
	if !s.TenantHasNamespace(userID, ns) {
		return fmt.Errorf("%w: %q", ErrScopeNotYours, ns)
	}
	return s.ClaimPackage(format, repository, userID)
}

// CanRead implements the registry Ownership contract: public and unclaimed
// names are readable by everyone; private names only by the owning user.
func (s *CentralStore) CanRead(ctx context.Context, format, repository string, userID int64) bool {
	own, err := s.GetPackageOwner(format, repository)
	if err != nil {
		// Unclaimed (proxied upstream packages): public.
		return true
	}
	if own.Visibility != "private" {
		return true
	}
	return own.OwnerUserID == userID
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
