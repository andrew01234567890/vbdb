package raftstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/andrew01234567890/vbdb/internal/storage"
	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
)

func TestRetryIdentityPreservesCanonicalCommandAndVersion(t *testing.T) {
	command, err := NewPut("users", "a", []byte(`{"v":1}`), storage.Condition{}, uuidv7.New)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := FreezeRetryIdentity(command)
	if err != nil {
		t.Fatal(err)
	}
	replay := identity.Clone()
	if err := CompareRetryIdentity(identity, replay); err != nil {
		t.Fatalf("equal retry rejected: %v", err)
	}
	if !bytes.Equal(identity.Command, replay.Command) || identity.CandidateVersion != command.Version {
		t.Fatal("retry identity did not retain command/version")
	}
	decoded, err := DecodeCommand(replay.Command)
	if err != nil {
		t.Fatal(err)
	}
	decoded.Value = []byte(`{"v":2}`)
	encoded, err := EncodeCommand(decoded)
	if err != nil {
		t.Fatal(err)
	}
	conflict := RetryIdentity{OperationID: command.OperationID, CandidateVersion: command.Version, Command: encoded}
	conflict.Digest = sha256.Sum256(encoded)
	if err := CompareRetryIdentity(identity, conflict); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("conflicting retry returned %v", err)
	}
}

func TestRetryResultMustMatchFrozenIdentity(t *testing.T) {
	command, err := NewPut("users", "a", []byte(`{"v":1}`), storage.Condition{}, uuidv7.New)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := FreezeRetryIdentity(command)
	if err != nil {
		t.Fatal(err)
	}
	result := Result{Command: command, Status: resultSuccess, Row: storage.Row{Table: command.Table, Key: command.Key, Version: command.Version, Sequence: 1, Value: append([]byte(nil), command.Value...)}}
	if err := RetryResultMatches(identity, result); err != nil {
		t.Fatalf("matching result rejected: %v", err)
	}
	result.Command.Value = []byte(`{"v":2}`)
	if err := RetryResultMatches(identity, result); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("conflicting result returned %v", err)
	}
}
