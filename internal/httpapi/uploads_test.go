package httpapi_test

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
	"sync"
	"testing"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/httpapi"
	"example.com/filedock/internal/store/localdisk"
	"example.com/filedock/internal/store/sqlite"
)

type uploadEnv struct {
	dataDir string
	meta    *sqlite.Store
	blobs   *localdisk.Store
	svc     *files.Service
	handler http.Handler
	tokenA  string
	tokenB  string
}

func newUploadEnv(t *testing.T, opts ...files.Option) *uploadEnv {
	t.Helper()
	dataDir := t.TempDir()
	return openUploadEnvAt(t, dataDir, opts...)
}

func openUploadEnvAt(t *testing.T, dataDir string, opts ...files.Option) *uploadEnv {
	t.Helper()
	ctx := context.Background()
	blobs, err := localdisk.NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore(localdisk): %v", err)
	}
	meta, err := sqlite.NewStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("NewStore(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })

	var seq int
	var seqMu sync.Mutex
	nextID := func() (string, error) {
		seqMu.Lock()
		defer seqMu.Unlock()
		seq++
		return fmt.Sprintf("id-%d-%d", time.Now().UnixNano(), seq), nil
	}

	acctSvc, err := accounts.NewService(meta, accounts.WithIDGenerator(nextID))
	if err != nil {
		t.Fatalf("accounts.NewService: %v", err)
	}
	fileSvc, err := files.NewService(blobs, meta, nextID, opts...)
	if err != nil {
		t.Fatalf("files.NewService: %v", err)
	}
	h := httpapi.NewHandler(fileSvc, acctSvc)

	env := &uploadEnv{
		dataDir: dataDir,
		meta:    meta,
		blobs:   blobs,
		svc:     fileSvc,
		handler: h,
	}
	// Register/login alice and bob (ignore already-registered on reopen).
	for _, pair := range []struct {
		user string
		dst  *string
	}{
		{"alice", &env.tokenA},
		{"bob", &env.tokenB},
	} {
		_ = env.doJSON("POST", "/api/v1/auth/register", "", map[string]string{
			"username": pair.user, "password": "password123",
		})
		rec := env.doJSON("POST", "/api/v1/auth/login", "", map[string]string{
			"username": pair.user, "password": "password123",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("login %s failed: %d %s", pair.user, rec.Code, rec.Body.String())
		}
		var out struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		*pair.dst = out.Token
	}
	return env
}

func (e *uploadEnv) doJSON(method, target, token string, body any) *httptest.ResponseRecorder {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *uploadEnv) putChunk(token, sessionID string, index int, data []byte, shaHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/uploads/%s/chunks/%d", sessionID, index), bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	if shaHeader != "" {
		req.Header.Set("X-Chunk-SHA256", shaHeader)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestUploadSessionDuplicateMissingBadHashCrossAccountAndConcurrentComplete(t *testing.T) {
	env := newUploadEnv(t, files.WithUploadChunkSize(10))

	payload := []byte("0123456789abcdefghijXYZ") // 23 bytes -> 3 chunks: 10, 10, 3
	totalSHA := shaHex(payload)

	// 1. Create upload session.
	createRec := env.doJSON("POST", "/api/v1/uploads", env.tokenA, map[string]any{
		"name":   "data.bin",
		"size":   len(payload),
		"sha256": totalSHA,
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create upload = %d %s", createRec.Code, createRec.Body.String())
	}
	var sess struct {
		ID             string `json:"id"`
		ChunkSize      int64  `json:"chunk_size"`
		TotalChunks    int    `json:"total_chunks"`
		UploadedChunks []int  `json:"uploaded_chunks"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	if sess.ChunkSize != 10 || sess.TotalChunks != 3 || len(sess.UploadedChunks) != 0 {
		t.Fatalf("unexpected session: %+v", sess)
	}

	// 2. Cross-account access must return 404.
	if rec := env.doJSON("GET", "/api/v1/uploads/"+sess.ID, env.tokenB, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bob GET alice session = %d, want 404", rec.Code)
	}
	if rec := env.putChunk(env.tokenB, sess.ID, 0, payload[:10], shaHex(payload[:10])); rec.Code != http.StatusNotFound {
		t.Fatalf("bob PUT alice chunk = %d, want 404", rec.Code)
	}
	if rec := env.doJSON("POST", "/api/v1/uploads/"+sess.ID+"/complete", env.tokenB, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bob complete alice session = %d, want 404", rec.Code)
	}

	// 3. Bad chunk hash rejected with 400.
	if rec := env.putChunk(env.tokenA, sess.ID, 0, payload[:10], shaHex([]byte("wrong"))); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad chunk hash = %d, want 400", rec.Code)
	}

	// 4. Upload chunk 0, then duplicate same content -> 200, then different content -> 409.
	if rec := env.putChunk(env.tokenA, sess.ID, 0, payload[:10], shaHex(payload[:10])); rec.Code != http.StatusOK {
		t.Fatalf("chunk 0 = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.putChunk(env.tokenA, sess.ID, 0, payload[:10], shaHex(payload[:10])); rec.Code != http.StatusOK {
		t.Fatalf("duplicate identical chunk 0 = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := env.putChunk(env.tokenA, sess.ID, 0, []byte("ZZZZZZZZZZ"), ""); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate conflicting chunk 0 = %d %s, want 409", rec.Code, rec.Body.String())
	}

	// 5. Complete with missing chunks -> 409; file must not appear in list or sync changes.
	if rec := env.doJSON("POST", "/api/v1/uploads/"+sess.ID+"/complete", env.tokenA, nil); rec.Code != http.StatusConflict {
		t.Fatalf("complete with missing chunks = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if rec := env.doJSON("GET", "/api/v1/files", env.tokenA, nil); rec.Body.String() != "[]\n" && rec.Body.String() != "[]" {
		t.Fatalf("files list after incomplete complete = %s, want empty", rec.Body.String())
	}

	// 6. Upload remaining chunks 1 and 2.
	if rec := env.putChunk(env.tokenA, sess.ID, 1, payload[10:20], shaHex(payload[10:20])); rec.Code != http.StatusOK {
		t.Fatalf("chunk 1 = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.putChunk(env.tokenA, sess.ID, 2, payload[20:], shaHex(payload[20:])); rec.Code != http.StatusOK {
		t.Fatalf("chunk 2 = %d %s", rec.Code, rec.Body.String())
	}

	// 7. Concurrent complete calls must return the EXACT same file ID and publish only once.
	const concurrency = 4
	results := make([]string, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := env.doJSON("POST", "/api/v1/uploads/"+sess.ID+"/complete", env.tokenA, nil)
			if rec.Code != http.StatusOK {
				t.Errorf("complete[%d] = %d %s", idx, rec.Code, rec.Body.String())
				return
			}
			var f struct {
				ID     string `json:"id"`
				SHA256 string `json:"sha256"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
				t.Errorf("decode complete[%d]: %v", idx, err)
				return
			}
			if f.SHA256 != totalSHA {
				t.Errorf("complete[%d] sha256 = %q, want %q", idx, f.SHA256, totalSHA)
			}
			results[idx] = f.ID
		}(i)
	}
	wg.Wait()

	for i := 1; i < concurrency; i++ {
		if results[i] == "" || results[i] != results[0] {
			t.Fatalf("concurrent complete file IDs mismatch: %v", results)
		}
	}

	// Retry complete again -> same file ID.
	retryRec := env.doJSON("POST", "/api/v1/uploads/"+sess.ID+"/complete", env.tokenA, nil)
	if retryRec.Code != http.StatusOK {
		t.Fatalf("retry complete = %d %s", retryRec.Code, retryRec.Body.String())
	}
	var retryFile struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(retryRec.Body.Bytes(), &retryFile)
	if retryFile.ID != results[0] {
		t.Fatalf("retry complete ID = %q, want %q", retryFile.ID, results[0])
	}

	// Verify only ONE file in list and ONE file.upsert event in sync changes.
	listRec := env.doJSON("GET", "/api/v1/files", env.tokenA, nil)
	var listed []map[string]any
	_ = json.Unmarshal(listRec.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Fatalf("listed files count = %d, want 1", len(listed))
	}
	syncRec := env.doJSON("GET", "/api/v1/sync/changes?after=0&limit=50", env.tokenA, nil)
	var syncOut struct {
		Items []struct {
			Type       string `json:"type"`
			ResourceID string `json:"resource_id"`
		} `json:"items"`
	}
	_ = json.Unmarshal(syncRec.Body.Bytes(), &syncOut)
	if len(syncOut.Items) != 1 || syncOut.Items[0].Type != "file.upsert" || syncOut.Items[0].ResourceID != results[0] {
		t.Fatalf("sync events = %+v, want single file.upsert for %s", syncOut.Items, results[0])
	}

	// Verify staged chunk directory was cleaned up after completion.
	if _, err := os.Stat(filepath.Join(env.dataDir, "uploads", sess.ID)); !os.IsNotExist(err) {
		t.Fatalf("expected uploads/%s dir to be removed after complete, err=%v", sess.ID, err)
	}
}

func TestUploadSessionWholeFileBadHashDeletedParentAndExpirationCleanup(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	env := newUploadEnv(t,
		files.WithUploadChunkSize(8),
		files.WithUploadSessionTTL(10*time.Minute),
		files.WithClock(clock),
	)

	// 1. Bad whole-file SHA-256 fails on complete and never publishes.
	data := []byte("hello world 1234") // 16 bytes
	createBad := env.doJSON("POST", "/api/v1/uploads", env.tokenA, map[string]any{
		"name":   "badhash.bin",
		"size":   len(data),
		"sha256": shaHex([]byte("different-content")),
	})
	if createBad.Code != http.StatusCreated {
		t.Fatalf("createBad = %d %s", createBad.Code, createBad.Body.String())
	}
	var badSess struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createBad.Body.Bytes(), &badSess)
	if rec := env.putChunk(env.tokenA, badSess.ID, 0, data[:8], ""); rec.Code != http.StatusOK {
		t.Fatalf("chunk 0 = %d", rec.Code)
	}
	if rec := env.putChunk(env.tokenA, badSess.ID, 1, data[8:], ""); rec.Code != http.StatusOK {
		t.Fatalf("chunk 1 = %d", rec.Code)
	}
	if rec := env.doJSON("POST", "/api/v1/uploads/"+badSess.ID+"/complete", env.tokenA, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("complete with bad whole-file hash = %d %s, want 400", rec.Code, rec.Body.String())
	}

	// 2. Deleted target folder before complete returns 404 and does not publish.
	folderRec := env.doJSON("POST", "/api/v1/folders", env.tokenA, map[string]string{"name": "tempdir"})
	if folderRec.Code != http.StatusCreated {
		t.Fatalf("create folder = %d", folderRec.Code)
	}
	var folder struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(folderRec.Body.Bytes(), &folder)

	createInFolder := env.doJSON("POST", "/api/v1/uploads", env.tokenA, map[string]any{
		"parent_id": folder.ID,
		"name":      "orphan.bin",
		"size":      8,
		"sha256":    shaHex(data[:8]),
	})
	var folderSess struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createInFolder.Body.Bytes(), &folderSess)
	if rec := env.putChunk(env.tokenA, folderSess.ID, 0, data[:8], ""); rec.Code != http.StatusOK {
		t.Fatalf("chunk 0 = %d", rec.Code)
	}
	if rec := env.doJSON("DELETE", "/api/v1/folders/"+folder.ID, env.tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete folder = %d", rec.Code)
	}
	if rec := env.doJSON("POST", "/api/v1/uploads/"+folderSess.ID+"/complete", env.tokenA, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("complete after parent deleted = %d %s, want 404", rec.Code, rec.Body.String())
	}

	// Confirm zero files published.
	listRec := env.doJSON("GET", "/api/v1/files", env.tokenA, nil)
	var listed []map[string]any
	_ = json.Unmarshal(listRec.Body.Bytes(), &listed)
	if len(listed) != 0 {
		t.Fatalf("listed files = %d, want 0", len(listed))
	}

	// 3. Advance clock past TTL: expired session returns 410, and cleanup sweeps chunks + DB rows.
	now = now.Add(15 * time.Minute)
	if rec := env.doJSON("GET", "/api/v1/uploads/"+badSess.ID, env.tokenA, nil); rec.Code != http.StatusGone {
		t.Fatalf("GET expired session = %d, want 410", rec.Code)
	}
	cleaned, err := env.svc.ProcessExpiredUploadSessions(context.Background(), 32)
	if err != nil || cleaned != 2 {
		t.Fatalf("ProcessExpiredUploadSessions = (%d, %v), want (2, nil)", cleaned, err)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "uploads", badSess.ID)); !os.IsNotExist(err) {
		t.Fatalf("expired chunks not cleaned up: %v", err)
	}
}
