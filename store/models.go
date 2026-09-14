package store

import (
	"encoding/json"
)

// GORM models mirror the central store schema. They are used by AutoMigrate to
// create portable DDL (sqlite / postgres / mysql) so the metadata layer can run
// on any of the three backends with no cgo. Binary columns (objects.content,
// snapshots.meta, revisions.changed_paths) are []byte and map to BLOB/BYTEA/
// LONGBLOB per dialect. Cross-dialect types (id autoincrement, TIMESTAMP) are
// handled by GORM.
//
// Ownership model: a USER is the top-level identity AND the ownership boundary
// (there is no separate tenant — user ≡ tenant). A user owns NAMESPACES; a
// namespace is owned by exactly one user. A repository lives under a namespace,
// so its owner (the namespace owner) is unambiguous. Additional collaborators
// are granted per-repository roles (maintainer / developer) via repo_members.

// repositories table. OwnerUserID scopes (namespace, name): user A and user B
// may each own an "acme/api" repo. The owner is the namespace owner.
type repoRow struct {
	ID             int64  `gorm:"primaryKey;autoIncrement"`
	OwnerUserID    int64  `gorm:"not null;default:0;uniqueIndex:idx_repo"`
	Namespace      string `gorm:"not null;uniqueIndex:idx_repo"`
	Name           string `gorm:"not null;uniqueIndex:idx_repo"`
	Created        int64  `gorm:"not null"`
	Description    string `gorm:"not null;default:''"`
	Visibility     string `gorm:"not null;default:'public'"`
	DefaultBranch  string `gorm:"column:default_branch;not null;default:'main'"`
	Kind           string `gorm:"not null;default:'normal'"`
	MirrorURL      string `gorm:"column:mirror_url;not null;default:''"`
	MirrorBranch   string `gorm:"column:mirror_branch;not null;default:'main'"`
	MirrorInterval int64  `gorm:"column:mirror_interval;not null;default:300"`
	MirrorLastRev  string `gorm:"column:mirror_last_rev;not null;default:''"`
	MirrorLastSync int64  `gorm:"column:mirror_last_sync;not null;default:0"`
	MirrorLastErr  string `gorm:"column:mirror_last_error;not null;default:''"`
	MirrorToken    string `gorm:"column:mirror_token;not null;default:''"`
}

func (repoRow) TableName() string { return "repositories" }

// namespaces table. A namespace (org) is owned by exactly one user; the
// (owner_user_id, name) pair is unique. A user may own many namespaces.
type namespaceRow struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	OwnerUserID int64  `gorm:"not null;uniqueIndex:idx_namespace"`
	Name        string `gorm:"not null;uniqueIndex:idx_namespace"`
	Created     int64  `gorm:"not null"`
}

func (namespaceRow) TableName() string { return "namespaces" }

// repo_members table grants a user a non-owner role on a repository. The owner
// is NOT stored here: it is derived from the repository's namespace owner
// (repoRow.OwnerUserID). Roles: "maintainer" | "developer".
type repoMemberRow struct {
	RepoID    int64  `gorm:"primaryKey;not null"`
	UserID    int64  `gorm:"primaryKey;not null"`
	Role      string `gorm:"not null;default:'developer'"`
	GrantedBy *int64
	Created   int64 `gorm:"not null"`
}

func (repoMemberRow) TableName() string { return "repo_members" }

// push_mirrors table.
type pushMirrorRow struct {
	ID        int64  `gorm:"primaryKey;autoIncrement"`
	RepoID    int64  `gorm:"not null;uniqueIndex:idx_pushmirror"`
	Name      string `gorm:"not null;uniqueIndex:idx_pushmirror"`
	URL       string `gorm:"not null"`
	Branch    string `gorm:"not null;default:'main'"`
	Token     string `gorm:"not null;default:''"`
	LastRev   string `gorm:"not null;default:''"`
	LastError string `gorm:"not null;default:''"`
}

// objects table (global, content-addressed).
type objectRow struct {
	ID      string `gorm:"primaryKey;column:sha"`
	Kind    int    `gorm:"not null"`
	Content []byte `gorm:"not null"`
}

// snapshots table.
type snapshotRow struct {
	RepoID     int64  `gorm:"primaryKey;not null"`
	ID         string `gorm:"primaryKey;column:sha;not null"`
	RevisionID string `gorm:"not null"`
	TreeID     string `gorm:"not null"`
	CommitTime int64  `gorm:"not null"`
	Meta       []byte `gorm:"not null"`
}

