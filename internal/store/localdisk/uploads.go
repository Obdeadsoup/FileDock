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
	"strings"

	"example.com/filedock/internal/files"
)

// SaveChunk streams a single upload chunk into a unique per-request file inside the session's
// staging directory, enforcing exact expectedSize and optional expectedSHA256.
// Because each request gets its own unique StorageKey, cleaning up a failed or duplicate request
// can never delete a previously confirmed chunk.
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

	nonce, err := generateObjectKey()
	if err != nil {
		return files.UploadChunk{}, fmt.Errorf("generate chunk storage key: %w", err)
	}
	storageKey := fmt.Sprintf("%d-%s.chunk", index, nonce)
	finalPath := filepath.Join(sessionDir, storageKey)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return files.UploadChunk{}, fmt.Errorf("commit chunk file: %w", err)
	}
	committed = true

	return files.UploadChunk{
		SessionID:  sessionID,
		Index:      index,
		Size:       written,
		SHA256:     actualSHA,
		StorageKey: storageKey,
	}, nil
}

// OpenChunk opens a previously confirmed chunk file using its recorded StorageKey
// (falling back to legacy <index>-<sha256>.chunk if StorageKey is empty).
func (s *Store) OpenChunk(ctx context.Context, sessionID string, chunk files.UploadChunk) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := files.ValidateID(sessionID); err != nil || chunk.Index < 0 {
		return nil, files.ErrUploadSessionNotFound
	}
	key := chunk.StorageKey
	if key == "" {
		if !isValidSHA256(chunk.SHA256) {
			return nil, files.ErrUploadSessionNotFound
		}
		key = fmt.Sprintf("%d-%s.chunk", chunk.Index, chunk.SHA256)
	}
	if !isSafeChunkStorageKey(key) {
		return nil, files.ErrUploadSessionNotFound
	}
	f, err := os.Open(filepath.Join(s.uploadsDir, sessionID, key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: chunk %d missing on disk", files.ErrIncompleteUpload, chunk.Index)
		}
		return nil, fmt.Errorf("open chunk %d: %w", chunk.Index, err)
	}
	return f, nil
}

// DeleteStagedChunk removes a single unreferenced per-request staged chunk file if present.
func (s *Store) DeleteStagedChunk(_ context.Context, sessionID, storageKey string) error {
	if err := files.ValidateID(sessionID); err != nil || !isSafeChunkStorageKey(storageKey) {
		return nil
	}
	err := os.Remove(filepath.Join(s.uploadsDir, sessionID, storageKey))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete staged chunk %q: %w", storageKey, err)
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

func isSafeChunkStorageKey(key string) bool {
	if key == "" || key == "." || key == ".." || strings.ContainsAny(key, `/\`) || strings.Contains(key, "..") {
		return false
	}
	return strings.HasSuffix(key, ".chunk")
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
