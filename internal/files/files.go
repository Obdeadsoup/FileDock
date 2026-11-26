package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// DefaultMaxFileSize limits a single uploaded file to 64 MiB.
	DefaultMaxFileSize int64 = 64 << 20
	// DefaultListLimit is used when no page size is specified.
	DefaultListLimit = 50
	// MaxListLimit caps the number of files returned per list request.
	MaxListLimit     = 100
	maxFilenameBytes = 255
)

var (
	ErrInvalidFilename   = errors.New("invalid filename")
	ErrInvalidID         = errors.New("invalid file id")
	ErrInvalidOwner      = errors.New("invalid owner id")
	ErrInvalidPagination = errors.New("invalid pagination parameters")
	ErrFileTooLarge      = errors.New("file exceeds maximum allowed size")
	ErrNotFound          = errors.New("file not found")
)

// File represents user-visible metadata for a stored file.
type File struct {
	ID              string
	Name            string
	ParentID        *string
	Size            int64
	SHA256          string
	ContentRevision int64
	CreatedAt       time.Time
}

// StoredFile pairs public file metadata with its owner ID and internal storage object key.
type StoredFile struct {
	File
	OwnerID   string
	ObjectKey string
}

// StagedBlob describes a file payload persisted in the blob store.
type StagedBlob struct {
	ObjectKey string
	Size      int64
	SHA256    string
}

// ListPage defines bounded pagination parameters for listing files.
type ListPage struct {
	Limit    int
	Offset   int
	ParentID *string
}

// BlobStore manages raw file content streams on storage.
type BlobStore interface {
	Save(ctx context.Context, r io.Reader, maxBytes int64) (StagedBlob, error)
	Open(ctx context.Context, objectKey string) (io.ReadCloser, error)
	Delete(ctx context.Context, objectKey string) error
}

// MetadataStore persists and queries file metadata records scoped by owner.
type MetadataStore interface {
	TreeStore
	CleanupAndSearchStore
	ShareStore
	SyncStore
	ContentUpdateStore
	Create(ctx context.Context, file StoredFile) error
	List(ctx context.Context, ownerID string, page ListPage) ([]File, error)
	GetByID(ctx context.Context, ownerID, id string) (StoredFile, error)
}

// IDGenerator produces unique, stable identifiers for uploaded files.
type IDGenerator func() (string, error)

// Service coordinates file validation, content storage, and owner-scoped metadata persistence.
type Service struct {
	blobs            BlobStore
	meta             MetadataStore
	idGen            IDGenerator
	maxFileSize      int64
	now              func() time.Time
	cleanupNotify    chan struct{}
	cleanupBaseDelay time.Duration
	cleanupMaxDelay  time.Duration
}

// Option configures optional settings on Service.
type Option func(*Service)

// WithMaxFileSize overrides the maximum accepted file byte size.
func WithMaxFileSize(maxBytes int64) Option {
	return func(s *Service) {
		if maxBytes > 0 {
			s.maxFileSize = maxBytes
		}
	}
}

// WithIDGenerator overrides the file ID generator.
func WithIDGenerator(gen IDGenerator) Option {
	return func(s *Service) {
		if gen != nil {
			s.idGen = gen
		}
	}
}

