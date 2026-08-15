#!/usr/bin/env bash
# Check every byte source that a Git publication can carry. This intentionally
# prints only paths and finding categories, never matched text.
set -u

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if ! git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf '%s\n' 'public-check: not inside a Git worktree' >&2
	exit 2
fi

history_manifest=
tree_manifest=
index_manifest=
worktree_manifest=
content_file=
failures=0
source_count=0
declare -A scanned_blobs=()

cleanup() {
	[ -z "$history_manifest" ] || rm -f -- "$history_manifest"
	[ -z "$tree_manifest" ] || rm -f -- "$tree_manifest"
	[ -z "$index_manifest" ] || rm -f -- "$index_manifest"
	[ -z "$worktree_manifest" ] || rm -f -- "$worktree_manifest"
	[ -z "$content_file" ] || rm -f -- "$content_file"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

report() {
	printf 'public-check: %s: %s\n' "$1" "$2"
	failures=1
}

make_temp() {
	local path
	path=$(mktemp "${TMPDIR:-/tmp}/vbdb-public-check.XXXXXX") || return 1
	printf '%s\n' "$path"
}

if ! history_manifest=$(make_temp) || ! tree_manifest=$(make_temp) || \
	! index_manifest=$(make_temp) || ! worktree_manifest=$(make_temp) || \
	! content_file=$(make_temp); then
	printf '%s\n' 'public-check: unable to create secure scan files' >&2
	exit 2
fi

# Keep this list narrow: ordinary words such as "token" and public header
# documentation must not be treated as credentials.
credential_pattern='-----BEGIN (RSA|EC|OPENSSH|DSA|PGP)? ?PRIVATE KEY-----|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[0-9A-Za-z-]{10,}|[A-Z][A-Z0-9_]*(SECRET|PASSWORD|PRIVATE_KEY)[A-Z0-9_]*[[:space:]]*=[[:space:]]*[^[:space:]#]{8,}'

# These are names, not content matches. Examples and source files remain
# publishable, while common local credential and private-material paths do not.
is_high_risk_name() {
	local path=$1 base=${1##*/}
	case "$path" in
		secrets/*|private/*|credentials/*|.codex/*|.claude/*|.aws/*|.ssh/*|.kube/*|*/secrets/*|*/private/*|*/credentials/*|*/.codex/*|*/.claude/*|*/.aws/*|*/.ssh/*|*/.kube/*)
			return 0
			;;
	esac
	case "$base" in
		.env|.env.*|*.pem|*.key|*.p12|*.pfx|*.jks|id_rsa|id_rsa.*|id_ed25519|id_ed25519.*|credentials|credentials.*|credentials.json|secret|secret.*)
			[ "$base" != ".env.example" ]
			return
			;;
	esac
	return 1
}

scan_content() {
	local label=$1 file=$2 grep_status
	source_count=$((source_count + 1))
	# -a is deliberate: binary files are scanned as bytes, while -q keeps
	# matched content out of output.
	LC_ALL=C grep -E -a -q -- "$credential_pattern" "$file" 2>/dev/null
	grep_status=$?
	case "$grep_status" in
		0) report "$label" 'credential or private-key signature' ;;
		1) ;;
		*) report "$label" 'unable to scan file' ;;
	esac
}

scan_blob() {
	local label=$1 object_id=$2
	if [[ ${scanned_blobs[$object_id]+seen} ]]; then
		return
	fi
	scanned_blobs[$object_id]=seen
	if ! git -C "$root" cat-file blob "$object_id" >"$content_file" 2>/dev/null; then
		report "$label" 'unable to read Git blob'
		exit 2
	fi
	scan_content "$label" "$content_file"
}

# Every commit reachable from current HEAD is publishable history. An unborn
# repository has no history; untracked/intended files are still checked below.
if git -C "$root" rev-parse --verify --quiet HEAD >/dev/null 2>&1; then
	if ! git -C "$root" rev-list --topo-order HEAD >"$history_manifest" 2>/dev/null; then
		printf '%s\n' 'public-check: unable to enumerate reachable history' >&2
		exit 2
	fi
	while IFS= read -r commit; do
		if ! git -C "$root" ls-tree -r -z --full-tree "$commit" >"$tree_manifest" 2>/dev/null; then
			printf '%s\n' 'public-check: unable to enumerate reachable tree' >&2
			exit 2
		fi
		while IFS= read -r -d '' record; do
			if [[ "$record" != *$'\t'* ]]; then
				report "HISTORY:$commit" 'malformed Git tree entry'
				exit 2
			fi
			meta=${record%%$'\t'*}
			path=${record#*$'\t'}
			read -r _ object_type object_id <<<"$meta"
			if [ "$object_type" != 'blob' ] || [ -z "$object_id" ]; then
				continue
			fi
			if is_high_risk_name "$path"; then
				report "HISTORY:$commit:$path" 'high-risk private artifact filename'
				continue
			fi
			scan_blob "HISTORY:$commit:$path" "$object_id"
		done <"$tree_manifest"
	done <"$history_manifest"
fi

# Stage-zero index bytes are publishable. Any unresolved stage is rejected
# instead of allowing an ambiguous index to pass the safety gate.
if ! git -C "$root" ls-files --stage -z >"$index_manifest" 2>/dev/null; then
	printf '%s\n' 'public-check: unable to enumerate index entries' >&2
	exit 2
fi
while IFS= read -r -d '' record; do
	if [[ "$record" != *$'\t'* ]]; then
		report 'INDEX' 'malformed index entry'
		exit 2
	fi
	meta=${record%%$'\t'*}
	path=${record#*$'\t'}
	read -r _ object_id stage <<<"$meta"
	if [ "$stage" != '0' ]; then
		report "INDEX:$path" 'unresolved index stage'
		continue
	fi
	if [ -z "$object_id" ]; then
		report "INDEX:$path" 'missing index object'
		exit 2
	fi
	if is_high_risk_name "$path"; then
		report "INDEX:$path" 'high-risk private artifact filename'
		continue
	fi
	scan_blob "INDEX:$path" "$object_id"
done <"$index_manifest"

# Current tracked and non-ignored untracked bytes are also checked. Deleted
# tracked paths have no current bytes; their HEAD/index representations above
# remain covered. Symlinks are read as link text rather than followed outside
# the worktree.
if ! git -C "$root" ls-files --cached --others --exclude-standard -z >"$worktree_manifest" 2>/dev/null; then
	printf '%s\n' 'public-check: unable to enumerate Git files' >&2
	exit 2
fi
while IFS= read -r -d '' path; do
	file="$root/$path"
	if is_high_risk_name "$path"; then
		report "WORKTREE:$path" 'high-risk private artifact filename'
		continue
	fi
	if [ ! -e "$file" ] && [ ! -L "$file" ]; then
		continue
	fi
	if [ -L "$file" ]; then
		if ! readlink "$file" >"$content_file" 2>/dev/null; then
			report "WORKTREE:$path" 'unable to read symlink'
			continue
		fi
		scan_content "WORKTREE:$path" "$content_file"
		continue
	fi
	if [ ! -f "$file" ]; then
		report "WORKTREE:$path" 'unsupported worktree file type'
		continue
	fi
	scan_content "WORKTREE:$path" "$file"
done <"$worktree_manifest"

if [ "$source_count" -eq 0 ] && [ "$failures" -eq 0 ]; then
	report 'repository' 'no Git files to scan'
fi

if [ "$failures" -eq 0 ]; then
	printf '%s\n' 'public-check: passed'
	exit 0
fi
printf '%s\n' 'public-check: failed; inspect the reported paths before publication' >&2
exit 1
