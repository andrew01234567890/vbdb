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

It does not run `public-check`, `public-check-selftest`,
`diff-check-selftest`, or `diff-check-ci`. Those repository publication-safety
and committed-diff scans remain available through their scripts and Makefile
targets as optional local tools. `make check` contains only the Go
quality/testing gate, so running it locally has the same scope as the workflow.

## Publication discipline

Optional does not mean that publication safety is waived. Before committing or
submitting a stack, the operator must inspect the exact staged diff and must
never publish credentials, secrets, private files, or unrelated changes. The
local public and diff checks are available to support that review when useful;
they are not claims about what GitHub Actions enforces.

## Consequences

CI remains focused on deterministic Go quality and test feedback without
scanning repository history or event-specific committed patch ranges. Local
operators retain the broader safety tooling for deliberate pre-publication
inspection, and the README and milestone checklist must describe those tools
as optional local checks.
