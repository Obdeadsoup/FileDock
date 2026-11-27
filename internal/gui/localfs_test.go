package gui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadTOCTOUAskAndRenameNeverOverwrite(t *testing.T) {
	dir := t.TempDir()
	downloaded := []byte("fresh-download-bytes")
	sum := sha256.Sum256(downloaded)
	sha := hex.EncodeToString(sum[:])

	// 1. Policy "ask": file appears AFTER pre-check and BEFORE final publish -> must NOT overwrite!
	existingContent := []byte("pre-existing-raced-file")
	_, conflict, err := downloadStreamToLocal(
		dir, "", "race.txt", sha, "ask",
		func() (downloadStreamResult, error) {
			return downloadStreamResult{Body: io.NopCloser(bytes.NewReader(downloaded)), HeaderSHA256: sha}, nil
		},
		func() {
			_ = os.WriteFile(filepath.Join(dir, "race.txt"), existingContent, 0o644)
		},
	)
	if !errors.Is(err, ErrLocalConflict) || conflict == nil || conflict.TargetPath != "race.txt" {
		t.Fatalf("TOCTOU ask = (conflict=%+v, err=%v), want ErrLocalConflict", conflict, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "race.txt"))
	if !bytes.Equal(got, existingContent) {
		t.Fatalf("TOCTOU ask overwrote existing file: got %q, want %q", got, existingContent)
	}

	// 2. Policy "rename": "race (1).txt" also appears BEFORE final publish -> must publish to "race (2).txt"!
	raced1 := []byte("raced-numbered-1")
	res, conflict2, err := downloadStreamToLocal(
		dir, "", "race.txt", sha, "rename",
		func() (downloadStreamResult, error) {
			return downloadStreamResult{Body: io.NopCloser(bytes.NewReader(downloaded)), HeaderSHA256: sha}, nil
		},
		func() {
			_ = os.WriteFile(filepath.Join(dir, "race (1).txt"), raced1, 0o644)
		},
	)
	if err != nil || conflict2 != nil || res.SavedPath != "race (2).txt" {
		t.Fatalf("TOCTOU rename = (%+v, conflict=%+v, err=%v), want race (2).txt", res, conflict2, err)
	}
	got1, _ := os.ReadFile(filepath.Join(dir, "race (1).txt"))
	if !bytes.Equal(got1, raced1) {
		t.Fatalf("TOCTOU rename overwrote race (1).txt: got %q", got1)
	}
	got2, _ := os.ReadFile(filepath.Join(dir, "race (2).txt"))
	if !bytes.Equal(got2, downloaded) {
		t.Fatalf("race (2).txt content mismatch: got %q", got2)
	}
}

func TestStale401DoesNotClearNewerLogin(t *testing.T) {
	rc, err := newRemoteClient("http://127.0.0.1:8080", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// First login (gen 1)
	rc.setAuth("old-token", &RemoteUser{ID: "u1", Username: "alice"})
	oldTok, _, oldGen := rc.currentAuthWithGen()

	// Newer login (gen 2) completes before the old request's 401 arrives
	rc.setAuth("new-token", &RemoteUser{ID: "u2", Username: "bob"})

	// Stale 401 from old-token arrives -> must NOT clear new-token!
	rc.clearAuthIfMatching(oldTok, oldGen)

	curTok, curUser := rc.currentAuth()
	if curTok != "new-token" || curUser == nil || curUser.Username != "bob" {
		t.Fatalf("stale 401 cleared newer login: tok=%q user=%+v", curTok, curUser)
	}

	// Matching 401 from new-token DOES clear it.
	newTok, _, newGen := rc.currentAuthWithGen()
	rc.clearAuthIfMatching(newTok, newGen)
	curTok, curUser = rc.currentAuth()
	if curTok != "" || curUser != nil {
		t.Fatalf("matching 401 failed to clear auth: tok=%q user=%+v", curTok, curUser)
	}
}
