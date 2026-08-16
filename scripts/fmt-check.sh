#!/usr/bin/env bash
# Verify every Go file without word-splitting paths, a cwd dependency, or an
# unbounded command line.
set -u
export LC_ALL=C

if ! root=$(CDPATH= cd -- "$(dirname -- "$0")/.." 2>/dev/null && pwd -P); then
	printf '%s\n' 'fmt-check: unable to canonicalize repository root' >&2
	exit 2
fi
manifest=$(mktemp "${TMPDIR:-/tmp}/vbdb-fmt-check.XXXXXX") || {
	printf '%s\n' 'fmt-check: unable to create file manifest' >&2
	exit 2
}
cleanup() { rm -f -- "$manifest"; }
trap cleanup EXIT

if ! find "$root" -type f -name '*.go' -not -path "$root/vendor/*" -print0 >"$manifest"; then
	printf '%s\n' 'fmt-check: unable to enumerate Go files' >&2
	exit 2
fi
files=()
while IFS= read -r -d '' file; do
	files+=("$file")
done <"$manifest"
if [ "${#files[@]}" -eq 0 ]; then
	printf '%s\n' 'fmt-check: no Go files found' >&2
	exit 2
fi

output=
batch=()
run_batch() {
	local batch_output batch_status
	[ "${#batch[@]}" -gt 0 ] || return 0
	batch_output=$(gofmt -l "${batch[@]}" 2>&1)
	batch_status=$?
	if [ "$batch_status" -ne 0 ]; then
		printf '%s\n' "$batch_output" >&2
		exit "$batch_status"
	fi
	if [ -n "$batch_output" ]; then
		if [ -n "$output" ]; then
			output+=$'\n'
		fi
		output+="$batch_output"
	fi
}
for file in "${files[@]}"; do
	batch+=("$file")
	if [ "${#batch[@]}" -ge 128 ]; then
		run_batch
		batch=()
	fi
done
run_batch
if [ -n "$output" ]; then
	printf '%s\n' 'fmt-check: files need gofmt:' >&2
	printf '%s\n' "$output" >&2
	exit 1
fi
