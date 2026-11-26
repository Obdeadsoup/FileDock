package files

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultCleanupBatchSize is the default number of queued disk cleanup tasks processed per batch.
	DefaultCleanupBatchSize = 32
	// DefaultCleanupInterval is how often the background worker polls for due cleanup tasks.
	DefaultCleanupInterval = 2 * time.Second

	defaultCleanupBaseDelay = 1 * time.Second
	defaultCleanupMaxDelay  = 60 * time.Second
)

var ErrInvalidSearchFilter = errors.New("invalid search filter")

// SearchFilter specifies owner-scoped file search criteria and pagination.
type SearchFilter struct {
	Query    string
	MinSize  *int64
	MaxSize  *int64
	ParentID *string
	Limit    int
	Offset   int
}

// CleanupTask represents a queued disk object deletion persisted in metadata storage.
type CleanupTask struct {
	ID        int64
	ObjectKey string
	Attempts  int
}

// CleanupAndSearchStore extends MetadataStore with transactional file deletion,
// disk cleanup task tracking, and owner-scoped file search.
type CleanupAndSearchStore interface {
	DeleteFile(ctx context.Context, ownerID, id string, now time.Time) error
	ListDueCleanups(ctx context.Context, now time.Time, limit int) ([]CleanupTask, error)
	AckCleanup(ctx context.Context, id int64) error
	FailCleanup(ctx context.Context, id int64, nextRetryAt time.Time, errMsg string) error
	ResetCleanupRetries(ctx context.Context) error
	SearchFiles(ctx context.Context, ownerID string, filter SearchFilter) ([]File, error)
}

// WithCleanupBackoff overrides the retry backoff delays for failed disk cleanup tasks.
func WithCleanupBackoff(base, max time.Duration) Option {
	return func(s *Service) {
		if base > 0 {
			s.cleanupBaseDelay = base
		}
		if max > 0 {
			s.cleanupMaxDelay = max
		}
		if s.cleanupMaxDelay < s.cleanupBaseDelay {
			s.cleanupMaxDelay = s.cleanupBaseDelay
		}
	}
}

// DeleteFile atomically deletes file metadata and enqueues its object key for asynchronous disk cleanup.
func (s *Service) DeleteFile(ctx context.Context, ownerID, id string) error {
	if err := validateLocation(ownerID, &id); err != nil {
		return err
	}
	if err := s.meta.DeleteFile(ctx, ownerID, id, s.now().UTC()); err != nil {
		return err
	}
	select {
	case s.cleanupNotify <- struct{}{}:
	default:
	}
	return nil
}

// Search returns a bounded page of files owned by ownerID matching the literal substring, size, and optional directory filter.
func (s *Service) Search(ctx context.Context, ownerID string, filter SearchFilter) ([]File, error) {
	if err := validateLocation(ownerID, filter.ParentID); err != nil {
		return nil, err
	}
	if filter.MinSize != nil && *filter.MinSize < 0 {
		return nil, fmt.Errorf("%w: min_size must be non-negative", ErrInvalidSearchFilter)
	}
	if filter.MaxSize != nil && *filter.MaxSize < 0 {
		return nil, fmt.Errorf("%w: max_size must be non-negative", ErrInvalidSearchFilter)
	}
	if filter.MinSize != nil && filter.MaxSize != nil && *filter.MinSize > *filter.MaxSize {
		return nil, fmt.Errorf("%w: min_size must not exceed max_size", ErrInvalidSearchFilter)
	}
	page, err := newPage(filter.ParentID, filter.Limit, filter.Offset)
	if err != nil {
		return nil, err
	}
	filter.Limit = page.Limit
	filter.Offset = page.Offset

	items, err := s.meta.SearchFiles(ctx, ownerID, filter)
	if err != nil {
		return nil, err
	}
	if items == nil {
		return []File{}, nil
	}
	return items, nil
}

// ProcessBlobCleanups processes up to batchSize due disk cleanup tasks outside any SQLite write transaction.
// A failing task is rescheduled with backoff so it never blocks other pending cleanup tasks.
func (s *Service) ProcessBlobCleanups(ctx context.Context, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = DefaultCleanupBatchSize
	}
	tasks, err := s.meta.ListDueCleanups(ctx, s.now().UTC(), batchSize)
	if err != nil {
		return 0, err
	}

	var (
		acked int
		errs  []error
	)
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		delErr := s.blobs.Delete(ctx, task.ObjectKey)
		if delErr == nil || errors.Is(delErr, ErrNotFound) {
			if ackErr := s.meta.AckCleanup(ctx, task.ID); ackErr != nil {
				errs = append(errs, fmt.Errorf("ack cleanup %d: %w", task.ID, ackErr))
				continue
			}
			acked++
			continue
		}

		nextRetry := s.now().UTC().Add(s.cleanupRetryDelay(task.Attempts))
		if failErr := s.meta.FailCleanup(ctx, task.ID, nextRetry, delErr.Error()); failErr != nil {
			errs = append(errs, fmt.Errorf("record cleanup failure %d: %w", task.ID, failErr))
		}
		errs = append(errs, fmt.Errorf("delete blob %q: %w", task.ObjectKey, delErr))
	}
	return acked, errors.Join(errs...)
}

// RunCleanupWorker processes pending cleanup tasks immediately on startup and then periodically
// or when notified by DeleteFile until ctx is canceled.
func (s *Service) RunCleanupWorker(ctx context.Context, interval time.Duration, batchSize int, onError func(error)) {
	if interval <= 0 {
		interval = DefaultCleanupInterval
	}
	if batchSize <= 0 {
		batchSize = DefaultCleanupBatchSize
	}
	if err := s.meta.ResetCleanupRetries(ctx); err != nil && onError != nil && ctx.Err() == nil {
		onError(err)
	}

	drain := func() {
		for ctx.Err() == nil {
			acked, err := s.ProcessBlobCleanups(ctx, batchSize)
			if err != nil && onError != nil && ctx.Err() == nil {
				onError(err)
			}
			if acked < batchSize {
				break
			}
		}
		for ctx.Err() == nil {
			cleaned, err := s.ProcessExpiredUploadSessions(ctx, batchSize)
			if err != nil && onError != nil && ctx.Err() == nil {
				onError(err)
			}
			if cleaned < batchSize {
				return
			}
		}
	}

	drain()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.cleanupNotify:
			drain()
		case <-ticker.C:
			drain()
		}
	}
}

func (s *Service) cleanupRetryDelay(attempts int) time.Duration {
	delay := s.cleanupBaseDelay
	if delay <= 0 {
		delay = defaultCleanupBaseDelay
	}
	maxDelay := s.cleanupMaxDelay
	if maxDelay < delay {
		maxDelay = delay
	}
	for i := 0; i < attempts && delay < maxDelay; i++ {
		if delay > maxDelay/2 {
			return maxDelay
		}
		delay *= 2
	}
	return min(delay, maxDelay)
}
