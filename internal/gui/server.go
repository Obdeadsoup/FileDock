package gui

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed web/*
var webFS embed.FS

const (
	guiSessionCookieName = "filedock_gui_session"
	guiCSRFHeaderName    = "X-FileDock-GUI-Token"
)

// Config configures the local dual-pane GUI HTTP server.
type Config struct {
	ServerURL  string
	LocalDir   string
	CACertFile string
	ListenAddr string // defaults to 127.0.0.1:0; must be loopback
	HTTPClient *http.Client
}

// Server serves the embedded dual-pane UI on 127.0.0.1 and brokers local/remote file actions.
type Server struct {
	cfg           Config
	localDir      string
	remote        *remoteClient
	jobs          *jobManager
	sessionSecret string
	csrfToken     string
	boundHostPort string
	mux           *http.ServeMux
}

// NewServer validates configuration, initializes confined local access and the remote HTTP client.
func NewServer(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.LocalDir) == "" {
		return nil, errors.New("--local-dir is required")
	}
	absLocal, err := filepath.Abs(cfg.LocalDir)
	if err != nil {
		return nil, fmt.Errorf("resolve --local-dir: %w", err)
	}
	info, err := os.Stat(absLocal)
	if err != nil {
		return nil, fmt.Errorf("stat --local-dir %q: %w", absLocal, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("--local-dir %q is not a directory", absLocal)
	}

	listenAddr := strings.TrimSpace(cfg.ListenAddr)
	if listenAddr == "" {
		listenAddr = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid --listen address %q: %w", listenAddr, err)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("GUI server may only listen on loopback (127.0.0.1/localhost/::1), got %q", listenAddr)
	}
	cfg.ListenAddr = listenAddr

	remote, err := newRemoteClient(cfg.ServerURL, cfg.CACertFile, cfg.HTTPClient)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:           cfg,
		localDir:      absLocal,
		remote:        remote,
		jobs:          newJobManager(),
		sessionSecret: randomHex(32),
		csrfToken:     randomHex(32),
		mux:           http.NewServeMux(),
	}
	s.registerRoutes()
	return s, nil
}

// Handler returns the HTTP handler (also useful for httptest).
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Serve binds to 127.0.0.1, prints the access URL to stdout, and serves until ctx is canceled.
func (s *Server) Serve(ctx context.Context, stdout io.Writer) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", s.cfg.ListenAddr, err)
	}
	s.boundHostPort = ln.Addr().String()
	accessURL := "http://" + s.boundHostPort
	if stdout != nil {
		_, _ = fmt.Fprintf(stdout, "FileDock GUI listening at %s (server=%s local_dir=%s)\n", accessURL, s.remote.baseURL, s.localDir)
	}

	httpSrv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	select {
	case err = <-serveErr:
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		err = <-serveErr
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /", s.serveIndex)
	s.mux.HandleFunc("GET /style.css", s.serveStatic("web/style.css", "text/css; charset=utf-8"))
	s.mux.HandleFunc("GET /app.js", s.serveStatic("web/app.js", "application/javascript; charset=utf-8"))

	// Protected local GUI API routes (require HttpOnly GUI session cookie + CSRF header + same-origin check).
	s.mux.Handle("GET /api/gui/status", s.protect(s.handleStatus))
	s.mux.Handle("POST /api/gui/auth/register", s.protect(s.handleRegister))
	s.mux.Handle("POST /api/gui/auth/login", s.protect(s.handleLogin))
	s.mux.Handle("POST /api/gui/auth/logout", s.protect(s.handleLogout))

	s.mux.Handle("GET /api/gui/local/list", s.protect(s.handleLocalList))
	s.mux.Handle("POST /api/gui/local/mkdir", s.protect(s.handleLocalMkdir))
	s.mux.Handle("POST /api/gui/local/rename", s.protect(s.handleLocalRename))
	s.mux.Handle("POST /api/gui/local/delete", s.protect(s.handleLocalDelete))

	s.mux.Handle("GET /api/gui/remote/list", s.protect(s.handleRemoteList))
	s.mux.Handle("GET /api/gui/remote/search", s.protect(s.handleRemoteSearch))
	s.mux.Handle("POST /api/gui/remote/folders", s.protect(s.handleRemoteCreateFolder))
	s.mux.Handle("PATCH /api/gui/remote/folders/{id}", s.protect(s.handleRemoteUpdateFolder))
	s.mux.Handle("DELETE /api/gui/remote/folders/{id}", s.protect(s.handleRemoteDeleteFolder))
	s.mux.Handle("PATCH /api/gui/remote/files/{id}", s.protect(s.handleRemoteUpdateFile))
	s.mux.Handle("DELETE /api/gui/remote/files/{id}", s.protect(s.handleRemoteDeleteFile))
	s.mux.Handle("GET /api/gui/remote/files/{id}/head", s.protect(s.handleRemoteHeadFile))

	s.mux.Handle("GET /api/gui/remote/shares", s.protect(s.handleRemoteListShares))
	s.mux.Handle("POST /api/gui/remote/shares", s.protect(s.handleRemoteCreateShare))
	s.mux.Handle("DELETE /api/gui/remote/shares/{id}", s.protect(s.handleRemoteDeleteShare))
	s.mux.Handle("GET /api/gui/remote/shares/{id}/browse", s.protect(s.handleRemoteBrowseShare))

	s.mux.Handle("POST /api/gui/transfer/upload", s.protect(s.handleUploadSimple))
	s.mux.Handle("POST /api/gui/transfer/replace", s.protect(s.handleReplaceContent))
	s.mux.Handle("POST /api/gui/transfer/upload-resumable", s.protect(s.handleStartResumableUpload))
	s.mux.Handle("GET /api/gui/transfer/jobs/{id}", s.protect(s.handleGetUploadJob))
	s.mux.Handle("POST /api/gui/transfer/jobs/{id}/cancel", s.protect(s.handleCancelUploadJob))
	s.mux.Handle("POST /api/gui/transfer/download", s.protect(s.handleDownload))
	s.mux.Handle("POST /api/gui/transfer/download-shared", s.protect(s.handleDownloadShared))
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "missing embedded index.html", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     guiSessionCookieName,
		Value:    s.sessionSecret,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	html := strings.Replace(string(raw), "__GUI_CSRF_TOKEN__", s.csrfToken, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, html)
}

func (s *Server) serveStatic(embedPath, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := webFS.ReadFile(embedPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	}
}

func (s *Server) protect(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.verifyOriginAndSession(r) {
			writeGUIError(w, http.StatusForbidden, "forbidden: invalid local GUI session or cross-origin request")
			return
		}
		next(w, r)
	})
}

func (s *Server) verifyOriginAndSession(r *http.Request) bool {
	// 1. Validate Host is loopback (and matches boundHostPort when bound).
	hostOnly := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		hostOnly = h
	}
	if !isLoopbackHost(hostOnly) {
		return false
	}
	if s.boundHostPort != "" && !strings.EqualFold(r.Host, s.boundHostPort) {
		_, boundPort, _ := net.SplitHostPort(s.boundHostPort)
		_, reqPort, _ := net.SplitHostPort(r.Host)
		if reqPort != boundPort {
			return false
		}
	}

	// 2. Validate Origin and Sec-Fetch-Site when present.
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Scheme, "http") || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	if fetchSite := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); fetchSite != "" {
		if fetchSite != "same-origin" && fetchSite != "none" {
			return false
		}
	}

	// 3. Validate HttpOnly GUI session cookie + CSRF header token.
	cookie, err := r.Cookie(guiSessionCookieName)
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.sessionSecret)) != 1 {
		return false
	}
	csrf := strings.TrimSpace(r.Header.Get(guiCSRFHeaderName))
	if subtle.ConstantTimeCompare([]byte(csrf), []byte(s.csrfToken)) != 1 {
		return false
	}
	return true
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	_, user := s.remote.currentAuth()
	writeGUIJSON(w, http.StatusOK, map[string]any{
		"server_url": s.remote.baseURL,
		"local_dir":  s.localDir,
		"logged_in":  user != nil,
		"user":       user,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	user, err := s.remote.Register(r.Context(), req.Username, req.Password)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusCreated, user)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	user, err := s.remote.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	// Never return the Bearer token to the browser; return only public user info.
	writeGUIJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.remote.Logout(r.Context()); err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLocalList(w http.ResponseWriter, r *http.Request) {
	entries, cleaned, err := listLocalDir(s.localDir, r.URL.Query().Get("path"))
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]any{
		"path":    cleaned,
		"entries": entries,
	})
}

func (s *Server) handleLocalMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	created, err := mkdirLocal(s.localDir, req.Parent, req.Name)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusCreated, map[string]string{"path": created})
}

func (s *Server) handleLocalRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		DstDir  string `json:"dst_dir"`
		NewName string `json:"new_name"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	dst, err := renameOrMoveLocal(s.localDir, req.Path, req.DstDir, req.NewName)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]string{"path": dst})
}

func (s *Server) handleLocalDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	if err := deleteLocal(s.localDir, req.Path); err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRemoteList(w http.ResponseWriter, r *http.Request) {
	parentID := strings.TrimSpace(r.URL.Query().Get("parent_id"))
	folders, err := s.remote.ListFolders(r.Context(), parentID)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	filesList, err := s.remote.ListFiles(r.Context(), parentID)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]any{
		"parent_id": parentID,
		"folders":   folders,
		"files":     filesList,
	})
}

func (s *Server) handleRemoteSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, err := s.remote.SearchFiles(
		r.Context(),
		q.Get("q"),
		parseOptionalIntParam(q.Get("min_size")),
		parseOptionalIntParam(q.Get("max_size")),
		strings.TrimSpace(q.Get("parent_id")),
	)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]any{"files": items})
}

func (s *Server) handleRemoteCreateFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string  `json:"name"`
		ParentID *string `json:"parent_id"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	folder, err := s.remote.CreateFolder(r.Context(), req.ParentID, req.Name)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusCreated, folder)
}

func (s *Server) handleRemoteUpdateFolder(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	folder, err := s.remote.UpdateFolder(r.Context(), r.PathValue("id"), req)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, folder)
}

func (s *Server) handleRemoteDeleteFolder(w http.ResponseWriter, r *http.Request) {
	if err := s.remote.DeleteFolder(r.Context(), r.PathValue("id")); err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRemoteUpdateFile(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	file, err := s.remote.UpdateFile(r.Context(), r.PathValue("id"), req)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, file)
}

func (s *Server) handleRemoteDeleteFile(w http.ResponseWriter, r *http.Request) {
	if err := s.remote.DeleteFile(r.Context(), r.PathValue("id")); err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRemoteHeadFile(w http.ResponseWriter, r *http.Request) {
	info, err := s.remote.HeadFile(r.Context(), r.PathValue("id"))
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]any{
		"etag":             info.ETag,
		"content_revision": info.ContentRevision,
		"sha256":           info.SHA256,
		"size":             info.Size,
	})
}

func (s *Server) handleRemoteListShares(w http.ResponseWriter, r *http.Request) {
	dir := strings.TrimSpace(r.URL.Query().Get("direction"))
	if dir == "" {
		dir = "received"
	}
	items, err := s.remote.ListShares(r.Context(), dir)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]any{"shares": items})
}

func (s *Server) handleRemoteCreateShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ResourceType      string `json:"resource_type"`
		ResourceID        string `json:"resource_id"`
		RecipientUsername string `json:"recipient_username"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	sh, err := s.remote.CreateShare(r.Context(), req.ResourceType, req.ResourceID, req.RecipientUsername)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusCreated, sh)
}

func (s *Server) handleRemoteDeleteShare(w http.ResponseWriter, r *http.Request) {
	if err := s.remote.DeleteShare(r.Context(), r.PathValue("id")); err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRemoteBrowseShare(w http.ResponseWriter, r *http.Request) {
	shareID := r.PathValue("id")
	parentID := strings.TrimSpace(r.URL.Query().Get("parent_id"))
	resourceType := strings.TrimSpace(r.URL.Query().Get("resource_type"))

	var folders []RemoteFolder
	if resourceType != "file" {
		var err error
		folders, err = s.remote.ListSharedFolders(r.Context(), shareID, parentID)
		if err != nil {
			code, msg := formatStatusCode(err)
			writeGUIError(w, code, msg)
			return
		}
	} else {
		folders = []RemoteFolder{}
	}
	filesList, err := s.remote.ListSharedFiles(r.Context(), shareID, parentID)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]any{
		"share_id":  shareID,
		"parent_id": parentID,
		"folders":   folders,
		"files":     filesList,
	})
}

func (s *Server) handleUploadSimple(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPath string  `json:"local_path"`
		ParentID  *string `json:"parent_id"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	f, _, cleaned, err := openLocalRegularFile(s.localDir, req.LocalPath)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	defer f.Close()

	uploaded, err := s.remote.UploadSimple(r.Context(), req.ParentID, filepath.Base(cleaned), f)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusCreated, uploaded)
}

func (s *Server) handleReplaceContent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPath string `json:"local_path"`
		FileID    string `json:"file_id"`
		IfMatch   string `json:"if_match"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	f, info, _, err := openLocalRegularFile(s.localDir, req.LocalPath)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	defer f.Close()

	updated, etag, err := s.remote.ReplaceFileContent(r.Context(), req.FileID, req.IfMatch, f, info.Size())
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, map[string]any{
		"file": updated,
		"etag": etag,
	})
}

func (s *Server) handleStartResumableUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPath       string  `json:"local_path"`
		ParentID        *string `json:"parent_id"`
		SessionID       string  `json:"session_id"`
		StopAfterChunks int     `json:"stop_after_chunks,omitempty"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	snap, err := s.jobs.startResumableUpload(
		s.localDir,
		s.remote,
		req.LocalPath,
		req.ParentID,
		strings.TrimSpace(req.SessionID),
		req.StopAfterChunks,
	)
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusAccepted, snap)
}

func (s *Server) handleGetUploadJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.get(r.PathValue("id"))
	if !ok {
		writeGUIError(w, http.StatusNotFound, "upload job not found")
		return
	}
	writeGUIJSON(w, http.StatusOK, job.snapshot())
}

func (s *Server) handleCancelUploadJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.get(r.PathValue("id"))
	if !ok {
		writeGUIError(w, http.StatusNotFound, "upload job not found")
		return
	}
	job.cancel()
	<-job.done
	writeGUIJSON(w, http.StatusOK, job.snapshot())
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FileID         string `json:"file_id"`
		FileName       string `json:"file_name"`
		ExpectedSHA256 string `json:"expected_sha256"`
		LocalDir       string `json:"local_dir"`
		ConflictPolicy string `json:"conflict_policy"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	res, conflict, err := downloadStreamToLocal(
		s.localDir,
		req.LocalDir,
		req.FileName,
		req.ExpectedSHA256,
		req.ConflictPolicy,
		func() (io.ReadCloser, error) {
			return s.remote.OpenFileDownload(r.Context(), req.FileID)
		},
	)
	if conflict != nil {
		writeGUIJSON(w, http.StatusConflict, map[string]any{
			"conflict":       true,
			"target_path":    conflict.TargetPath,
			"suggested_name": conflict.SuggestedName,
			"error":          "local file already exists",
		})
		return
	}
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, res)
}

func (s *Server) handleDownloadShared(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ShareID        string `json:"share_id"`
		FileID         string `json:"file_id"`
		FileName       string `json:"file_name"`
		ExpectedSHA256 string `json:"expected_sha256"`
		LocalDir       string `json:"local_dir"`
		ConflictPolicy string `json:"conflict_policy"`
	}
	if !decodeGUIJSON(w, r, &req) {
		return
	}
	res, conflict, err := downloadStreamToLocal(
		s.localDir,
		req.LocalDir,
		req.FileName,
		req.ExpectedSHA256,
		req.ConflictPolicy,
		func() (io.ReadCloser, error) {
			return s.remote.OpenSharedFileDownload(r.Context(), req.ShareID, req.FileID)
		},
	)
	if conflict != nil {
		writeGUIJSON(w, http.StatusConflict, map[string]any{
			"conflict":       true,
			"target_path":    conflict.TargetPath,
			"suggested_name": conflict.SuggestedName,
			"error":          "local file already exists",
		})
		return
	}
	if err != nil {
		code, msg := formatStatusCode(err)
		writeGUIError(w, code, msg)
		return
	}
	writeGUIJSON(w, http.StatusOK, res)
}

func decodeGUIJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeGUIError(w, http.StatusBadRequest, "invalid JSON request body")
		return false
	}
	return true
}

func writeGUIJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeGUIError(w http.ResponseWriter, status int, msg string) {
	writeGUIJSON(w, status, map[string]string{"error": msg})
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
