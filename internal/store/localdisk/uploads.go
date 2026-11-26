package localdisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"example.com/filedock/internal/files"
)

// SaveChunk streams a single upload chunk into the session's staging directory, enforcing
// exact expectedSize and optional expectedSHA256 before committing it under <index>-<sha256>.chunk.
func (s *Store) SaveChunk(ctx context.Context, sessionID string, index int, r io.Reader, expectedSize int64, expectedSHA256 string) (files.UploadChunk, error) {
	if err := ctx.Err(); err != nil {
		return files.UploadChunk{}, err
	}
	if err := files.ValidateID(sessionID); err != nil {
		return files.UploadChunk{}, fmt.Errorf("%w: invalid session id", files.ErrInvalidUploadSession)
	}
	if index < 0 {
		return files.UploadChunk{}, files.ErrInvalidChunkIndex
	}
	if expectedSize <= 0 {
		return files.UploadChunk{}, files.ErrChunkSizeMismatch
	}
	if r == nil {
		return files.UploadChunk{}, files.ErrChunkSizeMismatch
	}

	sessionDir := filepath.Join(s.uploadsDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return files.UploadChunk{}, fmt.Errorf("create upload session dir: %w", err)
	}

	tmpFile, err := os.CreateTemp(sessionDir, "chunk-tmp-*")
	if err != nil {
		return files.UploadChunk{}, fmt.Errorf("create chunk temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	closed := false
	committed := false
	defer func() {
		if !closed {
			_ = tmpFile.Close()
		}
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	limited := io.LimitReader(ctxReader{ctx: ctx, r: r}, expectedSize+1)
	buf := make([]byte, 32*1024)
	written, err := io.CopyBuffer(io.MultiWriter(tmpFile, hasher), limited, buf)
	if err != nil {
		return files.UploadChunk{}, err
	}
	if written != expectedSize {
		return files.UploadChunk{}, fmt.Errorf("%w: got %d bytes, expected %d", files.ErrChunkSizeMismatch, written, expectedSize)
	}

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if expectedSHA256 != "" && actualSHA != expectedSHA256 {
		return files.UploadChunk{}, fmt.Errorf("%w: got %s, expected %s", files.ErrChunkHashMismatch, actualSHA, expectedSHA256)
	}

	if err := tmpFile.Sync(); err != nil {
		return files.UploadChunk{}, fmt.Errorf("sync chunk temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		closed = true
		return files.UploadChunk{}, fmt.Errorf("close chunk temp file: %w", err)
	}
	closed = true

	finalPath := s.chunkPath(sessionID, index, actualSHA)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return files.UploadChunk{}, fmt.Errorf("commit chunk file: %w", err)
	}
	committed = true

	return files.UploadChunk{
		SessionID: sessionID,
		Index:     index,
		Size:      written,
		SHA256:    actualSHA,
	}, nil
}

// OpenChunk opens a previously staged chunk file by session ID, chunk index, and SHA-256.
func (s *Store) OpenChunk(ctx context.Context, sessionID string, index int, sha256Hex string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := files.ValidateID(sessionID); err != nil || index < 0 || !isValidSHA256(sha256Hex) {
		return nil, files.ErrUploadSessionNotFound
	}
	f, err := os.Open(s.chunkPath(sessionID, index, sha256Hex))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: chunk %d missing on disk", files.ErrIncompleteUpload, index)
		}
		return nil, fmt.Errorf("open chunk %d: %w", index, err)
	}
	return f, nil
}

// DeleteChunk removes a single staged chunk file if present.
func (s *Store) DeleteChunk(_ context.Context, sessionID string, index int, sha256Hex string) error {
	if err := files.ValidateID(sessionID); err != nil || index < 0 || !isValidSHA256(sha256Hex) {
		return nil
	}
	err := os.Remove(s.chunkPath(sessionID, index, sha256Hex))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete chunk %d: %w", index, err)
	}
	return nil
}

// DeleteSessionChunks removes the entire staging directory for sessionID.
func (s *Store) DeleteSessionChunks(_ context.Context, sessionID string) error {
	if err := files.ValidateID(sessionID); err != nil {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(s.uploadsDir, sessionID)); err != nil {
		return fmt.Errorf("delete upload session dir: %w", err)
	}
	return nil
}

func (s *Store) chunkPath(sessionID string, index int, sha256Hex string) string {
	return filepath.Join(s.uploadsDir, sessionID, fmt.Sprintf("%d-%s.chunk", index, sha256Hex))
}

func isValidSHA256(s string) bool {
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
