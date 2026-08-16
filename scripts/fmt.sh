#!/usr/bin/env bash
# Format every Go file without word-splitting paths.
set -u
export LC_ALL=C

if ! root=$(CDPATH= cd -- "$(dirname -- "$0")/.." 2>/dev/null && pwd -P); then
	printf '%s\n' 'fmt: unable to canonicalize repository root' >&2
	exit 2
fi

manifest=$(mktemp "${TMPDIR:-/tmp}/vbdb-fmt.XXXXXX") || {
	printf '%s\n' 'fmt: unable to create file manifest' >&2
	exit 2
}
cleanup() { rm -f -- "$manifest"; }
trap cleanup EXIT
if ! find "$root" -type f -name '*.go' -not -path "$root/vendor/*" -print0 >"$manifest"; then
	printf '%s\n' 'fmt: unable to enumerate Go files' >&2
	exit 2
fi
files=()
while IFS= read -r -d '' file; do
	files+=("$file")
done <"$manifest"
[ "${#files[@]}" -eq 0 ] || gofmt -w "${files[@]}"
