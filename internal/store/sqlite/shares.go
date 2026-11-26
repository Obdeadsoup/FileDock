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

func migrateShares(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS shares (
	id TEXT PRIMARY KEY NOT NULL,
	owner_id TEXT NOT NULL REFERENCES users(id),
	recipient_id TEXT NOT NULL REFERENCES users(id),
	resource_type TEXT NOT NULL CHECK (resource_type IN ('file', 'folder')),
	resource_id TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	UNIQUE (owner_id, recipient_id, resource_type, resource_id)
);
CREATE INDEX IF NOT EXISTS idx_shares_owner_page
	ON shares (owner_id, created_at_ns DESC, id ASC);
CREATE INDEX IF NOT EXISTS idx_shares_recipient_page
	ON shares (recipient_id, created_at_ns DESC, id ASC);
CREATE INDEX IF NOT EXISTS idx_shares_resource
	ON shares (resource_type, resource_id);`); err != nil {
		return fmt.Errorf("initialize shares schema: %w", err)
	}
	return nil
}

type shareRow struct {
	ID           string
	OwnerID      string
	RecipientID  string
	ResourceType string
	ResourceID   string
	CreatedAt    time.Time
}

func getShareForRecipient(ctx context.Context, db rowQuerier, recipientID, shareID string) (shareRow, error) {
	const query = `
SELECT id, owner_id, recipient_id, resource_type, resource_id, created_at
FROM shares
WHERE id = ? AND recipient_id = ?`
	var (
		row          shareRow
		createdAtStr string
	)
	err := db.QueryRowContext(ctx, query, shareID, recipientID).Scan(
		&row.ID,
		&row.OwnerID,
		&row.RecipientID,
		&row.ResourceType,
		&row.ResourceID,
		&createdAtStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shareRow{}, files.ErrShareNotFound
		}
		return shareRow{}, fmt.Errorf("get share: %w", err)
	}
	parsedAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return shareRow{}, fmt.Errorf("parse share created_at %q: %w", createdAtStr, err)
	}
	row.CreatedAt = parsedAt.UTC()
	return row, nil
}

func isFolderInSubtree(ctx context.Context, db rowQuerier, ownerID, rootFolderID, candidateFolderID string) (bool, error) {
	var inSubtree bool
	err := db.QueryRowContext(ctx, `
WITH RECURSIVE ancestors(id, parent_id) AS (
	SELECT id, parent_id FROM folders WHERE owner_id = ? AND id = ?
	UNION
	SELECT f.id, f.parent_id FROM folders f JOIN ancestors a ON f.id = a.parent_id WHERE f.owner_id = ?
)
SELECT EXISTS(SELECT 1 FROM ancestors WHERE id = ?)`,
		ownerID, candidateFolderID, ownerID, rootFolderID,
	).Scan(&inSubtree)
	if err != nil {
		return false, fmt.Errorf("check folder subtree: %w", err)
	}
	return inSubtree, nil
}

// CreateShare creates a read-only share or returns the existing authorization if one already exists.
func (s *Store) CreateShare(ctx context.Context, ownerID, shareID string, input files.CreateShareInput, now time.Time) (files.Share, bool, error) {
	if strings.TrimSpace(ownerID) == "" {
		return files.Share{}, false, files.ErrInvalidOwner
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return files.Share{}, false, fmt.Errorf("begin create share: %w", err)
	}
	defer tx.Rollback()

	var ownerUsername string
	if err := tx.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, ownerID).Scan(&ownerUsername); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return files.Share{}, false, files.ErrInvalidOwner
		}
		return files.Share{}, false, fmt.Errorf("lookup owner username: %w", err)
	}

	var recipientID, recipientUsername string
	if err := tx.QueryRowContext(ctx, `SELECT id, username FROM users WHERE username = ?`, input.RecipientUsername).
		Scan(&recipientID, &recipientUsername); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return files.Share{}, false, files.ErrRecipientNotFound
		}
		return files.Share{}, false, fmt.Errorf("lookup recipient %q: %w", input.RecipientUsername, err)
	}
	if recipientID == ownerID {
		return files.Share{}, false, files.ErrSelfShare
	}

	var resourceName string
	switch input.ResourceType {
	case files.ResourceTypeFile:
		if err := tx.QueryRowContext(ctx, `SELECT name FROM files WHERE id = ? AND owner_id = ?`, input.ResourceID, ownerID).
			Scan(&resourceName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return files.Share{}, false, files.ErrNotFound
			}
			return files.Share{}, false, fmt.Errorf("lookup shared file: %w", err)
		}
	case files.ResourceTypeFolder:
		if err := tx.QueryRowContext(ctx, `SELECT name FROM folders WHERE id = ? AND owner_id = ?`, input.ResourceID, ownerID).
			Scan(&resourceName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return files.Share{}, false, files.ErrFolderNotFound
			}
			return files.Share{}, false, fmt.Errorf("lookup shared folder: %w", err)
		}
	default:
		return files.Share{}, false, files.ErrInvalidResourceType
	}

	// Return existing authorization if already granted for this resource and recipient.
	var (
		existingID        string
		existingCreatedAt string
	)
	err = tx.QueryRowContext(ctx, `
SELECT id, created_at
FROM shares
WHERE owner_id = ? AND recipient_id = ? AND resource_type = ? AND resource_id = ?`,
		ownerID, recipientID, input.ResourceType, input.ResourceID,
	).Scan(&existingID, &existingCreatedAt)
	if err == nil {
		parsedAt, parseErr := time.Parse(time.RFC3339Nano, existingCreatedAt)
		if parseErr != nil {
			return files.Share{}, false, fmt.Errorf("parse existing share created_at: %w", parseErr)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return files.Share{}, false, commitErr
		}
		return files.Share{
			ID:                existingID,
			ResourceType:      input.ResourceType,
			ResourceID:        input.ResourceID,
			ResourceName:      resourceName,
			OwnerID:           ownerID,
			OwnerUsername:     ownerUsername,
			RecipientID:       recipientID,
			RecipientUsername: recipientUsername,
			Permission:        files.SharePermissionRead,
			CreatedAt:         parsedAt.UTC(),
		}, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return files.Share{}, false, fmt.Errorf("check existing share: %w", err)
	}

	nowUTC := now.UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO shares (id, owner_id, recipient_id, resource_type, resource_id, created_at, created_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		shareID,
		ownerID,
		recipientID,
		input.ResourceType,
		input.ResourceID,
		nowUTC.Format(time.RFC3339Nano),
		nowUTC.UnixNano(),
	); err != nil {
		return files.Share{}, false, fmt.Errorf("insert share: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return files.Share{}, false, err
	}

	return files.Share{
		ID:                shareID,
		ResourceType:      input.ResourceType,
		ResourceID:        input.ResourceID,
		ResourceName:      resourceName,
		OwnerID:           ownerID,
		OwnerUsername:     ownerUsername,
		RecipientID:       recipientID,
		RecipientUsername: recipientUsername,
		Permission:        files.SharePermissionRead,
		CreatedAt:         nowUTC,
	}, true, nil
}

// ListShares lists sent or received shares for userID with stable ordering and pagination.
func (s *Store) ListShares(ctx context.Context, userID, direction string, limit, offset int) ([]files.Share, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, files.ErrInvalidOwner
	}

	var filterColumn string
	switch direction {
	case files.ShareDirectionSent:
		filterColumn = "s.owner_id"
	case files.ShareDirectionReceived:
		filterColumn = "s.recipient_id"
	default:
		return nil, files.ErrInvalidShareDirection
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin list shares: %w", err)
	}
	defer tx.Rollback()

	query := `
SELECT s.id, s.resource_type, s.resource_id,
       COALESCE(f.name, d.name, ''),
       s.owner_id, ou.username,
       s.recipient_id, ru.username,
       s.created_at
FROM shares s
JOIN users ou ON ou.id = s.owner_id
JOIN users ru ON ru.id = s.recipient_id
LEFT JOIN files f ON s.resource_type = 'file' AND f.id = s.resource_id AND f.owner_id = s.owner_id
LEFT JOIN folders d ON s.resource_type = 'folder' AND d.id = s.resource_id AND d.owner_id = s.owner_id
WHERE ` + filterColumn + ` = ?
  AND ((s.resource_type = 'file' AND f.id IS NOT NULL) OR (s.resource_type = 'folder' AND d.id IS NOT NULL))
ORDER BY s.created_at_ns DESC, s.id ASC
LIMIT ? OFFSET ?`

	rows, err := tx.QueryContext(ctx, query, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query shares: %w", err)
	}
	defer rows.Close()

	result := make([]files.Share, 0)
	for rows.Next() {
		var (
			item         files.Share
			createdAtStr string
		)
		if err := rows.Scan(
			&item.ID,
			&item.ResourceType,
			&item.ResourceID,
			&item.ResourceName,
			&item.OwnerID,
			&item.OwnerUsername,
			&item.RecipientID,
			&item.RecipientUsername,
			&createdAtStr,
		); err != nil {
			return nil, fmt.Errorf("scan share row: %w", err)
		}
		parsedAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("parse share created_at %q: %w", createdAtStr, err)
		}
		item.Permission = files.SharePermissionRead
		item.CreatedAt = parsedAt.UTC()
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate share rows: %w", err)
	}
	return result, nil
}

// DeleteShare revokes a share owned by ownerID.
func (s *Store) DeleteShare(ctx context.Context, ownerID, shareID string) error {
	if strings.TrimSpace(ownerID) == "" {
		return files.ErrInvalidOwner
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM shares WHERE id = ? AND owner_id = ?`, shareID, ownerID)
	if err != nil {
		return fmt.Errorf("delete share: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("share delete rows affected: %w", err)
	}
	if affected == 0 {
		return files.ErrShareNotFound
	}
	return nil
}

