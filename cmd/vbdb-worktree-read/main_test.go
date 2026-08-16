package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunSnapshotsRegularFileAndBoundsGrowth(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "data.txt")
	if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := run([]string{"--root", root, "--path", "data.txt", "--max-bytes", "8"}, &output)
	if err != nil || output.String() != "snapshot" {
		t.Fatalf("run returned %v and %q", err, output.String())
	}
	output.Reset()
	err = run([]string{"--root", root, "--path", "data.txt", "--max-bytes", "7"}, &output)
	if code := errorCode(err); code != exitTooLarge || output.Len() != 0 {
		t.Fatalf("oversized run code=%d output=%q, want code %d and empty output", code, output.String(), exitTooLarge)
	}
	output.Reset()
	err = run([]string{"--root", root, "--path", "data.txt", "--max-bytes", "8388608"}, &output)
	if err != nil || output.String() != "snapshot" {
		t.Fatalf("8 MiB boundary returned %v and %q", err, output.String())
	}
}

func TestRunRejectsInvalidOrOverflowingMaxBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "data.txt")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, maxBytes := range []string{"0", "-1", "8388609", "9223372036854775807"} {
		var output bytes.Buffer
		err := run([]string{"--root", root, "--path", "data.txt", "--max-bytes", maxBytes}, &output)
		if code := errorCode(err); code != exitUsage || output.Len() != 0 {
			t.Errorf("max-bytes=%s code=%d output=%q, want usage and empty output", maxBytes, code, output.String())
		}
	}
}

func TestRunReturnsFinalSymlinkTextWithoutFollowingTarget(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	target := filepath.Join(external, "secret")
	if err := os.WriteFile(target, []byte("external secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	maxBytes := strconv.Itoa(len(target))
	if err := run([]string{"--root", root, "--path", "link", "--max-bytes", maxBytes}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != target {
		t.Fatalf("output=%q, want link text", output.String())
	}
	if strings.Contains(output.String(), "external secret") {
		t.Fatal("external file bytes were followed")
	}

	output.Reset()
	err := run([]string{"--root", root, "--path", "link", "--max-bytes", strconv.Itoa(len(target) - 1)}, &output)
	if code := errorCode(err); code != exitTooLarge || output.Len() != 0 {
		t.Fatalf("under-bound symlink code=%d output=%q, want code %d and empty output", code, output.String(), exitTooLarge)
	}
}

func TestRunRejectsEscapingParentSymlink(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "secret"), []byte("external secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := run([]string{"--root", root, "--path", "nested/secret", "--max-bytes", "128"}, &output)
	if code := errorCode(err); code != exitRead || output.Len() != 0 {
		t.Fatalf("escaping run code=%d output=%q, want code %d and empty output", code, output.String(), exitRead)
	}
}

func TestRunConcurrentSwapNeverEmitsExternalBytes(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	secret := []byte("external secret bytes")
	if err := os.WriteFile(filepath.Join(external, "secret"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "race.txt")
	if err := os.WriteFile(path, []byte("safe snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			safeTemp := filepath.Join(root, "safe.tmp")
			_ = os.WriteFile(safeTemp, []byte("safe snapshot"), 0o600)
			_ = os.Rename(safeTemp, path)
			linkTemp := filepath.Join(root, "link.tmp")
			_ = os.Symlink(filepath.Join(external, "secret"), linkTemp)
			_ = os.Rename(linkTemp, path)
		}
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("swap goroutine did not stop")
		}
	}()

	for i := 0; i < 200; i++ {
		var output bytes.Buffer
		err := run([]string{"--root", root, "--path", "race.txt", "--max-bytes", "64"}, &output)
		if strings.Contains(output.String(), string(secret)) {
			t.Fatalf("iteration %d emitted external bytes: %q", i, output.String())
		}
		if err != nil {
			if output.Len() != 0 {
				t.Fatalf("iteration %d returned error with non-empty output %q", i, output.String())
			}
			var exit *exitError
			if !errors.As(err, &exit) || (exit.code != exitRead && exit.code != exitNotPresent && exit.code != exitTooLarge) {
				t.Fatalf("iteration %d returned unexpected error %v", i, err)
			}
		}
	}
}

func TestRunConcurrentGrowthRemainsBounded(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "grow.txt")
	if err := os.WriteFile(path, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err == nil {
				_, _ = file.WriteString("0123456789")
				_ = file.Close()
			}
		}
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("growth goroutine did not stop")
		}
	}()

	for i := 0; i < 100; i++ {
		var output bytes.Buffer
		err := run([]string{"--root", root, "--path", "grow.txt", "--max-bytes", "64"}, &output)
		if output.Len() > 64 {
			t.Fatalf("iteration %d emitted %d bytes", i, output.Len())
		}
		if err != nil {
			if output.Len() != 0 {
				t.Fatalf("iteration %d returned error with non-empty output %q", i, output.String())
			}
			var exit *exitError
			if !errors.As(err, &exit) || exit.code != exitTooLarge {
				t.Fatalf("iteration %d returned unexpected error %v", i, err)
			}
		}
	}
}

func errorCode(err error) int {
	var exit *exitError
	if err != nil && errors.As(err, &exit) {
		return exit.code
	}
	return 0
}
