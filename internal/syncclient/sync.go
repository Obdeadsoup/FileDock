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
	"slices"
	"strings"
)

var (
	ErrDirNotEmpty    = errors.New("target directory must be empty on first sync")
	ErrServerMismatch = errors.New("local sync state belongs to a different server URL")
	ErrUserMismatch   = errors.New("local sync state belongs to a different user")
	ErrConflict       = errors.New("local conflict detected; refusing to overwrite or delete unmanaged changes")
	ErrHashMismatch   = errors.New("downloaded file SHA-256 does not match server metadata")
)

// Config configures a single one-way pull sync run.
type Config struct {
	ServerURL     string
	Token         string
	Dir           string
	PageLimit     int
	HTTPClient    *http.Client
	API           API
	AfterStepHook func(step string) error
}

// Result summarizes what a Pull run applied.
type Result struct {
	UserID         string
	StartCursor    int64
	EndCursor      int64
	EventsApplied  int
	Downloaded     int
	Reused         int
	Deleted        int
	FoldersCreated int
	FoldersRemoved int
}

// Pull fetches incremental changes from the server, replays them into the target directory tree,
// applies local filesystem updates safely inside a confined os.Root, and atomically commits state.
func Pull(ctx context.Context, cfg Config) (Result, error) {
	normalizedServer, err := NormalizeServerURL(cfg.ServerURL)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(cfg.Dir) == "" {
		return Result{}, errors.New("local directory (--dir) is required")
	}

	api := cfg.API
	if api == nil {
		api, err = NewHTTPAPI(normalizedServer, cfg.Token, cfg.HTTPClient)
		if err != nil {
			return Result{}, err
		}
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("ensure mirror directory: %w", err)
	}
	root, err := os.OpenRoot(cfg.Dir)
	if err != nil {
		return Result{}, fmt.Errorf("open confined mirror root: %w", err)
	}
	defer root.Close()

	state, exists, err := loadState(root)
	if err != nil {
		return Result{}, err
	}

	if exists {
		if state.ServerURL != normalizedServer {
			return Result{}, fmt.Errorf("%w: local=%q requested=%q", ErrServerMismatch, state.ServerURL, normalizedServer)
		}
	} else {
		if err := ensureFirstRunEmptyDir(cfg.Dir); err != nil {
			return Result{}, err
		}
	}

	me, err := api.Me(ctx)
	if err != nil {
		return Result{}, err
	}

	if !exists {
		state = newEmptyState(normalizedServer, me.ID)
		if err := ensureMetaDirs(root); err != nil {
			return Result{}, err
		}
		// Persist initial bound state (cursor 0) before any mirror mutation so an interrupted
		// first pull can safely resume without being rejected as a non-empty directory.
		if err := saveStateAtomically(root, state); err != nil {
			return Result{}, err
		}
	} else {
		if state.UserID != me.ID {
			return Result{}, fmt.Errorf("%w: local=%q authenticated=%q", ErrUserMismatch, state.UserID, me.ID)
		}
		if err := ensureMetaDirs(root); err != nil {
			return Result{}, err
		}
	}

	pageLimit := cfg.PageLimit
	if pageLimit <= 0 {
		pageLimit = defaultPageLimit
	}

	// 1. Paginate all currently visible events and replay them in memory to compute the target tree.
	targetFolders := make(map[string]FolderRecord, len(state.Folders))
	for k, v := range state.Folders {
		targetFolders[k] = v
	}
	targetFiles := make(map[string]FileRecord, len(state.Files))
	for k, v := range state.Files {
		targetFiles[k] = v
	}

	cursor := state.Cursor
	eventsApplied := 0
	for {
		page, err := api.ListChanges(ctx, cursor, pageLimit)
		if err != nil {
			return Result{}, err
		}
		for _, ev := range page.Items {
			applyEvent(targetFolders, targetFiles, ev)
			eventsApplied++
		}
		cursor = page.NextCursor
		if !page.HasMore || len(page.Items) == 0 {
			break
		}
	}

	result := Result{
		UserID:        me.ID,
		StartCursor:   state.Cursor,
		EndCursor:     cursor,
		EventsApplied: eventsApplied,
	}

	if eventsApplied == 0 {
		return result, nil
	}

	// 2. Compute deterministic local paths for the target directory tree.
	targetFolderPaths, targetFilePaths, err := computeLocalPaths(targetFolders, targetFiles)
	if err != nil {
		return Result{}, err
	}
	for id, rec := range targetFolders {
		rec.LocalPath = targetFolderPaths[id]
		targetFolders[id] = rec
	}
	for id, rec := range targetFiles {
		rec.LocalPath = targetFilePaths[id]
		targetFiles[id] = rec
	}

	// 3. Build sets of old tracked paths being vacated so we can distinguish stranger files from managed relocations.
	oldFileByPath := make(map[string]FileRecord, len(state.Files))
	for _, rec := range state.Files {
		if rec.LocalPath != "" {
			oldFileByPath[rec.LocalPath] = rec
		}
	}
	vacatedFilePaths := make(map[string]bool)
	var filesToDelete []FileRecord
	for id, oldRec := range state.Files {
		newRec, stillExists := targetFiles[id]
		if !stillExists {
			vacatedFilePaths[oldRec.LocalPath] = true
			filesToDelete = append(filesToDelete, oldRec)
		} else if oldRec.LocalPath != newRec.LocalPath {
			vacatedFilePaths[oldRec.LocalPath] = true
		}
	}

	// Pre-flight check: ensure deleted files have not been manually modified by the user.
	for _, oldRec := range filesToDelete {
		if err := verifyRemovableManagedFile(root, oldRec); err != nil {
			return Result{}, err
		}
	}

	// Pre-flight check & plan for every target file: reuse in place, reuse via move, or download.
	type movePlan struct {
		id         string
		sourcePath string
		stagePath  string
		targetPath string
		sha256     string
	}
	type downloadPlan struct {
		rec        FileRecord
		stagePath  string
		targetPath string
	}

	var (
		moves     []movePlan
		downloads []downloadPlan
	)

	// Sort file IDs for deterministic planning and fault-injection predictability.
	targetFileIDs := make([]string, 0, len(targetFiles))
	for id := range targetFiles {
		targetFileIDs = append(targetFileIDs, id)
	}
	slices.Sort(targetFileIDs)

	for _, id := range targetFileIDs {
		tf := targetFiles[id]
		oldRec, wasTracked := state.Files[id]
		stageMovePath := tmpDirRel + "/mv-" + safeTempToken(id)

		// Check if target path already holds an entry on disk.
		info, statErr := root.Lstat(tf.LocalPath)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return Result{}, fmt.Errorf("stat target %q: %w", tf.LocalPath, statErr)
		}
		if statErr == nil {
			if info.IsDir() {
				// Only allowed if this directory is a tracked folder being removed/moved (handled later), otherwise conflict.
				return Result{}, fmt.Errorf("%w: directory exists at file target %q", ErrConflict, tf.LocalPath)
			}
			diskSHA, hashErr := hashFileInRoot(root, tf.LocalPath)
			if hashErr != nil {
				return Result{}, hashErr
			}
			if diskSHA == tf.SHA256 {
				// Either unchanged file or already published before a prior crash.
				if wasTracked && oldRec.LocalPath != tf.LocalPath {
					// If the old path still lingers with the old hash, schedule its cleanup.
					if err := verifyRemovableManagedFile(root, oldRec); err != nil {
						return Result{}, err
					}
					filesToDelete = append(filesToDelete, oldRec)
				}
				result.Reused++
				continue
			}
			// Content at targetPath differs from tf.SHA256.
			if !vacatedFilePaths[tf.LocalPath] {
				if wasTracked && oldRec.LocalPath == tf.LocalPath {
					if diskSHA != oldRec.SHA256 {
						return Result{}, fmt.Errorf("%w: local file %q was modified", ErrConflict, tf.LocalPath)
					}
				} else {
					return Result{}, fmt.Errorf("%w: unmanaged file exists at %q", ErrConflict, tf.LocalPath)
				}
			} else if oldAtTarget, ok := oldFileByPath[tf.LocalPath]; ok {
				if diskSHA != oldAtTarget.SHA256 {
					return Result{}, fmt.Errorf("%w: local file %q was modified", ErrConflict, tf.LocalPath)
				}
			}
		}

		// Check if a previously interrupted run already staged a move for this file in .filedock/tmp/.
		if _, err := root.Lstat(stageMovePath); err == nil {
			diskSHA, hashErr := hashFileInRoot(root, stageMovePath)
			if hashErr != nil {
				return Result{}, hashErr
			}
			if diskSHA == tf.SHA256 {
				moves = append(moves, movePlan{
					id:         id,
					sourcePath: "", // already staged in stageMovePath
					stagePath:  stageMovePath,
					targetPath: tf.LocalPath,
					sha256:     tf.SHA256,
				})
				result.Reused++
				continue
			}
			_ = root.Remove(stageMovePath)
		}

		// Check if we can reuse the existing tracked local file at oldRec.LocalPath (move/rename with unchanged content).
		if wasTracked && oldRec.LocalPath != "" && oldRec.LocalPath != tf.LocalPath {
			if _, err := root.Lstat(oldRec.LocalPath); err == nil {
				diskSHA, hashErr := hashFileInRoot(root, oldRec.LocalPath)
				if hashErr != nil {
					return Result{}, hashErr
				}
				if diskSHA != oldRec.SHA256 {
					return Result{}, fmt.Errorf("%w: local file %q was modified", ErrConflict, oldRec.LocalPath)
				}
				if oldRec.SHA256 == tf.SHA256 {
					moves = append(moves, movePlan{
						id:         id,
						sourcePath: oldRec.LocalPath,
						stagePath:  stageMovePath,
						targetPath: tf.LocalPath,
						sha256:     tf.SHA256,
					})
					result.Reused++
					continue
				}
			}
		}

		downloads = append(downloads, downloadPlan{
			rec:        tf,
			stagePath:  tmpDirRel + "/dl-" + safeTempToken(id),
			targetPath: tf.LocalPath,
		})
	}

	// Pre-flight check for target folders: refuse if an unmanaged regular file occupies any target folder path.
	sortedTargetFolders := make([]string, 0, len(targetFolderPaths))
	for _, p := range targetFolderPaths {
		sortedTargetFolders = append(sortedTargetFolders, p)
	}
	slices.SortFunc(sortedTargetFolders, comparePathDepthAsc)
	for _, dirPath := range sortedTargetFolders {
		info, err := root.Lstat(dirPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return Result{}, fmt.Errorf("stat target folder %q: %w", dirPath, err)
		}
		if !info.IsDir() && !vacatedFilePaths[dirPath] {
			return Result{}, fmt.Errorf("%w: unmanaged file exists at folder path %q", ErrConflict, dirPath)
		}
	}

	// 4. Download all required files to temporary files and verify SHA-256 BEFORE mutating the mirror tree.
	for _, dl := range downloads {
		if err := downloadAndVerify(ctx, api, root, dl.rec, dl.stagePath); err != nil {
			return Result{}, err
		}
		result.Downloaded++
	}
	if err := runHook(cfg.AfterStepHook, "after_downloads"); err != nil {
		return Result{}, err
	}

	// 5. Stage local file moves into .filedock/tmp/ so swaps and case-only renames never collide.
	for _, mv := range moves {
		if mv.sourcePath == "" {
			continue
		}
		if err := root.Rename(mv.sourcePath, mv.stagePath); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return Result{}, fmt.Errorf("stage move %q: %w", mv.sourcePath, err)
			}
		}
	}
	if err := runHook(cfg.AfterStepHook, "after_stage_moves"); err != nil {
		return Result{}, err
	}

	// 6. Remove deleted managed files.
	for _, oldRec := range filesToDelete {
		if oldRec.LocalPath == "" {
			continue
		}
		if err := root.Remove(oldRec.LocalPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return Result{}, fmt.Errorf("remove deleted file %q: %w", oldRec.LocalPath, err)
		}
		result.Deleted++
	}
	if err := runHook(cfg.AfterStepHook, "after_deletes"); err != nil {
		return Result{}, err
	}

	// 7. Create target directories top-down.
	for _, dirPath := range sortedTargetFolders {
		created, err := ensureDirPath(root, dirPath)
		if err != nil {
			return Result{}, err
		}
		if created {
			result.FoldersCreated++
		}
	}
	if err := runHook(cfg.AfterStepHook, "after_mkdirs"); err != nil {
		return Result{}, err
	}

	// 8. Publish staged moves and verified downloads to their final paths.
	for _, mv := range moves {
		if err := ensureParentDirs(root, mv.targetPath); err != nil {
			return Result{}, err
		}
		if err := root.Rename(mv.stagePath, mv.targetPath); err != nil {
			return Result{}, fmt.Errorf("publish moved file %q: %w", mv.targetPath, err)
		}
	}
	for _, dl := range downloads {
		if err := ensureParentDirs(root, dl.targetPath); err != nil {
			return Result{}, err
		}
		if err := root.Rename(dl.stagePath, dl.targetPath); err != nil {
			return Result{}, fmt.Errorf("publish downloaded file %q: %w", dl.targetPath, err)
		}
	}
	if err := runHook(cfg.AfterStepHook, "after_publish"); err != nil {
		return Result{}, err
	}

	// 9. Remove obsolete tracked directories bottom-up (preserving non-empty directories with stranger files).
	targetFolderPathSet := make(map[string]bool, len(targetFolderPaths))
	for _, p := range targetFolderPaths {
		targetFolderPathSet[p] = true
	}
	var obsoleteDirs []string
	for _, oldFolder := range state.Folders {
		if oldFolder.LocalPath != "" && !targetFolderPathSet[oldFolder.LocalPath] {
			obsoleteDirs = append(obsoleteDirs, oldFolder.LocalPath)
		}
	}
	slices.SortFunc(obsoleteDirs, comparePathDepthDesc)
	for _, dirPath := range obsoleteDirs {
		if err := root.Remove(dirPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// If a user placed a stranger file inside a deleted folder, leave the directory intact to protect it.
			continue
		}
		result.FoldersRemoved++
	}
	if err := runHook(cfg.AfterStepHook, "after_rmdirs"); err != nil {
		return Result{}, err
	}

	// 10. Atomically commit updated manifest and cursor.
	nextState := State{
		Version:   stateVersion,
		ServerURL: normalizedServer,
		UserID:    me.ID,
		Cursor:    cursor,
		Folders:   targetFolders,
		Files:     targetFiles,
	}
	if err := saveStateAtomically(root, nextState); err != nil {
		return Result{}, err
	}

	return result, nil
}

