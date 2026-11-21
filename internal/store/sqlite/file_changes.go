package sqlite

import (
	"context"

	"example.com/filedock/internal/files"
)

func (s *Store) UpdateFile(ctx context.Context, ownerID, id string, changes files.Changes) (files.File, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return files.File{}, err
	}
	defer tx.Rollback()
	record, err := getFile(ctx, tx, ownerID, id)
	if err != nil {
		return files.File{}, err
	}
	if changes.SetParent {
		if err := checkParent(ctx, tx, ownerID, changes.ParentID); err != nil {
			return files.File{}, err
		}
		record.ParentID = changes.ParentID
	}
	if changes.Name != nil {
		record.Name = *changes.Name
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET name = ?, parent_id = ? WHERE owner_id = ? AND id = ?`,
		record.Name, record.ParentID, ownerID, id); err != nil {
		return files.File{}, err
	}
	return record.File, tx.Commit()
}
