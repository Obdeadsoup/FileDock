package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultUploadChunkSize is the default server-assigned chunk size (4 MiB).
	DefaultUploadChunkSize int64 = 4 << 20
	// DefaultUploadSessionTTL is how long an open upload session stays valid before expiring.
	DefaultUploadSessionTTL = 24 * time.Hour
	// DefaultMaxUploadStagingBytes caps the total declared size of active open sessions per owner (256 MiB).
	DefaultMaxUploadStagingBytes int64 = 256 << 20
	// DefaultMaxActiveUploadSessions caps concurrent open upload sessions per owner.
	DefaultMaxActiveUploadSessions = 16
	// DefaultUploadCleanupBatchSize is the number of expired upload sessions swept per cleanup pass.
	DefaultUploadCleanupBatchSize = 32
)

const (
	UploadStatusOpen      = "open"
	UploadStatusCompleted = "completed"
)

var (
	ErrInvalidUploadSession  = errors.New("invalid upload session parameters")
	ErrUploadSessionNotFound = errors.New("upload session not found")
	ErrUploadSessionExpired  = errors.New("upload session has expired")
	ErrUploadSessionClosed   = errors.New("upload session is already completed")
	ErrUploadQuotaExceeded   = errors.New("upload staging quota exceeded")
	ErrInvalidChunkIndex     = errors.New("invalid chunk index")
	ErrChunkSizeMismatch     = errors.New("chunk size does not match expected size")
	ErrChunkHashMismatch     = errors.New("chunk sha256 does not match expected hash")
	ErrChunkConflict         = errors.New("chunk already uploaded with different content")
	ErrIncompleteUpload      = errors.New("upload session is missing one or more chunks")
	ErrUploadHashMismatch    = errors.New("assembled file size or sha256 does not match upload session")
)

// CreateUploadRequest describes parameters to start a new chunked upload session.
type CreateUploadRequest struct {
	ParentID *string
	Name     string
	Size     int64
	SHA256   string
}

// UploadChunk describes a confirmed chunk stored for an upload session.
type UploadChunk struct {
	SessionID string
	Index     int
	Size      int64
	SHA256    string
	CreatedAt time.Time
}

// UploadSession describes a persisted resumable upload session and its progress.
type UploadSession struct {
	ID             string
	OwnerID        string
	ParentID       *string
	Name           string
	Size           int64
	SHA256         string
	ChunkSize      int64
	TotalChunks    int
	Status         string
	UploadedChunks []int
	UploadedBytes  int64
	FileID         *string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	CompletedAt    *time.Time
}

// ChunkStore manages staged upload chunk bytes outside the main object store.
type ChunkStore interface {
	SaveChunk(ctx context.Context, sessionID string, index int, r io.Reader, expectedSize int64, expectedSHA256 string) (UploadChunk, error)
	OpenChunk(ctx context.Context, sessionID string, index int, sha256 string) (io.ReadCloser, error)
	DeleteChunk(ctx context.Context, sessionID string, index int, sha256 string) error
	DeleteSessionChunks(ctx context.Context, sessionID string) error
}

// UploadSessionStore persists upload sessions and confirmed chunks in SQLite.
type UploadSessionStore interface {
	CreateUploadSession(ctx context.Context, session UploadSession, maxActiveSessions int, maxStagingBytes int64, now time.Time) error
	GetUploadSession(ctx context.Context, ownerID, sessionID string) (UploadSession, []UploadChunk, error)
	RecordUploadChunk(ctx context.Context, ownerID string, chunk UploadChunk, now time.Time) (alreadyConfirmed bool, err error)
	CompleteUploadSession(ctx context.Context, ownerID, sessionID string, file StoredFile, now time.Time) (published File, alreadyCompleted bool, err error)
	ListExpiredUploadSessions(ctx context.Context, now time.Time, limit int) ([]string, error)
	DeleteExpiredUploadSession(ctx context.Context, sessionID string) error
}

// WithUploadChunkSize overrides the chunk size assigned to new upload sessions.
func WithUploadChunkSize(chunkSize int64) Option {
	return func(s *Service) {
		if chunkSize > 0 {
			s.uploadChunkSize = chunkSize
		}
	}
}

// WithUploadSessionTTL overrides how long open upload sessions remain valid.
func WithUploadSessionTTL(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl > 0 {
			s.uploadSessionTTL = ttl
		}
	}
}

// WithUploadLimits overrides the per-owner active session count and staging byte caps.
func WithUploadLimits(maxActiveSessions int, maxStagingBytes int64) Option {
	return func(s *Service) {
		if maxActiveSessions > 0 {
			s.maxActiveSessions = maxActiveSessions
		}
		if maxStagingBytes > 0 {
			s.maxStagingBytes = maxStagingBytes
		}
	}
}

