#!/usr/bin/env bash
# Permanent real-binary contract for the Gas City agent image.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
image="${1:?usage: test-codex-runtime.sh IMAGE}"
version_file="$root/contrib/k8s/codex-runtime/version.env"

source "$version_file"
[[ "$(wc -l < "$version_file")" -eq 1 ]] || { echo "invalid Codex version file" >&2; exit 1; }
[[ "$CODEX_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "invalid Codex version" >&2; exit 1; }

docker run --rm \
  -e "EXPECTED_CODEX_VERSION=$CODEX_VERSION" \
  --entrypoint /bin/sh "$image" -c '
    set -eu
    root=/opt/gascity-codex/standalone
    test -f "$root/codex-package.json"
    test -x "$root/bin/codex"
    test -x "$root/bin/codex-code-mode-host"
    test -x "$root/codex-path/rg"
    test -x "$root/codex-resources/bwrap"
    test "$("$root/bin/codex" --version)" = "codex-cli $EXPECTED_CODEX_VERSION"
    test "$(codex --version)" = "codex-cli $EXPECTED_CODEX_VERSION"
    codex exec --sandbox read-only --ask-for-approval untrusted --help >/dev/null
    ! command -v node >/dev/null 2>&1
    ! command -v npm >/dev/null 2>&1
  '

# Exercise the bundled native app-server. The synthetic key is generated in
# the disposable container and is never emitted by this script.
docker run --rm \
  -e "OPENAI_BASE_URL=http://127.0.0.1:9" \
  --entrypoint python3 "$image" - <<'PY'
import json
import os
from pathlib import Path
import subprocess
import tempfile


def result(message):
    value = message.get("result", message)
    return value if isinstance(value, dict) else {}


def account_type(message):
    value = result(message)
    account = value.get("account")
    if not isinstance(account, dict):
        account = value
    raw = account.get("type", account.get("accountType"))
    return raw.strip().lower().replace("_", "-") if isinstance(raw, str) else ""


with tempfile.TemporaryDirectory(prefix="gascity-codex-contract-") as home:
    code_home = Path(home)
    (code_home / "config.toml").write_text('cli_auth_credentials_store = "file"\n', encoding="utf-8")
    os.chmod(code_home, 0o700)
    environment = os.environ.copy()
    environment["CODEX_HOME"] = str(code_home)
    key = "sk-contract-" + os.urandom(24).hex()
    process = subprocess.Popen(
        ["codex", "app-server", "--listen", "stdio://"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=environment,
    )
    sent_methods = []
    received = []
    next_id = [1]

    def send(method, params=None, notification=False):
        payload = {"jsonrpc": "2.0", "method": method}
        if not notification:
            payload["id"] = next_id[0]
            next_id[0] += 1
        if params is not None:
            payload["params"] = params
        sent_methods.append(method)
        process.stdin.write((json.dumps(payload, separators=(",", ":")) + "\n").encode())
        process.stdin.flush()
        if notification:
            return {}
        while True:
            line = process.stdout.readline()
            if not line:
                raise RuntimeError("Codex app-server closed before its response")
            message = json.loads(line)
            received.append(message)
            if message.get("id") == payload["id"]:
                if "error" in message:
                    raise RuntimeError("Codex app-server rejected a contract request")
                return message

    try:
        send("initialize", {"clientInfo": {"name": "gascity-codex-runtime-contract", "version": "1"}})
        send("initialized", notification=True)
        send("account/login/start", {"type": "apiKey", "apiKey": key})
        account = send("account/read", {})
        assert account_type(account) in {"apikey", "api-key", "openai-api-key", "openai-apikey"}
        auth_file = code_home / "auth.json"
        assert auth_file.is_file() and auth_file.stat().st_size > 0
        assert sent_methods == ["initialize", "initialized", "account/login/start", "account/read"]
    finally:
        process.terminate()
        stdout, stderr = process.communicate(timeout=10)
    assert key.encode() not in stdout
    assert key.encode() not in stderr
PY

echo "PASS: Codex standalone layout, compatibility argv, no Node/npm, and app-server API-key persistence (${CODEX_VERSION})"
