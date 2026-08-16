#!/usr/bin/env bash
# Exercise fmt-check from outside its repository and prove fail-closed empty
# repository behavior. Fixtures are disposable and live outside the checkout.
set -euo pipefail
export LC_ALL=C

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
tmp_base=${TMPDIR:-/tmp}
case "$tmp_base" in
	/*) ;;
	*)
		printf '%s\n' 'fmt-check-selftest: TMPDIR must be absolute' >&2
		exit 2
		;;
esac
temp_root=$(mktemp -d "$tmp_base/vbdb-fmt-check-selftest.XXXXXX")
cleanup() { rm -rf -- "$temp_root"; }
trap cleanup EXIT

repo="$temp_root/repo with spaces"
mkdir -p "$repo/scripts" "$repo/pkg"
cp "$root/scripts/fmt-check.sh" "$repo/scripts/fmt-check.sh"
printf '%s\n' 'package pkg' 'func Broken( ) {}' > "$repo/pkg/bad.go"
set +e
output=$(cd "$temp_root" && "$repo/scripts/fmt-check.sh" 2>&1)
status=$?
set -e
if [ "$status" -ne 1 ]; then
	printf 'fmt-check-selftest: outside-cwd unformatted status = %s, want 1\n' "$status" >&2
	exit 1
fi
case "$output" in
	*'bad.go'*) ;;
	*)
		printf '%s\n' 'fmt-check-selftest: unformatted file was not reported' >&2
		exit 1
		;;
esac

empty="$temp_root/empty"
mkdir -p "$empty/scripts"
cp "$root/scripts/fmt-check.sh" "$empty/scripts/fmt-check.sh"
set +e
output=$(cd "$temp_root" && "$empty/scripts/fmt-check.sh" 2>&1)
status=$?
set -e
if [ "$status" -ne 2 ]; then
	printf 'fmt-check-selftest: empty repository status = %s, want 2\n' "$status" >&2
	exit 1
fi
case "$output" in
	*'no Go files'*) ;;
	*)
		printf '%s\n' 'fmt-check-selftest: empty repository was not rejected' >&2
		exit 1
		;;
esac

printf '%s\n' 'fmt-check-selftest: passed'
