package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// User is a Lab account. A user owns namespaces (orgs) and repositories.
// Users are 1:1 with tenants: TenantID pins every credential to exactly one
// isolation boundary (0 = default tenant 1 for legacy rows).
type User struct {
	ID          int64
	TenantID    int64
	Username    string
	DisplayName string
	Created     time.Time
}

// Tenant is the top-level isolation boundary (orgs, repos, users, and the
// matching agent tenant all belong to one).
type Tenant struct {
	ID          int64
	Slug        string
	DisplayName string
	Disabled    bool
	Created     time.Time
}

// CreateTenant inserts a tenant. The slug must be unique.
func (s *CentralStore) CreateTenant(slug, displayName string) (*Tenant, error) {
	now := time.Now().UTC().UnixMilli()
	row := &tenantRow{Slug: slug, DisplayName: displayName, Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &Tenant{ID: row.ID, Slug: slug, DisplayName: displayName, Created: time.UnixMilli(now)}, nil
}

// GetTenant returns a tenant by id.
func (s *CentralStore) GetTenant(id int64) (*Tenant, error) {
	var row tenantRow
	err := s.d.gdb.Where("id=?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Tenant{ID: row.ID, Slug: row.Slug, DisplayName: row.DisplayName, Disabled: row.Disabled, Created: time.UnixMilli(row.Created)}, nil
}

// GetTenantBySlug returns a tenant by slug.
func (s *CentralStore) GetTenantBySlug(slug string) (*Tenant, error) {
	var row tenantRow
	err := s.d.gdb.Where("slug=?", slug).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Tenant{ID: row.ID, Slug: row.Slug, DisplayName: row.DisplayName, Disabled: row.Disabled, Created: time.UnixMilli(row.Created)}, nil
}

// AgentToken returns the tenant's agent credential ("" when unset).
func (s *CentralStore) AgentToken(tenantID int64) (string, error) {
	var row tenantRow
	if err := s.d.gdb.Select("agent_token").First(&row, tenantID).Error; err != nil {
		return "", err
	}
	return row.AgentToken, nil
}

// SetAgentToken stores the tenant's agent credential.
func (s *CentralStore) SetAgentToken(tenantID int64, token string) error {
	return s.d.gdb.Model(&tenantRow{}).Where("id=?", tenantID).Update("agent_token", token).Error
}

// ListTenants returns all tenants ordered by slug.
func (s *CentralStore) ListTenants() ([]*Tenant, error) {
	var rows []tenantRow
	if err := s.d.gdb.Order("slug").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Tenant, 0, len(rows))
	for i := range rows {
		out = append(out, &Tenant{ID: rows[i].ID, Slug: rows[i].Slug, DisplayName: rows[i].DisplayName, Disabled: rows[i].Disabled, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}

// ErrUsernameTaken is returned when creating a user that already exists.
var ErrUsernameTaken = errors.New("store: username already exists")

// ErrTokenNotFound is returned when a token is unknown.
var ErrTokenNotFound = errors.New("store: token not found")

// CreateUser inserts a new user. Creating the first user closes the instance
// (anonymous access stops being allowed). TenantID 0 pins the user to the
// default tenant.
func (s *CentralStore) CreateUser(username, displayName string) (*User, error) {
	return s.CreateUserTenant(0, username, displayName)
}

// CreateUserTenant is the tenant-explicit form.
func (s *CentralStore) CreateUserTenant(tid int64, username, displayName string) (*User, error) {
	if tid == 0 {
		tid = 1
	}
	now := time.Now().UTC().UnixMilli()
	row := &userRow{TenantID: tid, Username: username, DisplayName: displayName, Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	s.invalidateOpenCache()
	return &User{ID: row.ID, TenantID: tid, Username: username, DisplayName: displayName, Created: time.UnixMilli(now)}, nil
}

// GetUser returns a user by id.
func (s *CentralStore) GetUser(id int64) (*User, error) {
	var row userRow
	err := s.d.gdb.Where("id=?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &User{ID: row.ID, TenantID: row.TenantID, Username: row.Username, DisplayName: row.DisplayName, Created: time.UnixMilli(row.Created)}, nil
}

// GetUserByUsername returns a user by username.
func (s *CentralStore) GetUserByUsername(username string) (*User, error) {
	var row userRow
	err := s.d.gdb.Where("username=?", username).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &User{ID: row.ID, TenantID: row.TenantID, Username: row.Username, DisplayName: row.DisplayName, Created: time.UnixMilli(row.Created)}, nil
}

// ListUsers returns all users ordered by username.
func (s *CentralStore) ListUsers() ([]*User, error) {
	var rows []userRow
	if err := s.d.gdb.Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*User, 0, len(rows))
	for i := range rows {
		out = append(out, &User{ID: rows[i].ID, TenantID: rows[i].TenantID, Username: rows[i].Username, DisplayName: rows[i].DisplayName, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}
