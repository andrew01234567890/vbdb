#!/usr/bin/env bash
# Validate committed-range diff checking in an isolated temporary repository.
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp_base=${TMPDIR:-/tmp}
case "$tmp_base" in
	/*) ;;
	*)
		printf '%s\n' 'diff-check-selftest: TMPDIR must be absolute' >&2
		exit 2
		;;
esac
temp_root=$(mktemp -d "$tmp_base/vbdb-diff-check-selftest.XXXXXX")
repo="$temp_root/repo"
cleanup() { rm -rf -- "$temp_root"; }
trap cleanup EXIT
mkdir -p "$repo/scripts"
cp "$root/scripts/diff-check-ci.sh" "$repo/scripts/diff-check-ci.sh"
chmod 0755 "$repo/scripts/diff-check-ci.sh"
git -C "$repo" init -q
printf '%s\n' 'clean' > "$repo/data.txt"
git -C "$repo" add data.txt scripts/diff-check-ci.sh
git -C "$repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
initial=$(git -C "$repo" rev-parse HEAD)
printf '%s   \n' 'introduced whitespace error' > "$repo/data.txt"
git -C "$repo" add data.txt
git -C "$repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm whitespace
bad=$(git -C "$repo" rev-parse HEAD)
printf '%s\n' 'clean again' > "$repo/data.txt"
git -C "$repo" add data.txt
git -C "$repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm removal
head=$(git -C "$repo" rev-parse HEAD)
git -C "$repo" checkout -q -b divergent-base "$initial"
printf '%s\n' 'sibling base' > "$repo/data.txt"
git -C "$repo" add data.txt
git -C "$repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm sibling-base
divergent_base=$(git -C "$repo" rev-parse HEAD)
git -C "$repo" checkout -q -b divergent-head "$initial"
printf '%s\n' 'sibling clean head' > "$repo/data.txt"
git -C "$repo" add data.txt
git -C "$repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm sibling-head
divergent_head=$(git -C "$repo" rev-parse HEAD)

set +e
output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$initial" VBDB_HEAD_SHA="$head" "$repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
if [ "$status" -ne 1 ]; then
	printf 'diff-check-selftest: introduced whitespace status = %s, want 1\n' "$status" >&2
	exit 1
fi

set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA="$initial" VBDB_HEAD_SHA="$head" "$repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
if [ "$status" -ne 1 ]; then
	printf 'diff-check-selftest: push-range status = %s, want 1\n' "$status" >&2
	exit 1
fi

set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA=0000000000000000000000000000000000000000 VBDB_HEAD_SHA="$head" "$repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
if [ "$status" -ne 1 ]; then
	printf 'diff-check-selftest: initial-push status = %s, want 1\n' "$status" >&2
	exit 1
fi

set +e
output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$bad" VBDB_HEAD_SHA="$head" "$repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
if [ "$status" -ne 0 ]; then
	printf 'diff-check-selftest: clean post-removal range status = %s, want 0\n' "$status" >&2
	exit 1
fi

set +e
output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$divergent_base" VBDB_HEAD_SHA="$divergent_head" "$repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
if [ "$status" -ne 0 ]; then
	printf 'diff-check-selftest: divergent-base status = %s, want 0\n' "$status" >&2
	exit 1
fi
printf '%s\n' 'diff-check-selftest: passed'
