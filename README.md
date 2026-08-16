# VBDB

VBDB is a Go distributed document database under active construction. This
branch is Milestone 2: it provides a runnable, explicitly development-only
single-node HTTP database with durable JSON documents, UUIDv7 ETags, local
MVCC history, and optimistic concurrency control. It is not a distributed
database: there is no Raft, replication, transaction coordinator, query
engine, indexes, CDC, or multi-process safety yet.

## Requirements

- Go 1.26.4 or newer (the module targets Go 1.26.4).
- No third-party dependency is needed for this milestone.

## Local commands

```sh
make fmt-check
make fmt-check-selftest
make vet
make test
make race
make check
```

`make check` is the local Go quality gate and runs formatting, the formatting
self-test, vet, unit tests, and race tests. GitHub Actions runs the same Go
quality/testing checks only. It intentionally does not run repository
publication-safety or committed-diff scans.

Optional local publication-safety and diff checks are available when preparing
a commit or stack submission:

```sh
make public-check
make public-check-selftest
make diff-check
make diff-check-selftest
make diff-check-ci
```

Version checks work now:

```sh
go run ./cmd/vbdbd --version
go run ./cmd/vbdb-operator --version
```

Run the standalone development gateway with a temporary data directory:

```sh
data_dir=$(mktemp -d)
go run ./cmd/vbdbd --role=gateway --data-dir="$data_dir" --listen=127.0.0.1:8080
```

In another terminal, PUT and GET a document (the key is one URL segment):

```sh
curl -i -X PUT -H 'Content-Type: application/json' \
  --data '{"name":"Ada","active":true}' http://127.0.0.1:8080/users/ada
curl -i http://127.0.0.1:8080/users/ada
```

The response envelope is `{ "_key": "...", "_version": "<UUIDv7>",
"value": <document> }`; the version is also a strong quoted `ETag`. Use
`If-Match: "<etag>"` for replacement OCC or `If-None-Match: *` for
create-only. A `GET` accepts `If-None-Match: "<etag>"` and `*`.

PUT enforces the 1 MiB limit both on the received JSON request and on its
canonical JSON representation after validation; this bounds the durable row
as well as the request body.

`vbdbd --role=metadata` and `--role=storage` fail explicitly as not
implemented. `X-Transaction-Id`, transactions, QUERY, admin, and CDC are
explicit later-milestone boundaries. The data directory is owned by VBDB's
custom engine; do not run two processes against it. Stop the process and
restart it with the same directory to exercise synced close/reopen recovery.

## Packages

- `pkg/codec`: versioned canonical tuple/component encoding. Persistent keys
  do not use protobuf encoding; bytes and strings have an escaped,
  memcomparable representation.
- `pkg/jsondoc`: duplicate-key rejecting JSON validation and deterministic
  canonicalization with lossless `json.Number` handling.
- `pkg/clock`: injectable real and concurrency-safe manual clocks/timers.
- `pkg/failpoint`: explicitly injected, disabled-by-default named failpoints;
  no global production hook exists.
- `pkg/uuidv7`: RFC 9562 UUIDv7 generation/parsing with injectable clock and
  random reader.
- `internal/engine`: VBDB's owned ordered key/value durability engine.
- `internal/storage`: MVCC row versions and current-head semantics over that
  engine.
- `internal/httpapi`: direct-testable GET/PUT HTTP contract for the gateway.

The public and internal architecture contracts are in
[`docs/adr/`](docs/adr/), including the later-milestone boundaries. The
current milestone acceptance checklist is [`docs/milestones/m2.md`](docs/milestones/m2.md);
the Milestone 1 checklist remains as historical context.

`make public-check` is an optional local scan. It scans every commit reachable from local refs (plus a
detached `HEAD`), raw commit and annotated-tag objects/ref names, stage-zero
index blobs, and current tracked/non-ignored intended files for high-risk
artifact names (including kubeconfig, `.kube`/`.docker` trees, `.netrc`,
`.git-credentials`, `.npmrc`, `.pypirc`, `.pgpass`, `.htpasswd`, `.dockercfg`,
`.s3cfg`, `.boto`, `.terraformrc`, OpenVPN profiles, and Terraform state) and
credential/private-key signatures. This policy is deliberately fail-safe:
harmless fixtures under risky names are rejected too, with no unsafe
suppression mechanism; use explicitly safe example names. It reports safe
source identities and categories only; credential-shaped, high-risk, or
unsafe paths are digested and matching contents are never printed. It rejects
shallow history, grafts, gitlinks, and repository-selection overrides. `make
public-check-selftest` exercises binary data, ref/history/index/worktree path
coverage, commit/tag metadata, replacement-object and graft immunity, shallow
history rejection, gitlink rejection, hostile environment redirection, and
fail-closed enumeration. All fixtures are created outside the repository in a
validated temporary directory. It disables Git replace refs while scanning.
Local environment files, keys, credentials, and secret/private directories are
ignored by default; committed configuration must use a safe example file
instead. The scope is repo-wide: every object reachable from any local ref
(including commit/tag metadata and trees), annotated-ref names, stage-zero
index blobs, and current tracked or non-ignored untracked files is scanned.
Reachable tree objects are traversed recursively, including empty nested trees;
tree-object content and accounting are deduplicated, while every path
occurrence and directory prefix is checked. Bounded tree count, depth,
manifest, path, and cumulative-byte limits fail closed.
Symlinks are scanned as link text and never followed to external targets;
physical parent components are validated inside the repository and any escape
or broken parent fails closed. Repository-local Git worktree, excludes,
attribute, hideRefs, and diff-helper settings cannot redirect the scan.
Credential/private-key matches are redacted; oversized Git blobs/objects fail
closed at the bounded scanner limit rather than being partially inspected.

`make diff-check` is an optional local worktree check. `make diff-check-ci` is
also retained as an optional local check for an explicitly supplied event
base/head range; it is not invoked by GitHub Actions. Non-fast-forward or
unrelated push histories safely fall back to the full new head history, and
branch deletion events are skipped because they introduce no content. The
checker never performs transport: a caller must provide the repository and
event SHAs, and missing or unusable inputs fail closed. `make
diff-check-selftest` covers
introduced-then-removed whitespace, merge resolution checking (including a
legal mid-file blank, CRLF payloads, and a true blank at EOF), force-push fallback, hostile Git
configuration and attributes, pager suppression, and divergent pull-request
bases. Before committing or submitting a stack, inspect the exact staged diff
(`git diff --cached --stat` and `git diff --cached`) and run any appropriate
optional local scans; never publish secrets or private files.

Manual timer channels are one-slot buffered so deterministic `Advance` can
deliver without a goroutine; Real timer channels follow Go's runtime setting
(`asynctimerchan=0` is pinned by the module by default). Channel capacity is
not a shared API guarantee. Tests that intentionally abandon active Manual
timers should call `Manual.Prune` during teardown.
