#!/usr/bin/env bash
# Verify every Go file without word-splitting paths or hiding parser errors.
set -u

manifest=$(mktemp "${TMPDIR:-/tmp}/vbdb-fmt-check.XXXXXX") || {
	printf '%s\n' 'fmt-check: unable to create file manifest' >&2
	exit 2
}
cleanup() { rm -f -- "$manifest"; }
trap cleanup EXIT

if ! find . -type f -name '*.go' -not -path './vendor/*' -print0 >"$manifest"; then
	printf '%s\n' 'fmt-check: unable to enumerate Go files' >&2
	exit 2
fi
files=()
while IFS= read -r -d '' file; do
	files+=("$file")
done <"$manifest"
if [ "${#files[@]}" -eq 0 ]; then
	exit 0
fi

output=$(gofmt -l "${files[@]}" 2>&1)
status=$?
if [ "$status" -ne 0 ]; then
	printf '%s\n' "$output" >&2
	exit "$status"
fi
if [ -n "$output" ]; then
	printf '%s\n' 'fmt-check: files need gofmt:' >&2
	printf '%s\n' "$output" >&2
	exit 1
fi
