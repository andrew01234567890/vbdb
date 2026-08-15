# ADR 0001: Public HTTP and consistency contracts

- Status: accepted for v1
- Scope: public contract only; implementation begins in later milestones

## Decision

VBDB exposes JSON documents under `/` with server-owned `_key` and `_version`
metadata. `GET /{table}/{key}` returns a strong read and a quoted ETag.
`PUT /{table}/{key}` is a full replacement/upsert: 201 for create and 200 for
replace. `If-Match: "<uuidv7>"` is an optional exact version precondition;
`If-None-Match: *` is create-only. Failed autocommit conditions return 412.

Search is exclusively `QUERY /{table}` with media type
`application/vnd.vbdb.query+json`; there is no POST or GET search fallback.
Successful queries return 200, require a usable READY index, use opaque
logical cursors, and return 422 when no index can satisfy the request.
Responses carry row versions and `Cache-Control: no-store`. Errors use
`application/problem+json` with stable error codes.

Transactions use `POST /transactions/create`, `POST /transactions/commit`,
`POST /transactions/rollback`, and `GET /transactions/{id}`. A transaction is
selected by `X-Transaction-Id`; its staged data is private. Transactional PUT
returns 202 with a candidate version. It uses read-committed statements and
commit-time OCC: an ETag or unique-index conflict returns 409 and atomically
aborts the transaction. An open transaction holds no locks. The configurable
deadline defaults to 30 seconds and is capped at five minutes; it is fixed in
the replicated transaction record and cannot be extended by a heartbeat.
When it expires, the transaction automatically rolls back to `EXPIRED`.
Commit or rollback after expiry returns 410. A durable `COMMITTED` decision
wins only when recorded before the deadline, even if its HTTP response arrives
later; otherwise expiry wins. Creation returns 201, a committed or rolled-back
operation is 200, and an unknown transaction is 404. Repeating the same
terminal operation is idempotent; a conflicting terminal operation returns
409 and leaves the recorded terminal state unchanged.

CDC uses `GET /_cdc` or `GET /_cdc/{table}` with `Accept: text/event-stream`.
The stream returns 200 and events are committed logical transactions, globally
ordered, at-least-once, and resumed with `Last-Event-ID`. Consumers deduplicate
stable event IDs. `cdc.retention` defaults to 24 hours and a cursor older than
retained history returns 410 with the earliest available position.

`_admin`, `_cdc`, and `transactions` are reserved names. Authentication,
authorization, TLS, schema administration, and actual route handlers are later
milestones; this ADR does not imply they work in Milestone 1.

## Consequences

The API can remain stateless across gateways and resharding because cursors
contain logical positions rather than physical shard IDs. The no-fallback
QUERY rule makes missing indexes visible instead of hiding an accidental full
table scan.
