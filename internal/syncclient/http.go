package syncclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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
	ErrServerConflict       = errors.New("conflict on server (HTTP 409)")
	ErrSessionExpired       = errors.New("upload session expired on server (HTTP 410)")
	ErrInsecureTransport    = errors.New("bearer token requests require HTTPS or loopback HTTP")
)

// UploadSessionInfo represents upload session progress returned by /api/v1/uploads.
type UploadSessionInfo struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	ParentID       *string    `json:"parent_id"`
	Size           int64      `json:"size"`
	SHA256         string     `json:"sha256"`
	ChunkSize      int64      `json:"chunk_size"`
	TotalChunks    int        `json:"total_chunks"`
	Status         string     `json:"status"`
	UploadedChunks []int      `json:"uploaded_chunks"`
	UploadedBytes  int64      `json:"uploaded_bytes"`
	FileID         *string    `json:"file_id"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

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
	CreateUploadSession(ctx context.Context, parentID *string, name string, size int64, sha256Hex string) (UploadSessionInfo, error)
	GetUploadSession(ctx context.Context, sessionID string) (UploadSessionInfo, error)
	UploadChunk(ctx context.Context, sessionID string, index int, chunkSHA256 string, body io.Reader, contentLength int64) error
	CompleteUploadSession(ctx context.Context, sessionID string) (FileMeta, error)
}

// HTTPAPI implements API over HTTP using a Bearer token.
type HTTPAPI struct {
	baseURL string
	token   string
	client  *http.Client
}

// ValidateBearerURL ensures a target URL is safe for Bearer-authenticated requests:
// HTTPS is allowed for any host, while plain HTTP is restricted strictly to loopback hosts.
func ValidateBearerURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("server URL is required")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid server URL %q: must use http or https with a host", raw)
	}
	if scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("%w: %q", ErrInsecureTransport, raw)
	}
	u.Scheme = scheme
	return u, nil
}

// NormalizeServerURL validates and normalizes a FileDock server base URL.
func NormalizeServerURL(raw string) (string, error) {
	u, err := ValidateBearerURL(raw)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// NewHTTPClient creates a TLS-verifying HTTP client shared by upload, pull, and push.
// When caCertFile is non-empty, its PEM certificates are trusted in addition to system roots.
// Redirects enforce the same HTTPS-or-loopback policy and strip Authorization across origins.
func NewHTTPClient(caCertFile string) (*http.Client, error) {
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if trimmed := strings.TrimSpace(caCertFile); trimmed != "" {
		pemData, err := os.ReadFile(trimmed)
		if err != nil {
			return nil, fmt.Errorf("read CA certificate %q: %w", trimmed, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("CA certificate %q contains no valid PEM certificates", trimmed)
		}
		tlsCfg.RootCAs = pool
	}
	transport.TLSClientConfig = tlsCfg

	return &http.Client{
		Timeout:       60 * time.Second,
		Transport:     transport,
		CheckRedirect: bearerSafeCheckRedirect(nil),
	}, nil
}

func bearerSafeCheckRedirect(next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if _, err := ValidateBearerURL(req.URL.String()); err != nil {
			return fmt.Errorf("redirect blocked: %w", err)
		}
		if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
			req.Header.Del("Authorization")
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	schemeA := strings.ToLower(a.Scheme)
	schemeB := strings.ToLower(b.Scheme)
	if schemeA != schemeB {
		return false
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func isLoopbackHost(host string) bool {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return false
	}
	if strings.EqualFold(trimmed, "localhost") {
		return true
	}
	ip := net.ParseIP(trimmed)
	return ip != nil && ip.IsLoopback()
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
	var clientCopy *http.Client
	if httpClient == nil {
		clientCopy, err = NewHTTPClient("")
		if err != nil {
			return nil, err
		}
	} else {
		c := *httpClient
		c.CheckRedirect = bearerSafeCheckRedirect(httpClient.CheckRedirect)
		clientCopy = &c
	}
	return &HTTPAPI{
		baseURL: normalized,
		token:   strings.TrimSpace(token),
		client:  clientCopy,
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

// CreateUploadSession calls POST /api/v1/uploads to start a new resumable upload session.
func (a *HTTPAPI) CreateUploadSession(ctx context.Context, parentID *string, name string, size int64, sha256Hex string) (UploadSessionInfo, error) {
	payload := struct {
		ParentID *string `json:"parent_id"`
		Name     string  `json:"name"`
		Size     int64   `json:"size"`
		SHA256   string  `json:"sha256"`
	}{
		ParentID: parentID,
		Name:     name,
		Size:     size,
		SHA256:   sha256Hex,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return UploadSessionInfo{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/api/v1/uploads", bytes.NewReader(raw))
	if err != nil {
		return UploadSessionInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return UploadSessionInfo{}, fmt.Errorf("POST /api/v1/uploads: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return UploadSessionInfo{}, statusError(resp.StatusCode, "/api/v1/uploads")
	}
	var session UploadSessionInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONResponseSize)).Decode(&session); err != nil {
		return UploadSessionInfo{}, fmt.Errorf("decode POST /api/v1/uploads: %w", err)
	}
	return session, nil
}

// GetUploadSession calls GET /api/v1/uploads/{id} to inspect confirmed chunks and session metadata.
func (a *HTTPAPI) GetUploadSession(ctx context.Context, sessionID string) (UploadSessionInfo, error) {
	endpoint := "/api/v1/uploads/" + url.PathEscape(sessionID)
	req, err := a.newRequest(ctx, http.MethodGet, endpoint)
	if err != nil {
		return UploadSessionInfo{}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return UploadSessionInfo{}, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return UploadSessionInfo{}, statusError(resp.StatusCode, endpoint)
	}
	var session UploadSessionInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONResponseSize)).Decode(&session); err != nil {
		return UploadSessionInfo{}, fmt.Errorf("decode GET %s: %w", endpoint, err)
	}
	return session, nil
}

// UploadChunk streams a single chunk via PUT /api/v1/uploads/{id}/chunks/{index}.
func (a *HTTPAPI) UploadChunk(ctx context.Context, sessionID string, index int, chunkSHA256 string, body io.Reader, contentLength int64) error {
	endpoint := fmt.Sprintf("/api/v1/uploads/%s/chunks/%d", url.PathEscape(sessionID), index)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.baseURL+endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if chunkSHA256 != "" {
		req.Header.Set("X-Chunk-SHA256", chunkSHA256)
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return statusError(resp.StatusCode, endpoint)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONResponseSize))
	return nil
}

// CompleteUploadSession finalizes an upload session via POST /api/v1/uploads/{id}/complete.
func (a *HTTPAPI) CompleteUploadSession(ctx context.Context, sessionID string) (FileMeta, error) {
	endpoint := "/api/v1/uploads/" + url.PathEscape(sessionID) + "/complete"
	req, err := a.newRequest(ctx, http.MethodPost, endpoint)
	if err != nil {
		return FileMeta{}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return FileMeta{}, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return FileMeta{}, statusError(resp.StatusCode, endpoint)
	}
	var file FileMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONResponseSize)).Decode(&file); err != nil {
		return FileMeta{}, fmt.Errorf("decode POST %s: %w", endpoint, err)
	}
	return file, nil
}

func statusError(code int, endpoint string) error {
	switch code {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, endpoint)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrServerConflict, endpoint)
	case http.StatusGone:
		return fmt.Errorf("%w: %s", ErrSessionExpired, endpoint)
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
