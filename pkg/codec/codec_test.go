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
	want := []Component{Bytes([]byte{0, 1, 255}), name, Int64(math.MinInt64), Uint64(math.MaxUint64), Bool(true), Bool(false)}
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
		{1, byte(StringKind), 0, 1}, {1, byte(BoolKind), 2, 0},
		{1, 99, 0}, {1, byte(Uint64Kind), 0, 0}, {1, 0, 7},
	}
	for _, input := range cases {
		if _, err := DecodeTuple(input); err == nil {
			t.Errorf("DecodeTuple(%x) accepted malformed input", input)
		}
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
