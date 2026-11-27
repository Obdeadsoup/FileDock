package localdisk_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"example.com/filedock/internal/store/localdisk"
)

func TestEncryptedStoreObjectsChunksTamperAndSwap(t *testing.T) {
	ctx := context.Background()
	secretDir := t.TempDir()
	keyPath := filepath.Join(secretDir, "master.keyset.json")
	if err := localdisk.GenerateKeysetFile(keyPath); err != nil {
		t.Fatalf("GenerateKeysetFile: %v", err)
	}

	dataDir := filepath.Join(t.TempDir(), "data")
	store, err := localdisk.NewStoreWithConfig(dataDir, keyPath)
	if err != nil {
		t.Fatalf("NewStoreWithConfig(encrypted): %v", err)
	}
	if store.Mode() != localdisk.StorageModeEncrypted {
		t.Fatalf("Mode() = %q, want %q", store.Mode(), localdisk.StorageModeEncrypted)
	}

	// 1. Formal object encryption: plaintext SHA-256 & size match, disk bytes are ciphertext.
	plainA := bytes.Repeat([]byte("SECRET_PAYLOAD_ALPHA_"), 1024)
	plainB := bytes.Repeat([]byte("SECRET_PAYLOAD_BRAVO_"), 1024)
	sumA := sha256.Sum256(plainA)

	blobA, err := store.Save(ctx, bytes.NewReader(plainA), 1<<20)
	if err != nil {
		t.Fatalf("Save(A): %v", err)
	}
	if blobA.Size != int64(len(plainA)) || blobA.SHA256 != hex.EncodeToString(sumA[:]) {
		t.Fatalf("blobA metadata = %+v, want plaintext size=%d sha=%s", blobA, len(plainA), hex.EncodeToString(sumA[:]))
	}
	rawOnDiskA, err := os.ReadFile(filepath.Join(dataDir, "objects", blobA.ObjectKey))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawOnDiskA, []byte("SECRET_PAYLOAD_ALPHA_")) {
		t.Fatal("plaintext marker found inside encrypted object on disk")
	}

	rcA, err := store.Open(ctx, blobA.ObjectKey)
	if err != nil {
		t.Fatalf("Open(A): %v", err)
	}
	gotA, err := io.ReadAll(rcA)
	_ = rcA.Close()
	if err != nil || !bytes.Equal(gotA, plainA) {
		t.Fatalf("decrypted A mismatch: err=%v len=%d", err, len(gotA))
	}

	// 2. Object swap attack (copying A's ciphertext to B's path) must fail AAD verification.
	blobB, err := store.Save(ctx, bytes.NewReader(plainB), 1<<20)
	if err != nil {
		t.Fatalf("Save(B): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "objects", blobB.ObjectKey), rawOnDiskA, 0o600); err != nil {
		t.Fatal(err)
	}
	rcSwapped, err := store.Open(ctx, blobB.ObjectKey)
	if err == nil {
		_, err = io.ReadAll(rcSwapped)
		_ = rcSwapped.Close()
	}
	if err == nil {
		t.Fatal("expected AAD authentication failure when swapping object ciphertexts")
	}

	// 3. Ciphertext tampering must fail authentication on read.
	tampered := append([]byte(nil), rawOnDiskA...)
	tampered[len(tampered)-1] ^= 0xff
	if err := os.WriteFile(filepath.Join(dataDir, "objects", blobA.ObjectKey), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	rcTampered, err := store.Open(ctx, blobA.ObjectKey)
	if err == nil {
		_, err = io.ReadAll(rcTampered)
		_ = rcTampered.Close()
	}
	if err == nil {
		t.Fatal("expected authentication failure on tampered object ciphertext")
	}

	// 4. Staged chunks are encrypted on disk, decrypt cleanly, and reject chunk swaps.
	chunk0Plain := []byte("CHUNK_ZERO_SECRET_CONTENT_0000")
	chunk1Plain := []byte("CHUNK_ONE__SECRET_CONTENT_1111")
	c0, err := store.SaveChunk(ctx, "sess-1", 0, bytes.NewReader(chunk0Plain), int64(len(chunk0Plain)), "")
	if err != nil {
		t.Fatalf("SaveChunk(0): %v", err)
	}
	c1, err := store.SaveChunk(ctx, "sess-1", 1, bytes.NewReader(chunk1Plain), int64(len(chunk1Plain)), "")
	if err != nil {
		t.Fatalf("SaveChunk(1): %v", err)
	}
	rawChunk0, err := os.ReadFile(filepath.Join(dataDir, "uploads", "sess-1", c0.StorageKey))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawChunk0, []byte("CHUNK_ZERO_SECRET")) {
		t.Fatal("plaintext marker found inside encrypted staged chunk on disk")
	}
	rcC0, err := store.OpenChunk(ctx, "sess-1", c0)
	if err != nil {
		t.Fatalf("OpenChunk(0): %v", err)
	}
	gotC0, err := io.ReadAll(rcC0)
	_ = rcC0.Close()
	if err != nil || !bytes.Equal(gotC0, chunk0Plain) {
		t.Fatalf("decrypted chunk 0 mismatch: err=%v", err)
	}
	// Swap chunk0 ciphertext into chunk1's storage key -> must fail AAD check.
	if err := os.WriteFile(filepath.Join(dataDir, "uploads", "sess-1", c1.StorageKey), rawChunk0, 0o600); err != nil {
		t.Fatal(err)
	}
	rcC1Swapped, err := store.OpenChunk(ctx, "sess-1", c1)
	if err == nil {
		_, err = io.ReadAll(rcC1Swapped)
		_ = rcC1Swapped.Close()
	}
	if err == nil {
		t.Fatal("expected AAD authentication failure when swapping chunk ciphertexts")
	}
}