// revisions table.
type revisionRow struct {
	RepoID      int64  `gorm:"primaryKey;not null"`
	ID          string `gorm:"primaryKey;not null"`
	Hash        string `gorm:"not null"`
	Created     int64  `gorm:"not null"`
	ForkFrom    string
	ChangedPath []byte `gorm:"column:changed_paths"`
}

// refs table.
type refRow struct {
	RepoID int64  `gorm:"primaryKey;not null"`
	Name   string `gorm:"primaryKey;not null"`
	Kind   string `gorm:"not null"`
	Target string `gorm:"not null"`
}

// workspaces table.
type workspaceRow struct {
	ID              int64   `gorm:"primaryKey;autoIncrement"`
	Path            string  `gorm:"uniqueIndex"`
	RepoID          int64   `gorm:"not null"`
	CurrentRevision *string `gorm:"column:current_revision"`
	Branch          *string
}

// remotes table.
type remoteRow struct {
	RepoID        int64  `gorm:"primaryKey;not null"`
	Name          string `gorm:"primaryKey;not null"`
	URL           string `gorm:"not null"`
	Token         *string
	LastSyncTip   string `gorm:"not null;default:''"`
	DefaultBranch string `gorm:"not null;default:''"`
	Ancestry      []byte `gorm:"column:ancestry"`
}

// remote_refs table.
type remoteRefRow struct {
	RepoID     int64  `gorm:"primaryKey;not null"`
	RemoteName string `gorm:"primaryKey;not null"`
	Kind       string `gorm:"primaryKey;not null"`
	Name       string `gorm:"primaryKey;not null"`
	Target     string `gorm:"not null"`
}

// users table. A user is simultaneously the identity (credentials, audit) and
// the ownership boundary: it owns namespaces and repositories, and is bound to
// one abcp-agent tenant. `Disabled` stops the user's credentials from
// authenticating anywhere.
type userRow struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	Username    string `gorm:"not null;uniqueIndex"`
	DisplayName string `gorm:"not null;default:''"`
	Disabled    bool   `gorm:"not null;default:false"`
	// AgentTenant is the abcp-agent tenant id bound to this user (set at
	// provisioning time). It is the explicit record of the user↔agent binding.
	AgentTenant string `gorm:"not null;default:''"`
	// AgentToken is the user's agent.v1 bearer credential (the bootstrap token
	// minted by the agent's AdminService at provisioning). The gateway presents
	// it on every forwarded agent RPC of this user. It is an internal service
	// credential — opaque to lab users — stored as-is so the gateway can forward
	// it without a secrets service.
	AgentToken string `gorm:"not null;default:''"`
	Created    int64  `gorm:"not null"`
}

func (userRow) TableName() string { return "users" }

// tokens table.
type tokenRow struct {
	ID      int64  `gorm:"primaryKey;autoIncrement"`
	Token   string `gorm:"not null;uniqueIndex"`
	UserID  int64  `gorm:"not null"`
	Level   string `gorm:"not null;default:'write'"` // retained for backward compat; no longer authorizes
	Created int64  `gorm:"not null"`
}

// merge_requests table. SourceRepoID is the repository the source ref lives in;
// it equals RepoID for a same-repo MR and differs for a fork→upstream MR.
type mergeRequestRow struct {
	ID           int64  `gorm:"primaryKey;autoIncrement"`
	RepoID       int64  `gorm:"not null;uniqueIndex:idx_mr"`
	SourceRepoID int64  `gorm:"not null;default:0"`
	IID          int64  `gorm:"column:iid;not null;uniqueIndex:idx_mr"`
	Title        string `gorm:"not null"`
	Description  string `gorm:"not null;default:''"`
	Source       string `gorm:"not null"`
	Target       string `gorm:"not null"`
	State        string `gorm:"not null;default:'open'"`
	AuthorID     *int64
	Created      int64 `gorm:"not null"`
	Updated      int64 `gorm:"not null"`
}

// mr_reviews table.
type mrReviewRow struct {
	ID         int64 `gorm:"primaryKey;autoIncrement"`
	MRID       int64 `gorm:"not null;index"`
	ReviewerID *int64
	State      string `gorm:"not null"`
	Body       string `gorm:"not null;default:''"`
	Created    int64  `gorm:"not null"`
}

