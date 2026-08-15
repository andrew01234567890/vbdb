#!/usr/bin/env bash
# Check every byte source that a Git publication can carry. This intentionally
# prints only paths and finding categories, never matched text.
set -u
export GIT_NO_REPLACE_OBJECTS=1

require_bash4() {
	local major=$1
	if [ "$major" -lt 4 ]; then
		printf '%s\n' 'public-check: Bash 4 or newer is required' >&2
		return 1
	fi
}

if ! require_bash4 "${BASH_VERSINFO[0]:-0}"; then
	exit 2
fi

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if ! git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf '%s\n' 'public-check: not inside a Git worktree' >&2
	exit 2
fi
if ! shallow=$(git -C "$root" rev-parse --is-shallow-repository 2>/dev/null); then
	printf '%s\n' 'public-check: unable to determine shallow-repository state' >&2
	exit 2
fi
case "$shallow" in
	true)
		printf '%s\n' 'public-check: shallow repositories are not publishable' >&2
		exit 2
		;;
	false) ;;
	*)
		printf '%s\n' 'public-check: invalid shallow-repository state' >&2
		exit 2
		;;
esac

history_manifest=
tree_manifest=
refs_manifest=
index_manifest=
worktree_manifest=
content_file=
object_file=
failures=0
source_count=0
declare -A scanned_blobs=()
declare -A scanned_objects=()

