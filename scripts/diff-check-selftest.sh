#!/usr/bin/env bash
# Validate committed-range diff checking in an isolated temporary repository.
set -euo pipefail
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE \
	GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_SHALLOW_FILE \
	GIT_REPLACE_REF_BASE GIT_GRAFT_FILE GIT_NAMESPACE GIT_CONFIG \
	GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export LC_ALL=C
export GIT_PAGER=cat
export PAGER=cat

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
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

configure_git_identity() {
	local git_dir=$1
	git -C "$git_dir" config user.name diff-check
	git -C "$git_dir" config user.email diff-check@example.invalid
}

expect_merge_conflict() {
	local git_dir=$1
	local branch=$2
	local description=$3
	local merge_head
	local merge_status
	set +e
	git -C "$git_dir" merge --no-commit "$branch" >/dev/null 2>&1
	merge_status=$?
	set -e
	if ! merge_head=$(git -C "$git_dir" rev-parse -q --verify MERGE_HEAD 2>/dev/null); then
		merge_head=
	fi
	if [ "$merge_status" -eq 0 ] || [ -z "$merge_head" ] ||
		[ -z "$(git -C "$git_dir" ls-files --unmerged)" ]; then
		printf 'diff-check-selftest: %s did not produce an actual merge conflict (status=%s)\n' \
			"$description" "$merge_status" >&2
		exit 1
	fi
}

mkdir -p "$repo/scripts"
cp "$root/scripts/diff-check-ci.sh" "$repo/scripts/diff-check-ci.sh"
chmod 0755 "$repo/scripts/diff-check-ci.sh"
git -C "$repo" init -q
configure_git_identity "$repo"
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

# A merge-only attribute must be checked against every parent. The default
# combined diff omits a path changed only against the second parent; in these
# fixtures the merge resolves to that parent's trailing-whitespace line, so
# combined whitespace inspection alone cannot enforce the attribute policy.
run_merge_attribute_case() {
	local case_name=$1 attribute_rule=$2
	local attribute_repo="$temp_root/$case_name"
	local attribute_initial attribute_left attribute_right attribute_tree attribute_blob attribute_head
	local attribute_ref attribute_output attribute_status
	mkdir -p "$attribute_repo/scripts"
	cp "$root/scripts/diff-check-ci.sh" "$attribute_repo/scripts/diff-check-ci.sh"
	git -C "$attribute_repo" init -q
	configure_git_identity "$attribute_repo"
	printf '%s\n' base >"$attribute_repo/data.txt"
	git -C "$attribute_repo" add data.txt scripts/diff-check-ci.sh
	git -C "$attribute_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
	attribute_initial=$(git -C "$attribute_repo" rev-parse HEAD)
	git -C "$attribute_repo" checkout -q -b left
	printf '%s\n' left >"$attribute_repo/data.txt"
	git -C "$attribute_repo" add data.txt
	git -C "$attribute_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm left
	attribute_left=$(git -C "$attribute_repo" rev-parse HEAD)
	git -C "$attribute_repo" checkout -q -b right "$attribute_initial"
	printf '%s\n' "$attribute_rule" >"$attribute_repo/.gitattributes"
	printf '%s   \n' right >"$attribute_repo/data.txt"
	git -C "$attribute_repo" add .gitattributes scripts/diff-check-ci.sh
	attribute_blob=$(git -C "$attribute_repo" hash-object -w --no-filters data.txt)
	git -C "$attribute_repo" update-index --add --cacheinfo "100644,$attribute_blob,data.txt"
	attribute_tree=$(git -C "$attribute_repo" write-tree)
	attribute_right=$(printf '%s\n' right | git -C "$attribute_repo" commit-tree "$attribute_tree" -p "$attribute_initial")
	attribute_ref=$(git -C "$attribute_repo" symbolic-ref HEAD)
	git -C "$attribute_repo" update-ref "$attribute_ref" "$attribute_right" "$attribute_initial"
	git -C "$attribute_repo" checkout -q left
	set +e
	git -C "$attribute_repo" merge --no-commit right >/dev/null 2>&1
	attribute_status=$?
	set -e
	if [ "$attribute_status" -eq 0 ] || ! git -C "$attribute_repo" rev-parse -q --verify MERGE_HEAD >/dev/null 2>&1; then
		printf 'diff-check-selftest: %s did not produce the expected attribute merge conflict\n' "$case_name" >&2
		exit 1
	fi
	printf '%s   \n' right >"$attribute_repo/data.txt"
	git -C "$attribute_repo" add .gitattributes
	attribute_blob=$(git -C "$attribute_repo" hash-object -w --no-filters data.txt)
	git -C "$attribute_repo" update-index --add --cacheinfo "100644,$attribute_blob,data.txt"
	git -C "$attribute_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm 'merge attribute resolution'
	attribute_head=$(git -C "$attribute_repo" rev-parse HEAD)
	set +e
	attribute_output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$attribute_left" VBDB_HEAD_SHA="$attribute_head" "$attribute_repo/scripts/diff-check-ci.sh" 2>&1)
	attribute_status=$?
	set -e
	if [ "$attribute_status" -ne 2 ] || [[ "$attribute_output" != *'non-default content attribute'* ]]; then
		printf 'diff-check-selftest: %s merge-only attribute status=%s output=%s\n' \
			"$case_name" "$attribute_status" "$attribute_output" >&2
		exit 1
	fi
}

