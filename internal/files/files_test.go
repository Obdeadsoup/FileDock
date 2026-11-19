package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

type stubBlobStore struct {
	saveFn   func(ctx context.Context, r io.Reader, maxBytes int64) (StagedBlob, error)
	openFn   func(ctx context.Context, objectKey string) (io.ReadCloser, error)
	deleteFn func(ctx context.Context, objectKey string) error
	deleted  []string
}

func (s *stubBlobStore) Save(ctx context.Context, r io.Reader, maxBytes int64) (StagedBlob, error) {
	if s.saveFn != nil {
		return s.saveFn(ctx, r, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return StagedBlob{}, err
	}
	if int64(len(data)) > maxBytes {
		return StagedBlob{}, ErrFileTooLarge
	}
	return StagedBlob{
		ObjectKey: "obj-1",
		Size:      int64(len(data)),
		SHA256:    "dummysha256",
	}, nil
}

func (s *stubBlobStore) Open(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	if s.openFn != nil {
		return s.openFn(ctx, objectKey)
	}
	return io.NopCloser(strings.NewReader("hello")), nil
}

func (s *stubBlobStore) Delete(ctx context.Context, objectKey string) error {
	s.deleted = append(s.deleted, objectKey)
	if s.deleteFn != nil {
		return s.deleteFn(ctx, objectKey)
	}
	return nil
}

type stubMetadataStore struct {
	records  []StoredFile
	createFn func(ctx context.Context, file StoredFile) error
	listFn   func(ctx context.Context, page ListPage) ([]File, error)
	getFn    func(ctx context.Context, id string) (StoredFile, error)
}

func (m *stubMetadataStore) Create(ctx context.Context, file StoredFile) error {
	if m.createFn != nil {
		return m.createFn(ctx, file)
	}
	m.records = append(m.records, file)
	return nil
}

func (m *stubMetadataStore) List(ctx context.Context, page ListPage) ([]File, error) {
	if m.listFn != nil {
		return m.listFn(ctx, page)
	}
	out := make([]File, 0, len(m.records))
	for _, rec := range m.records {
		out = append(out, rec.File)
	}
	return out, nil
}

func (m *stubMetadataStore) GetByID(ctx context.Context, id string) (StoredFile, error) {
	if m.getFn != nil {
		return m.getFn(ctx, id)
	}
	for _, rec := range m.records {
		if rec.ID == id {
			return rec, nil
		}
	}
	return StoredFile{}, ErrNotFound
}

func TestValidateFilename(t *testing.T) {
	valid := []string{"hello.txt", "报告 (1).pdf", ".gitignore", "a b c.bin"}
	for _, name := range valid {
		if err := ValidateFilename(name); err != nil {
			t.Errorf("ValidateFilename(%q) unexpected error: %v", name, err)
		}
	}

	invalid := []string{"", "   ", ".", "..", "a/b.txt", `a\b.txt`, "bad\x00.txt", "bad\n.txt", strings.Repeat("a", 256)}
	for _, name := range invalid {
		if err := ValidateFilename(name); !errors.Is(err, ErrInvalidFilename) {
			t.Errorf("ValidateFilename(%q) = %v, want ErrInvalidFilename", name, err)
		}
	}
}

func TestUploadDeletesBlobWhenMetadataCreateFails(t *testing.T) {
	blobs := &stubBlobStore{
		saveFn: func(ctx context.Context, r io.Reader, maxBytes int64) (StagedBlob, error) {
			return StagedBlob{ObjectKey: "obj-rollback", Size: 5, SHA256: "abc"}, nil
		},
	}
	metaErr := errors.New("sqlite insert failed")
	meta := &stubMetadataStore{
		createFn: func(ctx context.Context, file StoredFile) error {
			return metaErr
		},
	}

	svc, err := NewService(blobs, meta, func() (string, error) { return "id-1", nil })
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Upload(context.Background(), "test.txt", strings.NewReader("hello"))
	if !errors.Is(err, metaErr) {
		t.Fatalf("Upload() error = %v, want %v", err, metaErr)
	}
	if !slices.Equal(blobs.deleted, []string{"obj-rollback"}) {
		t.Fatalf("deleted blobs = %v, want [obj-rollback]", blobs.deleted)
	}
}

func TestUploadAllowsDuplicateNamesWithDistinctIDs(t *testing.T) {
	blobs := &stubBlobStore{}
	meta := &stubMetadataStore{}
	seq := 0
	fixedNow := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)

	svc, err := NewService(
		blobs,
		meta,
		func() (string, error) {
			seq++
			return "id-" + string(rune('0'+seq)), nil
		},
		WithClock(func() time.Time { return fixedNow }),
	)
	if err != nil {
		t.Fatal(err)
	}

	first, err := svc.Upload(context.Background(), "same.txt", bytes.NewBufferString("one"))
	if err != nil {
		t.Fatalf("first Upload() error = %v", err)
	}
	second, err := svc.Upload(context.Background(), "same.txt", bytes.NewBufferString("two"))
	if err != nil {
		t.Fatalf("second Upload() error = %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("duplicate name uploads shared ID %q", first.ID)
	}
	if len(meta.records) != 2 {
		t.Fatalf("metadata count = %d, want 2", len(meta.records))
	}
}

func TestListPaginationBounds(t *testing.T) {
	blobs := &stubBlobStore{}
	var gotPage ListPage
	meta := &stubMetadataStore{
		listFn: func(ctx context.Context, page ListPage) ([]File, error) {
			gotPage = page
			return nil, nil
		},
	}
	svc, err := NewService(blobs, meta, func() (string, error) { return "id-1", nil })
	if err != nil {
		t.Fatal(err)
	}

	items, err := svc.List(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("List() = %#v, want non-nil empty slice", items)
	}
	if gotPage.Limit != DefaultListLimit || gotPage.Offset != 0 {
		t.Fatalf("page = %+v, want Limit=%d Offset=0", gotPage, DefaultListLimit)
	}

	if _, err := svc.List(context.Background(), MaxListLimit+500, 10); err != nil {
		t.Fatalf("List(capped) error = %v", err)
	}
	if gotPage.Limit != MaxListLimit || gotPage.Offset != 10 {
		t.Fatalf("capped page = %+v, want Limit=%d Offset=10", gotPage, MaxListLimit)
	}

	if _, err := svc.List(context.Background(), -1, 0); !errors.Is(err, ErrInvalidPagination) {
		t.Fatalf("List(negative limit) = %v, want ErrInvalidPagination", err)
	}
	if _, err := svc.List(context.Background(), 10, -1); !errors.Is(err, ErrInvalidPagination) {
		t.Fatalf("List(negative offset) = %v, want ErrInvalidPagination", err)
	}
}
