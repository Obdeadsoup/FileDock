package sqlite

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/store/localdisk"
)

func TestUploadCleansUpIfDestinationIsDeletedWhileStreaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	store, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Use a second connection pool so the test also exercises SQLite locking.
	other, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	const owner = "owner"
	if err := store.CreateUser(ctx, accounts.StoredUser{
		ID: owner, Username: "alice", PasswordHash: []byte("hash"), PasswordSalt: []byte("salt"),
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	folder := files.Folder{ID: "destination", Name: "destination", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateFolder(ctx, owner, folder); err != nil {
		t.Fatal(err)
	}
	blobs, err := localdisk.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := files.NewService(blobs, store, func() (string, error) { return "file-id", nil })
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	stop := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()) })
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := service.UploadTo(ctx, owner, &folder.ID, "demo.txt", reader)
		_ = reader.CloseWithError(err)
		done <- err
	}()
	if _, err := io.WriteString(writer, "upload content"); err != nil {
		t.Fatal(err)
	}
	if err := other.DeleteFolder(ctx, owner, folder.ID); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, files.ErrFolderNotFound) {
			t.Fatalf("upload error = %v, want folder not found", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := store.GetByID(ctx, owner, "file-id"); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("file lookup = %v, want file not found", err)
	}
	for _, subdir := range []string{"tmp", "objects"} {
		entries, err := os.ReadDir(filepath.Join(dir, subdir))
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s contains %v, read error = %v", subdir, entries, err)
		}
	}
}