// mr_comments table.
type mrCommentRow struct {
	ID       int64 `gorm:"primaryKey;autoIncrement"`
	MRID     int64 `gorm:"not null;index"`
	AuthorID *int64
	Body     string `gorm:"not null"`
	Path     *string
	Created  int64 `gorm:"not null"`
}

// gitRevisionLinkRow records the weak-traceability link between an easyvcs
// revision and a git commit produced when exporting/importing through the git
// bridge. It is plain easyvcs metadata (never stored as a .git object); it lets
// an easyvcs revision be correlated back to a commit sha for the same repo.
type gitRevisionLinkRow struct {
	ID         int64  `gorm:"primaryKey;autoIncrement"`
	RepoID     int64  `gorm:"not null;uniqueIndex:idx_gitlink"`
	RevisionID string `gorm:"not null;uniqueIndex:idx_gitlink"`
	CommitSHA  string `gorm:"not null;uniqueIndex:idx_gitlink"`
	// Ref is the git ref the commit was exported under (e.g. refs/heads/main).
	Ref string `gorm:"not null;default:''"`
	// Side is "export" (revision -> git) or "import" (git -> revision).
	Side    string `gorm:"not null;default:'export';uniqueIndex:idx_gitlink"`
	Created int64  `gorm:"not null"`
}

func (gitRevisionLinkRow) TableName() string { return "git_revision_links" }

// packageOwnerRow is the ownership record of one registry name. Ownership is by
// user (the former tenant), so (format, repository) belonging to exactly one
// user IS the global-namespace contract.
type packageOwnerRow struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	Format      string `gorm:"not null;uniqueIndex:idx_pkgowner"`
	Repository  string `gorm:"not null;uniqueIndex:idx_pkgowner"`
	OwnerUserID int64  `gorm:"not null;default:0"`
	Visibility  string `gorm:"not null;default:'public'"`
	Created     int64  `gorm:"not null"`
}

func (packageOwnerRow) TableName() string { return "package_owners" }

// auditLogRow is an append-only access/audit record. It is never updated or
// deleted (no undo / reflog semantics).
type auditLogRow struct {
	ID        int64  `gorm:"primaryKey;autoIncrement"`
	Timestamp int64  `gorm:"not null;index"`
	Action    string `gorm:"not null"`
	Namespace string `gorm:"not null;default:''"`
	Repo      string `gorm:"not null;default:''"`
	UserID    *int64
	IP        string `gorm:"not null;default:''"`
	Outcome   string `gorm:"not null;default:''"`
	Detail    string `gorm:"not null;default:''"`
}

func (auditLogRow) TableName() string { return "audit_log" }

// allModels returns every table model for AutoMigrate.
func allModels() []any {
	return []any{
		&repoRow{}, &pushMirrorRow{}, &objectRow{}, &snapshotRow{}, &revisionRow{},
		&refRow{}, &workspaceRow{}, &remoteRow{}, &remoteRefRow{},
		&userRow{}, &tokenRow{}, &namespaceRow{}, &repoMemberRow{},
		&mergeRequestRow{}, &mrReviewRow{}, &mrCommentRow{},
		&gitRevisionLinkRow{}, &auditLogRow{}, &packageOwnerRow{},
	}
}

// AuditEvent is a plain append-only record of an access attempt.
type AuditEvent struct {
	Timestamp int64
	Action    string // "advertise" | "fetch" | "push"
	Namespace string
	Repo      string
	UserID    *int64
	IP        string
	Outcome   string // "ok" | "denied" | "error"
	Detail    string
}

// decodeChangedPathsJSON decodes the changed_paths blob.
func decodeChangedPathsJSON(raw []byte) []string {
	if raw == nil {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// TableName methods pin GORM to the original canonical table names so any
// remaining raw-SQL callers (mirror.go/workspace.go) and the GORM layer agree.

func (pushMirrorRow) TableName() string   { return "push_mirrors" }
func (objectRow) TableName() string       { return "objects" }
func (snapshotRow) TableName() string     { return "snapshots" }
func (revisionRow) TableName() string     { return "revisions" }
func (refRow) TableName() string          { return "refs" }
func (workspaceRow) TableName() string    { return "workspaces" }
func (remoteRow) TableName() string       { return "remotes" }
func (remoteRefRow) TableName() string    { return "remote_refs" }
func (tokenRow) TableName() string        { return "tokens" }
func (mergeRequestRow) TableName() string { return "merge_requests" }
func (mrReviewRow) TableName() string     { return "mr_reviews" }
func (mrCommentRow) TableName() string    { return "mr_comments" }
