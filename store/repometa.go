package store

import (
	"errors"

	"gorm.io/gorm"
)

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
