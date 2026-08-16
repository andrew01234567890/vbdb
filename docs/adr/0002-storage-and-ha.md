# ADR 0002: Native Go storage, HA, and ownership invariants

- Status: accepted architecture direction
- Scope: later milestones; no storage implementation is in Milestone 1

## Decision

Production code is native Go. VBDB owns its local durable storage engine and
`etcd/raft` provides one consensus group per logical shard. A shard has RF3
voting replicas across three failure zones. Writes require a persisted quorum;
VBDB fails closed after quorum loss and never auto-promotes a minority. RF3 is
the stated crash-fault model: the shard remains durable and available through
one replica crash (subject to quorum placement), not through arbitrary
Byzantine or correlated storage failures.

The owned engine is a bounded ordered key/value LSM, not a general SQL or
distributed transaction engine. Its raw layer provides one serialized writer,
concurrent pinned readers, atomic synced batches, a segmented checksummed WAL,
bounded mutable and immutable memtables, immutable checksummed SSTables, and a
checksummed manifest. The first production format deliberately has only
overlapping L0 files and non-overlapping range-partitioned L1 files. Additional
levels, compression, mmap, direct I/O, range tombstones, remote SST reads, and
adaptive filters require later evidence and format decisions.

Physical engine LSNs are node-local recovery coordinates and never become
distributed timestamps. The MVCC layer encodes the authoritative per-range
Raft apply index in descending order after each canonical logical key. UUIDv7
remains an opaque proposer-owned row version/ETag, while commit HLC remains
cross-range barrier and CDC metadata. Raft metadata, transaction records,
deduplication results, primary rows, and local index records may share one
physical engine through separate canonical namespaces without sharing their
logical visibility rules. Global indexes remain separate replicated ranges and
therefore still require the transaction protocol; a local storage batch cannot
claim cross-range atomicity.

Deduplication result GC is itself a replicated durability transition. A full
result may be removed only after the same durable state contains bounded
anti-reuse evidence or a verifiable generation/expiry fence for that identity;
recovery must restore that evidence or fence before the replica serves writes.
Retired, expired, or indeterminate identities are rejected without mutation,
and exhaustion or uncertainty in anti-reuse admission fails closed rather than
turning the identity into a fresh blind PUT. Full results also have configured
count and encoded-byte limits at principal/tenant/route/target/range and
cluster scopes; projected capacity is reserved atomically before a new
identity can mutate or be acknowledged. If neither result capacity nor safe
anti-reuse evidence/fence can be reserved, admission fails closed.

Every write batch is encoded completely before entering the WAL. The engine
appends a length-bounded CRC32C frame, handles short writes, synchronizes the
WAL, and only then publishes the batch to memtables and advances its visible
LSN. Group commit may share one sync across complete independently framed
batches, but it cannot merge their atomic boundaries. A restart may discard
only an incomplete suffix of the final active WAL segment. A checksum failure,
malformed complete frame, sequence gap, conflicting duplicate, or corruption
anywhere before that incomplete suffix is fail-stop and must never be treated
as a missing key.

SST and manifest publication follows write-temp, file-sync, rename,
directory-sync, manifest-publish, then visibility. Obsolete SSTs and WAL
segments are deleted only after the manifest that supersedes them is durable
and every reader, backup, reshard, and restore pin has released them. Recovery
loads the one checksummed manifest selected by the durable current pointer,
validates referenced immutable files, and replays only WAL batches beyond its
flushed LSN. All engine file access is rooted beneath one owner-only shard data
directory; one process holds its exclusive ownership lock.

The future storage-record contract is checksummed records with recovery
validation before serving. A successful sync is trusted only when the local
engine reports completion on qualified stable storage. A reported sync
failure, short write that cannot be completed, ENOSPC, I/O error, or detected
recovery checksum/boundary corruption is terminal for that engine instance:
the node must not acknowledge or serve data whose durability is unproven. The
design does not claim to detect a storage device that lies about successful
fsync; no such local detection mechanism exists, so protection comes from
independent RF3 replicas, scrubbing, and failover to an intact quorum. This is
a design requirement only; no storage engine, file format, or fault-handling
path is implemented in Milestone 1.

The clock contract is explicit: `Clock.Now` readings used for elapsed-time
logic are monotonic within a process, while persisted HLC/deadline values are
explicit wall/HLC scalars with Go's process-local monotonic component stripped
before serialization. Equality and ordering of persisted values use their
encoded scalar fields, never hidden Go monotonic metadata. Manual clocks obey
the same nondecreasing/equality contract deterministically; Real clocks retain
the platform wall-clock failure behavior and require the replicated time
authority for deadline decisions. The Go module pins synchronous timer-channel
semantics with `godebug asynctimerchan=0`, so Real timer channels are
zero-capacity by default; an operator who overrides `GODEBUG` accepts the
standard library's alternate channel behavior. Manual timer channels are
one-element buffered so deterministic `Advance` can deliver without a
goroutine. Channel capacity is therefore not a cross-clock contract; callers
must use `C`, `Stop`, and `Reset`. Tests can call `Manual.Prune` at teardown to
release intentionally abandoned timers.

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

