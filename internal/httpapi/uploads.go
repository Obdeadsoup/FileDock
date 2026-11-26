package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"example.com/filedock/internal/files"
)

const maxUploadMetaBodyBytes int64 = 16 << 10 // 16 KiB limit for upload session JSON payloads.

type createUploadSessionRequest struct {
	ParentID *string `json:"parent_id"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	SHA256   string  `json:"sha256"`
}

type uploadSessionResponse struct {
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

type uploadChunkResponse struct {
	SessionID string `json:"session_id"`
	Index     int    `json:"index"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

func toUploadSessionResponse(s files.UploadSession) uploadSessionResponse {
	chunks := s.UploadedChunks
	if chunks == nil {
		chunks = []int{}
	}
	var completedAt *time.Time
	if s.CompletedAt != nil {
		utc := s.CompletedAt.UTC()
		completedAt = &utc
	}
	return uploadSessionResponse{
		ID:             s.ID,
		Name:           s.Name,
		ParentID:       s.ParentID,
		Size:           s.Size,
		SHA256:         s.SHA256,
		ChunkSize:      s.ChunkSize,
		TotalChunks:    s.TotalChunks,
		Status:         s.Status,
		UploadedChunks: chunks,
		UploadedBytes:  s.UploadedBytes,
		FileID:         s.FileID,
		CreatedAt:      s.CreatedAt.UTC(),
		ExpiresAt:      s.ExpiresAt.UTC(),
		CompletedAt:    completedAt,
	}
}

func (h *handler) createUploadSession(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadMetaBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req createUploadSessionRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum allowed size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON upload session payload")
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "unexpected trailing data in JSON payload")
		return
	}

	session, err := h.files.CreateUploadSession(r.Context(), ownerID, files.CreateUploadRequest{
		ParentID: req.ParentID,
		Name:     req.Name,
		Size:     req.Size,
		SHA256:   req.SHA256,
	})
	if err != nil {
		writeUploadSessionError(w, err, "failed to create upload session")
		return
	}
	writeJSON(w, http.StatusCreated, toUploadSessionResponse(session))
}

func (h *handler) getUploadSession(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}

	session, err := h.files.GetUploadSession(r.Context(), ownerID, r.PathValue("id"))
	if err != nil {
		writeUploadSessionError(w, err, "failed to get upload session")
		return
	}
	writeJSON(w, http.StatusOK, toUploadSessionResponse(session))
}

func (h *handler) uploadChunk(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}

	rawIndex := strings.TrimSpace(r.PathValue("index"))
	index, err := strconv.Atoi(rawIndex)
	if err != nil || index < 0 {
		writeError(w, http.StatusBadRequest, "invalid chunk index")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.files.UploadChunkSize())
	chunk, err := h.files.UploadChunk(
		r.Context(),
		ownerID,
		r.PathValue("id"),
		index,
		r.Header.Get("X-Chunk-SHA256"),
		putStreamReader{r: r.Body},
	)
	if err != nil {
		writeUploadSessionError(w, err, "failed to upload chunk")
		return
	}

	writeJSON(w, http.StatusOK, uploadChunkResponse{
		SessionID: chunk.SessionID,
		Index:     chunk.Index,
		Size:      chunk.Size,
		SHA256:    chunk.SHA256,
	})
}

func (h *handler) completeUploadSession(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadMetaBodyBytes)
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum allowed size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	file, err := h.files.CompleteUploadSession(r.Context(), ownerID, r.PathValue("id"))
	if err != nil {
		writeUploadSessionError(w, err, "failed to complete upload session")
		return
	}
	w.Header().Set("ETag", files.ContentETag(file))
	w.Header().Set("X-FileDock-Content-Revision", strconv.FormatInt(file.ContentRevision, 10))
	w.Header().Set("X-FileDock-SHA256", file.SHA256)
	writeJSON(w, http.StatusOK, toFileResponse(file))
}

func writeUploadSessionError(w http.ResponseWriter, err error, fallback string) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxErr), errors.Is(err, files.ErrFileTooLarge), errors.Is(err, files.ErrUploadQuotaExceeded):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, files.ErrUploadSessionNotFound), errors.Is(err, files.ErrFolderNotFound), errors.Is(err, files.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, files.ErrUploadSessionExpired):
		writeError(w, http.StatusGone, err.Error())
	case errors.Is(err, files.ErrChunkConflict), errors.Is(err, files.ErrIncompleteUpload), errors.Is(err, files.ErrUploadSessionClosed):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, files.ErrInvalidFilename),
		errors.Is(err, files.ErrInvalidID),
		errors.Is(err, files.ErrInvalidOwner),
		errors.Is(err, files.ErrInvalidUploadSession),
		errors.Is(err, files.ErrInvalidChunkIndex),
		errors.Is(err, files.ErrChunkSizeMismatch),
		errors.Is(err, files.ErrChunkHashMismatch),
		errors.Is(err, files.ErrUploadHashMismatch):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, fallback)
	}
}
