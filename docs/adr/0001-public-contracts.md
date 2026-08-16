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
ETag, create-only, and global-unique constraints are checked at commit when
supplied.

Search is exclusively `QUERY /{table}` with media type
`application/vnd.vbdb.query+json`; there is no POST or GET search fallback.
VBDB follows [RFC 10008](https://www.rfc-editor.org/rfc/rfc10008.html): QUERY
is safe and idempotent, requires this request `Content-Type`, and advertises
support with the Structured Field `Accept-Query: "application/vnd.vbdb.query+json"`
response header. Successful queries return 200, use one global post-invocation
HLC statement snapshot across all participating ranges and cursor pages, require
a usable READY index, and return 422 when no index can satisfy the request.
Opaque cursors bind the immutable snapshot timestamp and generation set.
VBDB retains `Cache-Control: no-store` as policy even though RFC 10008 permits
QUERY caching. Responses carry row versions. Errors use
`application/problem+json` with stable error codes.

Transactions use `POST /transactions/create`, `POST /transactions/commit`,
`POST /transactions/rollback`, and `GET /transactions/{id}`. A transaction is
selected by `X-Transaction-Id`; its staged data is visible only to GET and
QUERY requests carrying that same transaction ID. Those reads overlay the
transaction's latest staged PUTs on committed read-committed state and return
their candidate versions; requests without that ID and other transactions see
only committed data. Transactional PUT returns 202 with a candidate version.
It uses read-committed statements and commit-time OCC: an ETag or unique-index
conflict returns 409 and atomically aborts the transaction. An open transaction
holds no locks. The configurable
deadline defaults to 30 seconds and is capped at five minutes; it is fixed in
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
recorded terminal state unchanged. `GET /transactions/{id}` always reports the
recorded state, including `EXPIRED`.

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
locks or intents; prepare intents and short reservations exist only during
commit. GET and QUERY that encounter an intent resolve it against the recorded
decision: `COMMITTED` is visible atomically, while abort or `EXPIRED` intents
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
event tiebreak/deduplication key, so one logical committed transaction yields
one event even when it spans ranges. Consumers deduplicate stable event IDs.
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
