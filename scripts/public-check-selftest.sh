#!/usr/bin/env bash
# Exercise public-check with an untracked temporary fixture. The fixture is
# removed on every exit path and its synthetic marker is never printed.
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture=$(mktemp -d "$root/.public-check-selftest.XXXXXX")
broken_index=$(mktemp "$root/.public-check-broken-index.XXXXXX")
isolated=$(mktemp -d "$root/.public-check-isolated.XXXXXX")
cleanup() {
	rm -rf -- "$fixture"
	rm -f -- "$broken_index"
	rm -rf -- "$isolated"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Build the marker from fragments so this self-test does not itself contain a
# credential-looking signature that the publication gate could report.
marker_prefix='AK'
marker_suffix='IA0000000000000000'
printf '%s%s\n' "$marker_prefix" "$marker_suffix" > "$fixture/fixture.txt"

set +e
output=$("$root/scripts/public-check.sh" 2>&1)
check_status=$?
set -e

if [ "$check_status" -eq 0 ]; then
	printf '%s\n' 'public-check-selftest: detector unexpectedly passed' >&2
	exit 1
fi
case "$output" in
	*fixture.txt*) ;;
	*)
		printf '%s\n' 'public-check-selftest: detector did not report the fixture path' >&2
		exit 1
		;;
esac
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: detector leaked fixture contents' >&2
		exit 1
		;;
esac

printf '%s\n' 'not-a-git-index' > "$broken_index"
set +e
output=$(GIT_INDEX_FILE="$broken_index" "$root/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -eq 0 ]; then
	printf '%s\n' 'public-check-selftest: broken Git index unexpectedly passed' >&2
	exit 1
fi
case "$output" in
	*'unable to enumerate Git files'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: enumeration failure was not reported' >&2
		exit 1
		;;
esac
case "$output" in
	*passed*)
		printf '%s\n' 'public-check-selftest: enumeration failure reported success' >&2
		exit 1
		;;
esac

mkdir -p "$isolated/scripts"
cp "$root/scripts/public-check.sh" "$isolated/scripts/public-check.sh"
git -C "$isolated" init -q
printf '\n# %s%s\n' "$marker_prefix" "$marker_suffix" >> "$isolated/scripts/public-check.sh"
set +e
output=$("$isolated/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -eq 0 ]; then
	printf '%s\n' 'public-check-selftest: scanner source fixture unexpectedly passed' >&2
	exit 1
fi
case "$output" in
	*'scripts/public-check.sh'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: scanner source was not scanned' >&2
		exit 1
		;;
esac
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: scanner source check leaked fixture contents' >&2
		exit 1
		;;
esac
printf '%s\n' 'public-check-selftest: passed'