run_merge_attribute_case merge-only-diff '*.txt -diff'
run_merge_attribute_case merge-only-filter '*.txt filter=sentinel'

# A merge range whose base is the first parent must ignore whitespace that was
# already present in that parent, while rejecting whitespace introduced by the
# merge resolution itself.
merge_repo="$temp_root/merge"
mkdir -p "$merge_repo/scripts"
cp "$root/scripts/diff-check-ci.sh" "$merge_repo/scripts/diff-check-ci.sh"
git -C "$merge_repo" init -q
configure_git_identity "$merge_repo"
printf '%s\n' base > "$merge_repo/merge.txt"
git -C "$merge_repo" add merge.txt scripts/diff-check-ci.sh
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
merge_initial=$(git -C "$merge_repo" rev-parse HEAD)
git -C "$merge_repo" checkout -q -b left
printf '%s   \n' inherited > "$merge_repo/merge.txt"
git -C "$merge_repo" add merge.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm left
merge_left=$(git -C "$merge_repo" rev-parse HEAD)
git -C "$merge_repo" checkout -q -b right "$merge_initial"
printf '%s\n' other > "$merge_repo/merge.txt"
git -C "$merge_repo" add merge.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm right
git -C "$merge_repo" checkout -q left
expect_merge_conflict "$merge_repo" right 'inherited-whitespace merge'
printf '%s   \n' inherited > "$merge_repo/merge.txt"
git -C "$merge_repo" add merge.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm 'merge inherited whitespace'
merge_head=$(git -C "$merge_repo" rev-parse HEAD)
set +e
output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$merge_left" VBDB_HEAD_SHA="$merge_head" "$merge_repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 0 ] || {
	printf 'diff-check-selftest: inherited merge whitespace status = %s, want 0\n' "$status" >&2
	exit 1
}

git -C "$merge_repo" checkout -q -b left-resolution "$merge_initial"
mkdir -p "$merge_repo/nested"
printf '%s\n' left > "$merge_repo/nested/merge.txt"
git -C "$merge_repo" add nested/merge.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm left-resolution
merge_left_resolution=$(git -C "$merge_repo" rev-parse HEAD)
git -C "$merge_repo" checkout -q -b right-resolution "$merge_initial"
mkdir -p "$merge_repo/nested"
printf '%s\n' right > "$merge_repo/nested/merge.txt"
git -C "$merge_repo" add nested/merge.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm right-resolution
git -C "$merge_repo" checkout -q left-resolution
expect_merge_conflict "$merge_repo" right-resolution 'resolution merge'
printf '%s \t\n' resolved > "$merge_repo/nested/merge.txt"
git -C "$merge_repo" add nested/merge.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm 'merge resolution space tab'
merge_resolution_head=$(git -C "$merge_repo" rev-parse HEAD)
set +e
output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$merge_left_resolution" VBDB_HEAD_SHA="$merge_resolution_head" "$merge_repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: resolution merge whitespace status = %s, want 1\n' "$status" >&2
	exit 1
}

