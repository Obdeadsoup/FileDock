package syncclient_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"example.com/filedock/internal/syncclient"
)

func TestPushTwoMirrorsOfflineEditAndCursorPreservation(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	dirA := t.TempDir()
	dirB := t.TempDir()
	dirC := t.TempDir()

	fileID := ts.uploadFile(t, ts.tokenA, nil, "notes.txt", []byte("shared baseline v1"))

	// Mirrors A, B, and C all sync v1.
	for _, d := range []string{dirA, dirB, dirC} {
		if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: d}); err != nil {
			t.Fatalf("initial pull into %s error = %v", d, err)
		}
	}

	// Simulate another device uploading a separate file while A and B edit offline.
	_ = ts.uploadFile(t, ts.tokenA, nil, "from-other-device.txt", []byte("other device payload"))

	// Offline edits on Mirror A and Mirror B from the same baseline.
	editA := []byte("mirror A winning content")
	editB := []byte("mirror B conflicting content")
	if err := os.WriteFile(filepath.Join(dirA, "notes.txt"), editA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "notes.txt"), editB, 0o644); err != nil {
		t.Fatal(err)
	}

	// Mirror A pushes first -> succeeds (revision 1 -> 2), keeps cursor=1!
	pushA, err := syncclient.Push(ctx, syncclient.PushConfig{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		Dir:       dirA,
		RelPath:   "notes.txt",
	})
	if err != nil {
		t.Fatalf("Mirror A Push() error = %v", err)
	}
	if pushA.Skipped || pushA.Recovered || pushA.FileID != fileID || pushA.OldRevision != 1 || pushA.NewRevision != 2 || pushA.Cursor != 1 {
		t.Fatalf("unexpected pushA result: %+v", pushA)
	}

	// Mirror B pushes second -> conflicts (ErrRemoteConflict), preserves local editB and original manifest (rev 1)!
	if _, err := syncclient.Push(ctx, syncclient.PushConfig{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		Dir:       dirB,
		RelPath:   "notes.txt",
	}); !errors.Is(err, syncclient.ErrRemoteConflict) {
		t.Fatalf("Mirror B Push() error = %v, want ErrRemoteConflict", err)
	}
	gotB, err := os.ReadFile(filepath.Join(dirB, "notes.txt"))
	if err != nil || string(gotB) != string(editB) {
		t.Fatalf("Mirror B local edit was not preserved: %q err=%v", gotB, err)
	}
	var stateB syncclient.State
	rawB, err := os.ReadFile(filepath.Join(dirB, ".filedock", "state.json"))
	if err != nil || json.Unmarshal(rawB, &stateB) != nil {
		t.Fatalf("read Mirror B state: err=%v", err)
	}
	if stateB.Files[fileID].ContentRevision != 1 {
		t.Fatalf("Mirror B manifest revision changed on conflict: %d, want 1", stateB.Files[fileID].ContentRevision)
	}

	// Mirror A pulls next -> must still observe from-other-device.txt (cursor was NOT skipped) and reuse notes.txt!
	pullA, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirA})
	if err != nil {
		t.Fatalf("Mirror A follow-up Pull() error = %v", err)
	}
	if pullA.Downloaded != 1 {
		t.Fatalf("Mirror A follow-up Pull() downloaded = %d, want 1 (only from-other-device.txt)", pullA.Downloaded)
	}
	if _, err := os.Stat(filepath.Join(dirA, "from-other-device.txt")); err != nil {
		t.Fatalf("Mirror A missing from-other-device.txt after pull: %v", err)
	}

	// Unedited Mirror C pulls -> receives Mirror A's winning content and from-other-device.txt with identical hashes!
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirC}); err != nil {
		t.Fatalf("Mirror C Pull() error = %v", err)
	}
	gotC, err := os.ReadFile(filepath.Join(dirC, "notes.txt"))
	if err != nil || string(gotC) != string(editA) {
		t.Fatalf("Mirror C notes.txt = %q, want %q", gotC, editA)
	}
}

