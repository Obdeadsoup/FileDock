package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"example.com/filedock/internal/files"
)

type putStreamReader struct {
	r io.Reader
}

func (p putStreamReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, classifyStreamReadError(err)
	}
	return n, err
}

func (h *handler) replaceFileContent(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.authenticateFiles(w, r)
	if !ok {
		return
	}

	ifMatchValues := r.Header.Values("If-Match")
	if len(ifMatchValues) == 0 || (len(ifMatchValues) == 1 && strings.TrimSpace(ifMatchValues[0]) == "") {
		writeError(w, http.StatusPreconditionRequired, "If-Match header with current strong ETag is required")
		return
	}
	if len(ifMatchValues) != 1 {
		writeError(w, http.StatusBadRequest, files.ErrInvalidIfMatch.Error())
		return
	}
	expectedETag := strings.TrimSpace(ifMatchValues[0])
	if err := files.ValidateStrongETag(expectedETag); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	maxBodyBytes := h.files.MaxFileSize()
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	updated, err := h.files.ReplaceContent(r.Context(), ownerID, r.PathValue("id"), expectedETag, putStreamReader{r: r.Body})
	if err != nil {
		writeContentUpdateError(w, err)
		return
	}

	w.Header().Set("ETag", files.ContentETag(updated))
	writeJSON(w, http.StatusOK, toFileResponse(updated))
}

func writeContentUpdateError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) || errors.Is(err, files.ErrFileTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "file exceeds maximum allowed size")
		return
	}
	var badReq badRequestError
	switch {
	case errors.Is(err, files.ErrNotFound):
		writeError(w, http.StatusNotFound, files.ErrNotFound.Error())
	case errors.Is(err, files.ErrPreconditionFailed):
		writeError(w, http.StatusPreconditionFailed, err.Error())
	case errors.Is(err, files.ErrInvalidID), errors.Is(err, files.ErrInvalidIfMatch), errors.As(err, &badReq):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "failed to replace file content")
	}
}
