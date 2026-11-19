# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("cetirun_version", ROOT / "scripts/cetirun-version.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)

ACCEPTED_VERSIONS = (
    "0.0.0",
    "1.2.30",
    "0.1.0-alpha",
    "0.1.0-alpha.1",
    "0.1.0-beta.2",
    "0.1.0-dev.20260927.gabcdef0",
    "0.1.0-rc.1",
    "0.1.0-preview.custom",
    "0.1.0-0",
    "0.1.0-alpha-beta.1",
    "0.1.0-01a",
    "1.2.3-" + "a" * 122,
)

REJECTED_VERSIONS = (
    "", "1", "1.2", "1.2.x", "v1.2.3", "dev-1.2.3",
    " 1.2.3", "01.2.3", "1.02.3", "1.2.03",
    "1.2.3custom", "1.2.3.4", "1.2.3-", "1.2.3-alpha..1",
    "1.2.3-.alpha", "1.2.3-alpha.", "1.2.3-01", "1.2.3-alpha.01",
    "1.2.3 ", "1.2.3\n", "1.2.3-\tdev", '1.2.3-"quoted"',
    "1.2.3-'quoted'", "1.2.3-α", "1.2.٣",
    "1.2.3+build.123", "1.2.3-dev.1+build.123",
    "1.2.3-" + "a" * 123,
)

VERSION_FILES = (
    "app/cetirun-memory-context/assets/adapters/openclaw/package.json",
    "app/cetirun-chart/Chart.yaml",
    "app/cetirun-operator/chart/Chart.yaml",
    "app/cetirun-portal/chart/Chart.yaml",
    "app/cetirun-platform/Chart.yaml",
    "app/cetirun-platform/Chart.lock",
    "infra/istio/Chart.yaml",
    "app/cetirun-ai-gateway/chart/Chart.yaml",
    "app/cetirun-agent-broker/chart/Chart.yaml",
    "app/cetirun-connections-mcp/chart/Chart.yaml",
    "app/cetirun-observability/chart/Chart.yaml",
    "app/cetirun-operator/chart/values.yaml",
    "app/cetirun-operator/cmd/main.go",
    "app/cetirun-operator/deploy/deploy.sh",
    "app/cetirun-portal/deploy/deploy.sh",
    "app/cetirun-portal/go.mod",
    "app/internal/go.mod",
    "app/cetirun-portal/web/package.json",
    "app/cetirun-portal/web/package-lock.json",
    "examples/pi-agent/package.json",
    "examples/pi-agent/package-lock.json",
)


