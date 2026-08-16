# ADR 0004: CI and local publication-safety tooling

- Status: accepted for v1
- Scope: repository quality checks and publication workflow

## Decision

GitHub Actions is intentionally limited to Go quality and testing:

- formatting verification;
- formatting self-test;
- `go vet ./...`;
- unit tests; and
- race tests.

It does not run the repository's publication-safety or committed-diff scans.
The standalone publication-safety scripts remain available to the private
handover workflow, while the committed-diff scripts remain optional local
tools. None of those publication checks are exposed as Makefile targets.
`make check` contains only the Go quality/testing gate, so running it locally
has the same scope as the workflow.

## Publication discipline

Publication safety is governed by the private handover policy. Before
committing or submitting a stack, the operator must inspect the exact staged
diff and must never publish credentials, secrets, private files, or unrelated
changes. The standalone publication-safety scripts support that private review
when useful, while the diff checks remain optional local tools; none are claims
about what GitHub Actions enforces.

## Consequences

CI remains focused on deterministic Go quality and test feedback without
scanning repository history or event-specific committed patch ranges. The
private handover workflow retains the broader safety tooling for deliberate
pre-publication inspection, while the README and milestone checklist describe
the public Makefile and CI surfaces accurately.
