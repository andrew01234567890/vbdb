#!/usr/bin/env bash
# Exercise public-check with an untracked temporary fixture. The fixture is
# removed on every exit path and its synthetic marker is never printed.
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture=$(mktemp -d "$root/.public-check-selftest.XXXXXX")
cleanup() {
	rm -rf -- "$fixture"
}
trap cleanup EXIT HUP INT TERM

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
printf '%s\n' 'public-check-selftest: passed'
