package raftstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/andrew01234567890/vbdb/internal/storage"
)

func TestCommandEncodingIsCanonicalAndChecksummed(t *testing.T) {
	generate := deterministicUUIDs()
	op, _ := generate()
	version, _ := generate()
	command := Command{OperationID: op, Version: version, Table: "users", Key: "ada", Value: []byte(`{"n":1}`), Condition: storage.Condition{}}
	encoded, err := EncodeCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCommand(encoded)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := EncodeCommand(decoded)
	if err != nil || !bytes.Equal(encoded, canonical) {
		t.Fatalf("canonical bytes differ: %v", err)
	}
	encoded[len(encoded)-1] ^= 1
	if _, err := DecodeCommand(encoded); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("checksum result = %v", err)
	}
}

func TestCommandRejectsCoordinateAndConditionDrift(t *testing.T) {
	generate := deterministicUUIDs()
	op, _ := generate()
	version, _ := generate()
	match, _ := generate()
	for _, command := range []Command{
		{OperationID: op, Version: version, Table: "Users", Key: "ada", Value: []byte(`"v"`)},
		{OperationID: op, Version: version, Table: "users", Key: "a/b", Value: []byte(`"v"`)},
		{OperationID: op, Version: version, Table: "_cdc", Key: "ada", Value: []byte(`"v"`)},
		{OperationID: op, Version: version, Table: "users", Key: "ada", Value: []byte(`{"a":1,"a":2}`)},
		{OperationID: op, Version: version, Table: "users", Key: "ada", Value: []byte(`"v"`), Condition: storage.Condition{CreateOnly: true, IfMatch: &match}},
	} {
		if _, err := EncodeCommand(command); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("accepted invalid command %#v: %v", command, err)
		}
	}
}
