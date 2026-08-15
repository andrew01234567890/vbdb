# ADR 0002: Native Go storage, HA, and ownership invariants

- Status: accepted architecture direction
- Scope: later milestones; no storage implementation is in Milestone 1

## Decision

Production code is native Go. Pebble is the local durable storage engine and
`etcd/raft` provides one consensus group per logical shard. A shard has RF3
voting replicas across three failure zones. Writes require a persisted quorum;
VBDB fails closed after quorum loss and never auto-promotes a minority.

Follower reads remain strong. A leader publishes a closed timestamp; a
follower can answer only after applying the requested range generation through
that timestamp. Otherwise the gateway routes to the leader. Additional
non-voting read replicas may offload reads and background work but never count
toward durability.

Primary keys map to stable 128-bit hash tokens. Every range descriptor carries
an ID, logical bounds, a monotonically increasing generation, placements, and
serving state. Gateways watch the replicated metadata catalog. Requests carry
the generation and retry stale `RANGE_MOVED` responses internally; clients
never receive an HTTP redirect.

Online split, merge, and relocation copy from a follower snapshot, replay
post-barrier mutations, verify checksums/indexes/CDC position, establish a
target quorum, and then perform one fenced metadata cutover. Before that
cutover the old owner is authoritative; after it only the new owner is. A
short cutover queue/retry and bounded stale-route forwarding provide no
client-visible downtime. Operation IDs and replicated deduplication prevent a
successful PUT from being applied twice during rerouting.

Backups choose one global HLC barrier and publish a manifest only after every
shard, transaction decision, catalog record, and CDC position is durable. PITR
restores into a new isolated cluster and replays only committed logical
transactions. The journal cannot be deleted before the maximum of CDC, PITR,
and active-backup retention dependencies.

## Not implemented in Milestone 1

There is no Pebble database, Raft group, HTTP server, metadata catalog,
follower-read path, split/merge controller, autoscaler, CDC journal, backup,
or restore path yet. The `cmd` binaries intentionally stop before claiming
any of these behaviors.
