package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/store/localdisk"
	"example.com/filedock/internal/store/sqlite"
)

type flakyDeleteChunksBlobStore struct {
	*localdisk.Store
	failNextDeleteSession bool
}

func (f *flakyDeleteChunksBlobStore) DeleteSessionChunks(ctx context.Context, sessionID string) error {
	if f.failNextDeleteSession {
		f.failNextDeleteSession = false
		return errors.New("injected disk cleanup failure")
	}
	return f.Store.DeleteSessionChunks(ctx, sessionID)
}

// Regression 5: If DeleteSessionChunks fails right after CompleteUploadSession commits,
// a subsequent cleanup pass (including after restart) must still find and clean up the chunks,
// while keeping idempotent complete + single sync event + single audit record.
func TestRegression5_CompletedUploadChunkCleanupRetriesAfterFailureAndRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	blobs1, err := localdisk.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	meta1, err := sqlite.NewStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}

	var seq int
	idGen := func() (string, error) {
		seq++
		return fmt.Sprintf("rc-%d", seq), nil
	}
	acctSvc, err := accounts.NewService(meta1, accounts.WithIDGenerator(idGen))
	if err != nil {
		t.Fatal(err)
	}
	alice, err := acctSvc.Register(ctx, "alice", "password123")
	if err != nil {
		t.Fatal(err)
	}

	flakyBlobs := &flakyDeleteChunksBlobStore{Store: blobs1, failNextDeleteSession: true}
	svc1, err := files.NewService(flakyBlobs, meta1, idGen, files.WithUploadChunkSize(8))
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("0123456789abcdef")
	sess, err := svc1.CreateUploadSession(ctx, alice.ID, files.CreateUploadRequest{
		Name:   "retry-clean.bin",
		Size:   int64(len(payload)),
		SHA256: shaHex(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc1.UploadChunk(ctx, alice.ID, sess.ID, 0, shaHex(payload[:8]), bytes.NewReader(payload[:8])); err != nil {
		t.Fatal(err)
	}
	if _, err := svc1.UploadChunk(ctx, alice.ID, sess.ID, 1, shaHex(payload[8:]), bytes.NewReader(payload[8:])); err != nil {
		t.Fatal(err)
	}

	// Complete succeeds, but immediate DeleteSessionChunks fails!
	f1, err := svc1.CompleteUploadSession(ctx, alice.ID, sess.ID)
	if err != nil {
		t.Fatalf("CompleteUploadSession: %v", err)
	}
	// Staged chunk dir still exists on disk because cleanup failed.
	chunkDir := filepath.Join(dataDir, "uploads", sess.ID)
	if _, err := os.Stat(chunkDir); err != nil {
		t.Fatalf("expected leftover chunk dir after injected cleanup failure, got %v", err)
	}
	_ = meta1.Close()

	// Simulate server restart: reopen sqlite & localdisk and run cleanup worker pass.
	blobs2, err := localdisk.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	meta2, err := sqlite.NewStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer meta2.Close()

	svc2, err := files.NewService(blobs2, meta2, idGen, files.WithUploadChunkSize(8))
	if err != nil {
		t.Fatal(err)
	}

	cleaned, err := svc2.ProcessExpiredUploadSessions(ctx, 32)
	if err != nil {
		t.Fatalf("ProcessExpiredUploadSessions after restart: %v", err)
	}
	if cleaned < 1 {
		t.Fatalf("cleaned = %d, want >= 1 (leftover completed upload chunks not swept!)", cleaned)
	}
	if _, err := os.Stat(chunkDir); !os.IsNotExist(err) {
		t.Fatalf("expected chunk dir %s to be deleted after cleanup retry, err=%v", chunkDir, err)
	}

	// Retrying CompleteUploadSession after cleanup must STILL return the same file ID with 1 sync event & 1 audit record.
	f2, err := svc2.CompleteUploadSession(ctx, alice.ID, sess.ID)
	if err != nil || f2.ID != f1.ID {
		t.Fatalf("retry CompleteUploadSession = (%+v, %v), want ID=%s", f2, err, f1.ID)
	}
	changes, err := svc2.ListChanges(ctx, alice.ID, 0, 50)
	if err != nil || len(changes.Items) != 1 {
		t.Fatalf("sync changes = %d (err=%v), want 1", len(changes.Items), err)
	}
	audits, err := meta2.ListAuditLogs(ctx, files.AuditFilter{ActorUserID: alice.ID, Action: files.AuditActionFileUpload, Limit: 10})
	if err != nil || len(audits) != 1 {
		t.Fatalf("audit logs = %d (err=%v), want 1", len(audits), err)
	}
}
