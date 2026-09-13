package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Package ownership (artifact registry tenancy).
//
// The registry keeps ONE global namespace per format (npm-official model):
// names are globally unique, every name belongs to exactly one tenant, and
// visibility is a per-name flag (public by default; private = owning tenant
// only). Rows are created on first publish — the "claim".

// PackageOwner is the ownership record of one registry name.
type PackageOwner struct {
	Format     string
	Repository string
	TenantID   int64
	Visibility string // "public" | "private"
	Created    time.Time
}

// packageOwnerRow is the GORM model. (format, repository) is globally
// unique — that uniqueness IS the global-namespace contract.
type packageOwnerRow struct {
	ID         int64  `gorm:"primaryKey;autoIncrement"`
	Format     string `gorm:"not null;uniqueIndex:idx_pkgowner"`
	Repository string `gorm:"not null;uniqueIndex:idx_pkgowner"`
	TenantID   int64  `gorm:"not null;default:1"`
	Visibility string `gorm:"not null;default:'public'"`
	Created    int64  `gorm:"not null"`
}

func (packageOwnerRow) TableName() string { return "package_owners" }

// ErrPackageOwned is returned when another tenant owns the name.
var ErrPackageOwned = errors.New("store: package belongs to another tenant")

// ErrScopeNotYours is returned when the npm-style scope (@org/…) has no org
// evidence in the caller's tenant.
var ErrScopeNotYours = errors.New("store: scope does not belong to your tenant (create the org/repo first)")

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
	return &PackageOwner{Format: row.Format, Repository: row.Repository, TenantID: row.TenantID, Visibility: row.Visibility, Created: time.UnixMilli(row.Created)}, nil
}

// ClaimPackage records tenant as the owner of a previously unclaimed name.
// Returns ErrPackageOwned when the name is already owned by someone else.
func (s *CentralStore) ClaimPackage(format, repository string, tid int64) error {
	row := &packageOwnerRow{Format: format, Repository: repository, TenantID: tid, Visibility: "public", Created: time.Now().UTC().UnixMilli()}
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
		if existing.TenantID != tid {
			return fmt.Errorf("%w: %s/%s", ErrPackageOwned, format, repository)
		}
	}
	return nil
}

// SetPackageVisibility flips a name's visibility (owning tenant only).
func (s *CentralStore) SetPackageVisibility(format, repository string, tid int64, visibility string) error {
	if visibility != "public" && visibility != "private" {
		return fmt.Errorf("invalid visibility %q", visibility)
	}
	res := s.d.gdb.Model(&packageOwnerRow{}).
		Where("format=? AND repository=? AND tenant_id=?", format, repository, tid).
		Update("visibility", visibility)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// TenantHasNamespace reports whether a tenant has evidence of owning an org
// namespace: at least one repository (or membership row) under that name in
// the tenant. This grounds npm scope (and OCI namespace) ownership in the
// easyvcs org model — you can only publish @org/... when your tenant really
// has that org.
func (s *CentralStore) TenantHasNamespace(tid int64, namespace string) bool {
	var n int64
	s.d.gdb.Model(&repoRow{}).Where("tenant_id=? AND namespace=?", tid, namespace).Count(&n)
	if n > 0 {
		return true
	}
	s.d.gdb.Model(&namespaceMemberRow{}).Where("tenant_id=? AND namespace=?", tid, namespace).Count(&n)
	return n > 0
}

// AuthorizePublish implements the registry Ownership contract: the caller's
// tenant may publish (format, repository) when it already owns the name, or
// when the name is unclaimed AND the name's namespace part belongs to the
// tenant (npm @scope or OCI namespace mapped onto easyvcs orgs).
func (s *CentralStore) AuthorizePublish(ctx context.Context, format, repository string, tenantID int64) error {
	if tenantID == 0 {
		tenantID = 1
	}
	own, err := s.GetPackageOwner(format, repository)
	if err == nil {
		if own.TenantID == tenantID {
			return nil
		}
		return fmt.Errorf("%w: %s/%s", ErrPackageOwned, format, repository)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	// Unclaimed: the name's namespace part must belong to the tenant.
	ns := NamespaceOfName(format, repository)
	if ns == "" {
		return fmt.Errorf("%w: %s/%s has no namespace", ErrScopeNotYours, format, repository)
	}
	if !s.TenantHasNamespace(tenantID, ns) {
		return fmt.Errorf("%w: %q", ErrScopeNotYours, ns)
	}
	return s.ClaimPackage(format, repository, tenantID)
}

// CanRead implements the registry Ownership contract: public and unclaimed
// names are readable by everyone; private names only by the owning tenant.
func (s *CentralStore) CanRead(ctx context.Context, format, repository string, tenantID int64) bool {
	if tenantID == 0 {
		tenantID = 1
	}
	own, err := s.GetPackageOwner(format, repository)
	if err != nil {
		// Unclaimed (proxied upstream packages): public.
		return true
	}
	if own.Visibility != "private" {
		return true
	}
	return own.TenantID == tenantID
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
