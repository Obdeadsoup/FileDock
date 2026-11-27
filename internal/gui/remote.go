package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/filedock/internal/syncclient"
)

const maxGUIJSONBody = 16 << 20

var ErrRemoteNotLoggedIn = errors.New("not logged in to remote FileDock server")

type RemoteError struct {
	Status  int
	Message string
}

func (e *RemoteError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("remote server returned HTTP %d", e.Status)
}

type RemoteUser struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

type RemoteFolder struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ParentID  *string   `json:"parent_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type RemoteFile struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	ParentID        *string   `json:"parent_id"`
	Size            int64     `json:"size"`
	SHA256          string    `json:"sha256"`
	ContentRevision int64     `json:"content_revision"`
	CreatedAt       time.Time `json:"created_at"`
}

type RemoteShare struct {
	ID                string    `json:"id"`
	ResourceType      string    `json:"resource_type"`
	ResourceID        string    `json:"resource_id"`
	ResourceName      string    `json:"resource_name"`
	OwnerID           string    `json:"owner_id"`
	OwnerUsername     string    `json:"owner_username"`
	RecipientID       string    `json:"recipient_id"`
	RecipientUsername string    `json:"recipient_username"`
	Permission        string    `json:"permission"`
	CreatedAt         time.Time `json:"created_at"`
}

// remoteClient proxies GUI requests to the remote FileDock server while keeping
// the Bearer token strictly in process memory and reusing syncclient's TLS/redirect rules.
type remoteClient struct {
	baseURL    string
	httpClient *http.Client

	mu    sync.RWMutex
	token string
	user  *RemoteUser
}

func newRemoteClient(serverURL, caCertFile string, customClient *http.Client) (*remoteClient, error) {
	normalized, err := syncclient.NormalizeServerURL(serverURL)
	if err != nil {
		return nil, err
	}
	hc := customClient
	if hc == nil {
		hc, err = syncclient.NewHTTPClient(caCertFile)
		if err != nil {
			return nil, err
		}
	}
	return &remoteClient{
		baseURL:    normalized,
		httpClient: hc,
	}, nil
}

func (c *remoteClient) currentAuth() (string, *RemoteUser) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var u *RemoteUser
	if c.user != nil {
		cp := *c.user
		u = &cp
	}
	return c.token, u
}

func (c *remoteClient) setAuth(token string, user *RemoteUser) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
	if user != nil {
		cp := *user
		c.user = &cp
	} else {
		c.user = nil
	}
}

func (c *remoteClient) syncAPI() (*syncclient.HTTPAPI, error) {
	tok, _ := c.currentAuth()
	if tok == "" {
		return nil, ErrRemoteNotLoggedIn
	}
	return syncclient.NewHTTPAPI(c.baseURL, tok, c.httpClient)
}

func (c *remoteClient) doJSON(ctx context.Context, auth bool, method, endpoint string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		tok, _ := c.currentAuth()
		if tok == "" {
			return http.StatusUnauthorized, ErrRemoteNotLoggedIn
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized && auth {
		c.setAuth("", nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, parseRemoteError(resp)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxGUIJSONBody)).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func parseRemoteError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		return &RemoteError{Status: resp.StatusCode, Message: payload.Error}
	}
	return &RemoteError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
}

func (c *remoteClient) Register(ctx context.Context, username, password string) (RemoteUser, error) {
	var user RemoteUser
	_, err := c.doJSON(ctx, false, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": username,
		"password": password,
	}, &user)
	return user, err
}

func (c *remoteClient) Login(ctx context.Context, username, password string) (RemoteUser, error) {
	var out struct {
		Token string     `json:"token"`
		User  RemoteUser `json:"user"`
	}
	_, err := c.doJSON(ctx, false, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": username,
		"password": password,
	}, &out)
	if err != nil {
		return RemoteUser{}, err
	}
	c.setAuth(out.Token, &out.User)
	return out.User, nil
}

func (c *remoteClient) Logout(ctx context.Context) error {
	tok, _ := c.currentAuth()
	if tok == "" {
		return nil
	}
	_, err := c.doJSON(ctx, true, http.MethodPost, "/api/v1/auth/logout", nil, nil)
	c.setAuth("", nil)
	return err
}

func (c *remoteClient) ListFolders(ctx context.Context, parentID string) ([]RemoteFolder, error) {
	q := url.Values{}
	q.Set("limit", "100")
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	var items []RemoteFolder
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/folders?"+q.Encode(), nil, &items)
	if items == nil {
		items = []RemoteFolder{}
	}
	return items, err
}

func (c *remoteClient) ListFiles(ctx context.Context, parentID string) ([]RemoteFile, error) {
	q := url.Values{}
	q.Set("limit", "100")
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	var items []RemoteFile
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/files?"+q.Encode(), nil, &items)
	if items == nil {
		items = []RemoteFile{}
	}
	return items, err
}

func (c *remoteClient) SearchFiles(ctx context.Context, query, minSize, maxSize, parentID string) ([]RemoteFile, error) {
	q := url.Values{}
	q.Set("limit", "100")
	if query != "" {
		q.Set("q", query)
	}
	if minSize != "" {
		q.Set("min_size", minSize)
	}
	if maxSize != "" {
		q.Set("max_size", maxSize)
	}
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	var items []RemoteFile
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/files/search?"+q.Encode(), nil, &items)
	if items == nil {
		items = []RemoteFile{}
	}
	return items, err
}

func (c *remoteClient) CreateFolder(ctx context.Context, parentID *string, name string) (RemoteFolder, error) {
	var out RemoteFolder
	_, err := c.doJSON(ctx, true, http.MethodPost, "/api/v1/folders", map[string]any{
		"name":      name,
		"parent_id": parentID,
	}, &out)
	return out, err
}

func (c *remoteClient) UpdateFolder(ctx context.Context, id string, body map[string]any) (RemoteFolder, error) {
	var out RemoteFolder
	_, err := c.doJSON(ctx, true, http.MethodPatch, "/api/v1/folders/"+url.PathEscape(id), body, &out)
	return out, err
}

func (c *remoteClient) DeleteFolder(ctx context.Context, id string) error {
	_, err := c.doJSON(ctx, true, http.MethodDelete, "/api/v1/folders/"+url.PathEscape(id), nil, nil)
	return err
}

func (c *remoteClient) UpdateFile(ctx context.Context, id string, body map[string]any) (RemoteFile, error) {
	var out RemoteFile
	_, err := c.doJSON(ctx, true, http.MethodPatch, "/api/v1/files/"+url.PathEscape(id), body, &out)
	return out, err
}

func (c *remoteClient) DeleteFile(ctx context.Context, id string) error {
	_, err := c.doJSON(ctx, true, http.MethodDelete, "/api/v1/files/"+url.PathEscape(id), nil, nil)
	return err
}

func (c *remoteClient) UploadSimple(ctx context.Context, parentID *string, filename string, r io.Reader) (RemoteFile, error) {
	tok, _ := c.currentAuth()
	if tok == "" {
		return RemoteFile{}, ErrRemoteNotLoggedIn
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, r); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.CloseWithError(mw.Close())
	}()

	endpoint := "/api/v1/files"
	if parentID != nil && *parentID != "" {
		endpoint += "?parent_id=" + url.QueryEscape(*parentID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, pr)
	if err != nil {
		return RemoteFile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return RemoteFile{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return RemoteFile{}, parseRemoteError(resp)
	}
	var out RemoteFile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGUIJSONBody)).Decode(&out); err != nil {
		return RemoteFile{}, err
	}
	return out, nil
}

func (c *remoteClient) HeadFile(ctx context.Context, fileID string) (syncclient.RemoteContentInfo, error) {
	api, err := c.syncAPI()
	if err != nil {
		return syncclient.RemoteContentInfo{}, err
	}
	return api.HeadFile(ctx, fileID)
}

func (c *remoteClient) ReplaceFileContent(ctx context.Context, fileID, ifMatch string, body io.Reader, size int64) (RemoteFile, string, error) {
	tok, _ := c.currentAuth()
	if tok == "" {
		return RemoteFile{}, "", ErrRemoteNotLoggedIn
	}
	endpoint := "/api/v1/files/" + url.PathEscape(fileID) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+endpoint, body)
	if err != nil {
		return RemoteFile{}, "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if size >= 0 {
		req.ContentLength = size
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return RemoteFile{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return RemoteFile{}, "", parseRemoteError(resp)
	}
	var out RemoteFile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGUIJSONBody)).Decode(&out); err != nil {
		return RemoteFile{}, "", err
	}
	return out, strings.TrimSpace(resp.Header.Get("ETag")), nil
}

func (c *remoteClient) OpenFileDownload(ctx context.Context, fileID string) (io.ReadCloser, error) {
	tok, _ := c.currentAuth()
	if tok == "" {
		return nil, ErrRemoteNotLoggedIn
	}
	endpoint := "/api/v1/files/" + url.PathEscape(fileID) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		err := parseRemoteError(resp)
		_ = resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

func (c *remoteClient) ListShares(ctx context.Context, direction string) ([]RemoteShare, error) {
	var items []RemoteShare
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/shares?direction="+url.QueryEscape(direction)+"&limit=100", nil, &items)
	if items == nil {
		items = []RemoteShare{}
	}
	return items, err
}

func (c *remoteClient) CreateShare(ctx context.Context, resourceType, resourceID, recipientUsername string) (RemoteShare, error) {
	var out RemoteShare
	_, err := c.doJSON(ctx, true, http.MethodPost, "/api/v1/shares", map[string]string{
		"resource_type":      resourceType,
		"resource_id":        resourceID,
		"recipient_username": recipientUsername,
	}, &out)
	return out, err
}

func (c *remoteClient) DeleteShare(ctx context.Context, shareID string) error {
	_, err := c.doJSON(ctx, true, http.MethodDelete, "/api/v1/shares/"+url.PathEscape(shareID), nil, nil)
	return err
}

func (c *remoteClient) ListSharedFolders(ctx context.Context, shareID, parentID string) ([]RemoteFolder, error) {
	q := url.Values{}
	q.Set("limit", "100")
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	var items []RemoteFolder
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/shares/"+url.PathEscape(shareID)+"/folders?"+q.Encode(), nil, &items)
	if items == nil {
		items = []RemoteFolder{}
	}
	return items, err
}

func (c *remoteClient) ListSharedFiles(ctx context.Context, shareID, parentID string) ([]RemoteFile, error) {
	q := url.Values{}
	q.Set("limit", "100")
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	var items []RemoteFile
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/shares/"+url.PathEscape(shareID)+"/files?"+q.Encode(), nil, &items)
	if items == nil {
		items = []RemoteFile{}
	}
	return items, err
}

func (c *remoteClient) OpenSharedFileDownload(ctx context.Context, shareID, fileID string) (io.ReadCloser, error) {
	tok, _ := c.currentAuth()
	if tok == "" {
		return nil, ErrRemoteNotLoggedIn
	}
	endpoint := "/api/v1/shares/" + url.PathEscape(shareID) + "/files/" + url.PathEscape(fileID) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		err := parseRemoteError(resp)
		_ = resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

func (c *remoteClient) GetUploadSession(ctx context.Context, sessionID string) (syncclient.UploadSessionInfo, error) {
	api, err := c.syncAPI()
	if err != nil {
		return syncclient.UploadSessionInfo{}, err
	}
	return api.GetUploadSession(ctx, sessionID)
}

func formatStatusCode(err error) (int, string) {
	var re *RemoteError
	if errors.As(err, &re) {
		return re.Status, re.Message
	}
	switch {
	case errors.Is(err, ErrRemoteNotLoggedIn), errors.Is(err, syncclient.ErrUnauthorized):
		return http.StatusUnauthorized, err.Error()
	case errors.Is(err, ErrLocalPathForbidden):
		return http.StatusForbidden, err.Error()
	case errors.Is(err, ErrLocalNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, ErrLocalConflict), errors.Is(err, syncclient.ErrServerConflict):
		return http.StatusConflict, err.Error()
	case errors.Is(err, syncclient.ErrPreconditionFailed):
		return http.StatusPreconditionFailed, err.Error()
	case errors.Is(err, syncclient.ErrPayloadTooLarge):
		return http.StatusRequestEntityTooLarge, err.Error()
	case errors.Is(err, ErrLocalInvalidName), errors.Is(err, ErrLocalHashMismatch), errors.Is(err, syncclient.ErrUploadSourceMismatch):
		return http.StatusBadRequest, err.Error()
	default:
		return http.StatusBadGateway, err.Error()
	}
}

func parseOptionalIntParam(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	if _, err := strconv.ParseInt(trimmed, 10, 64); err != nil {
		return trimmed
	}
	return trimmed
}
