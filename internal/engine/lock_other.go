//go:build !linux

package engine

// Lockable is implemented by filesystems that provide an OS-level lock. The
// default non-Linux fallback uses an exclusive lock-file create instead.
type Lockable interface {
	TryLock() error
	Unlock() error
}
