# ADR 0004: Milestone 2 single-node durable engine

- Status: accepted for the development-only Milestone 2
- Scope: one process and one Pebble data directory; no distributed claims

## Decision

Use the pinned Pebble commit `abab24a50ec83672322ca925a212eb33634615e6` as the
local durable engine. Canonical `pkg/codec` tuples form physical keys for one
durable local sequence, immutable row versions, a durable UUID-version index,
and a current head. A writer lock reads the head, validates OCC conditions and
storage-owned coordinates, allocates the next sequence and a UUIDv7 that has
never appeared in immutable history, and commits history, head, version index,
and sequence in one `pebble.Sync` batch. The durable version index is checked
at write time and validated against history at open, so UUID reuse remains
rejected after restart without an unbounded process-local version set. A
bounded generator retry handles collisions without allowing a broken generator
to block a writer forever. Reads use the current head or one bounded history
lookup at a requested sequence.

Pebble treats a real WAL write/fsync failure as fail-stop: its commit pipeline
calls the configured logger's `Fatalf`, and the production logger exits the
process. The wrapper has a defensive terminal state only for returnable
pre-pipeline or batch-lifecycle errors; callers must reopen after such an
error. On Unix-like platforms, data directories are created and required to
have owner-only `0700` permissions before Pebble opens them; Windows relies on
its ACL model. The final path is cleaned before
`Lstat`, and the directory identity/mode is checked again after Pebble opens;
this is a practical race check, not a claim of race-free protection against a
hostile parent-directory operator. M2 trusts the configured path's parent
directories and defers fd-relative Pebble opening/fencing.

UUIDv7 is an opaque row version/ETag and never establishes commit order. The
binary records are versioned and checksummed. A malformed persisted record is
corruption, not a missing row. Storage validates canonical JSON at the
durability boundary, and the HTTP layer applies the exact GET/PUT condition
contract and returns stable problem envelopes without filesystem or
internal-error details. Persisted row and version-index values are rejected
against hard encoded-size bounds before checksum work or payload copies.

M2's open-time integrity pass is intentionally O(N) over persisted records and
uses transient validation state bounded by the on-disk dataset. Constant-memory
restart validation is deferred; the live engine no longer retains a
total-commit UUID registry because uniqueness is checked through durable
version-index records.

Table names are temporary implicit physical namespaces; there is no catalog or
DDL. A data directory is single-process only. The gateway bounds accepted
connections and active requests before body buffering, and graceful shutdown
has a hard process-exit deadline that never closes storage beneath a stuck
handler. Metadata/storage roles, transactions, QUERY, indexes, CDC, Raft,
replication, and cluster coordination stay deferred.
