// Package codec contains the versioned, non-protobuf encodings used by
// persistent VBDB keys. The format is deliberately small and specified here
// so persistence does not depend on a general-purpose serialization library.
package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

const formatVersion byte = 1

const int64SignMask uint64 = 1 << 63

// Kind identifies a tuple component's canonical representation.
type Kind byte

const (
	// These numeric assignments are persistent format-v1 tags. Do not use
	// iota here: adding or reordering a kind must never rewrite old keys.
	BytesKind  Kind = 0x01
	StringKind Kind = 0x02
	Int64Kind  Kind = 0x03
	Uint64Kind Kind = 0x04
	BoolKind   Kind = 0x05
)

// Component is one typed value in a tuple. Construct components with the
// constructors below; a zero Component is invalid.
type Component struct {
	kind Kind
	data []byte
}

func Bytes(value []byte) Component {
	return Component{kind: BytesKind, data: append([]byte(nil), value...)}
}

func String(value string) (Component, error) {
	if !utf8.ValidString(value) {
		return Component{}, errors.New("codec: string is not valid UTF-8")
	}
	return Component{kind: StringKind, data: []byte(value)}, nil
}

func Int64(value int64) Component {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], uint64(value)^int64SignMask)
	return Component{kind: Int64Kind, data: data[:]}
}

func Uint64(value uint64) Component {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	return Component{kind: Uint64Kind, data: data[:]}
}

func Bool(value bool) Component {
	if value {
		return Component{kind: BoolKind, data: []byte{1}}
	}
	return Component{kind: BoolKind, data: []byte{0}}
}

func (c Component) Kind() Kind { return c.kind }

func (c Component) Bytes() ([]byte, error) {
	if c.kind != BytesKind {
		return nil, fmt.Errorf("codec: component is %s, not bytes", c.kind)
	}
	return append([]byte(nil), c.data...), nil
}

// Text returns the string value of a StringKind component.
func (c Component) Text() (string, error) {
	if c.kind != StringKind {
		return "", fmt.Errorf("codec: component is %s, not string", c.kind)
	}
	return string(c.data), nil
}

func (c Component) Int64() (int64, error) {
	if c.kind != Int64Kind {
		return 0, fmt.Errorf("codec: component is %s, not int64", c.kind)
	}
	if len(c.data) != 8 {
		return 0, errors.New("codec: invalid int64 component")
	}
	return int64(binary.BigEndian.Uint64(c.data) ^ int64SignMask), nil
}

func (c Component) Uint64() (uint64, error) {
	if c.kind != Uint64Kind {
		return 0, fmt.Errorf("codec: component is %s, not uint64", c.kind)
	}
	if len(c.data) != 8 {
		return 0, errors.New("codec: invalid uint64 component")
	}
	return binary.BigEndian.Uint64(c.data), nil
}

func (c Component) Bool() (bool, error) {
	if c.kind != BoolKind {
		return false, fmt.Errorf("codec: component is %s, not bool", c.kind)
	}
	if len(c.data) != 1 || c.data[0] > 1 {
		return false, errors.New("codec: invalid bool component")
	}
	return c.data[0] == 1, nil
}

func (k Kind) String() string {
	switch k {
	case BytesKind:
		return "bytes"
	case StringKind:
		return "string"
	case Int64Kind:
		return "int64"
	case Uint64Kind:
		return "uint64"
	case BoolKind:
		return "bool"
	default:
		return fmt.Sprintf("kind(%d)", byte(k))
	}
}

