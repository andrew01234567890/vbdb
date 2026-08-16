#!/usr/bin/env bash
# Check every byte source that a Git publication can carry. This intentionally
# prints only paths and finding categories, never matched text.
set -u
# Repository-selection and object-override environment variables are caller
# controlled. Never let them redirect a publication scan away from this
# worktree; ordinary config/identity variables remain untouched.
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE \
	GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_SHALLOW_FILE \
	GIT_REPLACE_REF_BASE GIT_GRAFT_FILE GIT_NAMESPACE GIT_CONFIG \
	GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_CONFIG_NOSYSTEM=1
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

# Git's legacy grafts file can rewrite parentage locally and hide reachable
# history from rev-list. Resolve the shared object directory before any
# history walk and fail closed for every graft path type, including a broken
# symlink. Replace refs are disabled separately above.
common_dir=
if ! common_dir=$(git -C "$root" rev-parse --path-format=absolute --git-common-dir 2>/dev/null); then
	printf '%s\n' 'public-check: unable to resolve Git common directory' >&2
	exit 2
fi
case "$common_dir" in
	/*) ;;
	*)
		printf '%s\n' 'public-check: Git common directory was not absolute' >&2
		exit 2
		;;
esac
if ! common_dir=$(CDPATH= cd -- "$common_dir" && pwd -P); then
	printf '%s\n' 'public-check: unable to validate Git common directory' >&2
	exit 2
fi
check_grafts() {
	local grafts_path=$1
	if [ -e "$grafts_path/info/grafts" ] || [ -L "$grafts_path/info/grafts" ]; then
		return 1
	fi
	return 0
}
if ! check_grafts "$common_dir"; then
	printf '%s\n' 'public-check: legacy Git grafts are not publishable' >&2
	exit 2
fi

scan_tmp_root=${TMPDIR:-/tmp}
case "$scan_tmp_root" in
	/*) ;;
	*)
		printf '%s\n' 'public-check: scan temporary directory must be absolute' >&2
		exit 2
		;;
esac
if ! scan_tmp_root=$(CDPATH= cd -- "$scan_tmp_root" 2>/dev/null && pwd -P); then
	printf '%s\n' 'public-check: unable to validate scan temporary directory' >&2
	exit 2
fi
case "$scan_tmp_root" in
	"$root"|"$root"/*|"$common_dir"|"$common_dir"/*)
		printf '%s\n' 'public-check: scan temporary directory is inside the repository' >&2
		exit 2
		;;
esac
if [ ! -w "$scan_tmp_root" ] || [ ! -x "$scan_tmp_root" ]; then
	printf '%s\n' 'public-check: scan temporary directory is not writable' >&2
	exit 2
fi

history_manifest=
tree_manifest=
refs_manifest=
index_manifest=
worktree_manifest=
content_file=
object_file=
path_digest=
path_display_safe=0
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
	path=$(mktemp "$scan_tmp_root/vbdb-public-check.XXXXXX" 2>/dev/null) || return 1
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
key_phrase='PRIVATE KEY'
pem_pattern="${key_begin}(RSA|EC|OPENSSH|DSA) ${key_phrase}${key_end}"
pgp_key_pattern="${key_begin}PGP ${key_phrase} BLOCK${key_end}"
encrypted_key_pattern="${key_begin}ENCRYPTED ${key_phrase}${key_end}"
pkcs8_key_pattern="${key_begin}${key_phrase}${key_end}"
ssh2_encrypted_key_pattern=$(printf '%s%s%s%s' '---- BEGIN ' 'SSH2 ENCRYPTED ' 'PRIVATE KEY ' '----')
assignment_words=$(printf '%s%s%s%s' 'SECRET' '|PASSWORD|' 'PRIVATE' '_KEY')
assignment_prefix='(^|[^A-Z0-9_])[A-Z0-9_]*('
assignment_suffix=')[A-Z0-9_]*[[:space:]]*'
assignment_equals='='
assignment_value='[[:space:]]*'
assignment_value="${assignment_value}[\"']?"
assignment_forbidden='[^[:space:]#'
assignment_forbidden="${assignment_forbidden}\""
assignment_forbidden="${assignment_forbidden}'"
assignment_forbidden="${assignment_forbidden}("
assignment_forbidden="${assignment_forbidden}\$"
assignment_forbidden="${assignment_forbidden}\\["
assignment_forbidden="${assignment_forbidden}]"
assignment_value="${assignment_value}${assignment_forbidden}"
assignment_value="${assignment_value}[^[:space:]#]{7,}"
assignment_pattern="${assignment_prefix}${assignment_words}${assignment_suffix}${assignment_equals}${assignment_value}"
# Credential-bearing configuration is also commonly written as a quoted
# JSON/YAML key followed by a colon. Keep the key vocabulary explicit and the
# value bounded; broad matches for words such as "token" create false claims
# in ordinary public documentation. The fragments are assembled so this
# scanner source does not contain a complete credential-shaped test string.
credential_words=$(printf '%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s%s' \
	'SECRET' '|PASSWORD' '|PASSWD' '|TOKEN' '|API' '[_-]?KEY' \
	'|AUTH' '[_-]?TOKEN' '|ACCESS' '[_-]?KEY' '|CLIENT' '[_-]?SECRET' \
	'|PRIVATE' '[_-]?KEY' '|REGISTRY' '[_-]?PASSWORD')
credential_key_prefix='(^|[^A-Z0-9_])'
credential_quote=$(printf '\047')
credential_key_suffix="[\"${credential_quote}]?[[:space:]]*"
credential_value="[[:space:]]*[\"${credential_quote}]?[^[:space:]#()\$\\[]{7,}"
credential_colon_pattern="${credential_key_prefix}[\"${credential_quote}]?(${credential_words})${credential_key_suffix}:${credential_value}"
credential_pattern="${pem_pattern}|${pgp_key_pattern}|${encrypted_key_pattern}|${pkcs8_key_pattern}|${ssh2_encrypted_key_pattern}|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[0-9A-Za-z-]{10,}|${assignment_pattern}|${credential_colon_pattern}"

# These are names, not content matches. This is deliberately fail-safe: a
# harmless fixture under a credential-shaped name is rejected too, and its
# path is reported only by digest. Examples should use an explicitly safe name
# such as .env.example; do not add suppressions for a risky basename.
is_high_risk_name() {
	local path=$1
	local base=${path##*/}
	local lower_path lower_base
	lower_path=${path,,}
	lower_base=${base,,}
	case "$lower_path" in
		secrets/*|private/*|credentials/*|.codex/*|.claude/*|.aws/*|.ssh/*|.kube/*|.docker/*|*/secrets/*|*/private/*|*/credentials/*|*/.codex/*|*/.claude/*|*/.aws/*|*/.ssh/*|*/.kube/*|*/.docker/*)
			return 0
			;;
	esac
	case "$lower_base" in
		.env|.env.*)
			case "$lower_base" in
				.env.example|.env.sample|.env.template) return 1 ;;
				*) return 0 ;;
			esac
			;;
		*.pem|*.key|*.p12|*.pfx|*.jks|*.p8|*.tfstate|*.tfstate.*|id_rsa|id_rsa.*|id_ed25519|id_ed25519.*|id_ecdsa|id_ecdsa.*|id_dsa|id_dsa.*|credentials|credentials.json|credentials.yml|credentials.yaml|secret|secrets|.netrc|.git-credentials|.npmrc|.pypirc|.pgpass|.htpasswd|.dockercfg|kubeconfig|kubeconfig.*|*.kubeconfig)
			return 0
		;;
	esac
	return 1
}

scan_signature() {
	local file=$1 grep_status
	LC_ALL=C grep -E -a -q -- "$credential_pattern" "$file" 2>/dev/null
	grep_status=$?
	case "$grep_status" in
		0) return 0 ;;
		1) ;;
		*) return 2 ;;
	esac
	# Configuration-key syntax is case-insensitive; headers and token prefixes
	# retain their deliberately narrow exact matching.
	LC_ALL=C grep -E -a -i -q -- "${assignment_pattern}|${credential_colon_pattern}" "$file" 2>/dev/null
	return $?
}

scan_content() {
	local label=$1 file=$2 grep_status
	source_count=$((source_count + 1))
	# -a is deliberate: binary files are scanned as bytes, while -q keeps
	# matched content out of output.
	scan_signature "$file"
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

scan_path_bytes() {
	local source=$1 path=$2 grep_status
	if ! printf '%s' "$path" >"$content_file"; then
		report "$source" 'unable to materialize path bytes'
		return 2
	fi
	if [[ "$path" =~ ^[A-Za-z0-9._/@+=,-]+$ ]]; then
		path_display_safe=1
	else
		path_display_safe=0
	fi
	if ! path_digest=$(sha256sum "$content_file" 2>/dev/null); then
		report "$source" 'unable to identify path'
		return 2
	fi
	path_digest=${path_digest%%[[:space:]]*}
	scan_signature "$content_file"
	grep_status=$?
	case "$grep_status" in
		0)
			report "$source:$path_digest" 'credential-shaped path'
			return 0
			;;
		1) return 1 ;;
		*)
			report "$source" 'unable to scan path bytes'
			return 2
			;;
	esac
}

path_label() {
	local source=$1 path=$2 sensitive=${3:-0}
	if [ "$sensitive" -eq 0 ] && [ "$path_display_safe" -eq 1 ]; then
		printf '%s:%s\n' "$source" "$path"
	else
		printf '%s:%s\n' "$source" "$path_digest"
	fi
}

scan_tree() {
	local label=$1 treeish=$2 record meta path object_type object_id path_match path_sensitive safe_label
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
		path_match=0
		if scan_path_bytes "PATH:TREE:$label:$treeish" "$path"; then
			path_match=1
		else
			case "$?" in
				1) ;;
			*) exit 2 ;;
			esac
		fi
		path_sensitive=$path_match
		if is_high_risk_name "$path"; then
			path_sensitive=1
		fi
		safe_label=$(path_label "$label" "$path" "$path_sensitive")
		if [ "$object_type" != 'blob' ]; then
			report "$(path_label "PATH:TREE:$label:$treeish" "$path" "$path_sensitive")" 'unsupported historical tree entry'
			exit 2
		fi
		if [ -z "$object_id" ]; then
			report "$safe_label" 'missing historical tree object'
			exit 2
		fi
		if is_high_risk_name "$path"; then
			report "$safe_label" 'high-risk private artifact filename'
			continue
		fi
		scan_blob "$safe_label" "$object_id"
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
	path_match=0
	if scan_path_bytes "PATH:INDEX:$object_id" "$path"; then
		path_match=1
		safe_label="PATH:INDEX:$object_id"
	else
		case "$?" in
			1) ;;
			*) exit 2 ;;
		esac
	fi
	path_sensitive=$path_match
	if is_high_risk_name "$path"; then
		path_sensitive=1
	fi
	safe_label=$(path_label "INDEX:$object_id" "$path" "$path_sensitive")
	if [ "$stage" != '0' ]; then
		report "$safe_label" 'unresolved index stage'
		continue
	fi
	if [ -z "$object_id" ]; then
		report "$safe_label" 'missing index object'
		exit 2
	fi
	if is_high_risk_name "$path"; then
		report "$safe_label" 'high-risk private artifact filename'
		continue
	fi
	scan_blob "$safe_label" "$object_id"
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
	path_match=0
	if scan_path_bytes 'PATH:WORKTREE' "$path"; then
		path_match=1
		safe_label='PATH:WORKTREE'
	else
		case "$?" in
			1) ;;
			*) exit 2 ;;
		esac
	fi
	path_sensitive=$path_match
	if is_high_risk_name "$path"; then
		path_sensitive=1
	fi
	safe_label=$(path_label WORKTREE "$path" "$path_sensitive")
	if is_high_risk_name "$path"; then
		report "$safe_label" 'high-risk private artifact filename'
		continue
	fi
	if [ ! -e "$file" ] && [ ! -L "$file" ]; then
		continue
	fi
	if [ -L "$file" ]; then
		if ! readlink "$file" >"$content_file" 2>/dev/null; then
			report "$safe_label" 'unable to read symlink'
			continue
		fi
		scan_content "$safe_label" "$content_file"
		continue
	fi
	if [ ! -f "$file" ]; then
		report "$safe_label" 'unsupported worktree file type'
		continue
	fi
	scan_content "$safe_label" "$file"
done <"$worktree_manifest"

if [ "$source_count" -eq 0 ] && [ "$failures" -eq 0 ]; then
	report 'repository' 'no Git files to scan'
fi

if [ "$failures" -eq 0 ]; then
	printf '%s\n' 'public-check: passed'
	exit 0
fi
printf '%s\n' 'public-check: failed; inspect the reported findings before publication' >&2
exit 1
