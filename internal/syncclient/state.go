package syncclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
)

const (
	stateVersion  = 1
	metaDirName   = ".filedock"
	tmpDirRel     = ".filedock/tmp"
	stateFileRel  = ".filedock/state.json"
	reservedLower = ".filedock"
)

// State is the local sync manifest persisted inside .filedock/state.json (never stores tokens).
type State struct {
	Version   int                     `json:"version"`
	ServerURL string                  `json:"server_url"`
	UserID    string                  `json:"user_id"`
	Cursor    int64                   `json:"cursor"`
	Folders   map[string]FolderRecord `json:"folders"`
	Files     map[string]FileRecord   `json:"files"`
}

// FolderRecord tracks a synced folder by stable server ID and its assigned relative local path.
type FolderRecord struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ParentID  *string `json:"parent_id"`
	LocalPath string  `json:"local_path"`
}

// FileRecord tracks a synced file by stable server ID, content SHA-256, and relative local path.
type FileRecord struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ParentID  *string `json:"parent_id"`
	Size      int64   `json:"size"`
	SHA256    string  `json:"sha256"`
	LocalPath string  `json:"local_path"`
}

func newEmptyState(serverURL, userID string) State {
	return State{
		Version:   stateVersion,
		ServerURL: serverURL,
		UserID:    userID,
		Cursor:    0,
		Folders:   make(map[string]FolderRecord),
		Files:     make(map[string]FileRecord),
	}
}

func loadState(root *os.Root) (State, bool, error) {
	f, err := root.Open(stateFileRel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, false, nil
		}
		return State{}, false, fmt.Errorf("open local sync state: %w", err)
	}
	defer f.Close()

	var st State
	if err := json.NewDecoder(f).Decode(&st); err != nil {
		return State{}, false, fmt.Errorf("decode local sync state: %w", err)
	}
	if st.Version != stateVersion {
		return State{}, false, fmt.Errorf("unsupported local sync state version %d", st.Version)
	}
	if st.Folders == nil {
		st.Folders = make(map[string]FolderRecord)
	}
	if st.Files == nil {
		st.Files = make(map[string]FileRecord)
	}
	return st, true, nil
}

func saveStateAtomically(root *os.Root, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sync state: %w", err)
	}
	data = append(data, '\n')

	tmpPath := tmpDirRel + "/state.json.tmp"
	f, err := root.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = root.Remove(tmpPath)
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = root.Remove(tmpPath)
		return fmt.Errorf("sync temp state file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmpPath)
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := root.Rename(tmpPath, stateFileRel); err != nil {
		_ = root.Remove(tmpPath)
		return fmt.Errorf("commit sync state: %w", err)
	}
	return nil
}

type namedEntry struct {
	id       string
	name     string
	isFolder bool
}

