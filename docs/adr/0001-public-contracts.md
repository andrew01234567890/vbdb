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
`OPEN` with retryable 503. The comparison and terminal state are one consensus
transition, so rollback after expiry cannot report success.
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
locks or intents; prepare intents and reservations exist only during commit. If
the time authority is unavailable, fail-closed `PREPARING` reservations may
remain unbounded until a replicated decision is possible; this is an
intentional availability trade. GET and QUERY that encounter an intent resolve
it against the recorded decision: `COMMITTED` is visible atomically, while abort or `EXPIRED` intents
are ignored and removed. Materialization lag cannot expose a cross-shard
fracture. Recovery and failover replay the decision and intents before serving
the same visibility rule; an unresolved prepare remains hidden and retryable.

CDC uses `GET /_cdc` or `GET /_cdc/{table}` with `Accept: text/event-stream`.
The stream returns 200 and emits one event per committed logical transaction,
at-least-once, and resumes with `Last-Event-ID`. A position is the totally
ordered composite `(commit HLC, stable transaction ID)`. Each range persists
progress and an inclusive closed frontier `W`: every committed event at a
position `<= W` is durably enumerable from that range, and no later event can
appear at or below `W`. The merger chooses the inclusive global frontier
`F = min(W)` across participating ranges and emits only positions `<= F`;
therefore every emitted position is enumerable everywhere needed before it is
released, and a slow range cannot be skipped. The transaction ID is the stable
event tiebreak/deduplication key. Range journals persist participant fragments
keyed by that transaction ID and the declared participant set; the merger waits
until every fragment is enumerable through the fenced frontier, assembles one
complete logical event, and only then emits it. Consumer deduplication therefore
cannot drop mutation fragments. One logical committed transaction yields one
event even when it spans ranges.
Idle ranges advance their closed frontier with durable heartbeat records, not
with an unproven wall-clock assumption. The merger's range membership is fenced
by a catalog generation: a split child joins only after its creation frontier
is durably established, and a removed range cannot be omitted until its final
frontier is included. This keeps the minimum frontier in one ordering domain.
`cdc.retention` defaults to 24 hours and a cursor older than retained history
returns 410 with the earliest available position. The ordering merger,
frontiers, and journal are later-milestone mechanisms, not implemented here.

`_admin`, `_cdc`, and `transactions` are reserved names. Authentication,
authorization, TLS, schema administration, and actual route handlers are later
milestones; this ADR does not imply they work in Milestone 1.

## Consequences

The API can remain stateless across gateways and resharding because cursors
contain logical positions rather than physical shard IDs. The no-fallback
QUERY rule makes missing indexes visible instead of hiding an accidental full
table scan.
