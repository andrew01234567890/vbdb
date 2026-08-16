#!/usr/bin/env bash
# Check whitespace errors in every committed patch introduced by this CI run.
# The caller must provide an exact, validated commit range; no shell fragment
# is ever evaluated from event metadata.
set -u
export LC_ALL=C
export GIT_PAGER=cat
export PAGER=cat

# A publication gate must not inherit caller Git configuration, repository
# selection, replacement objects, or attribute overrides. Ordinary identity
# variables are intentionally left alone because this script never commits.
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE \
	GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_SHALLOW_FILE \
	GIT_REPLACE_REF_BASE GIT_GRAFT_FILE GIT_NAMESPACE GIT_CONFIG \
	GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GIT_NO_REPLACE_OBJECTS=1

if ! root=$(CDPATH= cd -- "$(dirname -- "$0")/.." 2>/dev/null && pwd -P); then
	printf '%s\n' 'diff-check-ci: unable to canonicalize repository root' >&2
	exit 2
fi
git_safe() {
	git --no-pager --work-tree="$root" -C "$root" \
		-c "core.worktree=$root" \
		-c core.excludesFile=/dev/null \
		-c core.attributesFile=/dev/null \
		-c core.autocrlf=false \
		-c core.eol=lf \
		-c core.fsmonitor=false \
		-c transfer.hideRefs= \
		-c receive.hideRefs= \
		-c uploadpack.hideRefs= \
		"$@"
}
content_attributes=(binary diff text whitespace filter working-tree-encoding eol crlf ident)
base=${VBDB_BASE_SHA-}
head=${VBDB_HEAD_SHA-}
event=${VBDB_EVENT_NAME-}
git_top=$(git_safe rev-parse --show-toplevel 2>/dev/null) || {
	printf '%s\n' 'diff-check-ci: unable to resolve Git worktree root' >&2
	exit 2
}
if ! git_top=$(CDPATH= cd -- "$git_top" 2>/dev/null && pwd -P); then
	printf '%s\n' 'diff-check-ci: unable to canonicalize Git worktree root' >&2
	exit 2
fi
if [ "$git_top" != "$root" ]; then
	printf '%s\n' 'diff-check-ci: Git worktree root differs from script root' >&2
	exit 2
fi
if ! git_safe rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf '%s\n' 'diff-check-ci: not inside a Git worktree' >&2
	exit 2
fi
diff_common_dir=$(git_safe rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || {
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
changed_manifest=$(mktemp "$diff_tmp_root/vbdb-diff-paths.XXXXXX" 2>/dev/null) || {
	rm -f -- "$merge_diff"
	printf '%s\n' 'diff-check-ci: unable to create changed-path manifest' >&2
	exit 2
}
attribute_manifest=$(mktemp "$diff_tmp_root/vbdb-diff-attributes.XXXXXX" 2>/dev/null) || {
	rm -f -- "$merge_diff" "$changed_manifest"
	printf '%s\n' 'diff-check-ci: unable to create attribute manifest' >&2
	exit 2
}
cleanup() { rm -f -- "$merge_diff" "$changed_manifest" "$attribute_manifest"; }
trap cleanup EXIT

die() {
	printf 'diff-check-ci: %s\n' "$1" >&2
	exit 2
}

check_whitespace_attributes() {
	local commit=$1 path attr_path attr_name attr_value attr_count
	# Materialize and check the producer status before iterating. A process
	# substitution would hide an enumeration failure behind a successful empty
	# while-loop and fail open. -m is required here: a merge's default combined
	# name-only diff omits paths changed against only one parent, including a
	# merge-only .gitattributes rule that could otherwise hide a content filter.
	if ! git_safe diff-tree -m --root --no-commit-id --name-only -r -z \
		--diff-filter=ACDMRTUXB "$commit" >"$changed_manifest" 2>/dev/null; then
		die 'unable to enumerate changed paths for whitespace attributes'
	fi
	while IFS= read -r -d '' path; do
		if ! git_safe check-attr --source="$commit" -z \
			"${content_attributes[@]}" -- "$path" >"$attribute_manifest" 2>/dev/null; then
			die 'unable to inspect changed-path attributes'
		fi
		attr_count=0
		while IFS= read -r -d '' attr_path &&
			IFS= read -r -d '' attr_name &&
			IFS= read -r -d '' attr_value; do
			attr_count=$((attr_count + 1))
			[ "$attr_count" -le "${#content_attributes[@]}" ] || die 'malformed attribute enumeration'
			[ "$attr_path" = "$path" ] || die 'malformed attribute enumeration'
			case "$attr_name:$attr_value" in
				binary:unspecified|diff:unspecified|text:unspecified|whitespace:unspecified|filter:unspecified|working-tree-encoding:unspecified|eol:unspecified|crlf:unspecified|ident:unspecified) ;;
				binary:*|diff:*|text:*|whitespace:*|filter:*|working-tree-encoding:*|eol:*|crlf:*|ident:*)
					die 'non-default content attribute on changed path'
					;;
				*) die 'malformed attribute enumeration' ;;
			esac
		done <"$attribute_manifest"
		[ "$attr_count" -eq "${#content_attributes[@]}" ] || die 'malformed attribute enumeration'
	done <"$changed_manifest"
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
	git_safe rev-parse --verify --quiet "$sha^{commit}" >/dev/null 2>&1
}

