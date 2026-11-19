package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"example.com/filedock/internal/files"
	"example.com/filedock/internal/store/localdisk"
	"example.com/filedock/internal/store/sqlite"
)

func TestHealth(t *testing.T) {
	handler := NewHandler(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := response.Body.String(); got != `{"status":"ok"}` {
		t.Errorf("body = %q, want %s", got, `{"status":"ok"}`)
	}
}

func TestHealthRejectsOtherMethods(t *testing.T) {
	handler := NewHandler(nil)
	for _, method := range []string{http.MethodPost, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(method, "/healthz", nil))

			if response.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
			}
			if got := response.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want %s", got, http.MethodGet)
			}
		})
	}
}

func newTestServerHandler(t *testing.T, dataDir string, opts ...files.Option) (http.Handler, func()) {
	t.Helper()
	blobs, err := localdisk.NewStore(dataDir)
	if err != nil {
		t.Fatalf("localdisk.NewStore() error = %v", err)
	}
	meta, err := sqlite.NewStore(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("sqlite.NewStore() error = %v", err)
	}

	var seq atomic.Int64
	idGen := func() (string, error) {
		n := seq.Add(1)
		return fmt.Sprintf("file-%d-%d", n, os.Getpid()), nil
	}

	svc, err := files.NewService(blobs, meta, idGen, opts...)
	if err != nil {
		_ = meta.Close()
		t.Fatalf("files.NewService() error = %v", err)
	}
	return NewHandler(svc), func() { _ = meta.Close() }
}

func buildMultipartUpload(t *testing.T, fieldName, filename string, content []byte) (string, *bytes.Buffer) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(fieldName, filename)
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("part.Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
	return writer.FormDataContentType(), &body
}

func TestUploadListDownloadAndRestart(t *testing.T) {
	dataDir := t.TempDir()
	handler, cleanup := newTestServerHandler(t, dataDir)

	// 1. Empty list returns literal JSON empty array.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/files", nil)
	listResp := httptest.NewRecorder()
	handler.ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("empty list status = %d, want 200", listResp.Code)
	}
	if got := strings.TrimSpace(listResp.Body.String()); got != "[]" {
		t.Fatalf("empty list body = %q, want []", got)
	}

	// 2. Upload first file.
	payload1 := []byte("hello filedock round 2!")
	sum1 := sha256.Sum256(payload1)
	wantHash1 := hex.EncodeToString(sum1[:])

	ct1, body1 := buildMultipartUpload(t, "file", "hello 世界.txt", payload1)
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/files", body1)
	req1.Header.Set("Content-Type", ct1)
	resp1 := httptest.NewRecorder()
	handler.ServeHTTP(resp1, req1)
	if resp1.Code != http.StatusCreated {
		t.Fatalf("upload 1 status = %d, body = %s", resp1.Code, resp1.Body.String())
	}

	var meta1 fileResponse
	if err := json.Unmarshal(resp1.Body.Bytes(), &meta1); err != nil {
		t.Fatalf("decode upload 1 response: %v", err)
	}
	if meta1.ID == "" || meta1.Name != "hello 世界.txt" || meta1.Size != int64(len(payload1)) || meta1.SHA256 != wantHash1 || meta1.CreatedAt.IsZero() {
		t.Fatalf("unexpected upload 1 metadata: %+v", meta1)
	}

	// 3. Same-name upload creates a distinct record.
	payload2 := []byte("second version with same name")
	ct2, body2 := buildMultipartUpload(t, "file", "hello 世界.txt", payload2)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/files", body2)
	req2.Header.Set("Content-Type", ct2)
	resp2 := httptest.NewRecorder()
	handler.ServeHTTP(resp2, req2)
	if resp2.Code != http.StatusCreated {
		t.Fatalf("upload 2 status = %d, body = %s", resp2.Code, resp2.Body.String())
	}
	var meta2 fileResponse
	if err := json.Unmarshal(resp2.Body.Bytes(), &meta2); err != nil {
		t.Fatalf("decode upload 2 response: %v", err)
	}
	if meta2.ID == meta1.ID {
		t.Fatalf("duplicate name upload reused ID %q", meta1.ID)
	}

	// 4. List returns both items.
	listResp2 := httptest.NewRecorder()
	handler.ServeHTTP(listResp2, httptest.NewRequest(http.MethodGet, "/api/v1/files", nil))
	if listResp2.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp2.Code)
	}
	var listed []fileResponse
	if err := json.Unmarshal(listResp2.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len(listed) = %d, want 2", len(listed))
	}

	// 5. Download by ID returns attachment and matching bytes + SHA-256.
	dlResp := httptest.NewRecorder()
	handler.ServeHTTP(dlResp, httptest.NewRequest(http.MethodGet, "/api/v1/files/"+meta1.ID+"/content", nil))
	if dlResp.Code != http.StatusOK {
		t.Fatalf("download status = %d, body = %s", dlResp.Code, dlResp.Body.String())
	}
	if disp := dlResp.Header().Get("Content-Disposition"); !strings.HasPrefix(disp, "attachment;") {
		t.Fatalf("Content-Disposition = %q, want attachment", disp)
	}
	gotSum := sha256.Sum256(dlResp.Body.Bytes())
	if hex.EncodeToString(gotSum[:]) != wantHash1 {
		t.Fatalf("downloaded content sha256 mismatch")
	}

	// 6. Simulate restart using the same dataDir.
	cleanup()
	handlerAfterRestart, cleanup2 := newTestServerHandler(t, dataDir)
	defer cleanup2()

	dlRestart := httptest.NewRecorder()
	handlerAfterRestart.ServeHTTP(dlRestart, httptest.NewRequest(http.MethodGet, "/api/v1/files/"+meta1.ID+"/content", nil))
	if dlRestart.Code != http.StatusOK {
		t.Fatalf("download after restart status = %d, body = %s", dlRestart.Code, dlRestart.Body.String())
	}
	gotSumRestart := sha256.Sum256(dlRestart.Body.Bytes())
	if hex.EncodeToString(gotSumRestart[:]) != wantHash1 {
		t.Fatalf("downloaded content sha256 mismatch after restart")
	}
}

