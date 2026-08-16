#!/usr/bin/env bash
# Check whitespace errors in every committed patch introduced by this CI run.
# The caller must provide an exact, validated commit range; no shell fragment
# is ever evaluated from event metadata.
set -u

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
base=${VBDB_BASE_SHA-}
head=${VBDB_HEAD_SHA-}
event=${VBDB_EVENT_NAME-}

die() {
	printf 'diff-check-ci: %s\n' "$1" >&2
	exit 2
}

is_sha() {
	case "$1" in
		''|*[!0123456789abcdefABCDEF]*) return 1 ;;
	esac
	case "${#1}" in
		40|64) return 0 ;;
		*) return 1 ;;
	esac
}

is_zero_sha() {
	case "$1" in
		0000000000000000000000000000000000000000|0000000000000000000000000000000000000000000000000000000000000000)
			return 0
			;;
		*) return 1 ;;
	esac
}

if ! git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	die 'not inside a Git worktree'
fi
if [ "$event" = 'pull_request' ]; then
	is_sha "$base" || die 'pull-request base SHA is missing or malformed'
elif [ "$event" = 'push' ]; then
	[ -n "$base" ] || die 'push predecessor SHA is missing'
	if ! is_zero_sha "$base"; then
		is_sha "$base" || die 'push predecessor SHA is malformed'
	fi
else
	die 'unsupported or missing CI event'
fi
is_sha "$head" || die 'head SHA is missing or malformed'

if ! git -C "$root" rev-parse --verify --quiet "$head^{commit}" >/dev/null; then
	die 'head SHA is not available locally'
fi
if [ "$event" = 'pull_request' ] || ! is_zero_sha "$base"; then
	if ! git -C "$root" rev-parse --verify --quiet "$base^{commit}" >/dev/null; then
		die 'base SHA is not available locally'
	fi
	if [ "$event" = 'pull_request' ]; then
		merge_base=$(git -C "$root" merge-base "$base" "$head" 2>/dev/null) || die 'pull-request base and head have no common ancestor'
		range="$merge_base..$head"
	else
		if ! git -C "$root" merge-base --is-ancestor "$base" "$head"; then
			die 'push predecessor SHA is not an ancestor of head SHA'
		fi
		range="$base..$head"
	fi
else
	range="$head"
fi

commits=$(git -C "$root" rev-list --reverse "$range" 2>/dev/null) || die 'unable to enumerate committed range'
if [ -z "$commits" ]; then
	die 'committed range contains no commits'
fi
while IFS= read -r commit; do
	[ -n "$commit" ] || continue
	if ! git -C "$root" diff-tree --check --root --no-commit-id -r -m "$commit"; then
		exit 1
	fi
done <<<"$commits"
printf '%s\n' 'diff-check-ci: passed'
