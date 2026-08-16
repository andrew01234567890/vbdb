//go:build linux

package engine

import (
	"errors"
	"syscall"
)

// Lockable is optionally implemented by File implementations that can hold
// an OS-level exclusive process lock. OSFS uses flock, which does not leave a
// stale lock after a crash.
type Lockable interface {
	TryLock() error
	Unlock() error
}

func (f *osFile) TryLock() error {
	return syscall.Flock(int(f.File.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func (f *osFile) Unlock() error {
	if err := syscall.Flock(int(f.File.Fd()), syscall.LOCK_UN); err != nil {
		if errors.Is(err, syscall.EBADF) {
			return nil
		}
		return err
	}
	return nil
}
