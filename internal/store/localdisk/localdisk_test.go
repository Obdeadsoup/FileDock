package localdisk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/filedock/internal/files"
)

type failingReader struct {
	prefix []byte
	err    error
	sent   bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		n := copy(p, f.prefix)
		return n, nil
	}
	return 0, f.err
}

func TestSaveOpenDeleteAndHash(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	payload := []byte("filedock round 2 content")
	sum := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(sum[:])

	staged, err := store.Save(context.Background(), bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if staged.Size != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", staged.Size, len(payload))
	}
	if staged.SHA256 != wantHash {
		t.Errorf("SHA256 = %q, want %q", staged.SHA256, wantHash)
	}

	rc, err := store.Open(context.Background(), staged.ObjectKey)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content = %q, want %q", got, payload)
	}

	assertDirEmpty(t, filepath.Join(dir, "tmp"))

	if err := store.Delete(context.Background(), staged.ObjectKey); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Open(context.Background(), staged.ObjectKey); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("Open() after Delete = %v, want ErrNotFound", err)
	}
}

func TestSaveCleansTempFileOnLimitExceededAndReadError(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	_, err = store.Save(context.Background(), strings.NewReader("123456"), 5)
	if !errors.Is(err, files.ErrFileTooLarge) {
		t.Fatalf("Save(oversize) = %v, want ErrFileTooLarge", err)
	}
	assertDirEmpty(t, filepath.Join(dir, "tmp"))
	assertDirEmpty(t, filepath.Join(dir, "objects"))

	readErr := errors.New("connection reset")
	_, err = store.Save(context.Background(), &failingReader{prefix: []byte("abc"), err: readErr}, 100)
	if !errors.Is(err, readErr) {
		t.Fatalf("Save(readErr) = %v, want %v", err, readErr)
	}
	assertDirEmpty(t, filepath.Join(dir, "tmp"))
	assertDirEmpty(t, filepath.Join(dir, "objects"))
}

func assertDirEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", path, err)
	}
	if len(entries) != 0 {
		t.Fatalf("dir %q not empty: %d entries", path, len(entries))
	}
}
