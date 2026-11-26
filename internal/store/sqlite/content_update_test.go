package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/store/localdisk"
)

func TestReplaceContentTransactionRollbackPreservesOriginalAndCleansNewBlob(t *testing.T) {
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

	svc, err := files.NewService(blobs, store, func() (string, error) { return "file-1", nil })
	if err != nil {
		t.Fatal(err)
	}

	origPayload := []byte("original v1 content")
	uploaded, err := svc.Upload(ctx, alice.ID, "doc.txt", bytes.NewReader(origPayload))
	if err != nil {
		t.Fatal(err)
	}
	origETag := files.ContentETag(uploaded)

	// Force ReplaceFileContent transaction to abort when enqueueing old blob cleanup.
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER fail_replace_cleanup BEFORE INSERT ON blob_cleanups
BEGIN
	SELECT RAISE(ABORT, 'simulated replace rollback');
END;`); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.ReplaceContent(ctx, alice.ID, uploaded.ID, origETag, bytes.NewReader([]byte("new v2 bytes"))); err == nil {
		t.Fatal("expected error from rolled-back ReplaceContent transaction")
	}

	// Original metadata and disk object must remain intact; newly written blob must be cleaned up.
	meta, rc, err := svc.OpenContent(ctx, alice.ID, uploaded.ID)
	if err != nil {
		t.Fatalf("OpenContent after rollback error = %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || meta.ContentRevision != 1 || !bytes.Equal(got, origPayload) {
		t.Fatalf("after rollback: rev=%d content=%q err=%v", meta.ContentRevision, got, err)
	}

	objs, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("objects on disk = %d, want 1 (newly staged blob must be cleaned up)", len(objs))
	}
}

func TestContentRevisionMigrationFromLegacyDB(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	blobs, err := localdisk.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := blobs.Save(ctx, bytes.NewReader([]byte("legacy v1")), 1024)
	if err != nil {
		t.Fatal(err)
	}

	// Construct a pre-content_revision SQLite DB (with files & sync_changes lacking content_revision).
	dbPath := filepath.Join(dir, "filedock.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const oldDDL = `
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
CREATE TABLE files (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	object_key TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	owner_id TEXT NOT NULL DEFAULT '',
	parent_id TEXT
);
CREATE TABLE sync_sequences (
	owner_id TEXT PRIMARY KEY NOT NULL,
	last_seq INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE sync_changes (
	owner_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	event_type TEXT NOT NULL,
	resource_id TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	name TEXT,
	parent_id TEXT,
	size INTEGER,
	sha256 TEXT,
	resource_created_at TEXT,
	resource_updated_at TEXT,
	PRIMARY KEY (owner_id, seq)
);`
	if _, err := rawDB.ExecContext(ctx, oldDDL); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	ts := now.Format(time.RFC3339Nano)
	if _, err := rawDB.ExecContext(ctx, `
INSERT INTO users (id, username, password_hash, password_salt, argon_time, argon_memory, argon_threads, argon_key_len, created_at, created_at_ns)
VALUES ('u1', 'alice', X'01', X'02', 1, 8192, 1, 32, ?, ?)`, ts, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `
INSERT INTO files (id, name, size, sha256, object_key, created_at, created_at_ns, owner_id, parent_id)
VALUES ('f1', 'old.txt', ?, ?, ?, ?, ?, 'u1', NULL)`, staged.Size, staged.SHA256, staged.ObjectKey, ts, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `INSERT INTO sync_sequences (owner_id, last_seq) VALUES ('u1', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `
INSERT INTO sync_changes (owner_id, seq, event_type, resource_id, occurred_at, name, parent_id, size, sha256, resource_created_at)
VALUES ('u1', 1, 'file.upsert', 'f1', ?, 'old.txt', NULL, ?, ?, ?)`, ts, staged.Size, staged.SHA256, ts); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	// Open twice to verify transactional repeatable migration.
	s1, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("first NewStore() error = %v", err)
	}
	_ = s1.Close()

	store, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("second NewStore() error = %v", err)
	}
	defer store.Close()

	rec, err := store.GetByID(ctx, "u1", "f1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ContentRevision != 1 {
		t.Fatalf("migrated file content_revision = %d, want 1", rec.ContentRevision)
	}

	page, err := store.ListChanges(ctx, "u1", 0, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].File == nil || page.Items[0].File.ContentRevision != 1 {
		t.Fatalf("migrated sync event = %+v (err=%v), want content_revision=1", page, err)
	}

	svc, err := files.NewService(blobs, store, func() (string, error) { return "unused", nil })
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.ReplaceContent(ctx, "u1", "f1", files.ContentETag(rec.File), bytes.NewReader([]byte("migrated v2")))
	if err != nil {
		t.Fatalf("ReplaceContent on migrated file error = %v", err)
	}
	if updated.ContentRevision != 2 {
		t.Fatalf("updated content_revision = %d, want 2", updated.ContentRevision)
	}
}
