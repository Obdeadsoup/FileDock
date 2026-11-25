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
	opened   []string
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
	s.opened = append(s.opened, objectKey)
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
	TreeStore
	CleanupAndSearchStore
	ShareStore
	SyncStore
	records  []StoredFile
	createFn func(ctx context.Context, file StoredFile) error
	listFn   func(ctx context.Context, ownerID string, page ListPage) ([]File, error)
	getFn    func(ctx context.Context, ownerID, id string) (StoredFile, error)
}

func (m *stubMetadataStore) Create(ctx context.Context, file StoredFile) error {
	if m.createFn != nil {
		return m.createFn(ctx, file)
	}
	m.records = append(m.records, file)
	return nil
}

func (m *stubMetadataStore) List(ctx context.Context, ownerID string, page ListPage) ([]File, error) {
	if m.listFn != nil {
		return m.listFn(ctx, ownerID, page)
	}
	out := make([]File, 0)
	for _, rec := range m.records {
		if rec.OwnerID == ownerID {
			out = append(out, rec.File)
		}
	}
	return out, nil
}

func (m *stubMetadataStore) GetByID(ctx context.Context, ownerID, id string) (StoredFile, error) {
	if m.getFn != nil {
		return m.getFn(ctx, ownerID, id)
	}
	for _, rec := range m.records {
		if rec.ID == id && rec.OwnerID == ownerID {
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

	_, err = svc.Upload(context.Background(), "user-1", "test.txt", strings.NewReader("hello"))
	if !errors.Is(err, metaErr) {
		t.Fatalf("Upload() error = %v, want %v", err, metaErr)
	}
	if !slices.Equal(blobs.deleted, []string{"obj-rollback"}) {
		t.Fatalf("deleted blobs = %v, want [obj-rollback]", blobs.deleted)
	}
}

func TestUploadAllowsDuplicateNamesAndEnforcesOwnerIsolation(t *testing.T) {
	blobs := &stubBlobStore{}
	meta := &stubMetadataStore{}
	seq := 0
	fixedNow := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

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

	first, err := svc.Upload(context.Background(), "user-a", "same.txt", bytes.NewBufferString("one"))
	if err != nil {
		t.Fatalf("first Upload() error = %v", err)
	}
	second, err := svc.Upload(context.Background(), "user-a", "same.txt", bytes.NewBufferString("two"))
	if err != nil {
		t.Fatalf("second Upload() error = %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("duplicate name uploads shared ID %q", first.ID)
	}
	if len(meta.records) != 2 {
		t.Fatalf("metadata count = %d, want 2", len(meta.records))
	}

	// User B cannot see User A's files in list or open User A's file content.
	bList, err := svc.List(context.Background(), "user-b", 0, 0)
	if err != nil {
		t.Fatalf("List(user-b) error = %v", err)
	}
	if len(bList) != 0 {
		t.Fatalf("List(user-b) len = %d, want 0", len(bList))
	}

	if _, _, err := svc.OpenContent(context.Background(), "user-b", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenContent(user-b, %q) = %v, want ErrNotFound", first.ID, err)
	}
	if len(blobs.opened) != 0 {
		t.Fatalf("blobs.Open must not be called when permission/metadata check fails; opened = %v", blobs.opened)
	}
}

func TestListPaginationBounds(t *testing.T) {
	blobs := &stubBlobStore{}
	var gotPage ListPage
	var gotOwner string
	meta := &stubMetadataStore{
		listFn: func(ctx context.Context, ownerID string, page ListPage) ([]File, error) {
			gotOwner = ownerID
			gotPage = page
			return nil, nil
		},
	}
	svc, err := NewService(blobs, meta, func() (string, error) { return "id-1", nil })
	if err != nil {
		t.Fatal(err)
	}

	items, err := svc.List(context.Background(), "user-1", 0, 0)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("List() = %#v, want non-nil empty slice", items)
	}
	if gotOwner != "user-1" || gotPage.Limit != DefaultListLimit || gotPage.Offset != 0 {
		t.Fatalf("owner=%q page=%+v, want owner=user-1 Limit=%d Offset=0", gotOwner, gotPage, DefaultListLimit)
	}

	if _, err := svc.List(context.Background(), "user-1", MaxListLimit+500, 10); err != nil {
		t.Fatalf("List(capped) error = %v", err)
	}
	if gotPage.Limit != MaxListLimit || gotPage.Offset != 10 {
		t.Fatalf("capped page = %+v, want Limit=%d Offset=10", gotPage, MaxListLimit)
	}

	if _, err := svc.List(context.Background(), "user-1", -1, 0); !errors.Is(err, ErrInvalidPagination) {
		t.Fatalf("List(negative limit) = %v, want ErrInvalidPagination", err)
	}
	if _, err := svc.List(context.Background(), "user-1", 10, -1); !errors.Is(err, ErrInvalidPagination) {
		t.Fatalf("List(negative offset) = %v, want ErrInvalidPagination", err)
	}
}
