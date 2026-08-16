# VBDB

VBDB is a Go distributed document database under active construction. This
branch is Milestone 1: it establishes the executable, encoding, deterministic
test, and architecture contracts. It does not serve HTTP or persist rows yet.
The binaries fail explicitly when a not-yet-implemented role is requested;
they do not claim to be a usable database.

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

`make public-check` scans every commit reachable from local refs (plus a
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

`make diff-check` checks the local worktree. CI also runs
`make diff-check-ci` over the exact event base/head range, checking every
introduced commit (including an initial push). Non-fast-forward or unrelated
push histories safely fall back to the full new head history, and branch
deletion events are skipped because they introduce no content. The checker
never performs transport: Actions uses `fetch-depth: 0`,
pull-request base/head absence fails closed, and a missing push predecessor
uses only the full-new-head fallback. `make diff-check-selftest` covers
introduced-then-removed whitespace, merge resolution checking (including a
legal mid-file blank, CRLF payloads, and a true blank at EOF), force-push fallback, hostile Git
configuration and attributes, pager suppression, and divergent pull-request
bases.

Manual timer channels are one-slot buffered so deterministic `Advance` can
deliver without a goroutine; Real timer channels follow Go's runtime setting
(`asynctimerchan=0` is pinned by the module by default). Channel capacity is
not a shared API guarantee. Tests that intentionally abandon active Manual
timers should call `Manual.Prune` during teardown.
