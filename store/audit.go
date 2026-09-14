package store

import "time"

// RecordAudit appends an audit event to the append-only audit_log table. It
// never updates or deletes (no undo/reflog semantics).
func (s *CentralStore) RecordAudit(ev AuditEvent) error {
	row := &auditLogRow{
		Timestamp: ev.Timestamp, Action: ev.Action, Namespace: ev.Namespace,
		Repo: ev.Repo, UserID: ev.UserID, IP: ev.IP, Outcome: ev.Outcome, Detail: ev.Detail,
	}
	if row.Timestamp == 0 {
		row.Timestamp = time.Now().UTC().UnixMilli()
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
