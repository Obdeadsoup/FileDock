package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
)

const (
	multipartBodyOverhead   int64 = 1 << 20 // 1 MiB for multipart headers and boundaries.
	maxCredentialsBodyBytes int64 = 4 << 10 // 4 KiB limit for auth JSON payloads.
	uniformLoginFailedMsg         = "invalid username or password"
)

var errMultipleFileParts = errors.New("multiple file parts are not allowed")

type badRequestError struct {
	err error
}

func (e badRequestError) Error() string { return e.err.Error() }
func (e badRequestError) Unwrap() error { return e.err }

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResponse struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

type loginResponse struct {
	Token     string       `json:"token"`
	ExpiresAt time.Time    `json:"expires_at"`
	User      userResponse `json:"user"`
}

type fileResponse struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	ParentID        *string   `json:"parent_id"`
	Size            int64     `json:"size"`
	SHA256          string    `json:"sha256"`
	ContentRevision int64     `json:"content_revision"`
	CreatedAt       time.Time `json:"created_at"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type handler struct {
	files    *files.Service
	accounts *accounts.Service
}

// NewHandler constructs the HTTP router for FileDock health, auth, and file endpoints.
func NewHandler(fileSvc *files.Service, accountSvc *accounts.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health)

	h := &handler{
		files:    fileSvc,
		accounts: accountSvc,
	}
	mux.HandleFunc("POST /api/v1/auth/register", h.register)
	mux.HandleFunc("POST /api/v1/auth/login", h.login)
	mux.HandleFunc("POST /api/v1/auth/logout", h.logout)
	mux.HandleFunc("GET /api/v1/auth/me", h.me)

	mux.HandleFunc("POST /api/v1/files", h.uploadFile)
	mux.HandleFunc("GET /api/v1/files", h.listFiles)
	mux.HandleFunc("GET /api/v1/files/search", h.searchFiles)
	mux.HandleFunc("GET /api/v1/files/{id}/content", h.downloadFile)
	mux.HandleFunc("PUT /api/v1/files/{id}/content", h.replaceFileContent)
	mux.HandleFunc("PATCH /api/v1/files/{id}", h.updateFile)
	mux.HandleFunc("DELETE /api/v1/files/{id}", h.deleteFile)
	mux.HandleFunc("POST /api/v1/folders", h.createFolder)
	mux.HandleFunc("GET /api/v1/folders", h.listFolders)
	mux.HandleFunc("PATCH /api/v1/folders/{id}", h.updateFolder)
	mux.HandleFunc("DELETE /api/v1/folders/{id}", h.deleteFolder)

	mux.HandleFunc("POST /api/v1/shares", h.createShare)
	mux.HandleFunc("GET /api/v1/shares", h.listShares)
	mux.HandleFunc("DELETE /api/v1/shares/{share_id}", h.deleteShare)
	mux.HandleFunc("GET /api/v1/shares/{share_id}/folders", h.listSharedFolders)
	mux.HandleFunc("GET /api/v1/shares/{share_id}/files", h.listSharedFiles)
	mux.HandleFunc("GET /api/v1/shares/{share_id}/files/{file_id}/content", h.downloadSharedFile)

	mux.HandleFunc("GET /api/v1/sync/changes", h.listSyncChanges)

	mux.HandleFunc("POST /api/v1/uploads", h.createUploadSession)
	mux.HandleFunc("GET /api/v1/uploads/{id}", h.getUploadSession)
	mux.HandleFunc("PUT /api/v1/uploads/{id}/chunks/{index}", h.uploadChunk)
	mux.HandleFunc("POST /api/v1/uploads/{id}/complete", h.completeUploadSession)
	return mux
}

func health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	if h.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "account service unavailable")
		return
	}

	req, ok := decodeCredentialsRequest(w, r)
	if !ok {
		return
	}

	user, err := h.accounts.Register(r.Context(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, accounts.ErrInvalidUsername), errors.Is(err, accounts.ErrInvalidPassword):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, accounts.ErrUsernameTaken):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "failed to register user")
		}
		return
	}

	writeJSON(w, http.StatusCreated, toUserResponse(user))
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	if h.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "account service unavailable")
		return
	}

	req, ok := decodeCredentialsRequest(w, r)
	if !ok {
		return
	}

	result, err := h.accounts.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, accounts.ErrInvalidUsername), errors.Is(err, accounts.ErrInvalidPassword):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, accounts.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, uniformLoginFailedMsg)
		default:
			writeError(w, http.StatusInternalServerError, "failed to process login")
		}
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{
		Token:     result.Token,
		ExpiresAt: result.ExpiresAt.UTC(),
		User:      toUserResponse(result.User),
	})
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if h.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "account service unavailable")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxCredentialsBodyBytes)
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum allowed size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	token, ok := extractBearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if err := h.accounts.Logout(r.Context(), token); err != nil {
		if errors.Is(err, accounts.ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to process logout")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	user, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(user))
}

func (h *handler) authenticate(w http.ResponseWriter, r *http.Request) (accounts.User, bool) {
	if h.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "account service unavailable")
		return accounts.User{}, false
	}
	token, ok := extractBearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return accounts.User{}, false
	}
	user, err := h.accounts.Authenticate(r.Context(), token)
	if err != nil {
		if errors.Is(err, accounts.ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return accounts.User{}, false
		}
		writeError(w, http.StatusInternalServerError, "failed to verify session")
		return accounts.User{}, false
	}
	return user, true
}

func extractBearerToken(r *http.Request) (string, bool) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(authHeader, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func decodeCredentialsRequest(w http.ResponseWriter, r *http.Request) (credentialsRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCredentialsBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req credentialsRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum allowed size")
			return credentialsRequest{}, false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON credentials payload")
		return credentialsRequest{}, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum allowed size")
			return credentialsRequest{}, false
		}
		writeError(w, http.StatusBadRequest, "unexpected trailing data in JSON payload")
		return credentialsRequest{}, false
	}
	return req, true
}

func (h *handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	if h.files == nil {
		writeError(w, http.StatusServiceUnavailable, "file service unavailable")
		return
	}
	user, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	parentID, ok := queryParent(w, r)
	if !ok {
		return
	}
	if err := h.files.CheckParent(r.Context(), user.ID, parentID); err != nil {
		writeTreeError(w, err, "failed to check upload destination")
		return
	}

	maxBodyBytes := h.files.MaxFileSize() + multipartBodyOverhead
	if maxBodyBytes <= 0 {
		maxBodyBytes = h.files.MaxFileSize()
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "request must be valid multipart/form-data")
		return
	}

	nextPart := func() (string, string, io.ReadCloser, error) {
		p, nextErr := mr.NextPart()
		if nextErr != nil {
			return "", "", nil, nextErr
		}
		formName, rawFilename, dispErr := parsePartContentDisposition(p.Header.Get("Content-Disposition"))
		if dispErr != nil {
			_ = p.Close()
			return "", "", nil, badRequestError{err: dispErr}
		}
		return formName, rawFilename, p, nil
	}

	for {
		formName, filename, part, err := nextPart()
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "missing file field")
			return
		}
		if err != nil {
			writeMultipartReadError(w, err)
			return
		}

		if formName != "file" {
			_, drainErr := io.Copy(io.Discard, part)
			_ = part.Close()
			if drainErr != nil {
				writeMultipartReadError(w, drainErr)
				return
			}
			continue
		}

		stream := &singleFilePartReader{
			part: part,
			nextPart: func() (string, io.ReadCloser, error) {
				name, _, nextRC, nextErr := nextPart()
				return name, nextRC, nextErr
			},
		}
		file, uploadErr := h.files.UploadTo(r.Context(), user.ID, parentID, filename, stream)
		_ = part.Close()
		if uploadErr != nil {
			writeUploadError(w, uploadErr)
			return
		}

		w.Header().Set("ETag", files.ContentETag(file))
		writeJSON(w, http.StatusCreated, toFileResponse(file))
		return
	}
}

// parsePartContentDisposition extracts the form field name and raw un-normalized filename
// from a multipart Content-Disposition header before any OS-specific filepath.Base stripping.
func parsePartContentDisposition(rawHeader string) (formName, filename string, err error) {
	if rawHeader == "" {
		return "", "", errors.New("missing Content-Disposition header")
	}
	mediaType, params, err := mime.ParseMediaType(preserveBackslashesInDisposition(rawHeader))
	if err != nil || mediaType != "form-data" {
		return "", "", errors.New("invalid Content-Disposition header")
	}
	return params["name"], params["filename"], nil
}

// preserveBackslashesInDisposition keeps raw single backslashes (such as unescaped Windows
// path separators in hand-crafted headers) intact when mime.ParseMediaType unescapes quoted-pairs.
func preserveBackslashesInDisposition(raw string) string {
	var out strings.Builder
	out.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			if i+1 < len(raw) && (raw[i+1] == '\\' || raw[i+1] == '"') {
				out.WriteByte('\\')
				out.WriteByte(raw[i+1])
				i++
				continue
			}
			out.WriteString(`\\`)
			continue
		}
		out.WriteByte(raw[i])
	}
	return out.String()
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	if h.files == nil {
		writeError(w, http.StatusServiceUnavailable, "file service unavailable")
		return
	}
	user, ok := h.authenticate(w, r)
	if !ok {
		return
	}

	parentID, ok := queryParent(w, r)
	if !ok {
		return
	}
	limit, offset, ok := queryPage(w, r)
	if !ok {
		return
	}
	items, err := h.files.ListIn(r.Context(), user.ID, parentID, limit, offset)
	if err != nil {
		writeTreeError(w, err, "failed to list files")
		return
	}

	resp := make([]fileResponse, 0, len(items))
	for _, item := range items {
		resp = append(resp, toFileResponse(item))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	if h.files == nil {
		writeError(w, http.StatusServiceUnavailable, "file service unavailable")
		return
	}
	user, ok := h.authenticate(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	file, rc, err := h.files.OpenContent(r.Context(), user.ID, id)
	if err != nil {
		switch {
		case errors.Is(err, files.ErrInvalidID):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, files.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "failed to open file")
		}
		return
	}
	defer rc.Close()

	streamContentResponse(w, r, file, rc, true)
}

func streamContentResponse(w http.ResponseWriter, r *http.Request, file files.File, rc io.Reader, includeVersionHeaders bool) {
	var (
		firstChunk []byte
		firstErr   error
	)
	if r.Method != http.MethodHead {
		buf := make([]byte, 32*1024)
		n, err := rc.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusInternalServerError, "failed to read file content")
			return
		}
		firstChunk = buf[:n]
		firstErr = err
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", formatAttachmentDisposition(file.Name))
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.Header().Set("ETag", files.ContentETag(file))
	w.Header().Set("X-FileDock-SHA256", file.SHA256)
	if includeVersionHeaders {
		rev := file.ContentRevision
		if rev <= 0 {
			rev = 1
		}
		w.Header().Set("X-FileDock-Content-Revision", strconv.FormatInt(rev, 10))
	}
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodHead {
		return
	}
	if len(firstChunk) > 0 {
		if _, err := w.Write(firstChunk); err != nil {
			return
		}
	}
	if errors.Is(firstErr, io.EOF) {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		if r.Context().Value(http.ServerContextKey) != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

type singleFilePartReader struct {
	part      io.Reader
	nextPart  func() (string, io.ReadCloser, error)
	eofTested bool
}

func (s *singleFilePartReader) Read(p []byte) (int, error) {
	if s.eofTested {
		return 0, io.EOF
	}
	n, err := s.part.Read(p)
	if !errors.Is(err, io.EOF) {
		if err != nil {
			return n, classifyStreamReadError(err)
		}
		return n, nil
	}
	s.eofTested = true
	for {
		formName, next, nextErr := s.nextPart()
		if errors.Is(nextErr, io.EOF) {
			return n, io.EOF
		}
		if nextErr != nil {
			return n, classifyStreamReadError(nextErr)
		}
		if formName == "file" {
			_ = next.Close()
			return n, errMultipleFileParts
		}
		_, drainErr := io.Copy(io.Discard, next)
		_ = next.Close()
		if drainErr != nil {
			return n, classifyStreamReadError(drainErr)
		}
	}
}

func classifyStreamReadError(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return err
	}
	return badRequestError{err: err}
}

func writeMultipartReadError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum allowed size")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid multipart payload")
}

func writeUploadError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) || errors.Is(err, files.ErrFileTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "file exceeds maximum allowed size")
		return
	}
	var badReq badRequestError
	if errors.Is(err, files.ErrInvalidFilename) || errors.Is(err, errMultipleFileParts) || errors.As(err, &badReq) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeTreeError(w, err, "failed to store file")
}

func toUserResponse(u accounts.User) userResponse {
	return userResponse{
		ID:        u.ID,
		Username:  u.Username,
		CreatedAt: u.CreatedAt.UTC(),
	}
}

func toFileResponse(f files.File) fileResponse {
	rev := f.ContentRevision
	if rev <= 0 {
		rev = 1
	}
	return fileResponse{
		ID:              f.ID,
		Name:            f.Name,
		ParentID:        f.ParentID,
		Size:            f.Size,
		SHA256:          f.SHA256,
		ContentRevision: rev,
		CreatedAt:       f.CreatedAt.UTC(),
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func formatAttachmentDisposition(name string) string {
	var fallback strings.Builder
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b >= 0x20 && b <= 0x7e && b != '"' && b != '\\' && b != ';' {
			fallback.WriteByte(b)
		} else if b >= 0x80 {
			fallback.WriteByte('_')
		}
	}
	asciiName := fallback.String()
	if asciiName == "" {
		asciiName = "download"
	}

	var encoded strings.Builder
	const hexDigits = "0123456789ABCDEF"
	for i := 0; i < len(name); i++ {
		b := name[i]
		if isRFC5987AttrChar(b) {
			encoded.WriteByte(b)
		} else {
			encoded.WriteByte('%')
			encoded.WriteByte(hexDigits[b>>4])
			encoded.WriteByte(hexDigits[b&0x0f])
		}
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, asciiName, encoded.String())
}

func isRFC5987AttrChar(b byte) bool {
	if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') {
		return true
	}
	switch b {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}
