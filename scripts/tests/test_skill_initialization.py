# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Run the npm initializer against a synthetic loopback-only registry."""

import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tarfile
import tempfile
import threading
import unittest
from urllib.parse import unquote


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "app/cetirun-chart/configs/init-skills.sh"


@unittest.skipUnless(shutil.which("npm"), "npm is required for local package qualification")
class SkillInitializationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-npm-skills-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.skills = self.root / "skills"
        self.requests = []
        self.reject_tarballs = False
        self.packages = {}
        for name in ("cetirun-fixture", "@fixture/skills"):
            for version in ("1.0.0", "1.0.1"):
                buffer = io.BytesIO()
                with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
                    for filename, content in {
                        "package.json": json.dumps({"name": name, "version": version, "license": "MIT"}),
                        "SKILL.md": f"Synthetic {name} {version}\n",
                        ".installed-version": f"{name}@{version}\n",
                    }.items():
                        data = content.encode()
                        info = tarfile.TarInfo("package/" + filename)
                        info.size = len(data)
                        info.mode = 0o644
                        archive.addfile(info, io.BytesIO(data))
                self.packages[(name, version)] = buffer.getvalue()
        fixture = self

        class Registry(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
                route = unquote(self.path.split("?", 1)[0])
                fixture.requests.append(route)
                if route.startswith("/tarballs/"):
                    if fixture.reject_tarballs:
                        self.send_error(503, "Synthetic package failure")
                        return
                    name, version = route.removeprefix("/tarballs/").removesuffix(".tgz").rsplit("-", 1)
                    data = fixture.packages.get((name.replace("__", "/"), version))
                    if data is None:
                        self.send_error(404)
                        return
                    media = "application/octet-stream"
                else:
                    name = route.removeprefix("/")
                    versions = {
                        version: {
                            "name": name, "version": version,
                            "dist": {
                                "tarball": f"{fixture.registry}/tarballs/{name.replace('/', '__')}-{version}.tgz",
                                "shasum": hashlib.sha1(content).hexdigest(),
                            },
                        }
                        for (package, version), content in fixture.packages.items() if package == name
                    }
                    if not versions:
                        self.send_error(404)
                        return
                    data = json.dumps({"name": name, "dist-tags": {"latest": "1.0.1"}, "versions": versions}).encode()
                    media = "application/json"
                self.send_response(200)
                self.send_header("Content-Type", media)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Registry)
        self.registry = f"http://127.0.0.1:{self.server.server_port}"
        thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(thread.join, 2)
        self.addCleanup(self.server.shutdown)
        for name in ("user.npmrc", "global.npmrc"):
            (self.root / name).write_text("")
        self.env = {
            "PATH": os.environ["PATH"], "HOME": str(self.root / "home"),
            "SKILLS_DIR": str(self.skills), "NPM_CACHE_DIR": str(self.root / "cache"),
            "NPM_REGISTRY": self.registry,
            "npm_config_userconfig": str(self.root / "user.npmrc"),
            "npm_config_globalconfig": str(self.root / "global.npmrc"),
            "npm_config_fetch_retries": "0",
            "npm_config_update_notifier": "false",
        }

    def run_init(self, specs):
        return subprocess.run(
            ["sh", str(SCRIPT)], env={**self.env, "NPM_PACKAGES": specs},
            text=True, capture_output=True, timeout=30,
        )

    def assert_ok(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def fail_copy(self, after_copy):
        tools = self.root / "tools"
        tools.mkdir()
        wrapper = tools / "cp"
        wrapper.write_text(f"#!/bin/sh\n{shutil.which('cp')} \"$@\" || exit 1\n{after_copy}\n")
        wrapper.chmod(0o700)
        self.env["PATH"] = str(tools) + os.pathsep + os.environ["PATH"]

    def test_real_packages_are_pinned_idempotent_updated_and_pruned(self):
        specs = "cetirun-fixture@1.0.0 @fixture/skills@1.0.0"
        self.assert_ok(self.run_init(specs))
        self.assertEqual((self.skills / "cetirun-fixture/SKILL.md").read_text(), "Synthetic cetirun-fixture 1.0.0\n")
        self.assertEqual((self.skills / "@fixture__skills/SKILL.md").read_text(), "Synthetic @fixture/skills 1.0.0\n")
        before = list(self.requests)
        self.assert_ok(self.run_init(specs))
        self.assertEqual(self.requests, before, "unchanged versions must not fetch again")
        self.assert_ok(self.run_init("cetirun-fixture@1.0.1"))
        self.assertEqual((self.skills / "cetirun-fixture/.installed-version").read_text(), "cetirun-fixture@1.0.1\n")
        self.assertEqual((self.skills / "cetirun-fixture/SKILL.md").read_text(), "Synthetic cetirun-fixture 1.0.1\n")
        self.assertFalse((self.skills / "@fixture__skills").exists())
        self.assert_ok(self.run_init(""))
        self.assertFalse((self.skills / "cetirun-fixture").exists())

    def test_download_failure_preserves_the_previous_installation(self):
        self.assert_ok(self.run_init("cetirun-fixture@1.0.0"))
        self.reject_tarballs = True
        result = self.run_init("cetirun-fixture@1.0.1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("FAILED npm install", (self.skills / ".errors.log").read_text())
        self.assertEqual((self.skills / "cetirun-fixture/SKILL.md").read_text(), "Synthetic cetirun-fixture 1.0.0\n")

    def test_failed_copy_cannot_publish_a_package_supplied_completion_marker(self):
        self.assert_ok(self.run_init("cetirun-fixture@1.0.0"))
        self.fail_copy("exit 23")
        result = self.run_init("cetirun-fixture@1.0.1")
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("FAILED", (self.skills / ".errors.log").read_text())
        self.assertEqual((self.skills / "cetirun-fixture/.installed-version").read_text(), "cetirun-fixture@1.0.0\n")
        self.assertEqual((self.skills / "cetirun-fixture/SKILL.md").read_text(), "Synthetic cetirun-fixture 1.0.0\n")
        self.assertEqual(list(self.skills.glob(".stage.*")), [])
        self.env["PATH"] = os.environ["PATH"]
        self.assert_ok(self.run_init("cetirun-fixture@1.0.1"))
        self.assertEqual((self.skills / "cetirun-fixture/SKILL.md").read_text(), "Synthetic cetirun-fixture 1.0.1\n")

    def test_failed_marker_write_does_not_publish_or_claim_success(self):
        self.fail_copy('rm -f "$3/.installed-version"\nmkdir "$3/.installed-version"')
        result = self.run_init("cetirun-fixture@1.0.0")
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.skills / "cetirun-fixture").exists())
        self.assertIn("FAILED", (self.skills / ".errors.log").read_text())
        self.assertEqual(list(self.skills.glob(".stage.*")), [])

    def test_unusable_initialization_root_fails_before_any_download(self):
        self.skills.write_text("Synthetic non-directory")
        result = self.run_init("")
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(result.stderr)
        self.assertEqual(self.requests, [])

    def test_sigkill_copy_is_reclaimed_before_the_next_install(self):
        self.assert_ok(self.run_init("cetirun-fixture@1.0.0"))
        self.fail_copy('kill -KILL "$PPID"\nexit 137')
        interrupted = self.run_init("cetirun-fixture@1.0.1")
        self.assertEqual(interrupted.returncode, -signal.SIGKILL, interrupted.stdout + interrupted.stderr)
        abandoned = list(self.skills.glob(".stage.*"))
        self.assertEqual(len(abandoned), 1)
        self.assertEqual((self.skills / "cetirun-fixture/.installed-version").read_text(), "cetirun-fixture@1.0.0\n")
        self.assertEqual((self.skills / "cetirun-fixture/SKILL.md").read_text(), "Synthetic cetirun-fixture 1.0.0\n")

        tools = self.root / "tools"
        (tools / "cp").unlink()
        npm = tools / "npm"
        npm.write_text(
            '#!/bin/sh\n'
            'for old in "$SKILLS_DIR"/.stage.??????; do\n'
            '  [ ! -d "$old" ] || { echo "abandoned copy still consumes retry space" >&2; exit 28; }\n'
            'done\n'
            f'exec "{shutil.which("npm")}" "$@"\n'
        )
        npm.chmod(0o700)
        self.assert_ok(self.run_init("cetirun-fixture@1.0.1"))
        self.assertEqual(list(self.skills.glob(".stage.*")), [])
        self.assertEqual((self.skills / "cetirun-fixture/.installed-version").read_text(), "cetirun-fixture@1.0.1\n")
        self.assertEqual((self.skills / "cetirun-fixture/SKILL.md").read_text(), "Synthetic cetirun-fixture 1.0.1\n")

    def test_empty_selection_reclaims_only_reserved_staging_directories(self):
        self.skills.mkdir()
        for name in (".stage.aB12cD", ".stage.012345"):
            stage = self.skills / name
            stage.mkdir()
            (stage / "partial").write_text("Disposable initializer copy")
        for name in (".stage.keep-notes", ".unrelated"):
            directory = self.skills / name
            directory.mkdir()
            (directory / "keep").write_text("Unrelated hidden data")
        self.assert_ok(self.run_init(""))
        self.assertFalse((self.skills / ".stage.aB12cD").exists())
        self.assertFalse((self.skills / ".stage.012345").exists())
        for name in (".stage.keep-notes", ".unrelated"):
            self.assertEqual((self.skills / name / "keep").read_text(), "Unrelated hidden data")
        self.assertEqual(self.requests, [])

    def test_unexpected_stage_link_is_not_followed_or_silently_deleted(self):
        self.skills.mkdir()
        other = self.root / "unrelated"
        other.mkdir()
        (other / "keep").write_text("Unrelated content")
        (self.skills / ".stage.abcdef").symlink_to(other, target_is_directory=True)
        result = self.run_init("cetirun-fixture@1.0.0")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("FAILED", (self.skills / ".errors.log").read_text())
        self.assertTrue((self.skills / ".stage.abcdef").is_symlink())
        self.assertEqual((other / "keep").read_text(), "Unrelated content")
        self.assertEqual(self.requests, [])

    def test_failed_stage_reclamation_stops_before_installation(self):
        self.skills.mkdir()
        stage = self.skills / ".stage.abcdef"
        stage.mkdir()
        (stage / "partial").write_text("Disposable copy")
        tools = self.root / "tools"
        tools.mkdir()
        wrapper = tools / "rm"
        wrapper.write_text('#!/bin/sh\necho "synthetic removal failure" >&2\nexit 1\n')
        wrapper.chmod(0o700)
        self.env["PATH"] = str(tools) + os.pathsep + os.environ["PATH"]
        result = self.run_init("cetirun-fixture@1.0.0")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("FAILED reclaiming", (self.skills / ".errors.log").read_text())
        self.assertTrue((stage / "partial").exists())
        self.assertEqual(self.requests, [])


if __name__ == "__main__":
    unittest.main()
