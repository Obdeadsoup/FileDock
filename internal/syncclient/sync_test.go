package syncclient_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/httpapi"
	"example.com/filedock/internal/store/localdisk"
	"example.com/filedock/internal/store/sqlite"
	"example.com/filedock/internal/syncclient"
)

type testServer struct {
	srv     *httptest.Server
	tokenA  string
	tokenB  string
	cleanup func()
}

var idSeq atomic.Int64

func startTestServer(t *testing.T) *testServer {
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
	idGen := func() (string, error) {
		return fmt.Sprintf("id-%d", idSeq.Add(1)), nil
	}
	fileSvc, err := files.NewService(blobs, meta, idGen)
	if err != nil {
		_ = meta.Close()
		t.Fatal(err)
	}
	acctSvc, err := accounts.NewService(meta, accounts.WithArgon2Params(accounts.Argon2Params{
		Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 32,
	}))
	if err != nil {
		_ = meta.Close()
		t.Fatal(err)
	}

	srv := httptest.NewServer(httpapi.NewHandler(fileSvc, acctSvc))
	ts := &testServer{
		srv: srv,
		cleanup: func() {
			srv.Close()
			_ = meta.Close()
		},
	}
	t.Cleanup(ts.cleanup)

	ts.tokenA = ts.registerAndLogin(t, "alice", "alice-pass")
	ts.tokenB = ts.registerAndLogin(t, "bob", "bob-pass")
	return ts
}

func (ts *testServer) registerAndLogin(t *testing.T, username, password string) string {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	resp, err := http.Post(ts.srv.URL+"/api/v1/auth/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register %s status = %d", username, resp.StatusCode)
	}
	resp, err = http.Post(ts.srv.URL+"/api/v1/auth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Token
}

func (ts *testServer) createFolder(t *testing.T, token, name string, parentID *string) string {
	t.Helper()
	payload := map[string]any{"name": name}
	if parentID != nil {
		payload["parent_id"] = *parentID
	}
	raw, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, ts.srv.URL+"/api/v1/folders", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("createFolder(%s) status = %d", name, resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.ID
}

func (ts *testServer) uploadFile(t *testing.T, token string, parentID *string, name string, content []byte) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	p, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Write(content)
	_ = mw.Close()

	target := ts.srv.URL + "/api/v1/files"
	if parentID != nil {
		target += "?parent_id=" + url.QueryEscape(*parentID)
	}
	req, _ := http.NewRequest(http.MethodPost, target, &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("uploadFile(%s) status = %d", name, resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.ID
}

func (ts *testServer) patch(t *testing.T, token, path, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, ts.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH %s status = %d", path, resp.StatusCode)
	}
}

func (ts *testServer) delete(t *testing.T, token, path string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE %s status = %d", path, resp.StatusCode)
	}
}

func snapshotMirror(t *testing.T, dir string) (dirs []string, fileHashes map[string]string) {
	t.Helper()
	fileHashes = make(map[string]string)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".filedock" || strings.HasPrefix(rel, ".filedock/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		fileHashes[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("walk mirror %s: %v", dir, err)
	}
	return dirs, fileHashes
}