run_crlf_merge_case() {
	local case_name=$1 resolution=$2 expected_status=$3
	local crlf_repo="$temp_root/$case_name"
	local crlf_initial crlf_left crlf_head crlf_output crlf_status
	mkdir -p "$crlf_repo/scripts"
	cp "$root/scripts/diff-check-ci.sh" "$crlf_repo/scripts/diff-check-ci.sh"
	git -C "$crlf_repo" init -q
	configure_git_identity "$crlf_repo"
	printf 'base\n' >"$crlf_repo/merge.txt"
	git -C "$crlf_repo" add merge.txt scripts/diff-check-ci.sh
	git -C "$crlf_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
	crlf_initial=$(git -C "$crlf_repo" rev-parse HEAD)
	git -C "$crlf_repo" checkout -q -b left
	printf 'left\n' >"$crlf_repo/merge.txt"
	git -C "$crlf_repo" add merge.txt
	git -C "$crlf_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm left
	crlf_left=$(git -C "$crlf_repo" rev-parse HEAD)
	git -C "$crlf_repo" checkout -q -b right "$crlf_initial"
	printf 'right\n' >"$crlf_repo/merge.txt"
	git -C "$crlf_repo" add merge.txt
	git -C "$crlf_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm right
	git -C "$crlf_repo" checkout -q left
	expect_merge_conflict "$crlf_repo" right "$case_name"
	printf '%b' "$resolution" >"$crlf_repo/merge.txt"
	git -C "$crlf_repo" add merge.txt
	git -C "$crlf_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm 'CRLF merge resolution'
	crlf_head=$(git -C "$crlf_repo" rev-parse HEAD)
	set +e
	crlf_output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$crlf_left" VBDB_HEAD_SHA="$crlf_head" "$crlf_repo/scripts/diff-check-ci.sh" 2>&1)
	crlf_status=$?
	set -e
	if [ "$crlf_status" -ne "$expected_status" ]; then
		printf 'diff-check-selftest: %s CRLF status=%s, want %s output=%s\n' \
			"$case_name" "$crlf_status" "$expected_status" "$crlf_output" >&2
		exit 1
	fi
}

run_crlf_merge_case merge-crlf-legal 'resolved\r\n' 0
run_crlf_merge_case merge-crlf-trailing 'resolved   \r\n' 1
run_crlf_merge_case merge-crlf-blank-at-eof 'resolved\r\n\r\n' 1

git -C "$merge_repo" checkout -q -b left-blank "$merge_initial"
mkdir -p "$merge_repo/nested"
printf '%s\n' left > "$merge_repo/nested/blank.txt"
printf '%s\n' left > "$merge_repo/nested/middle.txt"
git -C "$merge_repo" add nested/blank.txt nested/middle.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm left-blank
merge_left_blank=$(git -C "$merge_repo" rev-parse HEAD)
git -C "$merge_repo" checkout -q -b right-blank "$merge_initial"
mkdir -p "$merge_repo/nested"
printf '%s\n' right > "$merge_repo/nested/blank.txt"
printf '%s\n' right > "$merge_repo/nested/middle.txt"
git -C "$merge_repo" add nested/blank.txt nested/middle.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm right-blank
git -C "$merge_repo" checkout -q left-blank
expect_merge_conflict "$merge_repo" right-blank 'blank-line merge'
printf '%s\n\n' resolved > "$merge_repo/nested/blank.txt"
printf '%s\n\n%s\n' first second > "$merge_repo/nested/middle.txt"
git -C "$merge_repo" add nested/blank.txt nested/middle.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm 'merge blank at eof'
merge_blank_head=$(git -C "$merge_repo" rev-parse HEAD)
set +e
output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$merge_left_blank" VBDB_HEAD_SHA="$merge_blank_head" "$merge_repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: blank-at-EOF merge whitespace status = %s, want 1\n' "$status" >&2
	exit 1
}

