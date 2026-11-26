package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"example.com/filedock/internal/files"
)

const syncSchemaDDL = `
CREATE TABLE IF NOT EXISTS sync_sequences (
	owner_id TEXT PRIMARY KEY NOT NULL,
	last_seq INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sync_changes (
	owner_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	event_type TEXT NOT NULL CHECK (event_type IN ('file.upsert', 'file.delete', 'folder.upsert', 'folder.delete')),
	resource_id TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	name TEXT,
	parent_id TEXT,
	size INTEGER,
	sha256 TEXT,
	content_revision INTEGER,
	resource_created_at TEXT,
	resource_updated_at TEXT,
	PRIMARY KEY (owner_id, seq)
);`

func migrateSyncChanges(ctx context.Context, tx *sql.Tx) error {
	var existed bool
	if err := tx.QueryRowContext(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'sync_changes')`,
	).Scan(&existed); err != nil {
		return fmt.Errorf("check sync_changes table: %w", err)
	}

	if _, err := tx.ExecContext(ctx, syncSchemaDDL); err != nil {
		return fmt.Errorf("initialize sync schema: %w", err)
	}

	if existed {
		return nil
	}

	// First-time migration: backfill existing owned folders (parents before children) and owned files.
	type folderSeed struct {
		ownerID string
		folder  files.Folder
	}
	folderRows, err := tx.QueryContext(ctx, `
WITH RECURSIVE folder_tree(id, owner_id, parent_id, name, created_at, created_at_ns, updated_at, depth) AS (
	SELECT id, owner_id, parent_id, name, created_at, created_at_ns, updated_at, 0
	FROM folders
	WHERE owner_id != '' AND parent_id IS NULL
	UNION ALL
	SELECT f.id, f.owner_id, f.parent_id, f.name, f.created_at, f.created_at_ns, f.updated_at, t.depth + 1
	FROM folders f
	JOIN folder_tree t ON f.parent_id = t.id AND f.owner_id = t.owner_id
)
SELECT owner_id, id, name, parent_id, created_at, updated_at
FROM folder_tree
ORDER BY owner_id ASC, depth ASC, created_at_ns ASC, id ASC`)
	if err != nil {
		return fmt.Errorf("query folders for sync backfill: %w", err)
	}
	var seededFolders []folderSeed
	for folderRows.Next() {
		var (
			seed         folderSeed
			createdStr   string
			updatedStr   string
			parentIDNull sql.NullString
		)
		if err := folderRows.Scan(&seed.ownerID, &seed.folder.ID, &seed.folder.Name, &parentIDNull, &createdStr, &updatedStr); err != nil {
			_ = folderRows.Close()
			return fmt.Errorf("scan folder for sync backfill: %w", err)
		}
		if parentIDNull.Valid {
			p := parentIDNull.String
			seed.folder.ParentID = &p
		}
		createdAt, err := time.Parse(time.RFC3339Nano, createdStr)
		if err != nil {
			_ = folderRows.Close()
			return fmt.Errorf("parse folder created_at during sync backfill: %w", err)
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, updatedStr)
		if err != nil {
			_ = folderRows.Close()
			return fmt.Errorf("parse folder updated_at during sync backfill: %w", err)
		}
		seed.folder.CreatedAt = createdAt.UTC()
		seed.folder.UpdatedAt = updatedAt.UTC()
		seededFolders = append(seededFolders, seed)
	}
	if err := folderRows.Err(); err != nil {
		_ = folderRows.Close()
		return fmt.Errorf("iterate folders for sync backfill: %w", err)
	}
	_ = folderRows.Close()

	for _, seed := range seededFolders {
		if err := appendFolderUpsert(ctx, tx, seed.ownerID, seed.folder, seed.folder.UpdatedAt); err != nil {
			return err
		}
	}

	type fileSeed struct {
		ownerID string
		file    files.File
	}
	fileRows, err := tx.QueryContext(ctx, `
SELECT owner_id, id, name, parent_id, size, sha256, content_revision, created_at
FROM files
WHERE owner_id != '' AND owner_id IS NOT NULL
ORDER BY owner_id ASC, created_at_ns ASC, id ASC`)
	if err != nil {
		return fmt.Errorf("query files for sync backfill: %w", err)
	}
	var seededFiles []fileSeed
	for fileRows.Next() {
		var (
			seed         fileSeed
			createdStr   string
			parentIDNull sql.NullString
		)
		if err := fileRows.Scan(&seed.ownerID, &seed.file.ID, &seed.file.Name, &parentIDNull, &seed.file.Size, &seed.file.SHA256, &seed.file.ContentRevision, &createdStr); err != nil {
			_ = fileRows.Close()
			return fmt.Errorf("scan file for sync backfill: %w", err)
		}
		if parentIDNull.Valid {
			p := parentIDNull.String
			seed.file.ParentID = &p
		}
		createdAt, err := time.Parse(time.RFC3339Nano, createdStr)
		if err != nil {
			_ = fileRows.Close()
			return fmt.Errorf("parse file created_at during sync backfill: %w", err)
		}
		seed.file.CreatedAt = createdAt.UTC()
		seededFiles = append(seededFiles, seed)
	}
	if err := fileRows.Err(); err != nil {
		_ = fileRows.Close()
		return fmt.Errorf("iterate files for sync backfill: %w", err)
	}
	_ = fileRows.Close()

	for _, seed := range seededFiles {
		if err := appendFileUpsert(ctx, tx, seed.ownerID, seed.file, seed.file.CreatedAt); err != nil {
			return err
		}
	}

	return nil
}

func nextOwnerSeq(ctx context.Context, tx *sql.Tx, ownerID string) (int64, error) {
	var seq int64
	if err := tx.QueryRowContext(ctx, `
INSERT INTO sync_sequences (owner_id, last_seq) VALUES (?, 1)
ON CONFLICT(owner_id) DO UPDATE SET last_seq = last_seq + 1
RETURNING last_seq`, ownerID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("allocate sync sequence: %w", err)
	}
	return seq, nil
}

func appendFileUpsert(ctx context.Context, tx *sql.Tx, ownerID string, file files.File, occurredAt time.Time) error {
	seq, err := nextOwnerSeq(ctx, tx, ownerID)
	if err != nil {
		return err
	}
	rev := file.ContentRevision
	if rev <= 0 {
		rev = 1
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO sync_changes (
	owner_id, seq, event_type, resource_id, occurred_at,
	name, parent_id, size, sha256, content_revision, resource_created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ownerID,
		seq,
		files.ChangeFileUpsert,
		file.ID,
		occurredAt.UTC().Format(time.RFC3339Nano),
		file.Name,
		file.ParentID,
		file.Size,
		file.SHA256,
		rev,
		file.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert file.upsert sync change: %w", err)
	}
	return nil
}

func appendFileDelete(ctx context.Context, tx *sql.Tx, ownerID, fileID string, occurredAt time.Time) error {
	seq, err := nextOwnerSeq(ctx, tx, ownerID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO sync_changes (owner_id, seq, event_type, resource_id, occurred_at)
VALUES (?, ?, ?, ?, ?)`,
		ownerID,
		seq,
		files.ChangeFileDelete,
		fileID,
		occurredAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert file.delete sync change: %w", err)
	}
	return nil
}

