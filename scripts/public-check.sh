#!/usr/bin/env bash
# Check every byte source that a Git publication can carry. This intentionally
# prints only paths and finding categories, never matched text.
set -u
export LC_ALL=C
export GIT_PAGER=cat
export PAGER=cat
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

if ! root=$(CDPATH= cd -- "$(dirname -- "$0")/.." 2>/dev/null && pwd -P); then
	printf '%s\n' 'public-check: unable to canonicalize repository root' >&2
	exit 2
fi

# The repository-local config is untrusted too. Override worktree selection,
# global excludes/attributes, filesystem monitors, and hideRefs for every Git
# query. Repository .gitattributes remains active and is checked explicitly.
git_safe() {
	git --no-pager --work-tree="$root" -C "$root" \
		-c "core.worktree=$root" \
		-c core.excludesFile=/dev/null \
		-c core.attributesFile=/dev/null \
		-c core.fsmonitor=false \
		-c transfer.hideRefs= \
		-c receive.hideRefs= \
		-c uploadpack.hideRefs= \
		"$@"
}

git_top=$(git_safe rev-parse --show-toplevel 2>/dev/null) || {
	printf '%s\n' 'public-check: unable to resolve Git worktree root' >&2
	exit 2
}
if ! git_top=$(CDPATH= cd -- "$git_top" 2>/dev/null && pwd -P); then
	printf '%s\n' 'public-check: unable to canonicalize Git worktree root' >&2
	exit 2
fi
if [ "$git_top" != "$root" ]; then
	printf '%s\n' 'public-check: Git worktree root differs from script root' >&2
	exit 2
fi
if ! git_safe rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf '%s\n' 'public-check: not inside a Git worktree' >&2
	exit 2
fi
if ! shallow=$(git_safe rev-parse --is-shallow-repository 2>/dev/null); then
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
if ! common_dir=$(git_safe rev-parse --path-format=absolute --git-common-dir 2>/dev/null); then
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
tree_manifest_dir=
refs_manifest=
index_manifest=
worktree_manifest=
content_file=
object_file=
helper_build_dir=
worktree_reader=
path_digest=
path_display_safe=0
failures=0
source_count=0
path_count=0
max_scan_bytes=$((8 * 1024 * 1024))
max_manifest_bytes=$((8 * 1024 * 1024))
max_temp_bytes=$((64 * 1024 * 1024))
max_total_scan_bytes=$((512 * 1024 * 1024))
max_scanned_objects=100000
max_scanned_blobs=100000
max_scanned_trees=100000
max_tree_depth=1024
max_tree_object_bytes=$((256 * 1024 * 1024))
max_tree_manifest_bytes=$((64 * 1024 * 1024))
max_source_count=200000
max_path_count=200000
total_scan_bytes=0
tree_object_bytes=0
tree_manifest_bytes=0
declare -A scanned_blobs=()
declare -A scanned_objects=()
declare -A scanned_trees=()
declare -A tree_manifest_files=()
tree_queue_ids=()
tree_queue_prefixes=()
tree_queue_depths=()
tree_queue_labels=()
tree_queue_head=0

# Tests may request a lower bound, but no caller-controlled value may raise a
# safety limit. Invalid overrides fail closed.
is_safe_limit() {
	local value=$1
	case "$value" in
		''|*[!0-9]) return 1 ;;
	esac
	[ "${#value}" -le 9 ] || return 1
	[ "$value" -ne 0 ] 2>/dev/null
}

for limit_spec in \
	VBDB_PUBLIC_CHECK_MAX_OBJECTS:max_scanned_objects:100000 \
	VBDB_PUBLIC_CHECK_MAX_BLOBS:max_scanned_blobs:100000 \
	VBDB_PUBLIC_CHECK_MAX_TREES:max_scanned_trees:100000 \
	VBDB_PUBLIC_CHECK_MAX_TREE_DEPTH:max_tree_depth:1024 \
	VBDB_PUBLIC_CHECK_MAX_TREE_BYTES:max_tree_object_bytes:268435456 \
	VBDB_PUBLIC_CHECK_MAX_TREE_MANIFEST_BYTES:max_tree_manifest_bytes:67108864 \
	VBDB_PUBLIC_CHECK_MAX_SOURCES:max_source_count:200000 \
	VBDB_PUBLIC_CHECK_MAX_PATHS:max_path_count:200000 \
	VBDB_PUBLIC_CHECK_MAX_TOTAL_BYTES:max_total_scan_bytes:536870912; do
	IFS=: read -r limit_env limit_var limit_default <<<"$limit_spec"
	limit_value=${!limit_env-}
	if [ -n "$limit_value" ]; then
		if ! is_safe_limit "$limit_value"; then
				printf 'public-check: invalid %s limit\n' "$limit_env" >&2
				exit 2
		fi
		if [ "$limit_value" -lt "$limit_default" ]; then
			printf -v "$limit_var" '%s' "$limit_value"
		fi
	fi
