package localdisk

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/streamingaead"
	"github.com/tink-crypto/tink-go/v2/tink"
)

const (
	StorageModePlaintext = "plaintext"
	StorageModeEncrypted = "encrypted"

	storageModeFileName = "storage_mode.json"
	storageModeVersion  = 1
	keyCheckAAD         = "filedock:v1:keycheck"
	keyCheckSentinel    = "filedock-storage-v1-ok"
)

type storageModeFile struct {
	Version  int    `json:"version"`
	Mode     string `json:"mode"`
	KeyCheck string `json:"key_check,omitempty"`
}

// GenerateKeysetFile creates a new Tink Streaming AEAD (AES-256-GCM-HKDF-4KB) JSON keyset file
// with strict 0600 permissions. It refuses to overwrite an existing file.
func GenerateKeysetFile(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return errors.New("key output path must not be empty")
	}
	if _, err := os.Stat(trimmed); err == nil {
		return fmt.Errorf("key file %q already exists; refusing to overwrite", trimmed)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat key path %q: %w", trimmed, err)
	}

	handle, err := keyset.NewHandle(streamingaead.AES256GCMHKDF4KBKeyTemplate())
	if err != nil {
		return fmt.Errorf("generate streaming AEAD keyset: %w", err)
	}
	var buf bytes.Buffer
	if err := insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buf)); err != nil {
		return fmt.Errorf("serialize streaming AEAD keyset: %w", err)
	}

	parent := filepath.Dir(trimmed)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create key parent dir: %w", err)
	}
	f, err := os.OpenFile(trimmed, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create key file %q: %w", trimmed, err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(trimmed)
		return fmt.Errorf("write key file %q: %w", trimmed, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(trimmed)
		return fmt.Errorf("sync key file %q: %w", trimmed, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(trimmed)
		return fmt.Errorf("close key file %q: %w", trimmed, err)
	}
	return nil
}

func loadStreamingAEAD(keyFile string) (tink.StreamingAEAD, error) {
	info, err := os.Stat(keyFile)
	if err != nil {
		return nil, fmt.Errorf("stat encryption key file %q: %w", keyFile, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("encryption key file %q must be a regular file", keyFile)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("encryption key file %q has overly permissive mode %04o; restrict permissions to 0600", keyFile, perm)
	}

	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read encryption key file %q: %w", keyFile, err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, fmt.Errorf("parse Tink keyset %q: %w", keyFile, err)
	}
	primitive, err := streamingaead.New(handle)
	if err != nil {
		return nil, fmt.Errorf("initialize Tink Streaming AEAD from %q: %w", keyFile, err)
	}
	return primitive, nil
}

func initStorageMode(dataDir, keyFile string) (string, tink.StreamingAEAD, error) {
	modePath := filepath.Join(dataDir, storageModeFileName)
	raw, err := os.ReadFile(modePath)
	if err == nil {
		var meta storageModeFile
		if err := json.Unmarshal(raw, &meta); err != nil {
			return "", nil, fmt.Errorf("parse %s: %w", modePath, err)
		}
		if meta.Version != storageModeVersion {
			return "", nil, fmt.Errorf("unsupported storage mode version %d in %s", meta.Version, modePath)
		}
		switch meta.Mode {
		case StorageModePlaintext:
			if keyFile != "" {
				return "", nil, fmt.Errorf("data directory %q is initialized in plaintext mode; cannot enable encryption on an existing plaintext directory", dataDir)
			}
			return StorageModePlaintext, nil, nil
		case StorageModeEncrypted:
			if keyFile == "" {
				return "", nil, fmt.Errorf("data directory %q is encrypted; FILEDOCK_ENCRYPTION_KEY_FILE is required", dataDir)
			}
			aead, err := loadStreamingAEAD(keyFile)
			if err != nil {
				return "", nil, err
			}
			if err := verifyKeyCheck(aead, meta.KeyCheck); err != nil {
				return "", nil, fmt.Errorf("encryption key %q does not match encrypted data directory %q: %w", keyFile, dataDir, err)
			}
			return StorageModeEncrypted, aead, nil
		default:
			return "", nil, fmt.Errorf("unknown storage mode %q in %s", meta.Mode, modePath)
		}
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("read %s: %w", modePath, err)
	}

	// storage_mode.json does not exist yet: detect legacy plaintext directory with pre-existing data.
	hasExisting, err := dirHasExistingData(dataDir)
	if err != nil {
		return "", nil, err
	}
	if hasExisting {
		if keyFile != "" {
			return "", nil, fmt.Errorf("existing plaintext data directory %q cannot be switched to encrypted mode; initialize a new data directory for encryption", dataDir)
		}
		if err := writeStorageModeAtomically(modePath, storageModeFile{
			Version: storageModeVersion,
			Mode:    StorageModePlaintext,
		}); err != nil {
			return "", nil, err
		}
		return StorageModePlaintext, nil, nil
	}

	// Brand-new data directory.
	if keyFile == "" {
		if err := writeStorageModeAtomically(modePath, storageModeFile{
			Version: storageModeVersion,
			Mode:    StorageModePlaintext,
		}); err != nil {
			return "", nil, err
		}
		return StorageModePlaintext, nil, nil
	}

	aead, err := loadStreamingAEAD(keyFile)
	if err != nil {
		return "", nil, err
	}
	keyCheck, err := createKeyCheck(aead)
	if err != nil {
		return "", nil, err
	}
	if err := writeStorageModeAtomically(modePath, storageModeFile{
		Version:  storageModeVersion,
		Mode:     StorageModeEncrypted,
		KeyCheck: keyCheck,
	}); err != nil {
		return "", nil, err
	}
	return StorageModeEncrypted, aead, nil
}

func dirHasExistingData(dataDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dataDir, "filedock.db")); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, sub := range []string{"objects", "uploads"} {
		entries, err := os.ReadDir(filepath.Join(dataDir, sub))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		if len(entries) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func writeStorageModeAtomically(modePath string, meta storageModeFile) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal storage mode: %w", err)
	}
	data = append(data, '\n')

	tmpFile, err := os.CreateTemp(filepath.Dir(modePath), "storage-mode-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp storage mode file: %w", err)
	}
	tmpPath := tmpFile.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write storage mode: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync storage mode: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close storage mode: %w", err)
	}
	if err := os.Rename(tmpPath, modePath); err != nil {
		return fmt.Errorf("commit storage mode: %w", err)
	}
	ok = true
	return nil
}