func appendFolderUpsert(ctx context.Context, tx *sql.Tx, ownerID string, folder files.Folder, occurredAt time.Time) error {
	seq, err := nextOwnerSeq(ctx, tx, ownerID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO sync_changes (
	owner_id, seq, event_type, resource_id, occurred_at,
	name, parent_id, resource_created_at, resource_updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ownerID,
		seq,
		files.ChangeFolderUpsert,
		folder.ID,
		occurredAt.UTC().Format(time.RFC3339Nano),
		folder.Name,
		folder.ParentID,
		folder.CreatedAt.UTC().Format(time.RFC3339Nano),
		folder.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert folder.upsert sync change: %w", err)
	}
	return nil
}

func appendFolderDelete(ctx context.Context, tx *sql.Tx, ownerID, folderID string, occurredAt time.Time) error {
	seq, err := nextOwnerSeq(ctx, tx, ownerID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO sync_changes (owner_id, seq, event_type, resource_id, occurred_at)
VALUES (?, ?, ?, ?, ?)`,
		ownerID,
		seq,
		files.ChangeFolderDelete,
		folderID,
		occurredAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert folder.delete sync change: %w", err)
	}
	return nil
}

// ListChanges returns owner-scoped sync changes strictly after the exclusive cursor in ascending seq order.
// It queries limit+1 rows to determine has_more accurately without skipping concurrently inserted events.
func (s *Store) ListChanges(ctx context.Context, ownerID string, after int64, limit int) (files.ChangePage, error) {
	if strings.TrimSpace(ownerID) == "" {
		return files.ChangePage{}, files.ErrInvalidOwner
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return files.ChangePage{}, fmt.Errorf("begin list sync changes: %w", err)
	}
	defer tx.Rollback()

	const query = `
SELECT seq, event_type, resource_id, occurred_at,
       name, parent_id, size, sha256, content_revision, resource_created_at, resource_updated_at
FROM sync_changes
WHERE owner_id = ? AND seq > ?
ORDER BY seq ASC
LIMIT ?`

	rows, err := tx.QueryContext(ctx, query, ownerID, after, limit+1)
	if err != nil {
		return files.ChangePage{}, fmt.Errorf("query sync changes: %w", err)
	}
	defer rows.Close()

	items := make([]files.ChangeEvent, 0, limit)
	hasMore := false
	for rows.Next() {
		if len(items) == limit {
			hasMore = true
			break
		}
		var (
			ev         files.ChangeEvent
			occurred   string
			name       sql.NullString
			parentID   sql.NullString
			size       sql.NullInt64
			sha        sql.NullString
			contentRev sql.NullInt64
			resCreated sql.NullString
			resUpdated sql.NullString
		)
		if err := rows.Scan(
			&ev.Seq,
			&ev.Type,
			&ev.ResourceID,
			&occurred,
			&name,
			&parentID,
			&size,
			&sha,
			&contentRev,
			&resCreated,
			&resUpdated,
		); err != nil {
			return files.ChangePage{}, fmt.Errorf("scan sync change row: %w", err)
		}

		parsedOccurred, err := time.Parse(time.RFC3339Nano, occurred)
		if err != nil {
			return files.ChangePage{}, fmt.Errorf("parse sync occurred_at %q: %w", occurred, err)
		}
		ev.OccurredAt = parsedOccurred.UTC()

		var parentPtr *string
		if parentID.Valid {
			p := parentID.String
			parentPtr = &p
		}

		switch ev.Type {
		case files.ChangeFileUpsert:
			createdAt, err := time.Parse(time.RFC3339Nano, resCreated.String)
			if err != nil {
				return files.ChangePage{}, fmt.Errorf("parse sync file created_at %q: %w", resCreated.String, err)
			}
			rev := int64(1)
			if contentRev.Valid && contentRev.Int64 > 0 {
				rev = contentRev.Int64
			}
			ev.File = &files.File{
				ID:              ev.ResourceID,
				Name:            name.String,
				ParentID:        parentPtr,
				Size:            size.Int64,
				SHA256:          sha.String,
				ContentRevision: rev,
				CreatedAt:       createdAt.UTC(),
			}
		case files.ChangeFolderUpsert:
			createdAt, err := time.Parse(time.RFC3339Nano, resCreated.String)
			if err != nil {
				return files.ChangePage{}, fmt.Errorf("parse sync folder created_at %q: %w", resCreated.String, err)
			}
			updatedAt, err := time.Parse(time.RFC3339Nano, resUpdated.String)
			if err != nil {
				return files.ChangePage{}, fmt.Errorf("parse sync folder updated_at %q: %w", resUpdated.String, err)
			}
			ev.Folder = &files.Folder{
				ID:        ev.ResourceID,
				Name:      name.String,
				ParentID:  parentPtr,
				CreatedAt: createdAt.UTC(),
				UpdatedAt: updatedAt.UTC(),
			}
		}

		items = append(items, ev)
	}
	if err := rows.Err(); err != nil {
		return files.ChangePage{}, fmt.Errorf("iterate sync change rows: %w", err)
	}

	nextCursor := after
	if len(items) > 0 {
		nextCursor = items[len(items)-1].Seq
	}
	return files.ChangePage{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}
