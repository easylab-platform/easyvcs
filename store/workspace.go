package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WorkspaceMarkerFile is the name of the workspace pointer file placed at a
// working directory root.
const WorkspaceMarkerFile = ".easyvcs-workspace"

// WorkspaceMarker is the pointer content stored at a working directory root. It
// records which repository (namespace/name) this checkout belongs to, which
// revision is currently checked out, and the location of the central store
// (home path and optional DSN) so a workspace is self-describing even when its
// store lives outside the process's EASYVCS_HOME. IgnoreHash tracks the hash of
// the ignore files at the last commit so a revision in .gitignore/.vcsignore can
// trigger history rewriting.
type WorkspaceMarker struct {
	Repo            RepoRef `json:"repo"`
	CurrentRevision string  `json:"current_revision"`
	Branch          string  `json:"branch,omitempty"`
	IgnoreHash      string  `json:"ignore_hash,omitempty"`
	// Home is the central store root directory this workspace belongs to.
	// Empty means "use the process's HomeDir()".
	Home string `json:"home,omitempty"`
	// DSN optionally overrides the metadata backend location (e.g. a sqlite
	// file path or a postgres connection string). When set it takes precedence
	// over Home<DBFile>. Empty means default (Home/easyvcs.db, sqlite).
	DSN string `json:"dsn,omitempty"`
}

// LoadMarker reads and parses the workspace marker from a directory, walking
// upward. It returns the directory that contains the marker and the marker
// itself, or ErrNotFound if no marker exists above dir.
func LoadMarker(dir string) (string, *WorkspaceMarker, error) {
	cur := dir
	for {
		path := filepath.Join(cur, WorkspaceMarkerFile)
		b, err := os.ReadFile(path)
		if err == nil {
			var m WorkspaceMarker
			if err := json.Unmarshal(b, &m); err != nil {
				return "", nil, err
			}
			return cur, &m, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, err
		}
		next := filepath.Dir(cur)
		if next == cur {
			break
		}
		cur = next
	}
	return "", nil, fmt.Errorf("%w: no %s found above %s", ErrNotFound, WorkspaceMarkerFile, dir)
}

// LookupMarker loads a marker and returns it (without the directory). It is a
// convenience for callers that already know the root.
func LookupMarker(dir string) (*WorkspaceMarker, error) {
	_, m, err := LoadMarker(dir)
	return m, err
}

// OpenStoreForMarker opens the central store that a workspace marker belongs to.
// When the marker records a Home or DSN it is used directly, so a workspace can
// be reopened even if its store lives in a different location from the process
// EASYVCS_HOME. An empty Home/DSN falls back to the process defaults.
func OpenStoreForMarker(m *WorkspaceMarker) (*CentralStore, error) {
	if m == nil {
		return OpenDefault()
	}
	if m.DSN != "" {
		return OpenDriver(DriverConfig{Kind: KindSQLite, DSN: m.DSN})
	}
	if m.Home != "" {
		return Open(filepath.Join(m.Home, DefaultDBFile))
	}
	return OpenDefault()
}

// WriteMarker writes a workspace marker file at the given directory root. If the
// marker does not already record a store location (Home/DSN), the process's
// HomeDir() is recorded so the workspace is self-describing; a caller that
// binds a workspace to a different store sets Home/DSN explicitly beforehand.
func WriteMarker(dir string, m *WorkspaceMarker) error {
	if m.DSN == "" && m.Home == "" {
		m.Home = HomeDir()
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, WorkspaceMarkerFile), b, 0o644)
}

// WorkspaceRow is the workspaces table entry tracking a registered workspace.
type WorkspaceRow struct {
	Path            string
	Repo            RepoRef
	CurrentRevision string
	Branch          string
}

// PutWorkspace registers or updates a workspace row in the central store.
func (s *CentralStore) PutWorkspace(w *WorkspaceRow) error {
	repo, err := s.OpenRepo(w.Repo)
	if err != nil {
		return err
	}
	_, err = s.d.db.Exec(
		`INSERT INTO workspaces(path, repo_id, current_revision, branch) VALUES(?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET repo_id=excluded.repo_id, current_revision=excluded.current_revision, branch=excluded.branch`,
		w.Path, repo.repoID, nullable(w.CurrentRevision), nullable(w.Branch),
	)
	return err
}

// GetWorkspaceByPath looks up a workspace row by path.
func (s *CentralStore) GetWorkspaceByPath(path string) (*WorkspaceRow, error) {
	var repoID int64
	var curRev, branch sql.NullString
	err := s.d.db.QueryRow("SELECT repo_id, current_revision, branch FROM workspaces WHERE path=?", path).Scan(&repoID, &curRev, &branch)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	repo, err := s.repoByID(repoID)
	if err != nil {
		return nil, err
	}
	return &WorkspaceRow{
		Path: path, Repo: repo, CurrentRevision: curRev.String, Branch: branch.String,
	}, nil
}

// ListWorkspaces returns all registered workspaces.
func (s *CentralStore) ListWorkspaces() ([]*WorkspaceRow, error) {
	rows, err := s.d.db.Query("SELECT path, repo_id, current_revision, branch FROM workspaces")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WorkspaceRow
	for rows.Next() {
		var path string
		var repoID int64
		var curRev, branch sql.NullString
		if err := rows.Scan(&path, &repoID, &curRev, &branch); err != nil {
			return nil, err
		}
		repo, err := s.repoByID(repoID)
		if err != nil {
			continue
		}
		out = append(out, &WorkspaceRow{
			Path: path, Repo: repo, CurrentRevision: curRev.String, Branch: branch.String,
		})
	}
	return out, rows.Err()
}

func (s *CentralStore) repoByID(id int64) (RepoRef, error) {
	var ns, name string
	err := s.d.db.QueryRow("SELECT namespace, name FROM repositories WHERE id=?", id).Scan(&ns, &name)
	if err != nil {
		return RepoRef{}, err
	}
	return RepoRef{Namespace: ns, Name: name}, nil
}

// FindRepo walks up from dir to locate the repo a workspace belongs to. It
// returns the workspace marker and the repo-scoped handle. If the marker's
// repo is missing from the store, an error is returned.
func (s *CentralStore) ResolveWorkspace(dir string) (*WorkspaceMarker, *Repo, error) {
	marker, err := LookupMarker(dir)
	if err != nil {
		return nil, nil, err
	}
	repo, err := s.OpenRepo(marker.Repo)
	if err != nil {
		return nil, nil, err
	}
	return marker, repo, nil
}
