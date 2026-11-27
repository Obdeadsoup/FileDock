package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/httpapi"
	"example.com/filedock/internal/store/localdisk"
	"example.com/filedock/internal/store/sqlite"
)

type failCreateMetaStore struct {
	*sqlite.Store
}

func (failCreateMetaStore) Create(context.Context, files.StoredFile) error {
	return errors.New("injected sqlite create failure")
}

func TestEncryptedTamperedDownloadAndFailedTxCleanup(t *testing.T) {
	ctx := context.Background()
	keyPath := filepath.Join(t.TempDir(), "master.keyset.json")
	if err := localdisk.GenerateKeysetFile(keyPath); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	blobs, err := localdisk.NewStoreWithConfig(dataDir, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := sqlite.NewStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()

	var seq int
	nextID := func() (string, error) {
		seq++
		return fmt.Sprintf("enc-%d", seq), nil
	}
	acctSvc, err := accounts.NewService(meta, accounts.WithIDGenerator(nextID))
	if err != nil {
		t.Fatal(err)
	}
	fileSvc, err := files.NewService(blobs, meta, nextID)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.NewHandler(fileSvc, acctSvc))
	defer srv.Close()

	// Register & login alice.
	postJSON := func(path string, body any) *http.Response {
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := postJSON("/api/v1/auth/register", map[string]string{"username": "alice", "password": "password123"})
	resp.Body.Close()
	resp = postJSON("/api/v1/auth/login", map[string]string{"username": "alice", "password": "password123"})
	var loginOut struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&loginOut)
	resp.Body.Close()

	// Upload a 128 KiB file (multiple 4 KiB Tink segments) via chunked upload.
	payload := bytes.Repeat([]byte("ENCRYPTED_SEGMENT_DATA_"), 6000) // ~138 KiB
	createRaw, _ := json.Marshal(map[string]any{
		"name":   "large.bin",
		"size":   len(payload),
		"sha256": shaHex(payload),
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/uploads", bytes.NewReader(createRaw))
	req.Header.Set("Authorization", "Bearer "+loginOut.Token)
	req.Header.Set("Content-Type", "application/json")
	cResp, err := http.DefaultClient.Do(req)
	if err != nil || cResp.StatusCode != http.StatusCreated {
		t.Fatalf("create upload: %v", err)
	}
	var sess struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(cResp.Body).Decode(&sess)
	cResp.Body.Close()

	putReq, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/v1/uploads/%s/chunks/0", srv.URL, sess.ID), bytes.NewReader(payload))
	putReq.Header.Set("Authorization", "Bearer "+loginOut.Token)
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil || putResp.StatusCode != http.StatusOK {
		t.Fatalf("put chunk: %v", err)
	}
	putResp.Body.Close()

	compReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/uploads/%s/complete", srv.URL, sess.ID), nil)
	compReq.Header.Set("Authorization", "Bearer "+loginOut.Token)
	compResp, err := http.DefaultClient.Do(compReq)
	if err != nil || compResp.StatusCode != http.StatusOK {
		t.Fatalf("complete upload: %v", err)
	}
	var published struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(compResp.Body).Decode(&published)
	compResp.Body.Close()

	// Locate encrypted object on disk and flip a byte in the LAST segment (after 32 KiB header/first chunk).
	objEntries, err := os.ReadDir(filepath.Join(dataDir, "objects"))
	if err != nil || len(objEntries) != 1 {
		t.Fatalf("objects entries = %d err=%v", len(objEntries), err)
	}
	objPath := filepath.Join(dataDir, "objects", objEntries[0].Name())
	rawObj, err := os.ReadFile(objPath)
	if err != nil {
		t.Fatal(err)
	}
	rawObj[len(rawObj)-1] ^= 0xff
	if err := os.WriteFile(objPath, rawObj, 0o600); err != nil {
		t.Fatal(err)
	}

	// Client download MUST detect failure (either non-200 or read error / truncated body).
	dlReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/files/"+published.ID+"/content", nil)
	dlReq.Header.Set("Authorization", "Bearer "+loginOut.Token)
	dlResp, err := http.DefaultClient.Do(dlReq)
	if err == nil {
		body, readErr := io.ReadAll(dlResp.Body)
		dlResp.Body.Close()
		if dlResp.StatusCode == http.StatusOK && readErr == nil && bytes.Equal(body, payload) {
			t.Fatal("tampered encrypted download silently succeeded with intact payload")
		}
		if dlResp.StatusCode == http.StatusOK && readErr == nil {
			t.Fatalf("expected read error on tampered late segment, got clean EOF with len=%d", len(body))
		}
	}

	// Verify failed metadata transaction cleans up newly written encrypted object and temp files.
	failSvc, err := files.NewService(blobs, failCreateMetaStore{Store: meta}, nextID)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := meta.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failSvc.Upload(ctx, alice.ID, "fail.txt", bytes.NewReader([]byte("should-rollback"))); err == nil {
		t.Fatal("expected Upload to fail when metadata Create fails")
	}
	objAfter, _ := os.ReadDir(filepath.Join(dataDir, "objects"))
	tmpAfter, _ := os.ReadDir(filepath.Join(dataDir, "tmp"))
	if len(objAfter) != 1 || len(tmpAfter) != 0 {
		t.Fatalf("failed tx cleanup left orphans: objects=%d (want 1) tmp=%d (want 0)", len(objAfter), len(tmpAfter))
	}
}
