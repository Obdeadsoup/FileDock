package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSharesThreeAccountsSubtreeLifecycleAndRestart(t *testing.T) {
	dataDir := t.TempDir()
	env := newTestEnv(t, dataDir)
	h := env.handler

	// 1. Unauthenticated share endpoints return 401.
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/v1/shares", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodGet, "/api/v1/shares?direction=received", nil),
		httptest.NewRequest(http.MethodDelete, "/api/v1/shares/share-1", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/share-1/folders", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/share-1/files", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/share-1/files/file-1/content", nil),
	} {
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token status = %d, want 401", req.Method, req.URL.Path, resp.Code)
		}
	}

	tokenA := registerAndLogin(t, h, "alice", "alice-secret")
	tokenB := registerAndLogin(t, h, "bob", "bob-secret")
	tokenC := registerAndLogin(t, h, "carol", "carol-secret")

	upload := func(token string, parentID *string, name string, payload []byte) fileResponse {
		t.Helper()
		env.advance(time.Second)
		ct, body := buildMultipartUpload(t, "file", name, payload)
		target := "/api/v1/files"
		if parentID != nil {
			target += "?parent_id=" + url.QueryEscape(*parentID)
		}
		req := setBearer(httptest.NewRequest(http.MethodPost, target, body), token)
		req.Header.Set("Content-Type", ct)
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusCreated {
			t.Fatalf("upload(%q) status = %d, body = %s", name, resp.Code, resp.Body.String())
		}
		var out fileResponse
		if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	createShare := func(token, body string) (int, shareResponse) {
		t.Helper()
		env.advance(time.Second)
		req := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/shares", strings.NewReader(body)), token)
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		var out shareResponse
		if resp.Code == http.StatusCreated || resp.Code == http.StatusOK {
			if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode share: %v", err)
			}
		}
		return resp.Code, out
	}

	// Alice builds a multi-level folder tree:
	//   privateParent -> sharedRoot -> sub1 -> sub2
	//   otherDir (outside sharedRoot)
	privateParent := createFolderAPI(t, h, tokenA, `{"name":"private-parent"}`)
	sharedRoot := createFolderAPI(t, h, tokenA, `{"name":"shared-root","parent_id":"`+privateParent.ID+`"}`)
	sub1 := createFolderAPI(t, h, tokenA, `{"name":"level-1","parent_id":"`+sharedRoot.ID+`"}`)
	sub2 := createFolderAPI(t, h, tokenA, `{"name":"level-2","parent_id":"`+sub1.ID+`"}`)
	otherDir := createFolderAPI(t, h, tokenA, `{"name":"other-private"}`)

	standalonePayload := []byte("standalone shared file content")
	standaloneFile := upload(tokenA, &otherDir.ID, "standalone.txt", standalonePayload)
	rootFilePayload := []byte("direct file in shared root")
	rootFile := upload(tokenA, &sharedRoot.ID, "root-doc.txt", rootFilePayload)
	deepPayload := []byte("deep nested file in level-2")
	deepFile := upload(tokenA, &sub2.ID, "deep.txt", deepPayload)
	outsidePayload := []byte("outside file later moved in")
	outsideFile := upload(tokenA, &otherDir.ID, "outside.txt", outsidePayload)

	// 2. Alice shares standaloneFile and sharedRoot with Bob; duplicate returns existing share.
	code, fileShare := createShare(tokenA, `{"resource_type":"file","resource_id":"`+standaloneFile.ID+`","recipient_username":"bob"}`)
	if code != http.StatusCreated || fileShare.ID == "" || fileShare.Permission != "read" || fileShare.RecipientUsername != "bob" {
		t.Fatalf("create file share = (%d, %+v)", code, fileShare)
	}
	dupCode, dupFileShare := createShare(tokenA, `{"resource_type":"file","resource_id":"`+standaloneFile.ID+`","recipient_username":"bob"}`)
	if dupCode != http.StatusOK || dupFileShare.ID != fileShare.ID {
		t.Fatalf("duplicate file share = (%d, %+v), want (200, id=%s)", dupCode, dupFileShare, fileShare.ID)
	}

	code, folderShare := createShare(tokenA, `{"resource_type":"folder","resource_id":"`+sharedRoot.ID+`","recipient_username":"bob"}`)
	if code != http.StatusCreated || folderShare.ID == "" || folderShare.ResourceType != "folder" {
		t.Fatalf("create folder share = (%d, %+v)", code, folderShare)
	}

	// Self-share -> 400; unknown recipient -> 404; non-owner share -> 404.
	if c, _ := createShare(tokenA, `{"resource_type":"file","resource_id":"`+standaloneFile.ID+`","recipient_username":"alice"}`); c != http.StatusBadRequest {
		t.Fatalf("self share status = %d, want 400", c)
	}
	if c, _ := createShare(tokenA, `{"resource_type":"file","resource_id":"`+standaloneFile.ID+`","recipient_username":"nobody"}`); c != http.StatusNotFound {
		t.Fatalf("unknown recipient share status = %d, want 404", c)
	}
	if c, _ := createShare(tokenB, `{"resource_type":"file","resource_id":"`+standaloneFile.ID+`","recipient_username":"carol"}`); c != http.StatusNotFound {
		t.Fatalf("non-owner share status = %d, want 404", c)
	}

	// 3. List sent/received shares with pagination.
	listSharesAPI := func(handler http.Handler, token, query string) []shareResponse {
		t.Helper()
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares?"+query, nil), token))
		if resp.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/shares?%s status = %d, body = %s", query, resp.Code, resp.Body.String())
		}
		var items []shareResponse
		if err := json.Unmarshal(resp.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		return items
	}

	sentA := listSharesAPI(h, tokenA, "direction=sent")
	recvB := listSharesAPI(h, tokenB, "direction=received")
	recvC := listSharesAPI(h, tokenC, "direction=received")
	if len(sentA) != 2 || len(recvB) != 2 || len(recvC) != 0 {
		t.Fatalf("share counts sentA=%d recvB=%d recvC=%d, want 2/2/0", len(sentA), len(recvB), len(recvC))
	}
	pagedRecvB := listSharesAPI(h, tokenB, "direction=received&limit=1&offset=1")
	if len(pagedRecvB) != 1 || pagedRecvB[0].ID != fileShare.ID {
		t.Fatalf("paged recvB = %+v, want [%s]", pagedRecvB, fileShare.ID)
	}

	// 4. Bob downloads standalone shared file; response never exposes external parent_id or object_key.
	dlStandalone := httptest.NewRecorder()
	h.ServeHTTP(dlStandalone, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+fileShare.ID+"/files/"+standaloneFile.ID+"/content", nil), tokenB))
	if dlStandalone.Code != http.StatusOK || !bytes.Equal(dlStandalone.Body.Bytes(), standalonePayload) {
		t.Fatalf("Bob download standalone = (%d, %q)", dlStandalone.Code, dlStandalone.Body.String())
	}

	// 5. Bob browses multi-level shared folder subtree (root -> sub1 -> sub2) and downloads deep.txt.
	rootFoldersResp := httptest.NewRecorder()
	h.ServeHTTP(rootFoldersResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/folders", nil), tokenB))
	if rootFoldersResp.Code != http.StatusOK {
		t.Fatalf("Bob list shared root folders status = %d, body = %s", rootFoldersResp.Code, rootFoldersResp.Body.String())
	}
	if strings.Contains(rootFoldersResp.Body.String(), privateParent.ID) || strings.Contains(rootFoldersResp.Body.String(), "object_key") {
		t.Fatalf("shared folder response leaked external parent or object_key: %s", rootFoldersResp.Body.String())
	}
	var sharedRootFolders []folderResponse
	if err := json.Unmarshal(rootFoldersResp.Body.Bytes(), &sharedRootFolders); err != nil {
		t.Fatal(err)
	}
	if len(sharedRootFolders) != 1 || sharedRootFolders[0].ID != sub1.ID || sharedRootFolders[0].ParentID != nil {
		t.Fatalf("sharedRootFolders = %+v, want [id=%s, parent_id=nil]", sharedRootFolders, sub1.ID)
	}

	sub1FoldersResp := httptest.NewRecorder()
	h.ServeHTTP(sub1FoldersResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/folders?parent_id="+sub1.ID, nil), tokenB))
	if sub1FoldersResp.Code != http.StatusOK {
		t.Fatalf("Bob list sub1 folders status = %d", sub1FoldersResp.Code)
	}
	var sub1Folders []folderResponse
	if err := json.Unmarshal(sub1FoldersResp.Body.Bytes(), &sub1Folders); err != nil {
		t.Fatal(err)
	}
	if len(sub1Folders) != 1 || sub1Folders[0].ID != sub2.ID {
		t.Fatalf("sub1Folders = %+v, want [%s]", sub1Folders, sub2.ID)
	}

	sub2FilesResp := httptest.NewRecorder()
	h.ServeHTTP(sub2FilesResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/files?parent_id="+sub2.ID, nil), tokenB))
	if sub2FilesResp.Code != http.StatusOK {
		t.Fatalf("Bob list sub2 files status = %d", sub2FilesResp.Code)
	}
	var sub2Files []fileResponse
	if err := json.Unmarshal(sub2FilesResp.Body.Bytes(), &sub2Files); err != nil {
		t.Fatal(err)
	}
	if len(sub2Files) != 1 || sub2Files[0].ID != deepFile.ID {
		t.Fatalf("sub2Files = %+v, want [%s]", sub2Files, deepFile.ID)
	}

	dlDeep := httptest.NewRecorder()
	h.ServeHTTP(dlDeep, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/files/"+deepFile.ID+"/content", nil), tokenB))
	if dlDeep.Code != http.StatusOK || !bytes.Equal(dlDeep.Body.Bytes(), deepPayload) {
		t.Fatalf("Bob download deep.txt = (%d, %q)", dlDeep.Code, dlDeep.Body.String())
	}

	// 6. Carol holding Bob's share_ids cannot browse, download, or revoke (all 404).
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/folders", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/files", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/files/"+deepFile.ID+"/content", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+fileShare.ID+"/files/"+standaloneFile.ID+"/content", nil),
		httptest.NewRequest(http.MethodDelete, "/api/v1/shares/"+folderShare.ID, nil),
	} {
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, setBearer(req, tokenC))
		if resp.Code != http.StatusNotFound {
			t.Fatalf("Carol %s %s status = %d, want 404", req.Method, req.URL.Path, resp.Code)
		}
	}

	// 7. Bob cannot modify or delete Alice's resources via normal APIs (all 404) or revoke Alice's share (404).
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPatch, "/api/v1/files/"+standaloneFile.ID, strings.NewReader(`{"name":"hacked.txt"}`)),
		httptest.NewRequest(http.MethodDelete, "/api/v1/files/"+standaloneFile.ID, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/files/"+standaloneFile.ID+"/content", nil),
		httptest.NewRequest(http.MethodPatch, "/api/v1/folders/"+sharedRoot.ID, strings.NewReader(`{"name":"hacked"}`)),
		httptest.NewRequest(http.MethodDelete, "/api/v1/folders/"+sharedRoot.ID, nil),
		httptest.NewRequest(http.MethodDelete, "/api/v1/shares/"+folderShare.ID, nil),
	} {
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, setBearer(req, tokenB))
		if resp.Code != http.StatusNotFound {
			t.Fatalf("Bob %s %s status = %d, want 404", req.Method, req.URL.Path, resp.Code)
		}
	}

	// 8. Cross-subtree access via folderShare or fileShare returns 404.
	for _, path := range []string{
		"/api/v1/shares/" + folderShare.ID + "/folders?parent_id=" + privateParent.ID,
		"/api/v1/shares/" + folderShare.ID + "/folders?parent_id=" + otherDir.ID,
		"/api/v1/shares/" + folderShare.ID + "/files?parent_id=" + otherDir.ID,
		"/api/v1/shares/" + folderShare.ID + "/files/" + outsideFile.ID + "/content",
		"/api/v1/shares/" + fileShare.ID + "/files/" + rootFile.ID + "/content",
	} {
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, setBearer(httptest.NewRequest(http.MethodGet, path, nil), tokenB))
		if resp.Code != http.StatusNotFound {
			t.Fatalf("cross-subtree %s status = %d, want 404", path, resp.Code)
		}
	}

	// 9. Rename & move sharedRoot and standaloneFile -> shares remain valid.
	patchAPI := func(path, body string) {
		t.Helper()
		req := setBearer(httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body)), tokenA)
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("PATCH %s status = %d, body = %s", path, resp.Code, resp.Body.String())
		}
	}
	patchAPI("/api/v1/files/"+standaloneFile.ID, `{"name":"standalone-renamed.txt","parent_id":null}`)
	patchAPI("/api/v1/folders/"+sharedRoot.ID, `{"name":"shared-root-renamed","parent_id":"`+otherDir.ID+`"}`)

	// Move outsideFile INTO sub2 -> Bob can now download it via folderShare.
	patchAPI("/api/v1/files/"+outsideFile.ID, `{"parent_id":"`+sub2.ID+`"}`)
	dlMovedIn := httptest.NewRecorder()
	h.ServeHTTP(dlMovedIn, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/files/"+outsideFile.ID+"/content", nil), tokenB))
	if dlMovedIn.Code != http.StatusOK || !bytes.Equal(dlMovedIn.Body.Bytes(), outsidePayload) {
		t.Fatalf("Bob download moved-in file = (%d, %q)", dlMovedIn.Code, dlMovedIn.Body.String())
	}

	// Move deepFile OUT of sub2 to root -> Bob can no longer download it via folderShare (404).
	patchAPI("/api/v1/files/"+deepFile.ID, `{"parent_id":null}`)
	dlMovedOut := httptest.NewRecorder()
	h.ServeHTTP(dlMovedOut, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/files/"+deepFile.ID+"/content", nil), tokenB))
	if dlMovedOut.Code != http.StatusNotFound {
		t.Fatalf("Bob download moved-out file status = %d, want 404", dlMovedOut.Code)
	}

	// 10. Simulate server restart on the same dataDir -> shares still work.
	env.cleanup()
	env2 := newTestEnv(t, dataDir)
	defer env2.cleanup()
	h2 := env2.handler

	dlAfterRestart := httptest.NewRecorder()
	h2.ServeHTTP(dlAfterRestart, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+fileShare.ID+"/files/"+standaloneFile.ID+"/content", nil), tokenB))
	if dlAfterRestart.Code != http.StatusOK || !bytes.Equal(dlAfterRestart.Body.Bytes(), standalonePayload) {
		t.Fatalf("Bob download after restart = (%d, %q)", dlAfterRestart.Code, dlAfterRestart.Body.String())
	}

	// 11. Already-started download finishes even if share is revoked mid-stream; subsequent requests return 404.
	bobUser, err := env2.accountSvc.Authenticate(context.Background(), tokenB)
	if err != nil {
		t.Fatal(err)
	}
	_, openStream, err := env2.fileSvc.OpenSharedContent(context.Background(), bobUser.ID, folderShare.ID, outsideFile.ID)
	if err != nil {
		t.Fatalf("OpenSharedContent() error = %v", err)
	}

	revokeResp := httptest.NewRecorder()
	h2.ServeHTTP(revokeResp, setBearer(httptest.NewRequest(http.MethodDelete, "/api/v1/shares/"+folderShare.ID, nil), tokenA))
	if revokeResp.Code != http.StatusNoContent {
		t.Fatalf("revoke folder share status = %d, want 204", revokeResp.Code)
	}

	inFlightBytes, err := io.ReadAll(openStream)
	_ = openStream.Close()
	if err != nil || !bytes.Equal(inFlightBytes, outsidePayload) {
		t.Fatalf("in-flight stream after revocation failed: err=%v len=%d", err, len(inFlightBytes))
	}

	afterRevoke := httptest.NewRecorder()
	h2.ServeHTTP(afterRevoke, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+folderShare.ID+"/files/"+outsideFile.ID+"/content", nil), tokenB))
	if afterRevoke.Code != http.StatusNotFound {
		t.Fatalf("download after revocation status = %d, want 404", afterRevoke.Code)
	}

	// 12. Deleting shared file clears its share in the same transaction AND preserves blob_cleanups.
	delFileResp := httptest.NewRecorder()
	h2.ServeHTTP(delFileResp, setBearer(httptest.NewRequest(http.MethodDelete, "/api/v1/files/"+standaloneFile.ID, nil), tokenA))
	if delFileResp.Code != http.StatusNoContent {
		t.Fatalf("delete shared file status = %d, want 204", delFileResp.Code)
	}
	if got := listSharesAPI(h2, tokenB, "direction=received"); len(got) != 0 {
		t.Fatalf("expected 0 received shares after revoke+delete, got %+v", got)
	}

	acked, err := env2.fileSvc.ProcessBlobCleanups(context.Background(), 10)
	if err != nil || acked != 1 {
		t.Fatalf("ProcessBlobCleanups() after deleting shared file = (%d, %v), want (1, nil)", acked, err)
	}
	objEntries, err := os.ReadDir(filepath.Join(dataDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objEntries) != 3 {
		t.Fatalf("objects on disk = %d, want 3 after deleting 1 of 4 files", len(objEntries))
	}

	// Verify deleting an empty shared folder also removes its share atomically.
	emptySharedDir := createFolderAPI(t, h2, tokenA, `{"name":"empty-shared"}`)
	reqShare := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/shares", strings.NewReader(`{"resource_type":"folder","resource_id":"`+emptySharedDir.ID+`","recipient_username":"bob"}`)), tokenA)
	reqShare.Header.Set("Content-Type", "application/json")
	respShare := httptest.NewRecorder()
	h2.ServeHTTP(respShare, reqShare)
	if respShare.Code != http.StatusCreated {
		t.Fatalf("share empty folder status = %d", respShare.Code)
	}
	delDirResp := httptest.NewRecorder()
	h2.ServeHTTP(delDirResp, setBearer(httptest.NewRequest(http.MethodDelete, "/api/v1/folders/"+emptySharedDir.ID, nil), tokenA))
	if delDirResp.Code != http.StatusNoContent {
		t.Fatalf("delete empty shared folder status = %d, want 204", delDirResp.Code)
	}
	if got := listSharesAPI(h2, tokenB, "direction=received"); len(got) != 0 {
		t.Fatalf("expected 0 received shares after deleting empty shared folder, got %+v", got)
	}

	// Sanity check SHA-256 helper usage on standalone payload.
	wantSum := sha256.Sum256(standalonePayload)
	if hex.EncodeToString(wantSum[:]) != standaloneFile.SHA256 {
		t.Fatal("standalone sha256 mismatch")
	}
}
