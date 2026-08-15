# ADR 0003: Canonical document and key encoding

- Status: accepted and implemented in Milestone 1

## Decision

Persistent tuple keys use `pkg/codec`, not protobuf. A tuple is version byte
`01`, followed by typed components, followed by byte `00`. Bytes and UTF-8
strings use a zero-escape (`00 ff`) and `00 00` terminator. Fixed-width
integers are big-endian; signed int64 flips the sign bit before encoding.
Tuple bytes sort by value for components of the same kind and preserve prefix
ordering. Decoding rejects unknown tags, malformed escapes, invalid UTF-8,
truncation, and trailing bytes.

`pkg/jsondoc` parses with `json.Decoder.UseNumber`, rejects duplicate keys at
any depth, invalid UTF-8, trailing values, and invalid/non-finite number forms.
Container nesting is explicitly bounded at `MaxDepth` (128), before recursive
canonicalization. Canonical output is compact JSON with deterministic
object-key ordering and number tokens emitted without a float64 conversion,
preserving arbitrarily large integer precision. Exact numeric token spellings
are preserved in the canonical bytes; those bytes are not a semantic
numeric-equality oracle. For example, `1`, `1.0`, and `1e0` remain distinct
canonical spellings. `Document.Value` returns a deep copy, so callers cannot
mutate the persisted representation through the decoded map or slices.

## Consequences

The formats are deterministic and safe to use as persistent-key and row-value
contracts. Any future format change requires a new explicit format version;
there is no implicit protobuf or floating-point compatibility behavior.
