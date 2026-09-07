#!/usr/bin/env bash
# Compatibility boundary for the frozen built-in provider and Codex 0.153.4.
set -euo pipefail
native=/usr/local/libexec/gascity-codex-native
if [[ "${CODEX_COMPAT_TESTING:-}" == 1 ]]; then
  native="${CODEX_COMPAT_NATIVE:?CODEX_COMPAT_NATIVE is required in testing mode}"
fi
args=()
while (($#)); do
  case "$1" in
    --ask-for-approval)
      (($# >= 2)) || { echo 'codex compatibility: --ask-for-approval requires a mode' >&2; exit 64; }
      case "$2" in
        untrusted) args+=(--config 'approval_policy="on-request"') ;;
        never) args+=(--config 'approval_policy="never"') ;;
        *) echo "codex compatibility: unsupported approval mode: $2" >&2; exit 64 ;;
      esac
      shift 2 ;;
    --ask-for-approval=untrusted) args+=(--config 'approval_policy="on-request"'); shift ;;
    --ask-for-approval=never) args+=(--config 'approval_policy="never"'); shift ;;
    *) args+=("$1"); shift ;;
  esac
done
exec "$native" "${args[@]}"
