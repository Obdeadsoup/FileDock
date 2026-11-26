package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPageLimit    = 100
	maxJSONResponseSize = 16 << 20 // 16 MiB safety bound for JSON responses.
)

var (
	ErrUnauthorized         = errors.New("unauthorized: check FILEDOCK_TOKEN")
	ErrNotFound             = errors.New("resource not found on server")
	ErrPreconditionFailed   = errors.New("precondition failed on server (HTTP 412)")
	ErrPreconditionRequired = errors.New("precondition required on server (HTTP 428)")
	ErrPayloadTooLarge      = errors.New("file exceeds server maximum size (HTTP 413)")
)

// RemoteContentInfo describes the current remote version metadata returned by HEAD /api/v1/files/{id}/content.
type RemoteContentInfo struct {
	ETag            string
	ContentRevision int64
	SHA256          string
	Size            int64
}

// UserInfo represents the authenticated user returned by GET /api/v1/auth/me.
type UserInfo struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// FileMeta represents public file metadata in sync change events.
type FileMeta struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	ParentID        *string   `json:"parent_id"`
	Size            int64     `json:"size"`
	SHA256          string    `json:"sha256"`
	ContentRevision int64     `json:"content_revision,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// FolderMeta represents public folder metadata in sync change events.
type FolderMeta struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ParentID  *string   `json:"parent_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChangeEvent represents a single incremental sync event from GET /api/v1/sync/changes.
type ChangeEvent struct {
	Seq        int64       `json:"seq"`
	Type       string      `json:"type"`
	ResourceID string      `json:"resource_id"`
	OccurredAt time.Time   `json:"occurred_at"`
	File       *FileMeta   `json:"file,omitempty"`
	Folder     *FolderMeta `json:"folder,omitempty"`
}

