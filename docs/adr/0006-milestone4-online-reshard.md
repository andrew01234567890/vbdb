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