class CetiRunVersionTests(unittest.TestCase):
    def test_mcr_is_only_a_pull_endpoint(self):
        for registry in (MODULE.DEFAULT_REGISTRY, "mcr.microsoft.com", "mcr.microsoft.com:443/example",
                         "MCR.MICROSOFT.COM/example", "oci://mcr.microsoft.com/example"):
            with self.subTest(registry=registry), self.assertRaisesRegex(ValueError, "MCR is a pull endpoint"):
                MODULE.validate_push_registry(registry)
        for registry in ("registry.example.com/test", "synthetic.azurecr.io/public/example/staging/42"):
            MODULE.validate_push_registry(registry)

    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.addCleanup(MODULE.PENDING_UPDATES.clear)
        self.root = Path(temporary.name)
        (self.root / "scripts").mkdir()
        shutil.copyfile(ROOT / "scripts/release-artifacts.json", self.root / "scripts/release-artifacts.json")
        for relative in VERSION_FILES:
            destination = self.root / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / relative, destination)
        self.enterContext(patch.object(MODULE, "ROOT", self.root))
        self.istio = self.root / "infra/istio/Chart.yaml"
        self.version = MODULE.top_level_yaml(self.root / "app/cetirun-chart/Chart.yaml", "version")

    def test_accepts_release_and_prerelease_versions(self):
        for command in ("set", "check"):
            for version in ACCEPTED_VERSIONS:
                with self.subTest(command=command, version=version):
                    with patch.object(MODULE, f"{command}_version") as action, \
                            patch.object(sys, "argv", ["version", command, version]):
                        self.assertEqual(MODULE.main(), 0)
                    action.assert_called_once_with(version)

    def test_rejects_invalid_versions_before_updates(self):
        for command in ("set", "check"):
            for version in REJECTED_VERSIONS:
                with self.subTest(command=command, version=version):
                    with patch.object(MODULE, f"{command}_version") as action, \
                            patch.object(sys, "argv", ["version", command, version]):
                        with self.assertRaisesRegex(ValueError, "must be X.Y.Z"):
                            MODULE.main()
                    action.assert_not_called()

    def test_invalid_version_leaves_files_unchanged(self):
        before = {name: (self.root / name).read_bytes() for name in VERSION_FILES}
        for version in ("1.2.3custom", '1.2.3-"quoted"', "1.2.3+build.123"):
            with self.subTest(version=version):
                with patch.object(sys, "argv", ["version", "set", version]):
                    with self.assertRaises(ValueError):
                        MODULE.main()
                self.assertEqual(
                    before, {name: (self.root / name).read_bytes() for name in VERSION_FILES}
                )

    def test_preserves_suffix_in_version_files(self):
        for version in ACCEPTED_VERSIONS:
            with self.subTest(version=version):
                MODULE.set_version(version)
                for relative in VERSION_FILES:
                    if relative.endswith("Chart.lock"):
                        continue  # Updated by Helm, not the Python setter.
                    self.assertIn(version, (self.root / relative).read_text(), relative)
                self.assertEqual(
                    MODULE.top_level_yaml(self.root / "app/cetirun-chart/Chart.yaml", "version"), version
                )
                for relative in (
                    "app/cetirun-portal/web/package.json",
                    "app/cetirun-portal/web/package-lock.json",
                    "examples/pi-agent/package.json",
                    "examples/pi-agent/package-lock.json",
                ):
                    self.assertEqual(json.loads((self.root / relative).read_text())["version"], version)

    @unittest.skipUnless(shutil.which("helm") and shutil.which("make"), "requires Helm and Make")
    @patch.dict(os.environ, {"MAKEFLAGS": " -- VERSION=9.8.7", "VERSION": "9.8.7"})
    def test_make_set_version(self):
        # Each fixture tests its own version, not the outer Make invocation's.
        env = {
            name: value for name, value in os.environ.items()
            if name not in {"MAKEFLAGS", "MFLAGS", "MAKELEVEL", "MAKEOVERRIDES", "MAKEFILES", "VERSION"}
        }
        env.update({
            "KUBECONFIG": os.devnull,
            "HELM_REGISTRY_CONFIG": os.devnull,
            "HELM_REPOSITORY_CONFIG": os.devnull,
        })
        for relative in (
            "Makefile", "scripts/cetirun-version.py", "scripts/cetirun-release-metadata.sh",
            "scripts/tests/test_connections_mcp_chart.py",
        ):
            destination = self.root / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / relative, destination)
        shutil.copytree(ROOT / "infra/istio", self.root / "infra/istio",
                        dirs_exist_ok=True, ignore=shutil.ignore_patterns("charts", "__pycache__"))
        for relative in (
            "cetirun-platform", "cetirun-operator/chart", "cetirun-portal/chart",
            "cetirun-ai-gateway/chart", "cetirun-connections-mcp/chart",
            "cetirun-agent-broker/chart", "cetirun-observability/chart",
        ):
            shutil.copytree(
                ROOT / "app" / relative, self.root / "app" / relative,
                dirs_exist_ok=True, ignore=shutil.ignore_patterns("charts"),
            )
        for version in (
            "0.1.0-alpha.1", "0.1.0-beta.2", "0.1.1-dev.20261029.gc5f0eb8", "0.1.0", "0.1.2",
        ):
            with self.subTest(version=version):
                for target in ("set-version", "version-check"):
                    command = ["make", target, f"PYTHON={sys.executable}"]
                    if target == "set-version":
                        command.append(f"VERSION={version}")
                    result = subprocess.run(
                        command, cwd=self.root, env=env, text=True, capture_output=True, timeout=30
                    )
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                MODULE.check_version(version)
                lint = subprocess.run(
                    ["helm", "lint", "app/cetirun-platform", "--namespace", "cetirun-system",
                     "--set", "istio.enabled=true",
                     "--set", "cetirun-operator.domainSuffix=platform.example.invalid",
                     "--set", "cetirun-operator.cetiadmin.enabled=false",
                     "--set", "cetirun-portal.istio.host=portal.example.invalid"], cwd=self.root,
                    env=env, text=True, capture_output=True, timeout=30
                )
                self.assertEqual(lint.returncode, 0, lint.stdout + lint.stderr)
                for component in ("operator", "portal", "ai-gateway", "connections-mcp"):
                    archive = self.root / f"app/cetirun-platform/charts/cetirun-{component}-{version}.tgz"
                    self.assertTrue(archive.is_file(), archive)
                rendered = subprocess.run(
                    [sys.executable, "-B", "-m", "unittest", "-v",
                     "scripts.tests.test_connections_mcp_chart.ConnectionsMCPChartTests."
                     "test_full_stack_uses_separate_broker_and_native_mcp"],
                    cwd=self.root, env=env, text=True, capture_output=True, timeout=60,
                )
                self.assertEqual(rendered.returncode, 0, rendered.stdout + rendered.stderr)

    def test_set_synchronizes_broker_chart_and_app_versions(self):
        MODULE.set_version("0.1.2")
        broker = self.root / "app/cetirun-agent-broker/chart/Chart.yaml"
        for field in ("version", "appVersion"):
            self.assertEqual(MODULE.top_level_yaml(broker, field), "0.1.2")

    def test_check_rejects_stale_broker_versions(self):
        broker = self.root / "app/cetirun-agent-broker/chart/Chart.yaml"
        original = broker.read_text()
        for field in ("version", "appVersion"):
            with self.subTest(field=field):
                broker.write_text("".join(
                    f'{field}: "0.0.0"\n' if line.startswith(f"{field}:") else line
                    for line in original.splitlines(keepends=True)
                ))
                with self.assertRaisesRegex(ValueError, f"cetirun-agent-broker/chart/Chart.yaml: {field}"):
                    MODULE.check_version(self.version)

    def test_missing_broker_app_version_does_not_partially_update_files(self):
        broker = self.root / "app/cetirun-agent-broker/chart/Chart.yaml"
        broker.write_text("".join(
            line for line in broker.read_text().splitlines(keepends=True)
            if not line.startswith("appVersion:")
        ))
        before = {name: (self.root / name).read_bytes() for name in VERSION_FILES}
        with self.assertRaisesRegex(ValueError, "cetirun-agent-broker/chart/Chart.yaml"):
            MODULE.set_version("0.1.2")
        self.assertEqual(before, {name: (self.root / name).read_bytes() for name in VERSION_FILES})

    @unittest.skipUnless(shutil.which("helm"), "requires Helm")
    def test_broker_helm_test_follows_release_and_prerelease_app_versions(self):
        broker_dir = self.root / "app/cetirun-agent-broker/chart"
        shutil.copytree(ROOT / "app/cetirun-agent-broker/chart", broker_dir, dirs_exist_ok=True)
        for version in ("0.1.2", "0.1.2-rc.1"):
            with self.subTest(version=version):
                MODULE.set_version(version)
                self.assertEqual(MODULE.top_level_yaml(broker_dir / "Chart.yaml", "appVersion"), version)
                result = subprocess.run(
                    ["bash", str(ROOT / "scripts/cetirun-agent-broker-helm-test.sh")],
                    env={**os.environ, "CHART_DIR": str(broker_dir), "KUBECONFIG": os.devnull},
                    text=True, capture_output=True, timeout=30,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_set_synchronizes_istio_app_version(self):
        MODULE.set_version("1.2.3")
        self.assertEqual(MODULE.top_level_yaml(self.istio, "version"), "1.2.3")
        self.assertEqual(MODULE.top_level_yaml(self.istio, "appVersion"), "1.2.3")

    def test_check_rejects_stale_istio_app_version(self):
        lines = self.istio.read_text().splitlines()
        self.istio.write_text("\n".join(
            'appVersion: "0.0.0"' if line.startswith("appVersion:") else line
            for line in lines
        ) + "\n")
        with self.assertRaisesRegex(ValueError, "infra/istio/Chart.yaml: appVersion"):
            MODULE.check_version(self.version)

    def test_missing_app_version_does_not_partially_update_files(self):
        self.istio.write_text("".join(
            line for line in self.istio.read_text().splitlines(keepends=True)
            if not line.startswith("appVersion:")
        ))
        before = {name: (self.root / name).read_bytes() for name in VERSION_FILES}
        with self.assertRaisesRegex(ValueError, "infra/istio/Chart.yaml"):
            MODULE.set_version("1.2.3")
        self.assertEqual(before, {name: (self.root / name).read_bytes() for name in VERSION_FILES})


if __name__ == "__main__":
    unittest.main()
