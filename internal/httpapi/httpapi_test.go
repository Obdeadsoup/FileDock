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
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/store/localdisk"
	"example.com/filedock/internal/store/sqlite"
)

var fastTestArgonParams = accounts.Argon2Params{
	Time:    1,
	Memory:  8 * 1024,
	Threads: 1,
	KeyLen:  32,
}

func TestHealth(t *testing.T) {
	handler := NewHandler(nil, nil)
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
	handler := NewHandler(nil, nil)
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

var testIDSeq atomic.Int64

type testEnv struct {
	handler    http.Handler
	fileSvc    *files.Service
	accountSvc *accounts.Service
	cleanup    func()
	advance    func(d time.Duration)
}

func newTestEnv(t *testing.T, dataDir string, fileOpts ...files.Option) *testEnv {
	t.Helper()
	blobs, err := localdisk.NewStore(dataDir)
	if err != nil {
		t.Fatalf("localdisk.NewStore() error = %v", err)
	}
	meta, err := sqlite.NewStore(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("sqlite.NewStore() error = %v", err)
	}

	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	idGen := func() (string, error) {
		n := testIDSeq.Add(1)
		return fmt.Sprintf("id-%d-%d", n, os.Getpid()), nil
	}

	allFileOpts := append([]files.Option{files.WithClock(clock)}, fileOpts...)
	fileSvc, err := files.NewService(blobs, meta, idGen, allFileOpts...)
	if err != nil {
		_ = meta.Close()
		t.Fatalf("files.NewService() error = %v", err)
	}

	accountSvc, err := accounts.NewService(
		meta,
		accounts.WithClock(clock),
		accounts.WithSessionTTL(1*time.Hour),
		accounts.WithArgon2Params(fastTestArgonParams),
	)
	if err != nil {
		_ = meta.Close()
		t.Fatalf("accounts.NewService() error = %v", err)
	}

	return &testEnv{
		handler:    NewHandler(fileSvc, accountSvc),
		fileSvc:    fileSvc,
		accountSvc: accountSvc,
		cleanup:    func() { _ = meta.Close() },
		advance:    func(d time.Duration) { now = now.Add(d) },
	}
}

func registerAndLogin(t *testing.T, h http.Handler, username, password string) string {
	t.Helper()
	regBody := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	regReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(regBody))
	regReq.Header.Set("Content-Type", "application/json")
	regResp := httptest.NewRecorder()
	h.ServeHTTP(regResp, regReq)
	if regResp.Code != http.StatusCreated && regResp.Code != http.StatusConflict {
		t.Fatalf("register(%s) status = %d, body = %s", username, regResp.Code, regResp.Body.String())
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(regBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp := httptest.NewRecorder()
	h.ServeHTTP(loginResp, loginReq)
	if loginResp.Code != http.StatusOK {
		t.Fatalf("login(%s) status = %d, body = %s", username, loginResp.Code, loginResp.Body.String())
	}
	var lr loginResponse
	if err := json.Unmarshal(loginResp.Body.Bytes(), &lr); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if lr.Token == "" {
		t.Fatal("empty token from login")
	}
	return lr.Token
}

func setBearer(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
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

func TestAuthAndMultiAccountFileIsolation(t *testing.T) {
	dataDir := t.TempDir()
	env := newTestEnv(t, dataDir)
	defer env.cleanup()
	h := env.handler

	// 1. Unauthenticated requests return 401.
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/files", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/files", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/files/some-id/content", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil),
	} {
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token status = %d, want 401", req.Method, req.URL.Path, resp.Code)
		}
	}

	// 2. Oversize auth body returns 413.
	hugeAuth := fmt.Sprintf(`{"username":"alice","password":%q}`, strings.Repeat("p", int(maxCredentialsBodyBytes)+100))
	hugeResp := httptest.NewRecorder()
	h.ServeHTTP(hugeResp, httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(hugeAuth)))
	if hugeResp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize register status = %d, want 413", hugeResp.Code)
	}

	// 3. Register Alice and Bob; duplicate Alice returns 409.
	tokenA := registerAndLogin(t, h, "alice", "alice-secret")
	tokenB := registerAndLogin(t, h, "bob", "bob-secret")

	dupResp := httptest.NewRecorder()
	h.ServeHTTP(dupResp, httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"username":"alice","password":"new"}`)))
	if dupResp.Code != http.StatusConflict {
		t.Fatalf("duplicate register status = %d, want 409", dupResp.Code)
	}

	// 4. Uniform login failure response for wrong password vs unknown user.
	wrongPassResp := httptest.NewRecorder()
	h.ServeHTTP(wrongPassResp, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"alice","password":"wrong"}`)))
	unknownUserResp := httptest.NewRecorder()
	h.ServeHTTP(unknownUserResp, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"charlie","password":"wrong"}`)))
	if wrongPassResp.Code != http.StatusUnauthorized || unknownUserResp.Code != http.StatusUnauthorized {
		t.Fatalf("login failure codes = (%d, %d), want (401, 401)", wrongPassResp.Code, unknownUserResp.Code)
	}
	if wrongPassResp.Body.String() != unknownUserResp.Body.String() {
		t.Fatalf("non-uniform login failure bodies: %q vs %q", wrongPassResp.Body.String(), unknownUserResp.Body.String())
	}

	// 5. Alice uploads a file, lists it, and downloads it.
	payloadA := []byte("alice confidential document")
	sumA := sha256.Sum256(payloadA)
	wantHashA := hex.EncodeToString(sumA[:])

	ctA, bodyA := buildMultipartUpload(t, "file", "alice.txt", payloadA)
	upReqA := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", bodyA), tokenA)
	upReqA.Header.Set("Content-Type", ctA)
	upRespA := httptest.NewRecorder()
	h.ServeHTTP(upRespA, upReqA)
	if upRespA.Code != http.StatusCreated {
		t.Fatalf("Alice upload status = %d, body = %s", upRespA.Code, upRespA.Body.String())
	}
	var fileA fileResponse
	if err := json.Unmarshal(upRespA.Body.Bytes(), &fileA); err != nil {
		t.Fatal(err)
	}

	listRespA := httptest.NewRecorder()
	h.ServeHTTP(listRespA, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files", nil), tokenA))
	if listRespA.Code != http.StatusOK {
		t.Fatalf("Alice list status = %d", listRespA.Code)
	}
	var filesA []fileResponse
	if err := json.Unmarshal(listRespA.Body.Bytes(), &filesA); err != nil || len(filesA) != 1 || filesA[0].ID != fileA.ID {
		t.Fatalf("Alice list = %+v, err = %v", filesA, err)
	}

	dlRespA := httptest.NewRecorder()
	h.ServeHTTP(dlRespA, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+fileA.ID+"/content", nil), tokenA))
	if dlRespA.Code != http.StatusOK {
		t.Fatalf("Alice download status = %d", dlRespA.Code)
	}
	gotSumA := sha256.Sum256(dlRespA.Body.Bytes())
	if hex.EncodeToString(gotSumA[:]) != wantHashA {
		t.Fatalf("Alice download sha256 mismatch")
	}

	// 6. Bob's list does NOT contain Alice's file, and downloading Alice's file ID as Bob returns 404.
	listRespB := httptest.NewRecorder()
	h.ServeHTTP(listRespB, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files", nil), tokenB))
	if listRespB.Code != http.StatusOK || strings.TrimSpace(listRespB.Body.String()) != "[]" {
		t.Fatalf("Bob list = (%d, %s), want (200, [])", listRespB.Code, listRespB.Body.String())
	}

	dlRespB := httptest.NewRecorder()
	h.ServeHTTP(dlRespB, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+fileA.ID+"/content", nil), tokenB))
	if dlRespB.Code != http.StatusNotFound {
		t.Fatalf("Bob download of Alice file status = %d, want 404", dlRespB.Code)
	}

	// 7. Alice logs out -> token immediately returns 401.
	logoutResp := httptest.NewRecorder()
	h.ServeHTTP(logoutResp, setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil), tokenA))
	if logoutResp.Code != http.StatusOK {
		t.Fatalf("Alice logout status = %d, body = %s", logoutResp.Code, logoutResp.Body.String())
	}

	afterLogoutResp := httptest.NewRecorder()
	h.ServeHTTP(afterLogoutResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files", nil), tokenA))
	if afterLogoutResp.Code != http.StatusUnauthorized {
		t.Fatalf("Alice list after logout status = %d, want 401", afterLogoutResp.Code)
	}

	// 8. Bob's token expires after TTL -> returns 401.
	env.advance(2 * time.Hour)
	expiredResp := httptest.NewRecorder()
	h.ServeHTTP(expiredResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files", nil), tokenB))
	if expiredResp.Code != http.StatusUnauthorized {
		t.Fatalf("Bob list with expired token status = %d, want 401", expiredResp.Code)
	}
}

func TestMultipartFilenamePlatformConsistency(t *testing.T) {
	dataDir := t.TempDir()
	env := newTestEnv(t, dataDir)
	defer env.cleanup()
	token := registerAndLogin(t, env.handler, "alice", "secret")

	// All of these filenames must be rejected with 400 on every OS without filepath.Base stripping directory prefixes.
	invalidFilenames := []string{
		"",
		".",
		"..",
		"../evil.txt",
		"dir/evil.txt",
		"/etc/passwd",
		`..\evil.txt`,
		`dir\evil.txt`,
		`C:\temp\evil.txt`,
	}
	for _, name := range invalidFilenames {
		ct, body := buildMultipartUpload(t, "file", name, []byte("payload"))
		req := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", body), token)
		req.Header.Set("Content-Type", ct)
		resp := httptest.NewRecorder()
		env.handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Errorf("upload with filename %q: status = %d, want 400 (body: %s)", name, resp.Code, resp.Body.String())
		}
	}

	// Also verify a raw hand-crafted multipart payload containing unescaped single backslashes in filename.
	rawMultipart := "--boundary123\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"..\\raw\\evil.txt\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n" +
		"payload\r\n" +
		"--boundary123--\r\n"
	rawReq := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", strings.NewReader(rawMultipart)), token)
	rawReq.Header.Set("Content-Type", "multipart/form-data; boundary=boundary123")
	rawResp := httptest.NewRecorder()
	env.handler.ServeHTTP(rawResp, rawReq)
	if rawResp.Code != http.StatusBadRequest {
		t.Errorf("raw unescaped backslash filename status = %d, want 400", rawResp.Code)
	}
}

func TestUploadListDownloadAndRestart(t *testing.T) {
	dataDir := t.TempDir()
	env := newTestEnv(t, dataDir)
	token := registerAndLogin(t, env.handler, "alice", "password123")

	// 1. Empty list returns literal JSON empty array.
	listReq := setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files", nil), token)
	listResp := httptest.NewRecorder()
	env.handler.ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("empty list status = %d, want 200", listResp.Code)
	}
	if got := strings.TrimSpace(listResp.Body.String()); got != "[]" {
		t.Fatalf("empty list body = %q, want []", got)
	}

	// 2. Upload first file.
	payload1 := []byte("hello filedock round 3!")
	sum1 := sha256.Sum256(payload1)
	wantHash1 := hex.EncodeToString(sum1[:])

	ct1, body1 := buildMultipartUpload(t, "file", "hello 世界.txt", payload1)
	req1 := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", body1), token)
	req1.Header.Set("Content-Type", ct1)
	resp1 := httptest.NewRecorder()
	env.handler.ServeHTTP(resp1, req1)
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
	req2 := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", body2), token)
	req2.Header.Set("Content-Type", ct2)
	resp2 := httptest.NewRecorder()
	env.handler.ServeHTTP(resp2, req2)
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
	env.handler.ServeHTTP(listResp2, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files", nil), token))
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
	env.handler.ServeHTTP(dlResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+meta1.ID+"/content", nil), token))
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

	// 6. Simulate restart using the same dataDir (token digest and files persist in SQLite).
	env.cleanup()
	envAfterRestart := newTestEnv(t, dataDir)
	defer envAfterRestart.cleanup()

	dlRestart := httptest.NewRecorder()
	envAfterRestart.handler.ServeHTTP(dlRestart, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+meta1.ID+"/content", nil), token))
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
	env := newTestEnv(t, dataDir, files.WithMaxFileSize(16))
	defer env.cleanup()
	handler := env.handler
	token := registerAndLogin(t, handler, "alice", "password123")

	// Unknown ID -> 404.
	notFoundResp := httptest.NewRecorder()
	handler.ServeHTTP(notFoundResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files/does-not-exist/content", nil), token))
	if notFoundResp.Code != http.StatusNotFound {
		t.Errorf("unknown ID status = %d, want 404", notFoundResp.Code)
	}

	// Invalid ID -> 400.
	badIDResp := httptest.NewRecorder()
	handler.ServeHTTP(badIDResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files/%20/content", nil), token))
	if badIDResp.Code != http.StatusBadRequest {
		t.Errorf("invalid ID status = %d, want 400", badIDResp.Code)
	}

	// Invalid pagination -> 400.
	for _, query := range []string{"?limit=0", "?limit=-5", "?limit=abc", "?offset=-1", "?offset=xyz"} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files"+query, nil), token))
		if resp.Code != http.StatusBadRequest {
			t.Errorf("GET /api/v1/files%s status = %d, want 400", query, resp.Code)
		}
	}

	// Non-multipart POST -> 400.
	nonMultipart := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", strings.NewReader("plain text")), token)
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
	missingReq := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", &missingBody), token)
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
	multiReq := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", &multiBody), token)
	multiReq.Header.Set("Content-Type", multiW.FormDataContentType())
	multiResp := httptest.NewRecorder()
	handler.ServeHTTP(multiResp, multiReq)
	if multiResp.Code != http.StatusBadRequest {
		t.Errorf("multiple file parts status = %d, want 400", multiResp.Code)
	}

	// Oversize upload -> 413.
	ctOver, overBody := buildMultipartUpload(t, "file", "big.bin", bytes.Repeat([]byte("a"), 17))
	overReq := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", overBody), token)
	overReq.Header.Set("Content-Type", ctOver)
	overResp := httptest.NewRecorder()
	handler.ServeHTTP(overResp, overReq)
	if overResp.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize upload status = %d, want 413 (body: %s)", overResp.Code, overResp.Body.String())
	}

	// Verify none of the failed uploads appear in list or leave files on disk.
	listResp := httptest.NewRecorder()
	handler.ServeHTTP(listResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files", nil), token))
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

type failingMetaStore struct {
	files.TreeStore
	files.CleanupAndSearchStore
	files.ShareStore
	files.SyncStore
	files.ContentUpdateStore
}

func (failingMetaStore) Create(context.Context, files.StoredFile) error {
	return errors.New("db write failure")
}
func (failingMetaStore) List(context.Context, string, files.ListPage) ([]files.File, error) {
	return []files.File{}, nil
}
func (failingMetaStore) GetByID(context.Context, string, string) (files.StoredFile, error) {
	return files.StoredFile{}, files.ErrNotFound
}

func TestFailedMetadataWriteCleansUpBlobAndReturns500(t *testing.T) {
	dataDir := t.TempDir()
	blobs, err := localdisk.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := sqlite.NewStore(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()

	accountSvc, err := accounts.NewService(meta, accounts.WithArgon2Params(fastTestArgonParams))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := files.NewService(blobs, failingMetaStore{}, func() (string, error) { return "id-1", nil })
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(svc, accountSvc)
	token := registerAndLogin(t, handler, "alice", "password123")

	ct, body := buildMultipartUpload(t, "file", "fail.txt", []byte("content"))
	req := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", body), token)
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
	env := newTestEnv(t, dataDir)
	defer env.cleanup()
	handler := env.handler
	token := registerAndLogin(t, handler, "alice", "password123")

	for i := range 3 {
		ct, body := buildMultipartUpload(t, "file", fmt.Sprintf("f%d.txt", i), []byte{byte('a' + i)})
		req := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files", body), token)
		req.Header.Set("Content-Type", ct)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusCreated {
			t.Fatalf("upload %d status = %d", i, resp.Code)
		}
	}

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files?limit=2&offset=1", nil), token))
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
