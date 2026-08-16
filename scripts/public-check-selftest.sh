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
printf '%s\n' "$ghp_token" > "$fixture/forms/ghp.txt"
printf '%s\n' "$gho_token" > "$fixture/forms/gho.txt"
printf '%s\n' "$ghu_token" > "$fixture/forms/ghu.txt"
printf '%s\n' "$ghs_token" > "$fixture/forms/ghs.txt"
printf '%s\n' "$ghr_token" > "$fixture/forms/ghr.txt"
printf '%s\n' "$marker_prefix$marker_suffix" > "$scanner_repo/$aws_path"
printf '%s\n' "$ghp_token" > "$scanner_repo/$github_path"

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
for form in pgp encrypted pkcs8 ssh2 ghp gho ghu ghs ghr; do
	case "$output" in
		*"forms/$form.txt"*) ;;
		*)
			printf 'public-check-selftest: credential form %s was not rejected\n' "$form" >&2
			exit 1
			;;
	esac
done
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
for secret in "$marker_prefix$marker_suffix" "$pgp_header" "$encrypted_header" "$pkcs8_header" "$ssh2_header" "$ghp_token" "$gho_token" "$ghu_token" "$ghs_token" "$ghr_token"; do
	case "$output" in
		*"$secret"*)
			printf '%s\n' 'public-check-selftest: detector leaked fixture contents' >&2
			exit 1
			;;
	esac
done

printf '%s\n' 'not-a-git-index' > "$broken_index"
set +e
output=$(GIT_INDEX_FILE="$broken_index" "$scanner_repo/scripts/public-check.sh" 2>&1)
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
mkdir -p "$history_repo/secrets"
printf '%s\n' 'safe historical filename fixture' > "$history_repo/secrets/old.txt"
git -C "$history_repo" init -q
git -C "$history_repo" add scripts/public-check.sh history.txt secrets/old.txt -- "$aws_path" "$github_path"
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

mkdir -p "$staged_repo/scripts"
cp "$root/scripts/public-check.sh" "$staged_repo/scripts/public-check.sh"
printf '%s%s\n%s\n' "$marker_prefix" "$marker_suffix" "$pkcs8_header" > "$staged_repo/staged.txt"
printf '%s\n' "$marker_prefix$marker_suffix" > "$staged_repo/$index_aws_path"
printf '%s\n' "$ghu_token" > "$staged_repo/$index_github_path"
printf '%s\n' 'safe staged private filename fixture' > "$staged_repo/id_rsa"
git -C "$staged_repo" init -q
git -C "$staged_repo" add scripts/public-check.sh staged.txt id_rsa -- "$index_aws_path" "$index_github_path"
rm -f -- "$staged_repo/staged.txt" "$staged_repo/id_rsa" "$staged_repo/$index_aws_path" "$staged_repo/$index_github_path"
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
git -C "$isolated" init -q
git -C "$isolated" add scripts/public-check.sh deploy/.kube/config id_rsa secret.go secrets.go credentials.go .env.sample .env.template id_ecdsa id_dsa encrypted.p8 Deploy/.KUBE/Config ID_RSA Secrets PRIVATE/Thing
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
	*'PATH:WORKTREE:'*'high-risk private artifact filename'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: nested kube path was not rejected' >&2
		exit 1
		;;
esac
case "$output" in
	*'PATH:WORKTREE:'*) ;;
	*)
		printf '%s\n' 'public-check-selftest: credential-shaped worktree path was not rejected' >&2
		exit 1
		;;
esac
for path in 'deploy/.kube/config' 'id_rsa' 'id_ecdsa' 'id_dsa' 'encrypted.p8' 'Deploy/.KUBE/Config' 'ID_RSA' 'Secrets' 'PRIVATE/Thing'; do
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
