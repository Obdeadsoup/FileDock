package gui_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/gui"
	"example.com/filedock/internal/httpapi"
	"example.com/filedock/internal/store/localdisk"
	"example.com/filedock/internal/store/sqlite"
)

type testRemote struct {
	srv *httptest.Server
}

func startRemoteServer(t *testing.T) *testRemote {
	t.Helper()
	dataDir := t.TempDir()
	blobs, err := localdisk.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := sqlite.NewStore(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })

	var seq int
	var mu sync.Mutex
	idGen := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		seq++
		return fmt.Sprintf("gui-id-%d", seq), nil
	}
	acctSvc, err := accounts.NewService(meta, accounts.WithIDGenerator(idGen))
	if err != nil {
		t.Fatal(err)
	}
	fileSvc, err := files.NewService(blobs, meta, idGen, files.WithUploadChunkSize(1024))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.NewHandler(fileSvc, acctSvc))
	t.Cleanup(srv.Close)
	return &testRemote{srv: srv}
}

type guiClient struct {
	srv    *httptest.Server
	cookie *http.Cookie
	csrf   string
}

func startGUIServer(t *testing.T, remoteURL, localDir string) *guiClient {
	t.Helper()
	g, err := gui.NewServer(gui.Config{
		ServerURL: remoteURL,
		LocalDir:  localDir,
	})
	if err != nil {
		t.Fatalf("gui.NewServer: %v", err)
	}
	ts := httptest.NewServer(g.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "filedock_gui_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("missing filedock_gui_session cookie")
	}
	m := regexp.MustCompile(`name="gui-csrf"\s+content="([^"]+)"`).FindSubmatch(body)
	if len(m) != 2 {
		t.Fatal("missing gui-csrf meta token")
	}
	return &guiClient{srv: ts, cookie: cookie, csrf: string(m[1])}
}

func (c *guiClient) do(t *testing.T, method, path string, payload any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(c.cookie)
	req.Header.Set("X-FileDock-GUI-Token", c.csrf)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

func TestGUISecurityBoundariesAndDotFiledockProtection(t *testing.T) {
	remote := startRemoteServer(t)
	localDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(localDir, ".filedock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, ".filedock", "state.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	gc := startGUIServer(t, remote.srv.URL, localDir)

	// 1. Missing session cookie / CSRF -> 403.
	reqNoAuth, _ := http.NewRequest(http.MethodGet, gc.srv.URL+"/api/gui/local/list", nil)
	resp, err := http.DefaultClient.Do(reqNoAuth)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated local list = %d err=%v, want 403", resp.StatusCode, err)
	}
	resp.Body.Close()

	// 2. Cross-origin Origin header -> 403 even with valid cookie and CSRF.
	reqCross, _ := http.NewRequest(http.MethodGet, gc.srv.URL+"/api/gui/local/list", nil)
	reqCross.AddCookie(gc.cookie)
	reqCross.Header.Set("X-FileDock-GUI-Token", gc.csrf)
	reqCross.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(reqCross)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin local list = %d err=%v, want 403", resp.StatusCode, err)
	}
	resp.Body.Close()

	// 3. Valid local list hides .filedock.
	resp, raw := gc.do(t, http.MethodGet, "/api/gui/local/list", nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(raw), ".filedock") {
		t.Fatalf("local list = %d %s (must hide .filedock)", resp.StatusCode, string(raw))
	}

	// 4. Traversal and .filedock delete/access must be rejected with 403.
	for _, badPath := range []string{"../outside.txt", "/etc/passwd", ".filedock", ".filedock/state.json"} {
		r1, _ := gc.do(t, http.MethodPost, "/api/gui/local/delete", map[string]string{"path": badPath})
		if r1.StatusCode != http.StatusForbidden {
			t.Fatalf("delete %q = %d, want 403", badPath, r1.StatusCode)
		}
	}
}

