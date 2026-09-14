package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// User is both the identity and the ownership boundary. There is no separate
// tenant: a user owns namespaces (and therefore repositories), holds tokens,
// and is bound to one abcp-agent tenant. Username is globally unique.
type User struct {
	ID          int64
	Username    string
	DisplayName string
	Disabled    bool
	// AgentTenant / AgentToken are the explicit binding to the abcp-agent
	// tenant (see the row comment in models.go).
	AgentTenant string
	AgentToken  string
	Created     time.Time
}

// ErrUsernameTaken is returned when creating a user that already exists.
var ErrUsernameTaken = errors.New("store: username already exists")

// ErrTokenNotFound is returned when a token is unknown.
var ErrTokenNotFound = errors.New("store: token not found")

// CreateUser inserts a new user. Creating the first user closes the instance
// (anonymous access stops being allowed).
func (s *CentralStore) CreateUser(username, displayName string) (*User, error) {
	now := time.Now().UTC().UnixMilli()
	row := &userRow{Username: username, DisplayName: displayName, Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	s.invalidateOpenCache()
	return userFromRow(row), nil
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
	return userFromRow(&row), nil
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
	return userFromRow(&row), nil
}

// ListUsers returns all users ordered by username.
func (s *CentralStore) ListUsers() ([]*User, error) {
	var rows []userRow
	if err := s.d.gdb.Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*User, 0, len(rows))
	for i := range rows {
		out = append(out, userFromRow(&rows[i]))
	}
	return out, nil
}

// UpdateUser patches display name / disabled state.
func (s *CentralStore) UpdateUser(id int64, displayName *string, disabled *bool) (*User, error) {
	updates := map[string]any{}
	if displayName != nil {
		updates["display_name"] = *displayName
	}
	if disabled != nil {
		updates["disabled"] = *disabled
	}
	if len(updates) > 0 {
		if err := s.d.gdb.Model(&userRow{}).Where("id=?", id).Updates(updates).Error; err != nil {
			return nil, err
		}
	}
	return s.GetUser(id)
}

// DeleteUser removes a user and every row owned by it: namespaces, repositories
// (with their metadata), tokens, repo memberships, and package ownership claims.
func (s *CentralStore) DeleteUser(id int64) error {
	if _, err := s.GetUser(id); err != nil {
		return err
	}
	refs, err := s.ListForOwner(id)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if err := s.Delete(r); err != nil {
			return err
		}
	}
	// Repo member grants this user holds on OTHER repos.
	if err := s.d.gdb.Where("user_id=?", id).Delete(&repoMemberRow{}).Error; err != nil {
		return err
	}
	if err := s.d.gdb.Where("user_id=?", id).Delete(&tokenRow{}).Error; err != nil {
		return err
	}
	if err := s.d.gdb.Where("owner_user_id=?", id).Delete(&namespaceRow{}).Error; err != nil {
		return err
	}
	if err := s.d.gdb.Where("owner_user_id=?", id).Delete(&packageOwnerRow{}).Error; err != nil {
		return err
	}
	return s.d.gdb.Delete(&userRow{}, id).Error
}

// AgentToken returns the user's agent credential ("" when unset).
func (s *CentralStore) AgentToken(userID int64) (string, error) {
	var row userRow
	if err := s.d.gdb.Select("agent_token").First(&row, userID).Error; err != nil {
		return "", err
	}
	return row.AgentToken, nil
}

// AgentBinding returns the user's bound agent tenant id + credential.
func (s *CentralStore) AgentBinding(userID int64) (agentTenant, agentToken string, err error) {
	var row userRow
	if err := s.d.gdb.Select("agent_tenant", "agent_token").First(&row, userID).Error; err != nil {
		return "", "", err
	}
	return row.AgentTenant, row.AgentToken, nil
}

// SetAgentBinding stores the user's bound agent tenant id + credential (empty
// values are ignored, so a partial update never clears the other).
func (s *CentralStore) SetAgentBinding(userID int64, agentTenant, agentToken string) error {
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
	return s.d.gdb.Model(&userRow{}).Where("id=?", userID).Updates(updates).Error
}

