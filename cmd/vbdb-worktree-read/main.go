// Command vbdb-worktree-read snapshots one worktree entry without following
// a parent or final symlink outside the supplied physical root. It is kept
// dependency-free so the publication scanner can use it as a bounded helper.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	exitUsage             = 2
	exitRead              = 3
	exitTooLarge          = 4
	exitNotPresent        = 5
	maxAllowedBytes int64 = 8 * 1024 * 1024
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		var exit *exitError
		if errors.As(err, &exit) {
			fmt.Fprintln(os.Stderr, exit.Error())
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitRead)
	}
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func run(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("vbdb-worktree-read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rootName := fs.String("root", "", "physical worktree root")
	pathName := fs.String("path", "", "repository-relative path")
	maxBytes := fs.Int64("max-bytes", 0, "maximum snapshot size")
	if err := fs.Parse(args); err != nil {
		return &exitError{code: exitUsage, err: errors.New("invalid worktree-reader arguments")}
	}
	if *rootName == "" || *pathName == "" || *maxBytes <= 0 ||
		*maxBytes > maxAllowedBytes || *maxBytes >= int64(1<<63-1) || fs.NArg() != 0 {
		return &exitError{code: exitUsage, err: errors.New("root, path, and bounded positive max-bytes are required")}
	}
	readLimit := *maxBytes + 1

	root, err := os.OpenRoot(*rootName)
	if err != nil {
		return &exitError{code: exitRead, err: errors.New("unable to open confined worktree root")}
	}
	defer root.Close()

	info, err := root.Lstat(*pathName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &exitError{code: exitNotPresent, err: errors.New("worktree path is absent")}
		}
		return &exitError{code: exitRead, err: errors.New("unable to inspect confined worktree path")}
	}

	if info.Mode()&os.ModeSymlink != 0 {
		link, err := root.Readlink(*pathName)
		if err != nil {
			return &exitError{code: exitRead, err: errors.New("unable to read confined symlink")}
		}
		return writeBounded(output, []byte(link), *maxBytes)
	}
	if !info.Mode().IsRegular() {
		return &exitError{code: exitRead, err: errors.New("confined worktree path is not a regular file")}
	}

	file, err := root.Open(*pathName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &exitError{code: exitNotPresent, err: errors.New("worktree path disappeared")}
		}
		return &exitError{code: exitRead, err: errors.New("unable to open confined worktree file")}
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, readLimit))
	if err != nil {
		return &exitError{code: exitRead, err: errors.New("unable to snapshot confined worktree file")}
	}
	return writeBounded(output, data, *maxBytes)
}

func writeBounded(output io.Writer, data []byte, maxBytes int64) error {
	if int64(len(data)) > maxBytes {
		return &exitError{code: exitTooLarge, err: errors.New("worktree snapshot exceeds bounded scan size")}
	}
	if _, err := output.Write(data); err != nil {
		return &exitError{code: exitRead, err: errors.New("unable to emit worktree snapshot")}
	}
	return nil
}
