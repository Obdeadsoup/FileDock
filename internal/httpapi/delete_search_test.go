package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileDeleteAndSearchIsolation(t *testing.T) {
	dataDir := t.TempDir()
	env := newTestEnv(t, dataDir)
	defer env.cleanup()
	h := env.handler

	// Unauthenticated DELETE and search return 401.
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodDelete, "/api/v1/files/some-id", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/files/search?q=test", nil),
	} {
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token status = %d, want 401", req.Method, req.URL.Path, resp.Code)
		}
	}

	tokenA := registerAndLogin(t, h, "alice", "alice-secret")
	tokenB := registerAndLogin(t, h, "bob", "bob-secret")

	// Create folders for Alice and Bob.
	folderA := createFolderAPI(t, h, tokenA, `{"name":"工作资料"}`)
	subFolderA := createFolderAPI(t, h, tokenA, `{"name":"子目录","parent_id":"`+folderA.ID+`"}`)
	folderB := createFolderAPI(t, h, tokenB, `{"name":"bob-dir"}`)

	// Upload files with controlled timestamps for stable ordering and diverse names/sizes.
	uploadWith := func(token string, parentID *string, name string, payload []byte) fileResponse {
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
			t.Fatalf("decode upload(%q): %v", name, err)
		}
		return out
	}

	fRootSmall := uploadWith(tokenA, nil, "Report_100%.txt", bytes.Repeat([]byte("a"), 10))
	fDirLower := uploadWith(tokenA, &folderA.ID, "report_1000.txt", bytes.Repeat([]byte("b"), 25))
	fDirChinese := uploadWith(tokenA, &folderA.ID, "实验报告_50%.pdf", bytes.Repeat([]byte("c"), 50))
	fSubNested := uploadWith(tokenA, &subFolderA.ID, "实验报告_草稿.txt", bytes.Repeat([]byte("d"), 100))
	fBob := uploadWith(tokenB, &folderB.ID, "实验报告_50%.pdf", bytes.Repeat([]byte("e"), 50))

	searchAPI := func(token, rawQuery string) (int, []fileResponse) {
		t.Helper()
		target := "/api/v1/files/search"
		if rawQuery != "" {
			target += "?" + rawQuery
		}
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, setBearer(httptest.NewRequest(http.MethodGet, target, nil), token))
		if resp.Code != http.StatusOK {
			return resp.Code, nil
		}
		var items []fileResponse
		if err := json.Unmarshal(resp.Body.Bytes(), &items); err != nil {
			t.Fatalf("decode search(%q): %v (body=%s)", rawQuery, err, resp.Body.String())
		}
		return resp.Code, items
	}

	// 1. Default search covers all directories of Alice and excludes Bob's file.
	code, allA := searchAPI(tokenA, "")
	if code != http.StatusOK || len(allA) != 4 {
		t.Fatalf("all Alice search = (%d, len=%d), want (200, 4)", code, len(allA))
	}
	// Stable ordering: newest first (fSubNested, fDirChinese, fDirLower, fRootSmall).
	if allA[0].ID != fSubNested.ID || allA[1].ID != fDirChinese.ID || allA[2].ID != fDirLower.ID || allA[3].ID != fRootSmall.ID {
		t.Fatalf("unexpected search order: %+v", allA)
	}

	// 2. Literal case-sensitive matching with % and _ treated as ordinary characters.
	_, upperOnly := searchAPI(tokenA, "q="+url.QueryEscape("Report"))
	if len(upperOnly) != 1 || upperOnly[0].ID != fRootSmall.ID {
		t.Fatalf("case-sensitive Report search = %+v, want [%s]", upperOnly, fRootSmall.ID)
	}
	_, literalPercent := searchAPI(tokenA, "q="+url.QueryEscape("100%"))
	if len(literalPercent) != 1 || literalPercent[0].ID != fRootSmall.ID {
		t.Fatalf("literal 100%% search = %+v, want [%s]", literalPercent, fRootSmall.ID)
	}
	_, literalUnderscore := searchAPI(tokenA, "q="+url.QueryEscape("_50%"))
	if len(literalUnderscore) != 1 || literalUnderscore[0].ID != fDirChinese.ID {
		t.Fatalf("literal _50%% search = %+v, want [%s]", literalUnderscore, fDirChinese.ID)
	}

	// 3. Chinese substring search across all folders of Alice (does not leak Bob's file).
	_, chineseAll := searchAPI(tokenA, "q="+url.QueryEscape("实验报告"))
	if len(chineseAll) != 2 || chineseAll[0].ID != fSubNested.ID || chineseAll[1].ID != fDirChinese.ID {
		t.Fatalf("Chinese search = %+v, want [%s, %s]", chineseAll, fSubNested.ID, fDirChinese.ID)
	}

	// 4. Size filtering (inclusive min_size and max_size) + pagination.
	_, sized := searchAPI(tokenA, "min_size=25&max_size=50")
	if len(sized) != 2 || sized[0].ID != fDirChinese.ID || sized[1].ID != fDirLower.ID {
		t.Fatalf("size [25..50] search = %+v, want [%s, %s]", sized, fDirChinese.ID, fDirLower.ID)
	}
	_, paged := searchAPI(tokenA, "limit=2&offset=1")
	if len(paged) != 2 || paged[0].ID != fDirChinese.ID || paged[1].ID != fDirLower.ID {
		t.Fatalf("paginated search = %+v, want [%s, %s]", paged, fDirChinese.ID, fDirLower.ID)
	}

	// 5. parent_id restricts search to direct files of that folder only; unknown or other user's folder -> 404.
	_, inFolderA := searchAPI(tokenA, "parent_id="+url.QueryEscape(folderA.ID)+"&q="+url.QueryEscape("实验报告"))
	if len(inFolderA) != 1 || inFolderA[0].ID != fDirChinese.ID {
		t.Fatalf("parent_id direct search = %+v, want [%s]", inFolderA, fDirChinese.ID)
	}
	if code, _ := searchAPI(tokenA, "parent_id=unknown-folder"); code != http.StatusNotFound {
		t.Fatalf("search unknown parent_id status = %d, want 404", code)
	}
	if code, _ := searchAPI(tokenA, "parent_id="+url.QueryEscape(folderB.ID)); code != http.StatusNotFound {
		t.Fatalf("search Bob's parent_id as Alice status = %d, want 404", code)
	}

	// 6. Invalid search query params -> 400.
	for _, badQuery := range []string{"min_size=-1", "max_size=abc", "min_size=100&max_size=50", "limit=0", "offset=-1"} {
		if code, _ := searchAPI(tokenA, badQuery); code != http.StatusBadRequest {
			t.Errorf("search(%q) status = %d, want 400", badQuery, code)
		}
	}

	// 7. Original GET /api/v1/files without parent_id still lists only root files.
	rootListResp := httptest.NewRecorder()
	h.ServeHTTP(rootListResp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files", nil), tokenA))
	if rootListResp.Code != http.StatusOK {
		t.Fatalf("root list status = %d", rootListResp.Code)
	}
	var rootFiles []fileResponse
	if err := json.Unmarshal(rootListResp.Body.Bytes(), &rootFiles); err != nil {
		t.Fatal(err)
	}
	if len(rootFiles) != 1 || rootFiles[0].ID != fRootSmall.ID {
		t.Fatalf("root list = %+v, want [%s]", rootFiles, fRootSmall.ID)
	}

	// 8. Cross-user delete and unknown file delete return 404.
	for _, target := range []string{"/api/v1/files/" + fSubNested.ID, "/api/v1/files/does-not-exist"} {
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, setBearer(httptest.NewRequest(http.MethodDelete, target, nil), tokenB))
		if resp.Code != http.StatusNotFound {
			t.Fatalf("Bob DELETE %s status = %d, want 404", target, resp.Code)
		}
	}

	// 9. Deleting non-empty subfolder fails with 409 before deleting its file.
	delNonEmpty := httptest.NewRecorder()
	h.ServeHTTP(delNonEmpty, setBearer(httptest.NewRequest(http.MethodDelete, "/api/v1/folders/"+subFolderA.ID, nil), tokenA))
	if delNonEmpty.Code != http.StatusConflict {
		t.Fatalf("DELETE non-empty folder status = %d, want 409", delNonEmpty.Code)
	}

	// 10. Alice deletes fSubNested -> 204; immediately absent from list/search, download returns 404, subfolder now deletable.
	delFileResp := httptest.NewRecorder()
	h.ServeHTTP(delFileResp, setBearer(httptest.NewRequest(http.MethodDelete, "/api/v1/files/"+fSubNested.ID, nil), tokenA))
	if delFileResp.Code != http.StatusNoContent {
		t.Fatalf("DELETE file status = %d, want 204 (body=%s)", delFileResp.Code, delFileResp.Body.String())
	}

	dlDeleted := httptest.NewRecorder()
	h.ServeHTTP(dlDeleted, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+fSubNested.ID+"/content", nil), tokenA))
	if dlDeleted.Code != http.StatusNotFound {
		t.Fatalf("download deleted file status = %d, want 404", dlDeleted.Code)
	}

	_, afterDeleteSearch := searchAPI(tokenA, "q="+url.QueryEscape("草稿"))
	if len(afterDeleteSearch) != 0 {
		t.Fatalf("deleted file still returned in search: %+v", afterDeleteSearch)
	}

	delEmptyFolder := httptest.NewRecorder()
	h.ServeHTTP(delEmptyFolder, setBearer(httptest.NewRequest(http.MethodDelete, "/api/v1/folders/"+subFolderA.ID, nil), tokenA))
	if delEmptyFolder.Code != http.StatusNoContent {
		t.Fatalf("DELETE now-empty folder status = %d, want 204 (body=%s)", delEmptyFolder.Code, delEmptyFolder.Body.String())
	}

	// Bob's file is still intact and searchable only by Bob.
	_, bobSearch := searchAPI(tokenB, "q="+url.QueryEscape("实验报告"))
	if len(bobSearch) != 1 || bobSearch[0].ID != fBob.ID {
		t.Fatalf("Bob search = %+v, want [%s]", bobSearch, fBob.ID)
	}

	// Process queued blob cleanup and verify the object count on disk drops from 5 to 4.
	acked, err := env.fileSvc.ProcessBlobCleanups(context.Background(), 10)
	if err != nil || acked != 1 {
		t.Fatalf("ProcessBlobCleanups() = (%d, %v), want (1, nil)", acked, err)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("objects on disk = %d, want 4 after deleting 1 of 5 files", len(entries))
	}
}

func createFolderAPI(t *testing.T, h http.Handler, token, body string) folderResponse {
	t.Helper()
	req := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/folders", strings.NewReader(body)), token)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("createFolder(%s) status = %d, body = %s", body, resp.Code, resp.Body.String())
	}
	var out folderResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode folder: %v", err)
	}
	return out
}
