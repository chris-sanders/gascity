#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/native" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@"
EOF
chmod +x "$tmp/native"
out="$(CODEX_COMPAT_TESTING=1 CODEX_COMPAT_NATIVE="$tmp/native" "$root/codex-compat.sh" exec --sandbox read-only --ask-for-approval untrusted --help)"
test "$out" = $'exec\n--sandbox\nread-only\n--config\napproval_policy="on-request"\n--help'
out="$(CODEX_COMPAT_TESTING=1 CODEX_COMPAT_NATIVE="$tmp/native" "$root/codex-compat.sh" exec --ask-for-approval=never --help)"
test "$out" = $'exec\n--config\napproval_policy="never"\n--help'