// ChangePage is a single page returned by GET /api/v1/sync/changes.
type ChangePage struct {
	Items      []ChangeEvent `json:"items"`
	NextCursor int64         `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
}

// API defines the server operations required by the sync client.
type API interface {
	Me(ctx context.Context) (UserInfo, error)
	ListChanges(ctx context.Context, after int64, limit int) (ChangePage, error)
	DownloadFile(ctx context.Context, fileID string) (io.ReadCloser, error)
	HeadFile(ctx context.Context, fileID string) (RemoteContentInfo, error)
	ReplaceFileContent(ctx context.Context, fileID, ifMatch string, body io.Reader, contentLength int64) (FileMeta, string, error)
}

// HTTPAPI implements API over HTTP using a Bearer token.
type HTTPAPI struct {
	baseURL string
	token   string
	client  *http.Client
}

// NormalizeServerURL validates and normalizes a FileDock server base URL.
func NormalizeServerURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("server URL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid server URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid server URL %q: must use http or https with a host", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// NewHTTPAPI constructs an HTTPAPI client bound to serverURL and bearer token.
func NewHTTPAPI(serverURL, token string, httpClient *http.Client) (*HTTPAPI, error) {
	normalized, err := NormalizeServerURL(serverURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("FILEDOCK_TOKEN must not be empty")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &HTTPAPI{
		baseURL: normalized,
		token:   strings.TrimSpace(token),
		client:  httpClient,
	}, nil
}

func (a *HTTPAPI) newRequest(ctx context.Context, method, path string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	return req, nil
}

// Me calls GET /api/v1/auth/me to identify the authenticated account.
func (a *HTTPAPI) Me(ctx context.Context) (UserInfo, error) {
	req, err := a.newRequest(ctx, http.MethodGet, "/api/v1/auth/me")
	if err != nil {
		return UserInfo{}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return UserInfo{}, fmt.Errorf("request /api/v1/auth/me: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return UserInfo{}, statusError(resp.StatusCode, "/api/v1/auth/me")
	}
	var user UserInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONResponseSize)).Decode(&user); err != nil {
		return UserInfo{}, fmt.Errorf("decode /api/v1/auth/me: %w", err)
	}
	if strings.TrimSpace(user.ID) == "" {
		return UserInfo{}, errors.New("server returned empty user id from /api/v1/auth/me")
	}
	return user, nil
}

// ListChanges calls GET /api/v1/sync/changes?after=<after>&limit=<limit>.
func (a *HTTPAPI) ListChanges(ctx context.Context, after int64, limit int) (ChangePage, error) {
	if limit <= 0 {
		limit = defaultPageLimit
	}
	endpoint := fmt.Sprintf("/api/v1/sync/changes?after=%d&limit=%d", after, limit)
	req, err := a.newRequest(ctx, http.MethodGet, endpoint)
	if err != nil {
		return ChangePage{}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return ChangePage{}, fmt.Errorf("request sync changes: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ChangePage{}, statusError(resp.StatusCode, "/api/v1/sync/changes")
	}
	var page ChangePage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONResponseSize)).Decode(&page); err != nil {
		return ChangePage{}, fmt.Errorf("decode sync changes: %w", err)
	}
	return page, nil
}

// DownloadFile opens a download stream from GET /api/v1/files/{id}/content.
func (a *HTTPAPI) DownloadFile(ctx context.Context, fileID string) (io.ReadCloser, error) {
	endpoint := "/api/v1/files/" + url.PathEscape(fileID) + "/content"
	req, err := a.newRequest(ctx, http.MethodGet, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file %s: %w", fileID, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, statusError(resp.StatusCode, endpoint)
	}
	return resp.Body, nil
}

// HeadFile inspects remote opaque ETag, content revision, and SHA-256 via HEAD /api/v1/files/{id}/content.
func (a *HTTPAPI) HeadFile(ctx context.Context, fileID string) (RemoteContentInfo, error) {
	endpoint := "/api/v1/files/" + url.PathEscape(fileID) + "/content"
	req, err := a.newRequest(ctx, http.MethodHead, endpoint)
	if err != nil {
		return RemoteContentInfo{}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return RemoteContentInfo{}, fmt.Errorf("HEAD %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return RemoteContentInfo{}, statusError(resp.StatusCode, endpoint)
	}

	rev, _ := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("X-FileDock-Content-Revision")), 10, 64)
	size, _ := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64)
	return RemoteContentInfo{
		ETag:            strings.TrimSpace(resp.Header.Get("ETag")),
		ContentRevision: rev,
		SHA256:          strings.TrimSpace(resp.Header.Get("X-FileDock-SHA256")),
		Size:            size,
	}, nil
}

// ReplaceFileContent streams body to PUT /api/v1/files/{id}/content with an opaque If-Match ETag.
func (a *HTTPAPI) ReplaceFileContent(ctx context.Context, fileID, ifMatch string, body io.Reader, contentLength int64) (FileMeta, string, error) {
	endpoint := "/api/v1/files/" + url.PathEscape(fileID) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.baseURL+endpoint, body)
	if err != nil {
		return FileMeta{}, "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("If-Match", ifMatch)
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return FileMeta{}, "", fmt.Errorf("PUT %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return FileMeta{}, "", statusError(resp.StatusCode, endpoint)
	}
	var updated FileMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONResponseSize)).Decode(&updated); err != nil {
		return FileMeta{}, "", fmt.Errorf("decode PUT %s: %w", endpoint, err)
	}
	return updated, strings.TrimSpace(resp.Header.Get("ETag")), nil
}

func statusError(code int, endpoint string) error {
	switch code {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, endpoint)
	case http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %s", ErrPreconditionFailed, endpoint)
	case http.StatusPreconditionRequired:
		return fmt.Errorf("%w: %s", ErrPreconditionRequired, endpoint)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %s", ErrPayloadTooLarge, endpoint)
	default:
		return fmt.Errorf("server returned HTTP %d for %s", code, endpoint)
	}
}
