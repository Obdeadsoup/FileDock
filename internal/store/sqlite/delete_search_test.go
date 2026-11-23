package sqlite

import (
	"bytes"
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

type selectiveFailBlobStore struct {
	*localdisk.Store
	failKeys map[string]error
}

func (s *selectiveFailBlobStore) Delete(ctx context.Context, objectKey string) error {
	if err, ok := s.failKeys[objectKey]; ok {
		return err
	}
	return s.Store.Delete(ctx, objectKey)
}

func TestDeleteFileTransactionFailurePreservesMetadataAndBlob(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	blobs, err := localdisk.NewStore(dir)
	if err != nil {
		t.Fatalf("localdisk.NewStore() error = %v", err)
	}
	store, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	acctSvc, err := accounts.NewService(store, accounts.WithArgon2Params(accounts.Argon2Params{
		Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 32,
	}))
	if err != nil {
		t.Fatal(err)
	}
	alice, err := acctSvc.Register(ctx, "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}

	svc, err := files.NewService(blobs, store, func() (string, error) { return "file-tx-fail", nil })
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("must survive rollback")
	uploaded, err := svc.Upload(ctx, alice.ID, "keep.txt", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	// Inject a trigger that aborts inserting into blob_cleanups inside DeleteFile's transaction.
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER fail_cleanup_insert BEFORE INSERT ON blob_cleanups
BEGIN
	SELECT RAISE(ABORT, 'simulated cleanup enqueue failure');
END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if err := svc.DeleteFile(ctx, alice.ID, uploaded.ID); err == nil {
		t.Fatal("DeleteFile() expected error from aborted transaction, got nil")
	}

	// Original metadata and disk object must remain intact and downloadable.
	meta, rc, err := svc.OpenContent(ctx, alice.ID, uploaded.ID)
	if err != nil {
		t.Fatalf("OpenContent() after failed delete error = %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if meta.ID != uploaded.ID || !bytes.Equal(got, payload) {
		t.Fatalf("downloaded after failed delete = (%q, %q), want (%q, %q)", meta.ID, got, uploaded.ID, payload)
	}

	due, err := store.ListDueCleanups(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDueCleanups() error = %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected 0 cleanup tasks after rollback, got %d", len(due))
	}
}

func TestBlobCleanupRetryAfterRestartNonBlockingAndMissingObjectAck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	disk, err := localdisk.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	acctSvc, err := accounts.NewService(store, accounts.WithArgon2Params(accounts.Argon2Params{
		Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 32,
	}))
	if err != nil {
		t.Fatal(err)
	}
	alice, err := acctSvc.Register(ctx, "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}

	seq := 0
	idGen := func() (string, error) {
		seq++
		return "file-" + string(rune('a'+seq-1)), nil
	}
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

	flaky := &selectiveFailBlobStore{
		Store:    disk,
		failKeys: map[string]error{},
	}
	svc, err := files.NewService(
		flaky,
		store,
		idGen,
		files.WithClock(func() time.Time { return now }),
		files.WithCleanupBackoff(10*time.Second, 60*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	f1, err := svc.Upload(ctx, alice.ID, "transient-fail.txt", bytes.NewReader([]byte("one")))
	if err != nil {
		t.Fatal(err)
	}
	f2, err := svc.Upload(ctx, alice.ID, "normal.txt", bytes.NewReader([]byte("two")))
	if err != nil {
		t.Fatal(err)
	}
	f3, err := svc.Upload(ctx, alice.ID, "already-gone.txt", bytes.NewReader([]byte("three")))
	if err != nil {
		t.Fatal(err)
	}

	rec1, err := store.GetByID(ctx, alice.ID, f1.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec2, err := store.GetByID(ctx, alice.ID, f2.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec3, err := store.GetByID(ctx, alice.ID, f3.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate transient disk failure for f1, and pre-delete f3's disk object to test missing-object ack.
	flaky.failKeys[rec1.ObjectKey] = errors.New("temporary EIO")
	if err := os.Remove(filepath.Join(dir, "objects", rec3.ObjectKey)); err != nil {
		t.Fatalf("pre-remove object 3: %v", err)
	}

	for _, id := range []string{f1.ID, f2.ID, f3.ID} {
		if err := svc.DeleteFile(ctx, alice.ID, id); err != nil {
			t.Fatalf("DeleteFile(%s) error = %v", id, err)
		}
	}

	// Batch 1: f1 fails (backed off), f2 and f3 are NOT blocked and both succeed & get acked.
	acked, err := svc.ProcessBlobCleanups(ctx, 10)
	if err == nil {
		t.Fatal("expected error from transient disk failure on f1")
	}
	if acked != 2 {
		t.Fatalf("acked = %d, want 2 (f2 deleted + f3 missing-object acked)", acked)
	}

	// Verify f2 disk object is gone while f1 disk object still exists.
	if _, err := os.Stat(filepath.Join(dir, "objects", rec2.ObjectKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object 2 should be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects", rec1.ObjectKey)); err != nil {
		t.Fatalf("object 1 should still exist after failed delete: %v", err)
	}

	// Close SQLite and reopen with a healthy localdisk.Store to simulate process restart.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("reopened NewStore() error = %v", err)
	}
	defer reopened.Close()

	restartedDisk, err := localdisk.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	restartedSvc, err := files.NewService(restartedDisk, reopened, idGen)
	if err != nil {
		t.Fatal(err)
	}

	// Run cleanup worker briefly on startup; it resets retries, deletes f1's blob, and exits on cancel.
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		restartedSvc.RunCleanupWorker(workerCtx, 50*time.Millisecond, 10, nil)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "objects", rec1.ObjectKey)); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if _, err := os.Stat(filepath.Join(dir, "objects", rec1.ObjectKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object 1 should be deleted after restart retry, stat err = %v", err)
	}

	remaining, err := reopened.ListDueCleanups(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDueCleanups() after restart error = %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining cleanup tasks = %d, want 0", len(remaining))
	}
}