func TestPushUnchangedLegacyManifestAndRemoteDeleted(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	dir := t.TempDir()
	fileID := ts.uploadFile(t, ts.tokenA, nil, "doc.txt", []byte("baseline"))
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dir}); err != nil {
		t.Fatal(err)
	}

	// 1. Unchanged file skips upload.
	skipRes, err := syncclient.Push(ctx, syncclient.PushConfig{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		Dir:       dir,
		RelPath:   "doc.txt",
	})
	if err != nil || !skipRes.Skipped || skipRes.NewRevision != 1 {
		t.Fatalf("unchanged Push() = (%+v, %v), want Skipped=true rev=1", skipRes, err)
	}

	// 2. Untracked stranger file is rejected with ErrUntrackedFile.
	if err := os.WriteFile(filepath.Join(dir, "stranger.txt"), []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncclient.Push(ctx, syncclient.PushConfig{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		Dir:       dir,
		RelPath:   "stranger.txt",
	}); !errors.Is(err, syncclient.ErrUntrackedFile) {
		t.Fatalf("stranger Push() = %v, want ErrUntrackedFile", err)
	}

	// 3. Old manifest lacking content_revision returns ErrMissingBaseline and preserves local edits.
	statePath := filepath.Join(dir, ".filedock", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var st syncclient.State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	rec := st.Files[fileID]
	rec.ContentRevision = 0 // Simulate pre-revision manifest.
	st.Files[fileID] = rec
	updatedRaw, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(statePath, updatedRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	edited := []byte("edited while on legacy manifest")
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncclient.Push(ctx, syncclient.PushConfig{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		Dir:       dir,
		RelPath:   "doc.txt",
	}); !errors.Is(err, syncclient.ErrMissingBaseline) {
		t.Fatalf("legacy manifest Push() = %v, want ErrMissingBaseline", err)
	}
	gotDoc, _ := os.ReadFile(filepath.Join(dir, "doc.txt"))
	if string(gotDoc) != string(edited) {
		t.Fatalf("local edits lost on legacy manifest error: %q", gotDoc)
	}

	// Restore baseline revision=1, then delete remote file -> Push reports ErrRemoteDeleted and preserves local edits.
	rec.ContentRevision = 1
	st.Files[fileID] = rec
	updatedRaw, _ = json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(statePath, updatedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	ts.delete(t, ts.tokenA, "/api/v1/files/"+fileID)

	if _, err := syncclient.Push(ctx, syncclient.PushConfig{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		Dir:       dir,
		RelPath:   "doc.txt",
	}); !errors.Is(err, syncclient.ErrRemoteDeleted) {
		t.Fatalf("deleted remote Push() = %v, want ErrRemoteDeleted", err)
	}
	gotDoc, _ = os.ReadFile(filepath.Join(dir, "doc.txt"))
	if string(gotDoc) != string(edited) {
		t.Fatalf("local edits lost on remote deleted error: %q", gotDoc)
	}
}

type dropResponseAPI struct {
	syncclient.API
	dropNextPutResponse bool
	putCalls            int
}

func (d *dropResponseAPI) ReplaceFileContent(ctx context.Context, fileID, ifMatch string, body io.Reader, contentLength int64) (syncclient.FileMeta, string, error) {
	d.putCalls++
	updated, etag, err := d.API.ReplaceFileContent(ctx, fileID, ifMatch, body, contentLength)
	if err != nil {
		return updated, etag, err
	}
	if d.dropNextPutResponse {
		d.dropNextPutResponse = false
		return syncclient.FileMeta{}, "", io.ErrUnexpectedEOF
	}
	return updated, etag, nil
}

func TestPushResponseLostRecovery(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	dir := t.TempDir()
	_ = ts.uploadFile(t, ts.tokenA, nil, "draft.txt", []byte("v1 draft"))
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dir}); err != nil {
		t.Fatal(err)
	}

	v2 := []byte("v2 committed before response dropped")
	if err := os.WriteFile(filepath.Join(dir, "draft.txt"), v2, 0o644); err != nil {
		t.Fatal(err)
	}

	httpAPI, err := syncclient.NewHTTPAPI(ts.srv.URL, ts.tokenA, nil)
	if err != nil {
		t.Fatal(err)
	}
	flaky := &dropResponseAPI{API: httpAPI, dropNextPutResponse: true}

	// First Push commits on server, then loses the HTTP response.
	if _, err := syncclient.Push(ctx, syncclient.PushConfig{
		ServerURL: ts.srv.URL,
		Dir:       dir,
		RelPath:   "draft.txt",
		API:       flaky,
	}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("first Push() error = %v, want io.ErrUnexpectedEOF", err)
	}
	if flaky.putCalls != 1 {
		t.Fatalf("putCalls = %d, want 1", flaky.putCalls)
	}

	// Retry Push -> verifies remote matches snapshot & pending push, recovers without a second PUT!
	res, err := syncclient.Push(ctx, syncclient.PushConfig{
		ServerURL: ts.srv.URL,
		Dir:       dir,
		RelPath:   "draft.txt",
		API:       flaky,
	})
	if err != nil {
		t.Fatalf("retry Push() error = %v", err)
	}
	if !res.Recovered || res.OldRevision != 1 || res.NewRevision != 2 || flaky.putCalls != 1 {
		t.Fatalf("retry Push() = %+v putCalls=%d, want Recovered=true rev=1->2 putCalls=1", res, flaky.putCalls)
	}
	wantSum := sha256.Sum256(v2)
	if res.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("recovered sha256 = %q, want %q", res.SHA256, hex.EncodeToString(wantSum[:]))
	}
}

func TestFirstPullRecoversWhenOnlyDotFiledockTmpExists(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	_ = ts.uploadFile(t, ts.tokenA, nil, "hello.txt", []byte("hello world"))

	// 1. Directory containing ONLY .filedock/tmp (no state.json) recovers cleanly.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".filedock", "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dir})
	if err != nil || res.Downloaded != 1 {
		t.Fatalf("Pull() with leftover .filedock/tmp = (%+v, %v), want Downloaded=1", res, err)
	}

	// 2. Directory with .filedock/tmp AND a stranger file is still rejected, preserving the stranger file.
	dirWithStranger := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirWithStranger, ".filedock", "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	strangerPath := filepath.Join(dirWithStranger, "keep-me.txt")
	if err := os.WriteFile(strangerPath, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncclient.Pull(ctx, syncclient.Config{ServerURL: ts.srv.URL, Token: ts.tokenA, Dir: dirWithStranger}); !errors.Is(err, syncclient.ErrDirNotEmpty) {
		t.Fatalf("Pull() with stranger + .filedock/tmp = %v, want ErrDirNotEmpty", err)
	}
	got, err := os.ReadFile(strangerPath)
	if err != nil || string(got) != "precious" {
		t.Fatalf("stranger file not preserved: %q err=%v", got, err)
	}
}
