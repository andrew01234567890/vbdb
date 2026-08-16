// Package jsondoc validates and canonicalizes VBDB JSON documents without
// converting numbers through float64. Canonical output is compact JSON with
// object keys in encoding/json's deterministic lexical order and number
// tokens emitted losslessly.
package jsondoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"
)

// encoding/json currently rejects malformed number grammar while tokenizing;
// retain this explicit regexp as defense in depth if decoder behavior changes.
var numberPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// MaxDepth is the greatest number of nested JSON arrays or objects accepted
// by Parse. Bounding nesting keeps validation and canonicalization recursion
// finite before encoding/json is asked to marshal the decoded value.
const MaxDepth = 128

// MaxDocumentBytes is the largest JSON document accepted by Parse. Bounding
// the raw input before tokenization limits decoder, validation, and
// canonicalization work in addition to the nesting bound.
const MaxDocumentBytes = 1 << 20

// MaxCanonicalBytes bounds the compact canonical output. JSON escaping can
// expand a valid input (for example, HTML-sensitive bytes), so the output has
// its own limit. encoding/json may transiently build the escaped result before
// this check (at most the fixed escape expansion of MaxDocumentBytes); together
// with MaxDocumentBytes and MaxDepth this remains a fixed bound. Value's
// defensive copy is at most another MaxCanonicalBytes-scale allocation plus
// Go's map/slice headers.
const MaxCanonicalBytes = 4 << 20

// Document is a validated JSON document and its deterministic representation.
// The decoded value retains every number as json.Number, never float64.
type Document struct {
	value     any
	canonical []byte
}

// Parse validates input, rejects duplicate object keys and trailing data, and
// returns a lossless canonical document.
func Parse(input []byte) (Document, error) {
	if len(input) > MaxDocumentBytes {
		return Document{}, fmt.Errorf("jsondoc: document exceeds %d bytes", MaxDocumentBytes)
	}
	if !utf8.Valid(input) {
		return Document{}, errors.New("jsondoc: input is not valid UTF-8")
	}
	if err := validateSurrogateEscapes(input); err != nil {
		return Document{}, fmt.Errorf("jsondoc: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	value, err := parseValue(decoder, 0)
	if err != nil {
		return Document{}, fmt.Errorf("jsondoc: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return Document{}, errors.New("jsondoc: multiple JSON values")
		}
		return Document{}, fmt.Errorf("jsondoc: trailing data: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return Document{}, fmt.Errorf("jsondoc: canonical encoding: %w", err)
	}
	if len(canonical) > MaxCanonicalBytes {
		return Document{}, fmt.Errorf("jsondoc: canonical document exceeds %d bytes", MaxCanonicalBytes)
	}
	return Document{value: value, canonical: canonical}, nil
}

// Canonicalize validates input and returns its canonical JSON bytes.
func Canonicalize(input []byte) ([]byte, error) {
	document, err := Parse(input)
	if err != nil {
		return nil, err
	}
	return document.Bytes(), nil
}

// Validate reports whether input is a supported JSON document.
func Validate(input []byte) error {
	_, err := Parse(input)
	return err
}

// Bytes returns a copy of the canonical representation. The zero Document is
// the explicit JSON null value, so it returns "null" rather than nil.
func (d Document) Bytes() []byte {
	if d.canonical == nil {
		return []byte("null")
	}
	return append([]byte(nil), d.canonical...)
}

// Value returns a deep copy of the validated decoded value. Numbers are
// json.Number values. The zero Document returns nil for JSON null. Mutating
// the result cannot change the document or its canonical representation.
func (d Document) Value() any { return cloneValue(d.value) }

func parseValue(decoder *json.Decoder, depth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token := token.(type) {
	case json.Delim:
		if depth >= MaxDepth {
			return nil, fmt.Errorf("maximum nesting depth %d exceeded", MaxDepth)
		}
		switch token {
		case '{':
			return parseObject(decoder, depth+1)
		case '[':
			return parseArray(decoder, depth+1)
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", token)
		}
	case json.Number:
		if !numberPattern.MatchString(token.String()) {
			return nil, errors.New("unsupported number form")
		}
		return token, nil
	case string, bool, nil:
		return token, nil
	default:
		return nil, fmt.Errorf("unsupported token type %T", token)
	}
}

func parseObject(decoder *json.Decoder, depth int) (map[string]any, error) {
	object := make(map[string]any)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("object key is %T, not string", token)
		}
		if _, exists := object[key]; exists {
			return nil, errors.New("duplicate object key")
		}
		value, err := parseValue(decoder, depth)
		if err != nil {
			return nil, err
		}
		object[key] = value
	}
	end, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if end != json.Delim('}') {
		return nil, fmt.Errorf("object ended with %v", end)
	}
	return object, nil
}

func parseArray(decoder *json.Decoder, depth int) ([]any, error) {
	array := make([]any, 0)
	for decoder.More() {
		value, err := parseValue(decoder, depth)
		if err != nil {
			return nil, err
		}
		array = append(array, value)
	}
	end, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if end != json.Delim(']') {
		return nil, fmt.Errorf("array ended with %v", end)
	}
	return array, nil
}

func cloneValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(value))
		for key, nested := range value {
			clone[key] = cloneValue(nested)
		}
		return clone
	case []any:
		clone := make([]any, len(value))
		for i, nested := range value {
			clone[i] = cloneValue(nested)
		}
		return clone
	default:
		return value
	}
}

func validateSurrogateEscapes(input []byte) error {
	for offset := 0; offset < len(input); offset++ {
		if input[offset] != '"' {
			continue
		}
		for offset++; offset < len(input); offset++ {
			switch input[offset] {
			case '"':
				goto nextString
			case '\\':
				offset++
				if offset >= len(input) {
					return errors.New("unterminated JSON string escape")
				}
				if input[offset] != 'u' {
					continue
				}
				if offset+4 >= len(input) {
					return errors.New("truncated Unicode escape")
				}
				code, ok := parseHexQuad(input[offset+1 : offset+5])
				if !ok {
					return errors.New("invalid Unicode escape")
				}
				if isLowSurrogate(code) {
					return errors.New("unpaired low-surrogate escape")
				}
				if !isHighSurrogate(code) {
					offset += 4
					continue
				}
				// A high surrogate must be immediately followed by a low
				// surrogate escape. encoding/json otherwise replaces it with
				// U+FFFD, which would silently change persisted data.
				if offset+10 >= len(input) || input[offset+5] != '\\' || input[offset+6] != 'u' {
					return errors.New("unpaired high-surrogate escape")
				}
				low, ok := parseHexQuad(input[offset+7 : offset+11])
				if !ok || !isLowSurrogate(low) {
					return errors.New("high-surrogate escape is not followed by a low surrogate")
				}
				offset += 10
			}
		}
		return errors.New("unterminated JSON string")
	nextString:
	}
	return nil
}

func parseHexQuad(input []byte) (uint16, bool) {
	if len(input) != 4 {
		return 0, false
	}
	var value uint16
	for _, digit := range input {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value += uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value += uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value += uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func isHighSurrogate(value uint16) bool { return value >= 0xd800 && value <= 0xdbff }
func isLowSurrogate(value uint16) bool  { return value >= 0xdc00 && value <= 0xdfff }
