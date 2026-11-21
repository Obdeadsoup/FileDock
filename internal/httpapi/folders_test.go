package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFolderTreeAndFilePatch(t *testing.T) {
	env := newTestEnv(t, t.TempDir())
	defer env.cleanup()
	alice := registerAndLogin(t, env.handler, "alice", "alice-password")
	bob := registerAndLogin(t, env.handler, "bob", "bob-password")

	request := func(method, path, token, body string, status int, result any) {
		t.Helper()
		req := setBearer(httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body)), token)
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		env.handler.ServeHTTP(resp, req)
		if resp.Code != status {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.Code, status, resp.Body.String())
		}
		if result != nil {
			if err := json.Unmarshal(resp.Body.Bytes(), result); err != nil {
				t.Fatal(err)
			}
		}
	}

	var parent, child, foreign folderResponse
	request("POST", "/folders", alice, `{"name":"parent"}`, 201, &parent)
	request("POST", "/folders", alice, `{"name":"parent"}`, 409, nil)
	request("POST", "/folders", bob, `{"name":"parent"}`, 201, &foreign)
	request("POST", "/folders", alice, `{"name":"child","parent_id":"`+parent.ID+`"}`, 201, &child)
	request("GET", "/folders?parent_id="+parent.ID, bob, "", 404, nil)
	request("PATCH", "/folders/"+parent.ID, alice, `{"parent_id":"`+child.ID+`"}`, 409, nil)
	request("DELETE", "/folders/"+parent.ID, alice, "", 409, nil)

	contentType, body := buildMultipartUpload(t, "file", "test.txt", []byte("file contents"))
	req := setBearer(httptest.NewRequest(http.MethodPost, "/api/v1/files?parent_id="+child.ID, body), alice)
	req.Header.Set("Content-Type", contentType)
	resp := httptest.NewRecorder()
	env.handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", resp.Code, resp.Body.String())
	}
	var uploaded fileResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	filePath := "/files/" + uploaded.ID
	request("PATCH", filePath, alice, `{"name":"discard.txt","parent_id":"`+foreign.ID+`"}`, 404, nil)
	var items []fileResponse
	request("GET", "/files?parent_id="+child.ID, alice, "", 200, &items)
	if len(items) != 1 || items[0].Name != uploaded.Name {
		t.Fatalf("failed move changed the listing: %+v", items)
	}
	var updated fileResponse
	request("PATCH", filePath, alice, `{"name":"renamed.txt"}`, 200, &updated)
	if updated.ParentID == nil || *updated.ParentID != child.ID || updated.Name != "renamed.txt" {
		t.Fatalf("rename did not preserve location: %+v", updated)
	}
	request("DELETE", "/folders/"+child.ID, alice, "", 409, nil)
	request("PATCH", filePath, alice, `{"parent_id":null}`, 200, &updated)
	if updated.ParentID != nil || updated.ID != uploaded.ID || updated.SHA256 != uploaded.SHA256 {
		t.Fatalf("move to root changed file identity: %+v", updated)
	}
	request("DELETE", "/folders/"+child.ID, alice, "", 204, nil)
}
