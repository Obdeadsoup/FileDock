package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"example.com/filedock/internal/files"
)

type createShareRequest struct {
	ResourceType      string `json:"resource_type"`
	ResourceID        string `json:"resource_id"`
	RecipientUsername string `json:"recipient_username"`
}

type shareResponse struct {
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

func toShareResponse(s files.Share) shareResponse {
	return shareResponse{
		ID:                s.ID,
		ResourceType:      s.ResourceType,
		ResourceID:        s.ResourceID,
		ResourceName:      s.ResourceName,
		OwnerID:           s.OwnerID,
		OwnerUsername:     s.OwnerUsername,
		RecipientID:       s.RecipientID,
		RecipientUsername: s.RecipientUsername,
		Permission:        s.Permission,
		CreatedAt:         s.CreatedAt.UTC(),
	}
}

func (h *handler) createShare(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxCredentialsBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req createShareRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum allowed size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON share payload")
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds maximum allowed size")
			return
		}
		writeError(w, http.StatusBadRequest, "unexpected trailing data in JSON payload")
		return
	}

	share, created, err := h.files.CreateShare(r.Context(), ownerID, files.CreateShareInput{
		ResourceType:      req.ResourceType,
		ResourceID:        req.ResourceID,
		RecipientUsername: req.RecipientUsername,
	})
	if err != nil {
		writeShareError(w, err, "failed to create share")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, toShareResponse(share))
}

func (h *handler) listShares(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	dirs := q["direction"]
	if len(dirs) != 1 || (dirs[0] != files.ShareDirectionSent && dirs[0] != files.ShareDirectionReceived) {
		writeError(w, http.StatusBadRequest, files.ErrInvalidShareDirection.Error())
		return
	}
	limit, offset, ok := queryPage(w, r)
	if !ok {
		return
	}

	items, err := h.files.ListShares(r.Context(), userID, dirs[0], limit, offset)
	if err != nil {
		writeShareError(w, err, "failed to list shares")
		return
	}

	resp := make([]shareResponse, 0, len(items))
	for _, item := range items {
		resp = append(resp, toShareResponse(item))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) deleteShare(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}
	if err := h.files.DeleteShare(r.Context(), ownerID, r.PathValue("share_id")); err != nil {
		writeShareError(w, err, "failed to revoke share")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) listSharedFolders(w http.ResponseWriter, r *http.Request) {
	recipientID, ok := h.authenticateFiles(w, r)
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

	items, err := h.files.ListSharedFolders(r.Context(), recipientID, r.PathValue("share_id"), parentID, limit, offset)
	if err != nil {
		writeShareError(w, err, "failed to list shared folders")
		return
	}

	resp := make([]folderResponse, 0, len(items))
	for _, item := range items {
		resp = append(resp, toFolderResponse(item))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) listSharedFiles(w http.ResponseWriter, r *http.Request) {
	recipientID, ok := h.authenticateFiles(w, r)
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

	items, err := h.files.ListSharedFiles(r.Context(), recipientID, r.PathValue("share_id"), parentID, limit, offset)
	if err != nil {
		writeShareError(w, err, "failed to list shared files")
		return
	}

	resp := make([]fileResponse, 0, len(items))
	for _, item := range items {
		resp = append(resp, toFileResponse(item))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) downloadSharedFile(w http.ResponseWriter, r *http.Request) {
	recipientID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}

	file, rc, err := h.files.OpenSharedContent(r.Context(), recipientID, r.PathValue("share_id"), r.PathValue("file_id"))
	if err != nil {
		writeShareError(w, err, "failed to open shared file")
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

func writeShareError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, files.ErrShareNotFound),
		errors.Is(err, files.ErrRecipientNotFound),
		errors.Is(err, files.ErrNotFound),
		errors.Is(err, files.ErrFolderNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, files.ErrInvalidID),
		errors.Is(err, files.ErrInvalidResourceType),
		errors.Is(err, files.ErrInvalidShareDirection),
		errors.Is(err, files.ErrInvalidRecipient),
		errors.Is(err, files.ErrSelfShare),
		errors.Is(err, files.ErrInvalidPagination):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, fallback)
	}
}