// WithClock overrides the clock used for CreatedAt timestamps.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// NewService constructs a file Service with required storage adapters and ID generator.
func NewService(blobs BlobStore, meta MetadataStore, idGen IDGenerator, opts ...Option) (*Service, error) {
	if blobs == nil {
		return nil, errors.New("blob store is required")
	}
	if meta == nil {
		return nil, errors.New("metadata store is required")
	}
	if idGen == nil {
		return nil, errors.New("id generator is required")
	}
	s := &Service{
		blobs:            blobs,
		meta:             meta,
		idGen:            idGen,
		maxFileSize:      DefaultMaxFileSize,
		now:              func() time.Time { return time.Now().UTC() },
		cleanupNotify:    make(chan struct{}, 1),
		cleanupBaseDelay: defaultCleanupBaseDelay,
		cleanupMaxDelay:  defaultCleanupMaxDelay,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// MaxFileSize returns the maximum file size in bytes accepted by Upload.
func (s *Service) MaxFileSize() int64 {
	return s.maxFileSize
}

// Upload validates ownerID and filename, streams content to BlobStore, and registers metadata.
// If metadata persistence fails, the newly stored blob is deleted before returning.
func (s *Service) Upload(ctx context.Context, ownerID, filename string, r io.Reader) (File, error) {
	return s.UploadTo(ctx, ownerID, nil, filename, r)
}

// UploadTo streams a file into the selected directory.
func (s *Service) UploadTo(ctx context.Context, ownerID string, parentID *string, filename string, r io.Reader) (File, error) {
	if err := s.CheckParent(ctx, ownerID, parentID); err != nil {
		return File{}, err
	}
	if r == nil {
		return File{}, fmt.Errorf("%w: empty reader", ErrInvalidFilename)
	}
	if err := ValidateFilename(filename); err != nil {
		return File{}, err
	}

	id, err := s.idGen()
	if err != nil {
		return File{}, fmt.Errorf("generate file id: %w", err)
	}
	if err := ValidateID(id); err != nil {
		return File{}, fmt.Errorf("generated invalid file id: %w", err)
	}

	staged, err := s.blobs.Save(ctx, r, s.maxFileSize)
	if err != nil {
		return File{}, err
	}

	record := StoredFile{
		File: File{
			ID:              id,
			Name:            filename,
			ParentID:        parentID,
			Size:            staged.Size,
			SHA256:          staged.SHA256,
			ContentRevision: 1,
			CreatedAt:       s.now().UTC(),
		},
		OwnerID:   ownerID,
		ObjectKey: staged.ObjectKey,
	}

	if err := s.meta.Create(ctx, record); err != nil {
		// Clean up the newly written object so failed uploads leave no unreferenced blob.
		if delErr := s.blobs.Delete(context.WithoutCancel(ctx), staged.ObjectKey); delErr != nil {
			return File{}, errors.Join(err, fmt.Errorf("cleanup blob %q: %w", staged.ObjectKey, delErr))
		}
		return File{}, err
	}

	return record.File, nil
}

// List returns a bounded page of registered files owned by ownerID in stable order.
func (s *Service) List(ctx context.Context, ownerID string, limit, offset int) ([]File, error) {
	return s.ListIn(ctx, ownerID, nil, limit, offset)
}

// ListIn returns a page of the direct files of a directory.
func (s *Service) ListIn(ctx context.Context, ownerID string, parentID *string, limit, offset int) ([]File, error) {
	if err := validateLocation(ownerID, parentID); err != nil {
		return nil, err
	}
	page, err := newPage(parentID, limit, offset)
	if err != nil {
		return nil, err
	}
	items, err := s.meta.List(ctx, ownerID, page)
	if err != nil {
		return nil, err
	}
	if items == nil {
		return []File{}, nil
	}
	return items, nil
}

// UpdateFile atomically renames or moves a file without touching its content.
func (s *Service) UpdateFile(ctx context.Context, ownerID, id string, changes Changes) (File, error) {
	if err := changes.validate(ownerID, id); err != nil {
		return File{}, err
	}
	return s.meta.UpdateFile(ctx, ownerID, id, changes, s.now().UTC())
}

// OpenContent validates ownerID and id, checks ownership via metadata lookup first, and only then opens the disk stream.
func (s *Service) OpenContent(ctx context.Context, ownerID, id string) (File, io.ReadCloser, error) {
	if err := ValidateID(ownerID); err != nil {
		return File{}, nil, fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if err := ValidateID(id); err != nil {
		return File{}, nil, err
	}

	record, err := s.meta.GetByID(ctx, ownerID, id)
	if err != nil {
		return File{}, nil, err
	}

	rc, err := s.blobs.Open(ctx, record.ObjectKey)
	if err != nil {
		return File{}, nil, err
	}
	return record.File, rc, nil
}

// ValidateFilename ensures a client-supplied filename is a safe single path segment.
func ValidateFilename(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: filename must not be empty", ErrInvalidFilename)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%w: filename %q is not allowed", ErrInvalidFilename, name)
	}
	if len(name) > maxFilenameBytes {
		return fmt.Errorf("%w: filename exceeds %d bytes", ErrInvalidFilename, maxFilenameBytes)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: filename must not contain path separators", ErrInvalidFilename)
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b < 0x20 || b == 0x7f {
			return fmt.Errorf("%w: filename must not contain control characters", ErrInvalidFilename)
		}
	}
	return nil
}

// ValidateID checks that an identifier is non-empty and free of path or control characters.
func ValidateID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: empty id", ErrInvalidID)
	}
	if id == "." || id == ".." || strings.Contains(id, "..") {
		return fmt.Errorf("%w: invalid id", ErrInvalidID)
	}
	if strings.ContainsAny(id, "/\\ \t\r\n") {
		return fmt.Errorf("%w: invalid characters in id", ErrInvalidID)
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if b < 0x20 || b == 0x7f {
			return fmt.Errorf("%w: control character in id", ErrInvalidID)
		}
	}
	return nil
}
