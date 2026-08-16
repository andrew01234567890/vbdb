# ADR 0005: Milestone 3 owned-engine Raft shard

- Status: accepted for this milestone
- Scope: one logical shard, an in-process RF3 correctness harness

## Decision

Each replica uses `go.etcd.io/raft/v3` and persists all Raft records and
logical state through `*engine.Engine`. The engine WAL, SSTs, manifest, and
`CURRENT` are the only physical durability layer; no Raft-specific WAL and no
Pebble dependency are permitted. Existing Pebble markers are rejected before
open because this unreleased format has no migration contract.

Commands are canonical, versioned, CRC32C-checked bytes. The proposer supplies
both the operation UUIDv7 and row UUIDv7 before proposal. Apply never creates
identities. Operation results and dedupe records are committed with logical
rows and the applied index, so an ambiguous retry either replays the exact
result or returns an operation conflict without mutation.

The Raft apply index is the replicated MVCC coordinate (`storage.Row.Sequence`)
and UUIDv7 remains only an opaque ETag/OCC identity. Coordinates are validated
by `internal/coordinates` and reused by HTTP, storage, and Raft commands.

## Durability order

For each Ready:

1. Persist the Ready's entries, HardState, and snapshot metadata/chunks.
2. Apply committed commands and persist rows/history, results/dedupe, applied
   index, and ConfState.
3. Clone and send messages only after both durable phases succeed.
4. Call `Advance` last.

Engine poison, corruption, or an uncertain logical persistence result is fatal
to that replica. Pending proposals are bounded by count and retained command
bytes. Committed entries are applied one at a time so each logical batch stays
within the owned engine's 4,096-operation/16 MiB limits; a Ready admission
bound prevents an oversized Raft persistence batch from being silently split.

Snapshots use a checksummed logical stream and 256 KiB engine records. Chunks
are staged by snapshot-index generation and the metadata pointer is published
only after all chunks are durable. Incomplete generations are ignored on
restart. Snapshot installation remains bounded by the same per-entry engine
batch limits.

## Deliberate exclusions

This ADR does not claim production transport, routing/catalog authority,
linearizable follower serving, transactions, indexes, CDC, backup/PITR,
resharding, or operator policy. The transport is injectable and the RF3 tests
are deterministic fault-harness tests, not a production networking proof.
