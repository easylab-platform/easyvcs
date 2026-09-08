package store

import (
	"gorm.io/gorm/clause"
)

// AddBranchACL grants a user push access to a specific branch of a repository.
func (r *Repo) AddBranchACL(userID int64, branch string) error {
	row := &branchACLRow{RepoID: r.repoID, UserID: userID, Branch: branch}
	return r.cs.d.gdb.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error
}

// RemoveBranchACL revokes a user's push access to a branch.
func (r *Repo) RemoveBranchACL(userID int64, branch string) error {
	return r.cs.d.gdb.Where("repo_id=? AND user_id=? AND branch=?", r.repoID, userID, branch).Delete(&branchACLRow{}).Error
}

// ListBranchACL returns the branches a user may push in this repo.
func (r *Repo) ListBranchACL(userID int64) ([]string, error) {
	var rows []string
	if err := r.cs.d.gdb.Model(&branchACLRow{}).Where("repo_id=? AND user_id=?", r.repoID, userID).Pluck("branch", &rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// CanPushBranch reports whether a user may push the given branch. Semantics:
//   - a branch ACL row for (user, branch) => allowed;
//   - a user with at least one branch ACL row for this repo => allowed only for
//     listed branches (the allowlist is in effect);
//   - a user with no branch ACL rows for this repo => falls back to the repo's
//     write role (repo-wide access).
func (r *Repo) CanPushBranch(userID int64, branch string) (bool, error) {
	// Any row for this user + branch?
	var count int64
	if err := r.cs.d.gdb.Model(&branchACLRow{}).Where("repo_id=? AND user_id=? AND branch=?", r.repoID, userID, branch).Count(&count).Error; err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}
	// Does the user have ANY branch ACL rows here (i.e. an allowlist in effect)?
	var anyCount int64
	if err := r.cs.d.gdb.Model(&branchACLRow{}).Where("repo_id=? AND user_id=?", r.repoID, userID).Count(&anyCount).Error; err != nil {
		return false, err
	}
	if anyCount > 0 {
		return false, nil // allowlist active; this branch not listed
	}
	return true, nil // no allowlist -> repo-wide write role applies
}

// RecordAudit appends an audit event to the append-only audit_log table. It
// never updates or deletes (no undo/reflog semantics).
func (s *CentralStore) RecordAudit(ev AuditEvent) error {
	row := &auditLogRow{
		Timestamp: ev.Timestamp, Action: ev.Action, Namespace: ev.Namespace,
		Repo: ev.Repo, UserID: ev.UserID, IP: ev.IP, Outcome: ev.Outcome, Detail: ev.Detail,
	}
	return s.d.gdb.Create(row).Error
}

// ListAudit returns the most recent audit records (newest first), used for
// verification/diagnostics.
func (s *CentralStore) ListAudit(limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []auditLogRow
	if err := s.d.gdb.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]AuditEvent, 0, len(rows))
	for i := range rows {
		out = append(out, AuditEvent{
			Timestamp: rows[i].Timestamp, Action: rows[i].Action, Namespace: rows[i].Namespace,
			Repo: rows[i].Repo, UserID: rows[i].UserID, IP: rows[i].IP,
			Outcome: rows[i].Outcome, Detail: rows[i].Detail,
		})
	}
	return out, nil
}

