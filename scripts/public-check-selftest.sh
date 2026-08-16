#!/usr/bin/env bash
# Exercise public-check with an untracked temporary fixture. The fixture is
# removed on every exit path and its synthetic marker is never printed.
set -euo pipefail
export GIT_NO_REPLACE_OBJECTS=1

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp_base=${TMPDIR:-/tmp}
case "$tmp_base" in
	/*) ;;
	*)
		printf '%s\n' 'public-check-selftest: TMPDIR must be an absolute directory' >&2
		exit 2
		;;
esac
temp_root=$(mktemp -d "$tmp_base/vbdb-public-check-selftest.XXXXXX")
broken_index=$(mktemp "$temp_root/broken-index.XXXXXX")
scanner_repo=$(mktemp -d "$temp_root/scanner.XXXXXX")
fixture="$scanner_repo/fixture"
isolated=$(mktemp -d "$temp_root/isolated.XXXXXX")
clean_repo=$(mktemp -d "$temp_root/clean.XXXXXX")
replace_repo=$(mktemp -d "$temp_root/replace.XXXXXX")
shallow_repo=$(mktemp -d "$temp_root/shallow.XXXXXX")
empty_repo=$(mktemp -d "$temp_root/empty.XXXXXX")
history_repo=$(mktemp -d "$temp_root/history.XXXXXX")
staged_repo=$(mktemp -d "$temp_root/staged.XXXXXX")
gitlink_repo=$(mktemp -d "$temp_root/gitlink.XXXXXX")
graft_repo=$(mktemp -d "$temp_root/graft.XXXXXX")
redirect_repo=$(mktemp -d "$temp_root/redirect.XXXXXX")
wrapper_bin=$(mktemp -d "$temp_root/bin.XXXXXX")
cleanup() {
	rm -rf -- "$temp_root"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Build the marker from fragments so this self-test does not itself contain a
# credential-looking signature that the publication gate could report.
marker_prefix='AK'
marker_suffix='IA0000000000000000'
token_suffix='abcdefghijklmnopqrstuvwxyz123456'
ghp_token=$(printf '%s%s%s' 'gh' 'p_' "$token_suffix")
gho_token=$(printf '%s%s%s' 'gh' 'o_' "$token_suffix")
ghu_token=$(printf '%s%s%s' 'gh' 'u_' "$token_suffix")
ghs_token=$(printf '%s%s%s' 'gh' 's_' "$token_suffix")
ghr_token=$(printf '%s%s%s' 'gh' 'r_' "$token_suffix")
aws_path=$(printf '%s%s' 'AKIA' '1234567890ABCDEF')
github_path="$ghp_token"
index_aws_path=$(printf '%s%s' 'ASIA' 'FEDCBA0987654321')
index_github_path="$ghu_token"
worktree_aws_path=$(printf '%s%s' 'AKIA' 'ABCDEF0123456789')
worktree_github_path="$ghs_token"
pgp_header=$(printf '%s%s%s%s' '-----BEGIN ' 'PGP ' 'PRIVATE KEY ' 'BLOCK-----')
encrypted_header=$(printf '%s%s%s' '-----BEGIN ' 'ENCRYPTED ' 'PRIVATE KEY-----')
pkcs8_header=$(printf '%s%s%s' '-----BEGIN ' 'PRIVATE KEY' '-----')
ssh2_header=$(printf '%s%s%s%s' '---- BEGIN ' 'SSH2 ENCRYPTED ' 'PRIVATE KEY ' '----')
lower_assignment_key=$(printf '%s%s' 'secret' '_key')
lower_assignment_value=$(printf '%s' 'abcdefghijklmnopqrstuvwxyz')
punct_assignment_value=$(printf '%s%s' '!' 'abcdefghijklmnopqrstuvwxyz')
colon_key=$(printf '%s' 'password')
colon_value=$(printf '%s' 'abcdefghijklmnopqrstuvwxyz123456')
quoted_colon_key=$(printf '%s' 'api_key')
quoted_colon_value=$(printf '%s' '0123456789abcdef0123456789abcdef')
netrc_name=$(printf '%s%s' '.' 'netrc')
git_credentials_name=$(printf '%s%s' '.git-' 'credentials')
npmrc_name=$(printf '%s%s' '.' 'npmrc')
pypirc_name=$(printf '%s%s' '.' 'pypirc')
unsafe_newline=$(printf 'unsafe\npath.txt')
unsafe_tab=$(printf 'unsafe\tpath.txt')
unsafe_escape=$(printf 'unsafe\033path.txt')
unsafe_bidi=$(printf 'unsafe\342\200\256path.txt')
unsafe_unicode=$(printf 'unsafe\303\251path.txt')
bash_version_helper=$(sed -n '/^require_bash4()/,/^}/p' "$root/scripts/public-check.sh")
if [ -z "$bash_version_helper" ] || bash -c "$bash_version_helper; require_bash4 3" >/dev/null 2>&1 || ! bash -c "$bash_version_helper; require_bash4 4"; then
	printf '%s\n' 'public-check-selftest: Bash-version guard regression' >&2
	exit 1
fi
mkdir -p "$scanner_repo/scripts" "$fixture"
cp "$root/scripts/public-check.sh" "$scanner_repo/scripts/public-check.sh"
git -C "$scanner_repo" init -q
git -C "$scanner_repo" add scripts/public-check.sh
printf '%s%s\n' "$marker_prefix" "$marker_suffix" > "$fixture/fixture.txt"
printf 'prefix\0%s%s\0%s\0suffix\n' "$marker_prefix" "$marker_suffix" "$pkcs8_header" > "$fixture/fixture.bin"
mkdir -p "$fixture/forms"
printf '%s\n' "$pgp_header" > "$fixture/forms/pgp.txt"
printf '%s\n' "$encrypted_header" > "$fixture/forms/encrypted.txt"
printf '%s\n' "$pkcs8_header" > "$fixture/forms/pkcs8.txt"
printf '%s\n' "$ssh2_header" > "$fixture/forms/ssh2.txt"
printf '%s=%s\n' "$lower_assignment_key" "$lower_assignment_value" > "$fixture/forms/lowercase-assignment.txt"
printf '%s=%s\n' "$lower_assignment_key" "$punct_assignment_value" > "$fixture/forms/punctuation-assignment.txt"
printf '%s: %s\n' "$colon_key" "$colon_value" > "$fixture/forms/colon-credential.txt"
printf '"%s": "%s"\n' "$quoted_colon_key" "$quoted_colon_value" > "$fixture/forms/quoted-colon-credential.txt"
printf '%s\n' "$ghp_token" > "$fixture/forms/ghp.txt"
printf '%s\n' "$gho_token" > "$fixture/forms/gho.txt"
printf '%s\n' "$ghu_token" > "$fixture/forms/ghu.txt"
printf '%s\n' "$ghs_token" > "$fixture/forms/ghs.txt"
printf '%s\n' "$ghr_token" > "$fixture/forms/ghr.txt"
printf '%s\n' "$marker_prefix$marker_suffix" > "$scanner_repo/$aws_path"
printf '%s\n' "$ghp_token" > "$scanner_repo/$github_path"
printf '%s\n' 'machine example.invalid login public-check password fixture' > "$scanner_repo/$netrc_name"
printf '%s\n' 'https://user:fixture@example.invalid/repository.git' > "$scanner_repo/$git_credentials_name"
printf '%s\n' '//registry.example.invalid/:_authToken=fixture' > "$scanner_repo/$npmrc_name"
printf '%s\n' '[distutils]\nindex-servers =\n    fixture' > "$scanner_repo/$pypirc_name"

set +e
output=$("$scanner_repo/scripts/public-check.sh" 2>&1)
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
for form in pgp encrypted pkcs8 ssh2 lowercase-assignment punctuation-assignment colon-credential quoted-colon-credential ghp gho ghu ghs ghr; do
	case "$output" in
		*"forms/$form.txt"*) ;;
		*)
			printf 'public-check-selftest: credential form %s was not rejected\n' "$form" >&2
			exit 1
			;;
	esac
done
for artifact in "$netrc_name" "$git_credentials_name" "$npmrc_name" "$pypirc_name"; do
	case "$output" in
		*"$artifact"*)
			printf 'public-check-selftest: credential artifact path %s leaked\n' "$artifact" >&2
			exit 1
			;;
		*) ;;
	esac
done
artifact_findings=$(LC_ALL=C grep -F -c 'high-risk private artifact filename' <<<"$output" || true)
if [ "$artifact_findings" -lt 4 ]; then
	printf '%s\n' 'public-check-selftest: common credential artifact filenames were not all rejected' >&2
	exit 1
fi
case "$output" in
	*'PATH:WORKTREE:'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: credential-shaped worktree path was not rejected' >&2
		exit 1
		;;
esac
for sensitive_path in "$aws_path" "$github_path"; do
	case "$output" in
		*"$sensitive_path"*)
			printf '%s\n' 'public-check-selftest: sensitive worktree path leaked' >&2
			exit 1
			;;
	esac
done
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: detector leaked fixture contents' >&2
		exit 1
		;;
esac
for secret in "$marker_prefix$marker_suffix" "$pgp_header" "$encrypted_header" "$pkcs8_header" "$ssh2_header" "$lower_assignment_key=$lower_assignment_value" "$lower_assignment_key=$punct_assignment_value" "$colon_key: $colon_value" "$quoted_colon_key: $quoted_colon_value" "$ghp_token" "$gho_token" "$ghu_token" "$ghs_token" "$ghr_token"; do
	case "$output" in
		*"$secret"*)
			printf '%s\n' 'public-check-selftest: detector leaked fixture contents' >&2
			exit 1
			;;
	esac
done

printf '%s\n' 'not-a-git-index' > "$broken_index"
real_git=$(command -v git)
printf '%s\n' '#!/usr/bin/env bash' 'for arg in "$@"; do' '  if [ "$arg" = ls-files ]; then exit 1; fi' 'done' 'exec "$PUBLIC_CHECK_REAL_GIT" "$@"' > "$wrapper_bin/git"
chmod 0755 "$wrapper_bin/git"
set +e
output=$(PATH="$wrapper_bin:$PATH" PUBLIC_CHECK_REAL_GIT="$real_git" GIT_INDEX_FILE="$broken_index" "$scanner_repo/scripts/public-check.sh" 2>&1)
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
printf '%s%s\n%s\n' "$marker_prefix" "$marker_suffix" "$pkcs8_header" > "$history_repo/history.txt"
printf '%s\n' 'safe historical path fixture' > "$history_repo/$aws_path"
printf '%s\n' 'safe historical path fixture' > "$history_repo/$github_path"
printf '%s\n' "$marker_prefix$marker_suffix" > "$history_repo/$unsafe_newline"
mkdir -p "$history_repo/secrets"
printf '%s\n' 'safe historical filename fixture' > "$history_repo/secrets/old.txt"
git -C "$history_repo" init -q
git -C "$history_repo" add scripts/public-check.sh history.txt secrets/old.txt -- "$aws_path" "$github_path" "$unsafe_newline"
git -C "$history_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm initial
printf '%s\n' 'safe worktree and index' > "$history_repo/history.txt"
git -C "$history_repo" add history.txt
git -C "$history_repo" rm -q secrets/old.txt
git -C "$history_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm clean
git -C "$history_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit --allow-empty -qm "message-only $gho_token"
message_commit=$(git -C "$history_repo" rev-parse HEAD)
base_branch=$(git -C "$history_repo" symbolic-ref --short HEAD)
git -C "$history_repo" checkout -q -b unpublished-secret
git -C "$history_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit --allow-empty -qm "branch-only $ghs_token"
branch_commit=$(git -C "$history_repo" rev-parse HEAD)
git -C "$history_repo" checkout -q "$base_branch"
secret_tag_name=$(printf '%s%s%s' 'gh' 'p_' "$token_suffix")
git -C "$history_repo" -c user.name=public-check -c user.email=public-check@example.invalid tag -a "$secret_tag_name" -m "tag-only $ghr_token $pkcs8_header"
tag_object=$(git -C "$history_repo" rev-parse "refs/tags/$secret_tag_name^{tag}")
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
	*"COMMIT:HISTORY:$message_commit"*) ;;
	*)
		printf '%s\n' 'public-check-selftest: base commit metadata was not scanned' >&2
		exit 1
		;;
esac
case "$output" in
	*"COMMIT:HISTORY:$branch_commit"*) ;;
	*)
		printf '%s\n' 'public-check-selftest: non-HEAD branch commit metadata was not scanned' >&2
		exit 1
		;;
esac
case "$output" in
	*"TAG:$tag_object"*) ;;
	*)
		printf '%s\n' 'public-check-selftest: annotated tag message was not scanned' >&2
		exit 1
		;;
esac
case "$output" in
	*'PATH:TREE:HISTORY:'*'high-risk private artifact filename'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: deleted history private path was not rejected' >&2
		exit 1
		;;
esac
for sensitive_path in "$aws_path" "$github_path" 'secrets/old.txt'; do
	case "$output" in
		*"$sensitive_path"*)
			printf '%s\n' 'public-check-selftest: sensitive historical path leaked' >&2
			exit 1
			;;
	esac
done
for unsafe_path in "$unsafe_newline"; do
	case "$output" in
		*"$unsafe_path"*)
			printf '%s\n' 'public-check-selftest: unsafe historical path leaked' >&2
			exit 1
			;;
	esac
done
case "$output" in
	*"REF:$tag_object"*) ;;
	*)
		printf '%s\n' 'public-check-selftest: credential-shaped ref name was not rejected' >&2
		exit 1
		;;
esac
case "$output" in
	*"refs/tags/$secret_tag_name"*)
		printf '%s\n' 'public-check-selftest: raw credential-shaped ref name leaked' >&2
		exit 1
		;;
esac
for secret in "$marker_prefix$marker_suffix" "$pkcs8_header" "$gho_token" "$ghs_token" "$ghr_token" "$secret_tag_name"; do
	case "$output" in
		*"$secret"*)
			printf '%s\n' 'public-check-selftest: committed fixture contents leaked' >&2
			exit 1
			;;
	esac
done

mkdir -p "$graft_repo/scripts"
cp "$root/scripts/public-check.sh" "$graft_repo/scripts/public-check.sh"
printf '%s\n' 'safe graft fixture' > "$graft_repo/data.txt"
git -C "$graft_repo" init -q
git -C "$graft_repo" add scripts/public-check.sh data.txt
git -C "$graft_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm initial
printf '%s\n' "$marker_prefix$marker_suffix" > "$graft_repo/ancestor.txt"
git -C "$graft_repo" add ancestor.txt
git -C "$graft_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm ancestor-secret
rm -f -- "$graft_repo/ancestor.txt"
git -C "$graft_repo" add -u ancestor.txt
git -C "$graft_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm clean-child
graft_child=$(git -C "$graft_repo" rev-parse HEAD)
graft_common_dir=$(git -C "$graft_repo" rev-parse --path-format=absolute --git-common-dir)
mkdir -p "$graft_common_dir/info"
printf '%s\n' "$graft_child" > "$graft_common_dir/info/grafts"
awk '
index($0, "if ! check_grafts") == 1 { skip=1; next }
skip && $0 == "fi" { skip=0; next }
!skip { print }
' "$graft_repo/scripts/public-check.sh" > "$graft_repo/scripts/public-check-unprotected.sh"
chmod 0755 "$graft_repo/scripts/public-check-unprotected.sh"
set +e
output=$(GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 "$graft_repo/scripts/public-check-unprotected.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -ne 0 ]; then
	printf 'public-check-selftest: unprotected graft scanner status = %s, want 0\n' "$check_status" >&2
	exit 1
fi
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: unprotected graft scanner unexpectedly found hidden ancestor' >&2
		exit 1
		;;
	*passed*) ;;
	*)
		printf '%s\n' 'public-check-selftest: unprotected graft scanner did not pass cleanly' >&2
		exit 1
		;;
esac
set +e
output=$(GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 "$graft_repo/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -ne 2 ]; then
	printf 'public-check-selftest: protected graft scanner status = %s, want 2\n' "$check_status" >&2
	exit 1
fi
case "$output" in
	*'legacy Git grafts'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: protected graft scanner did not reject grafts' >&2
		exit 1
		;;
esac
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: protected graft scanner leaked ancestor content' >&2
		exit 1
		;;
esac

mkdir -p "$gitlink_repo/scripts"
cp "$root/scripts/public-check.sh" "$gitlink_repo/scripts/public-check.sh"
git -C "$gitlink_repo" init -q
git -C "$gitlink_repo" add scripts/public-check.sh
git -C "$gitlink_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm initial
gitlink_target=$(git -C "$gitlink_repo" rev-parse HEAD)
git -C "$gitlink_repo" update-index --add --cacheinfo "160000,$gitlink_target,submodule"
git -C "$gitlink_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm gitlink
set +e
output=$("$gitlink_repo/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -ne 2 ]; then
	printf 'public-check-selftest: gitlink status = %s, want 2\n' "$check_status" >&2
	exit 1
fi
case "$output" in
	*'unsupported historical tree entry'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: historical gitlink was not rejected' >&2
		exit 1
		;;
esac

mkdir -p "$replace_repo/scripts"
cp "$root/scripts/public-check.sh" "$replace_repo/scripts/public-check.sh"
printf '%s\n' 'clean child fixture' > "$replace_repo/data.txt"
git -C "$replace_repo" init -q
git -C "$replace_repo" add scripts/public-check.sh data.txt
git -C "$replace_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm initial
base_commit=$(git -C "$replace_repo" rev-parse HEAD)
printf '%s\n' 'secret child fixture' > "$replace_repo/data.txt"
git -C "$replace_repo" add data.txt
git -C "$replace_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit --allow-empty -qm "$marker_prefix$marker_suffix"
secret_commit=$(git -C "$replace_repo" rev-parse HEAD)
printf '%s\n' 'clean child fixture' > "$replace_repo/data.txt"
git -C "$replace_repo" add data.txt
git -C "$replace_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm clean
base_tree=$(git -C "$replace_repo" rev-parse "$base_commit^{tree}")
clean_replacement=$(printf '%s\n' 'clean replacement object' | git -C "$replace_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit-tree "$base_tree" -p "$base_commit")
git -C "$replace_repo" replace "$secret_commit" "$clean_replacement"
if ! git -C "$replace_repo" replace -l | grep -F -q -- "$secret_commit"; then
	printf '%s\n' 'public-check-selftest: replace ref was not installed' >&2
	exit 1
fi
sed '/^export GIT_NO_REPLACE_OBJECTS=1$/d' "$root/scripts/public-check.sh" > "$replace_repo/scripts/public-check-no-replace.sh"
chmod 0755 "$replace_repo/scripts/public-check-no-replace.sh"
set +e
output=$(env -u GIT_NO_REPLACE_OBJECTS "$replace_repo/scripts/public-check-no-replace.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -ne 0 ]; then
	printf 'public-check-selftest: unprotected scanner status = %s, want 0\n' "$check_status" >&2
	exit 1
fi
case "$output" in
	*passed*) ;;
	*)
		printf '%s\n' 'public-check-selftest: unprotected scanner did not demonstrate the replacement bypass' >&2
		exit 1
		;;
esac
case "$output" in
	*"COMMIT:HISTORY:$secret_commit"*)
		printf '%s\n' 'public-check-selftest: unprotected scanner unexpectedly saw replaced history' >&2
		exit 1
		;;
esac
set +e
output=$(GIT_NO_REPLACE_OBJECTS=0 "$replace_repo/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -eq 0 ]; then
	printf '%s\n' 'public-check-selftest: replace-object history was not rejected' >&2
	exit 1
fi
case "$output" in
	*"COMMIT:HISTORY:$secret_commit"*) ;;
	*)
		printf '%s\n' 'public-check-selftest: original replaced commit was not scanned' >&2
		exit 1
		;;
esac
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: replace-object fixture contents leaked' >&2
		exit 1
		;;
esac

if ! git clone -q --depth=1 "file://$history_repo" "$shallow_repo"; then
	printf '%s\n' 'public-check-selftest: unable to create shallow clone' >&2
	exit 1
fi
set +e
output=$("$shallow_repo/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -ne 2 ]; then
	printf 'public-check-selftest: shallow repository status = %s, want 2\n' "$check_status" >&2
	exit 1
fi
case "$output" in
	*shallow*) ;;
	*)
		printf '%s\n' 'public-check-selftest: shallow repository was not rejected' >&2
		exit 1
		;;
esac

mkdir -p "$clean_repo/scripts" "$clean_repo/docs"
cp "$root/scripts/public-check.sh" "$clean_repo/scripts/public-check.sh"
printf '%s\n' 'Public contract prose mentions token names and private key examples.' > "$clean_repo/docs/contract.md"
git -C "$clean_repo" init -q
git -C "$clean_repo" add scripts/public-check.sh docs/contract.md
set +e
output=$("$clean_repo/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -ne 0 ]; then
	printf '%s\n' 'public-check-selftest: ordinary docs or scanner source false-positive' >&2
	exit 1
fi
case "$output" in
	*passed*) ;;
	*)
		printf '%s\n' 'public-check-selftest: clean repository did not pass' >&2
		exit 1
		;;
esac

mkdir -p "$redirect_repo/scripts"
cp "$root/scripts/public-check.sh" "$redirect_repo/scripts/public-check.sh"
printf '%s\n' 'safe redirect fixture' > "$redirect_repo/clean.txt"
printf '%s\n' "$marker_prefix$marker_suffix" > "$redirect_repo/redirect-secret.txt"
git -C "$redirect_repo" init -q
git -C "$redirect_repo" add scripts/public-check.sh clean.txt redirect-secret.txt
git -C "$redirect_repo" -c user.name=public-check -c user.email=public-check@example.invalid commit -qm redirect-secret
redirect_git_dir=$(git -C "$redirect_repo" rev-parse --path-format=absolute --git-dir)
set +e
output=$(GIT_DIR="$redirect_git_dir" \
	GIT_WORK_TREE="$redirect_repo" \
	GIT_COMMON_DIR="$redirect_git_dir" \
	GIT_INDEX_FILE="$redirect_git_dir/index" \
	GIT_OBJECT_DIRECTORY="$redirect_git_dir/objects" \
	GIT_ALTERNATE_OBJECT_DIRECTORIES="$redirect_git_dir/objects" \
	GIT_SHALLOW_FILE="$redirect_git_dir/shallow" \
	GIT_REPLACE_REF_BASE=refs/replace \
	GIT_GRAFT_FILE="$redirect_git_dir/info/grafts" \
	GIT_NAMESPACE=redirect \
	GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.worktree GIT_CONFIG_VALUE_0="$redirect_repo" \
	GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 \
	"$root/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -ne 0 ]; then
	printf 'public-check-selftest: redirected-environment scanner status = %s, want 0\n' "$check_status" >&2
	exit 1
fi
case "$output" in
	*"$marker_prefix$marker_suffix"*)
		printf '%s\n' 'public-check-selftest: repository-selection environment redirected the scan' >&2
		exit 1
		;;
	*passed*) ;;
	*)
		printf '%s\n' 'public-check-selftest: redirected-environment scanner did not pass cleanly' >&2
		exit 1
		;;
esac

mkdir -p "$staged_repo/scripts"
cp "$root/scripts/public-check.sh" "$staged_repo/scripts/public-check.sh"
printf '%s%s\n%s\n' "$marker_prefix" "$marker_suffix" "$pkcs8_header" > "$staged_repo/staged.txt"
printf '%s\n' "$marker_prefix$marker_suffix" > "$staged_repo/$index_aws_path"
printf '%s\n' "$ghu_token" > "$staged_repo/$index_github_path"
printf '%s\n' "$marker_prefix$marker_suffix" > "$staged_repo/$unsafe_tab"
printf '%s\n' 'safe staged private filename fixture' > "$staged_repo/id_rsa"
git -C "$staged_repo" init -q
git -C "$staged_repo" add scripts/public-check.sh staged.txt id_rsa -- "$index_aws_path" "$index_github_path" "$unsafe_tab"
rm -f -- "$staged_repo/staged.txt" "$staged_repo/id_rsa" "$staged_repo/$index_aws_path" "$staged_repo/$index_github_path" "$staged_repo/$unsafe_tab"
set +e
output=$("$staged_repo/scripts/public-check.sh" 2>&1)
check_status=$?
set -e
if [ "$check_status" -eq 0 ]; then
	printf '%s\n' 'public-check-selftest: staged secret was not rejected' >&2
	exit 1
fi
case "$output" in
	*'INDEX:'*'staged.txt'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: staged blob was not scanned' >&2
		exit 1
		;;
esac
case "$output" in
	*'PATH:INDEX:'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: credential-shaped index path was not rejected' >&2
		exit 1
		;;
esac
for sensitive_path in "$index_aws_path" "$index_github_path" id_rsa; do
	case "$output" in
		*"$sensitive_path"*)
			printf '%s\n' 'public-check-selftest: sensitive index path leaked' >&2
			exit 1
			;;
	esac
done
case "$output" in
	*"$unsafe_tab"*)
		printf '%s\n' 'public-check-selftest: unsafe index path leaked' >&2
		exit 1
		;;
esac
for secret in "$marker_prefix$marker_suffix" "$pkcs8_header"; do
	case "$output" in
		*"$secret"*)
			printf '%s\n' 'public-check-selftest: staged fixture contents leaked' >&2
			exit 1
			;;
	esac
done

mkdir -p "$isolated/scripts"
cp "$root/scripts/public-check.sh" "$isolated/scripts/public-check.sh"
mkdir -p "$isolated/deploy/.kube"
printf '%s\n' 'safe fixture' > "$isolated/deploy/.kube/config"
printf '%s\n' 'safe fixture' > "$isolated/kubeconfig"
printf '%s\n' 'safe fixture' > "$isolated/cluster.kubeconfig"
printf '%s\n' 'safe fixture' > "$isolated/.pgpass"
printf '%s\n' 'safe fixture' > "$isolated/.htpasswd"
printf '%s\n' 'safe fixture' > "$isolated/.dockercfg"
printf '%s\n' 'safe fixture' > "$isolated/terraform.tfstate"
printf '%s\n' 'safe fixture' > "$isolated/terraform.tfstate.backup"
mkdir -p "$isolated/.docker"
printf '%s\n' 'safe fixture' > "$isolated/.docker/config.json"
printf '%s\n' 'safe fixture' > "$isolated/id_rsa"
printf '%s\n' 'safe fixture' > "$isolated/secret.go"
printf '%s\n' 'safe fixture' > "$isolated/secrets.go"
printf '%s\n' 'safe fixture' > "$isolated/credentials.go"
printf '%s\n' 'safe fixture' > "$isolated/.env.sample"
printf '%s\n' 'safe fixture' > "$isolated/.env.template"
printf '%s\n' 'safe fixture' > "$isolated/id_ecdsa"
printf '%s\n' 'safe fixture' > "$isolated/id_dsa"
printf '%s\n' 'safe fixture' > "$isolated/encrypted.p8"
mkdir -p "$isolated/Deploy/.KUBE" "$isolated/PRIVATE"
printf '%s\n' 'safe fixture' > "$isolated/Deploy/.KUBE/Config"
printf '%s\n' 'safe fixture' > "$isolated/ID_RSA"
printf '%s\n' 'safe fixture' > "$isolated/Secrets"
printf '%s\n' 'safe fixture' > "$isolated/PRIVATE/Thing"
printf '%s\n' "$marker_prefix$marker_suffix" > "$isolated/$worktree_aws_path"
printf '%s\n' "$ghs_token" > "$isolated/$worktree_github_path"
printf '%s\n' "$marker_prefix$marker_suffix" > "$isolated/$unsafe_newline"
printf '%s\n' "$marker_prefix$marker_suffix" > "$isolated/$unsafe_tab"
printf '%s\n' "$marker_prefix$marker_suffix" > "$isolated/$unsafe_escape"
printf '%s\n' "$marker_prefix$marker_suffix" > "$isolated/$unsafe_bidi"
printf '%s\n' "$marker_prefix$marker_suffix" > "$isolated/$unsafe_unicode"
git -C "$isolated" init -q
git -C "$isolated" add scripts/public-check.sh deploy/.kube/config kubeconfig cluster.kubeconfig .pgpass .htpasswd .dockercfg terraform.tfstate terraform.tfstate.backup .docker/config.json id_rsa secret.go secrets.go credentials.go .env.sample .env.template id_ecdsa id_dsa encrypted.p8 Deploy/.KUBE/Config ID_RSA Secrets PRIVATE/Thing -- "$unsafe_newline" "$unsafe_tab" "$unsafe_escape" "$unsafe_bidi"
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
	*'WORKTREE:'*'high-risk private artifact filename'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: nested kube path was not rejected' >&2
		exit 1
		;;
esac
case "$output" in
	*'WORKTREE:'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: credential-shaped worktree path was not rejected' >&2
		exit 1
		;;
esac
high_risk_count=$(LC_ALL=C grep -F -c 'high-risk private artifact filename' <<<"$output" || true)
if [ "$high_risk_count" -lt 17 ]; then
	printf '%s\n' 'public-check-selftest: case-folded/nested high-risk paths were not all detected' >&2
	exit 1
fi
digested_high_risk_count=$(LC_ALL=C grep -E -c 'WORKTREE:[0-9a-f]{64}: high-risk private artifact filename' <<<"$output" || true)
if [ "$digested_high_risk_count" -lt 17 ]; then
	printf '%s\n' 'public-check-selftest: high-risk path labels were not uniformly digested' >&2
	exit 1
fi
for path in 'deploy/.kube/config' 'kubeconfig' 'cluster.kubeconfig' '.pgpass' '.htpasswd' '.dockercfg' 'terraform.tfstate' 'terraform.tfstate.backup' '.docker/config.json' 'id_rsa' 'id_ecdsa' 'id_dsa' 'encrypted.p8' 'Deploy/.KUBE/Config' 'ID_RSA' 'Secrets' 'PRIVATE/Thing'; do
	case "$output" in
		*"$path"*)
			printf 'public-check-selftest: sensitive worktree path %s was not detected or redacted\n' "$path" >&2
			exit 1
			;;
		*) ;;
	esac
done
for path in "$worktree_aws_path" "$worktree_github_path" secret.go secrets.go credentials.go .env.sample .env.template; do
	case "$output" in
		*"$path"*)
			printf 'public-check-selftest: path %s was leaked or falsely rejected\n' "$path" >&2
			exit 1
			;;
	esac
done
for unsafe_path in "$unsafe_newline" "$unsafe_tab" "$unsafe_escape" "$unsafe_bidi" "$unsafe_unicode"; do
	case "$output" in
		*"$unsafe_path"*)
			printf '%s\n' 'public-check-selftest: unsafe worktree path leaked' >&2
			exit 1
			;;
	esac
done
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

for invalid_tmp in relative-tmp "$root"; do
	set +e
	output=$(TMPDIR="$invalid_tmp" "$root/scripts/public-check.sh" 2>&1)
	check_status=$?
	set -e
	if [ "$check_status" -ne 2 ]; then
		printf '%s\n' 'public-check-selftest: unsafe scanner temporary directory was accepted' >&2
		exit 1
	fi
	case "$output" in
		*passed*)
			printf '%s\n' 'public-check-selftest: unsafe scanner temporary directory reported success' >&2
			exit 1
			;;
	esac
done
printf '%s\n' 'public-check-selftest: passed'
