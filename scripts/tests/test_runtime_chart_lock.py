# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline regressions for private Runtime Chart image locking."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "app/cetirun-chart"
HELPER = ROOT / "scripts/cetirun-runtime-chart-lock.py"
RELEASE = ROOT / "scripts/cetirun-helm-release.sh"
OPENCLAW_DIGEST = "sha256:" + "a" * 64
HERMES_DIGEST = "sha256:" + "b" * 64
REGISTRY = "registry.example.com/candidate/r3"
VERSION = "0.1.1-r3"


def tree_digest(directory: Path) -> str:
    result = hashlib.sha256()
    for path in sorted(item for item in directory.rglob("*") if item.is_file()):
        result.update(str(path.relative_to(directory)).encode() + b"\0")
        result.update(path.read_bytes())
    return result.hexdigest()


class RuntimeChartLockTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-runtime-chart-lock-")
        self.addCleanup(temporary.cleanup)
        self.work = Path(temporary.name)
        self.source = self.work / "source"
        shutil.copytree(CHART, self.source)
        self.source_digest = tree_digest(self.source)

    def lock(self, output: Path, *, registry=REGISTRY, version=VERSION,
             openclaw=OPENCLAW_DIGEST, hermes=HERMES_DIGEST):
        return subprocess.run(
            [sys.executable, "-B", str(HELPER), "--source-chart", str(self.source),
             "--output-chart", str(output), "--registry", registry, "--version", version,
             "--openclaw-image-digest", openclaw, "--hermes-image-digest", hermes],
            capture_output=True, text=True, timeout=20,
        )

    def test_locks_a_private_copy_and_preserves_source(self):
        output = self.work / "locked"
        result = self.lock(output)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(tree_digest(self.source), self.source_digest)
        receipt = json.loads(result.stdout)
        self.assertEqual(receipt["openclawImage"], f"{REGISTRY}/openclaw-gateway@{OPENCLAW_DIGEST}")
        self.assertEqual(receipt["hermesImage"], f"{REGISTRY}/hermes-gateway@{HERMES_DIGEST}")
        self.assertEqual(receipt["version"], VERSION)
        self.assertEqual(receipt["lockHash"], hashlib.sha256(
            f"{REGISTRY}\n{VERSION}\n{OPENCLAW_DIGEST}\n{HERMES_DIGEST}".encode()).hexdigest())

        values = (output / "values.yaml").read_text()
        self.assertIn(f"openclaw-gateway\n    pullPolicy: Always\n    tag: \"\"\n    digest: {OPENCLAW_DIGEST}", values)
        self.assertIn(f"hermes-gateway\n    pullPolicy: Always\n    tag: \"\"\n    digest: {HERMES_DIGEST}", values)
        for runtime, digest in (("openclaw", OPENCLAW_DIGEST), ("hermes", HERMES_DIGEST)):
            profile = (output / "agents" / runtime / "profile.yaml").read_text()
            self.assertIn(f"- {REGISTRY}/{runtime}-gateway@{digest}\n", profile)
        self.assertEqual((output / "Chart.yaml").read_bytes(), (self.source / "Chart.yaml").read_bytes())

    def test_preserves_existing_qualified_images(self):
        profile = self.source / "agents/openclaw/profile.yaml"
        qualified = "registry.example.com/previous@sha256:" + "c" * 64
        profile.write_text(profile.read_text().replace("qualifiedImages: []", f"qualifiedImages:\n- {qualified}"))
        self.source_digest = tree_digest(self.source)
        output = self.work / "locked"
        result = self.lock(output)
        self.assertEqual(result.returncode, 0, result.stderr)
        locked = (output / "agents/openclaw/profile.yaml").read_text()
        self.assertIn(f"- {qualified}\n", locked)
        self.assertIn(f"- {REGISTRY}/openclaw-gateway@{OPENCLAW_DIGEST}\n", locked)
        self.assertEqual(tree_digest(self.source), self.source_digest)

    def test_locked_package_excludes_stale_dependencies_and_python_caches(self):
        stale = self.source / "charts/stale"
        stale.mkdir(parents=True)
        (stale / "Chart.yaml").write_text("apiVersion: v2\nname: stale\nversion: 0.1.0\n")
        cache = self.source / "__pycache__"
        cache.mkdir()
        (cache / "stale.pyc").write_bytes(b"stale cache")
        source_digest = tree_digest(self.source)

        output = self.work / "locked"
        result = self.lock(output)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((output / "charts").exists())
        self.assertFalse((output / "__pycache__").exists())
        self.assertEqual(tree_digest(self.source), source_digest)

        result = subprocess.run(
            ["helm", "package", str(output), "--destination", str(self.work)],
            capture_output=True, text=True, timeout=20,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        with tarfile.open(next(self.work.glob("*.tgz"))) as archive:
            names = archive.getnames()
        self.assertIn("cetirun/values.yaml", names)
        self.assertFalse(any("charts" in Path(name).parts or "__pycache__" in Path(name).parts for name in names))

    def test_rejects_invalid_or_replacing_lock_inputs_without_changing_source(self):
        cases = (
            {"openclaw": "sha256:not-a-digest"},
            {"registry": "https://registry.example.com"},
            {"version": "latest"},
        )
        for number, overrides in enumerate(cases):
            with self.subTest(overrides=overrides):
                result = self.lock(self.work / f"invalid-{number}", **overrides)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(tree_digest(self.source), self.source_digest)
                self.assertFalse((self.work / f"invalid-{number}").exists())

        values = self.source / "values.yaml"
        before, marker, openclaw = values.read_text().partition("openclaw:\n")
        self.assertTrue(marker)
        values.write_text(before + marker + openclaw.replace(
            'digest: ""', f"digest: sha256:{'c' * 64}", 1))
        changed_digest = tree_digest(self.source)
        result = self.lock(self.work / "replaced")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("refusing to replace", result.stderr)
        self.assertEqual(tree_digest(self.source), changed_digest)
        self.assertFalse((self.work / "replaced").exists())

    def test_rejects_output_inside_source_without_changing_source(self):
        source_alias = self.work / "source-alias"
        source_alias.symlink_to(self.source, target_is_directory=True)
        for output in (self.source / "locked", self.source / "agents/locked",
                       source_alias / "agents/locked"):
            with self.subTest(output=output):
                result = self.lock(output)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("outside the source chart", result.stderr)
                self.assertFalse(output.exists())
                self.assertEqual(tree_digest(self.source), self.source_digest)

    def test_rejects_a_missing_runtime_image_without_reading_the_next_runtime(self):
        values = self.source / "values.yaml"
        before, marker, openclaw = values.read_text().partition("openclaw:\n")
        self.assertTrue(marker)
        values.write_text(before + marker + openclaw.replace("  image:\n", "  nativeImage:\n", 1))
        source_digest = tree_digest(self.source)
        result = self.lock(self.work / "missing-image")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing openclaw.image values", result.stderr)
        self.assertEqual(tree_digest(self.source), source_digest)
        self.assertFalse((self.work / "missing-image").exists())

    def test_release_script_rejects_partial_digest_pair_before_lint_or_publish(self):
        result = subprocess.run(
            ["bash", str(RELEASE)], env=os.environ | {"OPENCLAW_IMAGE_DIGEST": OPENCLAW_DIGEST},
            capture_output=True, text=True, timeout=20,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must be supplied together", result.stderr)
        self.assertNotIn("Linting Helm chart", result.stdout)


if __name__ == "__main__":
    unittest.main()
