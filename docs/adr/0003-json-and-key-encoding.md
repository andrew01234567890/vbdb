# ADR 0003: Canonical document and key encoding

- Status: accepted and implemented in Milestone 1

## Decision

Persistent tuple keys use `pkg/codec`, not protobuf. Format v1 accepts at most
`MaxComponents = 128` components and `MaxTupleBytes = 1 MiB`; EncodeTuple and
DecodeTuple enforce both bounds, including zero-escape expansion. A tuple is
version byte
`01`, followed by typed components, followed by byte `00`. Bytes and UTF-8
strings use a zero-escape (`00 ff`) and `00 00` terminator. Fixed-width
integers are big-endian; signed int64 flips the sign bit before encoding.
Tuple bytes sort by value for components of the same kind and preserve prefix
ordering. Decoding rejects unknown tags, malformed escapes, invalid UTF-8,
truncation, and trailing bytes.
The `Bytes`/`BytesChecked` and `String` constructors reject variable values
whose raw length or zero-escape expansion cannot fit the tuple budget before
copying or converting them; `Bytes` retains its one-return compatibility API
by returning an invalid component that reports the same error when used.
Constructors copy caller-owned byte slices, and EncodeTuple performs the
remaining zero-escape and multi-component budget checks before appending data.

`pkg/jsondoc` parses with `json.Decoder.UseNumber`, rejects duplicate keys at
any depth, invalid UTF-8, trailing values, and invalid/non-finite number forms.
It rejects documents larger than `MaxDocumentBytes = 1 MiB` before parsing,
and invalid-number errors are stable and redacted rather than echoing the
untrusted token.
Unpaired UTF-16 surrogate escapes are rejected rather than normalized to
U+FFFD; paired surrogate escapes remain valid and canonicalize deterministically.
The explicit number regexp is intentional defense in depth: the current
`encoding/json` decoder already prevalidates number grammar, but this package
retains its own check at the token boundary.
Container nesting is explicitly bounded at `MaxDepth` (128), before recursive
canonicalization. Canonical output is compact JSON with deterministic
object-key ordering and number tokens emitted without a float64 conversion,
preserving arbitrarily large integer precision. Exact numeric token spellings
are preserved in the canonical bytes; those bytes are not a semantic
numeric-equality oracle. For example, `1`, `1.0`, and `1e0` remain distinct
canonical spellings. `encoding/json` HTML escaping remains deliberate in
canonical v1: `<`, `>`, `&`, U+2028, and U+2029 are emitted using the standard
`\u003c`, `\u003e`, `\u0026`, `\u2028`, and `\u2029` escapes; this is deterministic
escaping, not an RFC 8785/JCS claim.
Canonical output is bounded separately at `MaxCanonicalBytes = 4 MiB`; this
covers expansion from HTML/control escaping. `encoding/json` may transiently
build the escaped result before that check, but its expansion is bounded by
the fixed 1 MiB input and JSON escape forms. The input, canonical bytes, and
one defensive `Document.Value` copy are therefore bounded by fixed byte
limits (plus Go map/slice header overhead), rather than by an unbounded
caller-controlled expansion. `Document.Value` returns a deep copy, so callers cannot mutate the persisted
representation through the decoded map or slices. The zero `Document` is the
explicit canonical JSON null value: `Bytes` returns `null` and `Value` returns
`nil`.

## Consequences

The formats are deterministic and safe to use as persistent-key and row-value
contracts. Any future format change requires a new explicit format version;
there is no implicit protobuf or floating-point compatibility behavior.