# A blank added as the final changed line of a mid-file hunk is legal when the
# following unchanged context line remains. The checker must not confuse it
# with a blank at EOF (covered by the preceding case).
git -C "$merge_repo" checkout -q -b middle-base "$merge_initial"
mkdir -p "$merge_repo/nested"
printf '%s\n%s\n' base tail > "$merge_repo/nested/middle-only.txt"
git -C "$merge_repo" add nested/middle-only.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm middle-base
middle_base=$(git -C "$merge_repo" rev-parse HEAD)
git -C "$merge_repo" checkout -q -b left-middle "$middle_base"
printf '%s\n%s\n' left tail > "$merge_repo/nested/middle-only.txt"
git -C "$merge_repo" add nested/middle-only.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm left-middle
merge_left_middle=$(git -C "$merge_repo" rev-parse HEAD)
git -C "$merge_repo" checkout -q -b right-middle "$middle_base"
mkdir -p "$merge_repo/nested"
printf '%s\n%s\n' other tail > "$merge_repo/nested/middle-only.txt"
git -C "$merge_repo" add nested/middle-only.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm right-middle
git -C "$merge_repo" checkout -q left-middle
expect_merge_conflict "$merge_repo" right-middle 'middle-only merge'
printf '%s\n\n%s\n' first tail > "$merge_repo/nested/middle-only.txt"
git -C "$merge_repo" add nested/middle-only.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm 'merge middle blank'
merge_middle_head=$(git -C "$merge_repo" rev-parse HEAD)
set +e
output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$merge_left_middle" VBDB_HEAD_SHA="$merge_middle_head" "$merge_repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 0 ] || {
	printf 'diff-check-selftest: blank-middle merge status = %s, want 0\n' "$status" >&2
	exit 1
}

# A non-fast-forward push is checked from its merge base when available. If
# the predecessor is absent locally, the checker must use the safe full-head
# fallback and never invoke transport or repository-configured fetch helpers.
fallback_source="$temp_root/fallback-source"
fallback_remote="$temp_root/fallback-remote.git"
fallback_clone="$temp_root/fallback-clone"
mkdir -p "$fallback_source/scripts"
cp "$root/scripts/diff-check-ci.sh" "$fallback_source/scripts/diff-check-ci.sh"
git -C "$fallback_source" init -q
configure_git_identity "$fallback_source"
printf '%s\n' base > "$fallback_source/data.txt"
git -C "$fallback_source" add data.txt scripts/diff-check-ci.sh
git -C "$fallback_source" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
fallback_initial=$(git -C "$fallback_source" rev-parse HEAD)
git -C "$fallback_source" checkout -q -b old-base
printf '%s\n' old > "$fallback_source/data.txt"
git -C "$fallback_source" add data.txt
git -C "$fallback_source" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm old-base
fallback_old_base=$(git -C "$fallback_source" rev-parse HEAD)
git -C "$fallback_source" checkout -q -b new-head "$fallback_initial"
printf '%s   \n' 'new head' > "$fallback_source/data.txt"
git -C "$fallback_source" add data.txt
git -C "$fallback_source" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm new-head
fallback_new_head=$(git -C "$fallback_source" rev-parse HEAD)
git -C "$fallback_source" checkout -q --orphan unrelated
git -C "$fallback_source" rm -q -r --cached --ignore-unmatch .
git -C "$fallback_source" clean -q -fd
printf '%s\n' unrelated > "$fallback_source/unrelated.txt"
git -C "$fallback_source" add unrelated.txt
git -C "$fallback_source" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm unrelated
fallback_unrelated=$(git -C "$fallback_source" rev-parse HEAD)
git init --bare -q "$fallback_remote"
git -C "$fallback_source" remote add origin "$fallback_remote"
git -C "$fallback_source" push -q origin old-base new-head unrelated
git clone -q -b new-head "file://$fallback_remote" "$fallback_clone"
git -C "$fallback_clone" update-ref -d refs/remotes/origin/old-base
git -C "$fallback_clone" reflog expire --expire=now --all
git -C "$fallback_clone" gc --prune=now --quiet
if git -C "$fallback_clone" cat-file -e "$fallback_old_base^{commit}" 2>/dev/null; then
	printf '%s\n' 'diff-check-selftest: predecessor object was not made unavailable' >&2
	exit 1
