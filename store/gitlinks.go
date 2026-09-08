package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PutGitLink records a revision<->commit traceability link for the git bridge.
func (r *Repo) PutGitLink(revID, commitSHA, ref, side string) error {
	row := &gitRevisionLinkRow{
		RepoID: r.repoID, RevisionID: revID, CommitSHA: commitSHA,
		Ref: ref, Side: side, Created: time.Now().UTC().UnixMilli(),
	}
	return r.cs.d.gdb.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_id"}, {Name: "revision_id"}, {Name: "commit_sha"}, {Name: "side"}},
		UpdateAll: true,
	}).Create(row).Error
}

// GitCommitForRevision returns the last exported commit sha for a revision, if any.
func (r *Repo) GitCommitForRevision(revID string) (string, error) {
	var row gitRevisionLinkRow
	err := r.cs.d.gdb.Where("repo_id=? AND revision_id=? AND side=?", r.repoID, revID, "export").Order("id DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.CommitSHA, nil
}

// RevisionForGitCommit returns the easyvcs revision that a commit sha was
// imported as, if any.
func (r *Repo) RevisionForGitCommit(commitSHA string) (string, error) {
	var row gitRevisionLinkRow
	err := r.cs.d.gdb.Where("repo_id=? AND commit_sha=? AND side=?", r.repoID, commitSHA, "import").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.RevisionID, nil
}
