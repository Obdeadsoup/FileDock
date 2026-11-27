package localdisk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tink-crypto/tink-go/v2/tink"

	"example.com/filedock/internal/files"
)

// Store persists file payloads on the local filesystem using server-generated keys.
// When initialized with an encryption key file, all formal objects, temp upload files,
// and staged upload chunks are encrypted on disk with Tink Streaming AEAD.
type Store struct {
	objectsDir string
	tmpDir     string
	uploadsDir string
	mode       string
	aead       tink.StreamingAEAD
}

// NewStore initializes a localdisk Store in plaintext mode (compatible with existing callers and tests).
func NewStore(dataDir string) (*Store, error) {
	return NewStoreWithConfig(dataDir, "")
}

// NewStoreWithConfig initializes object, staging, and chunk upload directories under dataDir,
// enforcing persisted storage mode compatibility and optional Tink Streaming AEAD encryption.
func NewStoreWithConfig(dataDir, encryptionKeyFile string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("data dir must not be empty")
	}
	objectsDir := filepath.Join(dataDir, "objects")
	tmpDir := filepath.Join(dataDir, "tmp")
	uploadsDir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(objectsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create objects dir: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("create tmp dir: %w", err)
	}
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create uploads dir: %w", err)
	}

	mode, aead, err := initStorageMode(dataDir, encryptionKeyFile)
	if err != nil {
		return nil, err
	}

	return &Store{
		objectsDir: objectsDir,
		tmpDir:     tmpDir,
		uploadsDir: uploadsDir,
		mode:       mode,
		aead:       aead,
	}, nil
}

// Mode returns the active storage mode ("plaintext" or "encrypted").
func (s *Store) Mode() string {
	return s.mode
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Save streams r to a temporary file while hashing plaintext and enforcing maxBytes, then commits it to objects/.
// In encrypted mode, the temporary file is stream-encrypted directly with AAD bound to the pre-generated objectKey.
func (s *Store) Save(ctx context.Context, r io.Reader, maxBytes int64) (files.StagedBlob, error) {
	if err := ctx.Err(); err != nil {
		return files.StagedBlob{}, err
	}
	if maxBytes <= 0 {
		return files.StagedBlob{}, errors.New("maxBytes must be positive")
	}
	if r == nil {
		return files.StagedBlob{}, errors.New("reader must not be nil")
	}

	objectKey, err := generateObjectKey()
	if err != nil {
		return files.StagedBlob{}, fmt.Errorf("generate object key: %w", err)
	}

	tmpFile, err := os.CreateTemp(s.tmpDir, "upload-*")
	if err != nil {
		return files.StagedBlob{}, fmt.Errorf("create temp file: %w", err)
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

	var (
		dstWriter io.Writer = tmpFile
		encWriter io.WriteCloser
	)
	if s.aead != nil {
		encWriter, err = s.aead.NewEncryptingWriter(tmpFile, objectAAD(objectKey))
		if err != nil {
			return files.StagedBlob{}, fmt.Errorf("init object encrypting writer: %w", err)
		}
		dstWriter = encWriter
	}

	hasher := sha256.New()
	limited := io.LimitReader(ctxReader{ctx: ctx, r: r}, maxBytes+1)
	buf := make([]byte, 32*1024)
	written, err := io.CopyBuffer(io.MultiWriter(dstWriter, hasher), limited, buf)
	if err != nil {
		if encWriter != nil {
			_ = encWriter.Close()
		}
		return files.StagedBlob{}, err
	}
	if written > maxBytes {
		if encWriter != nil {
			_ = encWriter.Close()
		}
		return files.StagedBlob{}, files.ErrFileTooLarge
	}
	if encWriter != nil {
		if err := encWriter.Close(); err != nil {
			return files.StagedBlob{}, fmt.Errorf("finalize object encryption: %w", err)
		}
	}
	if err := tmpFile.Sync(); err != nil {
		return files.StagedBlob{}, fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		closed = true
		return files.StagedBlob{}, fmt.Errorf("close temp file: %w", err)
	}
	closed = true

	finalPath := filepath.Join(s.objectsDir, objectKey)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return files.StagedBlob{}, fmt.Errorf("commit object: %w", err)
	}
	committed = true

	return files.StagedBlob{
		ObjectKey: objectKey,
		Size:      written,
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// Open returns a ReadCloser for the object identified by objectKey.
// In encrypted mode, the stream is authenticated and decrypted with AAD bound to objectKey.
func (s *Store) Open(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !isValidObjectKey(objectKey) {
		return nil, fmt.Errorf("%w: invalid object key", files.ErrNotFound)
	}
	f, err := os.Open(filepath.Join(s.objectsDir, objectKey))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, files.ErrNotFound
		}
		return nil, fmt.Errorf("open object: %w", err)
	}
	if s.aead != nil {
		decReader, err := s.aead.NewDecryptingReader(f, objectAAD(objectKey))
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("init object decrypting reader: %w", err)
		}
		return &decryptReadCloser{r: decReader, f: f}, nil
	}
	return f, nil
}

// Delete removes the stored object identified by objectKey if it exists.
func (s *Store) Delete(_ context.Context, objectKey string) error {
	if !isValidObjectKey(objectKey) {
		return errors.New("invalid object key")
	}
	err := os.Remove(filepath.Join(s.objectsDir, objectKey))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

func generateObjectKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func isValidObjectKey(key string) bool {
	if len(key) != 32 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