fi
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA="$fallback_old_base" VBDB_HEAD_SHA="$fallback_new_head" "$fallback_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: missing-predecessor full-head status = %s, want 1\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA=not-a-sha VBDB_HEAD_SHA="$fallback_new_head" "$fallback_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 2 ] || {
	printf 'diff-check-selftest: malformed predecessor status = %s, want 2\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA=1111111111111111111111111111111111111111 VBDB_HEAD_SHA="$fallback_new_head" "$fallback_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: no-fetch missing predecessor status = %s, want 1\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA=1111111111111111111111111111111111111111 VBDB_HEAD_SHA="$fallback_new_head" "$fallback_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 2 ] || {
	printf 'diff-check-selftest: missing pull-request base status = %s, want 2\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA=1111111111111111111111111111111111111111 VBDB_HEAD_SHA="$fallback_initial" "$fallback_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 0 ] || {
	printf 'diff-check-selftest: clean full-head fallback status = %s, want 0\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA="$fallback_unrelated" VBDB_HEAD_SHA="$fallback_new_head" "$fallback_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: unrelated push fallback status = %s, want 1\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA="$fallback_old_base" VBDB_HEAD_SHA=0000000000000000000000000000000000000000 "$fallback_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 0 ] || {
	printf 'diff-check-selftest: branch deletion status = %s, want 0\n' "$status" >&2
	exit 1
}

# The checker must ignore hostile global config and reject path attributes that
# attempt to disable whitespace validation.
policy_repo="$temp_root/policy"
mkdir -p "$policy_repo/scripts"
cp "$root/scripts/diff-check-ci.sh" "$policy_repo/scripts/diff-check-ci.sh"
git -C "$policy_repo" init -q
configure_git_identity "$policy_repo"
printf '%s\n' clean > "$policy_repo/data.txt"
git -C "$policy_repo" add data.txt scripts/diff-check-ci.sh
git -C "$policy_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
policy_initial=$(git -C "$policy_repo" rev-parse HEAD)
printf '%s   \n' hostile-config > "$policy_repo/data.txt"
git -C "$policy_repo" add data.txt
git -C "$policy_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm hostile-config
policy_bad_config=$(git -C "$policy_repo" rev-parse HEAD)
hostile_config="$temp_root/hostile.gitconfig"
pager_sentinel="$temp_root/pager-invoked"
pager_helper="$temp_root/pager-helper"
printf '%s\n' '#!/usr/bin/env bash' "printf invoked > '$pager_sentinel'" 'cat' > "$pager_helper"
chmod 0755 "$pager_helper"
printf '%s\n' '[core]' 'whitespace = -trailing-space' "pager = $pager_helper" > "$hostile_config"
set +e
output=$(GIT_CONFIG_GLOBAL="$hostile_config" VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$policy_initial" VBDB_HEAD_SHA="$policy_bad_config" "$policy_repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: hostile core.whitespace status = %s, want 1\n' "$status" >&2
	exit 1
}

# Exercise the hostile pager configuration and an explicit hostile pager
# environment. The checker must force cat and --no-pager before any output.
set +e
output=$(GIT_CONFIG_GLOBAL="$hostile_config" GIT_PAGER="$pager_helper" PAGER="$pager_helper" \
	VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$policy_initial" VBDB_HEAD_SHA="$policy_bad_config" \
	"$policy_repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: hostile pager status = %s, want 1\n' "$status" >&2
	exit 1
}
if [ -e "$pager_sentinel" ]; then
	printf '%s\n' 'diff-check-selftest: configured pager helper was invoked' >&2
	exit 1
fi

# Repository-selection environment must not redirect the checker to an
# unrelated clean directory. The real fixture has a trailing-whitespace
# commit, so success would prove the hostile GIT_DIR/GIT_WORK_TREE won.
hostile_selection="$temp_root/hostile-selection"
mkdir -p "$hostile_selection"
set +e
output=$(GIT_DIR="$hostile_selection" GIT_WORK_TREE="$hostile_selection" \
	VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$policy_initial" VBDB_HEAD_SHA="$policy_bad_config" \
	"$policy_repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -ne 0 ] || {
	printf '%s\n' 'diff-check-selftest: hostile GIT_DIR/GIT_WORK_TREE redirected the check' >&2
	exit 1
}