func TestGUIEndToEndDualAccountConflictVersionAndResumableUpload(t *testing.T) {
	remote := startRemoteServer(t)
	localA := t.TempDir()
	localB := t.TempDir()
	if err := os.WriteFile(filepath.Join(localA, "doc.txt"), []byte("version-1-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localA, "doc-v2.txt"), []byte("version-2-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	bigPayload := bytes.Repeat([]byte("CHUNKED_GUI_DATA_"), 200) // 3400 bytes -> 4 chunks of 1024
	if err := os.WriteFile(filepath.Join(localA, "big.bin"), bigPayload, 0o644); err != nil {
		t.Fatal(err)
	}

	guiA := startGUIServer(t, remote.srv.URL, localA)
	guiB := startGUIServer(t, remote.srv.URL, localB)

	// Register & login Alice and Bob; login response must NOT expose Bearer token.
	for _, pair := range []struct {
		g    *guiClient
		user string
	}{{guiA, "alice"}, {guiB, "bob"}} {
		r, _ := pair.g.do(t, http.MethodPost, "/api/gui/auth/register", map[string]string{"username": pair.user, "password": "password123"})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("register %s = %d", pair.user, r.StatusCode)
		}
		r, raw := pair.g.do(t, http.MethodPost, "/api/gui/auth/login", map[string]string{"username": pair.user, "password": "password123"})
		if r.StatusCode != http.StatusOK || strings.Contains(string(raw), "token") {
			t.Fatalf("login %s = %d %s (must not leak token)", pair.user, r.StatusCode, string(raw))
		}
	}

	// 1. Alice uploads doc.txt, fetches ETag, updates content once, then retries stale ETag -> 412 Version Conflict!
	r, raw := guiA.do(t, http.MethodPost, "/api/gui/transfer/upload", map[string]any{"local_path": "doc.txt"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("upload doc.txt = %d %s", r.StatusCode, string(raw))
	}
	var docFile struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
	}
	_ = json.Unmarshal(raw, &docFile)

	_, headRaw := guiA.do(t, http.MethodGet, "/api/gui/remote/files/"+docFile.ID+"/head", nil)
	var head struct {
		ETag string `json:"etag"`
	}
	_ = json.Unmarshal(headRaw, &head)
	if head.ETag == "" {
		t.Fatal("expected non-empty ETag from head")
	}

	// First replace succeeds.
	r, _ = guiA.do(t, http.MethodPost, "/api/gui/transfer/replace", map[string]string{
		"local_path": "doc-v2.txt", "file_id": docFile.ID, "if_match": head.ETag,
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("replace with fresh ETag = %d, want 200", r.StatusCode)
	}
	// Second replace with STALE ETag returns 412 Precondition Failed!
	r, _ = guiA.do(t, http.MethodPost, "/api/gui/transfer/replace", map[string]string{
		"local_path": "doc.txt", "file_id": docFile.ID, "if_match": head.ETag,
	})
	if r.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("replace with stale ETag = %d, want 412", r.StatusCode)
	}

	// 2. Same-name download conflict: downloading doc.txt into localA (where doc.txt already exists) returns 409 conflict,
	//    then conflict_policy="rename" saves as "doc (1).txt" with verified SHA-256!
	v2Sum := sha256.Sum256([]byte("version-2-content"))
	v2SHA := hex.EncodeToString(v2Sum[:])
	r, raw = guiA.do(t, http.MethodPost, "/api/gui/transfer/download", map[string]string{
		"file_id": docFile.ID, "file_name": "doc.txt", "expected_sha256": v2SHA, "local_dir": "", "conflict_policy": "ask",
	})
	if r.StatusCode != http.StatusConflict || !strings.Contains(string(raw), `"conflict":true`) {
		t.Fatalf("same-name download = %d %s, want 409 conflict", r.StatusCode, string(raw))
	}
	r, raw = guiA.do(t, http.MethodPost, "/api/gui/transfer/download", map[string]string{
		"file_id": docFile.ID, "file_name": "doc.txt", "expected_sha256": v2SHA, "local_dir": "", "conflict_policy": "rename",
	})
	if r.StatusCode != http.StatusOK || !strings.Contains(string(raw), "doc (1).txt") {
		t.Fatalf("rename-policy download = %d %s", r.StatusCode, string(raw))
	}

	// 3. Cross-account access: Bob cannot download Alice's private file directly (404),
	//    but after Alice shares it, Bob downloads via shared endpoint with matching SHA-256!
	r, _ = guiB.do(t, http.MethodPost, "/api/gui/transfer/download", map[string]string{
		"file_id": docFile.ID, "file_name": "steal.txt", "local_dir": "", "conflict_policy": "ask",
	})
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("bob private download of alice file = %d, want 404", r.StatusCode)
	}
	r, shareRaw := guiA.do(t, http.MethodPost, "/api/gui/remote/shares", map[string]string{
		"resource_type": "file", "resource_id": docFile.ID, "recipient_username": "bob",
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create share = %d %s", r.StatusCode, string(shareRaw))
	}
	var sh struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(shareRaw, &sh)

	r, dlSharedRaw := guiB.do(t, http.MethodPost, "/api/gui/transfer/download-shared", map[string]string{
		"share_id": sh.ID, "file_id": docFile.ID, "file_name": "shared-doc.txt", "expected_sha256": v2SHA, "local_dir": "", "conflict_policy": "ask",
	})
	if r.StatusCode != http.StatusOK || !strings.Contains(string(dlSharedRaw), v2SHA) {
		t.Fatalf("bob shared download = %d %s", r.StatusCode, string(dlSharedRaw))
	}

	// 4. Resumable upload interrupt after 2 chunks + resume by session_id uploads ONLY remaining 2 chunks!
	r, job1Raw := guiA.do(t, http.MethodPost, "/api/gui/transfer/upload-resumable", map[string]any{
		"local_path": "big.bin", "stop_after_chunks": 2,
	})
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("start resumable upload = %d %s", r.StatusCode, string(job1Raw))
	}
	var job1 gui.UploadJobSnapshot
	_ = json.Unmarshal(job1Raw, &job1)

	// Poll until paused/canceled after 2 chunks.
	for i := 0; i < 50; i++ {
		_, jRaw := guiA.do(t, http.MethodGet, "/api/gui/transfer/jobs/"+job1.JobID, nil)
		_ = json.Unmarshal(jRaw, &job1)
		if job1.Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job1.Status != "canceled" || job1.ChunksSent != 2 || job1.TotalChunks != 4 {
		t.Fatalf("paused job1 = %+v, want canceled with 2/4 chunks sent", job1)
	}

	// Resume by SessionID -> should skip 2 chunks and send only remaining 2 chunks!
	r, job2Raw := guiA.do(t, http.MethodPost, "/api/gui/transfer/upload-resumable", map[string]any{
		"local_path": "big.bin", "session_id": job1.SessionID,
	})
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("resume upload = %d %s", r.StatusCode, string(job2Raw))
	}
	var job2 gui.UploadJobSnapshot
	_ = json.Unmarshal(job2Raw, &job2)
	for i := 0; i < 50; i++ {
		_, jRaw := guiA.do(t, http.MethodGet, "/api/gui/transfer/jobs/"+job2.JobID, nil)
		_ = json.Unmarshal(jRaw, &job2)
		if job2.Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	wantBigSum := sha256.Sum256(bigPayload)
	if job2.Status != "completed" || job2.ChunksSkipped != 2 || job2.ChunksSent != 2 || job2.SHA256 != hex.EncodeToString(wantBigSum[:]) {
		t.Fatalf("resumed job2 = %+v, want completed skipped=2 sent=2", job2)
	}
}
