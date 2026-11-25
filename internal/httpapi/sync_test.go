package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/filedock/internal/files"
)

type mirrorState struct {
	folders map[string]folderResponse
	files   map[string]fileResponse
}

func newMirrorState() *mirrorState {
	return &mirrorState{
		folders: make(map[string]folderResponse),
		files:   make(map[string]fileResponse),
	}
}

func (m *mirrorState) apply(events []syncChangeResponse) {
	for _, ev := range events {
		switch ev.Type {
		case files.ChangeFolderUpsert:
			if ev.Folder != nil {
				m.folders[ev.ResourceID] = *ev.Folder
			}
		case files.ChangeFolderDelete:
			delete(m.folders, ev.ResourceID)
		case files.ChangeFileUpsert:
			if ev.File != nil {
				m.files[ev.ResourceID] = *ev.File
			}
		case files.ChangeFileDelete:
			delete(m.files, ev.ResourceID)
		}
	}
}

func TestSyncChangesTwoClientsPaginationRestartAndIsolation(t *testing.T) {
	dataDir := t.TempDir()
	env := newTestEnv(t, dataDir)
	h := env.handler

	// 1. Unauthenticated -> 401.
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/v1/sync/changes", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated sync status = %d, want 401", unauth.Code)
	}

	tokenA := registerAndLogin(t, h, "alice", "alice-pass")
	tokenB := registerAndLogin(t, h, "bob", "bob-pass")

	// 2. Invalid query params -> 400.
	for _, q := range []string{"?after=-1", "?after=abc", "?after=", "?limit=0", "?limit=-2", "?limit=xyz"} {
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, setBearer(httptest.NewRequest(http.MethodGet, "/api/v1/sync/changes"+q, nil), tokenA))
		if resp.Code != http.StatusBadRequest {
			t.Errorf("GET /api/v1/sync/changes%s status = %d, want 400", q, resp.Code)
		}
	}

	fetchSync := func(handler http.Handler, token string, after int64, limit int) (syncChangesPageResponse, string) {
		t.Helper()
		target := fmt.Sprintf("/api/v1/sync/changes?after=%d&limit=%d", after, limit)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, setBearer(httptest.NewRequest(http.MethodGet, target, nil), token))
		if resp.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", target, resp.Code, resp.Body.String())
		}
		raw := resp.Body.String()
		if strings.Contains(raw, "object_key") {
			t.Fatalf("sync response leaked object_key: %s", raw)
		}
		var page syncChangesPageResponse
		if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode sync page: %v", err)
		}
		return page, raw
	}

	// 3. Empty page preserves requested cursor and returns JSON [].
	emptyPage, rawEmpty := fetchSync(h, tokenA, 7, 10)
	if len(emptyPage.Items) != 0 || emptyPage.NextCursor != 7 || emptyPage.HasMore || !strings.Contains(rawEmpty, `"items":[]`) {
		t.Fatalf("empty sync page = %+v raw=%s", emptyPage, rawEmpty)
	}

	upload := func(handler http.Handler, token string, parentID *string, name string, payload []byte) fileResponse {
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
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusCreated {
			t.Fatalf("upload(%q) status = %d, body = %s", name, resp.Code, resp.Body.String())
		}
		var out fileResponse
		if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	patch := func(handler http.Handler, token, path, body string) {
		t.Helper()
		env.advance(time.Second)
		req := setBearer(httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body)), token)
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("PATCH %s status = %d, body = %s", path, resp.Code, resp.Body.String())
		}
	}

	del := func(handler http.Handler, token, path string) {
		t.Helper()
		env.advance(time.Second)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, setBearer(httptest.NewRequest(http.MethodDelete, path, nil), token))
		if resp.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s status = %d, body = %s", path, resp.Code, resp.Body.String())
		}
	}

	// 4. Alice performs folder create, upload, root move, rename, and delete; Bob performs his own writes.
	dirA := createFolderAPI(t, h, tokenA, `{"name":"work"}`)                          // seq 1
	subA := createFolderAPI(t, h, tokenA, `{"name":"sub","parent_id":"`+dirA.ID+`"}`) // seq 2
	f1 := upload(h, tokenA, &subA.ID, "notes.txt", []byte("notes v1"))                // seq 3
	f2 := upload(h, tokenA, nil, "temp.txt", []byte("temp"))                          // seq 4
	_ = createFolderAPI(t, h, tokenB, `{"name":"bob-only"}`)                          // Bob seq 1
	_ = upload(h, tokenB, nil, "bob.txt", []byte("bob"))                              // Bob seq 2

	// Client 2 syncs early up to cursor after seq 4.
	client2 := newMirrorState()
	c2Page1, _ := fetchSync(h, tokenA, 0, 50)
	if len(c2Page1.Items) != 4 || c2Page1.NextCursor != 4 || c2Page1.HasMore {
		t.Fatalf("client2 initial sync = (len=%d, next=%d, more=%v), want (4, 4, false)", len(c2Page1.Items), c2Page1.NextCursor, c2Page1.HasMore)
	}
	client2.apply(c2Page1.Items)
	c2Cursor := c2Page1.NextCursor

	// Move subA to root (only subA emits folder.upsert; f1 still references subA.ID without extra event).
	patch(h, tokenA, "/api/v1/folders/"+subA.ID, `{"parent_id":null}`) // seq 5
	// Rename f1 and delete f2, then delete now-empty dirA.
	patch(h, tokenA, "/api/v1/files/"+f1.ID, `{"name":"notes-final.txt"}`) // seq 6
	del(h, tokenA, "/api/v1/files/"+f2.ID)                                 // seq 7
	del(h, tokenA, "/api/v1/folders/"+dirA.ID)                             // seq 8

	// 5. Client 1 starts from cursor 0 with limit=2; insert a NEW file mid-pagination to verify no skipped events.
	client1 := newMirrorState()
	c1Cursor := int64(0)
	p1, _ := fetchSync(h, tokenA, c1Cursor, 2)
	if len(p1.Items) != 2 || !p1.HasMore || p1.NextCursor != 2 {
		t.Fatalf("client1 page1 = (len=%d, next=%d, more=%v), want (2, 2, true)", len(p1.Items), p1.NextCursor, p1.HasMore)
	}
	// Repeated read at cursor 0 returns the exact same page.
	p1Repeat, _ := fetchSync(h, tokenA, 0, 2)
	if !reflect.DeepEqual(p1, p1Repeat) {
		t.Fatalf("repeated read mismatch: %+v vs %+v", p1, p1Repeat)
	}
	client1.apply(p1.Items)
	c1Cursor = p1.NextCursor

	// Insert new file while Client 1 is mid-pagination!
	f3 := upload(h, tokenA, nil, "concurrent.txt", []byte("arrived during pagination")) // seq 9

	for {
		page, _ := fetchSync(h, tokenA, c1Cursor, 2)
		client1.apply(page.Items)
		c1Cursor = page.NextCursor
		if !page.HasMore {
			break
		}
	}
	if c1Cursor != 9 {
		t.Fatalf("client1 final cursor = %d, want 9", c1Cursor)
	}

	// 6. Simulate server restart; Client 2 resumes from c2Cursor=4 and catches up to seq 9.
	env.cleanup()
	env2 := newTestEnv(t, dataDir)
	defer env2.cleanup()
	h2 := env2.handler

	for {
		page, _ := fetchSync(h2, tokenA, c2Cursor, 3)
		client2.apply(page.Items)
		c2Cursor = page.NextCursor
		if !page.HasMore {
			break
		}
	}
	if c2Cursor != 9 {
		t.Fatalf("client2 final cursor after restart = %d, want 9", c2Cursor)
	}

	// 7. Both clients must converge to the exact same final state:
	//    folders: {subA (parent_id=nil)}
	//    files:   {f1 (name="notes-final.txt", parent_id=subA.ID), f3 (name="concurrent.txt", parent_id=nil)}
	if !reflect.DeepEqual(client1, client2) {
		t.Fatalf("client1 and client2 diverged:\nc1=%+v\nc2=%+v", client1, client2)
	}
	if len(client1.folders) != 1 || client1.folders[subA.ID].ParentID != nil {
		t.Fatalf("unexpected final folders: %+v", client1.folders)
	}
	if len(client1.files) != 2 || client1.files[f1.ID].Name != "notes-final.txt" ||
		client1.files[f1.ID].ParentID == nil || *client1.files[f1.ID].ParentID != subA.ID ||
		client1.files[f3.ID].Name != "concurrent.txt" || client1.files[f3.ID].ParentID != nil {
		t.Fatalf("unexpected final files: %+v", client1.files)
	}

	// 8. Verify cross-account isolation: Bob's stream has only Bob's 2 events starting at seq 1.
	bobPage, _ := fetchSync(h2, tokenB, 0, 50)
	if len(bobPage.Items) != 2 || bobPage.Items[0].Seq != 1 || bobPage.Items[1].Seq != 2 || bobPage.HasMore {
		t.Fatalf("bob sync page = %+v, want 2 isolated events seq 1..2", bobPage)
	}
}