// UploadChunkSize returns the server-configured chunk size for new upload sessions.
func (s *Service) UploadChunkSize() int64 {
	return s.uploadChunkSize
}

// CreateUploadSession validates metadata and staging limits, then persists a new upload session.
func (s *Service) CreateUploadSession(ctx context.Context, ownerID string, req CreateUploadRequest) (UploadSession, error) {
	if err := s.CheckParent(ctx, ownerID, req.ParentID); err != nil {
		return UploadSession{}, err
	}
	if err := ValidateFilename(req.Name); err != nil {
		return UploadSession{}, err
	}
	if req.Size < 0 {
		return UploadSession{}, fmt.Errorf("%w: size must be non-negative", ErrInvalidUploadSession)
	}
	if req.Size > s.maxFileSize {
		return UploadSession{}, ErrFileTooLarge
	}
	shaHex := strings.ToLower(strings.TrimSpace(req.SHA256))
	if !isValidSHA256Hex(shaHex) {
		return UploadSession{}, fmt.Errorf("%w: sha256 must be a 64-character hex string", ErrInvalidUploadSession)
	}

	sessionID, err := s.idGen()
	if err != nil {
		return UploadSession{}, fmt.Errorf("generate upload session id: %w", err)
	}
	if err := ValidateID(sessionID); err != nil {
		return UploadSession{}, fmt.Errorf("generated invalid upload session id: %w", err)
	}

	now := s.now().UTC()
	chunkSize := s.uploadChunkSize
	totalChunks := computeTotalChunks(req.Size, chunkSize)

	session := UploadSession{
		ID:             sessionID,
		OwnerID:        ownerID,
		ParentID:       req.ParentID,
		Name:           req.Name,
		Size:           req.Size,
		SHA256:         shaHex,
		ChunkSize:      chunkSize,
		TotalChunks:    totalChunks,
		Status:         UploadStatusOpen,
		UploadedChunks: []int{},
		UploadedBytes:  0,
		CreatedAt:      now,
		ExpiresAt:      now.Add(s.uploadSessionTTL),
	}

	if err := s.meta.CreateUploadSession(ctx, session, s.maxActiveSessions, s.maxStagingBytes, now); err != nil {
		return UploadSession{}, err
	}
	return session, nil
}

