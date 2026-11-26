package syncclient

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var (
	dirLocksMu sync.Mutex
	dirLocks   = make(map[string]*sync.Mutex)
)

func lockDirInProcess(dir string) func() {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	dirLocksMu.Lock()
	mu, ok := dirLocks[abs]
	if !ok {
		mu = &sync.Mutex{}
		dirLocks[abs] = mu
	}
	dirLocksMu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// lockMirrorFile acquires an exclusive OS-level flock on .filedock/lock inside root.
// Callers must ensure .filedock exists first and hold the in-process dir lock.
func lockMirrorFile(root *os.Root) (func(), error) {
	f, err := root.OpenFile(metaDirName+"/lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open mirror lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire mirror lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
