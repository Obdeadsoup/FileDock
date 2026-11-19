package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"example.com/filedock/internal/files"
)

const multipartBodyOverhead int64 = 1 << 20 // 1 MiB for multipart headers and boundaries.

var errMultipleFileParts = errors.New("multiple file parts are not allowed")

type badRequestError struct {
	err error
}

func (e badRequestError) Error() string { return e.err.Error() }
func (e badRequestError) Unwrap() error { return e.err }

type fileResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type handler struct {
	svc *files.Service
}

// NewHandler constructs the HTTP router for FileDock endpoints.
func NewHandler(svc *files.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health)

	h := &handler{svc: svc}
	mux.HandleFunc("POST /api/v1/files", h.uploadFile)
	mux.HandleFunc("GET /api/v1/files", h.listFiles)
	mux.HandleFunc("GET /api/v1/files/{id}/content", h.downloadFile)
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

func (h *handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "file service unavailable")
		return
	}

	maxBodyBytes := h.svc.MaxFileSize() + multipartBodyOverhead
	if maxBodyBytes <= 0 {
		maxBodyBytes = h.svc.MaxFileSize()
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "request must be valid multipart/form-data")
		return
	}

	nextPart := func() (string, io.ReadCloser, error) {
		p, nextErr := mr.NextPart()
		if nextErr != nil {
			return "", nil, nextErr
		}
		return p.FormName(), p, nil
	}

	for {
		formName, part, err := nextPart()
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

		// Extract filename before streaming body through singleFilePartReader.
		type fileNamer interface {
			FileName() string
		}
		filename := ""
		if fn, ok := part.(fileNamer); ok {
			filename = fn.FileName()
		}

		stream := &singleFilePartReader{
			part:     part,
			nextPart: nextPart,
		}
		file, uploadErr := h.svc.Upload(r.Context(), filename, stream)
		_ = part.Close()
		if uploadErr != nil {
			writeUploadError(w, uploadErr)
			return
		}

		writeJSON(w, http.StatusCreated, toFileResponse(file))
		return
	}
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "file service unavailable")
		return
	}

	q := r.URL.Query()
	limit := 0
	if q.Has("limit") {
		raw := q.Get("limit")
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = parsed
	}

	offset := 0
	if q.Has("offset") {
		raw := q.Get("offset")
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = parsed
	}

	items, err := h.svc.List(r.Context(), limit, offset)
	if err != nil {
		if errors.Is(err, files.ErrInvalidPagination) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to list files")
		return
	}

	resp := make([]fileResponse, 0, len(items))
	for _, item := range items {
		resp = append(resp, toFileResponse(item))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "file service unavailable")
		return
	}

	id := r.PathValue("id")
	file, rc, err := h.svc.OpenContent(r.Context(), id)
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

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", formatAttachmentDisposition(file.Name))
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, rc)
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
	writeError(w, http.StatusInternalServerError, "failed to store file")
}

func toFileResponse(f files.File) fileResponse {
	return fileResponse{
		ID:        f.ID,
		Name:      f.Name,
		Size:      f.Size,
		SHA256:    f.SHA256,
		CreatedAt: f.CreatedAt.UTC(),
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
