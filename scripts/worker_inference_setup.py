#!/usr/bin/env python3

import argparse
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


NPM_PACKAGE_BY_PROVIDER = {
    "gemini": ("@google/gemini-cli", "GEMINI_CLI_VERSION", "0.40.0"),
    "mimocode": ("@mimo-ai/cli", "MIMOCODE_CLI_VERSION", "0.1.0"),
    "opencode": ("opencode-ai", "OPENCODE_CLI_VERSION", "1.14.33"),
    "pi": ("@earendil-works/pi-coding-agent", "PI_CODING_AGENT_VERSION", "0.74.0"),
}
# Providers whose installed binary name differs from the provider name.
BINARY_BY_PROVIDER = {
    "cursor": "cursor-agent",
    "mimocode": "mimo",
    "zcode": "zcode-repl",
}
# ZCode ships no public TUI, so there is nothing to npm-install: the pane runs
# the engine's own adapter. "Installing" it means copying the vendored script
# onto PATH.
ZCODE_ADAPTER_RELPATH = ("internal", "worker", "adapters", "zcode", "zcode-repl")
CLAUDE_CODE_VERSION = "2.1.123"
KIMI_CLI_VERSION = "1.42.0"
PI_OLLAMA_CLOUD_VERSION = "0.4.1"
REPO_ROOT = Path(__file__).resolve().parents[1]
CODEX_VERSION_FILE = REPO_ROOT / "contrib" / "k8s" / "codex-runtime" / "version.env"
CODEX_STANDALONE_INSTALLER = REPO_ROOT / "contrib" / "k8s" / "install-codex-standalone.sh"
CODEX_TARGET = "x86_64-unknown-linux-musl"


def codex_default_version() -> str:
    try:
        lines = CODEX_VERSION_FILE.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        raise SystemExit(f"could not read canonical Codex version file: {CODEX_VERSION_FILE}") from exc
    if len(lines) != 1 or not re.fullmatch(r"CODEX_VERSION=[0-9]+\.[0-9]+\.[0-9]+", lines[0]):
        raise SystemExit(f"canonical Codex version file is not exactly one stable version input: {CODEX_VERSION_FILE}")
    value = lines[0].split("=", 1)[1]
    return value


def install_codex_standalone(version: str, force: bool) -> int:
    root = Path(os.environ.get("CODEX_STANDALONE_ROOT", Path.home() / ".local" / "share" / "gascity-codex"))
    destination = root / version
    native = destination / "bin" / "codex"
    bin_dir = Path(os.environ.get("CODEX_STANDALONE_BIN_DIR", Path.home() / ".local" / "bin"))
    link = bin_dir / "codex"

    if not force and native.is_file() and os.access(native, os.X_OK):
        if link.is_symlink() and link.resolve() == native.resolve():
            print(f"codex standalone {version} already installed at {destination}; skipping install")
            return 0
    root.parent.mkdir(parents=True, exist_ok=True)
    temporary_parent = Path(tempfile.mkdtemp(prefix=f".{version}.", dir=root.parent))
    temporary = temporary_parent / "package"
    try:
        subprocess.run(
            [
                str(CODEX_STANDALONE_INSTALLER),
                "--version", version,
                "--target", CODEX_TARGET,
                "--destination", str(temporary),
            ],
            check=True,
        )
        if destination.exists():
            shutil.rmtree(destination)
        os.replace(temporary, destination)
    finally:
        shutil.rmtree(temporary_parent, ignore_errors=True)

    bin_dir.mkdir(parents=True, exist_ok=True)
    if link.exists() or link.is_symlink():
        if not force and not link.is_symlink():
            raise SystemExit(f"{link} already exists; use --force to replace it with the standalone Codex binary")
        link.unlink()
    link.symlink_to(native)
    if shutil.which("codex") is None:
        raise SystemExit(f"{link} is not on PATH; add {bin_dir} before running Codex worker inference")
    print(f"installed codex standalone {version} to {destination}")
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)
    install = subparsers.add_parser("install")
    install.add_argument("--profile", required=True)
    install.add_argument("--force", action="store_true")
    return parser.parse_args()