func applyEvent(folders map[string]FolderRecord, files map[string]FileRecord, ev ChangeEvent) {
	switch ev.Type {
	case "folder.upsert":
		if ev.Folder != nil {
			prev := folders[ev.ResourceID]
			folders[ev.ResourceID] = FolderRecord{
				ID:        ev.ResourceID,
				Name:      ev.Folder.Name,
				ParentID:  cloneStrPtr(ev.Folder.ParentID),
				LocalPath: prev.LocalPath,
			}
		}
	case "folder.delete":
		delete(folders, ev.ResourceID)
	case "file.upsert":
		if ev.File != nil {
			prev := files[ev.ResourceID]
			files[ev.ResourceID] = FileRecord{
				ID:              ev.ResourceID,
				Name:            ev.File.Name,
				ParentID:        cloneStrPtr(ev.File.ParentID),
				Size:            ev.File.Size,
				SHA256:          ev.File.SHA256,
				ContentRevision: ev.File.ContentRevision,
				LocalPath:       prev.LocalPath,
			}
		}
	case "file.delete":
		delete(files, ev.ResourceID)
	}
}

func downloadAndVerify(ctx context.Context, api API, root *os.Root, rec FileRecord, stagePath string) error {
	rc, err := api.DownloadFile(ctx, rec.ID)
	if err != nil {
		return err
	}
	defer rc.Close()

	f, err := root.OpenFile(stagePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create temp download file: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = root.Remove(stagePath)
		}
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), rc); err != nil {
		_ = f.Close()
		return fmt.Errorf("stream download for %s: %w", rec.ID, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync downloaded file for %s: %w", rec.ID, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close downloaded file for %s: %w", rec.ID, err)
	}

	gotSHA := hex.EncodeToString(hasher.Sum(nil))
	if gotSHA != rec.SHA256 {
		return fmt.Errorf("%w: file %s expected %s got %s", ErrHashMismatch, rec.ID, rec.SHA256, gotSHA)
	}
	committed = true
	return nil
}

