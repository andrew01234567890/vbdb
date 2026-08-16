package codec

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"
)

func TestFormatV1GoldenVectors(t *testing.T) {
	text, err := String("a\x00b")
	if err != nil {
		t.Fatal(err)
	}
	vectors := []struct {
		name       string
		hexEncoded string
		components []Component
	}{
		{name: "empty tuple", hexEncoded: "0100"},
		{name: "empty bytes", hexEncoded: "0101000000", components: []Component{mustBytes(t, nil)}},
		{name: "zero escaped bytes", hexEncoded: "010100ff000000", components: []Component{mustBytes(t, []byte{0})}},
		{name: "zero escaped string", hexEncoded: "01026100ff62000000", components: []Component{text}},
		{name: "signed minimum", hexEncoded: "0103000000000000000000", components: []Component{Int64(math.MinInt64)}},
		{name: "signed negative one", hexEncoded: "01037fffffffffffffff00", components: []Component{Int64(-1)}},
		{name: "signed zero", hexEncoded: "0103800000000000000000", components: []Component{Int64(0)}},
		{name: "signed maximum", hexEncoded: "0103ffffffffffffffff00", components: []Component{Int64(math.MaxInt64)}},
		{name: "unsigned zero", hexEncoded: "0104000000000000000000", components: []Component{Uint64(0)}},
		{name: "unsigned maximum", hexEncoded: "0104ffffffffffffffff00", components: []Component{Uint64(math.MaxUint64)}},
		{name: "false", hexEncoded: "01050000", components: []Component{Bool(false)}},
		{name: "true", hexEncoded: "01050100", components: []Component{Bool(true)}},
		{name: "tuple prefix", hexEncoded: "010261000000", components: []Component{mustString(t, "a")}},
		{name: "tuple after prefix", hexEncoded: "0102610000050100", components: []Component{mustString(t, "a"), Bool(true)}},
	}
	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			want, err := hex.DecodeString(vector.hexEncoded)
			if err != nil {
				t.Fatal(err)
			}
			got, err := EncodeTuple(vector.components...)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("encoded %x, want independent v1 bytes %x", got, want)
			}
			decoded, err := DecodeTuple(want)
			if err != nil {
				t.Fatalf("decoding hard-coded v1 bytes: %v", err)
			}
			if len(decoded) != len(vector.components) {
				t.Fatalf("decoded %d components, want %d", len(decoded), len(vector.components))
			}
			for i := range decoded {
				if !Equal(decoded[i], vector.components[i]) {
					t.Fatalf("decoded component %d differs", i)
				}
			}
		})
	}
}

func TestFormatV1KindTagsAreStable(t *testing.T) {
	got := []Kind{BytesKind, StringKind, Int64Kind, Uint64Kind, BoolKind}
	want := []Kind{0x01, 0x02, 0x03, 0x04, 0x05}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kind %d = 0x%02x, want persistent tag 0x%02x", i, got[i], want[i])
		}
	}
}

func mustString(t *testing.T, value string) Component {
	t.Helper()
	component, err := String(value)
	if err != nil {
		t.Fatal(err)
	}
	return component
}

func mustBytes(t *testing.T, value []byte) Component {
	t.Helper()
	return Bytes(value)
}
