#!/usr/bin/env python3

import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "scripts" / "worker_inference_setup.py"


def load_setup_module():
    spec = importlib.util.spec_from_file_location("worker_inference_setup", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"could not load {MODULE_PATH}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class WorkerInferenceSetupTest(unittest.TestCase):
    def test_codex_clean_install_creates_missing_root_and_reuses_it(self):
        setup = load_setup_module()
        with tempfile.TemporaryDirectory(prefix="gascity-codex-install-") as temporary:
            home = Path(temporary) / "home"
            root = home / ".local" / "share" / "gascity-codex"
            bin_dir = home / ".local" / "bin"
            installer = Path(temporary) / "fake-codex-installer"
            installer_log = Path(temporary) / "installer.log"
            npm_log = Path(temporary) / "npm.log"
            installer.write_text(
                """#!/bin/sh
set -eu
destination=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = --destination ]; then destination="$2"; shift 2; else shift; fi
done
printf '%s\\n' "$destination" >> "$FAKE_INSTALLER_LOG"
/bin/mkdir -p "$destination/bin"
printf '#!/bin/sh\\nexit 0\\n' > "$destination/bin/codex"
/bin/chmod 0755 "$destination/bin/codex"
""",
                encoding="utf-8",
            )
            installer.chmod(0o755)
            fake_npm = bin_dir / "npm"
            bin_dir.mkdir(parents=True)
            fake_npm.write_text(
                "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_NPM_LOG\"\nexit 99\n",
                encoding="utf-8",
            )
            fake_npm.chmod(0o755)

            environment = {
                "HOME": str(home),
                "PATH": str(bin_dir),
                "CODEX_STANDALONE_ROOT": str(root),
                "CODEX_STANDALONE_BIN_DIR": str(bin_dir),
                "FAKE_INSTALLER_LOG": str(installer_log),
                "FAKE_NPM_LOG": str(npm_log),
            }
            with mock.patch.dict(os.environ, environment, clear=False):
                self.assertEqual(setup.install_codex_standalone("9.8.7", False, installer), 0)
                native = root / "9.8.7" / "bin" / "codex"
                link = bin_dir / "codex"
                self.assertTrue(native.is_file())
                self.assertTrue(link.is_symlink())
                self.assertEqual(link.resolve(), native.resolve())

                self.assertEqual(setup.install_codex_standalone("9.8.7", False, installer), 0)

            self.assertEqual(len(installer_log.read_text(encoding="utf-8").splitlines()), 1)
            self.assertFalse(npm_log.exists(), "Codex standalone installation must not invoke npm")


if __name__ == "__main__":
    unittest.main()
