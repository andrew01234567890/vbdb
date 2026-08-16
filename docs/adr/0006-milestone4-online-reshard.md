# ADR 0006: Milestone 4 range identity and online split boundary

Status: accepted for the deterministic M4 implementation.

M4 represents a route with a canonical half-open key span and the complete
fence `(RangeID, Generation, OwnerEpoch, GroupID, RF3 voters, Phase)`. A
range ID is immutable. Generation changes when a catalog generation changes;
owner epoch changes when ownership or the Raft voter set changes; GroupID is
always explicit and is never inferred from RangeID. Exactly three sorted,
non-zero voters are required by this milestone's RF3-shaped harness.

Catalog values are bounded, versioned, canonical bytes protected by CRC32C.
Decoding validates lengths and counts before allocation, validates complete
coverage from negative to positive infinity, and re-encodes to reject a
non-canonical representation. APIs return defensive copies. `COPYING` and
`CATCHING_UP` descriptors are never routable.

This first slice defines vocabulary and pure validation only. Durable catalog
publication, ReadIndex serving, split transfer, and cutover are supplied by
the higher stacked slices. M4 remains a deterministic in-process proof; it
does not claim production routing RPC, independent child Raft lifecycle, or
automatic reshard policy.

## Durable catalog boundary

The catalog is persisted as one complete canonical value under the owned
engine's `m4/catalog/complete` key. The value is synced before the in-memory
pointer is replaced. Restart accepts only a complete, checksummed image and
cross-checks every current descriptor's exact RF3 voters with the durable
local Raft `ConfState`. A sync failure is fatal and cannot expose a partial
route. Removed range IDs remain retired tombstones, so a higher catalog
version cannot resurrect an old owner. This is a local durable metadata hook;
it is not a production catalog replication protocol.

## Follower-read linearization

`ReadIndex` is the only serving read path for a routed replica. The request
first captures a defensive copy of the serving descriptor and catalog version,
then registers a bounded request context before asking local Raft for a quorum
ReadIndex. Contexts contain a per-open, durably rotated boot nonce, a
monotonic sequence, and a CRC32C; cancellation retires the exact context so a
delayed response cannot satisfy a later request. The Ready loop copies and
correlates each `ReadState` only after the Ready's durable persistence and
logical apply work. Malformed or unknown contexts fail the replica closed.

The returned index is a logical applied-index fence. The replica waits until
its durable state machine has applied at least that index, reads under the
state lock, and then reacquires the catalog lock. It compares the complete
descriptor (span, range ID, generation, owner epoch, group, voters, phase) and
catalog version with the captured copy before returning the row. Any route
movement discards the copied row and returns `ErrRangeMoved`; a locally present
but stale row is never a successful fallback. Engine LSNs and local row
sequence values are not read freshness evidence.

Pending contexts are bounded by both count and retained coordinate bytes, and
all waiters wake on apply, cancellation, close, fatal storage failure, or
ReadIndex failure. This implementation is an in-process M4 proof over the M3
Raft transport; it does not claim production routing RPC, cache invalidation,
or automatic retry policy.

## Non-serving split transfer proof

The transfer layer models one source range and two child projections. It takes
the source's durable logical applied index as the barrier and binds the
snapshot manifest to the complete source descriptor, owner epoch, row bounds,
and CRC32C-protected canonical image. A matching healthy non-leader voter is
preferred as the copy source; leader fallback is available only as an
explicit deterministic-harness option. The source remains the sole serving
authority throughout this slice.

Snapshot bytes move through bounded `VBCP` chunks. Receivers validate magic,
version, count, index, lengths, reordering, exact duplicate equality, and the
complete checksum before exposing rows. Every post-barrier delta carries the
source span, immutable ID, generation, owner epoch, group, voters, phase, and
configuration fingerprint, together with canonical command/result identity.
The retained suffix is bounded by both count and bytes; missing or conflicting
sequences fail closed.

Catch-up sorts a copied suffix and requires strict contiguous source sequence.
It validates and applies the complete projected left/right images before one
swap, so a late malformed delta leaves the live child images unchanged. The
target proof requires both children to remain `CATCHING_UP`, exact adjacent
spans, matching RF3 membership, matching durable `ConfState`, and a quorum
acknowledgement. This branch does not publish child routes or claim a cutover;
that is one later durable catalog replacement.