def install_zcode_adapter(force: bool) -> int:
    """Copy the engine's zcode adapter onto PATH.

    The bin dir defaults to ~/.local/bin (ZCODE_ADAPTER_BIN_DIR overrides it),
    which must already be on PATH for the live suite's exec.LookPath to find it.
    """
    binary = BINARY_BY_PROVIDER["zcode"]
    existing = shutil.which(binary)
    repo_root = Path(__file__).resolve().parents[1]
    source = repo_root.joinpath(*ZCODE_ADAPTER_RELPATH)
    if not source.is_file():
        raise SystemExit(f"zcode adapter not found at {source}")

    bin_dir = Path(os.environ.get("ZCODE_ADAPTER_BIN_DIR", Path.home() / ".local" / "bin"))
    target = bin_dir / binary
    if existing and not force and Path(existing) != target:
        print(f"{binary} already present at {existing}; skipping install")
        return 0

    bin_dir.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, target)
    target.chmod(0o755)
    print(f"installed {binary} to {target}")

    if not shutil.which(binary):
        raise SystemExit(f"{target} is not on PATH; add {bin_dir} to PATH before running zcode/tmux-cli worker inference")
    return 0


def main() -> int:
    args = parse_args()
    if args.command != "install":
        raise SystemExit(f"unsupported command: {args.command}")
    provider = args.profile.split("/", 1)[0].strip().lower()
    if provider not in {"claude", "codex", "cursor", "kimi", "antigravity", "zcode", *NPM_PACKAGE_BY_PROVIDER}:
        raise SystemExit(f"unsupported worker-inference profile: {args.profile!r}")
    if provider == "codex":
        version = os.environ.get("CODEX_CLI_VERSION", codex_default_version())
        return install_codex_standalone(version, args.force)
    if provider == "cursor":
        binary = BINARY_BY_PROVIDER[provider]
        if not shutil.which(binary):
            raise SystemExit(
                f"{binary} was not found in PATH; install Cursor Agent CLI before running "
                f"{args.profile} worker inference"
            )
        print(f"{binary} already present in PATH; skipping install")
        return 0
    if provider == "antigravity":
        if not shutil.which("agy"):
            raise SystemExit("agy was not found in PATH; install Antigravity CLI before running antigravity/tmux-cli worker inference")
        print("agy already present in PATH; skipping install")
        return 0
    if provider == "zcode":
        return install_zcode_adapter(args.force)
    binary = BINARY_BY_PROVIDER.get(provider, provider)
    already_present = shutil.which(binary) is not None
    if already_present and not args.force and provider != "pi":
        print(f"{binary} already present in PATH; skipping install")
        return 0

    if provider == "claude":
        version = os.environ.get("CLAUDE_CODE_VERSION", CLAUDE_CODE_VERSION)
        installer = REPO_ROOT / ".github" / "scripts" / "install-claude-native.sh"
        subprocess.run([str(installer), version], check=True)
    elif provider == "kimi":
        version = os.environ.get("KIMI_CLI_VERSION", KIMI_CLI_VERSION)
        subprocess.run(["uv", "tool", "install", "--python", "3.13", f"kimi-cli=={version}"], check=True)
    else:
        package, env_var, default_version = NPM_PACKAGE_BY_PROVIDER[provider]
        if provider == "codex":
            default_version = codex_default_version()
        version = os.environ.get(env_var, default_version)
        if not already_present or args.force:
            subprocess.run(["npm", "install", "-g", f"{package}@{version}"], check=True)
        if provider == "pi":
            plugin_version = os.environ.get("PI_OLLAMA_CLOUD_VERSION", PI_OLLAMA_CLOUD_VERSION)
            subprocess.run(["pi", "install", f"npm:pi-ollama-cloud@{plugin_version}"], check=True)

    if not shutil.which(binary):
        raise SystemExit(f"{binary} was not found in PATH after installation")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