cleanup() {
	[ -z "$history_manifest" ] || rm -f -- "$history_manifest"
	[ -z "$tree_manifest" ] || rm -f -- "$tree_manifest"
	[ -z "$refs_manifest" ] || rm -f -- "$refs_manifest"
	[ -z "$index_manifest" ] || rm -f -- "$index_manifest"
	[ -z "$worktree_manifest" ] || rm -f -- "$worktree_manifest"
	[ -z "$content_file" ] || rm -f -- "$content_file"
	[ -z "$object_file" ] || rm -f -- "$object_file"
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
	! refs_manifest=$(make_temp) || ! index_manifest=$(make_temp) || \
	! worktree_manifest=$(make_temp) || ! content_file=$(make_temp) || \
	! object_file=$(make_temp); then
	printf '%s\n' 'public-check: unable to create secure scan files' >&2
	exit 2
fi

# Keep this list narrow: ordinary words such as "token" and public header
# documentation must not be treated as credentials.
key_begin='-----BEGIN '
key_end='-----'
private_word='PRIVATE KEY'
private_key_pattern="${key_begin}(RSA|EC|OPENSSH|DSA) ${private_word}${key_end}"
pgp_key_pattern="${key_begin}PGP ${private_word} BLOCK${key_end}"
encrypted_key_pattern="${key_begin}ENCRYPTED ${private_word}${key_end}"
pkcs8_key_pattern="${key_begin}${private_word}${key_end}"
credential_pattern="${private_key_pattern}|${pgp_key_pattern}|${encrypted_key_pattern}|${pkcs8_key_pattern}|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[0-9A-Za-z-]{10,}|[A-Z][A-Z0-9_]*(SECRET|PASSWORD|PRIVATE_KEY)[A-Z0-9_]*[[:space:]]*=[[:space:]]*[^[:space:]#]{8,}"

# These are names, not content matches. Examples and source files remain
# publishable, while common local credential and private-material paths do not.
is_high_risk_name() {
	local path=$1
	local base=${path##*/}
	local lower_path lower_base
	lower_path=$(printf '%s' "$path" | LC_ALL=C tr '[:upper:]' '[:lower:]')
	lower_base=$(printf '%s' "$base" | LC_ALL=C tr '[:upper:]' '[:lower:]')
	case "$lower_path" in
		secrets/*|private/*|credentials/*|.codex/*|.claude/*|.aws/*|.ssh/*|.kube/*|*/secrets/*|*/private/*|*/credentials/*|*/.codex/*|*/.claude/*|*/.aws/*|*/.ssh/*|*/.kube/*)
			return 0
			;;
	esac
	case "$lower_base" in
		.env|.env.*|*.pem|*.key|*.p12|*.pfx|*.jks|id_rsa|id_rsa.*|id_ed25519|id_ed25519.*|credentials|credentials.*|credentials.json|secret|secret.*|secrets|secrets.*)
			[ "$lower_base" != ".env.example" ]
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

scan_ref_name() {
	local ref=$1 object_id=$2
	printf '%s' "$ref" >"$content_file"
	scan_content "REF:$object_id" "$content_file"
}

scan_tree() {
	local label=$1 treeish=$2 record meta path object_type object_id
	if ! git -C "$root" ls-tree -r -z --full-tree "$treeish" >"$tree_manifest" 2>/dev/null; then
		printf '%s\n' 'public-check: unable to enumerate reachable tree' >&2
		exit 2
	fi
	while IFS= read -r -d '' record; do
		if [[ "$record" != *$'\t'* ]]; then
			report "$label" 'malformed Git tree entry'
			exit 2
		fi
		meta=${record%%$'\t'*}
		path=${record#*$'\t'}
		read -r _ object_type object_id <<<"$meta"
		if [ "$object_type" != 'blob' ] || [ -z "$object_id" ]; then
			continue
		fi
		if is_high_risk_name "$path"; then
			report "$label:$path" 'high-risk private artifact filename'
			continue
		fi
		scan_blob "$label:$path" "$object_id"
	done <"$tree_manifest"
}

scan_object() {
	local label=$1 object_id=$2 object_type line candidate target=
	if [[ ${scanned_objects[$object_id]+seen} ]]; then
		return
	fi
	scanned_objects[$object_id]=seen
	if ! object_type=$(git -C "$root" cat-file -t "$object_id" 2>/dev/null); then
		report "$label:$object_id" 'unable to read reachable Git object'
		exit 2
	fi
	case "$object_type" in
		commit)
			if ! git -C "$root" cat-file commit "$object_id" >"$object_file" 2>/dev/null; then
				report "COMMIT:$object_id" 'unable to read reachable commit object'
				exit 2
			fi
			scan_content "COMMIT:$label:$object_id" "$object_file"
			scan_tree "$label:$object_id" "$object_id"
			;;
		tag)
			if ! git -C "$root" cat-file tag "$object_id" >"$object_file" 2>/dev/null; then
				report "TAG:$object_id" 'unable to read reachable tag object'
				exit 2
			fi
			scan_content "TAG:$object_id" "$object_file"
			while IFS= read -r line; do
				case "$line" in
					object\ *)
						candidate=${line#object }
						if [[ "$candidate" =~ ^[0-9a-f]{40}$ || "$candidate" =~ ^[0-9a-f]{64}$ ]]; then
							target=$candidate
							break
						fi
						;;
				esac
			done <"$object_file"
			if [ -z "$target" ]; then
				report "TAG:$object_id" 'malformed reachable tag object'
				exit 2
			fi
			scan_object "$label" "$target"
			;;
		tree)
			scan_tree "TREE:$label:$object_id" "$object_id"
			;;
		blob)
			scan_blob "BLOB:$label:$object_id" "$object_id"
			;;
		*)
			report "$label:$object_id" 'unsupported reachable Git object type'
			exit 2
			;;
	esac
}

# Every commit reachable from any local ref, plus a detached HEAD, is
# publishable history. An unborn repository has no history; untracked files
# are still checked below. The raw commit payload is scanned for metadata and
# message content in addition to each commit tree.
if head_oid=$(git -C "$root" rev-parse --verify --quiet HEAD 2>/dev/null); then
	if ! git -C "$root" rev-list --topo-order --all "$head_oid" >"$history_manifest" 2>/dev/null; then
		printf '%s\n' 'public-check: unable to enumerate reachable history' >&2
		exit 2
	fi
else
	if ! git -C "$root" rev-list --topo-order --all >"$history_manifest" 2>/dev/null; then
		printf '%s\n' 'public-check: unable to enumerate reachable history' >&2
		exit 2
	fi
fi
while IFS= read -r commit; do
	scan_object 'HISTORY' "$commit"
done <"$history_manifest"

# Scan ref objects themselves so annotated tag messages and nested tag targets
# cannot bypass the commit/tree walk. for-each-ref emits a NUL after each ref
# name and a newline after its object ID.
if ! git -C "$root" for-each-ref --format='%(refname)%00%(objectname)' >"$refs_manifest" 2>/dev/null; then
	printf '%s\n' 'public-check: unable to enumerate refs' >&2
	exit 2
fi
while IFS= read -r -d '' ref; do
	if ! IFS= read -r object_id; then
		printf '%s\n' 'public-check: malformed refs enumeration' >&2
		exit 2
	fi
	if [[ ! "$object_id" =~ ^[0-9a-f]{40}$ && ! "$object_id" =~ ^[0-9a-f]{64}$ ]]; then
		printf '%s\n' 'public-check: malformed refs enumeration' >&2
		exit 2
	fi
	scan_ref_name "$ref" "$object_id"
	scan_object 'REF' "$object_id"
done <"$refs_manifest"

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
