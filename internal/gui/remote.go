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

const (
	maxGUIJSONBody     = 16 << 20
	defaultGUIPageSize = 50
	maxGUIPageSize     = 99 // fetch limit+1 (<= 100, server MaxListLimit) to determine exact has_more
)

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

// remoteClient proxies GUI requests for a single browser session while keeping
// the Bearer token strictly in process memory and tracking authGen so stale 401s
// from an earlier token can never clear a newer login.
type remoteClient struct {
	baseURL    string
	httpClient *http.Client

	mu      sync.RWMutex
	token   string
	user    *RemoteUser
	authGen uint64
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
	tok, u, _ := c.currentAuthWithGen()
	return tok, u
}

func (c *remoteClient) currentAuthWithGen() (string, *RemoteUser, uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var u *RemoteUser
	if c.user != nil {
		cp := *c.user
		u = &cp
	}
	return c.token, u, c.authGen
}

func (c *remoteClient) setAuth(token string, user *RemoteUser) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authGen++
	c.token = token
	if user != nil {
		cp := *user
		c.user = &cp
	} else {
		c.user = nil
	}
}

// clearAuthIfMatching clears auth state ONLY if no newer Login/Logout occurred since the request started.
func (c *remoteClient) clearAuthIfMatching(sentToken string, sentGen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authGen == sentGen && c.token == sentToken {
		c.authGen++
		c.token = ""
		c.user = nil
	}
}

func (c *remoteClient) syncAPI() (*syncclient.HTTPAPI, string, error) {
	tok, user := c.currentAuth()
	if tok == "" || user == nil {
		return nil, "", ErrRemoteNotLoggedIn
	}
	api, err := syncclient.NewHTTPAPI(c.baseURL, tok, c.httpClient)
	if err != nil {
		return nil, "", err
	}
	return api, user.ID, nil
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
	var (
		sentToken string
		sentGen   uint64
	)
	if auth {
		sentToken, _, sentGen = c.currentAuthWithGen()
		if sentToken == "" {
			return http.StatusUnauthorized, ErrRemoteNotLoggedIn
		}
		req.Header.Set("Authorization", "Bearer "+sentToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized && auth {
		c.clearAuthIfMatching(sentToken, sentGen)
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

func normalizePageParams(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultGUIPageSize
	}
	if limit > maxGUIPageSize {
		limit = maxGUIPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
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

func (c *remoteClient) Logout(ctx context.Context) (string, error) {
	tok, user, gen := c.currentAuthWithGen()
	if tok == "" {
		return "", nil
	}
	userID := ""
	if user != nil {
		userID = user.ID
	}
	_, err := c.doJSON(ctx, true, http.MethodPost, "/api/v1/auth/logout", nil, nil)
	c.clearAuthIfMatching(tok, gen)
	return userID, err
}

func (c *remoteClient) ListFolders(ctx context.Context, parentID string, limit, offset int) ([]RemoteFolder, bool, int, error) {
	limit, offset = normalizePageParams(limit, offset)
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit+1))
	q.Set("offset", strconv.Itoa(offset))
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	var items []RemoteFolder
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/folders?"+q.Encode(), nil, &items)
	if err != nil {
		return nil, false, offset, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if items == nil {
		items = []RemoteFolder{}
	}
	return items, hasMore, offset + len(items), nil
}

func (c *remoteClient) ListFiles(ctx context.Context, parentID string, limit, offset int) ([]RemoteFile, bool, int, error) {
	limit, offset = normalizePageParams(limit, offset)
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit+1))
	q.Set("offset", strconv.Itoa(offset))
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	var items []RemoteFile
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/files?"+q.Encode(), nil, &items)
	if err != nil {
		return nil, false, offset, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if items == nil {
		items = []RemoteFile{}
	}
	return items, hasMore, offset + len(items), nil
}

func (c *remoteClient) SearchFiles(ctx context.Context, query, minSize, maxSize, parentID string, limit, offset int) ([]RemoteFile, bool, int, error) {
	limit, offset = normalizePageParams(limit, offset)
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit+1))
	q.Set("offset", strconv.Itoa(offset))
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
	if err != nil {
		return nil, false, offset, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if items == nil {
		items = []RemoteFile{}
	}
	return items, hasMore, offset + len(items), nil
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
	tok, _, gen := c.currentAuthWithGen()
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

	if resp.StatusCode == http.StatusUnauthorized {
		c.clearAuthIfMatching(tok, gen)
	}
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
	api, _, err := c.syncAPI()
	if err != nil {
		return syncclient.RemoteContentInfo{}, err
	}
	return api.HeadFile(ctx, fileID)
}

func (c *remoteClient) ReplaceFileContent(ctx context.Context, fileID, ifMatch string, body io.Reader, size int64) (RemoteFile, string, error) {
	tok, _, gen := c.currentAuthWithGen()
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
	if resp.StatusCode == http.StatusUnauthorized {
		c.clearAuthIfMatching(tok, gen)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return RemoteFile{}, "", parseRemoteError(resp)
	}
	var out RemoteFile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGUIJSONBody)).Decode(&out); err != nil {
		return RemoteFile{}, "", err
	}
	return out, strings.TrimSpace(resp.Header.Get("ETag")), nil
}

