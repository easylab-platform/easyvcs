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
	// AgentTenant / AgentToken are the explicit binding to the abcp-agent
	// tenant (see the row comment in models.go).
	AgentTenant string
	AgentToken  string
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
	return tenantFromRow(&row), nil
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
	return tenantFromRow(&row), nil
}

func tenantFromRow(row *tenantRow) *Tenant {
	return &Tenant{
		ID: row.ID, Slug: row.Slug, DisplayName: row.DisplayName,
		Disabled: row.Disabled, Created: time.UnixMilli(row.Created),
		AgentTenant: row.AgentTenant, AgentToken: row.AgentToken,
	}
}

// AgentToken returns the tenant's agent credential ("" when unset).
func (s *CentralStore) AgentToken(tenantID int64) (string, error) {
	var row tenantRow
	if err := s.d.gdb.Select("agent_token").First(&row, tenantID).Error; err != nil {
		return "", err
	}
	return row.AgentToken, nil
}

// AgentBinding returns the tenant's bound agent tenant id + credential.
func (s *CentralStore) AgentBinding(tenantID int64) (agentTenant, agentToken string, err error) {
	var row tenantRow
	if err := s.d.gdb.Select("agent_tenant", "agent_token").First(&row, tenantID).Error; err != nil {
		return "", "", err
	}
	return row.AgentTenant, row.AgentToken, nil
}

// SetAgentBinding stores the tenant's bound agent tenant id + credential
// (empty values are ignored, so a partial update never clears the other).
func (s *CentralStore) SetAgentBinding(tenantID int64, agentTenant, agentToken string) error {
	updates := map[string]any{}
	if agentTenant != "" {
		updates["agent_tenant"] = agentTenant
	}
	if agentToken != "" {
		updates["agent_token"] = agentToken
	}
	if len(updates) == 0 {
		return nil
	}
	return s.d.gdb.Model(&tenantRow{}).Where("id=?", tenantID).Updates(updates).Error
}

// UpdateTenant patches display name / disabled state.
func (s *CentralStore) UpdateTenant(id int64, displayName *string, disabled *bool) (*Tenant, error) {
	updates := map[string]any{}
	if displayName != nil {
		updates["display_name"] = *displayName
	}
	if disabled != nil {
		updates["disabled"] = *disabled
	}
	if len(updates) > 0 {
		if err := s.d.gdb.Model(&tenantRow{}).Where("id=?", id).Updates(updates).Error; err != nil {
			return nil, err
		}
	}
	return s.GetTenant(id)
}

// ListUsersByTenant returns every user of one tenant, ordered by username.
func (s *CentralStore) ListUsersByTenant(tid int64) ([]*User, error) {
	var rows []userRow
	if err := s.d.gdb.Where("tenant_id=?", tid).Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*User, 0, len(rows))
	for i := range rows {
		out = append(out, &User{ID: rows[i].ID, TenantID: rows[i].TenantID, Username: rows[i].Username, DisplayName: rows[i].DisplayName, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}

// StrongestRoleOfUser returns the highest role a user holds in any namespace
// of their tenant ("" when they hold none). Used as the tenant-level role
// projection while tenant members are modeled via namespace membership.
func (s *CentralStore) StrongestRoleOfUser(userID int64) string {
	var rows []namespaceMemberRow
	if err := s.d.gdb.Where("user_id=?", userID).Find(&rows).Error; err != nil {
		return ""
	}
	rank := map[string]int{RoleReadonly: 1, RoleMember: 2, RoleAdmin: 3, RoleOwner: 4}
	best, bestRank := "", 0
	for _, r := range rows {
		if rank[r.Role] > bestRank {
			best, bestRank = r.Role, rank[r.Role]
		}
	}
	return best
}

// ListTenants returns all tenants ordered by slug.
func (s *CentralStore) ListTenants() ([]*Tenant, error) {
	var rows []tenantRow
	if err := s.d.gdb.Order("slug").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Tenant, 0, len(rows))
	for i := range rows {
		out = append(out, tenantFromRow(&rows[i]))
	}
	return out, nil
}

// DeleteTenant removes a tenant and every row scoped to it: its repositories
// (with their metadata), users, tokens, namespace memberships, and package
// ownership claims. The default tenant (id 1) is protected. The caller is
// responsible for cascading to the agent (AdminService.DeleteTenant).
func (s *CentralStore) DeleteTenant(id int64) error {
	if id == 0 || id == 1 {
		return errors.New("store: the default tenant cannot be deleted")
	}
	t, err := s.GetTenant(id)
	if err != nil {
		return err
	}
	refs, err := s.ListForTenant(t.ID)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if err := s.Delete(r); err != nil {
			return err
		}
	}
	var users []userRow
	if err := s.d.gdb.Where("tenant_id=?", id).Find(&users).Error; err != nil {
		return err
	}
	for _, u := range users {
		if err := s.d.gdb.Where("user_id=?", u.ID).Delete(&tokenRow{}).Error; err != nil {
			return err
		}
	}
	if err := s.d.gdb.Where("tenant_id=?", id).Delete(&namespaceMemberRow{}).Error; err != nil {
		return err
	}
	if err := s.d.gdb.Where("tenant_id=?", id).Delete(&userRow{}).Error; err != nil {
		return err
	}
	if err := s.d.gdb.Where("tenant_id=?", id).Delete(&packageOwnerRow{}).Error; err != nil {
		return err
	}
	return s.d.gdb.Delete(&tenantRow{}, id).Error
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
