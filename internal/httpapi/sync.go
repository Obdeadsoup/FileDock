package httpapi

import (
	"net/http"
	"strconv"
	"time"
)

type syncChangeResponse struct {
	Seq        int64           `json:"seq"`
	Type       string          `json:"type"`
	ResourceID string          `json:"resource_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	File       *fileResponse   `json:"file,omitempty"`
	Folder     *folderResponse `json:"folder,omitempty"`
}

type syncChangesPageResponse struct {
	Items      []syncChangeResponse `json:"items"`
	NextCursor int64                `json:"next_cursor"`
	HasMore    bool                 `json:"has_more"`
}

func (h *handler) listSyncChanges(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}

	after, limit, ok := querySyncParams(w, r)
	if !ok {
		return
	}

	page, err := h.files.ListChanges(r.Context(), ownerID, after, limit)
	if err != nil {
		writeTreeError(w, err, "failed to list sync changes")
		return
	}

	items := make([]syncChangeResponse, 0, len(page.Items))
	for _, ev := range page.Items {
		item := syncChangeResponse{
			Seq:        ev.Seq,
			Type:       ev.Type,
			ResourceID: ev.ResourceID,
			OccurredAt: ev.OccurredAt.UTC(),
		}
		if ev.File != nil {
			f := toFileResponse(*ev.File)
			item.File = &f
		}
		if ev.Folder != nil {
			d := toFolderResponse(*ev.Folder)
			item.Folder = &d
		}
		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, syncChangesPageResponse{
		Items:      items,
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
	})
}

func querySyncParams(w http.ResponseWriter, r *http.Request) (after int64, limit int, ok bool) {
	q := r.URL.Query()
	if values, exists := q["after"]; exists {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "after must be a single non-negative integer")
			return 0, 0, false
		}
		parsed, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return 0, 0, false
		}
		after = parsed
	}
	if values, exists := q["limit"]; exists {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "limit must be a single positive integer")
			return 0, 0, false
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return 0, 0, false
		}
		limit = parsed
	}
	return after, limit, true
}
