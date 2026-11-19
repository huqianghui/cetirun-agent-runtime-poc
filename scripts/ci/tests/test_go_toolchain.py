# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Keep native checks, modules, and shipped Go builders on one toolchain."""

from pathlib import Path
import re
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[3]
GO_IMAGE = re.compile(r"([a-zA-Z0-9._/-]*golang):([^\s@]+)(?:@sha256:([0-9a-f]{64}))?")


class GoToolchainTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.version = (ROOT / ".go-version").read_text().strip()
        output = subprocess.check_output(
            ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
            cwd=ROOT,
        )
        cls.paths = {
            ROOT / path.decode()
            for path in output.split(b"\0")
            if path and (ROOT / path.decode()).is_file()
        }

    def test_security_floor(self):
        self.assertRegex(self.version, r"^\d+\.\d+\.\d+$")
        # GO-2026-6809 is fixed in Go 1.27 starting with 1.27.3.
        self.assertGreaterEqual(tuple(map(int, self.version.split("."))), (1, 27, 3))

    def test_module_minimums_match(self):
        modules = sorted(path for path in self.paths if path.name == "go.mod")
        self.assertGreaterEqual(len(modules), 2)
        for path in modules:
            with self.subTest(path=path.relative_to(ROOT)):
                text = path.read_text()
                self.assertEqual(
                    re.findall(r"^go (\S+)$", text, re.MULTILINE), [self.version]
                )
                for toolchain in re.findall(r"^toolchain go(\S+)$", text, re.MULTILINE):
                    self.assertEqual(toolchain, self.version)

    def test_every_go_workflow_uses_the_version_file(self):
        count = 0
        for path in sorted((ROOT / ".github/workflows").glob("*.y*ml")):
            text = path.read_text()
            steps = len(re.findall(r"uses:\s*actions/setup-go@", text))
            if not steps:
                continue
            count += steps
            with self.subTest(path=path.relative_to(ROOT)):
                versions = re.findall(
                    r"^\s+go-version-file:\s*[\"']?([^\"'\s#]+)", text, re.MULTILINE
                )
                self.assertEqual(versions, [".go-version"] * steps)
                self.assertNotRegex(text, r"(?m)^\s+go-version:")
        self.assertGreaterEqual(count, 5)

    def test_all_go_builders_match_and_are_digest_pinned(self):
        builders = set()
        digests = set()
        for path in sorted(self.paths):
            if path.name != "Dockerfile" and not path.name.startswith("Dockerfile."):
                continue
            for image, version, digest in GO_IMAGE.findall(path.read_text()):
                with self.subTest(path=path.relative_to(ROOT), image=image):
                    self.assertEqual(image, "mcr.microsoft.com/oss/go/microsoft/golang")
                    # A Microsoft packaging revision does not change the Go release.
                    self.assertRegex(version, rf"^{re.escape(self.version)}(?:-\d+)?$")
                    self.assertRegex(digest, r"^[0-9a-f]{64}$")
                    builders.add(path.relative_to(ROOT).as_posix())
                    digests.add(digest)
        self.assertTrue(
            {
                "app/cetirun-chart/Dockerfile.base",
                "app/cetirun-agent-broker/Dockerfile",
                "app/cetirun-operator/Dockerfile",
                "app/cetirun-portal/Dockerfile",
            }.issubset(builders)
        )
        self.assertEqual(len(digests), 1)

    def dependabot_directories(self, ecosystem):
        text = (ROOT / ".github/dependabot.yml").read_text()
        block = re.search(
            rf'(?ms)^  - package-ecosystem: "{ecosystem}"\n(.*?)(?=^  - package-ecosystem:|\Z)',
            text,
        )
        self.assertIsNotNone(block, f"Missing Dependabot ecosystem: {ecosystem}")
        directories = re.search(r"(?ms)^    directories:\n(.*?)(?=^    \S)", block[1])
        self.assertIsNotNone(directories)
        return set(re.findall(r'^      - "([^"\n]+)"$', directories[1], re.MULTILINE)), block[1]

    def test_dependabot_covers_and_can_parse_every_go_builder(self):
        directories, block = self.dependabot_directories("docker")
        self.assertIn('group-by: "dependency-name"', block)
        for path in sorted(self.paths):
            if path.name != "Dockerfile" and not path.name.startswith("Dockerfile."):
                continue
            text = path.read_text()
            images = GO_IMAGE.findall(text)
            if not images:
                continue
            with self.subTest(path=path.relative_to(ROOT)):
                self.assertIn("/" + path.parent.relative_to(ROOT).as_posix(), directories)
                visible_images = GO_IMAGE.findall("\n".join(
                    line for line in text.splitlines() if line.startswith("FROM ")
                ))
                self.assertEqual(images, visible_images, "Dependabot cannot update images hidden in ARGs")

    def test_dependabot_covers_all_application_go_modules(self):
        directories, block = self.dependabot_directories("gomod")
        self.assertIn('dependency-type: "all"', block)
        for path in sorted(self.paths):
            if path.name == "go.mod" and path.relative_to(ROOT).parts[0] == "app":
                with self.subTest(path=path.relative_to(ROOT)):
                    self.assertIn("/" + path.parent.relative_to(ROOT).as_posix(), directories)

    def test_runtime_go_override_defaults_to_the_discoverable_stage(self):
        text = (ROOT / "app/cetirun-chart/Dockerfile.base").read_text()
        self.assertEqual(re.findall(r"(?m)^ARG GO_BUILD_IMAGE=(\S+)$", text), ["go-toolchain"])
        self.assertRegex(text, r"(?m)^FROM \S+golang:\S+ AS go-toolchain$")
        stages = re.findall(r"(?m)^FROM \$\{GO_BUILD_IMAGE\} AS (\S+)$", text)
        self.assertEqual(stages, ["tailscale-build", "gh-build", "kubectl-build"])


if __name__ == "__main__":
    unittest.main()