func createKeyCheck(aead tink.StreamingAEAD) (string, error) {
	var buf bytes.Buffer
	w, err := aead.NewEncryptingWriter(&buf, []byte(keyCheckAAD))
	if err != nil {
		return "", fmt.Errorf("encrypt key check: %w", err)
	}
	if _, err := w.Write([]byte(keyCheckSentinel)); err != nil {
		_ = w.Close()
		return "", fmt.Errorf("write key check: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close key check: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func verifyKeyCheck(aead tink.StreamingAEAD, encoded string) error {
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(ciphertext) == 0 {
		return errors.New("invalid key_check ciphertext")
	}
	r, err := aead.NewDecryptingReader(bytes.NewReader(ciphertext), []byte(keyCheckAAD))
	if err != nil {
		return err
	}
	plaintext, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if string(plaintext) != keyCheckSentinel {
		return errors.New("key_check sentinel mismatch")
	}
	return nil
}

func objectAAD(objectKey string) []byte {
	return []byte("filedock:v1:object:" + objectKey)
}

func chunkAAD(sessionID string, index int, storageKey string) []byte {
	return fmt.Appendf(nil, "filedock:v1:chunk:%s:%d:%s", sessionID, index, storageKey)
}

type decryptReadCloser struct {
	r io.Reader
	f *os.File
}

func (d *decryptReadCloser) Read(p []byte) (int, error) {
	return d.r.Read(p)
}

func (d *decryptReadCloser) Close() error {
	return d.f.Close()
}
