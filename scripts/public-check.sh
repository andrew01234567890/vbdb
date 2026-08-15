#!/usr/bin/env bash
# Check the files that would be included in a normal Git publication. This
# intentionally prints only paths and finding categories, never matched text.
set -u

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if ! git -C "$root" rev-parse --git-dir >/dev/null 2>&1; then
	printf '%s\n' 'public-check: not inside a Git repository' >&2
	exit 2
fi

status=0
own_script='scripts/public-check.sh'

report() {
	printf 'public-check: %s: %s\n' "$1" "$2"
	status=1
}

# These are names, not content matches. Examples and source files remain
# publishable, while common local credential and private-material paths do not.
is_high_risk_name() {
	local path=$1 base=${1##*/}
	case "$path" in
		secrets/*|private/*|credentials/*|.codex/*|.claude/*|.aws/*|.ssh/*|.kube/config|*/secrets/*|*/private/*|*/credentials/*|*/.codex/*|*/.claude/*|*/.aws/*|*/.ssh/*)
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

# Keep this list narrow: ordinary words such as "token" and public header
# documentation must not be treated as credentials.
credential_pattern='-----BEGIN (RSA|EC|OPENSSH|DSA|PGP)? ?PRIVATE KEY-----|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[0-9A-Za-z-]{10,}|[A-Z][A-Z0-9_]*(SECRET|PASSWORD|PRIVATE_KEY)[A-Z0-9_]*[[:space:]]*=[[:space:]]*[^[:space:]#]{8,}'

while IFS= read -r -d '' path; do
	rel=$path
	[ "$rel" = "$own_script" ] && continue
	if is_high_risk_name "$rel"; then
		report "$rel" 'high-risk private artifact filename'
		continue
	fi

	file="$root/$rel"
	if [ ! -f "$file" ]; then
		continue
	fi
	grep -E -I -q -- "$credential_pattern" "$file"
	grep_status=$?
	case "$grep_status" in
		0) report "$rel" 'credential or private-key signature' ;;
		1) ;;
		*) report "$rel" 'unable to scan file' ;;
	esac
done < <(git -C "$root" ls-files --cached --others --exclude-standard -z)

if [ "$status" -eq 0 ]; then
	printf '%s\n' 'public-check: passed'
else
	printf '%s\n' 'public-check: failed; inspect the reported paths before publication' >&2
fi
exit "$status"
