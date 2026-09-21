//go:build unix

package datalock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFileName is the lock file inside the data directory.
const LockFileName = "otter.lock"

// Acquire takes the exclusive data-directory lock without blocking. It fails
// with ErrLocked when another process holds it.
func Acquire(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, LockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s: %v", ErrLocked, dir, err)
	}
	return &Lock{release: func() error {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}}, nil
}
