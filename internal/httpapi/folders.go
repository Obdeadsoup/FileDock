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

type folderResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ParentID  *string   `json:"parent_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toFolderResponse(folder files.Folder) folderResponse {
	return folderResponse{
		ID: folder.ID, Name: folder.Name, ParentID: folder.ParentID,
		CreatedAt: folder.CreatedAt.UTC(), UpdatedAt: folder.UpdatedAt.UTC(),
	}
}

func (h *handler) authenticateFiles(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.files == nil {
		writeError(w, http.StatusServiceUnavailable, "file service unavailable")
		return "", false
	}
	user, ok := h.authenticate(w, r)
	return user.ID, ok
}

func (h *handler) createFolder(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}
	fields, ok := decodeMetadata(w, r)
	if !ok {
		return
	}
	if fields.Name == nil {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	folder, err := h.files.CreateFolder(r.Context(), ownerID, fields.ParentID, *fields.Name)
	if err != nil {
		writeTreeError(w, err, "failed to create folder")
		return
	}
	writeJSON(w, http.StatusCreated, toFolderResponse(folder))
}

func (h *handler) listFolders(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
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
	items, err := h.files.ListFolders(r.Context(), ownerID, parentID, limit, offset)
	if err != nil {
		writeTreeError(w, err, "failed to list folders")
		return
	}
	response := make([]folderResponse, 0, len(items))
	for _, item := range items {
		response = append(response, toFolderResponse(item))
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *handler) updateFolder(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}
	changes, ok := decodeMetadata(w, r)
	if !ok {
		return
	}
	item, err := h.files.UpdateFolder(r.Context(), ownerID, r.PathValue("id"), changes)
	if err != nil {
		writeTreeError(w, err, "failed to update folder")
		return
	}
	writeJSON(w, http.StatusOK, toFolderResponse(item))
}

func (h *handler) deleteFolder(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}
	if err := h.files.DeleteFolder(r.Context(), ownerID, r.PathValue("id")); err != nil {
		writeTreeError(w, err, "failed to delete folder")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) updateFile(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}
	changes, ok := decodeMetadata(w, r)
	if !ok {
		return
	}
	item, err := h.files.UpdateFile(r.Context(), ownerID, r.PathValue("id"), changes)
	if err != nil {
		writeTreeError(w, err, "failed to update file")
		return
	}
	writeJSON(w, http.StatusOK, toFileResponse(item))
}

func decodeMetadata(w http.ResponseWriter, r *http.Request) (files.Changes, bool) {
	var payload *struct {
		Name     json.RawMessage `json:"name"`
		ParentID json.RawMessage `json:"parent_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload == nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "request must be a JSON object with name or parent_id")
		return files.Changes{}, false
	}
	var changes files.Changes
	if payload.Name != nil {
		if err := json.Unmarshal(payload.Name, &changes.Name); err != nil || changes.Name == nil {
			writeError(w, http.StatusBadRequest, "name must be a string")
			return files.Changes{}, false
		}
	}
	changes.SetParent = payload.ParentID != nil
	if changes.SetParent {
		if err := json.Unmarshal(payload.ParentID, &changes.ParentID); err != nil {
			writeError(w, http.StatusBadRequest, "parent_id must be a string or null")
			return files.Changes{}, false
		}
	}
	return changes, true
}

func queryParent(w http.ResponseWriter, r *http.Request) (*string, bool) {
	// Parse only the URL, even when the original request carries an upload.
	query := http.Request{Method: http.MethodGet, URL: r.URL}
	if err := query.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid query string")
		return nil, false
	}
	values := query.Form["parent_id"]
	if len(values) == 0 {
		return nil, true
	}
	if len(values) != 1 || files.ValidateID(values[0]) != nil {
		writeError(w, http.StatusBadRequest, "parent_id must be a single directory ID")
		return nil, false
	}
	return &values[0], true
}

func queryPage(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	q := r.URL.Query()
	if q.Has("limit") {
		parsed, err := strconv.Atoi(q.Get("limit"))
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return 0, 0, false
		}
		limit = parsed
	}
	if q.Has("offset") {
		parsed, err := strconv.Atoi(q.Get("offset"))
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = parsed
	}
	return limit, offset, true
}

func writeTreeError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrFolderNotFound):
		if errors.Is(err, files.ErrFolderNotFound) {
			writeError(w, http.StatusNotFound, files.ErrFolderNotFound.Error())
		} else {
			writeError(w, http.StatusNotFound, files.ErrNotFound.Error())
		}
	case errors.Is(err, files.ErrFolderCycle), errors.Is(err, files.ErrFolderNotEmpty), errors.Is(err, files.ErrFolderNameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, files.ErrInvalidID), errors.Is(err, files.ErrInvalidFilename),
		errors.Is(err, files.ErrEmptyChanges), errors.Is(err, files.ErrInvalidPagination),
		errors.Is(err, files.ErrInvalidSearchFilter), errors.Is(err, files.ErrInvalidSyncCursor):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, fallback)
	}
}
