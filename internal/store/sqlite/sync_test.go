package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/store/localdisk"
)

func TestSyncChangeWriteFailureRollsBackMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	blobs, err := localdisk.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
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

	seq := 0
	svc, err := files.NewService(blobs, store, func() (string, error) {
		seq++
		return "res-" + string(rune('a'+seq-1)), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	folder, err := svc.CreateFolder(ctx, alice.ID, nil, "orig-dir")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("rollback-protected payload")
	uploaded, err := svc.UploadTo(ctx, alice.ID, &folder.ID, "orig.txt", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	// Abort any subsequent insert into sync_changes.
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER fail_sync_insert BEFORE INSERT ON sync_changes
BEGIN
	SELECT RAISE(ABORT, 'simulated sync_changes write failure');
END;`); err != nil {
		t.Fatal(err)
	}

	// 1. Folder rename fails and keeps original name.
	newFolderName := "should-rollback-dir"
	if _, err := svc.UpdateFolder(ctx, alice.ID, folder.ID, files.Changes{Name: &newFolderName}); err == nil {
		t.Fatal("UpdateFolder expected error when sync_changes insert fails")
	}
	folders, err := svc.ListFolders(ctx, alice.ID, nil, 10, 0)
	if err != nil || len(folders) != 1 || folders[0].Name != "orig-dir" {
		t.Fatalf("folders after rollback = %+v, err=%v; want orig-dir", folders, err)
	}

	// 2. File rename fails and keeps original name.
	newFileName := "should-rollback.txt"
	if _, err := svc.UpdateFile(ctx, alice.ID, uploaded.ID, files.Changes{Name: &newFileName}); err == nil {
		t.Fatal("UpdateFile expected error when sync_changes insert fails")
	}

	// 3. File delete fails, preserves metadata + downloadable blob, and enqueues no blob_cleanups.
	if err := svc.DeleteFile(ctx, alice.ID, uploaded.ID); err == nil {
		t.Fatal("DeleteFile expected error when sync_changes insert fails")
	}
	meta, rc, err := svc.OpenContent(ctx, alice.ID, uploaded.ID)
	if err != nil {
		t.Fatalf("OpenContent after failed delete error = %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || meta.Name != "orig.txt" || !bytes.Equal(got, payload) {
		t.Fatalf("file after rolled-back delete = (%q, %q, err=%v)", meta.Name, got, err)
	}
	due, err := store.ListDueCleanups(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("blob_cleanups after rolled-back delete = %d (err=%v), want 0", len(due), err)
	}
}

func TestSyncMigrationBackfillOrderIdempotencyAndClaimLegacy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Build a pre-sync database (with users, folders, files, shares, blob_cleanups, but NO sync_changes table).
	dbPath := filepath.Join(dir, "filedock.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const preSyncDDL = `
PRAGMA foreign_keys = ON;
CREATE TABLE users (
	id TEXT PRIMARY KEY,
	username TEXT NOT NULL UNIQUE,
	password_hash BLOB NOT NULL,
	password_salt BLOB NOT NULL,
	argon_time INTEGER NOT NULL,
	argon_memory INTEGER NOT NULL,
	argon_threads INTEGER NOT NULL,
	argon_key_len INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL
);
CREATE TABLE sessions (
	token_hash TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	expires_at_ns INTEGER NOT NULL,
	FOREIGN KEY (user_id) REFERENCES users(id)
);
CREATE TABLE folders (
	id TEXT PRIMARY KEY NOT NULL,
	owner_id TEXT NOT NULL REFERENCES users(id),
	parent_id TEXT REFERENCES folders(id),
	name TEXT NOT NULL,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (owner_id, parent_id, name)
);
CREATE TABLE files (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	object_key TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	owner_id TEXT NOT NULL DEFAULT '',
	parent_id TEXT REFERENCES folders(id)
);
`
	if _, err := rawDB.ExecContext(ctx, preSyncDDL); err != nil {
		t.Fatalf("init pre-sync schema: %v", err)
	}

	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339Nano) }
	ns := func(d time.Duration) int64 { return t0.Add(d).UnixNano() }

	if _, err := rawDB.ExecContext(ctx, `
INSERT INTO users (id, username, password_hash, password_salt, argon_time, argon_memory, argon_threads, argon_key_len, created_at, created_at_ns)
VALUES ('user-a', 'alice', X'01', X'02', 1, 8192, 1, 32, ?, ?)`,
		ts(0), ns(0)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := rawDB.ExecContext(ctx, `
INSERT INTO folders (id, owner_id, parent_id, name, created_at, created_at_ns, updated_at) VALUES
	('dir-root',  'user-a', NULL,        'root-dir',  ?, ?, ?),
	('dir-child', 'user-a', 'dir-root',  'child-dir', ?, ?, ?),
	('dir-grand', 'user-a', 'dir-child', 'grand-dir', ?, ?, ?)`,
		ts(3*time.Second), ns(3*time.Second), ts(3*time.Second),
		ts(2*time.Second), ns(2*time.Second), ts(2*time.Second),
		ts(1*time.Second), ns(1*time.Second), ts(1*time.Second)); err != nil {
		t.Fatalf("seed folders: %v", err)
	}
	if _, err := rawDB.ExecContext(ctx, `
INSERT INTO files (id, name, size, sha256, object_key, created_at, created_at_ns, owner_id, parent_id) VALUES
	('file-owned-deep', 'deep.txt',   12, 'sha-deep',   'obj-deep',   ?, ?, 'user-a', 'dir-grand'),
	('file-owned-root', 'root.txt',    8, 'sha-root',   'obj-root',   ?, ?, 'user-a', NULL),
	('file-legacy',     'legacy.txt',  5, 'sha-legacy', 'obj-legacy', ?, ?, '',       NULL)`,
		ts(4*time.Second), ns(4*time.Second),
		ts(5*time.Second), ns(5*time.Second),
		ts(0), ns(0)); err != nil {
		t.Fatalf("seed files: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	// Open store twice to verify first-time backfill + idempotent restart (no duplicate events).
	store1, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("first NewStore() error = %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("second NewStore() error = %v", err)
	}
	defer store.Close()

	page, err := store.ListChanges(ctx, "user-a", 0, 50)
	if err != nil {
		t.Fatalf("ListChanges() error = %v", err)
	}
	// Expect exactly 5 events: 3 folders (root -> child -> grand) + 2 owned files; NO legacy file and NO duplicates.
	if len(page.Items) != 5 || page.HasMore || page.NextCursor != 5 {
		t.Fatalf("backfilled page = (len=%d, next=%d, has_more=%v), want (5, 5, false)", len(page.Items), page.NextCursor, page.HasMore)
	}

	wantOrder := []struct {
		typ string
		id  string
	}{
		{files.ChangeFolderUpsert, "dir-root"},
		{files.ChangeFolderUpsert, "dir-child"},
		{files.ChangeFolderUpsert, "dir-grand"},
		{files.ChangeFileUpsert, "file-owned-deep"},
		{files.ChangeFileUpsert, "file-owned-root"},
	}
	for i, want := range wantOrder {
		ev := page.Items[i]
		if ev.Seq != int64(i+1) || ev.Type != want.typ || ev.ResourceID != want.id {
			t.Fatalf("item[%d] = (seq=%d, type=%s, id=%s), want (%d, %s, %s)", i, ev.Seq, ev.Type, ev.ResourceID, i+1, want.typ, want.id)
		}
	}

	// Claim legacy files for alice -> legacy file now enters alice's change stream as seq 6 with parent_id == nil.
	claimed, err := store.ClaimLegacyFiles(ctx, "alice")
	if err != nil || claimed != 1 {
		t.Fatalf("ClaimLegacyFiles(alice) = (%d, %v), want (1, nil)", claimed, err)
	}
	afterClaim, err := store.ListChanges(ctx, "user-a", page.NextCursor, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterClaim.Items) != 1 || afterClaim.Items[0].Seq != 6 ||
		afterClaim.Items[0].Type != files.ChangeFileUpsert ||
		afterClaim.Items[0].ResourceID != "file-legacy" ||
		afterClaim.Items[0].File == nil || afterClaim.Items[0].File.ParentID != nil {
		t.Fatalf("afterClaim = %+v, want single file.upsert for file-legacy at seq 6", afterClaim)
	}
}
