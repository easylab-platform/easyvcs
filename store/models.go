package store

import (
	"encoding/json"
	"time"
)

// GORM models mirror the central store schema. They are used by AutoMigrate to
// create portable DDL (sqlite / postgres / mysql) so the metadata layer can run
// on any of the three backends with no cgo. Binary columns (objects.content,
// snapshots.meta, revisions.changed_paths) are []byte and map to BLOB/BYTEA/
// LONGBLOB per dialect. Cross-dialect types (id autoincrement, TIMESTAMP) are
// handled by GORM.

// repositories table.
type repoRow struct {
	ID             int64  `gorm:"primaryKey;autoIncrement"`
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

// ---- Lab (hosting) entities ----

// users table.
type userRow struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	Username    string `gorm:"not null;uniqueIndex"`
	DisplayName string `gorm:"not null;default:''"`
	Created     int64  `gorm:"not null"`
}

// tokens table.
type tokenRow struct {
	ID      int64  `gorm:"primaryKey;autoIncrement"`
	Token   string `gorm:"not null;uniqueIndex"`
	UserID  int64  `gorm:"not null"`
	Level   string `gorm:"not null;default:'write'"`
	Created int64  `gorm:"not null"`
}

// namespace_members table.
type namespaceMemberRow struct {
	Namespace string `gorm:"primaryKey;not null"`
	UserID    int64  `gorm:"primaryKey;not null"`
	Role      string `gorm:"not null;default:'member'"`
}

// merge_requests table.
type mergeRequestRow struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	RepoID      int64  `gorm:"not null;uniqueIndex:idx_mr"`
	IID         int64  `gorm:"column:iid;not null;uniqueIndex:idx_mr"`
	Title       string `gorm:"not null"`
	Description string `gorm:"not null;default:''"`
	Source      string `gorm:"not null"`
	Target      string `gorm:"not null"`
	State       string `gorm:"not null;default:'open'"`
	AuthorID    *int64
	Created     int64 `gorm:"not null"`
	Updated     int64 `gorm:"not null"`
}

// mr_reviews table.
type mrReviewRow struct {
	ID         int64  `gorm:"primaryKey;autoIncrement"`
	MRID       int64  `gorm:"not null;index"`
	ReviewerID *int64
	State      string `gorm:"not null"`
	Body       string `gorm:"not null;default:''"`
	Created    int64  `gorm:"not null"`
}

// mr_comments table.
type mrCommentRow struct {
	ID       int64  `gorm:"primaryKey;autoIncrement"`
	MRID     int64  `gorm:"not null;index"`
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

// allModels returns every table model for AutoMigrate.
func allModels() []any {
	return []any{
		&repoRow{}, &pushMirrorRow{}, &objectRow{}, &snapshotRow{}, &revisionRow{},
		&refRow{}, &workspaceRow{}, &remoteRow{}, &remoteRefRow{},
		&userRow{}, &tokenRow{}, &namespaceMemberRow{},
		&mergeRequestRow{}, &mrReviewRow{}, &mrCommentRow{},
		&gitRevisionLinkRow{},
	}
}

// changedPathsJSON encodes a []string as a JSON byte blob (legacy column).
func changedPathsJSON(paths []string) []byte {
	if paths == nil {
		return nil
	}
	b, err := json.Marshal(paths)
	if err != nil {
		return nil
	}
	return b
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

// unixMillis returns ms; used to keep time columns portable integers.
func unixMillis(t time.Time) int64 { return t.UnixMilli() }

// TableName methods pin GORM to the original canonical table names so any
// remaining raw-SQL callers (mirror.go/workspace.go) and the GORM layer agree.

func (repoRow) TableName() string         { return "repositories" }
func (pushMirrorRow) TableName() string   { return "push_mirrors" }
func (objectRow) TableName() string       { return "objects" }
func (snapshotRow) TableName() string     { return "snapshots" }
func (revisionRow) TableName() string     { return "revisions" }
func (refRow) TableName() string          { return "refs" }
func (workspaceRow) TableName() string    { return "workspaces" }
func (remoteRow) TableName() string       { return "remotes" }
func (remoteRefRow) TableName() string    { return "remote_refs" }
func (userRow) TableName() string         { return "users" }
func (tokenRow) TableName() string        { return "tokens" }
func (namespaceMemberRow) TableName() string { return "namespace_members" }
func (mergeRequestRow) TableName() string { return "merge_requests" }
func (mrReviewRow) TableName() string     { return "mr_reviews" }
func (mrCommentRow) TableName() string    { return "mr_comments" }