// computeLocalPaths deterministically assigns relative local paths to every folder and file.
// Entries in the same parent directory whose names collide (identical or differing only in case,
// or colliding with the reserved ".filedock" state directory) receive a stable suffix derived
// from the SHA-256 of their resource ID so they never overwrite one another on case-sensitive
// or case-insensitive filesystems.
func computeLocalPaths(folders map[string]FolderRecord, files map[string]FileRecord) (map[string]string, map[string]string, error) {
	childrenByParent := make(map[string][]namedEntry)
	for _, d := range folders {
		parentKey := ""
		if d.ParentID != nil {
			parentKey = *d.ParentID
		}
		childrenByParent[parentKey] = append(childrenByParent[parentKey], namedEntry{
			id:       d.ID,
			name:     d.Name,
			isFolder: true,
		})
	}
	for _, f := range files {
		parentKey := ""
		if f.ParentID != nil {
			parentKey = *f.ParentID
		}
		childrenByParent[parentKey] = append(childrenByParent[parentKey], namedEntry{
			id:       f.ID,
			name:     f.Name,
			isFolder: false,
		})
	}

	localNames := make(map[string]string, len(folders)+len(files))
	for parentKey, entries := range childrenByParent {
		assigned := assignSiblingNames(parentKey == "", entries)
		for k, v := range assigned {
			localNames[k] = v
		}
	}

	folderPaths := make(map[string]string, len(folders))
	var resolveFolder func(id string, visiting map[string]bool) (string, error)
	resolveFolder = func(id string, visiting map[string]bool) (string, error) {
		if p, ok := folderPaths[id]; ok {
			return p, nil
		}
		if visiting[id] {
			return "", fmt.Errorf("folder cycle detected at %q", id)
		}
		rec, ok := folders[id]
		if !ok {
			return "", fmt.Errorf("missing parent folder %q", id)
		}
		visiting[id] = true
		defer delete(visiting, id)

		seg := localNames[entryKey(true, id)]
		if rec.ParentID == nil {
			folderPaths[id] = seg
			return seg, nil
		}
		parentPath, err := resolveFolder(*rec.ParentID, visiting)
		if err != nil {
			return "", err
		}
		full := path.Join(parentPath, seg)
		folderPaths[id] = full
		return full, nil
	}

	for id := range folders {
		if _, err := resolveFolder(id, make(map[string]bool)); err != nil {
			return nil, nil, err
		}
	}

	filePaths := make(map[string]string, len(files))
	for id, rec := range files {
		seg := localNames[entryKey(false, id)]
		if rec.ParentID == nil {
			filePaths[id] = seg
			continue
		}
		parentPath, ok := folderPaths[*rec.ParentID]
		if !ok {
			return nil, nil, fmt.Errorf("file %q references missing parent folder %q", id, *rec.ParentID)
		}
		filePaths[id] = path.Join(parentPath, seg)
	}

	return folderPaths, filePaths, nil
}

func entryKey(isFolder bool, id string) string {
	if isFolder {
		return "d:" + id
	}
	return "f:" + id
}

func assignSiblingNames(isRoot bool, entries []namedEntry) map[string]string {
	// Sort deterministically by ID so assignment never depends on map iteration order.
	slices.SortFunc(entries, func(a, b namedEntry) int {
		if a.isFolder != b.isFolder {
			if a.isFolder {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})

	groups := make(map[string][]namedEntry, len(entries))
	for _, e := range entries {
		lower := strings.ToLower(e.name)
		groups[lower] = append(groups[lower], e)
	}

	assigned := make(map[string]string, len(entries))
	usedLower := make(map[string]int, len(entries))

	// First pass: disambiguate any group with >1 entries or colliding with reserved ".filedock" at root.
	for _, e := range entries {
		lower := strings.ToLower(e.name)
		needsSuffix := len(groups[lower]) > 1 || (isRoot && lower == reservedLower)
		name := e.name
		if needsSuffix {
			name = disambiguateName(e.name, e.id, e.isFolder, 8)
		}
		assigned[entryKey(e.isFolder, e.id)] = name
		usedLower[strings.ToLower(name)]++
	}

	// Second pass: if a suffixed name still collides with another sibling's literal name, use a 16-hex tag.
	for _, e := range entries {
		k := entryKey(e.isFolder, e.id)
		if usedLower[strings.ToLower(assigned[k])] > 1 {
			assigned[k] = disambiguateName(e.name, e.id, e.isFolder, 16)
		}
	}

	return assigned
}

func disambiguateName(name, id string, isFolder bool, tagLen int) string {
	sum := sha256.Sum256([]byte(id))
	tag := hex.EncodeToString(sum[:])[:tagLen]
	if isFolder {
		return name + "__" + tag
	}
	ext := path.Ext(name)
	// Do not treat a leading dot (e.g. ".env") as an extension with an empty stem.
	if ext == "" || ext == name {
		return name + "__" + tag
	}
	stem := strings.TrimSuffix(name, ext)
	return stem + "__" + tag + ext
}