if ! git_safe rev-parse --is-inside-work-tree >/dev/null 2>&1; then
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
ensure_commit "$head" || die 'head SHA is not available locally'
if [ "$event" = 'pull_request' ] || ! is_zero_sha "$base"; then
	if [ "$event" = 'pull_request' ]; then
		ensure_commit "$base" || die 'pull-request base SHA is not available locally'
		merge_base=$(git_safe merge-base "$base" "$head" 2>/dev/null) || die 'pull-request base and head have no common ancestor'
		range="$merge_base..$head"
	else
		if ! ensure_commit "$base"; then
			printf '%s\n' 'diff-check-ci: push predecessor is unavailable; scanning full head history' >&2
			range="$head"
		elif git_safe merge-base --is-ancestor "$base" "$head"; then
			range="$base..$head"
		elif merge_base=$(git_safe merge-base "$base" "$head" 2>/dev/null); then
			range="$merge_base..$head"
		else
			printf '%s\n' 'diff-check-ci: push histories are unrelated; scanning full head history' >&2
			range="$head"
		fi
	fi
else
	range="$head"
fi

commits=$(git_safe rev-list --reverse "$range" 2>/dev/null) || die 'unable to enumerate committed range'
if [ -z "$commits" ]; then
	die 'committed range contains no commits'
fi
while IFS= read -r commit; do
	[ -n "$commit" ] || continue
	parent_line=$(git_safe rev-list --parents -n 1 "$commit" 2>/dev/null) || die 'unable to inspect commit parents'
	parent_count=$(awk '{print NF - 1}' <<<"$parent_line")
	if [ "$parent_count" -gt 2 ]; then
		die 'octopus merges are unsupported by the combined whitespace checker'
	fi
	check_whitespace_attributes "$commit"
	if [ "$parent_count" -gt 1 ]; then
		# Combined diff checks only the merge resolution, not whitespace merely
		# inherited from one parent. Git's combined --check mode still reports
		# some inherited errors, so the exact all-parent-added-line parser below
		# is the merge check. Per-commit checks catch every ordinary patch.
		if ! git_safe diff-tree --cc --unified=1 -r --no-commit-id --no-color --no-ext-diff --no-textconv "$commit" >"$merge_diff"; then
			die 'unable to inspect combined merge diff'
		fi
		# Git's combined --check mode does not report trailing whitespace on
		# lines marked as a resolution. Inspect only added combined lines;
		# inherited lines are absent from this diff and therefore ignored. One
		# context line is retained so an added blank that ends a mid-file change
		# is not confused with a blank at end-of-file.
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
			# A CRLF payload is still one logical line. Strip exactly one
			# terminal CR before testing trailing spaces or blank-at-EOF;
			# preserve any additional CR as content.
			{ tail=substr(rest, length(rest), 1); if (tail == "\r") rest=substr(rest, 1, length(rest)-1) }
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
		if ! git_safe -c core.whitespace=blank-at-eol,blank-at-eof,space-before-tab diff-tree --check --root --no-commit-id -r --no-ext-diff --no-textconv "$commit"; then
			exit 1
		fi
	fi
done <<<"$commits"
printf '%s\n' 'diff-check-ci: passed'
