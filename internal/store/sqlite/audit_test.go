package sqlite_test

import (
	"context"
	"testing"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/store/sqlite"
)

func TestAuditLogsCoverOperationsRollbackAndIdempotency(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.NewStore(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	for _, u := range []accounts.StoredUser{
		{
			User:         accounts.User{ID: "u-alice", Username: "alice", CreatedAt: now},
			PasswordHash: []byte("h"), PasswordSalt: []byte("s"),
			ArgonParams: accounts.Argon2Params{Time: 1, Memory: 64, Threads: 1, KeyLen: 32},
		},
		{
			User:         accounts.User{ID: "u-bob", Username: "bob", CreatedAt: now},
			PasswordHash: []byte("h"), PasswordSalt: []byte("s"),
			ArgonParams: accounts.Argon2Params{Time: 1, Memory: 64, Threads: 1, KeyLen: 32},
		},
	} {
		if err := store.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Folder create + update + rollback on empty-folder violation.
	f1 := files.Folder{ID: "dir-1", Name: "docs", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateFolder(ctx, "u-alice", f1); err != nil {
		t.Fatal(err)
	}
	newFolderName := "docs-renamed"
	if _, err := store.UpdateFolder(ctx, "u-alice", "dir-1", files.Changes{Name: &newFolderName}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	// 2. Upload session complete + idempotent retry -> only ONE file.upload audit record!
	dirID := "dir-1"
	sess := files.UploadSession{
		ID: "sess-1", OwnerID: "u-alice", ParentID: &dirID, Name: "a.txt",
		Size: 5, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ChunkSize: 10, TotalChunks: 1, Status: files.UploadStatusOpen,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := store.CreateUploadSession(ctx, sess, 16, 1<<20, now); err != nil {
		t.Fatal(err)
	}
	sf := files.StoredFile{
		File: files.File{
			ID: "file-1", Name: "a.txt", ParentID: &dirID, Size: 5,
			SHA256: sess.SHA256, ContentRevision: 1, CreatedAt: now.Add(2 * time.Second),
		},
		OwnerID: "u-alice", ObjectKey: "0123456789abcdef0123456789abcdef",
	}
	if _, already, err := store.CompleteUploadSession(ctx, "u-alice", "sess-1", sf, now.Add(2*time.Second)); err != nil || already {
		t.Fatalf("first CompleteUploadSession = (already=%v, err=%v)", already, err)
	}
	// Retry complete must NOT append a second audit record.
	if _, already, err := store.CompleteUploadSession(ctx, "u-alice", "sess-1", sf, now.Add(3*time.Second)); err != nil || !already {
		t.Fatalf("retry CompleteUploadSession = (already=%v, err=%v)", already, err)
	}

	// 3. Rollback test: deleting non-empty dir-1 fails and must NOT record folder.delete!
	if err := store.DeleteFolder(ctx, "u-alice", "dir-1", now.Add(4*time.Second)); err != files.ErrFolderNotEmpty {
		t.Fatalf("DeleteFolder(non-empty) = %v, want ErrFolderNotEmpty", err)
	}

	// 4. File content update (and rollback on wrong ETag).
	if _, err := store.ReplaceFileContent(ctx, "u-alice", "file-1", `"wrong-etag"`, files.StagedBlob{
		ObjectKey: "fedcba9876543210fedcba9876543210", Size: 6, SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}, now.Add(5*time.Second)); err != files.ErrPreconditionFailed {
		t.Fatalf("ReplaceFileContent(wrong ETag) = %v, want ErrPreconditionFailed", err)
	}
	if _, err := store.ReplaceFileContent(ctx, "u-alice", "file-1", files.ContentETag(sf.File), files.StagedBlob{
		ObjectKey: "fedcba9876543210fedcba9876543210", Size: 6, SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}, now.Add(6*time.Second)); err != nil {
		t.Fatalf("ReplaceFileContent: %v", err)
	}

	// 5. File rename/move.
	newFileName := "b.txt"
	if _, err := store.UpdateFile(ctx, "u-alice", "file-1", files.Changes{Name: &newFileName}, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}

	// 6. Share create + duplicate idempotent create (must only audit once!) + share delete.
	sh, created, err := store.CreateShare(ctx, "u-alice", "share-1", files.CreateShareInput{
		ResourceType: files.ResourceTypeFile, ResourceID: "file-1", RecipientUsername: "bob",
	}, now.Add(8*time.Second))
	if err != nil || !created {
		t.Fatalf("CreateShare = (%+v, %v, %v)", sh, created, err)
	}
	if _, created2, err := store.CreateShare(ctx, "u-alice", "share-dup", files.CreateShareInput{
		ResourceType: files.ResourceTypeFile, ResourceID: "file-1", RecipientUsername: "bob",
	}, now.Add(9*time.Second)); err != nil || created2 {
		t.Fatalf("duplicate CreateShare = (created=%v, err=%v)", created2, err)
	}
	if err := store.DeleteShare(ctx, "u-alice", "share-1"); err != nil {
		t.Fatal(err)
	}

	// 7. File delete + folder delete.
	if err := store.DeleteFile(ctx, "u-alice", "file-1", now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteFolder(ctx, "u-alice", "dir-1", now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Verify exact set of 9 audit entries (zero duplicates, zero failed/rolled-back entries).
	all, err := store.ListAuditLogs(ctx, files.AuditFilter{ActorUserID: "u-alice", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 9 {
		t.Fatalf("audit count = %d, want 9: %+v", len(all), all)
	}
	counts := make(map[string]int)
	for _, e := range all {
		counts[e.Action]++
	}
	for _, wantAction := range []string{
		files.AuditActionFolderCreate,
		files.AuditActionFolderUpdate,
		files.AuditActionFileUpload,
		files.AuditActionFileContentUpdate,
		files.AuditActionFileUpdate,
		files.AuditActionShareCreate,
		files.AuditActionShareDelete,
		files.AuditActionFileDelete,
		files.AuditActionFolderDelete,
	} {
		if counts[wantAction] != 1 {
			t.Errorf("audit count for %s = %d, want 1", wantAction, counts[wantAction])
		}
	}

	// Verify filtering by action and limit.
	uploads, err := store.ListAuditLogs(ctx, files.AuditFilter{Action: files.AuditActionFileUpload, Limit: 5})
	if err != nil || len(uploads) != 1 || uploads[0].ResourceID != "file-1" {
		t.Fatalf("filtered file.upload = (%+v, %v)", uploads, err)
	}
	limited, err := store.ListAuditLogs(ctx, files.AuditFilter{Limit: 2})
	if err != nil || len(limited) != 2 {
		t.Fatalf("limited audit logs = (%d, %v), want 2", len(limited), err)
	}
}