The freshness fence also covers unresolved prepare records. A read at snapshot
T must wait for the replicated decision, or return retryable 503, whenever an
unresolved prepare has an earliest possible commit timestamp at or before T.
It may skip that prepare only with durable evidence that its commit (if any)
is strictly after T. A follower cannot satisfy the fence from a local
apply-time guess; it must have the matching range generation, applied index,
and prepare-resolution evidence after recovery or leader change.

The cache layers are bounded gateway row caches, replica decoded/index caches,
the engine's immutable metadata/data-block caches, the operating-system page
cache, and local storage. Every strong cache hit carries a matching range
generation plus applied-index or HLC freshness fence; CDC invalidation is only
an optimization and never the correctness proof. Transaction-private staged
data never enters a shared cache tier. Engine cache keys include immutable file
identity and block offset, and a block is checksummed before admission. Cache
miss or eviction may change latency only, never visibility. QUERY responses
remain `no-store`.

CDC closed frontiers are durable replicated state, not process-local counters.
A restart or leader change must recover the last durably validated per-range
frontier before serving it, and an unresolved prepare/commit position keeps the
frontier below that position until its decision and participant fragments are
durably resolved. This prevents a late event at or below an advertised frontier
from appearing after recovery.

Global secondary indexes are independently sharded, replicated ranges and
synchronous participants in row writes/2PC. Every write validates and reserves
global uniqueness there; index generations move through fenced `BACKFILL`,
`CATCHUP`, and `READY` states, and QUERY may use only a `READY` generation.
There are no foreign keys in this contract. These indexes are later-milestone
mechanisms, not implemented in Milestone 1.

Primary keys map to stable 128-bit hash tokens for shard/range routing only.
Within a range, the primary key remains the ordered canonical tuple; codec
ordering and cursor logical bounds therefore remain meaningful and are not
replaced by hash order. Every range descriptor carries an ID, logical bounds,
a monotonically increasing generation, placements, and serving state. Gateways
watch the replicated metadata catalog. Requests carry
the generation and retry stale `RANGE_MOVED` responses internally; clients
never receive an HTTP redirect.

Online split, merge, and relocation copy from a follower snapshot, replay
post-barrier mutations, verify checksums/indexes/CDC position, establish a
target quorum, and then perform one fenced metadata cutover. Before that
cutover the old owner is authoritative; after it only the new owner is. A
short cutover queue/retry and bounded stale-route forwarding provide no
client-visible downtime. Operation IDs and replicated deduplication prevent a
successful PUT from being applied twice during rerouting. Full-result GC must
also preserve the durable post-GC anti-reuse evidence or generation fence, so
a delayed retry after restart cannot be mistaken for a new operation and
overwrite a later write.

Backups choose one global HLC barrier and publish a manifest only after every
shard, transaction decision, catalog record, and CDC position is durable. PITR
restores into a new isolated cluster and replays only committed logical
transactions. The journal cannot be deleted before the maximum of CDC, PITR,
and active-backup retention dependencies.

The Go operator later owns CRDs and reconciliation for placement,
backups/restores, resharding, and autoscaling, but it never guesses data-plane
authority; status must reflect fenced catalog/Raft evidence. None of these
mechanisms is implemented in Milestone 1.

Transaction deadlines use a replicated/quorum-derived monotonic HLC authority,
not an individual node's wall clock. A deadline makes a transaction eligible
for an expiry timer or sweeper to attempt the replicated `EXPIRED` decision;
it is not a local automatic rollback. The quorum time service returns a
replicated uncertainty interval `[earliest, latest]` guaranteed to contain
cluster time under the declared maximum skew bound. `COMMITTED` may be chosen
only when `latest < deadline`; `EXPIRED` may be chosen only when
`earliest >= deadline`. If the interval straddles the deadline, or the bound
is unavailable during a clock jump, leadership change, or failover, the state
remains in its current non-terminal `OPEN` or `PREPARING` state and the request
returns retryable 503 until the authority recovers. In particular, uncertainty
never moves `PREPARING` back to `OPEN`. The chosen interval evidence, deadline
comparison, and terminal state are one consensus transition, retained for
recovery and history checking; no replica uses local apply-time wall clock. A
client `ROLLED_BACK` decision also requires `latest < deadline`;
`earliest >= deadline` chooses `EXPIRED`, while a straddling or unavailable
interval leaves the current non-terminal `OPEN` or `PREPARING` state with
retryable 503. These mechanisms are later milestones and are not implemented
here.

## Not implemented in Milestone 1

There is no custom storage engine, Raft group, HTTP server, metadata catalog,
follower-read path, split/merge controller, autoscaler, CDC journal, backup,
or restore path yet. The `cmd` binaries intentionally stop before claiming any
of these behaviors.
