package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"example.com/filedock/internal/files"
)

const auditSchemaDDL = `
CREATE TABLE IF NOT EXISTS audit_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	actor_user_id TEXT NOT NULL,
	action TEXT NOT NULL,
	resource_type TEXT NOT NULL,
	resource_id TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	occurred_at_ns INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_occurred
	ON audit_logs (occurred_at_ns DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_audit_logs_actor
	ON audit_logs (actor_user_id, occurred_at_ns DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_audit_logs_action
	ON audit_logs (action, occurred_at_ns DESC, id DESC);
`

func migrateAuditLogs(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, auditSchemaDDL); err != nil {
		return fmt.Errorf("initialize audit_logs schema: %w", err)
	}
	return nil
}

func appendAuditLog(ctx context.Context, tx *sql.Tx, actorUserID, action, resourceType, resourceID string, occurredAt time.Time) error {
	utc := occurredAt.UTC()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO audit_logs (actor_user_id, action, resource_type, resource_id, occurred_at, occurred_at_ns)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		actorUserID,
		action,
		resourceType,
		resourceID,
		utc.Format(time.RFC3339Nano),
		utc.UnixNano(),
	); err != nil {
		return fmt.Errorf("insert audit log (%s %s:%s): %w", action, resourceType, resourceID, err)
	}
	return nil
}

// ListAuditLogs returns audit records ordered newest-first, filtered by optional actor user ID and action.
func (s *Store) ListAuditLogs(ctx context.Context, filter files.AuditFilter) ([]files.AuditEntry, error) {
	norm, err := files.NormalizeAuditFilter(filter)
	if err != nil {
		return nil, err
	}

	var (
		clauses []string
		args    []any
	)
	if norm.ActorUserID != "" {
		clauses = append(clauses, "actor_user_id = ?")
		args = append(args, norm.ActorUserID)
	}
	if norm.Action != "" {
		clauses = append(clauses, "action = ?")
		args = append(args, norm.Action)
	}

	query := `SELECT id, actor_user_id, action, resource_type, resource_id, occurred_at FROM audit_logs`
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, " AND ")
	}
	query += ` ORDER BY occurred_at_ns DESC, id DESC LIMIT ?`
	args = append(args, norm.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit logs: %w", err)
	}
	defer rows.Close()

	entries := make([]files.AuditEntry, 0)
	for rows.Next() {
		var (
			entry       files.AuditEntry
			occurredStr string
		)
		if err := rows.Scan(
			&entry.ID,
			&entry.ActorUserID,
			&entry.Action,
			&entry.ResourceType,
			&entry.ResourceID,
			&occurredStr,
		); err != nil {
			return nil, fmt.Errorf("scan audit log: %w", err)
		}
		parsedAt, err := time.Parse(time.RFC3339Nano, occurredStr)
		if err != nil {
			return nil, fmt.Errorf("parse audit occurred_at %q: %w", occurredStr, err)
		}
		entry.OccurredAt = parsedAt.UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit logs: %w", err)
	}
	return entries, nil
}
