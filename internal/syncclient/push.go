package syncclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var (
	ErrNotInitialized  = errors.New("mirror directory is not initialized; run pull first")
	ErrUntrackedFile   = errors.New("file is not tracked in local sync manifest")
	ErrMissingBaseline = errors.New("local manifest lacks content_revision baseline; pull before pushing")
	ErrRemoteConflict  = errors.New("remote file version changed; pull and resolve conflict before pushing")
	ErrRemoteDeleted   = errors.New("remote file was deleted on server")
	ErrInvalidPushPath = errors.New("invalid relative file path inside mirror")
)

// PushConfig configures an explicit single-file content push from a synced mirror.
type PushConfig struct {
	ServerURL  string
	Token      string
	Dir        string
	RelPath    string
	HTTPClient *http.Client
	API        API
}

// PushResult summarizes the outcome of a Push operation.
type PushResult struct {
	UserID      string
	FileID      string
	LocalPath   string
	Skipped     bool
	Recovered   bool
	OldRevision int64
	NewRevision int64
	SHA256      string
	Size        int64
	Cursor      int64
}

// Push uploads the modified content of a single tracked regular file using its baseline revision
// and opaque server ETag as If-Match, without advancing the global pull cursor.
func Push(ctx context.Context, cfg PushConfig) (PushResult, error) {
	normalizedServer, err := NormalizeServerURL(cfg.ServerURL)
	if err != nil {
		return PushResult{}, err
	}
	if strings.TrimSpace(cfg.Dir) == "" {
		return PushResult{}, errors.New("local directory (--dir) is required")
	}
	relPath, err := normalizeMirrorRelPath(cfg.RelPath)
	if err != nil {
		return PushResult{}, err
	}

	api := cfg.API
	if api == nil {
		api, err = NewHTTPAPI(normalizedServer, cfg.Token, cfg.HTTPClient)
		if err != nil {
			return PushResult{}, err
		}
	}

	unlockInProc := lockDirInProcess(cfg.Dir)
	defer unlockInProc()

	root, err := os.OpenRoot(cfg.Dir)
	if err != nil {
		return PushResult{}, fmt.Errorf("open confined mirror root: %w", err)
	}
	defer root.Close()

	state, exists, err := loadState(root)
	if err != nil {
		return PushResult{}, err
	}
	if !exists {
		return PushResult{}, fmt.Errorf("%w: %s", ErrNotInitialized, cfg.Dir)
	}
	if state.ServerURL != normalizedServer {
		return PushResult{}, fmt.Errorf("%w: local=%q requested=%q", ErrServerMismatch, state.ServerURL, normalizedServer)
	}

	if err := ensureMetaDirs(root); err != nil {
		return PushResult{}, err
	}
	unlockFile, err := lockMirrorFile(root)
	if err != nil {
		return PushResult{}, err
	}
	defer unlockFile()

	// Re-load state after acquiring cross-process lock to ensure fresh manifest.
	state, exists, err = loadState(root)
	if err != nil {
		return PushResult{}, err
	}
	if !exists {
		return PushResult{}, fmt.Errorf("%w: %s", ErrNotInitialized, cfg.Dir)
	}
	if state.ServerURL != normalizedServer {
		return PushResult{}, fmt.Errorf("%w: local=%q requested=%q", ErrServerMismatch, state.ServerURL, normalizedServer)
	}

	me, err := api.Me(ctx)
	if err != nil {
		return PushResult{}, err
	}
	if state.UserID != me.ID {
		return PushResult{}, fmt.Errorf("%w: local=%q authenticated=%q", ErrUserMismatch, state.UserID, me.ID)
	}

	rec, found := findTrackedFileByPath(state.Files, relPath)
	if !found {
		return PushResult{}, fmt.Errorf("%w: %s", ErrUntrackedFile, relPath)
	}
	if rec.ContentRevision <= 0 || strings.TrimSpace(rec.SHA256) == "" {
		return PushResult{}, fmt.Errorf("%w: %s", ErrMissingBaseline, relPath)
	}

	// Verify local entry is a regular file (never follow symlinks or push directories).
	info, err := root.Lstat(relPath)
	if err != nil {
		return PushResult{}, fmt.Errorf("stat local file %q: %w", relPath, err)
	}
	if !info.Mode().IsRegular() {
		return PushResult{}, fmt.Errorf("%w: %q is not a regular file", ErrInvalidPushPath, relPath)
	}

	// Create a stable temporary snapshot and stream-compute its SHA-256 before contacting the server.
	snapPath := tmpDirRel + "/push-" + safeTempToken(rec.ID)
	snapSHA, snapSize, err := snapshotLocalFile(root, relPath, snapPath)
	if err != nil {
		return PushResult{}, err
	}
	defer func() { _ = root.Remove(snapPath) }()

	// Skip upload if content has not changed relative to local baseline.
	if snapSHA == rec.SHA256 {
		clearPendingPushIfMatches(root, &state, rec.ID)
		return PushResult{
			UserID:      me.ID,
			FileID:      rec.ID,
			LocalPath:   relPath,
			Skipped:     true,
			OldRevision: rec.ContentRevision,
			NewRevision: rec.ContentRevision,
			SHA256:      rec.SHA256,
			Size:        rec.Size,
			Cursor:      state.Cursor,
		}, nil
	}

	// Query remote HEAD to obtain the opaque ETag and verify it matches our recorded baseline.
	remoteInfo, err := api.HeadFile(ctx, rec.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			clearPendingPushIfMatches(root, &state, rec.ID)
			return PushResult{}, fmt.Errorf("%w: %s (id=%s)", ErrRemoteDeleted, relPath, rec.ID)
		}
		return PushResult{}, err
	}

	// Check if remote matches our baseline (content_revision and sha256).
	if remoteInfo.ContentRevision != rec.ContentRevision || remoteInfo.SHA256 != rec.SHA256 {
		// Response-loss recovery: if a prior push of this exact snapshot already committed on the server,
		// verify remote matches (baseRevision+1, snapSHA) and record success without re-uploading.
		if pending, ok := state.PendingPushes[rec.ID]; ok && isConfirmedPendingPush(&pending, rec, snapSHA, remoteInfo) {
			rec.SHA256 = remoteInfo.SHA256
			rec.Size = remoteInfo.Size
			rec.ContentRevision = remoteInfo.ContentRevision
			state.Files[rec.ID] = rec
			delete(state.PendingPushes, rec.ID)
			state.PendingPush = nil
			if err := saveStateAtomically(root, state); err != nil {
				return PushResult{}, err
			}
			return PushResult{
				UserID:      me.ID,
				FileID:      rec.ID,
				LocalPath:   relPath,
				Recovered:   true,
				OldRevision: rec.ContentRevision - 1,
				NewRevision: rec.ContentRevision,
				SHA256:      rec.SHA256,
				Size:        rec.Size,
				Cursor:      state.Cursor,
			}, nil
		}

		// Remote diverged: report conflict, preserve local edits and original manifest.
		clearPendingPushIfMatches(root, &state, rec.ID)
		return PushResult{}, fmt.Errorf(
			"%w: %s baseline revision=%d sha=%s, remote revision=%d sha=%s",
			ErrRemoteConflict, relPath, rec.ContentRevision, rec.SHA256, remoteInfo.ContentRevision, remoteInfo.SHA256,
		)
	}

	if strings.TrimSpace(remoteInfo.ETag) == "" {
		return PushResult{}, errors.New("server HEAD response did not include an ETag")
	}

	// Record pending push intent per file ID before sending PUT so a lost response can be safely recovered on retry.
	if state.PendingPushes == nil {
		state.PendingPushes = make(map[string]PendingPushRecord)
	}
	state.PendingPushes[rec.ID] = PendingPushRecord{
		FileID:       rec.ID,
		BaseRevision: rec.ContentRevision,
		BaseSHA256:   rec.SHA256,
		TargetSHA256: snapSHA,
		TargetSize:   snapSize,
	}
	state.PendingPush = nil
	if err := saveStateAtomically(root, state); err != nil {
		return PushResult{}, err
	}

	snapFile, err := root.Open(snapPath)
	if err != nil {
		return PushResult{}, fmt.Errorf("open push snapshot: %w", err)
	}
	updated, _, putErr := api.ReplaceFileContent(ctx, rec.ID, remoteInfo.ETag, snapFile, snapSize)
	_ = snapFile.Close()
	if putErr != nil {
		if errors.Is(putErr, ErrPreconditionFailed) {
			clearPendingPushIfMatches(root, &state, rec.ID)
			return PushResult{}, fmt.Errorf("%w: %s (HTTP 412)", ErrRemoteConflict, relPath)
		}
		if errors.Is(putErr, ErrNotFound) {
			clearPendingPushIfMatches(root, &state, rec.ID)
			return PushResult{}, fmt.Errorf("%w: %s (id=%s)", ErrRemoteDeleted, relPath, rec.ID)
		}
		// Keep PendingPushes[rec.ID] recorded on uncertain/transport error so retry can verify if server committed.
		return PushResult{}, putErr
	}

	oldRev := rec.ContentRevision
	rec.SHA256 = updated.SHA256
	rec.Size = updated.Size
	rec.ContentRevision = updated.ContentRevision
	state.Files[rec.ID] = rec
	delete(state.PendingPushes, rec.ID)
	state.PendingPush = nil

	// Keep state.Cursor unchanged so subsequent pull still observes this and other devices' events.
	if err := saveStateAtomically(root, state); err != nil {
		return PushResult{}, err
	}

	return PushResult{
		UserID:      me.ID,
		FileID:      rec.ID,
		LocalPath:   relPath,
		OldRevision: oldRev,
		NewRevision: rec.ContentRevision,
		SHA256:      rec.SHA256,
		Size:        rec.Size,
		Cursor:      state.Cursor,
	}, nil
}

