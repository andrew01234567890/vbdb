package raftstore

import (
	"bytes"
	"crypto/sha256"

	"github.com/andrew01234567890/vbdb/pkg/uuidv7"
)

// RetryIdentity freezes the exact proposal bytes and candidate row version at
// the first attempt. Movement and response loss may refresh routing, but they
// never mint another version or operation ID.
type RetryIdentity struct {
	OperationID      uuidv7.UUID
	CandidateVersion uuidv7.UUID
	Command          []byte
	Digest           [32]byte
}

func FreezeRetryIdentity(command Command) (RetryIdentity, error) {
	if err := validateCommand(command); err != nil {
		return RetryIdentity{}, err
	}
	encoded, err := EncodeCommand(command)
	if err != nil {
		return RetryIdentity{}, err
	}
	return RetryIdentity{OperationID: command.OperationID, CandidateVersion: command.Version, Command: append([]byte(nil), encoded...), Digest: sha256.Sum256(encoded)}, nil
}

func (identity RetryIdentity) Clone() RetryIdentity {
	identity.Command = append([]byte(nil), identity.Command...)
	return identity
}

func (identity RetryIdentity) Validate() error {
	if _, err := uuidv7.UUIDFromBytes(identity.OperationID[:]); err != nil {
		return ErrInvalidOperationID
	}
	if _, err := uuidv7.UUIDFromBytes(identity.CandidateVersion[:]); err != nil {
		return ErrInvalidCommand
	}
	if len(identity.Command) == 0 || sha256.Sum256(identity.Command) != identity.Digest {
		return ErrOperationConflict
	}
	command, err := DecodeCommand(identity.Command)
	if err != nil || command.OperationID != identity.OperationID || command.Version != identity.CandidateVersion {
		return ErrOperationConflict
	}
	return nil
}

func (identity RetryIdentity) Equal(other RetryIdentity) bool {
	return identity.OperationID == other.OperationID && identity.CandidateVersion == other.CandidateVersion && identity.Digest == other.Digest && bytes.Equal(identity.Command, other.Command)
}

// CompareRetryIdentity distinguishes an idempotent replay from deterministic
// operation conflict. A conflicting replay must not mutate state.
func CompareRetryIdentity(first, retry RetryIdentity) error {
	if err := first.Validate(); err != nil {
		return err
	}
	if err := retry.Validate(); err != nil {
		return err
	}
	if first.OperationID != retry.OperationID {
		return nil
	}
	if first.Equal(retry) {
		return nil
	}
	return ErrOperationConflict
}

func RetryResultMatches(identity RetryIdentity, result Result) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	encoded, err := EncodeCommand(result.Command)
	if err != nil || !bytes.Equal(encoded, identity.Command) {
		return ErrOperationConflict
	}
	return nil
}