git -C "$policy_repo" checkout -q "$policy_initial"
printf '%s\n' '*.txt whitespace=-trailing-space' > "$policy_repo/.gitattributes"
git -C "$policy_repo" add .gitattributes
git -C "$policy_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm attributes
policy_attributes=$(git -C "$policy_repo" rev-parse HEAD)
printf '%s   \n' attributed > "$policy_repo/data.txt"
git -C "$policy_repo" add data.txt
git -C "$policy_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm attributes-bad
policy_bad_attributes=$(git -C "$policy_repo" rev-parse HEAD)
set +e
output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$policy_attributes" VBDB_HEAD_SHA="$policy_bad_attributes" "$policy_repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 2 ] || {
	printf 'diff-check-selftest: disabling .gitattributes status = %s, want 2\n' "$status" >&2
	exit 1
}
case "$output" in
	*'non-default content attribute'*) ;;
	*)
		printf '%s\n' 'diff-check-selftest: attribute override was not rejected' >&2
		exit 1
		;;
esac

for attribute_rule in '*.txt -diff' '*.txt binary' '*.txt text' \
	'*.txt whitespace=-trailing-space' '*.txt eol=lf' \
	'*.txt working-tree-encoding=UTF-8' '*.txt crlf' '*.txt ident'; do
	attribute_repo="$temp_root/attribute-${attribute_rule//[^A-Za-z0-9]/-}"
	mkdir -p "$attribute_repo/scripts"
	cp "$root/scripts/diff-check-ci.sh" "$attribute_repo/scripts/diff-check-ci.sh"
	git -C "$attribute_repo" init -q
	configure_git_identity "$attribute_repo"
	printf '%s\n' clean > "$attribute_repo/data.txt"
	git -C "$attribute_repo" add data.txt scripts/diff-check-ci.sh
	git -C "$attribute_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
	printf '%s\n' "$attribute_rule" > "$attribute_repo/.gitattributes"
	git -C "$attribute_repo" add .gitattributes
	git -C "$attribute_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm attribute
	attribute_commit=$(git -C "$attribute_repo" rev-parse HEAD)
	printf '%s   \n' attributed > "$attribute_repo/data.txt"
	git -C "$attribute_repo" add data.txt
	git -C "$attribute_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm bad
	attribute_bad=$(git -C "$attribute_repo" rev-parse HEAD)
	set +e
	output=$(VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$attribute_commit" VBDB_HEAD_SHA="$attribute_bad" "$attribute_repo/scripts/diff-check-ci.sh" 2>&1)
	status=$?
	set -e
	[ "$status" -eq 2 ] || {
		printf 'diff-check-selftest: attribute rule %s status = %s, want 2\n' "$attribute_rule" "$status" >&2
		exit 1
	}
	case "$output" in
		*'non-default content attribute'*) ;;
		*)
			printf 'diff-check-selftest: attribute rule %s was not rejected\n' "$attribute_rule" >&2
			exit 1
			;;
	esac
done

# A configured clean filter is a content-transform helper and must not execute
# before the attribute policy rejects it. Build the changed commit through a
# no-filter index update, then assert the external helper sentinel is absent.
filter_repo="$temp_root/filter"
filter_helper="$temp_root/filter-helper"
filter_sentinel="$temp_root/filter-invoked"
mkdir -p "$filter_repo/scripts"
cp "$root/scripts/diff-check-ci.sh" "$filter_repo/scripts/diff-check-ci.sh"
printf '%s\n' clean > "$filter_repo/data.txt"
printf '%s\n' '*.txt filter=sentinel' > "$filter_repo/.gitattributes"
printf '%s\n' '#!/usr/bin/env bash' \
	"printf invoked > '$filter_sentinel'" 'cat' > "$filter_helper"