func TestTwoDirectoriesDifferentCursorsSameNamesAndOfflineDelete(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	dirA := t.TempDir()
	dirB := t.TempDir()

	// Initial server state: nested folder + normal file + duplicate-name files + case-only colliding files.
	docsDir := ts.createFolder(t, ts.tokenA, "docs", nil)
	subDir := ts.createFolder(t, ts.tokenA, "sub", &docsDir)
	fKeep := ts.uploadFile(t, ts.tokenA, &subDir, "notes.txt", []byte("keep me"))
	_ = ts.uploadFile(t, ts.tokenA, &docsDir, "dup.txt", []byte("dup version 1"))
	_ = ts.uploadFile(t, ts.tokenA, &docsDir, "dup.txt", []byte("dup version 2"))
	_ = ts.uploadFile(t, ts.tokenA, nil, "Case.txt", []byte("upper case"))
	_ = ts.uploadFile(t, ts.tokenA, nil, "case.txt", []byte("lower case"))

	// Dir A syncs early from cursor 0.
	resA1, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirA, PageLimit: 2})
	if err != nil {
		t.Fatalf("dirA pull 1 error = %v", err)
	}
	if resA1.Downloaded != 5 {
		t.Fatalf("dirA pull 1 downloaded = %d, want 5", resA1.Downloaded)
	}

	// Verify token is never written to state.json.
	stateBytes, err := os.ReadFile(filepath.Join(dirA, ".filedock", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBytes), ts.tokenA) {
		t.Fatal("state.json must never contain the Bearer token")
	}

	// Repeat pull with no changes does not re-download.
	resA2, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirA})
	if err != nil || resA2.Downloaded != 0 || resA2.EventsApplied != 0 {
		t.Fatalf("dirA repeat pull = (%+v, %v), want 0 downloads and 0 events", resA2, err)
	}

	// While Dir B is still offline (at cursor 0), create and delete an ephemeral file, move/rename fKeep, delete empty subDir.
	ephemeral := ts.uploadFile(t, ts.tokenA, nil, "ephemeral.txt", []byte("gone before sync"))
	ts.delete(t, ts.tokenA, "/api/v1/files/"+ephemeral)
	ts.patch(t, ts.tokenA, "/api/v1/files/"+fKeep, `{"name":"notes-renamed.txt","parent_id":null}`)
	ts.delete(t, ts.tokenA, "/api/v1/folders/"+subDir)

	// Dir A catches up incrementally: fKeep is moved/reused with 0 downloads!
	resA3, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirA})
	if err != nil {
		t.Fatalf("dirA pull 3 error = %v", err)
	}
	if resA3.Downloaded != 0 || resA3.Reused == 0 {
		t.Fatalf("dirA incremental pull = %+v, want Downloaded=0 and Reused>0", resA3)
	}

	// Dir B syncs from cursor 0: never downloads the offline-deleted ephemeral file!
	resB, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirB, PageLimit: 3})
	if err != nil {
		t.Fatalf("dirB pull error = %v", err)
	}
	if resB.Downloaded != 5 {
		t.Fatalf("dirB downloaded = %d, want 5 (ephemeral file must not be downloaded)", resB.Downloaded)
	}

	dirsA, hashesA := snapshotMirror(t, dirA)
	dirsB, hashesB := snapshotMirror(t, dirB)
	if !reflect.DeepEqual(dirsA, dirsB) {
		t.Fatalf("mirror dirs mismatch:\nA=%v\nB=%v", dirsA, dirsB)
	}
	if !reflect.DeepEqual(hashesA, hashesB) {
		t.Fatalf("mirror file hashes mismatch:\nA=%v\nB=%v", hashesA, hashesB)
	}
	if len(hashesA) != 5 {
		t.Fatalf("expected 5 distinct files (including dup & case collisions), got %d: %v", len(hashesA), hashesA)
	}
}

type faultyAPI struct {
	syncclient.API
	failDownloadOnce    bool
	corruptDownloadOnce bool
}

type brokenReader struct {
	data []byte
	sent bool
}

func (b *brokenReader) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		n := copy(p, b.data)
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}
func (b *brokenReader) Close() error { return nil }

func (f *faultyAPI) DownloadFile(ctx context.Context, fileID string) (io.ReadCloser, error) {
	if f.failDownloadOnce {
		f.failDownloadOnce = false
		return &brokenReader{data: []byte("partial")}, nil
	}
	if f.corruptDownloadOnce {
		f.corruptDownloadOnce = false
		return io.NopCloser(strings.NewReader("corrupted bytes")), nil
	}
	return f.API.DownloadFile(ctx, fileID)
}