// EncodeTuple returns a canonical tuple. The byte layout is:
//
//	01                                 format version
//	component*                         typed components
//	00                                 tuple terminator
//
// Bytes and strings use a memcomparable escaped terminator: 00 bytes become
// 00 ff and the component ends with 00 00. Fixed-width numeric payloads use
// big-endian bytes; signed integers flip the sign bit. Consequently values of
// one kind, and tuples with equal component prefixes, sort in value order with
// bytes.Compare. Kind tags intentionally define ordering between unlike kinds.
func EncodeTuple(components ...Component) ([]byte, error) {
	encoded := []byte{formatVersion}
	for _, component := range components {
		if err := validate(component); err != nil {
			return nil, err
		}
		encoded = append(encoded, byte(component.kind))
		switch component.kind {
		case BytesKind, StringKind:
			encoded = appendEscaped(encoded, component.data)
		default:
			encoded = append(encoded, component.data...)
		}
	}
	return append(encoded, 0), nil
}

// DecodeTuple decodes one complete tuple and rejects unknown kinds, malformed
// escapes, truncated fixed-width values, and trailing bytes.
func DecodeTuple(encoded []byte) ([]Component, error) {
	if len(encoded) < 2 || encoded[0] != formatVersion {
		return nil, errors.New("codec: invalid tuple version or length")
	}
	var components []Component
	for offset := 1; offset < len(encoded); {
		if encoded[offset] == 0 {
			if offset != len(encoded)-1 {
				return nil, errors.New("codec: trailing bytes after tuple terminator")
			}
			return components, nil
		}
		kind := Kind(encoded[offset])
		offset++
		switch kind {
		case BytesKind, StringKind:
			data, next, err := readEscaped(encoded, offset)
			if err != nil {
				return nil, err
			}
			if kind == StringKind && !utf8.Valid(data) {
				return nil, errors.New("codec: string component is not valid UTF-8")
			}
			components = append(components, Component{kind: kind, data: data})
			offset = next
		case Int64Kind, Uint64Kind:
			if len(encoded)-offset < 8 {
				return nil, errors.New("codec: truncated numeric component")
			}
			components = append(components, Component{kind: kind, data: append([]byte(nil), encoded[offset:offset+8]...)})
			offset += 8
		case BoolKind:
			if offset >= len(encoded) || encoded[offset] > 1 {
				return nil, errors.New("codec: invalid bool component")
			}
			components = append(components, Component{kind: kind, data: []byte{encoded[offset]}})
			offset++
		default:
			return nil, fmt.Errorf("codec: unknown component kind 0x%02x", byte(kind))
		}
	}
	return nil, errors.New("codec: missing tuple terminator")
}

func validate(c Component) error {
	switch c.kind {
	case BytesKind:
		return nil
	case StringKind:
		if !utf8.Valid(c.data) {
			return errors.New("codec: string is not valid UTF-8")
		}
		return nil
	case Int64Kind, Uint64Kind:
		if len(c.data) != 8 {
			return errors.New("codec: numeric component has invalid width")
		}
		return nil
	case BoolKind:
		if len(c.data) != 1 || c.data[0] > 1 {
			return errors.New("codec: bool component has invalid value")
		}
		return nil
	default:
		return fmt.Errorf("codec: unknown component kind %d", c.kind)
	}
}

func appendEscaped(dst, source []byte) []byte {
	for _, b := range source {
		dst = append(dst, b)
		if b == 0 {
			dst = append(dst, 0xff)
		}
	}
	return append(dst, 0, 0)
}

func readEscaped(encoded []byte, offset int) ([]byte, int, error) {
	data := make([]byte, 0, 16)
	for offset < len(encoded) {
		b := encoded[offset]
		offset++
		if b != 0 {
			data = append(data, b)
			continue
		}
		if offset >= len(encoded) {
			return nil, 0, errors.New("codec: unterminated variable component")
		}
		escape := encoded[offset]
		offset++
		switch escape {
		case 0:
			return data, offset, nil
		case 0xff:
			data = append(data, 0)
		default:
			return nil, 0, errors.New("codec: invalid variable-component escape")
		}
	}
	return nil, 0, errors.New("codec: unterminated variable component")
}

// Equal reports whether two components have identical canonical values.
func Equal(a, b Component) bool { return a.kind == b.kind && bytes.Equal(a.data, b.data) }