// ListSharedFolders lists direct subfolders inside a shared folder subtree within a single read transaction.
func (s *Store) ListSharedFolders(ctx context.Context, recipientID, shareID string, page files.ListPage) ([]files.Folder, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin list shared folders: %w", err)
	}
	defer tx.Rollback()

	share, err := getShareForRecipient(ctx, tx, recipientID, shareID)
	if err != nil {
		return nil, err
	}
	if share.ResourceType != files.ResourceTypeFolder {
		return nil, files.ErrShareNotFound
	}
	if err := checkParent(ctx, tx, share.OwnerID, &share.ResourceID); err != nil {
		return nil, files.ErrShareNotFound
	}

	targetFolderID := share.ResourceID
	if page.ParentID != nil {
		inSubtree, err := isFolderInSubtree(ctx, tx, share.OwnerID, share.ResourceID, *page.ParentID)
		if err != nil {
			return nil, err
		}
		if !inSubtree {
			return nil, files.ErrFolderNotFound
		}
		targetFolderID = *page.ParentID
	}

	rows, err := tx.QueryContext(ctx, `SELECT `+folderColumns+` FROM folders
WHERE owner_id = ? AND parent_id = ?
ORDER BY created_at_ns DESC, id ASC
LIMIT ? OFFSET ?`, share.OwnerID, targetFolderID, page.Limit, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("query shared folders: %w", err)
	}
	defer rows.Close()

	items := make([]files.Folder, 0)
	for rows.Next() {
		folder, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		// Treat the shared root folder as the recipient's logical root; never expose external parents.
		if folder.ParentID != nil && *folder.ParentID == share.ResourceID {
			folder.ParentID = nil
		}
		items = append(items, folder)
	}
	return items, rows.Err()
}

