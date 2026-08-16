#!/usr/bin/env bash
# Check both staged and unstaged worktree patches with repository-independent
# whitespace policy. This is the local counterpart of diff-check-ci.sh.
set -u
export LC_ALL=C
export GIT_PAGER=cat
export PAGER=cat

unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE \
	GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_SHALLOW_FILE \
	GIT_REPLACE_REF_BASE GIT_GRAFT_FILE GIT_NAMESPACE GIT_CONFIG \
	GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GIT_NO_REPLACE_OBJECTS=1

if ! root=$(CDPATH= cd -- "$(dirname -- "$0")/.." 2>/dev/null && pwd -P); then
	printf '%s\n' 'diff-check: unable to canonicalize repository root' >&2
	exit 2
fi

# Repository-local config is untrusted. Force this physical worktree, disable
# external excludes/attributes and filesystem monitors, and ignore hideRefs so
# neither local configuration nor a linked-worktree alias can redirect checks.
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

git_top=$(git_safe rev-parse --show-toplevel 2>/dev/null) || {
	printf '%s\n' 'diff-check: unable to resolve Git worktree root' >&2
	exit 2
}
if ! git_top=$(CDPATH= cd -- "$git_top" 2>/dev/null && pwd -P); then
	printf '%s\n' 'diff-check: unable to canonicalize Git worktree root' >&2
	exit 2
fi
if [ "$git_top" != "$root" ]; then
	printf '%s\n' 'diff-check: Git worktree root differs from script root' >&2
	exit 2
fi
if ! git_safe rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf '%s\n' 'diff-check: not inside a Git worktree' >&2
	exit 2
fi
common_dir=$(git_safe rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || {
	printf '%s\n' 'diff-check: unable to resolve Git common directory' >&2
	exit 2
}
case "$common_dir" in
	/*) ;;
	*) printf '%s\n' 'diff-check: Git common directory was not absolute' >&2; exit 2 ;;
esac
common_dir=$(CDPATH= cd -- "$common_dir" 2>/dev/null && pwd -P) || {
	printf '%s\n' 'diff-check: unable to validate Git common directory' >&2
	exit 2
}
tmp_root=${TMPDIR:-/tmp}
case "$tmp_root" in
	/*) ;;
	*) printf '%s\n' 'diff-check: temporary directory must be absolute' >&2; exit 2 ;;
esac
tmp_root=$(CDPATH= cd -- "$tmp_root" 2>/dev/null && pwd -P) || {
	printf '%s\n' 'diff-check: unable to validate temporary directory' >&2
	exit 2
}
case "$tmp_root" in
	"$root"|"$root"/*|"$common_dir"|"$common_dir"/*)
		printf '%s\n' 'diff-check: temporary directory is inside the repository' >&2
		exit 2
		;;
esac
if [ ! -w "$tmp_root" ] || [ ! -x "$tmp_root" ]; then
	printf '%s\n' 'diff-check: temporary directory is not writable' >&2
	exit 2
fi

worktree_paths=$(mktemp "$tmp_root/vbdb-diff-local-worktree.XXXXXX" 2>/dev/null) || {
	printf '%s\n' 'diff-check: unable to create worktree path manifest' >&2
	exit 2
}
index_paths=$(mktemp "$tmp_root/vbdb-diff-local-index.XXXXXX" 2>/dev/null) || {
	rm -f -- "$worktree_paths"
	printf '%s\n' 'diff-check: unable to create index path manifest' >&2
	exit 2
}
tracked_paths=$(mktemp "$tmp_root/vbdb-diff-local-tracked.XXXXXX" 2>/dev/null) || {
	rm -f -- "$worktree_paths" "$index_paths"
	printf '%s\n' 'diff-check: unable to create tracked path manifest' >&2
	exit 2
}
all_paths=$(mktemp "$tmp_root/vbdb-diff-local-all.XXXXXX" 2>/dev/null) || {
	rm -f -- "$worktree_paths" "$index_paths" "$tracked_paths"
	printf '%s\n' 'diff-check: unable to create complete path manifest' >&2
	exit 2
}
attrs=$(mktemp "$tmp_root/vbdb-diff-local-attrs.XXXXXX" 2>/dev/null) || {
	rm -f -- "$worktree_paths" "$index_paths" "$tracked_paths" "$all_paths"
	printf '%s\n' 'diff-check: unable to create attribute manifest' >&2
	exit 2
}
cleanup() { rm -f -- "$worktree_paths" "$index_paths" "$tracked_paths" "$all_paths" "$attrs"; }
trap cleanup EXIT

die() { printf 'diff-check: %s\n' "$1" >&2; exit 2; }

check_attributes() {
	local source_mode=$1 path attr_path attr_name attr_value attr_count
	local path_manifest=$2
	while IFS= read -r -d '' path; do
		if [ "$source_mode" = cached ]; then
			git_safe check-attr --cached -z "${content_attributes[@]}" -- \
				"$path" >"$attrs" 2>/dev/null || die 'unable to inspect cached attributes'
		else
			git_safe check-attr -z "${content_attributes[@]}" -- \
				"$path" >"$attrs" 2>/dev/null || die 'unable to inspect worktree attributes'
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
				binary:*|diff:*|text:*|whitespace:*|filter:*|working-tree-encoding:*|eol:*|crlf:*|ident:*) die 'non-default content attribute on changed path' ;;
				*) die 'malformed attribute enumeration' ;;
			esac
		done <"$attrs"
		[ "$attr_count" -eq "${#content_attributes[@]}" ] || die 'malformed attribute enumeration'
	done <"$path_manifest"
}

# Check attributes for every tracked/untracked path before any worktree diff
# operation. A changed path with filter.<name>.clean must be rejected before
# Git is allowed to clean-convert it while determining whether it changed.
if ! git_safe ls-files --cached -z >"$tracked_paths" 2>/dev/null; then
	die 'unable to enumerate tracked paths'
fi
cp -- "$tracked_paths" "$all_paths" || die 'unable to materialize complete path manifest'
if ! git_safe ls-files --others --exclude-standard -z >>"$all_paths" 2>/dev/null; then
	die 'unable to enumerate untracked paths'
fi
check_attributes worktree "$all_paths"
check_attributes cached "$tracked_paths"

if ! git_safe diff --no-ext-diff --no-textconv --name-only -z >"$worktree_paths" 2>/dev/null; then
	die 'unable to enumerate worktree paths'
fi
if ! git_safe diff --cached --no-ext-diff --no-textconv --name-only -z >"$index_paths" 2>/dev/null; then
	die 'unable to enumerate index paths'
fi
check_attributes worktree "$worktree_paths"
check_attributes cached "$index_paths"

if ! git_safe -c core.whitespace=blank-at-eol,blank-at-eof,space-before-tab \
	diff --no-ext-diff --no-textconv --check; then
	exit 1
fi
if ! git_safe -c core.whitespace=blank-at-eol,blank-at-eof,space-before-tab \
	diff --cached --no-ext-diff --no-textconv --check; then
	exit 1
fi
printf '%s\n' 'diff-check: passed'
