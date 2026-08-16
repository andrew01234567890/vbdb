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

# A merge range whose base is the first parent must ignore whitespace that was
# already present in that parent, while rejecting whitespace introduced by the
# merge resolution itself.
merge_repo="$temp_root/merge"
mkdir -p "$merge_repo/scripts"
cp "$root/scripts/diff-check-ci.sh" "$merge_repo/scripts/diff-check-ci.sh"
git -C "$merge_repo" init -q
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
set +e
git -C "$merge_repo" merge --no-commit right >/dev/null 2>&1
merge_status=$?
set -e
[ "$merge_status" -ne 0 ] || {
	printf '%s\n' 'diff-check-selftest: expected merge conflict was absent' >&2
	exit 1
}
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
set +e
git -C "$merge_repo" merge --no-commit right-resolution >/dev/null 2>&1
merge_status=$?
set -e
[ "$merge_status" -ne 0 ] || {
	printf '%s\n' 'diff-check-selftest: expected resolution merge conflict was absent' >&2
	exit 1
}
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
set +e
git -C "$merge_repo" merge --no-commit right-blank >/dev/null 2>&1
merge_status=$?
set -e
[ "$merge_status" -ne 0 ] || {
	printf '%s\n' 'diff-check-selftest: expected blank-line merge conflict was absent' >&2
	exit 1
}
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

git -C "$merge_repo" checkout -q -b left-middle "$merge_initial"
mkdir -p "$merge_repo/nested"
printf '%s\n' left > "$merge_repo/nested/middle-only.txt"
git -C "$merge_repo" add nested/middle-only.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm left-middle
merge_left_middle=$(git -C "$merge_repo" rev-parse HEAD)
git -C "$merge_repo" checkout -q -b right-middle "$merge_initial"
mkdir -p "$merge_repo/nested"
printf '%s\n' right > "$merge_repo/nested/middle-only.txt"
git -C "$merge_repo" add nested/middle-only.txt
git -C "$merge_repo" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm right-middle
git -C "$merge_repo" checkout -q left-middle
set +e
git -C "$merge_repo" merge --no-commit right-middle >/dev/null 2>&1
merge_status=$?
set -e
[ "$merge_status" -ne 0 ] || {
	printf '%s\n' 'diff-check-selftest: expected middle-only merge conflict was absent' >&2
	exit 1
}
printf '%s\n\n%s\n' first second > "$merge_repo/nested/middle-only.txt"
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

# A non-fast-forward push is checked from its merge base. The predecessor is
# deliberately absent locally and must be fetched by its exact object ID.
fetch_source="$temp_root/fetch-source"
fetch_remote="$temp_root/fetch-remote.git"
fetch_clone="$temp_root/fetch-clone"
mkdir -p "$fetch_source/scripts"
cp "$root/scripts/diff-check-ci.sh" "$fetch_source/scripts/diff-check-ci.sh"
git -C "$fetch_source" init -q
printf '%s\n' base > "$fetch_source/data.txt"
git -C "$fetch_source" add data.txt scripts/diff-check-ci.sh
git -C "$fetch_source" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm initial
fetch_initial=$(git -C "$fetch_source" rev-parse HEAD)
git -C "$fetch_source" checkout -q -b old-base
printf '%s\n' old > "$fetch_source/data.txt"
git -C "$fetch_source" add data.txt
git -C "$fetch_source" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm old-base
fetch_old_base=$(git -C "$fetch_source" rev-parse HEAD)
git -C "$fetch_source" checkout -q -b new-head "$fetch_initial"
printf '%s   \n' 'new head' > "$fetch_source/data.txt"
git -C "$fetch_source" add data.txt
git -C "$fetch_source" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm new-head
fetch_new_head=$(git -C "$fetch_source" rev-parse HEAD)
git -C "$fetch_source" checkout -q --orphan unrelated
git -C "$fetch_source" rm -q -r --cached --ignore-unmatch .
git -C "$fetch_source" clean -q -fd
printf '%s\n' unrelated > "$fetch_source/unrelated.txt"
git -C "$fetch_source" add unrelated.txt
git -C "$fetch_source" -c user.name=diff-check -c user.email=diff-check@example.invalid commit -qm unrelated
fetch_unrelated=$(git -C "$fetch_source" rev-parse HEAD)
git init --bare -q "$fetch_remote"
git -C "$fetch_source" remote add origin "$fetch_remote"
git -C "$fetch_source" push -q origin old-base new-head unrelated
git clone -q -b new-head "file://$fetch_remote" "$fetch_clone"
git -C "$fetch_clone" update-ref -d refs/remotes/origin/old-base
git -C "$fetch_clone" reflog expire --expire=now --all
git -C "$fetch_clone" gc --prune=now --quiet
if git -C "$fetch_clone" cat-file -e "$fetch_old_base^{commit}" 2>/dev/null; then
	printf '%s\n' 'diff-check-selftest: predecessor object was not made unavailable' >&2
	exit 1
fi
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA="$fetch_old_base" VBDB_HEAD_SHA="$fetch_new_head" VBDB_ALLOW_DIFF_FETCH=1 VBDB_FETCH_REMOTE=origin "$fetch_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: fetched non-FF bad commit status = %s, want 1\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA=not-a-sha VBDB_HEAD_SHA="$fetch_new_head" "$fetch_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 2 ] || {
	printf 'diff-check-selftest: malformed predecessor status = %s, want 2\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA=1111111111111111111111111111111111111111 VBDB_HEAD_SHA="$fetch_new_head" "$fetch_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: no-fetch missing predecessor status = %s, want 1\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA=1111111111111111111111111111111111111111 VBDB_HEAD_SHA="$fetch_initial" "$fetch_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 0 ] || {
	printf 'diff-check-selftest: clean full-head fallback status = %s, want 0\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA="$fetch_unrelated" VBDB_HEAD_SHA="$fetch_new_head" VBDB_ALLOW_DIFF_FETCH=1 VBDB_FETCH_REMOTE=origin "$fetch_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 1 ] || {
	printf 'diff-check-selftest: unrelated push fallback status = %s, want 1\n' "$status" >&2
	exit 1
}
set +e
output=$(VBDB_EVENT_NAME=push VBDB_BASE_SHA="$fetch_old_base" VBDB_HEAD_SHA=0000000000000000000000000000000000000000 "$fetch_clone/scripts/diff-check-ci.sh" 2>&1)
status=$?
set -e
[ "$status" -eq 0 ] || {
	printf 'diff-check-selftest: branch deletion status = %s, want 0\n' "$status" >&2
	exit 1
}
common_temp=$(git -C "$repo" rev-parse --path-format=absolute --git-common-dir)
for invalid_tmp in relative-tmp "$repo" "$common_temp"; do
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