func TestDownloadFaultsHashMismatchAndMidApplyCrashRecovery(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	dir := t.TempDir()
	f1 := ts.uploadFile(t, ts.tokenA, nil, "a.txt", []byte("alpha content"))

	httpAPI, err := syncclient.NewHTTPAPI(ts.srv.URL, ts.tokenA, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Download interruption fails cleanly without advancing cursor or leaving partial mirror file.
	interrupted := &faultyAPI{API: httpAPI, failDownloadOnce: true}
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Dir: dir, API: interrupted}); err == nil {
		t.Fatal("expected error on interrupted download")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a.txt must not be published on interrupted download, stat err=%v", err)
	}

	// 2. SHA-256 mismatch fails with ErrHashMismatch without publishing.
	corrupt := &faultyAPI{API: httpAPI, corruptDownloadOnce: true}
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Dir: dir, API: corrupt}); !errors.Is(err, syncclient.ErrHashMismatch) {
		t.Fatalf("corrupt download error = %v, want ErrHashMismatch", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a.txt must not be published on hash mismatch, stat err=%v", err)
	}

	// Healthy pull now succeeds.
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dir}); err != nil {
		t.Fatalf("healthy pull error = %v", err)
	}

	// 3. Prepare a multi-op batch (new folder, new file, rename/move existing file, delete another file) and inject crash faults at each apply step.
	for _, crashStep := range []string{"after_downloads", "after_stage_moves", "after_deletes", "after_mkdirs", "after_publish", "after_rmdirs"} {
		t.Run("crash_"+crashStep, func(t *testing.T) {
			stepDir := t.TempDir()
			if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: stepDir}); err != nil {
				t.Fatal(err)
			}

			sub := ts.createFolder(t, ts.tokenA, "dir-"+crashStep, nil)
			tmpFile := ts.uploadFile(t, ts.tokenA, &sub, "new-"+crashStep+".txt", []byte("payload-"+crashStep))
			ts.patch(t, ts.tokenA, "/api/v1/files/"+f1, fmt.Sprintf(`{"name":"moved-%s.txt"}`, crashStep))

			injectedErr := errors.New("injected crash at " + crashStep)
			_, err := syncclient.Pull(ctx, syncclient.Config{
				ServerURL: ts.srv.URL,
				Token:     ts.tokenA,
				Dir:       stepDir,
				AfterStepHook: func(step string) error {
					if step == crashStep {
						return injectedErr
					}
					return nil
				},
			})
			if !errors.Is(err, injectedErr) {
				t.Fatalf("expected injected error at %s, got %v", crashStep, err)
			}

			// Re-run Pull without fault injection -> must replay safely and converge!
			if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: stepDir}); err != nil {
				t.Fatalf("recovery pull after %s error = %v", crashStep, err)
			}

			// Compare against a clean reference mirror synced from scratch.
			refDir := t.TempDir()
			if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: refDir}); err != nil {
				t.Fatal(err)
			}
			d1, h1 := snapshotMirror(t, stepDir)
			d2, h2 := snapshotMirror(t, refDir)
			if !reflect.DeepEqual(d1, d2) || !reflect.DeepEqual(h1, h2) {
				t.Fatalf("recovered mirror diverged from reference:\ngot dirs=%v hashes=%v\nwant dirs=%v hashes=%v", d1, h1, d2, h2)
			}

			// Clean up per-subtest file/folder.
			ts.delete(t, ts.tokenA, "/api/v1/files/"+tmpFile)
			ts.delete(t, ts.tokenA, "/api/v1/folders/"+sub)
		})
	}
}

func TestDirSafetyBindingMismatchAndStrangerProtection(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	// 1. First run refuses non-empty directory.
	nonEmptyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmptyDir, "existing.txt"), []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: nonEmptyDir}); !errors.Is(err, syncclient.ErrDirNotEmpty) {
		t.Fatalf("first pull into non-empty dir = %v, want ErrDirNotEmpty", err)
	}

	// 2. Sync Alice into empty dir, then test server URL mismatch and user mismatch.
	dir := t.TempDir()
	f1 := ts.uploadFile(t, ts.tokenA, nil, "tracked.txt", []byte("original"))
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dir}); err != nil {
		t.Fatal(err)
	}

	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: "http://127.0.0.1:1", Token: ts.tokenA, Dir: dir}); !errors.Is(err, syncclient.ErrServerMismatch) {
		t.Fatalf("server mismatch error = %v, want ErrServerMismatch", err)
	}
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenB, Dir: dir}); !errors.Is(err, syncclient.ErrUserMismatch) {
		t.Fatalf("user mismatch error = %v, want ErrUserMismatch", err)
	}

	// 3. Stranger file at non-colliding path is preserved; stranger file at target path causes conflict.
	strangerPath := filepath.Join(dir, "my-local-note.txt")
	if err := os.WriteFile(strangerPath, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	collidePath := filepath.Join(dir, "collide.txt")
	if err := os.WriteFile(collidePath, []byte("unmanaged user file"), 0o644); err != nil {
		t.Fatal(err)
	}

	ts.uploadFile(t, ts.tokenA, nil, "collide.txt", []byte("server file"))
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dir}); !errors.Is(err, syncclient.ErrConflict) {
		t.Fatalf("stranger collision error = %v, want ErrConflict", err)
	}
	// Verify collide.txt was NOT overwritten and strangerPath is intact.
	gotCollide, _ := os.ReadFile(collidePath)
	if string(gotCollide) != "unmanaged user file" {
		t.Fatalf("collide.txt was overwritten: %q", gotCollide)
	}
	_ = os.Remove(collidePath)
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dir}); err != nil {
		t.Fatalf("pull after resolving stranger collision error = %v", err)
	}
	gotStranger, err := os.ReadFile(strangerPath)
	if err != nil || string(gotStranger) != "do not touch" {
		t.Fatalf("stranger file lost or altered: %q err=%v", gotStranger, err)
	}

	// 4. User manually modifies tracked.txt -> server deletion reports ErrConflict and preserves user edits.
	trackedPath := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(trackedPath, []byte("user local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts.delete(t, ts.tokenA, "/api/v1/files/"+f1)
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dir}); !errors.Is(err, syncclient.ErrConflict) {
		t.Fatalf("deleting locally modified file error = %v, want ErrConflict", err)
	}
	gotTracked, err := os.ReadFile(trackedPath)
	if err != nil || string(gotTracked) != "user local edit" {
		t.Fatalf("locally modified tracked.txt was deleted or overwritten: %q err=%v", gotTracked, err)
	}
}

