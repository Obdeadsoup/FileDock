package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"example.com/filedock/internal/files"
)

func migrateBlobCleanups(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS blob_cleanups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	object_key TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_retry_at_ns INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_blob_cleanups_due
	ON blob_cleanups (next_retry_at_ns ASC, id ASC);`); err != nil {
		return fmt.Errorf("initialize blob_cleanups schema: %w", err)
	}
	return nil
}

// DeleteFile removes the file metadata row and enqueues its object_key for disk cleanup in the same transaction.
func (s *Store) DeleteFile(ctx context.Context, ownerID, id string, now time.Time) error {
	if strings.TrimSpace(ownerID) == "" {
		return files.ErrInvalidOwner
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin file deletion: %w", err)
	}
	defer tx.Rollback()

	record, err := getFile(ctx, tx, ownerID, id)
	if err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM files WHERE owner_id = ? AND id = ?`, ownerID, id)
	if err != nil {
		return fmt.Errorf("delete file metadata: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("file delete rows affected: %w", err)
	}
	if affected == 0 {
		return files.ErrNotFound
	}

	nowUTC := now.UTC()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO blob_cleanups (object_key, created_at, created_at_ns, attempts, next_retry_at_ns, last_error)
VALUES (?, ?, ?, 0, 0, '')`,
		record.ObjectKey,
		nowUTC.Format(time.RFC3339Nano),
		nowUTC.UnixNano(),
	); err != nil {
		return fmt.Errorf("enqueue blob cleanup: %w", err)
	}

	return tx.Commit()
}

// ListDueCleanups returns up to limit cleanup tasks whose next_retry_at_ns is due at or before now.
func (s *Store) ListDueCleanups(ctx context.Context, now time.Time, limit int) ([]files.CleanupTask, error) {
	const query = `
SELECT id, object_key, attempts
FROM blob_cleanups
WHERE next_retry_at_ns <= ?
ORDER BY next_retry_at_ns ASC, id ASC
LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, now.UTC().UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("query due blob cleanups: %w", err)
	}
	defer rows.Close()

	tasks := make([]files.CleanupTask, 0)
	for rows.Next() {
		var task files.CleanupTask
		if err := rows.Scan(&task.ID, &task.ObjectKey, &task.Attempts); err != nil {
			return nil, fmt.Errorf("scan blob cleanup row: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate blob cleanup rows: %w", err)
	}
	return tasks, nil
}

// AckCleanup removes a completed cleanup task from the queue.
func (s *Store) AckCleanup(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM blob_cleanups WHERE id = ?`, id); err != nil {
		return fmt.Errorf("ack blob cleanup: %w", err)
	}
	return nil
}

// FailCleanup records a failed disk deletion attempt and schedules its next retry time.
func (s *Store) FailCleanup(ctx context.Context, id int64, nextRetryAt time.Time, errMsg string) error {
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE blob_cleanups
SET attempts = attempts + 1, next_retry_at_ns = ?, last_error = ?
WHERE id = ?`,
		nextRetryAt.UTC().UnixNano(),
		errMsg,
		id,
	); err != nil {
		return fmt.Errorf("record blob cleanup failure: %w", err)
	}
	return nil
}

// ResetCleanupRetries marks previously backed-off cleanup tasks immediately due on process startup.
func (s *Store) ResetCleanupRetries(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE blob_cleanups SET next_retry_at_ns = 0 WHERE next_retry_at_ns > 0`); err != nil {
		return fmt.Errorf("reset blob cleanup retries: %w", err)
	}
	return nil
}

// SearchFiles searches files owned by ownerID using parameterized literal case-sensitive substring,
// byte-size bounds, optional direct parent folder filter, stable ordering, and bounded pagination.
func (s *Store) SearchFiles(ctx context.Context, ownerID string, filter files.SearchFilter) ([]files.File, error) {
	if strings.TrimSpace(ownerID) == "" {
		return nil, files.ErrInvalidOwner
	}

	tx, err := s.beginList(ctx, ownerID, filter.ParentID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		clauses = []string{"owner_id = ?"}
		args    = []any{ownerID}
	)
	if filter.ParentID != nil {
		clauses = append(clauses, "parent_id = ?")
		args = append(args, *filter.ParentID)
	}
	if filter.Query != "" {
		clauses = append(clauses, "instr(name, ?) > 0")
		args = append(args, filter.Query)
	}
	if filter.MinSize != nil {
		clauses = append(clauses, "size >= ?")
		args = append(args, *filter.MinSize)
	}
	if filter.MaxSize != nil {
		clauses = append(clauses, "size <= ?")
		args = append(args, *filter.MaxSize)
	}

	query := `SELECT id, name, parent_id, size, sha256, created_at
FROM files
WHERE ` + strings.Join(clauses, " AND ") + `
ORDER BY created_at_ns DESC, id ASC
LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search files: %w", err)
	}
	defer rows.Close()

	result := make([]files.File, 0)
	for rows.Next() {
		var (
			item         files.File
			createdAtStr string
		)
		if err := rows.Scan(&item.ID, &item.Name, &item.ParentID, &item.Size, &item.SHA256, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan search file row: %w", err)
		}
		parsedAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("parse created_at %q: %w", createdAtStr, err)
		}
		item.CreatedAt = parsedAt.UTC()
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search file rows: %w", err)
	}
	return result, nil
}
