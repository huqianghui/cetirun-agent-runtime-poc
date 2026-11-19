# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Source-stage qualification with an explicitly selected, cached Node image."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
IMAGE = os.environ.get("CETIRUN_PERMISSIONS_TEST_IMAGE")


@unittest.skipUnless(IMAGE, "set CETIRUN_PERMISSIONS_TEST_IMAGE to a cached Node image")
class ExampleImagePermissionsTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(shutil.which("docker"), "Docker is required; no image will be pulled")
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-image-modes-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.config = self.directory / "docker-config"
        self.config.mkdir()
        inspected = self.docker(
            "image", "inspect", "--format", "{{.Id}} {{.Os}}/{{.Architecture}}", IMAGE,
        )
        self.assertEqual(inspected.returncode, 0, inspected.stderr)
        self.base_id, self.platform = inspected.stdout.split()
        self.assertTrue(self.base_id.startswith("sha256:"))

    def docker(self, *args, timeout=60):
        return subprocess.run(
            ["docker", "--config", str(self.config), *args],
            text=True, capture_output=True, timeout=timeout,
        )

    def context(self, example, suffix=""):
        source = ROOT / "examples" / example
        target = self.directory / (example + suffix)
        previous = os.umask(0o077)
        try:
            target.mkdir()
            for path in (source / "Dockerfile", *source.glob("*.mjs")):
                shutil.copyfile(path, target / path.name)
            if example == "vscode-server":
                extension = target / "gateway-extension"
                extension.mkdir()
                for path in (source / "gateway-extension").iterdir():
                    shutil.copyfile(path, extension / path.name)
                nested = extension / "nested"
                nested.mkdir()
                (nested / "data.json").write_text('{"fixture": true}\n')
                executable = nested / "probe.sh"
                executable.write_text("#!/bin/sh\nexit 0\n")
                executable.chmod(0o700)
        finally:
            os.umask(previous)
        self.assertEqual(target.stat().st_mode & 0o777, 0o700)
        self.assertEqual(next(target.glob("*.mjs")).stat().st_mode & 0o777, 0o600)
        return target

    def build(self, context):
        image_file = context / "image-id"
        result = self.docker(
            "build", "--pull=false", "--network=none", "--platform", self.platform,
            "--target", "runtime-source", "--build-arg", f"BASE_IMAGE={IMAGE}",
            "--iidfile", str(image_file), str(context), timeout=120,
        )
        return result, image_file

    def run_image(self, image_id, *command):
        return self.docker(
            "run", "--rm", "--pull=never", "--platform", self.platform,
            "--network=none", "--read-only", "--user", "1024:1024",
            "--cap-drop=ALL", "--security-opt=no-new-privileges",
            "--entrypoint", "node", image_id, *command,
        )

    def test_restrictive_context_is_readable_as_runtime_user(self):
        for example, directory, entrypoint in (
            ("pi-agent", "/opt/cetirun-pi", "server.mjs"),
            ("vscode-server", "/opt/cetirun-vscode", "start.mjs"),
        ):
            with self.subTest(example=example):
                context = self.context(example)
                built, image_file = self.build(context)
                self.assertEqual(built.returncode, 0, built.stdout + built.stderr)
                image_id = image_file.read_text().strip()
                print(f"\n{example}: base={self.base_id} stage={image_id} {self.platform} uid=1024 gid=1024")
                checked = self.run_image(image_id, "--input-type=module", "-e", """
                    import assert from 'node:assert/strict';
                    import {spawnSync} from 'node:child_process';
                    import fs from 'node:fs';
                    import path from 'node:path';
                    assert.equal(process.getuid(), 1024);
                    assert.equal(process.getgid(), 1024);
                    function readTree(directory) {
                      assert.equal(fs.statSync(directory).mode & 0o777, 0o755, directory);
                      for (const entry of fs.readdirSync(directory, {withFileTypes: true})) {
                        const name = path.join(directory, entry.name);
                        if (entry.isDirectory()) readTree(name);
                        else {
                          fs.readFileSync(name);
                          assert.equal(fs.statSync(name).uid, 0, name);
                          const executable = name.endsWith('/pi-tui.mjs') || name.endsWith('/probe.sh');
                          assert.equal(fs.statSync(name).mode & 0o777, executable ? 0o755 : 0o644, name);
                        }
                      }
                    }
                    readTree(process.argv[1]);
                    const tui = path.join(process.argv[1], 'pi-tui.mjs');
                    if (fs.existsSync(tui)) {
                      const result = spawnSync(tui, ['--help'], {encoding: 'utf8'});
                      assert.equal(result.status, 0, result.stderr);
                      assert.match(result.stdout, /^Usage: pi-tui/);
                    }
                """, directory)
                self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)
                help_result = self.run_image(image_id, f"{directory}/{entrypoint}", "--help")
                self.assertEqual(help_result.returncode, 0, help_result.stderr)
                self.assertTrue(help_result.stdout.strip())

    def test_build_rejects_root_readable_but_nonroot_inaccessible_sources(self):
        cases = (
            ("pi-agent", "server.mjs", "0600"),
            ("pi-agent", "pi-runtime.mjs", "0600"),
            ("pi-agent", "pi-tui.mjs", "0644"),
            ("vscode-server", "start.mjs", "0600"),
            ("vscode-server", "browser-storage.mjs", "0600"),
            ("vscode-server", "gateway-extension/extension.cjs", "0600"),
            ("vscode-server", "gateway-extension", "0700"),
            ("vscode-server", "gateway-extension/nested", "0700"),
        )
        for index, (example, path, mode) in enumerate(cases):
            with self.subTest(example=example, path=path, mode=mode):
                context = self.context(example, f"-denied-{index}")
                dockerfile = context / "Dockerfile"
                source = dockerfile.read_text()
                self.assertIn("\nUSER 1024:1024\n", source)
                dockerfile.write_text(source.replace(
                    "\nUSER 1024:1024\n",
                    f"\nRUN chmod {mode} {path} && test -r {path}\nUSER 1024:1024\n",
                    1,
                ))
                result, _ = self.build(context)
                output = result.stdout + result.stderr
                self.assertNotEqual(result.returncode, 0, output)
                self.assertRegex(output, r"EACCES|Permission denied")
                self.assertIn(path, output)

    def test_permission_changes_do_not_open_home_or_unrelated_paths(self):
        for example in ("pi-agent", "vscode-server"):
            with self.subTest(example=example):
                context = self.context(example)
                dockerfile = context / "Dockerfile"
                dockerfile.write_text(dockerfile.read_text().replace(
                    "\nUSER root\n",
                    "\nUSER root\n"
                    "RUN mkdir -m 0700 /home/mode-fixture /opt/unrelated-fixture "
                    "&& touch /home/mode-fixture/private /opt/unrelated-fixture/private "
                    "&& chmod 0600 /home/mode-fixture/private /opt/unrelated-fixture/private\n",
                    1,
                ))
                built, image_file = self.build(context)
                self.assertEqual(built.returncode, 0, built.stdout + built.stderr)
                checked = self.run_image(image_file.read_text().strip(), "-e", """
                    const assert = require('node:assert/strict');
                    const fs = require('node:fs');
                    for (const directory of ['/home/mode-fixture', '/opt/unrelated-fixture']) {
                      assert.equal(fs.statSync(directory).mode & 0o777, 0o700, directory);
                      assert.throws(() => fs.readFileSync(`${directory}/private`), {code: 'EACCES'});
                    }
                """)
                self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)


if __name__ == "__main__":
    unittest.main()