func TestErrorCasesAndFailedUploadsExcludedFromList(t *testing.T) {
	dataDir := t.TempDir()
	handler, cleanup := newTestServerHandler(t, dataDir, files.WithMaxFileSize(16))
	defer cleanup()

	// Unknown ID -> 404.
	notFoundResp := httptest.NewRecorder()
	handler.ServeHTTP(notFoundResp, httptest.NewRequest(http.MethodGet, "/api/v1/files/does-not-exist/content", nil))
	if notFoundResp.Code != http.StatusNotFound {
		t.Errorf("unknown ID status = %d, want 404", notFoundResp.Code)
	}

	// Invalid ID -> 400.
	badIDResp := httptest.NewRecorder()
	handler.ServeHTTP(badIDResp, httptest.NewRequest(http.MethodGet, "/api/v1/files/%20/content", nil))
	if badIDResp.Code != http.StatusBadRequest {
		t.Errorf("invalid ID status = %d, want 400", badIDResp.Code)
	}

	// Invalid pagination -> 400.
	for _, query := range []string{"?limit=0", "?limit=-5", "?limit=abc", "?offset=-1", "?offset=xyz"} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/v1/files"+query, nil))
		if resp.Code != http.StatusBadRequest {
			t.Errorf("GET /api/v1/files%s status = %d, want 400", query, resp.Code)
		}
	}

	// Non-multipart POST -> 400.
	nonMultipart := httptest.NewRequest(http.MethodPost, "/api/v1/files", strings.NewReader("plain text"))
	nonMultipart.Header.Set("Content-Type", "application/json")
	nonMPResp := httptest.NewRecorder()
	handler.ServeHTTP(nonMPResp, nonMultipart)
	if nonMPResp.Code != http.StatusBadRequest {
		t.Errorf("non-multipart status = %d, want 400", nonMPResp.Code)
	}

	// Missing "file" field -> 400.
	var missingBody bytes.Buffer
	mw := multipart.NewWriter(&missingBody)
	_ = mw.WriteField("other", "value")
	_ = mw.Close()
	missingReq := httptest.NewRequest(http.MethodPost, "/api/v1/files", &missingBody)
	missingReq.Header.Set("Content-Type", mw.FormDataContentType())
	missingResp := httptest.NewRecorder()
	handler.ServeHTTP(missingResp, missingReq)
	if missingResp.Code != http.StatusBadRequest {
		t.Errorf("missing file field status = %d, want 400", missingResp.Code)
	}

	// Multiple "file" fields -> 400 and nothing stored.
	var multiBody bytes.Buffer
	multiW := multipart.NewWriter(&multiBody)
	p1, _ := multiW.CreateFormFile("file", "a.txt")
	_, _ = p1.Write([]byte("hello"))
	p2, _ := multiW.CreateFormFile("file", "b.txt")
	_, _ = p2.Write([]byte("world"))
	_ = multiW.Close()
	multiReq := httptest.NewRequest(http.MethodPost, "/api/v1/files", &multiBody)
	multiReq.Header.Set("Content-Type", multiW.FormDataContentType())
	multiResp := httptest.NewRecorder()
	handler.ServeHTTP(multiResp, multiReq)
	if multiResp.Code != http.StatusBadRequest {
		t.Errorf("multiple file parts status = %d, want 400", multiResp.Code)
	}

	// Invalid filename -> 400.
	for _, badName := range []string{"..", "..\\evil.txt"} {
		var badNameBody bytes.Buffer
		bnw := multipart.NewWriter(&badNameBody)
		bnp, _ := bnw.CreateFormFile("file", badName)
		_, _ = bnp.Write([]byte("ok"))
		_ = bnw.Close()
		badNameReq := httptest.NewRequest(http.MethodPost, "/api/v1/files", &badNameBody)
		badNameReq.Header.Set("Content-Type", bnw.FormDataContentType())
		badNameResp := httptest.NewRecorder()
		handler.ServeHTTP(badNameResp, badNameReq)
		if badNameResp.Code != http.StatusBadRequest {
			t.Errorf("invalid filename %q status = %d, want 400", badName, badNameResp.Code)
		}
	}

	// Oversize upload -> 413.
	ctOver, overBody := buildMultipartUpload(t, "file", "big.bin", bytes.Repeat([]byte("a"), 17))
	overReq := httptest.NewRequest(http.MethodPost, "/api/v1/files", overBody)
	overReq.Header.Set("Content-Type", ctOver)
	overResp := httptest.NewRecorder()
	handler.ServeHTTP(overResp, overReq)
	if overResp.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize upload status = %d, want 413 (body: %s)", overResp.Code, overResp.Body.String())
	}

	// Verify none of the failed uploads appear in list or leave files on disk.
	listResp := httptest.NewRecorder()
	handler.ServeHTTP(listResp, httptest.NewRequest(http.MethodGet, "/api/v1/files", nil))
	if listResp.Code != http.StatusOK || strings.TrimSpace(listResp.Body.String()) != "[]" {
		t.Fatalf("list after failed uploads = (%d, %q), want (200, [])", listResp.Code, listResp.Body.String())
	}

	for _, sub := range []string{"tmp", "objects"} {
		entries, err := os.ReadDir(filepath.Join(dataDir, sub))
		if err != nil {
			t.Fatalf("ReadDir(%s) error = %v", sub, err)
		}
		if len(entries) != 0 {
			t.Fatalf("%s directory not empty after failed uploads: %d entries", sub, len(entries))
		}
	}
}