func TestFirstPullCrashAfterPublishRecoversAndProtectsNonEmptyDir(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	dirFolder := ts.createFolder(t, ts.tokenA, "docs", nil)
	_ = ts.uploadFile(t, ts.tokenA, &dirFolder, "first.txt", []byte("initial sync file"))

	// 1. First pull into a brand-new empty directory crashes right after publishing files, before final state commit.
	mirrorDir := t.TempDir()
	injected := errors.New("simulated crash after publish on first pull")
	_, err := syncclient.Pull(ctx, syncclient.Config{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		Dir:       mirrorDir,
		AfterStepHook: func(step string) error {
			if step == "after_publish" {
				return injected
			}
			return nil
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("expected injected crash, got %v", err)
	}
	// Confirm file was already published on disk before the crash.
	if _, statErr := os.Stat(filepath.Join(mirrorDir, "docs", "first.txt")); statErr != nil {
		t.Fatalf("expected docs/first.txt to exist after publish step: %v", statErr)
	}

	// 2. Re-running Pull on the same directory must NOT fail with ErrDirNotEmpty; it reuses published file and converges.
	res, err := syncclient.Pull(ctx, syncclient.Config{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		Dir:       mirrorDir,
	})
	if err != nil {
		t.Fatalf("recovery pull after first-run publish crash failed: %v", err)
	}
	if res.Downloaded != 0 || res.Reused != 1 {
		t.Fatalf("recovery pull = %+v, want Downloaded=0 Reused=1", res)
	}

	// 3. A genuinely non-empty directory with pre-existing user files is still rejected with ErrDirNotEmpty.
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "preexisting.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: nonEmpty}); !errors.Is(err, syncclient.ErrDirNotEmpty) {
		t.Fatalf("pull into genuinely non-empty dir = %v, want ErrDirNotEmpty", err)
	}
}

func TestTwoMirrorsSyncInPlaceContentUpdate(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	dirA := t.TempDir()
	dirB := t.TempDir()

	fileID := ts.uploadFile(t, ts.tokenA, nil, "report.txt", []byte("version 1 content"))

	// Mirror A syncs v1.
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirA}); err != nil {
		t.Fatalf("dirA pull v1 error = %v", err)
	}

	// Read ETag and update content in place via PUT /api/v1/files/{id}/content.
	headReq, _ := http.NewRequest(http.MethodHead, ts.srv.URL+"/api/v1/files/"+fileID+"/content", nil)
	headReq.Header.Set("Authorization", "Bearer "+ts.tokenA)
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = headResp.Body.Close()
	etag := headResp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag on HEAD response")
	}

	v2Content := []byte("version 2 updated content in place")
	putReq, _ := http.NewRequest(http.MethodPut, ts.srv.URL+"/api/v1/files/"+fileID+"/content", bytes.NewReader(v2Content))
	putReq.Header.Set("Authorization", "Bearer "+ts.tokenA)
	putReq.Header.Set("If-Match", etag)
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT content status = %d, want 200", putResp.StatusCode)
	}

	// Mirror A pulls incrementally (updates report.txt in place), Mirror B pulls from cursor 0.
	resA, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirA})
	if err != nil {
		t.Fatalf("dirA pull v2 error = %v", err)
	}
	if resA.Downloaded != 1 {
		t.Fatalf("dirA pull v2 downloaded = %d, want 1", resA.Downloaded)
	}

	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirB}); err != nil {
		t.Fatalf("dirB pull v2 error = %v", err)
	}

	dA, hA := snapshotMirror(t, dirA)
	dB, hB := snapshotMirror(t, dirB)
	if !reflect.DeepEqual(dA, dB) || !reflect.DeepEqual(hA, hB) {
		t.Fatalf("mirrors diverged after in-place PUT:\nA=(%v, %v)\nB=(%v, %v)", dA, hA, dB, hB)
	}
	wantSum := sha256.Sum256(v2Content)
	if hA["report.txt"] != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("report.txt hash = %q, want %q", hA["report.txt"], hex.EncodeToString(wantSum[:]))
	}
}