func TestStorageModeGuardsKeyPermissionsAndLegacyCompat(t *testing.T) {
	secretDir := t.TempDir()
	key1 := filepath.Join(secretDir, "key1.json")
	key2 := filepath.Join(secretDir, "key2.json")
	if err := localdisk.GenerateKeysetFile(key1); err != nil {
		t.Fatal(err)
	}
	if err := localdisk.GenerateKeysetFile(key2); err != nil {
		t.Fatal(err)
	}

	// 1. Encrypted directory rejects missing key and wrong key on reopen.
	encDir := filepath.Join(t.TempDir(), "enc-data")
	if _, err := localdisk.NewStoreWithConfig(encDir, key1); err != nil {
		t.Fatalf("init encrypted dir: %v", err)
	}
	if _, err := localdisk.NewStoreWithConfig(encDir, ""); err == nil {
		t.Fatal("expected error opening encrypted dir without key")
	}
	if _, err := localdisk.NewStoreWithConfig(encDir, key2); err == nil {
		t.Fatal("expected error opening encrypted dir with wrong key")
	}
	if _, err := localdisk.NewStoreWithConfig(encDir, key1); err != nil {
		t.Fatalf("reopen encrypted dir with matching key: %v", err)
	}

	// 2. Overly permissive key file (0644) is rejected.
	looseKey := filepath.Join(secretDir, "loose.json")
	if err := localdisk.GenerateKeysetFile(looseKey); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(looseKey, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := localdisk.NewStoreWithConfig(filepath.Join(t.TempDir(), "new-enc"), looseKey); err == nil {
		t.Fatal("expected error when key file has 0644 permissions")
	}

	// 3. Existing legacy plaintext directory (has filedock.db, no storage_mode.json) continues in plaintext,
	//    and refuses switching to encrypted mode.
	legacyDir := filepath.Join(t.TempDir(), "legacy-data")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "filedock.db"), []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := localdisk.NewStoreWithConfig(legacyDir, key1); err == nil {
		t.Fatal("expected error switching existing legacy plaintext dir to encrypted mode")
	}
	plainStore, err := localdisk.NewStoreWithConfig(legacyDir, "")
	if err != nil {
		t.Fatalf("open legacy plaintext dir: %v", err)
	}
	if plainStore.Mode() != localdisk.StorageModePlaintext {
		t.Fatalf("legacy Mode() = %q, want plaintext", plainStore.Mode())
	}
	// After storage_mode.json is written as plaintext, switching to encrypted still fails.
	if _, err := localdisk.NewStoreWithConfig(legacyDir, key1); err == nil {
		t.Fatal("expected error switching persisted plaintext dir to encrypted mode")
	}
}
