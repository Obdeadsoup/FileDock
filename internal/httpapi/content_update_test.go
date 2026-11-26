package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"example.com/filedock/internal/files"
)

type failingBodyReader struct {
	prefix []byte
	sent   bool
}

func (f *failingBodyReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		n := copy(p, f.prefix)
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}
func (f *failingBodyReader) Close() error { return nil }

func TestConcurrentContentUpdateAndPreconditions(t *testing.T) {
	dataDir := t.TempDir()
	env := newTestEnv(t, dataDir, files.WithMaxFileSize(64))
	defer env.cleanup()
	h := env.handler

	tokenA := registerAndLogin(t, h, "alice", "alice-pass")
	tokenB := registerAndLogin(t, h, "bob", "bob-pass")

	// 1. Upload initial file v1.
	v1Payload := []byte("initial version 1")
	ct, body := buildMultipartUpload(t, "file", "doc.txt", v1Payload)
	upReq := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", body), tokenA)
	upReq.Header.Set("Content-Type", ct)
	upResp := httptest.NewRecorder()
	h.ServeHTTP(upResp, upReq)
	if upResp.Code != http.StatusCreated {
		t.Fatalf("upload v1 status = %d, body = %s", upResp.Code, upResp.Body.String())
	}
	var v1Meta fileResponse
	if err := json.Unmarshal(upResp.Body.Bytes(), &v1Meta); err != nil {
		t.Fatal(err)
	}
	if v1Meta.ContentRevision != 1 {
		t.Fatalf("initial content_revision = %d, want 1", v1Meta.ContentRevision)
	}

	// 2. GET and HEAD return the same strong ETag.
	headResp := httptest.NewRecorder()
	h.ServeHTTP(headResp, setBearer(httptest.NewRequest(http.MethodHead, "/api/v1/files/"+v1Meta.ID+"/content", nil), tokenA))
	if headResp.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", headResp.Code)
	}
	etag1 := headResp.Header().Get("ETag")
	if !strings.HasPrefix(etag1, `"`) || !strings.HasSuffix(etag1, `"`) || strings.HasPrefix(etag1, `W/`) {
		t.Fatalf("HEAD ETag = %q, want strong quoted ETag", etag1)
	}

	getResp := httptest.NewRecorder()
	h.ServeHTTP(getResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+v1Meta.ID+"/content", nil), tokenA))
	if getResp.Code != http.StatusOK || getResp.Header().Get("ETag") != etag1 {
		t.Fatalf("GET status=%d ETag=%q, want (200, %q)", getResp.Code, getResp.Header().Get("ETag"), etag1)
	}

	// 3. Rename/move does NOT increment content_revision or change ETag.
	patchReq := setBearer(httptest.NewRequest(http.MethodPatch, "/api/v1/files/"+v1Meta.ID, strings.NewReader(`{"name":"doc-renamed.txt"}`)), tokenA)
	patchReq.Header.Set("Content-Type", "application/json")
	patchResp := httptest.NewRecorder()
	h.ServeHTTP(patchResp, patchReq)
	if patchResp.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d", patchResp.Code)
	}
	var patched fileResponse
	if err := json.Unmarshal(patchResp.Body.Bytes(), &patched); err != nil {
		t.Fatal(err)
	}
	if patched.ContentRevision != 1 {
		t.Fatalf("content_revision after rename = %d, want 1", patched.ContentRevision)
	}

	// 4. Error cases: 401 unauthenticated, 428 missing If-Match, 400 weak/* If-Match, 404 cross-account & unknown, 413 oversize, 400 read interrupt.
	putReq := func(token, fileID, ifMatch string, r io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/files/"+fileID+"/content", r)
		if token != "" {
			setBearer(req, token)
		}
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		return resp
	}

	if r := putReq("", v1Meta.ID, etag1, strings.NewReader("x")); r.Code != http.StatusUnauthorized {
		t.Errorf("unauth PUT status = %d, want 401", r.Code)
	}
	if r := putReq(tokenA, v1Meta.ID, "", strings.NewReader("x")); r.Code != http.StatusPreconditionRequired {
		t.Errorf("missing If-Match PUT status = %d, want 428", r.Code)
	}
	for _, badETag := range []string{"*", `W/"weak"`, `unquoted`} {
		if r := putReq(tokenA, v1Meta.ID, badETag, strings.NewReader("x")); r.Code != http.StatusBadRequest {
			t.Errorf("bad If-Match %q status = %d, want 400", badETag, r.Code)
		}
	}
	if r := putReq(tokenB, v1Meta.ID, etag1, strings.NewReader("x")); r.Code != http.StatusNotFound {
		t.Errorf("cross-account PUT status = %d, want 404", r.Code)
	}
	if r := putReq(tokenA, "unknown-id", etag1, strings.NewReader("x")); r.Code != http.StatusNotFound {
		t.Errorf("unknown-id PUT status = %d, want 404", r.Code)
	}
	if r := putReq(tokenA, v1Meta.ID, etag1, bytes.NewReader(bytes.Repeat([]byte("z"), 65))); r.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize PUT status = %d, want 413", r.Code)
	}
	if r := putReq(tokenA, v1Meta.ID, etag1, &failingBodyReader{prefix: []byte("part")}); r.Code != http.StatusBadRequest {
		t.Errorf("interrupted PUT status = %d, want 400", r.Code)
	}

	// Record sync cursor right before concurrent updates.
	syncBeforeResp := httptest.NewRecorder()
	h.ServeHTTP(syncBeforeResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/sync/changes?after=0&limit=50", nil), tokenA))
	var syncBefore syncChangesPageResponse
	if err := json.Unmarshal(syncBeforeResp.Body.Bytes(), &syncBefore); err != nil {
		t.Fatal(err)
	}

	// 5. Two devices concurrently PUT different payloads using the same ETag -> exactly 1 succeeds (200), 1 fails (412).
	payloadDev1 := []byte("device 1 winning or losing payload")
	payloadDev2 := []byte("device 2 competing payload bytes!!")

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results [2]*httptest.ResponseRecorder
	)
	payloads := [][]byte{payloadDev1, payloadDev2}
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = putReq(tokenA, v1Meta.ID, etag1, bytes.NewReader(payloads[idx]))
		}(i)
	}
	close(start)
	wg.Wait()

	var (
		okCount       int
		conflictCount int
		winnerIdx     = -1
		winnerResp    fileResponse
		winnerETag    string
	)
	for i, r := range results {
		switch r.Code {
		case http.StatusOK:
			okCount++
			winnerIdx = i
			winnerETag = r.Header().Get("ETag")
			if err := json.Unmarshal(r.Body.Bytes(), &winnerResp); err != nil {
				t.Fatal(err)
			}
		case http.StatusPreconditionFailed:
			conflictCount++
		default:
			t.Fatalf("device %d unexpected status %d: %s", i, r.Code, r.Body.String())
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("concurrent results: ok=%d, 412=%d; want 1 and 1", okCount, conflictCount)
	}
	if winnerResp.ID != v1Meta.ID || winnerResp.Name != "doc-renamed.txt" || winnerResp.ContentRevision != 2 || winnerETag == etag1 {
		t.Fatalf("unexpected winner metadata=%+v etag=%q", winnerResp, winnerETag)
	}

	// 6. Download by original ID returns winning payload and matching SHA-256.
	dlWin := httptest.NewRecorder()
	h.ServeHTTP(dlWin, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+v1Meta.ID+"/content", nil), tokenA))
	if dlWin.Code != http.StatusOK || !bytes.Equal(dlWin.Body.Bytes(), payloads[winnerIdx]) {
		t.Fatalf("download winner = (%d, %q), want %q", dlWin.Code, dlWin.Body.Bytes(), payloads[winnerIdx])
	}
	winSum := sha256.Sum256(payloads[winnerIdx])
	if hex.EncodeToString(winSum[:]) != winnerResp.SHA256 {
		t.Fatalf("winner sha256 mismatch")
	}

	// 7. Sync change stream has exactly 1 new file.upsert event with content_revision=2.
	syncAfterResp := httptest.NewRecorder()
	h.ServeHTTP(syncAfterResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/sync/changes?after=0&limit=50", nil), tokenA))
	var syncAfter syncChangesPageResponse
	if err := json.Unmarshal(syncAfterResp.Body.Bytes(), &syncAfter); err != nil {
		t.Fatal(err)
	}
	if len(syncAfter.Items) != len(syncBefore.Items)+1 {
		t.Fatalf("sync events count = %d, want %d", len(syncAfter.Items), len(syncBefore.Items)+1)
	}
	lastEv := syncAfter.Items[len(syncAfter.Items)-1]
	if lastEv.Type != files.ChangeFileUpsert || lastEv.ResourceID != v1Meta.ID ||
		lastEv.File == nil || lastEv.File.ContentRevision != 2 || lastEv.File.SHA256 != winnerResp.SHA256 {
		t.Fatalf("unexpected sync event after PUT: %+v", lastEv)
	}

	// 8. Background cleanup removes the replaced old disk object, leaving exactly 1 object on disk.
	acked, err := env.fileSvc.ProcessBlobCleanups(context.Background(), 10)
	if err != nil || acked != 1 {
		t.Fatalf("ProcessBlobCleanups() = (%d, %v), want (1, nil)", acked, err)
	}
	objs, err := os.ReadDir(filepath.Join(dataDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("objects on disk = %d, want 1 (winner only)", len(objs))
	}
	_ = errors.New
}
