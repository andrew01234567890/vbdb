#!/usr/bin/env bash
# Check whitespace errors in every committed patch introduced by this CI run.
# The caller must provide an exact, validated commit range; no shell fragment
# is ever evaluated from event metadata.
set -u

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
base=${VBDB_BASE_SHA-}
head=${VBDB_HEAD_SHA-}
event=${VBDB_EVENT_NAME-}
allow_fetch=${VBDB_ALLOW_DIFF_FETCH-}
fetch_remote=${VBDB_FETCH_REMOTE-}
if ! git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf '%s\n' 'diff-check-ci: not inside a Git worktree' >&2
	exit 2
fi
diff_common_dir=$(git -C "$root" rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || {
	printf '%s\n' 'diff-check-ci: unable to resolve Git common directory' >&2
	exit 2
}
case "$diff_common_dir" in
	/*) ;;
	*)
		printf '%s\n' 'diff-check-ci: Git common directory was not absolute' >&2
		exit 2
		;;
esac
diff_common_dir=$(CDPATH= cd -- "$diff_common_dir" 2>/dev/null && pwd -P) || {
	printf '%s\n' 'diff-check-ci: unable to validate Git common directory' >&2
	exit 2
}
diff_tmp_root=${TMPDIR:-/tmp}
case "$diff_tmp_root" in
	/*) ;;
	*)
		printf '%s\n' 'diff-check-ci: temporary directory must be absolute' >&2
		exit 2
		;;
esac
if ! diff_tmp_root=$(CDPATH= cd -- "$diff_tmp_root" 2>/dev/null && pwd -P); then
	printf '%s\n' 'diff-check-ci: unable to validate temporary directory' >&2
	exit 2
fi
case "$diff_tmp_root" in
	"$root"|"$root"/*|"$diff_common_dir"|"$diff_common_dir"/*)
		printf '%s\n' 'diff-check-ci: temporary directory is inside the repository' >&2
		exit 2
		;;
esac
if [ ! -w "$diff_tmp_root" ] || [ ! -x "$diff_tmp_root" ]; then
	printf '%s\n' 'diff-check-ci: temporary directory is not writable' >&2
	exit 2
fi
merge_diff=$(mktemp "$diff_tmp_root/vbdb-diff-check.XXXXXX" 2>/dev/null) || {
	printf '%s\n' 'diff-check-ci: unable to create temporary diff file' >&2
	exit 2
}
cleanup() { rm -f -- "$merge_diff"; }
trap cleanup EXIT

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

ensure_commit() {
	local sha=$1
	if git -C "$root" rev-parse --verify --quiet "$sha^{commit}" >/dev/null 2>&1; then
		return 0
	fi
	if [ "$allow_fetch" != '1' ]; then
		return 1
	fi
	case "$fetch_remote" in
		''|-*|*[!A-Za-z0-9._/-]*) die 'fetch remote is missing or malformed' ;;
	esac
	if ! git -C "$root" fetch --no-tags --no-recurse-submodules --no-prune "$fetch_remote" "$sha" >/dev/null 2>&1; then
		return 1
	fi
	git -C "$root" rev-parse --verify --quiet "$sha^{commit}" >/dev/null 2>&1
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

if [ "$event" = 'push' ] && is_zero_sha "$head"; then
	printf '%s\n' 'diff-check-ci: branch deletion has no introduced commits; passed'
	exit 0
fi
ensure_commit "$head" || die 'head SHA is not available locally or by exact fetch'
if [ "$event" = 'pull_request' ] || ! is_zero_sha "$base"; then
	if [ "$event" = 'pull_request' ]; then
		ensure_commit "$base" || die 'pull-request base SHA is not available locally or by exact fetch'
		merge_base=$(git -C "$root" merge-base "$base" "$head" 2>/dev/null) || die 'pull-request base and head have no common ancestor'
		range="$merge_base..$head"
	else
		if ! ensure_commit "$base"; then
			printf '%s\n' 'diff-check-ci: push predecessor is unavailable; scanning full head history' >&2
			range="$head"
		elif git -C "$root" merge-base --is-ancestor "$base" "$head"; then
			range="$base..$head"
		elif merge_base=$(git -C "$root" merge-base "$base" "$head" 2>/dev/null); then
			range="$merge_base..$head"
		else
			printf '%s\n' 'diff-check-ci: push histories are unrelated; scanning full head history' >&2
			range="$head"
		fi
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
	parent_line=$(git -C "$root" rev-list --parents -n 1 "$commit" 2>/dev/null) || die 'unable to inspect commit parents'
	parent_count=$(awk '{print NF - 1}' <<<"$parent_line")
	if [ "$parent_count" -gt 2 ]; then
		die 'octopus merges are unsupported by the combined whitespace checker'
	fi
	if [ "$parent_count" -gt 1 ]; then
		# Combined diff checks only the merge resolution, not whitespace merely
		# inherited from one parent. Git's combined --check mode still reports
		# some inherited errors, so the exact all-parent-added-line parser below
		# is the merge check. Per-commit checks catch every ordinary patch.
		if ! git -C "$root" diff-tree --cc --unified=0 -r --no-commit-id --no-color "$commit" >"$merge_diff"; then
			die 'unable to inspect combined merge diff'
		fi
		# Git's combined --check mode does not report trailing whitespace on
		# lines marked as a resolution. Inspect only added combined lines;
		# inherited lines are absent from this diff and therefore ignored.
		plus_prefix=$(printf '%*s' "$parent_count" '' | tr ' ' '+')
		LC_ALL=C awk -v prefix="$plus_prefix" '
			$0 ~ /^diff --cc / {
				if (pending_blank) found=1
				in_hunk=0
				pending_blank=0
				next
			}
			$0 ~ /^@@/ { in_hunk=1; pending_blank=0; next }
			!in_hunk { next }
			{ pending_blank=0 }
			index($0, prefix) != 1 { next }
			{ rest=substr($0, length(prefix)+1) }
			rest ~ /[[:blank:]]$/ || rest ~ /^[[:blank:]]* \t/ { found=1; exit }
			rest == "" { pending_blank=1 }
			END { if (pending_blank) found=1; exit !found }
		' "$merge_diff"
		merge_check_status=$?
		case "$merge_check_status" in
			0)
				printf 'diff-check-ci: merge commit %s introduces whitespace error\n' "$commit" >&2
				exit 1
				;;
			1) ;;
			*) die 'unable to inspect combined merge whitespace' ;;
		esac
	else
		if ! git -C "$root" diff-tree --check --root --no-commit-id -r "$commit"; then
			exit 1
		fi
	fi
done <<<"$commits"
printf '%s\n' 'diff-check-ci: passed'
