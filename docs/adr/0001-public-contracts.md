# ADR 0001: Public HTTP and consistency contracts

- Status: accepted for v1
- Scope: public contract only; implementation begins in later milestones

## Decision

VBDB exposes JSON documents under `/` with server-owned `_key` and `_version`
metadata. `GET /{table}/{key}` returns a strong read and a quoted ETag.
`PUT /{table}/{key}` is a full replacement/upsert: 201 for create and 200 for
replace. `If-Match: "<uuidv7>"` is an optional exact version precondition;
`If-None-Match: *` is create-only. Failed autocommit conditions return 412.
Without either precondition, replacement is intentionally blind
last-writer-wins within the range (the later-milestone M2 policy). Conditional
ETag and create-only preconditions are checked on every write. Global-unique
constraints are schema invariants checked on every write, including an
unconditional blind PUT; an autocommit uniqueness violation is 409, while 412
is reserved for a failed HTTP precondition. Blind last-writer-wins never
bypasses uniqueness.

Search is exclusively `QUERY /{table}` with media type
`application/vnd.vbdb.query+json`; there is no POST or GET search fallback.
VBDB follows [RFC 10008](https://www.rfc-editor.org/rfc/rfc10008.html): QUERY
is safe and idempotent, requires this request `Content-Type`, and advertises
support with the Structured Field list member `Accept-Query: "application/vnd.vbdb.query+json"`
response header. Successful queries return 200, use one global post-invocation
HLC statement snapshot across all participating ranges and cursor pages, require
a usable READY index, and return 422 when no index can satisfy the request.
Missing `Content-Type` is a 4xx (VBDB uses 400); a specified but unsupported
request media type returns 415 with `Accept-Query`; malformed media types or
inconsistent query syntax/body return 400; an understood, syntactically valid
but unprocessable query, including one with no READY index, returns 422. An
unsupported requested response media type returns 406.
Opaque cursors bind the immutable snapshot timestamp and generation set.
VBDB retains `Cache-Control: no-store` as policy even though RFC 10008 permits
QUERY caching. Responses carry row versions. Errors use
`application/problem+json` with stable error codes.

Transactions use `POST /transactions/create`, `POST /transactions/commit`,
`POST /transactions/rollback`, and `GET /transactions/status`. A transaction is
selected by `X-Transaction-Id`, an opaque bearer capability containing at least
128 bits from a CSPRNG (the row-version UUIDv7 type is not reused for this
purpose pending authentication). Its staged data is visible only to GET and
QUERY requests carrying that same transaction ID. Those reads overlay the
transaction's latest staged PUTs on committed read-committed state and return
their candidate versions; requests without that ID and other transactions see
only committed data. Transactional PUT returns 202 with a candidate version.

Every successful staged mutation has one authoritative replicated transaction
record before the gateway returns 202. The record contains a separate
non-secret logical transaction ID (the bearer is only a lookup capability), a
per-transaction request sequence and/or stable
`Idempotency-Key`, target row key, immutable committed baseline (absence or
the exact committed row version and value digest), candidate row value and
candidate `_version`, and the operation digest. The candidate version is
proposed once by the proposer (UUIDv7), stored in that record, and carried
unchanged by every replica and apply/replay path; replicas never mint a new
version while applying it. A 202 acknowledges this complete staged state, not
gateway memory. Failover recovery replays the record and restores the same
baseline, candidate, version, request sequence, and idempotency identity
before serving the transaction; a missing or checksum-invalid record is
fail-closed and cannot be reconstructed from a partial candidate.
Autocommit writes use the same proposer-once `_version` rule in their durable
logical transaction record; retries and replica apply never mint a replacement
version.

Creation and every idempotent replay response, including a creation request
that has no `X-Transaction-Id`, carries `Cache-Control: no-store, private` and
`Vary: X-Transaction-Id`. A bearer capability is never cacheable or written to
an access log, application log, trace, metric, or error.

`Idempotency-Key` is the public stable retry identity for every mutating
request, including transaction creation, autocommit PUTs, staged PUTs, and
terminal transaction operations. It is opaque and scoped by authenticated
principal, route, and
target. The replicated dedup record retains the request digest and original
HTTP result for the configured dedup horizon (at least the transaction
deadline plus the gateway-retry window). A retry with the same identity and
digest replays that result after gateway restart; reuse with a different
digest returns 409. Replay consults the durable dedup/operation record before
applying anything, so it cannot clobber a newer write. A request sequence is
still recorded for transaction history and ordering even when an idempotency
identity is supplied.

Full-result GC must not make an identity fresh. Before deleting a complete
dedup result, the replicated state must durably retain bounded anti-reuse
evidence (for example, a retired identity and request digest) or install a
durable generation/expiry fence that verifiably excludes delayed retries. A
retired, expired, or indeterminate identity is rejected without mutation; it
must never fall through to blind PUT. If anti-reuse admission is exhausted, or
the evidence/fence cannot be durably established, GC or new admission fails
closed without acknowledging a mutation. Recovery must restore the evidence
or fence before accepting writes. Identity reuse is permitted only after a
durable fence proves that the old identity is safely outside the retry
ambiguity window.

Every request carrying `X-Transaction-Id` requires the response to include
`Cache-Control: no-store, private` and `Vary: X-Transaction-Id`, including
transactional PUT (202), GET/QUERY overlays, and transaction status and
terminal responses. Gateways and services must never log the bearer value or
place it in CDC positions, CDC event payloads, metrics labels, traces, or
error text. This also applies to autocommit requests: an autocommit mutation
has a separate logical transaction/event ID in its replicated record and CDC
event, and has no bearer capability at all.

The staged row/byte and transaction counts are bounded: at most 10,000 staged
rows and 64 MiB of staged canonical row bytes per open transaction. Each
transaction also reserves at most 4 MiB of encoded CDC metadata (logical event
ID, participant set, and operation metadata), so its complete logical CDC event
has a hard 68 MiB encoded limit. Admission
is atomic across the authenticated principal and tenant (at most 32 open
transactions each) and the cluster (at most 100,000 non-terminal `OPEN` or
`PREPARING` transactions): the replicated admission record reserves capacity
before creation or staging, so concurrent gateways cannot oversubscribe either
scope. A projected encoded event above 68 MiB, or a missing reservation for its
complete event bytes, is rejected atomically with 413/429 before staged 202.
Commit performs the same reservation with the journal and delivery queues; if
the complete event cannot fit, commit remains non-terminal and returns
retryable 429 rather than publishing a partial event. Cursors are bounded per
principal/tenant (64 open cursors) and cluster
(10,000), with a 16 KiB opaque state token, a 64 MiB frozen snapshot-plus-
overlay, at most 1,000 rows per page, and a 4 MiB response. The cursor's
snapshot is retained by reference, not copied without accounting. Exceeding a
bound returns 413 or 429 without changing durable state.

The MVCC contract also bounds retained state: each range admits at most 8 GiB
or 100 million retained versions for active snapshots and enforces a 24-hour
minimum horizon. Before GC could cross an active cursor's horizon, admission
backpressures new snapshots/writes with retryable 429/503 and does not delete
history; if accounting or GC coordination is uncertain it fails closed. A
cursor whose snapshot is beyond the durably advertised horizon returns 410.
CDC bounds each complete encoded logical event to 68 MiB, each stream queue to
256 MiB and 10,000 events, and the cluster to 1,000 streams. Queue admission
accounts encoded bytes, not just event count, and cannot split one logical
event into independently acknowledged events. Capacity pressure returns 429
with a retryable error rather than dropping or reordering events. Operators
configure `cdc.retention.window` (default 24 hours; values below 24 hours are
invalid) and `cdc.retention.bytes` (default 4 GiB per range, a soft byte
budget). The byte budget never evicts an event still inside the configured
window: if no event is older than that window, journal/queue admission applies
backpressure or fails closed with retryable 429/503. Once an event is older
than the configured window and its frontier is durably closed, the byte budget
may evict old events; the range advances its earliest-available frontier and a
cursor/`Last-Event-ID` before that frontier returns 410. Operators may raise
limits but may not reduce the retention window or remove these bounds.

GET evaluates the overlay at request invocation. A cursor-based transactional
QUERY freezes the staged overlay/write-set snapshot together with its first
page's immutable statement snapshot; later staged PUTs appear only in a new
uncursored QUERY, never partway through an existing cursor.
It uses read-committed statements and commit-time OCC: an ETag or unique-index
conflict returns 409 and atomically aborts the transaction. An open transaction
holds no locks. A transaction keeps one write intent per row: its first
committed-state If-Match/create-only precondition is the immutable OCC baseline,
while later staged PUTs replace only the candidate value/version. For example,
If-Match(v0) -> candidate v1 -> If-Match(v1) -> candidate v2 commits only v2
after validating the original v0 baseline; create-only followed by a
replacement remains create-only against the original absence. A later
transaction-visible condition may be checked against the transaction-private
candidate while staging (a miss is 412), but cannot erase or weaken the
committed-state baseline. A first blind PUT establishes a blind committed-state
baseline; a later If-Match(candidate) can validate the private candidate for
staging but does not convert that original baseline into a conditional write.
Read-committed statements prohibit G0 dirty writes, G1 dirty reads, intermediate reads, and
circular information-flow observations: a statement observes only committed
state and never a partial transaction. Statement-to-statement nonrepeatable
reads and phantoms are allowed, as are write skew and other anomalies not
excluded by read committed. Blind last-writer-wins intentionally permits a
concurrent lost update; an `If-Match` precondition is checked again at the
serialized commit transition to request conflict detection when the caller
chooses it.
Autocommit preconditions and uniqueness validate in one serialized write
transition; transactional committed-state OCC and uniqueness validate only in
the replicated prepare/COMMIT transition. The configurable deadline defaults
to 30 seconds and is capped at five minutes; it is fixed in
the replicated transaction record and cannot be extended by a heartbeat. A
deadline makes the transaction eligible for a replicated `EXPIRED` decision;
an expiry timer or sweeper only attempts that consensus transition. If the
time authority is uncertain or unavailable, the transaction remains `OPEN` or
`PREPARING` and requests return retryable 503. Once the authority proves
expiry, the applied `EXPIRED` decision releases resources and is authoritative.
The replicated recorded state is authoritative: 410 is returned only when that
state is `EXPIRED`. A durable `COMMITTED` or `ROLLED_BACK` state recorded before
the deadline wins over a later wall-clock expiry; retrying the same terminal
operation then returns 200. A durable `COMMITTED` decision can win only when
recorded before the deadline; otherwise the replicated expiry state wins.
Terminal precedence is derived only from the replicated recorded state: once
`COMMITTED` or `ROLLED_BACK` is recorded, retrying that same terminal operation
returns 200 regardless of later local wall-clock time; only a recorded
`EXPIRED` state returns 410. A different terminal operation remains 409.
Each terminal decision entry carries a timestamp from a replicated/quorum-
derived monotonic HLC authority. The quorum time service supplies a replicated
uncertainty interval `[earliest, latest]` guaranteed to contain cluster time
under the declared maximum skew bound. `COMMITTED` may be chosen only when
`latest < deadline`; `EXPIRED` may be chosen only when `earliest >= deadline`.
If the interval straddles the deadline or the bound is unavailable, the
transaction remains in its current non-terminal `OPEN` or `PREPARING` state
and the request returns retryable 503 until the authority recovers. The
interval evidence, deadline comparison, and terminal state are one consensus
transition; no replica uses local apply-time wall clock. Expiry becomes
authoritative only after an `EXPIRED` entry is applied.
A client-requested `ROLLED_BACK` decision uses the same deadline authority:
`latest < deadline` is required, `earliest >= deadline` chooses `EXPIRED`, and
an interval that straddles the deadline or has no bound leaves the transaction
in its current non-terminal `OPEN` or `PREPARING` state with retryable 503. No
uncertainty path may move `PREPARING` back to `OPEN`. The comparison and
terminal state are one consensus transition, so rollback after expiry cannot
report success.
Creation returns 201, a committed or rolled-back operation is 200, and an
unknown transaction is 404. Repeating the same terminal operation is
idempotent; a conflicting terminal operation returns 409 and leaves the
recorded terminal state unchanged. `GET /transactions/status` always reports
the recorded state, including `EXPIRED`; all transaction routes select the
bearer capability only through `X-Transaction-Id`, never through a URL path.

For a cross-shard commit, each participant performs its ETag/create-only/global-
unique checks and installs its prepare reservations/intents atomically in one
serialized replicated state-machine transition. Validation and reservation
cannot be separate operations, so two prepares cannot both validate the same
committed version or unique key. Every touched row is serialized while the
transaction is `PREPARING`: a blind writer encountering an unresolved prepare
waits or retries, then re-prepares and may overwrite after the prior decision
under the blind last-writer-wins policy. Only an ETag/create-only or global-
unique mismatch returns 409 and aborts. Replicated participant prepare intents
are installed as part of that same participant prepare transition. Every
participant must be prepared before one replicated transaction decision becomes the
linearization point. While `OPEN`, staged data remains private and holds no
locks or intents; prepare intents and reservations exist only during commit. The
prepare record contains a replicated `prepare_deadline` no later than the
transaction deadline plus a fixed 30-second resolver grace. A quorum resolver
with a live replicated lease must drive `COMMITTED`, `ROLLED_BACK`, or `EXPIRED`
before that deadline; the lease and deadline are state-machine data, not a
local wall-clock timer. If the resolver loses quorum or the time bound is
uncertain, participants keep the reservation and affected requests fail closed
with retryable 503 until a new resolver can safely decide; they never locally
expire or release a prepare. Thus resolution is bounded whenever the declared
resolver liveness holds, while a partition preserves safety rather than
inventing an unsafe expiry. GET and QUERY that encounter an intent resolve
it against the recorded decision: `COMMITTED` is visible atomically, while abort or `EXPIRED` intents
are ignored and removed. Materialization lag cannot expose a cross-shard
fracture. Recovery and failover replay the decision and intents before serving
the same visibility rule. An unresolved prepare is hidden; if its earliest
possible commit timestamp is at or before the request snapshot T, the range
must wait for the replicated decision or return retryable 503. It may be
skipped only with durable evidence that its commit (if any) is strictly after
T. The same rule applies to the global statement snapshot and every cursor
page: a freshness fence must prove each range has applied through T and has no
unresolved prepare that could commit at or before T.

CDC uses `GET /_cdc` or `GET /_cdc/{table}` with `Accept: text/event-stream`.
The stream returns 200 and emits one event per committed logical transaction,
at-least-once, and resumes with `Last-Event-ID`. A position is the totally
ordered composite `(commit HLC, logical event ID)`. Each range persists
progress and an inclusive closed frontier `W`: every committed event at a
position `<= W` is durably enumerable from that range, and no later event can
appear at or below `W`. The merger chooses the inclusive global frontier
`F = min(W)` across participating ranges and emits only positions `<= F`;
therefore every emitted position is enumerable everywhere needed before it is
released, and a slow range cannot be skipped. The logical event ID (a separate
non-secret random/ordered identifier) is the stable event tiebreak/deduplication
key. It is never the `X-Transaction-Id` bearer capability, and the bearer or
verifier must not enter journal, CDC, metrics, trace, or log fields. Range
journals persist participant fragments keyed by the logical event ID and the
declared participant set; the merger waits until every fragment is enumerable
through the fenced frontier, assembles one complete logical event, and only
then emits it. Consumer deduplication therefore cannot drop mutation
fragments. One logical committed transaction yields one event even when it
spans ranges. Autocommit has exactly the same logical event-ID rule.
Each range's `W` is a durable, replicated monotonic value: after restart or
leader change, a replacement leader may advertise only the last durably
validated frontier or a later one justified by the same journal evidence, never
a locally remembered value. `W` stays strictly below any position whose prepare
or commit decision is unresolved. The range cannot advance `W` through that
position until the decision and every required participant fragment are durably
resolved and enumerable, so a late `<= W` event cannot appear after recovery.
Idle ranges advance their closed frontier with durable heartbeat records, not
with an unproven wall-clock assumption. The merger's range membership is fenced
by a catalog generation: a split child joins only after its creation frontier
is durably established, and a removed range cannot be omitted until its final
frontier is included. This keeps the minimum frontier in one ordering domain.
`cdc.retention.window` defaults to 24 hours and is a hard minimum configurable
window; `cdc.retention.bytes` defaults to 4 GiB per range and is a soft budget.
The journal must retain every event within the configured window even when the
byte budget is full, applying backpressure/fail-closed admission instead of
shortening the window. After the window elapses, the byte budget may evict only
durably closed older events. Each range publishes the resulting earliest
available position, and a cursor or `Last-Event-ID` before it returns 410 with
that frontier. Stream and queue limits are the bounded values above. The
ordering merger, frontiers, and journal are later-milestone mechanisms, not
implemented here.

`_admin`, `_cdc`, and `transactions` are reserved names. Authentication,
authorization, TLS, schema administration, and actual route handlers are later
milestones; this ADR does not imply they work in Milestone 1.

## Consequences

The API can remain stateless across gateways and resharding because cursors
contain logical positions rather than physical shard IDs. The no-fallback
QUERY rule makes missing indexes visible instead of hiding an accidental full
table scan.