type failingMetaStore struct{}

func (failingMetaStore) Create(context.Context, files.StoredFile) error {
	return errors.New("db write failure")
}
func (failingMetaStore) List(context.Context, files.ListPage) ([]files.File, error) {
	return []files.File{}, nil
}
func (failingMetaStore) GetByID(context.Context, string) (files.StoredFile, error) {
	return files.StoredFile{}, files.ErrNotFound
}

func TestFailedMetadataWriteCleansUpBlobAndReturns500(t *testing.T) {
	dataDir := t.TempDir()
	blobs, err := localdisk.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := files.NewService(blobs, failingMetaStore{}, func() (string, error) { return "id-1", nil })
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(svc)

	ct, body := buildMultipartUpload(t, "file", "fail.txt", []byte("content"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files", body)
	req.Header.Set("Content-Type", ct)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.Code)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("objects dir has %d entries, want 0 after metadata failure rollback", len(entries))
	}
}

func TestPaginationWithBoundedLimit(t *testing.T) {
	dataDir := t.TempDir()
	handler, cleanup := newTestServerHandler(t, dataDir)
	defer cleanup()

	for i := range 3 {
		ct, body := buildMultipartUpload(t, "file", fmt.Sprintf("f%d.txt", i), []byte{byte('a' + i)})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/files", body)
		req.Header.Set("Content-Type", ct)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusCreated {
			t.Fatalf("upload %d status = %d", i, resp.Code)
		}
	}

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/v1/files?limit=2&offset=1", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("paginated list status = %d", resp.Code)
	}
	var items []fileResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	_ = io.Discard
}
