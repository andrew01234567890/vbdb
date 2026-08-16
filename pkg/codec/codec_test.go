package codec

import (
	"bytes"
	"math"
	"testing"
)

func TestTupleRoundTrip(t *testing.T) {
	name, err := String("a\x00b")
	if err != nil {
		t.Fatal(err)
	}
	bytesComponent := Bytes([]byte{0, 1, 255})
	want := []Component{bytesComponent, name, Int64(math.MinInt64), Uint64(math.MaxUint64), Bool(true), Bool(false)}
	encoded, err := EncodeTuple(want...)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeTuple(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d components, want %d", len(got), len(want))
	}
	for i := range want {
		if !Equal(want[i], got[i]) {
			t.Fatalf("component %d differs: %#v != %#v", i, want[i], got[i])
		}
	}
}

func TestTupleLexicographicOrder(t *testing.T) {
	values := []string{"", "a", "a\x00", "aa", "b"}
	encoded := make([][]byte, len(values))
	for i, value := range values {
		component, err := String(value)
		if err != nil {
			t.Fatal(err)
		}
		encoded[i], err = EncodeTuple(component)
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < len(encoded); i++ {
		if bytes.Compare(encoded[i-1], encoded[i]) >= 0 {
			t.Fatalf("encoded %q is not less than %q", values[i-1], values[i])
		}
	}
}

func TestFixedWidthKindsSortByValue(t *testing.T) {
	tests := []struct {
		name   string
		values []Component
	}{
		{
			name:   "int64",
			values: []Component{Int64(math.MinInt64), Int64(-1), Int64(0), Int64(1), Int64(math.MaxInt64)},
		},
		{
			name:   "uint64",
			values: []Component{Uint64(0), Uint64(1), Uint64(math.MaxUint64)},
		},
		{
			name:   "bool",
			values: []Component{Bool(false), Bool(true)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded := make([][]byte, len(test.values))
			for i, value := range test.values {
				var err error
				encoded[i], err = EncodeTuple(value)
				if err != nil {
					t.Fatal(err)
				}
			}
			for i := 1; i < len(encoded); i++ {
				if bytes.Compare(encoded[i-1], encoded[i]) >= 0 {
					t.Fatalf("encoded value %d is not less than value %d", i-1, i)
				}
			}
		})
	}
}

func TestTuplePrefixSortsBeforeLongerTuple(t *testing.T) {
	a, err := String("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := String("b")
	if err != nil {
		t.Fatal(err)
	}
	short, err := EncodeTuple(a)
	if err != nil {
		t.Fatal(err)
	}
	long, err := EncodeTuple(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(short, long) >= 0 {
		t.Fatalf("tuple prefix %x was not less than longer tuple %x", short, long)
	}
}

func FuzzTupleRoundTrip(f *testing.F) {
	seeds := [][]byte{
		{formatVersion, 0},
		{formatVersion, byte(BytesKind), 0, 0, 0},
		{formatVersion, byte(StringKind), 'a', 0, 0, 0},
		{formatVersion, byte(Int64Kind), 0x80, 0, 0, 0, 0, 0, 0, 0, 0},
		{formatVersion, byte(Uint64Kind), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0},
		{formatVersion, byte(BoolKind), 1, 0},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		components, err := DecodeTuple(input)
		if err != nil {
			return
		}
		canonical, err := EncodeTuple(components...)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(canonical, input) {
			t.Fatalf("successful decode was not canonical: input=%x canonical=%x", input, canonical)
		}
	})
}

func TestDecodeRejectsMalformedInput(t *testing.T) {
	cases := [][]byte{
		{}, {2, 0}, {1}, {1, byte(StringKind), 'a', 0},
		{1, byte(StringKind), 0xff, 0, 0, 0},
		{1, byte(StringKind), 0, 1}, {1, byte(BoolKind), 2, 0},
		{1, 99, 0}, {1, byte(Uint64Kind), 0, 0}, {1, 0, 7},
	}
	for _, input := range cases {
		if _, err := DecodeTuple(input); err == nil {
			t.Errorf("DecodeTuple(%x) accepted malformed input", input)
		}
	}
}

func TestTupleResourceBounds(t *testing.T) {
	components := make([]Component, MaxComponents)
	for i := range components {
		components[i] = Bool(false)
	}
	encoded, err := EncodeTuple(components...)
	if err != nil {
		t.Fatalf("max-component tuple: %v", err)
	}
	if _, err := DecodeTuple(encoded); err != nil {
		t.Fatalf("decode max-component tuple: %v", err)
	}
	if _, err := EncodeTuple(append(components, Bool(true))...); err == nil {
		t.Fatal("EncodeTuple accepted too many components")
	}
	tooMany := append([]byte{formatVersion}, make([]byte, 0, MaxComponents*2+1)...)
	for i := 0; i < MaxComponents+1; i++ {
		tooMany = append(tooMany, byte(BoolKind), 0)
	}
	tooMany = append(tooMany, 0)
	if _, err := DecodeTuple(tooMany); err == nil {
		t.Fatal("DecodeTuple accepted too many components")
	}

	plain := bytes.Repeat([]byte{'a'}, MaxTupleBytes-5)
	plainComponent := Bytes(plain)
	encoded, err = EncodeTuple(plainComponent)
	if err != nil || len(encoded) != MaxTupleBytes {
		t.Fatalf("plain tuple boundary: len=%d err=%v", len(encoded), err)
	}
	if _, err := DecodeTuple(encoded); err != nil {
		t.Fatalf("decode plain tuple boundary: %v", err)
	}
	escaped := bytes.Repeat([]byte{'a'}, MaxTupleBytes-6)
	escaped[MaxTupleBytes/2] = 0
	escapedComponent := Bytes(escaped)
	encoded, err = EncodeTuple(escapedComponent)
	if err != nil || len(encoded) != MaxTupleBytes {
		t.Fatalf("escaped tuple boundary: len=%d err=%v", len(encoded), err)
	}
	if _, err := DecodeTuple(encoded); err != nil {
		t.Fatalf("decode escaped tuple boundary: %v", err)
	}
	if _, err := DecodeTuple(append(encoded, 0)); err == nil {
		t.Fatal("DecodeTuple accepted an over-limit tuple")
	}
	tooMuchEscaping := Component{kind: BytesKind, data: bytes.Repeat([]byte{0}, MaxTupleBytes-5)}
	if _, err := EncodeTuple(tooMuchEscaping); err == nil {
		t.Fatal("EncodeTuple accepted a component whose zero escapes exceed the tuple limit")
	}
}

func TestConstructorsCopyByteSlices(t *testing.T) {
	input := []byte{1, 2}
	component := Bytes(input)
	input[0] = 9
	got, err := component.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 1 {
		t.Fatalf("constructor retained mutable input: %v", got)
	}
}

func TestConstructorsRejectOversizedVariableComponentsBeforeCopy(t *testing.T) {
	tooLarge := make([]byte, MaxTupleBytes)
	if _, err := BytesChecked(tooLarge); err == nil {
		t.Fatal("Bytes accepted a source that cannot fit a tuple")
	}
	if _, err := Bytes(tooLarge).Bytes(); err == nil {
		t.Fatal("legacy Bytes constructor returned a usable oversized component")
	}
	if _, err := String(string(tooLarge)); err == nil {
		t.Fatal("String accepted a source that cannot fit a tuple")
	}
	if _, err := BytesChecked(bytes.Repeat([]byte{0}, MaxTupleBytes-5)); err == nil {
		t.Fatal("Bytes accepted a source whose zero escapes cannot fit a tuple")
	}
}

func TestEncodeRejectsOversizedComponentBeforeTraversingData(t *testing.T) {
	// This Component is intentionally assembled inside package codec to model a
	// caller bypassing the constructors. The encoder must reject its impossible
	// raw length before walking or copying the data.
	component := Component{kind: BytesKind, data: make([]byte, MaxTupleBytes+1)}
	if _, err := EncodeTuple(component); err == nil {
		t.Fatal("EncodeTuple accepted an oversized component")
	}
}

func TestTextAccessorIsExplicit(t *testing.T) {
	component, err := String("hello")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := component.Text(); err != nil || got != "hello" {
		t.Fatalf("Text() = %q, %v", got, err)
	}
}

func TestInvalidDeferredComponentPropagatesThroughEveryAccessor(t *testing.T) {
	invalid := Bytes(make([]byte, MaxTupleBytes))
	if invalid.Valid() {
		t.Fatal("deferred constructor failure reported as valid")
	}
	if invalid.Err() == nil {
		t.Fatal("deferred constructor failure was not exposed")
	}
	if invalid.Kind() != InvalidKind {
		t.Fatalf("invalid component Kind() = %v, want InvalidKind", invalid.Kind())
	}
	if Equal(invalid, invalid) {
		t.Fatal("Equal treated a deferred-error component as a value")
	}
	if _, err := invalid.Bytes(); err == nil {
		t.Fatal("Bytes accessor accepted deferred constructor error")
	}
	if _, err := invalid.Text(); err == nil {
		t.Fatal("Text accessor discarded deferred constructor error")
	}
	if _, err := invalid.Int64(); err == nil {
		t.Fatal("Int64 accessor discarded deferred constructor error")
	}
	if _, err := invalid.Uint64(); err == nil {
		t.Fatal("Uint64 accessor discarded deferred constructor error")
	}
	if _, err := invalid.Bool(); err == nil {
		t.Fatal("Bool accessor discarded deferred constructor error")
	}
}

func TestInvalidZeroAndMalformedComponentsAreNotValues(t *testing.T) {
	var zero Component
	if zero.Valid() || zero.Err() == nil || zero.Kind() != InvalidKind {
		t.Fatalf("zero component state: valid=%v err=%v kind=%v", zero.Valid(), zero.Err(), zero.Kind())
	}
	if Equal(zero, zero) {
		t.Fatal("Equal treated zero component as a value")
	}
	malformed := Component{kind: Int64Kind, data: []byte{1}}
	if malformed.Valid() || malformed.Err() == nil || malformed.Kind() != InvalidKind {
		t.Fatalf("malformed component state: valid=%v err=%v kind=%v", malformed.Valid(), malformed.Err(), malformed.Kind())
	}
	if Equal(malformed, malformed) {
		t.Fatal("Equal treated malformed component as a value")
	}
	oversized := Component{kind: BytesKind, data: make([]byte, MaxTupleBytes)}
	if oversized.Valid() || oversized.Err() == nil || oversized.Kind() != InvalidKind {
		t.Fatalf("oversized component state: valid=%v err=%v kind=%v", oversized.Valid(), oversized.Err(), oversized.Kind())
	}
	if Equal(oversized, oversized) {
		t.Fatal("Equal treated oversized component as a value")
	}
}
