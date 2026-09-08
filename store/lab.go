package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// User is a Lab account. A user owns namespaces (orgs) and repositories.
type User struct {
	ID          int64
	Username    string
	DisplayName string
	Created     time.Time
}

// Token is a bearer token bound to a user with an access level.
type Token struct {
	ID      int64
	Token   string
	UserID  int64
	Level   string // "read" or "write"
	Created time.Time
}

// NamespaceMember is a user's membership in a namespace (org).
type NamespaceMember struct {
	Namespace string
	UserID    int64
	Role      string // "owner" | "member"
}

// RepoMeta is the hosting metadata for a repository.
type RepoMeta struct {
	Description   string
	Visibility    string // "public" | "private"
	DefaultBranch string
	// Kind is "normal" (read-write) or "mirror" (read-only clone of an external
	// git repo; edits are forbidden).
	Kind string
	// Mirror fields are relevant only when Kind == "mirror".
	MirrorURL      string
	MirrorBranch   string
	MirrorInterval int // seconds between scheduled pulls; 0 = manual only
	MirrorLastRev  string
	MirrorLastSync int64 // epoch millis
	MirrorLastErr  string
	MirrorToken    string // never returned by API
}

// IsMirror reports whether the repository is a read-only external mirror.
func (r *Repo) IsMirror() bool {
	meta, err := r.RepoMeta()
	return err == nil && meta.Kind == "mirror"
}

// MergeRequest models a change-tracking request between two revisions.
type MergeRequest struct {
	ID          int64
	RepoID      int64
	IID         int64
	Title       string
	Description string
	Source      string // revision id or branch name
	Target      string // revision id or branch name
	State       string // "open" | "merged" | "closed"
	AuthorID    *int64
	Created     time.Time
	Updated     time.Time
}

// Review is a reviewer's verdict on a merge request.
type Review struct {
	ID         int64
	MRID       int64
	ReviewerID *int64
	State      string // "approved" | "request_changes" | "comment"
	Body       string
	Created    time.Time
}

// Comment is a discussion line on a merge request.
type Comment struct {
	ID       int64
	MRID     int64
	AuthorID *int64
	Body     string
	Path     string
	Created  time.Time
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
	return &User{ID: row.ID, Username: username, DisplayName: displayName, Created: time.UnixMilli(now)}, nil
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
	return &User{ID: row.ID, Username: row.Username, DisplayName: row.DisplayName, Created: time.UnixMilli(row.Created)}, nil
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
	return &User{ID: row.ID, Username: row.Username, DisplayName: row.DisplayName, Created: time.UnixMilli(row.Created)}, nil
}

