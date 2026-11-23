package httpapi

import (
	"net/http"
	"strconv"

	"example.com/filedock/internal/files"
)

func (h *handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}
	if err := h.files.DeleteFile(r.Context(), ownerID, r.PathValue("id")); err != nil {
		writeTreeError(w, err, "failed to delete file")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) searchFiles(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}
	filter, ok := querySearchFilter(w, r)
	if !ok {
		return
	}

	items, err := h.files.Search(r.Context(), ownerID, filter)
	if err != nil {
		writeTreeError(w, err, "failed to search files")
		return
	}

	resp := make([]fileResponse, 0, len(items))
	for _, item := range items {
		resp = append(resp, toFileResponse(item))
	}
	writeJSON(w, http.StatusOK, resp)
}

func querySearchFilter(w http.ResponseWriter, r *http.Request) (files.SearchFilter, bool) {
	parentID, ok := queryParent(w, r)
	if !ok {
		return files.SearchFilter{}, false
	}
	limit, offset, ok := queryPage(w, r)
	if !ok {
		return files.SearchFilter{}, false
	}

	q := r.URL.Query()
	var queryText string
	if values, exists := q["q"]; exists {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "q must be a single value")
			return files.SearchFilter{}, false
		}
		queryText = values[0]
	}

	minSize, ok := parseOptionalSizeParam(w, q["min_size"], "min_size")
	if !ok {
		return files.SearchFilter{}, false
	}
	maxSize, ok := parseOptionalSizeParam(w, q["max_size"], "max_size")
	if !ok {
		return files.SearchFilter{}, false
	}
	if minSize != nil && maxSize != nil && *minSize > *maxSize {
		writeError(w, http.StatusBadRequest, "min_size must not exceed max_size")
		return files.SearchFilter{}, false
	}

	return files.SearchFilter{
		Query:    queryText,
		MinSize:  minSize,
		MaxSize:  maxSize,
		ParentID: parentID,
		Limit:    limit,
		Offset:   offset,
	}, true
}

func parseOptionalSizeParam(w http.ResponseWriter, values []string, name string) (*int64, bool) {
	if len(values) == 0 {
		return nil, true
	}
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, name+" must be a single non-negative integer")
		return nil, false
	}
	parsed, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || parsed < 0 {
		writeError(w, http.StatusBadRequest, name+" must be a non-negative integer")
		return nil, false
	}
	return &parsed, true
}