func verifyRemovableManagedFile(root *os.Root, oldRec FileRecord) error {
	if oldRec.LocalPath == "" {
		return nil
	}
	info, err := root.Lstat(oldRec.LocalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %q: %w", oldRec.LocalPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: expected file at %q, found directory", ErrConflict, oldRec.LocalPath)
	}
	diskSHA, err := hashFileInRoot(root, oldRec.LocalPath)
	if err != nil {
		return err
	}
	if diskSHA != oldRec.SHA256 {
		return fmt.Errorf("%w: local file %q was modified", ErrConflict, oldRec.LocalPath)
	}
	return nil
}

func hashFileInRoot(root *os.Root, relPath string) (string, error) {
	f, err := root.Open(relPath)
	if err != nil {
		return "", fmt.Errorf("open %q for hash: %w", relPath, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %q: %w", relPath, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func ensureMetaDirs(root *os.Root) error {
	if _, err := ensureDirPath(root, metaDirName); err != nil {
		return err
	}
	if _, err := ensureDirPath(root, tmpDirRel); err != nil {
		return err
	}
	return nil
}

func ensureParentDirs(root *os.Root, relPath string) error {
	parent := path.Dir(relPath)
	if parent == "." || parent == "" {
		return nil
	}
	_, err := ensureDirPath(root, parent)
	return err
}

func ensureDirPath(root *os.Root, relPath string) (bool, error) {
	parts := strings.Split(relPath, "/")
	cur := ""
	createdAny := false
	for _, seg := range parts {
		if seg == "" || seg == "." {
			continue
		}
		if cur == "" {
			cur = seg
		} else {
			cur = cur + "/" + seg
		}
		info, err := root.Lstat(cur)
		if err == nil {
			if !info.IsDir() {
				return false, fmt.Errorf("%w: file exists where directory %q is required", ErrConflict, cur)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("stat directory %q: %w", cur, err)
		}
		if err := root.Mkdir(cur, 0o755); err != nil {
			return false, fmt.Errorf("create directory %q: %w", cur, err)
		}
		createdAny = true
	}
	return createdAny, nil
}

func comparePathDepthAsc(a, b string) int {
	da := strings.Count(a, "/")
	db := strings.Count(b, "/")
	if da != db {
		if da < db {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func comparePathDepthDesc(a, b string) int {
	return -comparePathDepthAsc(a, b)
}

func safeTempToken(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:12])
}

func cloneStrPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func ensureFirstRunEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("inspect mirror directory: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s", ErrDirNotEmpty, dir)
	}
	return nil
}

func runHook(hook func(string) error, step string) error {
	if hook == nil {
		return nil
	}
	return hook(step)
}
