#!/usr/bin/env bash
# Exercise public-check with an untracked temporary fixture. The fixture is
# removed on every exit path and its synthetic marker is never printed.
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture=$(mktemp -d "$root/.public-check-selftest.XXXXXX")
broken_index=$(mktemp "$root/.public-check-broken-index.XXXXXX")
isolated=$(mktemp -d "$root/.public-check-isolated.XXXXXX")
empty_repo=$(mktemp -d "$root/.public-check-empty.XXXXXX")
history_repo=$(mktemp -d "$root/.public-check-history.XXXXXX")
staged_repo=$(mktemp -d "$root/.public-check-staged.XXXXXX")
cleanup() {
	rm -rf -- "$fixture"
	rm -f -- "$broken_index"
	rm -rf -- "$isolated"
	rm -rf -- "$empty_repo"
	rm -rf -- "$history_repo"
	rm -rf -- "$staged_repo"
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
printf 'prefix\0%s%s\0suffix\n' "$marker_prefix" "$marker_suffix" > "$fixture/fixture.bin"

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
	*fixture.bin*) ;;
	*)
		printf '%s\n' 'public-check-selftest: binary fixture was not scanned' >&2
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
	*'unable to enumerate'*) ;;
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

mkdir -p "$history_repo/scripts"
cp "$root/scripts/public-check.sh" "$history_repo/scripts/public-check.sh"
printf '%s%s\n' "$marker_prefix" "$marker_suffix" > "$history_repo/history.txt"
mkdir -p "$history_repo/secrets"
printf '%s\n' 'safe historical filename fixture' > "$history_repo/secrets/old.txt"
git -C "$history_repo" init -q
git -C "$history_repo" add scripts/public-check.sh history.txt secrets/old.txt
git -C "$history_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm initial
printf '%s\n' 'safe worktree and index' > "$history_repo/history.txt"
git -C "$history_repo" add history.txt
git -C "$history_repo" rm -q secrets/old.txt
git -C "$history_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm clean
set +e
output=$("$history_repo/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -eq 0 ]; then
	printf '%s\n' 'public-check-selftest: committed secret was not rejected' >&2
	exit 1
fi
case "$output" in
	*'HISTORY:'*'history.txt'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: reachable history blob was not scanned' >&2
		exit 1
		;;
esac
case "$output" in
	*'HISTORY:'*'secrets/old.txt'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: deleted history private path was not rejected' >&2
		exit 1
		;;
esac
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: committed fixture contents leaked' >&2
		exit 1
		;;
esac

mkdir -p "$staged_repo/scripts"
cp "$root/scripts/public-check.sh" "$staged_repo/scripts/public-check.sh"
printf '%s%s\n' "$marker_prefix" "$marker_suffix" > "$staged_repo/staged.txt"
git -C "$staged_repo" init -q
git -C "$staged_repo" add scripts/public-check.sh staged.txt
rm -f -- "$staged_repo/staged.txt"
set +e
output=$("$staged_repo/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -eq 0 ]; then
	printf '%s\n' 'public-check-selftest: staged secret was not rejected' >&2
	exit 1
fi
case "$output" in
	*'INDEX:staged.txt'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: staged blob was not scanned' >&2
		exit 1
		;;
esac
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: staged fixture contents leaked' >&2
		exit 1
		;;
esac

mkdir -p "$isolated/scripts"
cp "$root/scripts/public-check.sh" "$isolated/scripts/public-check.sh"
mkdir -p "$isolated/deploy/.kube"
printf '%s\n' 'safe fixture' > "$isolated/deploy/.kube/config"
printf '%s\n' 'safe fixture' > "$isolated/id_rsa"
git -C "$isolated" init -q
git -C "$isolated" add scripts/public-check.sh deploy/.kube/config id_rsa
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
	*'deploy/.kube/config'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: nested kube path was not rejected' >&2
		exit 1
		;;
esac
case "$output" in
	*'id_rsa'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: high-risk key name was not rejected' >&2
		exit 1
		;;
esac
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: scanner source check leaked fixture contents' >&2
		exit 1
		;;
esac

mkdir -p "$empty_repo/scripts"
cp "$root/scripts/public-check.sh" "$empty_repo/scripts/public-check.sh"
git -C "$empty_repo" init -q
printf '%s\n' 'scripts/public-check.sh' > "$empty_repo/.git/info/exclude"
set +e
output=$("$empty_repo/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -eq 0 ]; then
	printf '%s\n' 'public-check-selftest: empty enumeration unexpectedly passed' >&2
	exit 1
fi
case "$output" in
	*'no Git files to scan'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: empty enumeration was not rejected' >&2
		exit 1
		;;
esac
case "$output" in
	*passed*)
		printf '%s\n' 'public-check-selftest: empty enumeration reported success' >&2
		exit 1
		;;
esac
printf '%s\n' 'public-check-selftest: passed'
