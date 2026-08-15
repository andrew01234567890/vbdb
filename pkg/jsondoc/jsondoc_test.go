package jsondoc

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"
)

func TestCanonicalizeSortsKeysAndPreservesInteger(t *testing.T) {
	input := []byte(`{"z":1,"a":900719925474099312345678901234567890,"nested":{"b":true,"a":null}}`)
	got, err := Canonicalize(input)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":900719925474099312345678901234567890,"nested":{"a":null,"b":true},"z":1}`
	if string(got) != want {
		t.Fatalf("canonical JSON = %s, want %s", got, want)
	}
	document, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	object := document.Value().(map[string]any)
	number, ok := object["a"].(json.Number)
	if !ok {
		t.Fatalf("integer type = %T, want json.Number", object["a"])
	}
	if _, ok := new(big.Int).SetString(number.String(), 10); !ok {
		t.Fatalf("integer was not retained exactly: %q", number)
	}
}

func TestRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	for _, input := range []string{
		`{"a":1,"a":2}`,
		`{"a":{"x":1,"x":2}}`,
		`[{"x":1,"x":2}]`,
	} {
		if err := Validate([]byte(input)); err == nil {
			t.Errorf("accepted duplicate keys: %s", input)
		}
	}
}

func TestRejectsInvalidUTF8TrailingAndUnsupportedNumbers(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"a":1} {"b":2}`),
		[]byte(`{"a":NaN}`),
		[]byte(`{"a":1e}`),
		[]byte(`{"a":01}`),
		{'{', '"', 'a', '"', ':', '"', 0xff, '"', '}'},
	}
	for _, input := range cases {
		if err := Validate(input); err == nil {
			t.Errorf("accepted invalid JSON %x", input)
		}
	}
}

func TestRejectsUnpairedSurrogateEscapes(t *testing.T) {
	for _, input := range []string{
		`{"value":"\ud800"}`,
		`{"value":"\udc00"}`,
		`{"value":"\ud800x"}`,
		`{"value":"\ud800\u0041"}`,
		`{"value":"\ud800\ud7ff"}`,
	} {
		if err := Validate([]byte(input)); err == nil {
			t.Errorf("accepted unpaired surrogate escape: %s", input)
		}
	}
}

func TestAcceptsPairedSurrogatesDeterministically(t *testing.T) {
	input := []byte(`{"value":"\ud834\udd1e"}`)
	first, err := Canonicalize(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Canonicalize(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != `{"value":"𝄞"}` || string(second) != string(first) {
		t.Fatalf("paired surrogate canonicalization = %s then %s", first, second)
	}
}

func TestPreservesNumericTokenSpellings(t *testing.T) {
	spellings := []string{"1", "1.0", "1e0", "-0", "1E+2"}
	canonical := make([]string, len(spellings))
	for i, spelling := range spellings {
		got, err := Canonicalize([]byte(spelling))
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", spelling, err)
		}
		canonical[i] = string(got)
		if canonical[i] != spelling {
			t.Errorf("Canonicalize(%q) = %q, want exact spelling", spelling, canonical[i])
		}
	}
	for i := range canonical {
		for j := i + 1; j < len(canonical); j++ {
			if canonical[i] == canonical[j] {
				t.Errorf("canonical spellings %q and %q unexpectedly equal", spellings[i], spellings[j])
			}
		}
	}
}

func TestCanonicalBytesAreIndependentCopies(t *testing.T) {
	canonical, err := Canonicalize([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	canonical[0] = 'x'
	document, err := Parse([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(document.Bytes(), []byte(`{"a":1}`)) {
		t.Fatalf("document bytes changed through returned slice: %s", document.Bytes())
	}
}
