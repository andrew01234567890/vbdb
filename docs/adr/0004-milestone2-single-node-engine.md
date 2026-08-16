# ADR 0004: Milestone 2 single-node owned engine

- Status: accepted and implemented for the M2 development-only single-node scope
- Scope: development-only, one process, one local data directory; no
  distributed claims

## Decision

Milestone 2 will use a small native-Go ordered key/value engine owned by VBDB.
It is deliberately a raw durability and snapshot layer, not a SQL engine, a
transaction coordinator, a Raft log, or an MVCC policy engine. The raw API
accepts bounded byte keys and values, exposes one serialized writer and
concurrent read snapshots, and commits an already-encoded batch as one atomic
unit. It does not know tables, JSON, row versions, ETags, transactions,
indexes, CDC, or global ordering.

The storage/MVCC adapter is the only layer that assigns row semantics. It
encodes canonical `pkg/codec` tuples for the sequence metadata, immutable row
history, durable UUID-version index, and current head. A writer validates the
coordinates and OCC condition under the writer lock, allocates the next local
row sequence, generates a UUIDv7, and submits exactly four records (history,
head, version-index, and sequence) in one raw-engine batch. The adapter sets
the raw engine's key/value/batch bounds from the exact legal encoded storage
limits. Reads use the current head or a forward-merged bounded history lookup.
A history record is never overwritten; current-head state is the latest
visible version for a logical table/key. There is no catalog or DDL in M2.

The raw engine's physical LSN is a node-local recovery coordinate. It orders
WAL batches and identifies what has been replayed or flushed; it is not a row
sequence, a UUID, a wall clock, or a distributed timestamp. M2 row sequences
are storage-layer coordinates used by local historical reads. UUIDv7 is an
opaque row version and strong HTTP ETag only; it never establishes commit
order. In a later replicated design, the Raft apply index will be the
authoritative MVCC timestamp (encoded in the MVCC layer after the logical key,
in descending order), while the physical LSN remains local. Raft metadata,
transaction records, global indexes, deduplication, and cross-range atomicity
remain above the raw engine.

## Raw engine contract

The first owned-engine slice has an active mutable memtable and a segmented
WAL. Every batch is fully encoded and length-bounded before it is appended.
Each WAL frame has a versioned header, bounded length, batch/LSN metadata, the
complete operation payload, and a CRC32C over the frame. The writer handles
short writes, synchronizes the WAL, and publishes the batch to the active
memtable only after sync succeeds. A successful write therefore means
durable-before-visible: all operations in the batch become visible together,
and no operation is visible when the durability point was not reached. Group
commit is not implemented in M2; each accepted batch has its own WAL sync.

The implemented M2 format is a two-level LSM: overlapping immutable L0 SSTs
and non-overlapping, range-partitioned L1 SSTs. SST blocks, SST metadata, and
the manifest use versioned, bounded, checksummed records. Publication writes
and syncs temporary files, renames them, syncs the containing directory, and
then advances the checksummed `CURRENT` pointer with the same protocol. A
manifest supersedes old SSTs and WAL segments before they can be deleted, and
readers/snapshots/iterators must release their pins before obsolete SSTs are
removed. Flush and compaction are synchronous in this milestone; there is no
background compaction, group commit, block cache, or metrics subsystem yet.

All engine file access is rooted beneath one owner-only data directory through
Go's `os.Root` API. Opening the root rejects symlinks and non-directories,
enforces owner-only permissions on Unix-like systems, and acquires exclusive
single-process ownership before serving. This is a local ownership boundary,
not a distributed fencing protocol. No second process may open the directory.
The root is closed only after the engine and ownership lock have stopped all
access; configured parent directories remain outside the engine's trust
boundary.

## Recovery and failure policy

Open first validates the bounded, checksummed manifest and every referenced
immutable file, then replays WAL batches beyond the manifest's flushed LSN.
Only an incomplete suffix of the final active WAL segment may be truncated.
An incomplete frame anywhere else, a checksum failure, malformed complete
frame, unsupported version, length violation, LSN gap, conflicting duplicate,
invalid key/value, missing manifest member, or SST/manifest corruption is a
terminal recovery error. It is never treated as a missing key and is never
silently repaired or salvaged. The process must fail closed: it must not
acknowledge writes or serve data when durability or recovery is unproven.
There is no online migration or salvage mode in v1.

The persisted record formats use explicit versions, CRC32C, and hard bounds
before checksum work or payload allocation/copying. Bounds apply to keys,
values, frame lengths, record counts, SST metadata, and manifest members.
Volume encryption is the only v1 at-rest encryption assumption. v1 does not
add engine compression, levels above L1, mmap or direct I/O, range tombstones,
or remote SST reads. Background compaction, group commit, block caching,
metrics, backups, Raft, and distributed MVCC timestamps are later-milestone
work.

## What this ADR does and does not claim

The M2 implementation and tests prove the HTTP GET/PUT/OCC contract, canonical
JSON and coordinate validation, UUIDv7 ETag behavior, local row
history/current-head semantics, bounded versioned row records with CRC32C,
four-record atomic row batches through the owned engine, close/reopen behavior,
WAL framing/replay, durable-before-visible publication, strict corruption
handling, descriptor-rooted ownership, immutable SST/manifest publication,
two-level LSM reads and compaction, snapshot pinning, and several injected
failure paths. Row startup logical-integrity validation still builds
startup-only maps proportional to the persisted row/history count; constant-
memory row validation is deferred.

M2 remains explicitly development-only and single-node. `Last` is a forward
merge over the iterator and is not performance-optimized. M3+ may add Raft
apply timestamps, transactions, global indexes, replication, CDC, immutable
snapshot manifests, and the Raft journal. Later backup/PITR will combine an
immutable snapshot manifest with a retained Raft journal; a raw-engine
snapshot or local LSN alone is not PITR.

The M2 gateway remains development-only and single-node. Metadata/storage
roles, transactions, QUERY, admin, indexes, CDC, Raft, replication, and
cluster coordination stay deferred.
