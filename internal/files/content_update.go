package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	ErrInvalidIfMatch     = errors.New("If-Match must be a concrete strong ETag")
	ErrPreconditionFailed = errors.New("file content version precondition failed")
)

// ContentUpdateStore atomically replaces file content metadata with version verification,
// old object cleanup enqueue, and sync event recording in a single write transaction.
type ContentUpdateStore interface {
	ReplaceFileContent(ctx context.Context, ownerID, id, expectedETag string, staged StagedBlob, now time.Time) (File, error)
}

// ContentETag returns the strong HTTP ETag representing the file's current content version.
func ContentETag(f File) string {
	rev := f.ContentRevision
	if rev <= 0 {
		rev = 1
	}
	return fmt.Sprintf(`"%s:%d:%s"`, f.ID, rev, f.SHA256)
}

// ValidateStrongETag checks that raw is a single concrete strong quoted ETag (not "*" or weak "W/...").
func ValidateStrongETag(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) < 3 || trimmed[0] != '"' || trimmed[len(trimmed)-1] != '"' {
		return ErrInvalidIfMatch
	}
	inner := trimmed[1 : len(trimmed)-1]
	if inner == "" || strings.ContainsAny(inner, `",`) {
		return ErrInvalidIfMatch
	}
	for i := 0; i < len(inner); i++ {
		if inner[i] < 0x21 || inner[i] == 0x7f {
			return ErrInvalidIfMatch
		}
	}
	return nil
}

// ReplaceContent streams new bytes for an existing file if its current strong ETag matches expectedETag.
// It pre-checks ownership and version before streaming to disk, re-verifies version inside the SQLite
// write transaction, and cleans up the newly staged blob if the condition fails or the transaction rolls back.
func (s *Service) ReplaceContent(ctx context.Context, ownerID, id, expectedETag string, r io.Reader) (File, error) {
	if err := ValidateID(ownerID); err != nil {
		return File{}, fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if err := ValidateID(id); err != nil {
		return File{}, err
	}
	if err := ValidateStrongETag(expectedETag); err != nil {
		return File{}, err
	}
	if r == nil {
		return File{}, fmt.Errorf("%w: empty reader", ErrInvalidFilename)
	}

	// Pre-check ownership and current version before streaming the request body to disk.
	current, err := s.meta.GetByID(ctx, ownerID, id)
	if err != nil {
		return File{}, err
	}
	if ContentETag(current.File) != expectedETag {
		return File{}, ErrPreconditionFailed
	}

	staged, err := s.blobs.Save(ctx, r, s.maxFileSize)
	if err != nil {
		return File{}, err
	}

	updated, err := s.meta.ReplaceFileContent(ctx, ownerID, id, expectedETag, staged, s.now().UTC())
	if err != nil {
		if delErr := s.blobs.Delete(context.WithoutCancel(ctx), staged.ObjectKey); delErr != nil {
			return File{}, errors.Join(err, fmt.Errorf("cleanup new blob %q: %w", staged.ObjectKey, delErr))
		}
		return File{}, err
	}

	select {
	case s.cleanupNotify <- struct{}{}:
	default:
	}
	return updated, nil
}
