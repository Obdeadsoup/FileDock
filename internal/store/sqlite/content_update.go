package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"example.com/filedock/internal/files"
)

func migrateFileContentRevision(ctx context.Context, tx *sql.Tx) error {
	hasCol, err := tableHasColumn(ctx, tx, "files", "content_revision")
	if err != nil {
		return err
	}
	if !hasCol {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE files ADD COLUMN content_revision INTEGER NOT NULL DEFAULT 1`); err != nil {
			return fmt.Errorf("add content_revision column to files: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET content_revision = 1 WHERE content_revision IS NULL OR content_revision < 1`); err != nil {
		return fmt.Errorf("normalize files content_revision: %w", err)
	}
	return nil
}

func migrateSyncContentRevision(ctx context.Context, tx *sql.Tx) error {
	hasCol, err := tableHasColumn(ctx, tx, "sync_changes", "content_revision")
	if err != nil {
		return err
	}
	if !hasCol {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE sync_changes ADD COLUMN content_revision INTEGER`); err != nil {
			return fmt.Errorf("add content_revision column to sync_changes: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sync_changes
SET content_revision = 1
WHERE event_type = 'file.upsert' AND (content_revision IS NULL OR content_revision < 1)`); err != nil {
		return fmt.Errorf("normalize sync_changes content_revision: %w", err)
	}
	return nil
}

// ReplaceFileContent atomically verifies expectedETag inside the write transaction, updates file metadata
// and content_revision, enqueues the replaced object_key into blob_cleanups, and records a file.upsert event.
func (s *Store) ReplaceFileContent(
	ctx context.Context,
	ownerID, id, expectedETag string,
	staged files.StagedBlob,
	now time.Time,
) (files.File, error) {
	if strings.TrimSpace(ownerID) == "" {
		return files.File{}, files.ErrInvalidOwner
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return files.File{}, fmt.Errorf("begin file content update: %w", err)
	}
	defer tx.Rollback()

	current, err := getFile(ctx, tx, ownerID, id)
	if err != nil {
		return files.File{}, err
	}
	if files.ContentETag(current.File) != expectedETag {
		return files.File{}, files.ErrPreconditionFailed
	}

	curRev := current.ContentRevision
	if curRev < 1 {
		curRev = 1
	}
	nextRev := curRev + 1

	res, err := tx.ExecContext(ctx, `
UPDATE files
SET size = ?, sha256 = ?, object_key = ?, content_revision = ?
WHERE owner_id = ? AND id = ? AND content_revision = ?`,
		staged.Size,
		staged.SHA256,
		staged.ObjectKey,
		nextRev,
		ownerID,
		id,
		curRev,
	)
	if err != nil {
		return files.File{}, fmt.Errorf("update file content metadata: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return files.File{}, fmt.Errorf("file content update rows affected: %w", err)
	}
	if affected == 0 {
		return files.File{}, files.ErrPreconditionFailed
	}

	nowUTC := now.UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO blob_cleanups (object_key, created_at, created_at_ns, attempts, next_retry_at_ns, last_error)
VALUES (?, ?, ?, 0, 0, '')`,
		current.ObjectKey,
		nowUTC.Format(time.RFC3339Nano),
		nowUTC.UnixNano(),
	); err != nil {
		return files.File{}, fmt.Errorf("enqueue replaced blob cleanup: %w", err)
	}

	updated := current.File
	updated.Size = staged.Size
	updated.SHA256 = staged.SHA256
	updated.ContentRevision = nextRev

	if err := appendFileUpsert(ctx, tx, ownerID, updated, nowUTC); err != nil {
		return files.File{}, err
	}

	if err := tx.Commit(); err != nil {
		return files.File{}, fmt.Errorf("commit file content update: %w", err)
	}
	return updated, nil
}
