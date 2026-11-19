# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline checks for the independent terminal image and its build entrypoint."""

import hashlib
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import threading
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/terminal-docker-build.sh"


class TerminalImageTests(unittest.TestCase):
    def run_download(self, statuses, *, valid_checksum=True):
        image = (ROOT / "app/cetirun-operator/Dockerfile.terminal").read_text()
        command = "curl " + image.split("    curl ", 1)[1].split("\n\nFROM ", 1)[0]
        command = command.replace("\\\n", "")
        source = re.search(r'"https://[^\"]+"', command).group()
        payload = b"synthetic ttyd binary\n"
        attempts = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                status = statuses[min(len(attempts), len(statuses) - 1)]
                attempts.append(status)
                body = payload if status == 200 else b"synthetic download failure\n"
                self.send_response(status)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_):
                pass

        with tempfile.TemporaryDirectory(prefix="cetirun-terminal-download-") as temp:
            output = Path(temp) / "ttyd"
            with HTTPServer(("127.0.0.1", 0), Handler) as server:
                thread = threading.Thread(target=server.serve_forever, daemon=True)
                thread.start()
                try:
                    command = command.replace(source, f'"http://127.0.0.1:{server.server_port}/tool"')
                    command = command.replace("/ttyd", str(output)).replace("curl ", "curl -q ", 1)
                    result = subprocess.run(
                        ["sh", "-c", command], capture_output=True, text=True, timeout=75,
                        env={
                            **os.environ,
                            "NO_PROXY": "127.0.0.1", "no_proxy": "127.0.0.1",
                            "terminal_sha": hashlib.sha256(payload).hexdigest() if valid_checksum else "0" * 64,
                        },
                    )
                finally:
                    server.shutdown()
                    thread.join(timeout=5)
                    self.assertFalse(thread.is_alive(), "loopback server did not stop")
            if result.returncode == 0:
                self.assertEqual(output.read_bytes(), payload)
                self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o755)
            elif output.exists():
                self.assertEqual(output.stat().st_mode & 0o111, 0)
        return result, attempts

    def test_download_recovers_from_gateway_timeout(self):
        result, attempts = self.run_download([504, 200])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(attempts, [504, 200])

    def test_download_retry_exhaustion_still_fails(self):
        result, attempts = self.run_download([504])
        self.assertEqual(result.returncode, 22, result.stderr)
        self.assertEqual(attempts, [504] * 6)

    def test_permanent_download_error_is_not_retried(self):
        result, attempts = self.run_download([404])
        self.assertEqual(result.returncode, 22, result.stderr)
        self.assertEqual(attempts, [404])

    def test_download_checksum_mismatch_prevents_executable_install(self):
        result, attempts = self.run_download([200], valid_checksum=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(attempts, [200])
        self.assertIn("FAILED", result.stdout)

    def test_mcr_release_builds_and_publishes_terminal(self):
        pipeline = (ROOT / ".pipelines/release-mcr.yml").read_text()
        inventory = json.loads((ROOT / "scripts/release-artifacts.json").read_text())
        terminal = next(entry for entry in inventory["images"] if entry["name"] == "cetirun-terminal")
        self.assertEqual(terminal["script"], "scripts/terminal-docker-build.sh")
        self.assertIn("scripts/cetirun-release.py publish-images", pipeline)
        self.assertIn("scripts/cetirun-release.py list images", pipeline)

    def test_help_does_not_require_docker_or_network(self):
        result = subprocess.run(
            ["/bin/bash", str(SCRIPT), "--help"],
            env={"PATH": "/not-installed"},
            capture_output=True, text=True, check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("PUSH defaults to false", result.stdout)

    def test_invalid_inputs_fail_before_docker(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-terminal-invalid-") as temp:
            docker = Path(temp) / "docker"
            docker.write_text("#!/bin/sh\nexit 99\n")
            docker.chmod(0o755)
            for key, value in (
                ("PUSH", "maybe"), ("PLATFORM", "linux/unknown"),
                ("REGISTRY", "https://registry.example.com"), ("IMAGE_TAG", "bad tag"),
            ):
                env = {
                    **os.environ, "PATH": f"{temp}:{os.environ['PATH']}",
                    "VERSION": "0.1.1", "GIT_SHA": "synthetic",
                    "REGISTRY": "registry.example.com", "PUSH": "false",
                    "PLATFORM": "linux/amd64", "IMAGE_TAG": "0.1.1", key: value,
                }
                result = subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 2, result.stderr)

    def test_build_uses_dedicated_dockerfile_and_does_not_push(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-terminal-build-") as temp:
            directory = Path(temp)
            log = directory / "args.jsonl"
            docker = directory / "docker"
            docker.write_text(
                "#!/usr/bin/env python3\nimport json, os, sys\n"
                "with open(os.environ['TERMINAL_TEST_LOG'], 'a') as output:\n"
                "    output.write(json.dumps(sys.argv[1:]) + '\\n')\n"
            )
            docker.chmod(0o755)
            env = {**os.environ, "PATH": f"{temp}:{os.environ['PATH']}",
                   "VERSION": "0.1.1", "GIT_SHA": "synthetic", "REGISTRY": "registry.example.com",
                   "PUSH": "false", "PLATFORM": "linux/amd64", "TERMINAL_TEST_LOG": str(log)}
            result = subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            commands = [json.loads(line) for line in log.read_text().splitlines()]
            build = commands[-1]
            self.assertEqual(build[:2], ["buildx", "build"])
            self.assertIn(str(ROOT / "app/cetirun-operator/Dockerfile.terminal"), build)
            self.assertIn("registry.example.com/cetirun-terminal:0.1.1", build)
            self.assertIn("--load", build)
            self.assertNotIn("--push", build)

    def test_image_is_independent_and_preserves_upstream_license(self):
        image = (ROOT / "app/cetirun-operator/Dockerfile.terminal").read_text()
        self.assertIn('ENTRYPOINT ["/terminal"]', image)
        self.assertIn('CMD ["serve"]', image)
        self.assertIn("sha256sum --check", image)
        self.assertNotIn("/manager", image)
        self.assertTrue((ROOT / "app/cetirun-operator/third_party/ttyd.LICENSE").is_file())
        manager = (ROOT / "app/cetirun-operator/Dockerfile").read_text()
        self.assertNotIn("/ttyd", manager)


if __name__ == "__main__":
    unittest.main()
