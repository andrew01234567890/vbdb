package uuidv7

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDeterministicBitsAndCanonicalText(t *testing.T) {
	random := bytes.NewReader(bytes.Repeat([]byte{0xab}, 10))
	uuid, err := (Generator{Now: func() time.Time { return time.UnixMilli(0x010203040506) }, Rand: random}).New()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := uuid.String(), "01020304-0506-7bab-abab-abababababab"; got != want {
		t.Fatalf("UUIDv7 text = %q, want %q", got, want)
	}
	if uuid[6]>>4 != 7 || uuid[8]>>6 != 2 {
		t.Fatalf("version/variant bits not set: %x", uuid)
	}
	parsed, err := Parse(uuid.String())
	if err != nil || parsed != uuid {
		t.Fatalf("Parse(String(uuid)) = %x, %v", parsed, err)
	}
}

func TestParseRejectsNonCanonicalOrNonV7(t *testing.T) {
	valid := "01020304-0506-7bab-abab-abababababab"
	for _, value := range []string{
		strings.ToUpper(valid), "0102030405067bcb8bababababababab", "01020304-0506-6bcb-8bab-abababababab",
		"01020304-0506-7bcb-cbab-abababababab", "01020304-0506-7bcb-8bab-abababababag",
	} {
		if _, err := Parse(value); err == nil {
			t.Errorf("Parse(%q) unexpectedly succeeded", value)
		}
	}
}

func TestRandomError(t *testing.T) {
	_, err := (Generator{Now: func() time.Time { return time.UnixMilli(1) }, Rand: strings.NewReader("short")}).New()
	if err == nil || !strings.Contains(err.Error(), "random source") {
		t.Fatalf("New error = %v, want random source error", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("random error = %v, want wrapped EOF", err)
	}
}

func TestClockRangeAndBinaryValidation(t *testing.T) {
	for name, now := range map[string]time.Time{
		"before unix epoch":          time.Unix(-1, 0),
		"beyond 48-bit milliseconds": time.UnixMilli(int64(1 << 48)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (Generator{Now: func() time.Time { return now }, Rand: bytes.NewReader(bytes.Repeat([]byte{1}, 10))}).New(); err == nil {
				t.Fatal("out-of-range clock unexpectedly accepted")
			}
		})
	}
	valid := mustTestUUID()
	for name, input := range map[string][]byte{
		"short":   valid[:15],
		"long":    append(append([]byte(nil), valid[:]...), 0),
		"version": func() []byte { copy := valid; copy[6] = 0x60; return copy[:] }(),
		"variant": func() []byte { copy := valid; copy[8] = 0x40; return copy[:] }(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := UUIDFromBytes(input); err == nil {
				t.Fatal("invalid binary UUID unexpectedly accepted")
			}
		})
	}
}

func mustTestUUID() UUID {
	uuid, err := (Generator{Now: func() time.Time { return time.UnixMilli(1) }, Rand: bytes.NewReader(bytes.Repeat([]byte{1}, 10))}).New()
	if err != nil {
		panic(err)
	}
	return uuid
}
