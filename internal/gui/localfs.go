package gui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	reservedMetaDir    = ".filedock"
	downloadTempPrefix = ".filedock-dl-"
)

var (
	ErrLocalPathForbidden = errors.New("local path is outside allowed directory or targets protected .filedock metadata")
	ErrLocalNotFound      = errors.New("local file or directory not found")
	ErrLocalConflict      = errors.New("target local path already exists")
	ErrLocalHashMismatch  = errors.New("downloaded file SHA-256 does not match expected hash")
	ErrLocalInvalidName   = errors.New("invalid local entry name")
)

type LocalEntry struct {
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	IsDir       bool      `json:"is_dir"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
	Permissions string    `json:"permissions"`
}

type LocalDownloadConflict struct {
	TargetPath    string `json:"target_path"`
	SuggestedName string `json:"suggested_name"`
}

type LocalDownloadResult struct {
	SavedPath string `json:"saved_path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// normalizeLocalRelPath validates a relative path inside --local-dir WITHOUT stripping
// leading or trailing spaces from directory/file names, while rejecting absolute paths,
// parent traversal, NUL bytes, and protected .filedock metadata.
func normalizeLocalRelPath(raw string, allowRoot bool) (string, error) {
	if raw == "" || raw == "." || raw == "/" {
		if allowRoot {
			return "", nil
		}
		return "", fmt.Errorf("%w: path is required", ErrLocalPathForbidden)
	}
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\`) {
		return "", fmt.Errorf("%w: absolute path is not allowed", ErrLocalPathForbidden)
	}
	if strings.Contains(raw, "\x00") {
		return "", fmt.Errorf("%w: NUL byte not allowed", ErrLocalPathForbidden)
	}

	// Trim only trailing slash(es) if any, never spaces.
	trimmedSlash := strings.TrimRight(raw, "/")
	if trimmedSlash == "" {
		if allowRoot {
			return "", nil
		}
		return "", fmt.Errorf("%w: root path not allowed for this operation", ErrLocalPathForbidden)
	}

	rawParts := strings.Split(trimmedSlash, "/")
	parts := make([]string, 0, len(rawParts))
	for _, seg := range rawParts {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: invalid path segment %q", ErrLocalPathForbidden, seg)
		}
		if os.PathSeparator == '\\' && strings.Contains(seg, `\`) {
			return "", fmt.Errorf("%w: invalid separator in segment %q", ErrLocalPathForbidden, seg)
		}
		parts = append(parts, seg)
	}
	cleaned := strings.Join(parts, "/")
	if isProtectedLocalPath(cleaned) {
		return "", fmt.Errorf("%w: %s is protected", ErrLocalPathForbidden, reservedMetaDir)
	}
	return cleaned, nil
}

func validateSingleEntryName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: name must not be empty", ErrLocalInvalidName)
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%w: invalid entry name %q", ErrLocalInvalidName, name)
	}
	if len(name) > 255 {
		return "", fmt.Errorf("%w: name exceeds 255 bytes", ErrLocalInvalidName)
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return "", fmt.Errorf("%w: control characters not allowed", ErrLocalInvalidName)
		}
	}
	if strings.EqualFold(name, reservedMetaDir) || strings.HasPrefix(name, downloadTempPrefix) {
		return "", fmt.Errorf("%w: %q is reserved", ErrLocalPathForbidden, name)
	}
	return name, nil
}

func isProtectedLocalPath(rel string) bool {
	if rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for i, seg := range parts {
		if i == 0 && strings.EqualFold(seg, reservedMetaDir) {
			return true
		}
		if strings.HasPrefix(seg, downloadTempPrefix) {
			return true
		}
	}
	return false
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func baseRelName(rel string) string {
	if idx := strings.LastIndex(rel, "/"); idx >= 0 {
		return rel[idx+1:]
	}
	return rel
}

func isValidSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func openLocalRoot(localDir string) (*os.Root, error) {
	root, err := os.OpenRoot(localDir)
	if err != nil {
		return nil, fmt.Errorf("open confined local dir: %w", err)
	}
	return root, nil
}

func listLocalDir(localDir, relDir string) ([]LocalEntry, string, error) {
	cleaned, err := normalizeLocalRelPath(relDir, true)
	if err != nil {
		return nil, "", err
	}
	root, err := openLocalRoot(localDir)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()

	openPath := "."
	if cleaned != "" {
		info, err := root.Lstat(cleaned)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, "", ErrLocalNotFound
			}
			return nil, "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, "", fmt.Errorf("%w: %q is not a directory", ErrLocalPathForbidden, cleaned)
		}
		openPath = cleaned
	}

	f, err := root.Open(openPath)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	dirEntries, err := f.ReadDir(-1)
	if err != nil {
		return nil, "", err
	}

	result := make([]LocalEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		name := de.Name()
		if (cleaned == "" && strings.EqualFold(name, reservedMetaDir)) || strings.HasPrefix(name, downloadTempPrefix) {
			continue
		}
		if de.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		entryPath := joinRel(cleaned, name)
		result = append(result, LocalEntry{
			Name:        name,
			Path:        entryPath,
			IsDir:       info.IsDir(),
			Size:        info.Size(),
			ModTime:     info.ModTime().UTC(),
			Permissions: info.Mode().String(),
		})
	}
	slices.SortFunc(result, func(a, b LocalEntry) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return result, cleaned, nil
}

func mkdirLocal(localDir, parentRel, name string) (string, error) {
	parent, err := normalizeLocalRelPath(parentRel, true)
	if err != nil {
		return "", err
	}
	validName, err := validateSingleEntryName(name)
	if err != nil {
		return "", err
	}
	target := joinRel(parent, validName)
	if isProtectedLocalPath(target) {
		return "", ErrLocalPathForbidden
	}

	root, err := openLocalRoot(localDir)
	if err != nil {
		return "", err
	}
	defer root.Close()

	if err := root.Mkdir(target, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", ErrLocalConflict
		}
		return "", err
	}
	return target, nil
}

func renameOrMoveLocal(localDir, srcRel, dstDirRel, newName string) (string, error) {
	src, err := normalizeLocalRelPath(srcRel, false)
	if err != nil {
		return "", err
	}
	dstDir, err := normalizeLocalRelPath(dstDirRel, true)
	if err != nil {
		return "", err
	}
	validName, err := validateSingleEntryName(newName)
	if err != nil {
		return "", err
	}
	dst := joinRel(dstDir, validName)
	if isProtectedLocalPath(dst) {
		return "", ErrLocalPathForbidden
	}
	if src == dst {
		return dst, nil
	}

	root, err := openLocalRoot(localDir)
	if err != nil {
		return "", err
	}
	defer root.Close()

	srcInfo, err := root.Lstat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrLocalNotFound
		}
		return "", err
	}
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: symlinks are not supported", ErrLocalPathForbidden)
	}
	if _, err := root.Lstat(dst); err == nil {
		return "", ErrLocalConflict
	}
	if err := root.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func deleteLocal(localDir, targetRel string) error {
	target, err := normalizeLocalRelPath(targetRel, false)
	if err != nil {
		return err
	}
	root, err := openLocalRoot(localDir)
	if err != nil {
		return err
	}
	defer root.Close()

	info, err := root.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrLocalNotFound
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: symlinks are not supported", ErrLocalPathForbidden)
	}
	return root.Remove(target)
}

func openLocalRegularFile(localDir, relPath string) (*os.File, os.FileInfo, string, error) {
	cleaned, err := normalizeLocalRelPath(relPath, false)
	if err != nil {
		return nil, nil, "", err
	}
	root, err := openLocalRoot(localDir)
	if err != nil {
		return nil, nil, "", err
	}
	defer root.Close()

	info, err := root.Lstat(cleaned)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, "", ErrLocalNotFound
		}
		return nil, nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, "", fmt.Errorf("%w: %q is not a regular file", ErrLocalPathForbidden, cleaned)
	}
	f, err := root.Open(cleaned)
	if err != nil {
		return nil, nil, "", err
	}
	return f, info, cleaned, nil
}

type downloadStreamResult struct {
	Body         io.ReadCloser
	HeaderSHA256 string
}

func downloadStreamToLocal(
	localDir, targetDirRel, fileName, expectedSHA256, conflictPolicy string,
	openStream func() (downloadStreamResult, error),
	beforePublish func(),
) (LocalDownloadResult, *LocalDownloadConflict, error) {
	dirRel, err := normalizeLocalRelPath(targetDirRel, true)
	if err != nil {
		return LocalDownloadResult{}, nil, err
	}
	validName, err := validateSingleEntryName(fileName)
	if err != nil {
		return LocalDownloadResult{}, nil, err
	}
	if conflictPolicy == "" {
		conflictPolicy = "ask"
	}
	if conflictPolicy != "ask" && conflictPolicy != "rename" && conflictPolicy != "overwrite" {
		return LocalDownloadResult{}, nil, fmt.Errorf("unknown conflict policy %q", conflictPolicy)
	}

	trimmedExpected := strings.TrimSpace(expectedSHA256)
	if trimmedExpected == "" || !isValidSHA256Hex(trimmedExpected) {
		return LocalDownloadResult{}, nil, fmt.Errorf("%w: expected_sha256 is required for download integrity verification", ErrLocalHashMismatch)
	}

	root, err := openLocalRoot(localDir)
	if err != nil {
		return LocalDownloadResult{}, nil, err
	}
	defer root.Close()

	if dirRel != "" {
		info, err := root.Lstat(dirRel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return LocalDownloadResult{}, nil, ErrLocalNotFound
			}
			return LocalDownloadResult{}, nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return LocalDownloadResult{}, nil, fmt.Errorf("%w: target dir %q is not a directory", ErrLocalPathForbidden, dirRel)
		}
	}

	targetPath := joinRel(dirRel, validName)

	// Fast pre-check before downloading for ask and overwrite-on-directory.
	if info, statErr := root.Lstat(targetPath); statErr == nil {
		switch conflictPolicy {
		case "ask":
			suggested := suggestNonCollidingName(root, dirRel, validName)
			return LocalDownloadResult{}, &LocalDownloadConflict{
				TargetPath:    targetPath,
				SuggestedName: suggested,
			}, ErrLocalConflict
		case "overwrite":
			if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return LocalDownloadResult{}, nil, fmt.Errorf("%w: cannot overwrite non-regular entry %q", ErrLocalConflict, targetPath)
			}
		}
	}

	streamRes, err := openStream()
	if err != nil {
		return LocalDownloadResult{}, nil, err
	}
	defer streamRes.Body.Close()

	if hdrSHA := strings.TrimSpace(streamRes.HeaderSHA256); hdrSHA != "" {
		if !isValidSHA256Hex(hdrSHA) || !strings.EqualFold(hdrSHA, trimmedExpected) {
			return LocalDownloadResult{}, nil, fmt.Errorf("%w: server header sha256 %q does not match expected %q", ErrLocalHashMismatch, hdrSHA, trimmedExpected)
		}
	}

	tmpName := downloadTempPrefix + randomHex(8) + ".tmp"
	tmpPath := joinRel(dirRel, tmpName)
	tmpFile, err := root.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return LocalDownloadResult{}, nil, fmt.Errorf("create local download temp file: %w", err)
	}
	defer func() {
		_ = root.Remove(tmpPath)
	}()

	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmpFile, hasher), streamRes.Body)
	if copyErr != nil {
		_ = tmpFile.Close()
		return LocalDownloadResult{}, nil, fmt.Errorf("stream download: %w", copyErr)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return LocalDownloadResult{}, nil, fmt.Errorf("sync download temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return LocalDownloadResult{}, nil, fmt.Errorf("close download temp file: %w", err)
	}

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualSHA, trimmedExpected) {
		return LocalDownloadResult{}, nil, fmt.Errorf("%w: got %s, expected %s", ErrLocalHashMismatch, actualSHA, trimmedExpected)
	}

	if beforePublish != nil {
		beforePublish()
	}

	finalPath, conflict, pubErr := publishDownloadedFile(root, tmpPath, dirRel, validName, conflictPolicy)
	if conflict != nil {
		return LocalDownloadResult{}, conflict, ErrLocalConflict
	}
	if pubErr != nil {
		return LocalDownloadResult{}, nil, pubErr
	}

	return LocalDownloadResult{
		SavedPath: finalPath,
		Size:      written,
		SHA256:    actualSHA,
	}, nil, nil
}

// publishDownloadedFile atomically publishes tmpPath according to conflictPolicy.
// For "ask" and "rename", it uses os.Root.Link so an existing file that appeared after the
// pre-check can NEVER be overwritten.
func publishDownloadedFile(root *os.Root, tmpPath, dirRel, validName, conflictPolicy string) (string, *LocalDownloadConflict, error) {
	targetPath := joinRel(dirRel, validName)
	switch conflictPolicy {
	case "ask":
		err := root.Link(tmpPath, targetPath)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				suggested := suggestNonCollidingName(root, dirRel, validName)
				return "", &LocalDownloadConflict{
					TargetPath:    targetPath,
					SuggestedName: suggested,
				}, ErrLocalConflict
			}
			return "", nil, fmt.Errorf("publish downloaded file: %w", err)
		}
		return targetPath, nil, nil

	case "rename":
		// Try the base name first, then numbered suffixes until Link succeeds without overwriting.
		if err := root.Link(tmpPath, targetPath); err == nil {
			return targetPath, nil, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("publish downloaded file: %w", err)
		}
		ext := path.Ext(validName)
		stem := strings.TrimSuffix(validName, ext)
		if ext == validName {
			stem = validName
			ext = ""
		}
		for i := 1; i < 10000; i++ {
			candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
			full := joinRel(dirRel, candidate)
			err := root.Link(tmpPath, full)
			if err == nil {
				return full, nil, nil
			}
			if !errors.Is(err, os.ErrExist) {
				return "", nil, fmt.Errorf("publish renamed downloaded file: %w", err)
			}
		}
		fallback := joinRel(dirRel, fmt.Sprintf("%s-%s%s", stem, randomHex(4), ext))
		if err := root.Link(tmpPath, fallback); err != nil {
			return "", nil, fmt.Errorf("publish renamed downloaded file: %w", err)
		}
		return fallback, nil, nil

	case "overwrite":
		if info, err := root.Lstat(targetPath); err == nil {
			if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", nil, fmt.Errorf("%w: cannot overwrite non-regular entry %q", ErrLocalConflict, targetPath)
			}
		}
		if err := root.Rename(tmpPath, targetPath); err != nil {
			return "", nil, fmt.Errorf("overwrite downloaded file: %w", err)
		}
		return targetPath, nil, nil

	default:
		return "", nil, fmt.Errorf("unknown conflict policy %q", conflictPolicy)
	}
}

func suggestNonCollidingName(root *os.Root, dirRel, name string) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if ext == name {
		stem = name
		ext = ""
	}
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		full := joinRel(dirRel, candidate)
		if _, err := root.Lstat(full); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%s%s", stem, randomHex(4), ext)
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
