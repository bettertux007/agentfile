//go:build windows

package lock

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type windowsLock struct {
	file *os.File
}

func (l *windowsLock) Close() error {
	if l.file == nil {
		return nil
	}
	var ol windows.Overlapped
	unlockErr := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &ol)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// Acquire takes an exclusive advisory lock on path without unlinking it on Close.
func Acquire(path string) (io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lockfileex %s: %w", path, err)
	}
	return &windowsLock{file: f}, nil
}
