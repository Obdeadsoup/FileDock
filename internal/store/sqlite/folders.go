package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"example.com/filedock/internal/files"
)

func migrateFolders(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS folders (
	id TEXT PRIMARY KEY NOT NULL,
	owner_id TEXT NOT NULL REFERENCES users(id),
	parent_id TEXT REFERENCES folders(id),
	name TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (owner_id, parent_id, name)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_folders_root_name
	ON folders (owner_id, name) WHERE parent_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_folders_parent_page
	ON folders (owner_id, parent_id, created_at_ns DESC, id ASC);`); err != nil {
		return fmt.Errorf("initialize folders schema: %w", err)
	}
	hasParentID, err := tableHasColumn(ctx, tx, "files", "parent_id")
	if err != nil {
		return err
	}
	if !hasParentID {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE files ADD COLUMN parent_id TEXT REFERENCES folders(id)`); err != nil {
			return fmt.Errorf("add parent_id column to files: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_files_parent_page
	ON files (owner_id, parent_id, created_at_ns DESC, id ASC);`); err != nil {
		return fmt.Errorf("create files parent index: %w", err)
	}
	return nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type scanner interface {
	Scan(...any) error
}

func checkParent(ctx context.Context, db rowQuerier, ownerID string, parentID *string) error {
	if parentID == nil {
		return nil
	}
	var found int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM folders WHERE owner_id = ? AND id = ?`, ownerID, *parentID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return files.ErrFolderNotFound
	}
	return err
}

func (s *Store) CheckFolder(ctx context.Context, ownerID, id string) error {
	return checkParent(ctx, s.db, ownerID, &id)
}

func (s *Store) beginList(ctx context.Context, ownerID string, parentID *string) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	if err := checkParent(ctx, tx, ownerID, parentID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (s *Store) CreateFolder(ctx context.Context, ownerID string, folder files.Folder) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkParent(ctx, tx, ownerID, folder.ParentID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO folders (id, owner_id, parent_id, name, created_at, created_at_ns, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, folder.ID, ownerID, folder.ParentID, folder.Name,
		folder.CreatedAt.Format(time.RFC3339Nano), folder.CreatedAt.UnixNano(), folder.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return folderWriteError(err)
	}
	return tx.Commit()
}

const folderColumns = `id, name, parent_id, created_at, updated_at`

func scanFolder(row scanner) (files.Folder, error) {
	var folder files.Folder
	var created, updated string
	if err := row.Scan(&folder.ID, &folder.Name, &folder.ParentID, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return files.Folder{}, files.ErrFolderNotFound
		}
		return files.Folder{}, err
	}
	var err error
	folder.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return files.Folder{}, err
	}
	folder.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return folder, err
}

func (s *Store) ListFolders(ctx context.Context, ownerID string, page files.ListPage) ([]files.Folder, error) {
	tx, err := s.beginList(ctx, ownerID, page.ParentID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+folderColumns+` FROM folders
WHERE owner_id = ? AND parent_id IS ? ORDER BY created_at_ns DESC, id ASC LIMIT ? OFFSET ?`,
		ownerID, page.ParentID, page.Limit, page.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]files.Folder, 0)
	for rows.Next() {
		folder, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, folder)
	}
	return items, rows.Err()
}

func (s *Store) UpdateFolder(ctx context.Context, ownerID, id string, changes files.Changes, now time.Time) (files.Folder, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return files.Folder{}, err
	}
	defer tx.Rollback()
	if err := checkParent(ctx, tx, ownerID, &id); err != nil {
		return files.Folder{}, err
	}
	if changes.SetParent && changes.ParentID != nil {
		if err := checkParent(ctx, tx, ownerID, changes.ParentID); err != nil {
			return files.Folder{}, err
		}
		var cycle bool
		err := tx.QueryRowContext(ctx, `
WITH RECURSIVE ancestors(id, parent_id) AS (
	SELECT id, parent_id FROM folders WHERE owner_id = ? AND id = ?
	UNION
	SELECT f.id, f.parent_id FROM folders f JOIN ancestors a ON f.id = a.parent_id WHERE f.owner_id = ?
)
SELECT EXISTS(SELECT 1 FROM ancestors WHERE id = ?)`, ownerID, *changes.ParentID, ownerID, id).Scan(&cycle)
		if err != nil {
			return files.Folder{}, err
		}
		if cycle {
			return files.Folder{}, files.ErrFolderCycle
		}
	}
	folder, err := scanFolder(tx.QueryRowContext(ctx, `
UPDATE folders SET name = COALESCE(?, name),
	parent_id = CASE WHEN ? THEN ? ELSE parent_id END, updated_at = ?
WHERE owner_id = ? AND id = ? RETURNING `+folderColumns,
		changes.Name, changes.SetParent, changes.ParentID, now.Format(time.RFC3339Nano), ownerID, id))
	if err != nil {
		return files.Folder{}, folderWriteError(err)
	}
	return folder, tx.Commit()
}

func folderWriteError(err error) error {
	if isUniqueViolation(err) && strings.Contains(err.Error(), "folders.owner_id") {
		return files.ErrFolderNameTaken
	}
	return err
}

func (s *Store) DeleteFolder(ctx context.Context, ownerID, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkParent(ctx, tx, ownerID, &id); err != nil {
		return err
	}
	var hasChildren bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM folders WHERE owner_id = ? AND parent_id = ?)
OR EXISTS(SELECT 1 FROM files WHERE owner_id = ? AND parent_id = ?)`, ownerID, id, ownerID, id).Scan(&hasChildren); err != nil {
		return err
	}
	if hasChildren {
		return files.ErrFolderNotEmpty
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE resource_type = 'folder' AND resource_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE owner_id = ? AND id = ?`, ownerID, id); err != nil {
		return err
	}
	return tx.Commit()
}