func userFromRow(row *userRow) *User {
	return &User{
		ID: row.ID, Username: row.Username, DisplayName: row.DisplayName,
		Disabled: row.Disabled, Created: time.UnixMilli(row.Created),
		AgentTenant: row.AgentTenant, AgentToken: row.AgentToken,
	}
}

// ---- namespaces (owned by exactly one user) ----

// Namespace is an org owned by one user. Repositories live under it, so its
// owner is also the repository owner.
type Namespace struct {
	Name        string
	OwnerUserID int64
	Created     time.Time
}

// EnsureNamespace creates the namespace for its owner when absent (idempotent).
func (s *CentralStore) EnsureNamespace(owner int64, name string) error {
	return s.ensureNamespaceRow(owner, name)
}

// OwnerOfNamespace returns the user id owning the namespace (0 when absent).
func (s *CentralStore) OwnerOfNamespace(name string) (int64, error) {
	var row namespaceRow
	err := s.d.gdb.Where("name=?", name).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return row.OwnerUserID, nil
}

// ListNamespacesByOwner returns the namespaces owned by a user.
func (s *CentralStore) ListNamespacesByOwner(owner int64) ([]*Namespace, error) {
	var rows []namespaceRow
	if err := s.d.gdb.Where("owner_user_id=?", owner).Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Namespace, 0, len(rows))
	for i := range rows {
		out = append(out, &Namespace{Name: rows[i].Name, OwnerUserID: rows[i].OwnerUserID, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}

// TenantHasNamespace reports whether a user owns a namespace (registry scope
// evidence: you may publish @org/… only when you own that namespace).
func (s *CentralStore) TenantHasNamespace(userID int64, namespace string) bool {
	if userID == 0 {
		return false
	}
	var n int64
	s.d.gdb.Model(&namespaceRow{}).Where("owner_user_id=? AND name=?", userID, namespace).Count(&n)
	return n > 0
}

// ---- per-repository collaborator roles ----

// Repository roles. The owner is derived from the namespace owner and is not
// stored as a repo_members row; maintainer/developer are explicit grants.
const (
	RoleMaintainer = "maintainer"
	RoleDeveloper  = "developer"
)

// RepoMember is a user's non-owner role on a repository.
type RepoMember struct {
	RepoID int64
	UserID int64
	Role   string
}

// validRepoRole reports whether role is a grantable collaborator role.
func validRepoRole(role string) bool {
	return role == RoleMaintainer || role == RoleDeveloper
}

// SetRepoMember grants or updates a user's role on a repository. Owner cannot
// be granted here (the owner is the namespace owner).
func (s *CentralStore) SetRepoMember(repoID, userID int64, role string, grantedBy *int64) error {
	if !validRepoRole(role) {
		return errors.New("store: repo role must be maintainer or developer")
	}
	row := &repoMemberRow{RepoID: repoID, UserID: userID, Role: role, GrantedBy: grantedBy, Created: time.Now().UTC().UnixMilli()}
	return s.d.gdb.Clauses(gormConflictUpdateAll()).Create(row).Error
}

// RemoveRepoMember revokes a user's role on a repository.
func (s *CentralStore) RemoveRepoMember(repoID, userID int64) error {
	return s.d.gdb.Where("repo_id=? AND user_id=?", repoID, userID).Delete(&repoMemberRow{}).Error
}

// ListRepoMembers returns the explicit (non-owner) members of a repository.
func (s *CentralStore) ListRepoMembers(repoID int64) ([]*RepoMember, error) {
	var rows []repoMemberRow
	if err := s.d.gdb.Where("repo_id=?", repoID).Order("user_id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*RepoMember, 0, len(rows))
	for i := range rows {
		out = append(out, &RepoMember{RepoID: rows[i].RepoID, UserID: rows[i].UserID, Role: rows[i].Role})
	}
	return out, nil
}

// RepoMemberRole returns the explicit role a user holds on a repo ("" when a
// collaborator grant is absent).
func (s *CentralStore) RepoMemberRole(repoID, userID int64) (string, error) {
	var row repoMemberRow
	err := s.d.gdb.Where("repo_id=? AND user_id=?", repoID, userID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.Role, nil
}