func isConfirmedPendingPush(pending *PendingPushRecord, rec FileRecord, snapSHA string, remote RemoteContentInfo) bool {
	if pending == nil {
		return false
	}
	return pending.FileID == rec.ID &&
		pending.BaseRevision == rec.ContentRevision &&
		pending.BaseSHA256 == rec.SHA256 &&
		pending.TargetSHA256 == snapSHA &&
		remote.ContentRevision == rec.ContentRevision+1 &&
		remote.SHA256 == snapSHA
}

func clearPendingPushIfMatches(root *os.Root, state *State, fileID string) {
	changed := false
	if _, ok := state.PendingPushes[fileID]; ok {
		delete(state.PendingPushes, fileID)
		changed = true
	}
	if state.PendingPush != nil && state.PendingPush.FileID == fileID {
		state.PendingPush = nil
		changed = true
	}
	if changed {
		_ = saveStateAtomically(root, *state)
	}
}

func findTrackedFileByPath(files map[string]FileRecord, relPath string) (FileRecord, bool) {
	for _, rec := range files {
		if rec.LocalPath == relPath {
			return rec, true
		}
	}
	return FileRecord{}, false
}

func normalizeMirrorRelPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: --path is required", ErrInvalidPushPath)
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, `\`) {
		return "", fmt.Errorf("%w: path must be relative to mirror root", ErrInvalidPushPath)
	}
	slashed := strings.ReplaceAll(trimmed, `\`, "/")
	cleaned := path.Clean(slashed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: path escapes mirror root", ErrInvalidPushPath)
	}
	if cleaned == metaDirName || strings.HasPrefix(cleaned, metaDirName+"/") {
		return "", fmt.Errorf("%w: cannot push internal %s metadata", ErrInvalidPushPath, metaDirName)
	}
	return cleaned, nil
}

func snapshotLocalFile(root *os.Root, relPath, snapPath string) (string, int64, error) {
	src, err := root.Open(relPath)
	if err != nil {
		return "", 0, fmt.Errorf("open %q for snapshot: %w", relPath, err)
	}
	defer src.Close()

	dst, err := root.OpenFile(snapPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("create push snapshot: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = root.Remove(snapPath)
		}
	}()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(dst, hasher), src)
	if err != nil {
		_ = dst.Close()
		return "", 0, fmt.Errorf("copy snapshot for %q: %w", relPath, err)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return "", 0, fmt.Errorf("sync snapshot for %q: %w", relPath, err)
	}
	if err := dst.Close(); err != nil {
		return "", 0, fmt.Errorf("close snapshot for %q: %w", relPath, err)
	}
	ok = true
	return hex.EncodeToString(hasher.Sum(nil)), written, nil
}