// GetUploadSession returns owner-scoped upload session state and confirmed chunk indices.
func (s *Service) GetUploadSession(ctx context.Context, ownerID, sessionID string) (UploadSession, error) {
	if err := ValidateID(ownerID); err != nil {
		return UploadSession{}, fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if err := ValidateID(sessionID); err != nil {
		return UploadSession{}, fmt.Errorf("%w: %v", ErrInvalidUploadSession, err)
	}

	session, chunks, err := s.meta.GetUploadSession(ctx, ownerID, sessionID)
	if err != nil {
		return UploadSession{}, err
	}
	now := s.now().UTC()
	if session.Status == UploadStatusOpen && !now.Before(session.ExpiresAt) {
		return UploadSession{}, ErrUploadSessionExpired
	}

	indices := make([]int, 0, len(chunks))
	var uploadedBytes int64
	for _, c := range chunks {
		indices = append(indices, c.Index)
		uploadedBytes += c.Size
	}
	session.UploadedChunks = indices
	session.UploadedBytes = uploadedBytes
	return session, nil
}

// UploadChunk validates and stages a single chunk for an open upload session.
// Re-uploading the same chunk index with identical bytes is idempotent; different bytes return ErrChunkConflict.
func (s *Service) UploadChunk(ctx context.Context, ownerID, sessionID string, index int, expectedSHA256 string, r io.Reader) (UploadChunk, error) {
	if err := ValidateID(ownerID); err != nil {
		return UploadChunk{}, fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if err := ValidateID(sessionID); err != nil {
		return UploadChunk{}, fmt.Errorf("%w: %v", ErrInvalidUploadSession, err)
	}
	if r == nil {
		return UploadChunk{}, fmt.Errorf("%w: empty chunk body", ErrChunkSizeMismatch)
	}

	shaHeader := strings.ToLower(strings.TrimSpace(expectedSHA256))
	if shaHeader != "" && !isValidSHA256Hex(shaHeader) {
		return UploadChunk{}, fmt.Errorf("%w: invalid X-Chunk-SHA256 header", ErrChunkHashMismatch)
	}

	session, _, err := s.meta.GetUploadSession(ctx, ownerID, sessionID)
	if err != nil {
		return UploadChunk{}, err
	}
	now := s.now().UTC()
	if session.Status == UploadStatusCompleted {
		return UploadChunk{}, ErrUploadSessionClosed
	}
	if !now.Before(session.ExpiresAt) {
		return UploadChunk{}, ErrUploadSessionExpired
	}
	if index < 0 || index >= session.TotalChunks {
		return UploadChunk{}, fmt.Errorf("%w: chunk %d out of range [0, %d)", ErrInvalidChunkIndex, index, session.TotalChunks)
	}

	expectedSize := expectedChunkSize(session.Size, session.ChunkSize, session.TotalChunks, index)

	// Stage chunk bytes to disk outside any SQLite write transaction.
	staged, err := s.blobs.SaveChunk(ctx, sessionID, index, r, expectedSize, shaHeader)
	if err != nil {
		return UploadChunk{}, err
	}
	staged.CreatedAt = s.now().UTC()

	alreadyConfirmed, err := s.meta.RecordUploadChunk(ctx, ownerID, staged, staged.CreatedAt)
	if err != nil {
		// Remove the staged chunk file when recording fails (including ErrChunkConflict where
		// the conflicting chunk has a different sha256 filename than the existing confirmed chunk).
		_ = s.blobs.DeleteChunk(context.WithoutCancel(ctx), sessionID, index, staged.SHA256)
		return UploadChunk{}, err
	}
	_ = alreadyConfirmed
	return staged, nil
}

// CompleteUploadSession verifies all chunks, assembles and hashes the full file outside SQLite write
// transactions, and atomically publishes the file + sync event + completed session state.
// Concurrent or retried completions return the exact same File and publish only once.
func (s *Service) CompleteUploadSession(ctx context.Context, ownerID, sessionID string) (File, error) {
	if err := ValidateID(ownerID); err != nil {
		return File{}, fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if err := ValidateID(sessionID); err != nil {
		return File{}, fmt.Errorf("%w: %v", ErrInvalidUploadSession, err)
	}

	unlock := s.lockUploadSession(sessionID)
	defer unlock()

	session, chunks, err := s.meta.GetUploadSession(ctx, ownerID, sessionID)
	if err != nil {
		return File{}, err
	}
	if session.Status == UploadStatusCompleted && session.FileID != nil {
		return completedSessionFile(session), nil
	}

	now := s.now().UTC()
	if !now.Before(session.ExpiresAt) {
		return File{}, ErrUploadSessionExpired
	}

	// Verify destination folder still exists and belongs to owner before doing assembly I/O.
	if err := s.CheckParent(ctx, ownerID, session.ParentID); err != nil {
		return File{}, err
	}

	// Verify all chunks [0, total_chunks) are confirmed and sum to session.Size.
	if len(chunks) != session.TotalChunks {
		return File{}, fmt.Errorf("%w: have %d of %d chunks", ErrIncompleteUpload, len(chunks), session.TotalChunks)
	}
	var sumBytes int64
	for i, c := range chunks {
		if c.Index != i {
			return File{}, fmt.Errorf("%w: missing chunk %d", ErrIncompleteUpload, i)
		}
		expectedSize := expectedChunkSize(session.Size, session.ChunkSize, session.TotalChunks, i)
		if c.Size != expectedSize {
			return File{}, fmt.Errorf("%w: chunk %d size %d != %d", ErrUploadHashMismatch, i, c.Size, expectedSize)
		}
		sumBytes += c.Size
	}
	if sumBytes != session.Size {
		return File{}, fmt.Errorf("%w: total chunk bytes %d != expected %d", ErrUploadHashMismatch, sumBytes, session.Size)
	}

	// Stream-read chunks sequentially and persist into the main blob store outside any SQLite write tx.
	seqReader := &chunkSequenceReader{
		ctx:       ctx,
		blobs:     s.blobs,
		sessionID: sessionID,
		chunks:    chunks,
	}
	staged, err := s.blobs.Save(ctx, seqReader, s.maxFileSize)
	_ = seqReader.Close()
	if err != nil {
		return File{}, err
	}

	if staged.Size != session.Size || staged.SHA256 != session.SHA256 {
		_ = s.blobs.Delete(context.WithoutCancel(ctx), staged.ObjectKey)
		return File{}, fmt.Errorf("%w: assembled size=%d sha256=%s, expected size=%d sha256=%s",
			ErrUploadHashMismatch, staged.Size, staged.SHA256, session.Size, session.SHA256)
	}

	fileID, err := s.idGen()
	if err != nil {
		_ = s.blobs.Delete(context.WithoutCancel(ctx), staged.ObjectKey)
		return File{}, fmt.Errorf("generate file id: %w", err)
	}
	if err := ValidateID(fileID); err != nil {
		_ = s.blobs.Delete(context.WithoutCancel(ctx), staged.ObjectKey)
		return File{}, fmt.Errorf("generated invalid file id: %w", err)
	}

	commitNow := s.now().UTC()
	record := StoredFile{
		File: File{
			ID:              fileID,
			Name:            session.Name,
			ParentID:        session.ParentID,
			Size:            staged.Size,
			SHA256:          staged.SHA256,
			ContentRevision: 1,
			CreatedAt:       commitNow,
		},
		OwnerID:   ownerID,
		ObjectKey: staged.ObjectKey,
	}

	published, alreadyCompleted, err := s.meta.CompleteUploadSession(ctx, ownerID, sessionID, record, commitNow)
	if err != nil {
		_ = s.blobs.Delete(context.WithoutCancel(ctx), staged.ObjectKey)
		return File{}, err
	}
	if alreadyCompleted {
		_ = s.blobs.Delete(context.WithoutCancel(ctx), staged.ObjectKey)
		return published, nil
	}

	// Clean up staged chunks outside the SQLite transaction; session completion record stays in SQLite for retries.
	_ = s.blobs.DeleteSessionChunks(context.WithoutCancel(ctx), sessionID)
	return published, nil
}

// ProcessExpiredUploadSessions sweeps expired open upload sessions by removing their staged chunks
// on disk outside SQLite write transactions and then deleting their SQLite records.
func (s *Service) ProcessExpiredUploadSessions(ctx context.Context, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = DefaultUploadCleanupBatchSize
	}
	expiredIDs, err := s.meta.ListExpiredUploadSessions(ctx, s.now().UTC(), batchSize)
	if err != nil {
		return 0, err
	}

	var (
		cleaned int
		errs    []error
	)
	for _, sessionID := range expiredIDs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := s.blobs.DeleteSessionChunks(ctx, sessionID); err != nil {
			errs = append(errs, fmt.Errorf("delete expired upload chunks %q: %w", sessionID, err))
			continue
		}
		if err := s.meta.DeleteExpiredUploadSession(ctx, sessionID); err != nil {
			errs = append(errs, fmt.Errorf("delete expired upload session %q: %w", sessionID, err))
			continue
		}
		cleaned++
	}
	return cleaned, errors.Join(errs...)
}

func (s *Service) lockUploadSession(sessionID string) func() {
	s.uploadMuGuard.Lock()
	if s.uploadSessionMu == nil {
		s.uploadSessionMu = make(map[string]*sync.Mutex)
	}
	mu, ok := s.uploadSessionMu[sessionID]
	if !ok {
		mu = &sync.Mutex{}
		s.uploadSessionMu[sessionID] = mu
	}
	s.uploadMuGuard.Unlock()

	mu.Lock()
	return mu.Unlock
}

func completedSessionFile(session UploadSession) File {
	createdAt := session.CreatedAt
	if session.CompletedAt != nil {
		createdAt = *session.CompletedAt
	}
	id := ""
	if session.FileID != nil {
		id = *session.FileID
	}
	return File{
		ID:              id,
		Name:            session.Name,
		ParentID:        session.ParentID,
		Size:            session.Size,
		SHA256:          session.SHA256,
		ContentRevision: 1,
		CreatedAt:       createdAt.UTC(),
	}
}

func computeTotalChunks(totalSize, chunkSize int64) int {
	if totalSize == 0 {
		return 0
	}
	return int((totalSize + chunkSize - 1) / chunkSize)
}

func expectedChunkSize(totalSize, chunkSize int64, totalChunks, index int) int64 {
	if index < totalChunks-1 {
		return chunkSize
	}
	return totalSize - int64(index)*chunkSize
}

func isValidSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

type chunkSequenceReader struct {
	ctx       context.Context
	blobs     BlobStore
	sessionID string
	chunks    []UploadChunk
	pos       int
	cur       io.ReadCloser
}

func (r *chunkSequenceReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			if r.pos >= len(r.chunks) {
				return 0, io.EOF
			}
			c := r.chunks[r.pos]
			rc, err := r.blobs.OpenChunk(r.ctx, r.sessionID, c.Index, c.SHA256)
			if err != nil {
				return 0, fmt.Errorf("open chunk %d: %w", c.Index, err)
			}
			r.cur = rc
		}
		n, err := r.cur.Read(p)
		if errors.Is(err, io.EOF) {
			closeErr := r.cur.Close()
			r.cur = nil
			r.pos++
			if closeErr != nil {
				return n, closeErr
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (r *chunkSequenceReader) Close() error {
	if r.cur != nil {
		err := r.cur.Close()
		r.cur = nil
		return err
	}
	return nil
}
