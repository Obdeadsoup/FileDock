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
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"example.com/filedock/internal/gui"
)

// newBrowserJarClient opens a fresh browser session against the SAME GUI server instance
// using its own independent cookie jar and CSRF token.
func newBrowserJarClient(t *testing.T, baseURL string) (*http.Client, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	resp, err := client.Get(baseURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	m := regexp.MustCompile(`name="gui-csrf"\s+content="([^"]+)"`).FindSubmatch(body)
	if len(m) != 2 {
		t.Fatal("missing gui-csrf token in index.html")
	}
	return client, string(m[1])
}

func doWithJar(t *testing.T, client *http.Client, csrf, baseURL, method, path string, payload any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, baseURL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-FileDock-GUI-Token", csrf)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

// Regression 1: Local path semantics (leading/trailing spaces, nested paths) and mandatory download SHA-256.
func TestRegression1_LocalPathSpacesAndMandatoryDownloadSHA256(t *testing.T) {
	remote := startRemoteServer(t)
	localDir := t.TempDir()

	// Create nested dir and file with leading & trailing spaces.
	spacedDir := " nested dir "
	spacedFile := " spaced file .txt "
	if err := os.MkdirAll(filepath.Join(localDir, spacedDir), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("spaces preserved content")
	if err := os.WriteFile(filepath.Join(localDir, spacedDir, spacedFile), content, 0o644); err != nil {
		t.Fatal(err)
	}

	gc := startGUIServer(t, remote.srv.URL, localDir)

	// 1. Listing root and entering " nested dir " must preserve exact leading/trailing spaces.
	r, raw := gc.do(t, http.MethodGet, "/api/gui/local/list?path="+strings.ReplaceAll(spacedDir, " ", "%20"), nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("list %q = %d %s, want 200", spacedDir, r.StatusCode, string(raw))
	}
	var listResp struct {
		Path    string `json:"path"`
		Entries []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(raw, &listResp)
	if listResp.Path != spacedDir || len(listResp.Entries) != 1 || listResp.Entries[0].Path != spacedDir+"/"+spacedFile {
		t.Fatalf("spaced dir listing = %+v, want path=%q entry=%q", listResp, spacedDir, spacedDir+"/"+spacedFile)
	}

	// 2. Register/login and upload the spaced file using the path returned by list.
	gc.do(t, http.MethodPost, "/api/gui/auth/register", map[string]string{"username": "alice", "password": "password123"})
	gc.do(t, http.MethodPost, "/api/gui/auth/login", map[string]string{"username": "alice", "password": "password123"})

	r, upRaw := gc.do(t, http.MethodPost, "/api/gui/transfer/upload", map[string]any{
		"local_path": listResp.Entries[0].Path,
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("upload spaced file %q = %d %s, want 201", listResp.Entries[0].Path, r.StatusCode, string(upRaw))
	}
	var uploaded struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(upRaw, &uploaded)

	// 3. Download without expected_sha256 must NOT silently skip integrity verification.
	r, dlRaw := gc.do(t, http.MethodPost, "/api/gui/transfer/download", map[string]string{
		"file_id":         uploaded.ID,
		"file_name":       "no-hash.txt",
		"expected_sha256": "",
		"local_dir":       "",
		"conflict_policy": "ask",
	})
	if r.StatusCode == http.StatusOK {
		t.Fatalf("download with empty expected_sha256 unexpectedly succeeded: %s", string(dlRaw))
	}

	// 4. Rename and delete spaced file must work using exact paths.
	r, renRaw := gc.do(t, http.MethodPost, "/api/gui/local/rename", map[string]string{
		"path":     spacedDir + "/" + spacedFile,
		"dst_dir":  spacedDir,
		"new_name": " renamed .txt ",
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("rename spaced file = %d %s, want 200", r.StatusCode, string(renRaw))
	}
	r, delRaw := gc.do(t, http.MethodPost, "/api/gui/local/delete", map[string]string{
		"path": spacedDir + "/ renamed .txt ",
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("delete spaced file = %d %s, want 200", r.StatusCode, string(delRaw))
	}
}

// Regression 2: Two independent browser sessions on the SAME GUI server instance must be isolated.
func TestRegression2_IsolatedBrowserSessionsOnSameGUIInstance(t *testing.T) {
	remote := startRemoteServer(t)
	localDir := t.TempDir()
	bigPayload := bytes.Repeat([]byte("ISOLATION_TEST_DATA_"), 200)
	if err := os.WriteFile(filepath.Join(localDir, "alice.bin"), bigPayload, 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := gui.NewServer(gui.Config{ServerURL: remote.srv.URL, LocalDir: localDir})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(g.Handler())
	defer ts.Close()

	// Two independent browser cookie jars hitting the SAME GUI server instance.
	clientA, csrfA := newBrowserJarClient(t, ts.URL)
	clientB, csrfB := newBrowserJarClient(t, ts.URL)
	if csrfA == csrfB {
		t.Fatalf("independent browser sessions share identical CSRF token %q", csrfA)
	}

	// Register alice and bob, log in alice in browser A and bob in browser B.
	doWithJar(t, clientA, csrfA, ts.URL, http.MethodPost, "/api/gui/auth/register", map[string]string{"username": "alice", "password": "password123"})
	doWithJar(t, clientB, csrfB, ts.URL, http.MethodPost, "/api/gui/auth/register", map[string]string{"username": "bob", "password": "password123"})

	rA, _ := doWithJar(t, clientA, csrfA, ts.URL, http.MethodPost, "/api/gui/auth/login", map[string]string{"username": "alice", "password": "password123"})
	if rA.StatusCode != http.StatusOK {
		t.Fatalf("login alice = %d", rA.StatusCode)
	}
	rB, _ := doWithJar(t, clientB, csrfB, ts.URL, http.MethodPost, "/api/gui/auth/login", map[string]string{"username": "bob", "password": "password123"})
	if rB.StatusCode != http.StatusOK {
		t.Fatalf("login bob = %d", rB.StatusCode)
	}

	// Browser A must STILL be logged in as alice (not overwritten by bob!).
	_, stA := doWithJar(t, clientA, csrfA, ts.URL, http.MethodGet, "/api/gui/status", nil)
	if !strings.Contains(string(stA), `"username":"alice"`) {
		t.Fatalf("browser A status after bob login = %s, want alice", string(stA))
	}
	_, stB := doWithJar(t, clientB, csrfB, ts.URL, http.MethodGet, "/api/gui/status", nil)
	if !strings.Contains(string(stB), `"username":"bob"`) {
		t.Fatalf("browser B status = %s, want bob", string(stB))
	}

	// Alice starts a resumable upload job; Bob must NOT be able to poll or cancel Alice's job.
	rJob, jobRaw := doWithJar(t, clientA, csrfA, ts.URL, http.MethodPost, "/api/gui/transfer/upload-resumable", map[string]any{
		"local_path": "alice.bin", "stop_after_chunks": 1,
	})
	if rJob.StatusCode != http.StatusAccepted {
		t.Fatalf("alice start job = %d %s", rJob.StatusCode, string(jobRaw))
	}
	var job gui.UploadJobSnapshot
	_ = json.Unmarshal(jobRaw, &job)

	rBobPoll, _ := doWithJar(t, clientB, csrfB, ts.URL, http.MethodGet, "/api/gui/transfer/jobs/"+job.JobID, nil)
	if rBobPoll.StatusCode == http.StatusOK {
		t.Fatalf("bob polled alice's upload job %s and got 200, want 403/404", job.JobID)
	}
	rBobCancel, _ := doWithJar(t, clientB, csrfB, ts.URL, http.MethodPost, "/api/gui/transfer/jobs/"+job.JobID+"/cancel", nil)
	if rBobCancel.StatusCode == http.StatusOK {
		t.Fatalf("bob canceled alice's upload job %s and got 200, want 403/404", job.JobID)
	}

	// Logging out browser B must NOT log out browser A.
	doWithJar(t, clientB, csrfB, ts.URL, http.MethodPost, "/api/gui/auth/logout", nil)
	_, stAAfter := doWithJar(t, clientA, csrfA, ts.URL, http.MethodGet, "/api/gui/status", nil)
	if !strings.Contains(string(stAAfter), `"username":"alice"`) {
		t.Fatalf("browser A status after bob logout = %s, want alice still logged in", string(stAAfter))
	}
}

// Regression 3: Pagination for 101 items in remote list & search so item #101 is reachable.
func TestRegression3_GUIPagination101Items(t *testing.T) {
	remote := startRemoteServer(t)
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "seed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gc := startGUIServer(t, remote.srv.URL, localDir)
	gc.do(t, http.MethodPost, "/api/gui/auth/register", map[string]string{"username": "alice", "password": "password123"})
	gc.do(t, http.MethodPost, "/api/gui/auth/login", map[string]string{"username": "alice", "password": "password123"})

	// Create 101 remote folders via GUI API.
	for i := 0; i < 101; i++ {
		r, raw := gc.do(t, http.MethodPost, "/api/gui/remote/folders", map[string]any{
			"name": fmt.Sprintf("dir-%03d", i),
		})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create folder %d = %d %s", i, r.StatusCode, string(raw))
		}
	}

	// Paginate through all folders using folder_offset / folder_limit until folders_has_more == false.
	seen := make(map[string]bool)
	offset := 0
	for pages := 0; pages < 10; pages++ {
		r, raw := gc.do(t, http.MethodGet, fmt.Sprintf("/api/gui/remote/list?folder_offset=%d&folder_limit=50", offset), nil)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("list page %d = %d %s", pages, r.StatusCode, string(raw))
		}
		var out struct {
			Folders           []struct{ Name string } `json:"folders"`
			FoldersHasMore    bool                    `json:"folders_has_more"`
			FoldersNextOffset int                     `json:"folders_next_offset"`
		}
		_ = json.Unmarshal(raw, &out)
		for _, f := range out.Folders {
			seen[f.Name] = true
		}
		if !out.FoldersHasMore {
			break
		}
		offset = out.FoldersNextOffset
	}
	if len(seen) != 101 {
		t.Fatalf("paginated folders count = %d, want 101", len(seen))
	}

	// Also verify search pagination across 101 files.
	for i := 0; i < 101; i++ {
		r, raw := gc.do(t, http.MethodPost, "/api/gui/transfer/upload", map[string]any{
			"local_path": "seed.txt",
		})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("upload seed %d = %d %s", i, r.StatusCode, string(raw))
		}
	}
	seenFiles := make(map[string]bool)
	fileOffset := 0
	for pages := 0; pages < 10; pages++ {
		r, raw := gc.do(t, http.MethodGet, fmt.Sprintf("/api/gui/remote/search?q=seed&offset=%d&limit=50", fileOffset), nil)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("search page %d = %d %s", pages, r.StatusCode, string(raw))
		}
		var out struct {
			Files      []struct{ ID string } `json:"files"`
			HasMore    bool                  `json:"has_more"`
			NextOffset int                   `json:"next_offset"`
		}
		_ = json.Unmarshal(raw, &out)
		for _, f := range out.Files {
			seenFiles[f.ID] = true
		}
		if !out.HasMore {
			break
		}
		fileOffset = out.NextOffset
	}
	if len(seenFiles) != 101 {
		t.Fatalf("paginated search files count = %d, want 101", len(seenFiles))
	}
}

// Regression 4: GUI resumable upload via converged syncclient rejects mismatched source on resume.
func TestRegression4_GUIResumableUploadSourceMismatchRejected(t *testing.T) {
	remote := startRemoteServer(t)
	localDir := t.TempDir()
	orig := bytes.Repeat([]byte("CONVERGED_UPLOAD_"), 200)
	if err := os.WriteFile(filepath.Join(localDir, "data.bin"), orig, 0o644); err != nil {
		t.Fatal(err)
	}
	gc := startGUIServer(t, remote.srv.URL, localDir)
	gc.do(t, http.MethodPost, "/api/gui/auth/register", map[string]string{"username": "alice", "password": "password123"})
	gc.do(t, http.MethodPost, "/api/gui/auth/login", map[string]string{"username": "alice", "password": "password123"})

	r, raw := gc.do(t, http.MethodPost, "/api/gui/transfer/upload-resumable", map[string]any{
		"local_path": "data.bin", "stop_after_chunks": 1,
	})
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("start resumable = %d %s", r.StatusCode, string(raw))
	}
	var job gui.UploadJobSnapshot
	_ = json.Unmarshal(raw, &job)
	for i := 0; i < 50; i++ {
		_, jRaw := gc.do(t, http.MethodGet, "/api/gui/transfer/jobs/"+job.JobID, nil)
		_ = json.Unmarshal(jRaw, &job)
		if job.Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Mutate local file and attempt resume -> must fail with 400 ErrUploadSourceMismatch!
	if err := os.WriteFile(filepath.Join(localDir, "data.bin"), []byte("mutated"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2, raw2 := gc.do(t, http.MethodPost, "/api/gui/transfer/upload-resumable", map[string]any{
		"local_path": "data.bin", "session_id": job.SessionID,
	})
	if r2.StatusCode != http.StatusBadRequest {
		t.Fatalf("resume with mismatched source = %d %s, want 400", r2.StatusCode, string(raw2))
	}
	_ = context.Background()
	_ = sha256.New()
	_ = hex.EncodeToString
}
