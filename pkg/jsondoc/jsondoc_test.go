package jsondoc

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
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

func TestCanonicalEscapesHTMLDeterministically(t *testing.T) {
	canonical, err := Canonicalize([]byte(`{"value":"<&>"}`))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"value":"\u003c\u0026\u003e"}`
	if string(canonical) != want {
		t.Fatalf("canonical HTML escaping = %s, want %s", canonical, want)
	}
	again, err := Canonicalize(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != want {
		t.Fatalf("canonical HTML escaping was not idempotent: %s", again)
	}
}

func TestZeroDocumentIsJSONNull(t *testing.T) {
	var document Document
	if got := string(document.Bytes()); got != "null" {
		t.Fatalf("zero Document.Bytes() = %q, want null", got)
	}
	if got := document.Value(); got != nil {
		t.Fatalf("zero Document.Value() = %#v, want nil", got)
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

func TestRejectsNestingBeyondMaximum(t *testing.T) {
	for _, test := range []struct {
		name  string
		open  string
		close string
	}{
		{name: "arrays", open: "[", close: "]"},
		{name: "objects", open: `{"a":`, close: "}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			boundary := strings.Repeat(test.open, MaxDepth) + "null" + strings.Repeat(test.close, MaxDepth)
			if err := Validate([]byte(boundary)); err != nil {
				t.Fatalf("boundary depth rejected: %v", err)
			}
			tooDeep := strings.Repeat(test.open, MaxDepth+1) + "null" + strings.Repeat(test.close, MaxDepth+1)
			if err := Validate([]byte(tooDeep)); err == nil {
				t.Fatal("depth above maximum was accepted")
			}
			malformed := strings.Repeat(test.open, MaxDepth+1)
			if err := Validate([]byte(malformed)); err == nil {
				t.Fatal("malformed over-depth input was accepted")
			}
		})
	}
}

func TestValueReturnsDeepCopy(t *testing.T) {
	document, err := Parse([]byte(`{"nested":{"items":[{"value":1}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	value := document.Value().(map[string]any)
	nested := value["nested"].(map[string]any)
	items := nested["items"].([]any)
	items[0].(map[string]any)["value"] = json.Number("99")
	items = append(items, json.Number("2"))
	nested["items"] = items
	nested["new"] = true
	value["nested"] = nested
	value["other"] = "changed"

	if got, want := string(document.Bytes()), `{"nested":{"items":[{"value":1}]}}`; got != want {
		t.Fatalf("mutating Value changed canonical bytes: got %s, want %s", got, want)
	}
	second := document.Value().(map[string]any)
	if _, ok := second["other"]; ok {
		t.Fatal("mutating Value changed the stored value")
	}
	if got := second["nested"].(map[string]any)["items"].([]any); len(got) != 1 {
		t.Fatalf("mutating Value changed stored nested slice: %#v", got)
	}
}