func (c *remoteClient) OpenFileDownload(ctx context.Context, fileID string) (downloadStreamResult, error) {
	tok, _, gen := c.currentAuthWithGen()
	if tok == "" {
		return downloadStreamResult{}, ErrRemoteNotLoggedIn
	}
	endpoint := "/api/v1/files/" + url.PathEscape(fileID) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return downloadStreamResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return downloadStreamResult{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.clearAuthIfMatching(tok, gen)
	}
	if resp.StatusCode != http.StatusOK {
		err := parseRemoteError(resp)
		_ = resp.Body.Close()
		return downloadStreamResult{}, err
	}
	return downloadStreamResult{
		Body:         resp.Body,
		HeaderSHA256: strings.TrimSpace(resp.Header.Get("X-FileDock-SHA256")),
	}, nil
}

func (c *remoteClient) ListShares(ctx context.Context, direction string, limit, offset int) ([]RemoteShare, bool, int, error) {
	limit, offset = normalizePageParams(limit, offset)
	q := url.Values{}
	q.Set("direction", direction)
	q.Set("limit", strconv.Itoa(limit+1))
	q.Set("offset", strconv.Itoa(offset))
	var items []RemoteShare
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/shares?"+q.Encode(), nil, &items)
	if err != nil {
		return nil, false, offset, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if items == nil {
		items = []RemoteShare{}
	}
	return items, hasMore, offset + len(items), nil
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

func (c *remoteClient) ListSharedFolders(ctx context.Context, shareID, parentID string, limit, offset int) ([]RemoteFolder, bool, int, error) {
	limit, offset = normalizePageParams(limit, offset)
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit+1))
	q.Set("offset", strconv.Itoa(offset))
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	var items []RemoteFolder
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/shares/"+url.PathEscape(shareID)+"/folders?"+q.Encode(), nil, &items)
	if err != nil {
		return nil, false, offset, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if items == nil {
		items = []RemoteFolder{}
	}
	return items, hasMore, offset + len(items), nil
}

func (c *remoteClient) ListSharedFiles(ctx context.Context, shareID, parentID string, limit, offset int) ([]RemoteFile, bool, int, error) {
	limit, offset = normalizePageParams(limit, offset)
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit+1))
	q.Set("offset", strconv.Itoa(offset))
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	var items []RemoteFile
	_, err := c.doJSON(ctx, true, http.MethodGet, "/api/v1/shares/"+url.PathEscape(shareID)+"/files?"+q.Encode(), nil, &items)
	if err != nil {
		return nil, false, offset, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if items == nil {
		items = []RemoteFile{}
	}
	return items, hasMore, offset + len(items), nil
}

func (c *remoteClient) OpenSharedFileDownload(ctx context.Context, shareID, fileID string) (downloadStreamResult, error) {
	tok, _, gen := c.currentAuthWithGen()
	if tok == "" {
		return downloadStreamResult{}, ErrRemoteNotLoggedIn
	}
	endpoint := "/api/v1/shares/" + url.PathEscape(shareID) + "/files/" + url.PathEscape(fileID) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return downloadStreamResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return downloadStreamResult{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.clearAuthIfMatching(tok, gen)
	}
	if resp.StatusCode != http.StatusOK {
		err := parseRemoteError(resp)
		_ = resp.Body.Close()
		return downloadStreamResult{}, err
	}
	return downloadStreamResult{
		Body:         resp.Body,
		HeaderSHA256: strings.TrimSpace(resp.Header.Get("X-FileDock-SHA256")),
	}, nil
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

func parseQueryInt(q url.Values, key string, fallback int) int {
	raw := strings.TrimSpace(q.Get(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}
