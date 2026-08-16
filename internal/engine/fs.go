package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// File is the small file surface used by the engine. Implementations may
// deliberately return short writes or injected Sync errors in tests.
type File interface {
	ioReaderAt
	ioWriterAt
	Close() error
	Stat() (fs.FileInfo, error)
	Sync() error
	Truncate(size int64) error
}

// FS is the filesystem seam. Paths passed to a custom FS are already confined
// to the Open root; the production OSFS is additionally accessed through the
// descriptor-relative os.Root path below.
type FS interface {
	MkdirAll(path string, perm fs.FileMode) error
	Lstat(path string) (fs.FileInfo, error)
	OpenFile(path string, flag int, perm fs.FileMode) (File, error)
	ReadDir(path string) ([]fs.DirEntry, error)
	Rename(oldPath, newPath string) error
	Remove(path string) error
}

// OSFS is the production filesystem implementation.
type OSFS struct{}

func (OSFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }
func (OSFS) Lstat(path string) (fs.FileInfo, error)       { return os.Lstat(path) }
func (OSFS) OpenFile(path string, flag int, perm fs.FileMode) (File, error) {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	return &osFile{File: f}, nil
}
func (OSFS) ReadDir(path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }
func (OSFS) Rename(oldPath, newPath string) error       { return os.Rename(oldPath, newPath) }
func (OSFS) Remove(path string) error                   { return os.Remove(path) }

// These local aliases keep the public File interface readable while avoiding
// an accidental dependency on methods not needed by the engine.
type ioReaderAt interface {
	ReadAt(p []byte, off int64) (n int, err error)
}
type ioWriterAt interface {
	WriteAt(p []byte, off int64) (n int, err error)
}

type osFile struct{ *os.File }

func (f *osFile) Stat() (fs.FileInfo, error) { return f.File.Stat() }

// rootFS enforces the root boundary even when a custom FS implementation is
// supplied. The custom filesystem sees only canonical paths below root.
type rootFS struct {
	base string
	fs   FS
	root *os.Root
}

func (r rootFS) relative(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty relative path", ErrFilesystem)
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: absolute path escapes root", ErrFilesystem)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path escapes root", ErrFilesystem)
	}
	return clean, nil
}

func (r rootFS) path(name string) (string, error) {
	clean, err := r.relative(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.base, clean), nil
}

func (r rootFS) MkdirAll(name string, perm fs.FileMode) error {
	if r.root != nil {
		clean, err := r.relative(name)
		if err != nil {
			return err
		}
		return r.root.MkdirAll(clean, perm)
	}
	path, err := r.path(name)
	if err != nil {
		return err
	}
	return r.fs.MkdirAll(path, perm)
}
func (r rootFS) Lstat(name string) (fs.FileInfo, error) {
	if r.root != nil {
		clean, err := r.relative(name)
		if err != nil {
			return nil, err
		}
		return r.root.Lstat(clean)
	}
	path, err := r.path(name)
	if err != nil {
		return nil, err
	}
	return r.fs.Lstat(path)
}
func (r rootFS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	if r.root != nil {
		clean, err := r.relative(name)
		if err != nil {
			return nil, err
		}
		file, err := r.root.OpenFile(clean, flag, perm)
		if err != nil {
			return nil, err
		}
		return &osFile{File: file}, nil
	}
	path, err := r.path(name)
	if err != nil {
		return nil, err
	}
	return r.fs.OpenFile(path, flag, perm)
}
func (r rootFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if r.root != nil {
		clean, err := r.relative(name)
		if err != nil {
			return nil, err
		}
		return fs.ReadDir(r.root.FS(), clean)
	}
	path, err := r.path(name)
	if err != nil {
		return nil, err
	}
	return r.fs.ReadDir(path)
}
func (r rootFS) Rename(oldName, newName string) error {
	if r.root != nil {
		oldClean, err := r.relative(oldName)
		if err != nil {
			return err
		}
		newClean, err := r.relative(newName)
		if err != nil {
			return err
		}
		return r.root.Rename(oldClean, newClean)
	}
	oldPath, err := r.path(oldName)
	if err != nil {
		return err
	}
	newPath, err := r.path(newName)
	if err != nil {
		return err
	}
	return r.fs.Rename(oldPath, newPath)
}
func (r rootFS) Remove(name string) error {
	if r.root != nil {
		clean, err := r.relative(name)
		if err != nil {
			return err
		}
		return r.root.Remove(clean)
	}
	path, err := r.path(name)
	if err != nil {
		return err
	}
	return r.fs.Remove(path)
}

func (r rootFS) closeRoot() error {
	if r.root == nil {
		return nil
	}
	return r.root.Close()
}

func syncOSDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open parent directory for sync: %v", ErrFilesystem, err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("%w: sync parent directory: %v", ErrFilesystem, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("%w: close parent directory: %v", ErrFilesystem, err)
	}
	return nil
}

func prepareDirectory(fsys rootFS, name string) error {
	info, err := fsys.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := fsys.MkdirAll(name, 0o700); err != nil {
			return fmt.Errorf("%w: create %s: %v", ErrFilesystem, name, err)
		}
		info, err = fsys.Lstat(name)
	}
	if err != nil {
		return fmt.Errorf("%w: stat %s: %v", ErrFilesystem, name, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s", ErrInvalidDataDir, name)
	}
	return nil
}

func ensureDirectory(fsys rootFS, name string) error {
	if err := fsys.MkdirAll(name, 0o700); err != nil {
		return fmt.Errorf("%w: create %s: %v", ErrFilesystem, name, err)
	}
	return prepareDirectory(fsys, name)
}

func validSegmentName(name string) (uint64, bool) {
	const prefix = "segment-"
	const suffix = ".wal"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if len(digits) != 20 {
		return 0, false
	}
	var n uint64
	for i := range digits {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
		n = n*10 + uint64(digits[i]-'0')
	}
	return n, true
}

func segmentName(n uint64) string { return fmt.Sprintf("segment-%020d.wal", n) }
