package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/store/sqlite"
)

func TestClaimLegacySubcommand(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("FILEDOCK_DATA_DIR", dataDir)

	ctx := context.Background()
	dbPath := filepath.Join(dataDir, "filedock.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rawDB.ExecContext(ctx, `
CREATE TABLE files (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	object_key TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL
);
INSERT INTO files (id, name, size, sha256, object_key, created_at, created_at_ns)
VALUES ('legacy-1', 'old.txt', 5, 'abc', 'obj-1', '2026-11-19T00:00:00Z', 100);
`)
	if err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	store, err := sqlite.NewStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	acctSvc, err := accounts.NewService(store, accounts.WithArgon2Params(accounts.Argon2Params{
		Time:    1,
		Memory:  8 * 1024,
		Threads: 1,
		KeyLen:  32,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acctSvc.Register(ctx, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Missing --owner returns error.
	if err := run([]string{"claim-legacy"}, io.Discard, logger); err == nil {
		t.Fatal("expected error when --owner is omitted")
	}

	// Claim for alice succeeds and reports 1 claimed file.
	var out bytes.Buffer
	if err := run([]string{"claim-legacy", "--owner", "alice"}, &out, logger); err != nil {
		t.Fatalf("run(claim-legacy) error = %v", err)
	}
	if !strings.Contains(out.String(), "claimed 1 legacy file(s) for owner alice") {
		t.Fatalf("output = %q, want claimed 1 message", out.String())
	}

	// Repeated claim reports 0 claimed files.
	out.Reset()
	if err := run([]string{"claim-legacy", "--owner", "alice"}, &out, logger); err != nil {
		t.Fatalf("second run(claim-legacy) error = %v", err)
	}
	if !strings.Contains(out.String(), "claimed 0 legacy file(s) for owner alice") {
		t.Fatalf("second output = %q, want claimed 0 message", out.String())
	}

	_ = time.UTC
}