chmod 0755 "$filter_helper"
git -C "$filter_repo" init -q
configure_git_identity "$filter_repo"
git -C "$filter_repo" add data.txt .gitattributes scripts/diff-check-ci.sh
git -C "$filter_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
filter_base=$(git -C "$filter_repo" rev-parse HEAD)
git -C "$filter_repo" config filter.sentinel.clean "$filter_helper"
printf '%s   \n' filtered > "$filter_repo/data.txt"
filter_blob=$(git -C "$filter_repo" hash-object -w --no-filters data.txt)
git -C "$filter_repo" update-index --add --cacheinfo "100644,$filter_blob,data.txt"
filter_tree=$(git -C "$filter_repo" write-tree)
filter_commit=$(printf '%s\n' filtered | git -C "$filter_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit-tree "$filter_tree" -p "$filter_base")
filter_ref=$(git -C "$filter_repo" symbolic-ref HEAD)
git -C "$filter_repo" update-ref "$filter_ref" "$filter_commit" "$filter_base"
filter_head=$(git -C "$filter_repo" rev-parse HEAD)
rm -f -- "$filter_sentinel"
set +e
output=$(FILTER_SENTINEL="$filter_sentinel" VBDB_EVENT_NAME=pull_request \
	VBDB_BASE_SHA="$filter_base" VBDB_HEAD_SHA="$filter_head" \
	"$filter_repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 2 ] || {
	printf 'diff-check-selftest: clean-filter attribute status = %s, want 2\n' "$status" >&2
	exit 1
}
case "$output" in
	*'non-default content attribute'*) ;;
	*)
		printf '%s\n' 'diff-check-selftest: clean-filter attribute was not rejected' >&2
		exit 1
		;;
esac
if [ -e "$filter_sentinel" ]; then
	printf '%s\n' 'diff-check-selftest: clean filter helper executed before rejection' >&2
	exit 1
fi
cp "$root/scripts/diff-check-local.sh" "$filter_repo/scripts/diff-check-local.sh"
git -C "$filter_repo" add scripts/diff-check-local.sh
# Leave a real worktree change so the local checker has a changed path to
# inspect; its name-only enumeration must still reject the filter attribute.
printf '%s   \n' local-filter > "$filter_repo/data.txt"
rm -f -- "$filter_sentinel"
set +e
output=$(FILTER_SENTINEL="$filter_sentinel" "$filter_repo/scripts/diff-check-local.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 2 ] || {
	printf 'diff-check-selftest: local clean-filter attribute status = %s, want 2\n' "$status" >&2
	exit 1
}
case "$output" in
	*'non-default content attribute'*) ;;
	*)
		printf '%s\n' 'diff-check-selftest: local clean-filter attribute was not rejected' >&2
		exit 1
		;;
esac
if [ -e "$filter_sentinel" ]; then
	printf '%s\n' 'diff-check-selftest: local clean filter helper executed before rejection' >&2
	exit 1
fi

# A Git enumeration failure must not be hidden by a process substitution or an
# empty while-loop.
wrapper_bin="$temp_root/git-wrapper-bin"
mkdir -p "$wrapper_bin"
real_git=$(command -v git)
printf '%s\n' '#!/usr/bin/env bash' 'for arg in "$@"; do' \
	'  [ "$arg" = diff-tree ] && exit 91' 'done' \
	'exec "$DIFF_CHECK_REAL_GIT" "$@"' > "$wrapper_bin/git"
chmod 0755 "$wrapper_bin/git"
set +e
output=$(PATH="$wrapper_bin:$PATH" DIFF_CHECK_REAL_GIT="$real_git" \
	VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$initial" VBDB_HEAD_SHA="$head" \
	"$repo/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 2 ] || {
	printf 'diff-check-selftest: enumeration failure status = %s, want 2\n' "$status" >&2
	exit 1
}
case "$output" in
	*'unable to enumerate changed paths'*) ;;
	*)
		printf '%s\n' 'diff-check-selftest: enumeration failure was not reported' >&2
		exit 1
		;;
esac

