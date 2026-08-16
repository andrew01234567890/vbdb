# VBDB

VBDB is a Go distributed document database under active construction. This
branch is Milestone 1: it establishes the executable, encoding, deterministic
test, and architecture contracts. It does not serve HTTP or persist rows yet.
The binaries fail explicitly when a not-yet-implemented role is requested;
they do not claim to be a usable database.

## Requirements

- Go 1.26 or newer (the module targets Go 1.26).
- No third-party dependency is needed for this milestone.

## Local commands

```sh
make fmt-check
make vet
make test
make race
make public-check
make public-check-selftest
make diff-check
make diff-check-selftest
make check
```

Version checks work now:

```sh
go run ./cmd/vbdbd --version
go run ./cmd/vbdb-operator --version
```

`vbdbd` accepts the future role contract (`gateway`, `metadata`, or `storage`)
but exits with a clear not-implemented error until the corresponding milestone
lands. `vbdb-operator` is intentionally dependency-free until its operator
milestone.

## Packages

- `pkg/codec`: versioned canonical tuple/component encoding. Persistent keys
  do not use protobuf encoding; bytes and strings have an escaped,
  memcomparable representation.
- `pkg/jsondoc`: duplicate-key rejecting JSON validation and deterministic
  canonicalization with lossless `json.Number` handling.
- `pkg/clock`: injectable real and concurrency-safe manual clocks/timers.
- `pkg/failpoint`: explicitly injected, disabled-by-default named failpoints;
  no global production hook exists.

The public and internal architecture contracts are in
[`docs/adr/`](docs/adr/), including the later-milestone boundaries. The
milestone acceptance checklist is [`docs/milestones/m1.md`](docs/milestones/m1.md).

`make public-check` scans commits and trees reachable from all local refs (plus
a detached `HEAD`), raw commit and annotated-tag objects, stage-zero index
blobs, and current tracked/non-ignored intended files for high-risk artifact
names and credential/private-key signatures. It reports safe source identities
and categories only; credential-shaped or high-risk filenames are redacted and
matching contents are never printed. `make public-check-selftest` exercises
binary data, ref/history/index/worktree path coverage, commit/tag metadata,
replace-object immunity, shallow-history rejection, gitlink rejection, and
fail-closed enumeration. All fixtures are created outside the repository in a
validated temporary directory. It requires a complete non-shallow history and
disables Git replace refs while scanning. Local environment files, keys,
credentials, and secret/private directories are ignored by default; committed
configuration must use a safe example file instead.

`make diff-check` checks the local worktree. CI also runs
`make diff-check-ci` over the exact event base/head range, checking every
introduced commit (including an initial push); `make diff-check-selftest`
covers introduced-then-removed whitespace and divergent pull-request bases.
