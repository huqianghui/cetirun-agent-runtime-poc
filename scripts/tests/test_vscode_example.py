# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import importlib.util
import io
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
EXAMPLE = ROOT / "examples/vscode-server"
spec = importlib.util.spec_from_file_location("vscode_install", EXAMPLE / "install.py")
INSTALL = importlib.util.module_from_spec(spec)
spec.loader.exec_module(INSTALL)


class VSCodeExampleTests(unittest.TestCase):
    def test_official_artifacts_have_immutable_architecture_specific_pins(self):
        lock = json.loads((EXAMPLE / "vscode-lock.json").read_text())
        self.assertRegex(lock["commit"], r"^[a-f0-9]{40}$")
        digests = []
        for arch in ("amd64", "arm64"):
            for kind in ("cli", "server"):
                artifact = lock["artifacts"][arch][kind]
                self.assertRegex(artifact["sha256"], r"^[a-f0-9]{64}$")
                digests.append(artifact["sha256"])
        self.assertEqual(len(set(digests)), 4)

    def test_installer_refuses_existing_data_before_network(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(INSTALL.urllib.request, "urlopen") as request:
            output = Path(directory)
            (output / "keep").write_text("user data")
            with self.assertRaisesRegex(ValueError, "empty output"):
                INSTALL.install("amd64", output, EXAMPLE / "vscode-lock.json")
            request.assert_not_called()
            self.assertEqual((output / "keep").read_text(), "user data")

    def test_checksum_failure_cannot_extract_or_execute_an_artifact(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(INSTALL.urllib.request, "urlopen", return_value=io.BytesIO(b"synthetic bad archive")), patch.object(INSTALL.tarfile, "open") as unpack:
            with self.assertRaisesRegex(ValueError, "Checksum mismatch"):
                INSTALL.install("amd64", Path(directory), EXAMPLE / "vscode-lock.json")
            unpack.assert_not_called()
            self.assertEqual(list(Path(directory).iterdir()), [])

    def test_build_requires_license_acceptance_and_retains_nonroot_entrypoint(self):
        source = (EXAMPLE / "Dockerfile").read_text()
        self.assertIn("ARG ACCEPT_VSCODE_SERVER_LICENSE_TERMS=false", source)
        self.assertIn('test "$ACCEPT_VSCODE_SERVER_LICENSE_TERMS" = true', source)
        self.assertIn("USER 1024:1024", source)
        self.assertIn("LicenseRef-Microsoft-VS-Code-Server", source)
        self.assertNotIn("npm install", source)

    @unittest.skipUnless(shutil.which("node"), "Node.js is required for the browser adapter tests")
    def test_source_export_runs_without_repository_state_or_accounts(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-vscode-export-") as directory:
            target = Path(directory) / "vscode-server"
            shutil.copytree(EXAMPLE, target)
            result = subprocess.run(["node", "--test", str(target / "test/runtime.test.mjs")],
                                    text=True, capture_output=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