# Local worktree and index checks share the same hostile-config and attribute
# policy; neither may fall back to plain configurable `git diff --check`.
local_repo="$temp_root/local"
mkdir -p "$local_repo/scripts"
cp "$root/scripts/diff-check-local.sh" "$local_repo/scripts/diff-check-local.sh"
git -C "$local_repo" init -q
configure_git_identity "$local_repo"
printf '%s\n' clean > "$local_repo/data.txt"
git -C "$local_repo" add data.txt scripts/diff-check-local.sh
git -C "$local_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
local_initial=$(git -C "$local_repo" rev-parse HEAD)
printf '%s   \n' worktree > "$local_repo/data.txt"
set +e
output=$(GIT_CONFIG_GLOBAL="$hostile_config" "$local_repo/scripts/diff-check-local.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: local worktree whitespace status = %s, want 1\n' "$status" >&2
	exit 1
}
git -C "$local_repo" restore -- data.txt
printf '%s   \n' staged > "$local_repo/data.txt"
git -C "$local_repo" add data.txt
set +e
output=$(GIT_CONFIG_GLOBAL="$hostile_config" "$local_repo/scripts/diff-check-local.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: local index whitespace status = %s, want 1\n' "$status" >&2
	exit 1
}
git -C "$local_repo" reset -q --hard "$local_initial"
printf '%s\n' '*.txt -diff' > "$local_repo/.gitattributes"
git -C "$local_repo" add .gitattributes
git -C "$local_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm disable-diff
printf '%s   \n' disabled > "$local_repo/data.txt"
set +e
output=$("$local_repo/scripts/diff-check-local.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 2 ] || {
	printf 'diff-check-selftest: local non-default attribute status = %s, want 2\n' "$status" >&2
	exit 1
}
case "$output" in
	*'non-default content attribute'*) ;;
	*)
		printf '%s\n' 'diff-check-selftest: local attribute override was not rejected' >&2
		exit 1
		;;
esac

local_config_repo="$temp_root/local-config"
local_config_outside="$temp_root/local-config-outside"
mkdir -p "$local_config_repo/scripts" "$local_config_outside"
cp "$root/scripts/diff-check-local.sh" "$local_config_repo/scripts/diff-check-local.sh"
printf '%s\n' clean > "$local_config_repo/data.txt"
printf '%s\n' outside-clean > "$local_config_outside/data.txt"
git -C "$local_config_repo" init -q
configure_git_identity "$local_config_repo"
git -C "$local_config_repo" add data.txt scripts/diff-check-local.sh
git -C "$local_config_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
# A redirected worktree is clean, while the physical repository root has a
# distinct trailing-whitespace change. A vulnerable checker would pass by
# inspecting the redirected directory; the safe checker must find this path
# (or fail closed) before reporting success.
printf '%s   \n' real-root > "$local_config_repo/data.txt"
git -C "$local_config_repo" config core.worktree "$local_config_outside"
set +e
output=$("$local_config_repo/scripts/diff-check-local.sh" 2>&1)
status=$?
set -e
[ "$status" -ne 0 ] || {
	printf 'diff-check-selftest: local core.worktree redirect concealed real-root whitespace\n' >&2
	exit 1
}
case "$output" in
	*'passed'*)
		printf '%s\n' 'diff-check-selftest: local-config worktree check passed despite real-root whitespace' >&2
		exit 1
		;;
esac

common_temp=$(git -C "$repo" rev-parse --path-format=absolute --git-common-dir)
repo_alias="$temp_root/repo-alias"
ln -s "$repo" "$repo_alias"
common_alias="$temp_root/common-alias"
ln -s "$common_temp" "$common_alias"
for invalid_tmp in relative-tmp "$repo" "$common_temp" "$repo_alias" "$common_alias"; do
	set +e
	output=$(TMPDIR="$invalid_tmp" VBDB_EVENT_NAME=pull_request VBDB_BASE_SHA="$initial" VBDB_HEAD_SHA="$initial" "$repo/scripts/diff-check-ci.sh" 2>&1)
	status=$?
	set -e
	[ "$status" -eq 2 ] || {
		printf 'diff-check-selftest: unsafe temporary directory status = %s, want 2\n' "$status" >&2
		exit 1
	}
done
printf '%s\n' 'diff-check-selftest: passed'