done

cleanup() {
	[ -z "$helper_build_dir" ] || rm -rf -- "$helper_build_dir"
	[ -z "$history_manifest" ] || rm -f -- "$history_manifest"
	[ -z "$tree_manifest_dir" ] || rm -rf -- "$tree_manifest_dir"
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

# Build the reviewed, dependency-free reader afresh for every scan. The
# output lives in a randomized, mode-0700 directory outside the repository;
# no ignored repository binary or caller-controlled Go environment is trusted.
helper_source_dir="$root/cmd/vbdb-worktree-read"
if [ ! -d "$helper_source_dir" ] || [ -L "$root/cmd" ] ||
	[ -L "$helper_source_dir" ] || [ -L "$helper_source_dir/main.go" ] ||
	[ ! -f "$helper_source_dir/main.go" ]; then
	printf '%s\n' 'public-check: reviewed worktree reader source is unavailable or symlinked' >&2
	exit 2
fi
helper_source_physical=$(CDPATH= cd -- "$helper_source_dir" 2>/dev/null && pwd -P) || {
	printf '%s\n' 'public-check: unable to canonicalize worktree reader source' >&2
	exit 2
}
if [ "$helper_source_physical" != "$helper_source_dir" ]; then
	printf '%s\n' 'public-check: worktree reader source escapes repository' >&2
	exit 2
fi
if ! helper_build_dir=$(mktemp -d "$scan_tmp_root/vbdb-public-reader.XXXXXX" 2>/dev/null); then
	printf '%s\n' 'public-check: unable to create private reader build directory' >&2
	exit 2
fi
if ! chmod 0700 "$helper_build_dir" || [ -L "$helper_build_dir" ] ||
	! helper_build_dir_physical=$(CDPATH= cd -- "$helper_build_dir" 2>/dev/null && pwd -P) ||
	[ "$helper_build_dir_physical" != "$helper_build_dir" ]; then
	printf '%s\n' 'public-check: private reader build directory failed physical validation' >&2
	exit 2
fi
case "$helper_build_dir" in
	"$scan_tmp_root"/*) ;;
	*)
		printf '%s\n' 'public-check: private reader build directory escaped temporary root' >&2
		exit 2
		;;
esac
if ! cp -- "$helper_source_dir/main.go" "$helper_build_dir/main.go" ||
	! printf '%s\n' 'module vbdb-worktree-reader' 'go 1.26.4' >"$helper_build_dir/go.mod"; then
	printf '%s\n' 'public-check: unable to stage reviewed worktree reader source' >&2
	exit 2
fi
worktree_reader="$helper_build_dir/vbdb-worktree-read"
if ! (
	cd -- "$helper_build_dir" || exit 1
	unset GOFLAGS GOENV GOWORK GOTOOLCHAIN GOPROXY GOSUMDB GOPRIVATE GONOPROXY GONOSUMDB GO111MODULE
	export GOFLAGS= GOENV=off GOWORK=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off
	export GOPRIVATE= GONOPROXY= GONOSUMDB= GO111MODULE=on
	export GOCACHE="$helper_build_dir/go-cache" GOMODCACHE="$helper_build_dir/go-modcache"
	go build -buildvcs=false -mod=mod -o "$worktree_reader" .
); then
	printf '%s\n' 'public-check: reviewed worktree reader build failed' >&2
	exit 2
fi
if ! chmod 0700 "$worktree_reader" || [ -L "$worktree_reader" ] || [ ! -x "$worktree_reader" ]; then
	printf '%s\n' 'public-check: freshly built worktree reader failed validation' >&2
	exit 2
fi

report() {
	printf 'public-check: %s: %s\n' "$1" "$2"
	failures=1
}

make_temp() {
	local path
	path=$(mktemp "$scan_tmp_root/vbdb-public-check.XXXXXX" 2>/dev/null) || return 1
	printf '%s\n' "$path"
}

account_temp_bytes() {
	local total=0 file size
	for file in "$history_manifest" "$refs_manifest" \
		"$index_manifest" "$worktree_manifest" "$content_file" "$object_file"; do
		if [ -z "$file" ] || [ ! -e "$file" ]; then
			continue
		fi
		if ! size=$(wc -c <"$file" 2>/dev/null); then
			return 1
		fi
		total=$((total + size))
		if [ "$total" -gt "$max_temp_bytes" ]; then
			return 2
		fi
	done
	if [ "$tree_manifest_bytes" -gt "$max_temp_bytes" ] ||
		[ "$total" -gt $((max_temp_bytes - tree_manifest_bytes)) ]; then
		return 2
	fi
	return 0
}

account_tree_counter() {
	local label=$1 bytes=$2 counter_name=$3 limit=$4 current remaining next
	case "$bytes" in
		''|*[!0-9])
			report "$label" 'invalid tree-byte count'
			exit 2
		;;
	esac
	[ "${#bytes}" -le 9 ] || {
		report "$label" 'tree-byte count overflow'
		exit 2
	}
	current=${!counter_name}
	if [ "$current" -gt "$limit" ]; then
		report "$label" 'tree safety limit exceeded'
		exit 2
	fi
	remaining=$((limit - current))
	if [ "$bytes" -gt "$remaining" ]; then
		report "$label" 'tree safety limit exceeded'
		exit 2
	fi
	next=$((current + bytes))
	printf -v "$counter_name" '%s' "$next"
}

account_scan_bytes() {
	local label=$1 bytes=$2 remaining
	case "$bytes" in
		''|*[!0-9])
			report "$label" 'invalid scan-byte count'
			exit 2
			;;
	esac
	[ "${#bytes}" -le 9 ] || {
		report "$label" 'scan-byte count overflow'
		exit 2
	}
	if [ "$total_scan_bytes" -gt "$max_total_scan_bytes" ]; then
		report "$label" 'cumulative scan-byte safety limit exceeded'
		exit 2
	fi
	remaining=$((max_total_scan_bytes - total_scan_bytes))
	if [ "$bytes" -gt "$remaining" ]; then
		report "$label" 'cumulative scan-byte safety limit exceeded'
		exit 2
	fi
	total_scan_bytes=$((total_scan_bytes + bytes))
}

write_bounded_manifest() {
	local output=$1 status size
	shift
	git_safe "$@" 2>/dev/null |
		head -c "$((max_manifest_bytes + 1))" >"$output"
	local -a statuses=("${PIPESTATUS[@]}")
	if ! size=$(wc -c <"$output" 2>/dev/null); then
		return 1
	fi
	if [ "$size" -gt "$max_manifest_bytes" ]; then
		return 2
	fi
	account_scan_bytes "MANIFEST:$*" "$size"
	if [ "${statuses[0]}" -ne 0 ] || [ "${statuses[1]}" -ne 0 ]; then
		return 1
	fi
	account_temp_bytes
	status=$?
	[ "$status" -eq 0 ] || return "$status"
	return 0
}

if ! history_manifest=$(make_temp) || \
	! refs_manifest=$(make_temp) || ! index_manifest=$(make_temp) || \
	! worktree_manifest=$(make_temp) || ! content_file=$(make_temp) || \
	! object_file=$(make_temp); then
	printf '%s\n' 'public-check: unable to create secure scan files' >&2
	exit 2
fi
if ! tree_manifest_dir=$(mktemp -d "$scan_tmp_root/vbdb-public-trees.XXXXXX" 2>/dev/null) ||
	! chmod 0700 "$tree_manifest_dir" || [ -L "$tree_manifest_dir" ] ||
	! tree_manifest_physical=$(CDPATH= cd -- "$tree_manifest_dir" 2>/dev/null && pwd -P) ||
	[ "$tree_manifest_physical" != "$tree_manifest_dir" ]; then
	report 'TREE' 'unable to create validated tree manifest directory'
	exit 2
fi
case "$tree_manifest_dir" in
	"$scan_tmp_root"/*) ;;
	*)
	report 'TREE' 'tree manifest directory escaped temporary root'
	exit 2
	;;
esac

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
assignment_words=$(printf '%s' \
	'SECRET|PASSWORD|PRIVATE_KEY|ACCESS_KEY|SECRET_ACCESS_KEY|DB_PASSWORD|DATABASE_PASSWORD|CLIENT_SECRET')
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
credential_words=$(printf '%s' \
	'SECRET|PASSWORD|PASSWD|TOKEN|API[_-]?KEY|AUTH[_-]?TOKEN|' \
	'ACCESS[_-]?KEY|SECRET[_-]?KEY|SECRET[_-]?ACCESS[_-]?KEY|' \
	'DB[_-]?PASSWORD|DATABASE[_-]?PASSWORD|CLIENT[_-]?SECRET|' \
	'PRIVATE[_-]?KEY|REGISTRY[_-]?PASSWORD')
credential_key_prefix='(^|[^A-Z0-9_])'
credential_quote=$(printf '\047')
credential_key_suffix="[\"${credential_quote}]?[[:space:]]*"
credential_value="[[:space:]]*[\"${credential_quote}]?[^[:space:]#()\$\\[]{7,}"
credential_colon_pattern="${credential_key_prefix}[\"${credential_quote}]?(${credential_words})${credential_key_suffix}:${credential_value}"
# These are deliberately high-entropy, provider-specific forms. The scanner
# never prints a match; it reports only the source category/path digest.
url_basic_auth_pattern='https?://[^[:space:]/@]+:[^[:space:]@]{8,}@'
google_api_key_pattern='AIza[0-9A-Za-z_-]{35}'
openai_key_pattern='sk-(proj-)?[A-Za-z0-9_-]{20,}'
jwt_pattern='eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}'
slack_webhook_pattern='https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+ '
# The trailing-space fragment above is removed so the pattern source cannot
# accidentally become a complete webhook fixture in this public scanner.
slack_webhook_pattern=${slack_webhook_pattern% }
kube_client_key_pattern='client-key-data:[[:space:]]*[A-Za-z0-9+/=]{40,}'
credential_pattern="${pem_pattern}|${pgp_key_pattern}|${encrypted_key_pattern}|${pkcs8_key_pattern}|${ssh2_encrypted_key_pattern}|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[0-9A-Za-z-]{10,}|${url_basic_auth_pattern}|${google_api_key_pattern}|${openai_key_pattern}|${jwt_pattern}|${slack_webhook_pattern}|${kube_client_key_pattern}|${assignment_pattern}|${credential_colon_pattern}"

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
		*.pem|*.key|*.p12|*.pfx|*.jks|*.p8|*.tfstate|*.tfstate.*|*.ovpn|id_rsa|id_rsa.*|id_ed25519|id_ed25519.*|id_ecdsa|id_ecdsa.*|id_dsa|id_dsa.*|credentials|credentials.json|credentials.yml|credentials.yaml|secret|secrets|.netrc|_netrc|.git-credentials|.npmrc|.pypirc|.pgpass|.htpasswd|.dockercfg|.s3cfg|.boto|.terraformrc|kubeconfig|kubeconfig.*|*.kubeconfig)
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
	local label=$1 file=$2 already_accounted=${3:-0} grep_status
	source_count=$((source_count + 1))
	if [ "$source_count" -gt "$max_source_count" ]; then
		report "$label" 'source-count safety limit exceeded'
		exit 2
	fi
	local size
	if ! size=$(wc -c <"$file" 2>/dev/null); then
		report "$label" 'unable to size file'
		return
	fi
	if [ "$size" -gt "$max_scan_bytes" ]; then
		report "$label" 'file exceeds bounded scan size'
		return
	fi
	if [ "$already_accounted" -eq 0 ]; then
		account_scan_bytes "$label" "$size"
	fi
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
	if [ "${#scanned_blobs[@]}" -gt "$max_scanned_blobs" ]; then
		report "$label" 'unique-blob safety limit exceeded'
		exit 2
	fi
	local blob_size
	if ! blob_size=$(git_safe cat-file -s "$object_id" 2>/dev/null); then
		report "$label" 'unable to size Git blob'
		exit 2
	fi
	if [ "$blob_size" -gt "$max_scan_bytes" ]; then
		report "$label" 'Git blob exceeds bounded scan size'
		exit 2
	fi
	account_scan_bytes "$label" "$blob_size"
	if ! git_safe cat-file blob "$object_id" >"$content_file" 2>/dev/null; then
		report "$label" 'unable to read Git blob'
		exit 2
	fi
	scan_content "$label" "$content_file" 1
}

scan_ref_name() {
	local ref=$1 object_id=$2
	printf '%s' "$ref" >"$content_file"
	scan_content "REF:$object_id" "$content_file"
}

read_object_bounded() {
	local object_type=$1 object_id=$2 output=$3 object_size
	if ! object_size=$(git_safe cat-file -s "$object_id" 2>/dev/null); then
		return 1
	fi
	if [ "$object_size" -gt "$max_scan_bytes" ]; then
		return 2
	fi
	account_scan_bytes "OBJECT:$object_id" "$object_size"
	git_safe cat-file "$object_type" "$object_id" >"$output" 2>/dev/null
}

scan_path_bytes() {
	local source=$1 path=$2 path_size grep_status
	path_count=$((path_count + 1))
	if [ "$path_count" -gt "$max_path_count" ]; then
		report "$source" 'path-count safety limit exceeded'
		exit 2
	fi
	if ! printf '%s' "$path" >"$content_file"; then
		report "$source" 'unable to materialize path bytes'
		return 2
	fi
	if ! path_size=$(wc -c <"$content_file" 2>/dev/null); then
		report "$source" 'unable to size path bytes'
		return 2
	fi
	account_scan_bytes "$source" "$path_size"
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

enqueue_tree() {
	local tree_id=$1 prefix=$2 depth=$3 label=$4
	case "$tree_id" in
		''|*[!0-9a-fA-F]*)
			report "$label" 'malformed reachable tree object'
			exit 2
		;;
	esac
	if [ "$depth" -gt "$max_tree_depth" ]; then
		report "$label" 'tree-depth safety limit exceeded'
		exit 2
	fi
	if [ "${#tree_queue_ids[@]}" -ge "$max_path_count" ]; then
		report "$label" 'tree-queue safety limit exceeded'
		exit 2
	fi
	tree_queue_ids+=([${#tree_queue_ids[@]}]="$tree_id")
	tree_queue_prefixes+=([${#tree_queue_prefixes[@]}]="$prefix")
	tree_queue_depths+=([${#tree_queue_depths[@]}]="$depth")
	tree_queue_labels+=([${#tree_queue_labels[@]}]="$label")
}

scan_tree_occurrence() {
	local label=$1 tree_id=$2 prefix=$3 depth=$4
	local record meta name object_type object_id path path_match path_sensitive safe_label
	local manifest manifest_status manifest_size tree_size
	if [[ ! ${scanned_trees[$tree_id]+seen} ]]; then
		scanned_trees[$tree_id]=seen
		if [ "${#scanned_trees[@]}" -gt "$max_scanned_trees" ]; then
			report "$label:$tree_id" 'unique-tree safety limit exceeded'
			exit 2
		fi
		if [[ ! ${scanned_objects[$tree_id]+seen} ]]; then
			scanned_objects[$tree_id]=seen
			if [ "${#scanned_objects[@]}" -gt "$max_scanned_objects" ]; then
				report "$label:$tree_id" 'unique-object safety limit exceeded'
				exit 2
			fi
		fi
		if ! tree_size=$(git_safe cat-file -s "$tree_id" 2>/dev/null); then
			report "$label:$tree_id" 'unable to size reachable tree'
			exit 2
		fi
		if [ "$tree_size" -gt "$max_scan_bytes" ]; then
			report "$label:$tree_id" 'reachable Git tree exceeds bounded scan size'
			exit 2
		fi
		account_tree_counter "TREE:$tree_id" "$tree_size" tree_object_bytes "$max_tree_object_bytes"
		if ! read_object_bounded tree "$tree_id" "$object_file"; then
			report "$label:$tree_id" 'unable to read reachable Git tree'
			exit 2
		fi
		scan_content "TREE:$label:$tree_id" "$object_file" 1
		if ! manifest=$(mktemp "$tree_manifest_dir/manifest.XXXXXX" 2>/dev/null); then
			report "$label:$tree_id" 'unable to create tree manifest'
			exit 2
		fi
		manifest_status=0
		write_bounded_manifest "$manifest" ls-tree -z "$tree_id" || manifest_status=$?
		if [ "$manifest_status" -eq 2 ]; then
			report "$label:$tree_id" 'reachable tree listing exceeds bounded scan size'
			exit 2
		fi
		if [ "$manifest_status" -ne 0 ]; then
			report "$label:$tree_id" 'unable to enumerate reachable tree'
			exit 2
		fi
		if ! manifest_size=$(wc -c <"$manifest" 2>/dev/null); then
			report "$label:$tree_id" 'unable to size tree manifest'
			exit 2
		fi
		account_tree_counter "TREE-MANIFEST:$tree_id" "$manifest_size" tree_manifest_bytes "$max_tree_manifest_bytes"
		if ! account_temp_bytes; then
			report "$label:$tree_id" 'tree manifest temporary-byte safety limit exceeded'
			exit 2
		fi
		tree_manifest_files[$tree_id]=$manifest
	fi
	manifest=${tree_manifest_files[$tree_id]}
	while IFS= read -r -d '' record; do
		if [[ "$record" != *$'\t'* ]]; then
			report "$label:$tree_id" 'malformed Git tree entry'
			exit 2
		fi
		meta=${record%%$'\t'*}
		name=${record#*$'\t'}
		read -r _ object_type object_id <<<"$meta"
		if [ -z "$object_type" ] || [ -z "$object_id" ] || [ -z "$name" ]; then
			report "$label:$tree_id" 'malformed Git tree entry'
			exit 2
		fi
		case "$object_id" in
			*[!0-9a-fA-F]*)
				report "$label:$tree_id" 'malformed Git tree object identifier'
				exit 2
			;;
		esac
		case "${#object_id}" in
			40|64) ;;
			*)
				report "$label:$tree_id" 'malformed Git tree object identifier'
				exit 2
			;;
		esac
		path="${prefix}${name}"
		path_match=0
		if scan_path_bytes "PATH:TREE:$label:$tree_id" "$path"; then
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
		case "$object_type" in
			tree)
			if is_high_risk_name "$path"; then
				report "$safe_label" 'high-risk private artifact filename'
			fi
			enqueue_tree "$object_id" "$path/" "$((depth + 1))" "$safe_label"
			;;
			blob)
				if is_high_risk_name "$path"; then
					report "$safe_label" 'high-risk private artifact filename'
					continue
				fi
				scan_blob "$safe_label" "$object_id"
			;;
			*)
				report "$safe_label" 'unsupported historical tree entry'
				exit 2
			;;
		esac
	done <"$manifest"
}

drain_tree_queue() {
	local tree_id prefix depth label
	while [ "$tree_queue_head" -lt "${#tree_queue_ids[@]}" ]; do
		tree_id=${tree_queue_ids[$tree_queue_head]}
		prefix=${tree_queue_prefixes[$tree_queue_head]}
		depth=${tree_queue_depths[$tree_queue_head]}
		label=${tree_queue_labels[$tree_queue_head]}
		tree_queue_head=$((tree_queue_head + 1))
		scan_tree_occurrence "$label" "$tree_id" "$prefix" "$depth"
	done
}

scan_tree() {
	local label=$1 treeish=$2 tree_id
	if ! tree_id=$(git_safe rev-parse --verify "$treeish^{tree}" 2>/dev/null); then
		printf '%s\n' 'public-check: unable to resolve reachable tree' >&2
		exit 2
	fi
	enqueue_tree "$tree_id" '' 0 "$label"
}

scan_object() {
	local label=$1 object_id=$2 object_type line candidate target=
	local read_status
	if [[ ${scanned_objects[$object_id]+seen} ]]; then
		return
	fi
	scanned_objects[$object_id]=seen
	if [ "${#scanned_objects[@]}" -gt "$max_scanned_objects" ]; then
		report "$label:$object_id" 'unique-object safety limit exceeded'
		exit 2
	fi
	if ! object_type=$(git_safe cat-file -t "$object_id" 2>/dev/null); then
		report "$label:$object_id" 'unable to read reachable Git object'
		exit 2
	fi
	case "$object_type" in
		commit)
			read_status=0
			read_object_bounded commit "$object_id" "$object_file" || read_status=$?
			if [ "$read_status" -ne 0 ]; then
				if [ "$read_status" -eq 2 ]; then
					report "COMMIT:$object_id" 'reachable Git object exceeds bounded scan size'
					exit 2
				fi
				report "COMMIT:$object_id" 'unable to read reachable commit object'
				exit 2
			fi
			scan_content "COMMIT:$label:$object_id" "$object_file" 1
			scan_tree "$label:$object_id" "$object_id"
			;;
		tag)
			read_status=0
			read_object_bounded tag "$object_id" "$object_file" || read_status=$?
			if [ "$read_status" -ne 0 ]; then
				if [ "$read_status" -eq 2 ]; then
					report "TAG:$object_id" 'reachable Git object exceeds bounded scan size'
					exit 2
				fi
				report "TAG:$object_id" 'unable to read reachable tag object'
				exit 2
			fi
			scan_content "TAG:$object_id" "$object_file" 1
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

validate_worktree_parent() {
	local file=$1 parent physical
	case "$file" in
		"$root"/*) ;;
		*) return 1 ;;
	esac
	parent=${file%/*}
	if [ "$parent" = "$file" ]; then
		parent=$root
	fi
	if ! physical=$(CDPATH= cd -- "$parent" 2>/dev/null && pwd -P); then
		return 1
	fi
	case "$physical" in
		"$root"|"$root"/*) return 0 ;;
		*) return 1 ;;
	esac
}

# Every commit reachable from any local ref, plus a detached HEAD, is
# publishable history. An unborn repository has no history; untracked files
# are still checked below. The raw commit payload is scanned for metadata and
# message content in addition to each commit tree.
if head_oid=$(git_safe rev-parse --verify --quiet HEAD 2>/dev/null); then
	history_status=0
	write_bounded_manifest "$history_manifest" rev-list --topo-order --all "$head_oid" || history_status=$?
	if [ "$history_status" -ne 0 ]; then
		printf '%s\n' 'public-check: unable to enumerate reachable history' >&2
		exit 2
	fi
else
	history_status=0
	write_bounded_manifest "$history_manifest" rev-list --topo-order --all || history_status=$?
	if [ "$history_status" -ne 0 ]; then
		printf '%s\n' 'public-check: unable to enumerate reachable history' >&2
		exit 2
	fi
fi
while IFS= read -r commit; do
	scan_object 'HISTORY' "$commit"
done <"$history_manifest"
drain_tree_queue

# Scan ref objects themselves so annotated tag messages and nested tag targets
# cannot bypass the commit/tree walk. for-each-ref emits a NUL after each ref
# name and a newline after its object ID.
refs_status=0
write_bounded_manifest "$refs_manifest" for-each-ref --format='%(refname)%00%(objectname)' || refs_status=$?
if [ "$refs_status" -ne 0 ]; then
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
drain_tree_queue

# Stage-zero index bytes are publishable. Any unresolved stage is rejected
# instead of allowing an ambiguous index to pass the safety gate.
index_status=0
write_bounded_manifest "$index_manifest" ls-files --stage -z || index_status=$?
if [ "$index_status" -ne 0 ]; then
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
worktree_status=0
write_bounded_manifest "$worktree_manifest" ls-files --cached --others --exclude-standard -z || worktree_status=$?
if [ "$worktree_status" -ne 0 ]; then
	printf '%s\n' 'public-check: unable to enumerate Git files' >&2
	exit 2
fi
while IFS= read -r -d '' path; do
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
	if ! validate_worktree_parent "$root/$path"; then
		report "$safe_label" 'worktree parent escapes or cannot be resolved inside repository'
		exit 2
	fi
	if is_high_risk_name "$path"; then
		report "$safe_label" 'high-risk private artifact filename'
		continue
	fi
	reader_status=0
	"$worktree_reader" --root "$root" --path "$path" \
		--max-bytes "$max_scan_bytes" >"$content_file" 2>/dev/null || reader_status=$?
	case "$reader_status" in
		0) scan_content "$safe_label" "$content_file" ;;
		4) report "$safe_label" 'file exceeds bounded scan size' ;;
		5) continue ;;
		*) report "$safe_label" 'unable to read confined worktree entry' ;;
	esac
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
