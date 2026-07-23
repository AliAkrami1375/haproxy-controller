package store

import (
	"context"
	"database/sql"
)

// Audit records a state-changing action. Callers ignore the error: an audit
// write must never block the operation it describes.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, `
		INSERT INTO audit_log (user_id, username, action, entity, entity_id, detail, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nullInt64(e.UserID), e.Username, e.Action, e.Entity, e.EntityID,
		truncate(e.Detail, 2000), e.IP)
	return err
}

// ListAudit returns audit entries newest first, optionally filtered by a
// substring match on action, entity, username or detail.
func (s *Store) ListAudit(ctx context.Context, filter string, limit, offset int) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	query := `SELECT id, user_id, username, action, entity, entity_id, detail, ip, created_at
	          FROM audit_log`
	args := []any{}
	if filter != "" {
		query += ` WHERE action LIKE ? OR entity LIKE ? OR username LIKE ? OR detail LIKE ?`
		like := "%" + filter + "%"
		args = append(args, like, like, like, like)
	}
	query += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(c, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*AuditEntry
	for rows.Next() {
		var (
			e       AuditEntry
			userID  sql.NullInt64
			created sql.NullString
		)
		if err := rows.Scan(&e.ID, &userID, &e.Username, &e.Action, &e.Entity,
			&e.EntityID, &e.Detail, &e.IP, &created); err != nil {
			return nil, err
		}
		if userID.Valid {
			id := userID.Int64
			e.UserID = &id
		}
		e.CreatedAt = parseTime(created)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountAudit returns the total number of audit rows matching a filter.
func (s *Store) CountAudit(ctx context.Context, filter string) (int, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	query := "SELECT COUNT(*) FROM audit_log"
	args := []any{}
	if filter != "" {
		query += ` WHERE action LIKE ? OR entity LIKE ? OR username LIKE ? OR detail LIKE ?`
		like := "%" + filter + "%"
		args = append(args, like, like, like, like)
	}
	var n int
	err := s.db.QueryRowContext(c, query, args...).Scan(&n)
	return n, err
}

// TrimAudit keeps the newest rows and deletes the rest.
func (s *Store) TrimAudit(ctx context.Context, keep int) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, `
		DELETE FROM audit_log WHERE id NOT IN (
			SELECT id FROM audit_log ORDER BY id DESC LIMIT ?
		)`, keep)
	return err
}
