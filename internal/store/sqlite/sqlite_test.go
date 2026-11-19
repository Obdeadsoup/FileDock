package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"example.com/filedock/internal/files"
)

func TestStorePersistenceOrderingPaginationAndReinit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	empty, err := store.List(ctx, files.ListPage{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("List(empty) error = %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("len(empty) = %d, want 0", len(empty))
	}

	t1 := time.Date(2026, 11, 19, 7, 0, 0, 100, time.UTC)
	t2 := time.Date(2026, 11, 19, 7, 0, 1, 200, time.UTC)
	records := []files.StoredFile{
		{
			File:      files.File{ID: "id-b", Name: "same.txt", Size: 10, SHA256: "hash-b", CreatedAt: t1},
			ObjectKey: "obj-b",
		},
		{
			File:      files.File{ID: "id-a", Name: "same.txt", Size: 20, SHA256: "hash-a", CreatedAt: t1},
			ObjectKey: "obj-a",
		},
		{
			File:      files.File{ID: "id-c", Name: "newer.txt", Size: 30, SHA256: "hash-c", CreatedAt: t2},
			ObjectKey: "obj-c",
		},
	}
	for _, rec := range records {
		if err := store.Create(ctx, rec); err != nil {
			t.Fatalf("Create(%s) error = %v", rec.ID, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Re-open on the same directory to verify idempotent schema init and persistence.
	reopened, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("reopened NewStore() error = %v", err)
	}
	defer reopened.Close()

	page1, err := reopened.List(ctx, files.ListPage{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("List(page1) error = %v", err)
	}
	if len(page1) != 2 || page1[0].ID != "id-c" || page1[1].ID != "id-a" {
		t.Fatalf("page1 = %+v, want [id-c, id-a]", page1)
	}

	page2, err := reopened.List(ctx, files.ListPage{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("List(page2) error = %v", err)
	}
	if len(page2) != 1 || page2[0].ID != "id-b" {
		t.Fatalf("page2 = %+v, want [id-b]", page2)
	}

	got, err := reopened.GetByID(ctx, "id-a")
	if err != nil {
		t.Fatalf("GetByID(id-a) error = %v", err)
	}
	if got.ObjectKey != "obj-a" || !got.CreatedAt.Equal(t1) || got.SHA256 != "hash-a" {
		t.Fatalf("GetByID(id-a) = %+v", got)
	}

	if _, err := reopened.GetByID(ctx, "missing"); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
}
