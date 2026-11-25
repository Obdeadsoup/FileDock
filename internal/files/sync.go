package files

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	ChangeFileUpsert   = "file.upsert"
	ChangeFileDelete   = "file.delete"
	ChangeFolderUpsert = "folder.upsert"
	ChangeFolderDelete = "folder.delete"
)

var ErrInvalidSyncCursor = errors.New("invalid sync cursor")

// ChangeEvent represents a single owner-scoped incremental sync event.
type ChangeEvent struct {
	Seq        int64
	Type       string
	ResourceID string
	OccurredAt time.Time
	File       *File
	Folder     *Folder
}

// ChangePage is a bounded page of change events ordered by ascending Seq.
type ChangePage struct {
	Items      []ChangeEvent
	NextCursor int64
	HasMore    bool
}

// SyncStore reads owner-scoped incremental change events.
type SyncStore interface {
	ListChanges(ctx context.Context, ownerID string, after int64, limit int) (ChangePage, error)
}

// ListChanges returns owner-scoped sync change events strictly after cursor in ascending seq order.
func (s *Service) ListChanges(ctx context.Context, ownerID string, after int64, limit int) (ChangePage, error) {
	if err := ValidateID(ownerID); err != nil {
		return ChangePage{}, fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if after < 0 {
		return ChangePage{}, fmt.Errorf("%w: after must be non-negative", ErrInvalidSyncCursor)
	}
	page, err := newPage(nil, limit, 0)
	if err != nil {
		return ChangePage{}, err
	}

	result, err := s.meta.ListChanges(ctx, ownerID, after, page.Limit)
	if err != nil {
		return ChangePage{}, err
	}
	if result.Items == nil {
		result.Items = []ChangeEvent{}
	}
	return result, nil
}
