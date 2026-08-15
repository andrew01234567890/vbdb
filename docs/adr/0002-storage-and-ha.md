# ADR 0002: Native Go storage, HA, and ownership invariants

- Status: accepted architecture direction
- Scope: later milestones; no storage implementation is in Milestone 1

## Decision

Production code is native Go. Pebble is the local durable storage engine and
`etcd/raft` provides one consensus group per logical shard. A shard has RF3
voting replicas across three failure zones. Writes require a persisted quorum;
VBDB fails closed after quorum loss and never auto-promotes a minority.

Default GET reads are linearizable when offloaded. After request invocation,
the gateway obtains a shard-leader `ReadIndex` (or an equivalent valid
leader-lease freshness fence) for the range. The fence binds the range
generation and a required applied index. A follower or non-voting read replica
may perform the data read only after matching the range generation and
applying through that index. If the leader fence is unavailable, the gateway
fails closed or reroutes to the leader; it does not silently serve an older
result.

Default QUERY is a strong statement snapshot. After request invocation, the
gateway chooses one global HLC read timestamp/barrier and binds every
participating range and every cursor page to that immutable timestamp and
generation set. Each range/read replica must be visible through that barrier
and its matching post-invocation freshness fence before contributing results;
if any range cannot serve it, the gateway fails closed or reroutes to the
leader. This is the atomic cross-range and cross-page guarantee, but its
global-barrier API/implementation is a later milestone and is not implemented
in Milestone 1. An explicit caller-supplied older HLC/as-of mode may be added
later for stale reads. Closed timestamps are reserved for that stale/as-of mode
and are not the freshness proof for default GET or QUERY. Any leader-lease
alternative must use an auditable monotonic clock and bounded-drift lease
invariant; otherwise the implementation must use `ReadIndex`. Additional
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

Transaction deadlines use a replicated/quorum-derived monotonic HLC authority,
not an individual node's wall clock. The quorum time service returns a
replicated uncertainty interval `[earliest, latest]` guaranteed to contain
cluster time under the declared maximum skew bound. `COMMITTED` may be chosen
only when `latest < deadline`; `EXPIRED` may be chosen only when
`earliest >= deadline`. If the interval straddles the deadline, or the bound
is unavailable during a clock jump, leadership change, or failover, the state
remains `OPEN` and the request returns retryable 503 until the authority
recovers. The chosen interval evidence, deadline comparison, and terminal state
are one consensus transition, retained for recovery and history checking; no
replica uses local apply-time wall clock. These mechanisms are later milestones
and are not implemented here.

## Not implemented in Milestone 1

There is no Pebble database, Raft group, HTTP server, metadata catalog,
follower-read path, split/merge controller, autoscaler, CDC journal, backup,
or restore path yet. The `cmd` binaries intentionally stop before claiming
any of these behaviors.
