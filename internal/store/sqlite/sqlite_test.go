package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/store/localdisk"
)

func TestStorePersistenceOrderingPaginationAndOwnerIsolation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	empty, err := store.List(ctx, "user-1", files.ListPage{Limit: 10, Offset: 0})
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
			OwnerID:   "user-1",
			ObjectKey: "obj-b",
		},
		{
			File:      files.File{ID: "id-a", Name: "same.txt", Size: 20, SHA256: "hash-a", CreatedAt: t1},
			OwnerID:   "user-1",
			ObjectKey: "obj-a",
		},
		{
			File:      files.File{ID: "id-c", Name: "newer.txt", Size: 30, SHA256: "hash-c", CreatedAt: t2},
			OwnerID:   "user-1",
			ObjectKey: "obj-c",
		},
		{
			File:      files.File{ID: "id-other", Name: "bob.txt", Size: 40, SHA256: "hash-d", CreatedAt: t2},
			OwnerID:   "user-2",
			ObjectKey: "obj-d",
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

	page1, err := reopened.List(ctx, "user-1", files.ListPage{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("List(page1) error = %v", err)
	}
	if len(page1) != 2 || page1[0].ID != "id-c" || page1[1].ID != "id-a" {
		t.Fatalf("page1 = %+v, want [id-c, id-a]", page1)
	}

	page2, err := reopened.List(ctx, "user-1", files.ListPage{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("List(page2) error = %v", err)
	}
	if len(page2) != 1 || page2[0].ID != "id-b" {
		t.Fatalf("page2 = %+v, want [id-b]", page2)
	}

	got, err := reopened.GetByID(ctx, "user-1", "id-a")
	if err != nil {
		t.Fatalf("GetByID(user-1, id-a) error = %v", err)
	}
	if got.ObjectKey != "obj-a" || !got.CreatedAt.Equal(t1) || got.SHA256 != "hash-a" {
		t.Fatalf("GetByID(user-1, id-a) = %+v", got)
	}

	// Cross-user access and missing ID both return ErrNotFound.
	if _, err := reopened.GetByID(ctx, "user-2", "id-a"); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("GetByID(user-2, id-a) = %v, want ErrNotFound", err)
	}
	if _, err := reopened.GetByID(ctx, "user-1", "missing"); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("GetByID(user-1, missing) = %v, want ErrNotFound", err)
	}
}

func TestRound2MigrationAndClaimLegacyFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 1. Write actual disk object using localdisk.Store (as in Round 2).
	blobStore, err := localdisk.NewStore(dir)
	if err != nil {
		t.Fatalf("localdisk.NewStore() error = %v", err)
	}
	legacyPayload := []byte("round 2 legacy file payload stored before accounts existed")
	sum := sha256.Sum256(legacyPayload)
	wantSHA := hex.EncodeToString(sum[:])

	staged, err := blobStore.Save(ctx, bytes.NewReader(legacyPayload), int64(len(legacyPayload)))
	if err != nil {
		t.Fatalf("blobStore.Save() error = %v", err)
	}

	// 2. Construct exact Round 2 SQLite schema (without owner_id) and insert a legacy file record.
	dbPath := filepath.Join(dir, "filedock.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	const round2DDL = `
CREATE TABLE files (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	object_key TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL
);
CREATE INDEX idx_files_created_at_ns_id
	ON files (created_at_ns DESC, id ASC);
`
	if _, err := rawDB.ExecContext(ctx, round2DDL); err != nil {
		t.Fatalf("create round 2 schema: %v", err)
	}
	legacyCreatedAt := time.Date(2026, 11, 19, 15, 0, 0, 123456000, time.UTC)
	const legacyFileID = "legacy-file-001"
	_, err = rawDB.ExecContext(
		ctx,
		`INSERT INTO files (id, name, size, sha256, object_key, created_at, created_at_ns) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		legacyFileID,
		"round2-report.txt",
		staged.Size,
		staged.SHA256,
		staged.ObjectKey,
		legacyCreatedAt.Format(time.RFC3339Nano),
		legacyCreatedAt.UnixNano(),
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("rawDB.Close() error = %v", err)
	}

	// 3. Run NewStore twice to verify repeatable transactional migration.
	store1, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("first migrated NewStore() error = %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close() error = %v", err)
	}

	store, err := NewStore(ctx, dir)
	if err != nil {
		t.Fatalf("second migrated NewStore() error = %v", err)
	}
	defer store.Close()

	accountSvc, err := accounts.NewService(store, accounts.WithArgon2Params(accounts.Argon2Params{
		Time:    1,
		Memory:  8 * 1024,
		Threads: 1,
		KeyLen:  32,
	}))
	if err != nil {
		t.Fatalf("accounts.NewService() error = %v", err)
	}
	fileSvc, err := files.NewService(blobStore, store, func() (string, error) { return "new-id", nil })
	if err != nil {
		t.Fatalf("files.NewService() error = %v", err)
	}

	// 4. Register alice and bob; neither implicitly claims the legacy file.
	alice, err := accountSvc.Register(ctx, "alice", "alice-pass")
	if err != nil {
		t.Fatalf("Register(alice) error = %v", err)
	}
	bob, err := accountSvc.Register(ctx, "bob", "bob-pass")
	if err != nil {
		t.Fatalf("Register(bob) error = %v", err)
	}

	for _, u := range []accounts.User{alice, bob} {
		list, err := fileSvc.List(ctx, u.ID, 0, 0)
		if err != nil {
			t.Fatalf("List(%s) error = %v", u.Username, err)
		}
		if len(list) != 0 {
			t.Fatalf("legacy file must be hidden from %s before claim; got %+v", u.Username, list)
		}
		if _, _, err := fileSvc.OpenContent(ctx, u.ID, legacyFileID); !errors.Is(err, files.ErrNotFound) {
			t.Fatalf("OpenContent(%s, legacy) before claim = %v, want ErrNotFound", u.Username, err)
		}
	}

	// 5. Claim for nonexistent user fails.
	if _, err := store.ClaimLegacyFiles(ctx, "unknown"); !errors.Is(err, accounts.ErrUserNotFound) {
		t.Fatalf("ClaimLegacyFiles(unknown) = %v, want ErrUserNotFound", err)
	}

	// 6. Explicitly claim legacy files for alice.
	claimed, err := store.ClaimLegacyFiles(ctx, "alice")
	if err != nil {
		t.Fatalf("ClaimLegacyFiles(alice) error = %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}

	// 7. Alice can now list and download by original ID with intact metadata and SHA-256.
	aliceFiles, err := fileSvc.List(ctx, alice.ID, 0, 0)
	if err != nil {
		t.Fatalf("List(alice) after claim error = %v", err)
	}
	if len(aliceFiles) != 1 {
		t.Fatalf("len(aliceFiles) = %d, want 1", len(aliceFiles))
	}
	if aliceFiles[0].ID != legacyFileID || aliceFiles[0].SHA256 != wantSHA || !aliceFiles[0].CreatedAt.Equal(legacyCreatedAt) {
		t.Fatalf("migrated metadata mismatch: %+v", aliceFiles[0])
	}

	meta, rc, err := fileSvc.OpenContent(ctx, alice.ID, legacyFileID)
	if err != nil {
		t.Fatalf("OpenContent(alice, legacy) after claim error = %v", err)
	}
	downloaded, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	gotSum := sha256.Sum256(downloaded)
	if hex.EncodeToString(gotSum[:]) != wantSHA || meta.SHA256 != wantSHA {
		t.Fatalf("downloaded SHA-256 mismatch after migration and claim")
	}

	// Bob still cannot access Alice's claimed file.
	if _, _, err := fileSvc.OpenContent(ctx, bob.ID, legacyFileID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("OpenContent(bob, legacy) after claim = %v, want ErrNotFound", err)
	}

	// 8. Repeated claim is idempotent and reports 0 newly claimed files.
	claimedAgain, err := store.ClaimLegacyFiles(ctx, "bob")
	if err != nil {
		t.Fatalf("repeated ClaimLegacyFiles(bob) error = %v", err)
	}
	if claimedAgain != 0 {
		t.Fatalf("claimedAgain = %d, want 0", claimedAgain)
	}
}