// ListUsers returns all users ordered by username.
func (s *CentralStore) ListUsers() ([]*User, error) {
	var rows []userRow
	if err := s.d.gdb.Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*User, 0, len(rows))
	for i := range rows {
		out = append(out, &User{ID: rows[i].ID, Username: rows[i].Username, DisplayName: rows[i].DisplayName, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}

// hashToken derives the storage form of a bearer token: hex(SHA-256(token)).
// Tokens are only ever compared by hash, so the database never holds the
// credential itself. The plaintext value is returned by CreateToken exactly
// once and must be shown to the user then.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateToken inserts a bearer token for a user. The token is stored as its
// SHA-256 hash; the returned Token carries the plaintext for one-time display.
func (s *CentralStore) CreateToken(token string, userID int64, level string) (*Token, error) {
	if level != "read" && level != "write" {
		return nil, fmt.Errorf("invalid token level %q (read|write)", level)
	}
	now := time.Now().UTC().UnixMilli()
	row := &tokenRow{Token: hashToken(token), UserID: userID, Level: level, Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &Token{ID: row.ID, Token: token, UserID: userID, Level: level, Created: time.UnixMilli(now)}, nil
}

// LookupToken resolves a token string to its user and level. It returns
// ErrTokenNotFound when the token is unknown. The plaintext token is echoed
// back in the returned record for interface stability (the caller supplied it).
func (s *CentralStore) LookupToken(token string) (*Token, error) {
	var row tokenRow
	err := s.d.gdb.Where("token=?", hashToken(token)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Token{ID: row.ID, Token: token, UserID: row.UserID, Level: row.Level, Created: time.UnixMilli(row.Created)}, nil
}

// ListTokens returns all tokens for a user. The stored value is a SHA-256 hash
// and is NOT returned; Token.Token is empty (the plaintext is only available
// from CreateToken at creation time).
func (s *CentralStore) ListTokens(userID int64) ([]*Token, error) {
	var rows []tokenRow
	if err := s.d.gdb.Where("user_id=?", userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Token, 0, len(rows))
	for i := range rows {
		out = append(out, &Token{ID: rows[i].ID, Token: "", UserID: rows[i].UserID, Level: rows[i].Level, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}

// DeleteToken removes a token by its secret value.
func (s *CentralStore) DeleteToken(token string) error {
	res := s.d.gdb.Where("token=?", hashToken(token)).Delete(&tokenRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTokenNotFound
	}
	return nil
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

// RawQuery exposes a raw GORM query on the central store (test/diagnostic
// helper; prefer the typed methods above).
func (s *CentralStore) RawQuery(sql string) *gorm.DB { return s.d.gdb.Raw(sql) }

// RepoMeta returns the hosting metadata for a repository.
func (r *Repo) RepoMeta() (RepoMeta, error) {
	var row repoRow
	if err := r.cs.d.gdb.Where("id=?", r.repoID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return RepoMeta{}, ErrNotFound
		}
		return RepoMeta{}, err
	}
	decToken, err := decryptSecret(row.MirrorToken)
	if err != nil {
		return RepoMeta{}, err
	}
	return RepoMeta{
		Description: row.Description, Visibility: row.Visibility, DefaultBranch: row.DefaultBranch,
		Kind: row.Kind, MirrorURL: row.MirrorURL, MirrorBranch: row.MirrorBranch,
		MirrorInterval: int(row.MirrorInterval), MirrorLastRev: row.MirrorLastRev,
		MirrorLastSync: row.MirrorLastSync, MirrorLastErr: row.MirrorLastErr, MirrorToken: decToken,
	}, nil
}

// UpdateRepoMeta updates description/visibility/default branch.
func (r *Repo) UpdateRepoMeta(m RepoMeta) error {
	return r.cs.d.gdb.Model(&repoRow{}).Where("id=?", r.repoID).Updates(map[string]any{
		"description": m.Description, "visibility": m.Visibility, "default_branch": m.DefaultBranch,
	}).Error
}

// UpdateMirrorMeta records mirror state (url, branch, interval, last result).
func (r *Repo) UpdateMirrorMeta(m RepoMeta) error {
	enc, err := encryptSecret(m.MirrorToken)
	if err != nil {
		return err
	}
	return r.cs.d.gdb.Model(&repoRow{}).Where("id=?", r.repoID).Updates(map[string]any{
		"kind": m.Kind, "mirror_url": m.MirrorURL, "mirror_branch": m.MirrorBranch,
		"mirror_interval": m.MirrorInterval, "mirror_last_rev": m.MirrorLastRev,
		"mirror_last_sync": m.MirrorLastSync, "mirror_last_error": m.MirrorLastErr,
		"mirror_token": enc,
	}).Error
}

// CreateMergeRequest inserts a new MR and returns it. IID is assigned
// monotonically per repo.
func (s *CentralStore) CreateMergeRequest(repoID int64, mr *MergeRequest) (*MergeRequest, error) {
	now := time.Now().UTC().UnixMilli()
	var maxIID *int64
	if err := s.d.gdb.Model(&mergeRequestRow{}).Where("repo_id=?", repoID).Select("COALESCE(MAX(iid),0)+1").Scan(&maxIID).Error; err != nil {
		return nil, fmt.Errorf("assign MR iid: %w", err)
	}
	iid := int64(1)
	if maxIID != nil {
		iid = *maxIID
	}
	row := &mergeRequestRow{
		RepoID: repoID, IID: iid, Title: mr.Title, Description: mr.Description,
		Source: mr.Source, Target: mr.Target, State: mr.State,
		AuthorID: mr.AuthorID, Created: now, Updated: now,
	}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &MergeRequest{
		ID: row.ID, RepoID: repoID, IID: iid, Title: mr.Title, Description: mr.Description,
		Source: mr.Source, Target: mr.Target, State: mr.State, AuthorID: mr.AuthorID,
		Created: time.UnixMilli(now), Updated: time.UnixMilli(now),
	}, nil
}

// GetMergeRequest returns an MR by repo + iid.
func (s *CentralStore) GetMergeRequest(repoID, iid int64) (*MergeRequest, error) {
	var row mergeRequestRow
	err := s.d.gdb.Where("repo_id=? AND iid=?", repoID, iid).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &MergeRequest{
		ID: row.ID, RepoID: row.RepoID, IID: row.IID, Title: row.Title, Description: row.Description,
		Source: row.Source, Target: row.Target, State: row.State, AuthorID: row.AuthorID,
		Created: time.UnixMilli(row.Created), Updated: time.UnixMilli(row.Updated),
	}, nil
}

// ListMergeRequests returns MRs for a repo, optionally filtered by state.
func (s *CentralStore) ListMergeRequests(repoID int64, state string) ([]*MergeRequest, error) {
	q := s.d.gdb.Where("repo_id=?", repoID)
	if state != "" {
		q = q.Where("state=?", state)
	}
	var rows []mergeRequestRow
	if err := q.Order("iid DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*MergeRequest, 0, len(rows))
	for i := range rows {
		out = append(out, &MergeRequest{
			ID: rows[i].ID, RepoID: rows[i].RepoID, IID: rows[i].IID, Title: rows[i].Title,
			Description: rows[i].Description, Source: rows[i].Source, Target: rows[i].Target,
			State: rows[i].State, AuthorID: rows[i].AuthorID,
			Created: time.UnixMilli(rows[i].Created), Updated: time.UnixMilli(rows[i].Updated),
		})
	}
	return out, nil
}

// UpdateMergeRequestState updates an MR state (merged/closed/reopen).
func (s *CentralStore) UpdateMergeRequestState(repoID, iid int64, state string) error {
	return s.d.gdb.Model(&mergeRequestRow{}).Where("repo_id=? AND iid=?", repoID, iid).Updates(map[string]any{
		"state": state, "updated": time.Now().UTC().UnixMilli(),
	}).Error
}

// AddReview appends a review to an MR.
func (s *CentralStore) AddReview(mrID int64, reviewerID *int64, state, body string) (*Review, error) {
	now := time.Now().UTC().UnixMilli()
	row := &mrReviewRow{MRID: mrID, ReviewerID: reviewerID, State: state, Body: body, Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &Review{ID: row.ID, MRID: mrID, ReviewerID: reviewerID, State: state, Body: body, Created: time.UnixMilli(now)}, nil
}

// ListReviews returns reviews for an MR.
func (s *CentralStore) ListReviews(mrID int64) ([]*Review, error) {
	var rows []mrReviewRow
	if err := s.d.gdb.Where("mr_id=?", mrID).Order("created").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Review, 0, len(rows))
	for i := range rows {
		out = append(out, &Review{ID: rows[i].ID, MRID: rows[i].MRID, ReviewerID: rows[i].ReviewerID, State: rows[i].State, Body: rows[i].Body, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}

// AddComment appends a comment to an MR.
func (s *CentralStore) AddComment(mrID int64, authorID *int64, body, path string) (*Comment, error) {
	now := time.Now().UTC().UnixMilli()
	row := &mrCommentRow{MRID: mrID, AuthorID: authorID, Body: body, Path: strPtr(path), Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &Comment{ID: row.ID, MRID: mrID, AuthorID: authorID, Body: body, Path: path, Created: time.UnixMilli(now)}, nil
}

// ListComments returns comments for an MR.
func (s *CentralStore) ListComments(mrID int64) ([]*Comment, error) {
	var rows []mrCommentRow
	if err := s.d.gdb.Where("mr_id=?", mrID).Order("created").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Comment, 0, len(rows))
	for i := range rows {
		path := ""
		if rows[i].Path != nil {
			path = *rows[i].Path
		}
		out = append(out, &Comment{ID: rows[i].ID, MRID: rows[i].MRID, AuthorID: rows[i].AuthorID, Body: rows[i].Body, Path: path, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}



func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}


