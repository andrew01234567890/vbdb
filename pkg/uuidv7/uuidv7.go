// Package uuidv7 implements the UUID version 7 format from RFC 9562.
//
// A UUIDv7 is an opaque identifier in VBDB. It is intentionally not used as a
// commit-ordering primitive; the storage engine has its own durable sequence.
package uuidv7

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	textLength  = 36
	maxUnixMS   = uint64(1<<48 - 1)
	variantMask = byte(0xc0)
)

// UUID is a 128-bit UUIDv7 value.
type UUID [16]byte

// Generator makes UUIDv7 values using an injected clock and random source.
// The zero Generator uses time.Now and crypto/rand.Reader.
type Generator struct {
	Now  func() time.Time
	Rand io.Reader
}

// New returns a UUIDv7 using the production clock and cryptographic random
// source.
func New() (UUID, error) { return (Generator{}).New() }

// New returns a UUIDv7 with the generator's clock and random source.
func (g Generator) New() (UUID, error) {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	random := io.Reader(rand.Reader)
	if g.Rand != nil {
		random = g.Rand
	}
	ms := now().UnixMilli()
	if ms < 0 || uint64(ms) > maxUnixMS {
		return UUID{}, errors.New("uuidv7: clock is outside the UUID timestamp range")
	}
	var u UUID
	u[0] = byte(uint64(ms) >> 40)
	u[1] = byte(uint64(ms) >> 32)
	u[2] = byte(uint64(ms) >> 24)
	u[3] = byte(uint64(ms) >> 16)
	u[4] = byte(uint64(ms) >> 8)
	u[5] = byte(ms)
	if _, err := io.ReadFull(random, u[6:]); err != nil {
		return UUID{}, fmt.Errorf("uuidv7: random source: %w", err)
	}
	u[6] = 0x70 | (u[6] & 0x0f)
	u[8] = 0x80 | (u[8] & 0x3f)
	return u, nil
}

// Parse accepts only canonical lowercase hyphenated UUID text and UUIDv7
// values with the RFC 9562 variant.
func Parse(text string) (UUID, error) {
	if len(text) != textLength || text[8] != '-' || text[13] != '-' || text[18] != '-' || text[23] != '-' {
		return UUID{}, errors.New("uuidv7: invalid canonical text")
	}
	for i, r := range text {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if r >= 'A' && r <= 'F' {
			return UUID{}, errors.New("uuidv7: uppercase text is not canonical")
		}
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return UUID{}, errors.New("uuidv7: invalid hexadecimal text")
		}
	}
	var u UUID
	if _, err := hex.Decode(u[:], []byte(strings.ReplaceAll(text, "-", ""))); err != nil {
		return UUID{}, errors.New("uuidv7: invalid hexadecimal text")
	}
	if u[6]>>4 != 7 || u[8]&variantMask != 0x80 {
		return UUID{}, errors.New("uuidv7: not an RFC 9562 UUIDv7")
	}
	return u, nil
}

// String returns canonical lowercase hyphenated UUID text.
func (u UUID) String() string {
	var text [textLength]byte
	hex.Encode(text[0:8], u[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], u[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], u[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], u[8:10])
	text[23] = '-'
	hex.Encode(text[24:36], u[10:16])
	return string(text[:])
}

// Bytes returns a copy of the UUID bytes.
func (u UUID) Bytes() []byte { return append([]byte(nil), u[:]...) }

// UUIDFromBytes constructs a UUIDv7 from its binary representation.
func UUIDFromBytes(value []byte) (UUID, error) {
	if len(value) != 16 {
		return UUID{}, errors.New("uuidv7: UUID must contain 16 bytes")
	}
	var u UUID
	copy(u[:], value)
	if u[6]>>4 != 7 || u[8]&variantMask != 0x80 {
		return UUID{}, errors.New("uuidv7: not an RFC 9562 UUIDv7")
	}
	return u, nil
}
