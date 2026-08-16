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

func TestRunHelp(t *testing.T) {
	if err := run([]string{"--help"}); err != nil {
		t.Fatalf("run --help returned %v", err)
	}
}

func TestRunRequiresVersionUntilOperatorMilestone(t *testing.T) {
	err := run(nil)
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("run returned %v, want explicit not-implemented error", err)
	}
}
