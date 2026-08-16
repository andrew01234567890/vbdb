#!/usr/bin/env bash
# Format every Go file without word-splitting paths.
set -u

manifest=$(mktemp "${TMPDIR:-/tmp}/vbdb-fmt.XXXXXX") || {
	printf '%s\n' 'fmt: unable to create file manifest' >&2
	exit 2
}
cleanup() { rm -f -- "$manifest"; }
trap cleanup EXIT
if ! find . -type f -name '*.go' -not -path './vendor/*' -print0 >"$manifest"; then
	printf '%s\n' 'fmt: unable to enumerate Go files' >&2
	exit 2
fi
files=()
while IFS= read -r -d '' file; do
	files+=("$file")
done <"$manifest"
[ "${#files[@]}" -eq 0 ] || gofmt -w "${files[@]}"