// ListSharedFiles lists direct files inside a shared folder subtree (or the single shared file) within a single read transaction.
func (s *Store) ListSharedFiles(ctx context.Context, recipientID, shareID string, page files.ListPage) ([]files.File, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin list shared files: %w", err)
	}
	defer tx.Rollback()

	share, err := getShareForRecipient(ctx, tx, recipientID, shareID)
	if err != nil {
		return nil, err
	}

	if share.ResourceType == files.ResourceTypeFile {
		if page.ParentID != nil {
			return nil, files.ErrFolderNotFound
		}
		record, err := getFile(ctx, tx, share.OwnerID, share.ResourceID)
		if err != nil {
			return nil, files.ErrNotFound
		}
		record.File.ParentID = nil
		if page.Offset > 0 {
			return []files.File{}, nil
		}
		return []files.File{record.File}, nil
	}

	if err := checkParent(ctx, tx, share.OwnerID, &share.ResourceID); err != nil {
		return nil, files.ErrShareNotFound
	}

	targetFolderID := share.ResourceID
	if page.ParentID != nil {
		inSubtree, err := isFolderInSubtree(ctx, tx, share.OwnerID, share.ResourceID, *page.ParentID)
		if err != nil {
			return nil, err
		}
		if !inSubtree {
			return nil, files.ErrFolderNotFound
		}
		targetFolderID = *page.ParentID
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, name, parent_id, size, sha256, content_revision, created_at
FROM files
WHERE owner_id = ? AND parent_id = ?
ORDER BY created_at_ns DESC, id ASC
LIMIT ? OFFSET ?`, share.OwnerID, targetFolderID, page.Limit, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("query shared files: %w", err)
	}
	defer rows.Close()

	result := make([]files.File, 0)
	for rows.Next() {
		var (
			item         files.File
			createdAtStr string
		)
		if err := rows.Scan(&item.ID, &item.Name, &item.ParentID, &item.Size, &item.SHA256, &item.ContentRevision, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan shared file row: %w", err)
		}
		parsedAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("parse created_at %q: %w", createdAtStr, err)
		}
		item.CreatedAt = parsedAt.UTC()
		if item.ParentID != nil && *item.ParentID == share.ResourceID {
			item.ParentID = nil
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// GetSharedFile verifies recipient share authorization and subtree membership in a single read transaction.
func (s *Store) GetSharedFile(ctx context.Context, recipientID, shareID, fileID string) (files.StoredFile, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return files.StoredFile{}, fmt.Errorf("begin get shared file: %w", err)
	}
	defer tx.Rollback()

	share, err := getShareForRecipient(ctx, tx, recipientID, shareID)
	if err != nil {
		return files.StoredFile{}, files.ErrNotFound
	}

	record, err := getFile(ctx, tx, share.OwnerID, fileID)
	if err != nil {
		return files.StoredFile{}, files.ErrNotFound
	}

	switch share.ResourceType {
	case files.ResourceTypeFile:
		if record.ID != share.ResourceID {
			return files.StoredFile{}, files.ErrNotFound
		}
		record.File.ParentID = nil
	case files.ResourceTypeFolder:
		if err := checkParent(ctx, tx, share.OwnerID, &share.ResourceID); err != nil {
			return files.StoredFile{}, files.ErrNotFound
		}
		if record.ParentID == nil {
			return files.StoredFile{}, files.ErrNotFound
		}
		inSubtree, err := isFolderInSubtree(ctx, tx, share.OwnerID, share.ResourceID, *record.ParentID)
		if err != nil {
			return files.StoredFile{}, err
		}
		if !inSubtree {
			return files.StoredFile{}, files.ErrNotFound
		}
		if *record.ParentID == share.ResourceID {
			record.File.ParentID = nil
		}
	default:
		return files.StoredFile{}, files.ErrNotFound
	}

	return record, nil
}
