package main

import (
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunRequiresStrictRole(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--role", "unknown"}, {"--role", "gateway", "extra"}, {"--version", "--role", "gateway"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run(%v) unexpectedly succeeded", args)
		}
	}
}

func TestRunRejectsUnimplementedRoleHonestly(t *testing.T) {
	err := run([]string{"--role", "storage"})
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("run returned %v, want explicit not-implemented error", err)
	}
}
