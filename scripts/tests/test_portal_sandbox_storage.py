# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline contracts for Portal Sandbox HOME StorageClass selection wiring."""

import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
OFFLINE_ENV = {
    **os.environ,
    "KUBECONFIG": os.devnull,
    "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}
CHART = ROOT / "app/cetirun-portal/chart"
FIXTURE = ROOT / "app/cetirun-platform/ci/deployment-example.yaml"
SPEC = importlib.util.spec_from_file_location("cetirun_platform_deploy", ROOT / "scripts/cetirun_platform_deploy.py")
DEPLOY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DEPLOY)


@unittest.skipUnless(shutil.which("helm"), "requires existing Helm")
class PortalSandboxStorageChartTests(unittest.TestCase):
    def render(self, values=None):
        command = ["helm", "template", "portal", str(CHART), "--namespace", "portal-system",
                   "--set-string", "istio.host=portal.example.invalid"]
        for value in values or []:
            command.extend(["--set-string", value])
        output = subprocess.run(command, env=OFFLINE_ENV, check=True, text=True, capture_output=True).stdout
        return [item for item in yaml.safe_load_all(output) if item]

    def test_selected_source_gets_only_narrow_read_permissions(self):
        documents = self.render([
            "sandboxStorage.platformNamespace=cetirun-system",
            "sandboxStorage.platformConfigMapName=sandbox-platform",
            "sandboxStorage.defaultStorageClassName=managed-csi",
        ])
        deployment = next(item for item in documents if item["kind"] == "Deployment")
        environment = {item["name"]: item.get("value") for item in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(environment["SANDBOX_STORAGE_PLATFORM_NAMESPACE"], "cetirun-system")
        self.assertEqual(environment["SANDBOX_STORAGE_PLATFORM_CONFIG_MAP_NAME"], "sandbox-platform")
        self.assertEqual(environment["SANDBOX_STORAGE_DEFAULT_CLASS_NAME"], "managed-csi")
        role = next(item for item in documents if item["kind"] == "Role" and item["metadata"]["name"].endswith("sandbox-storage"))
        self.assertEqual(role["metadata"]["namespace"], "cetirun-system")
        self.assertEqual(role["rules"], [{"apiGroups": [""], "resources": ["configmaps"], "resourceNames": ["sandbox-platform"], "verbs": ["get"]}])
        cluster_role = next(item for item in documents if item["kind"] == "ClusterRole")
        storage_rule = next(item for item in cluster_role["rules"] if item["resources"] == ["storageclasses"])
        self.assertEqual(storage_rule["verbs"], ["get"])
        self.assertNotIn("list", storage_rule["verbs"])
        self.assertNotIn("watch", storage_rule["verbs"])

    def test_unconfigured_portal_has_no_storage_reader_permissions(self):
        documents = self.render()
        names = {item["metadata"]["name"] for item in documents}
        self.assertNotIn("portal-cetirun-portal-sandbox-storage", names)
        deployment = next(item for item in documents if item["kind"] == "Deployment")
        environment = {item["name"] for item in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertFalse(any(name.startswith("SANDBOX_STORAGE_") for name in environment))
        cluster_role = next(item for item in documents if item["kind"] == "ClusterRole")
        self.assertFalse(any(rule["resources"] == ["storageclasses"] for rule in cluster_role["rules"]))

    def test_chart_rejects_partial_or_invalid_source(self):
        for values, expected in (
            (("sandboxStorage.platformNamespace=cetirun-system",), "must be configured together"),
            (("sandboxStorage.platformConfigMapName=sandbox-platform",), "must be configured together"),
            (("sandboxStorage.platformNamespace=Invalid", "sandboxStorage.platformConfigMapName=sandbox-platform"), "valid namespace"),
            (("sandboxStorage.platformNamespace=cetirun-system", "sandboxStorage.platformConfigMapName=invalid/name"), "valid ConfigMap"),
            (("sandboxStorage.defaultStorageClassName=invalid/name",), "valid StorageClass"),
        ):
            with self.subTest(values=values):
                command = ["helm", "template", "portal", str(CHART), "--set-string", "istio.host=portal.example.invalid"]
                for value in values:
                    command.extend(["--set-string", value])
                result = subprocess.run(command, env=OFFLINE_ENV,
                                        text=True, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)


class DeploymentWiringTests(unittest.TestCase):
    def configure(self, configuration):
        with tempfile.TemporaryDirectory(prefix="cetirun-storage-wiring-") as temporary:
            path = Path(temporary) / "environment.json"
            path.write_text(json.dumps(configuration))
            return DEPLOY.configuration(path)

    def configuration(self):
        document = yaml.safe_load(FIXTURE.read_text())
        document["platform"]["values"]["cetirun-operator"]["cetiadmin"] = {"enabled": False}
        return document

    def test_enabled_operator_source_is_wired_to_portal(self):
        configuration = self.configuration()
        configuration["platform"]["values"]["cetirun-operator"]["sandboxSources"] = {
            "enabled": True, "nativeExecution": True, "bootstrapOwnedPrerequisites": True,
            "platformConfigMapName": "sandbox-platform",
        }
        configuration["platform"]["values"]["cetirun-portal"] = {"sandboxStorage": {"defaultStorageClassName": "managed-csi"}}
        _, values = self.configure(configuration)
        self.assertEqual(values["platform"]["cetirun-portal"]["sandboxStorage"], {
            "platformNamespace": "cetirun-system", "platformConfigMapName": "sandbox-platform",
            "defaultStorageClassName": "managed-csi",
        })

    def test_conflicting_or_unavailable_source_is_rejected(self):
        configuration = self.configuration()
        configuration["platform"]["values"]["cetirun-operator"]["sandboxSources"] = {
            "enabled": True, "nativeExecution": True, "bootstrapOwnedPrerequisites": True,
            "platformConfigMapName": "sandbox-platform",
        }
        configuration["platform"]["values"]["cetirun-portal"] = {"sandboxStorage": {"platformConfigMapName": "other-platform"}}
        with self.assertRaisesRegex(ValueError, "conflicts with the selected Operator platform source"):
            self.configure(configuration)
        configuration = self.configuration()
        configuration["platform"]["values"]["cetirun-portal"] = {"sandboxStorage": {
            "platformNamespace": "cetirun-system", "platformConfigMapName": "sandbox-platform"}}
        with self.assertRaisesRegex(ValueError, "requires enabled cetirun-operator.sandboxSources"):
            self.configure(configuration)

    @unittest.skipUnless(shutil.which("helm"), "requires existing Helm")
    def test_direct_umbrella_source_must_match_operator(self):
        configuration = self.configuration()
        configuration["platform"]["values"]["cetirun-operator"]["sandboxSources"] = {
            "enabled": True, "nativeExecution": True, "bootstrapOwnedPrerequisites": True,
            "platformConfigMapName": "sandbox-platform",
        }
        configuration["platform"]["values"]["cetirun-operator"]["sandboxAdmission"]["podsEnabled"] = True
        _, values = self.configure(configuration)
        with tempfile.TemporaryDirectory(prefix="cetirun-storage-platform-") as temporary:
            base = Path(temporary)
            for relative in (
                "cetirun-platform", "cetirun-operator/chart", "cetirun-portal/chart",
                "cetirun-ai-gateway/chart", "cetirun-connections-mcp/chart", "cetirun-observability/chart",
            ):
                shutil.copytree(ROOT / "app" / relative, base / relative, ignore=shutil.ignore_patterns("charts"))
            chart = base / "cetirun-platform"
            subprocess.run(["helm", "dependency", "build", "--skip-refresh", str(chart)],
                           env=OFFLINE_ENV, check=True, text=True, capture_output=True, timeout=60)
            value_file = base / "values.json"
            value_file.write_text(json.dumps(values["platform"]))
            command = ["helm", "template", "platform", str(chart), "--namespace", "cetirun-system",
                       "--kube-version", "1.37.0", "--values", str(value_file)]
            success = subprocess.run(command, env=OFFLINE_ENV, text=True, capture_output=True)
            self.assertEqual(success.returncode, 0, success.stderr)
            values["platform"]["cetirun-portal"]["sandboxStorage"] = {
                "platformNamespace": "", "platformConfigMapName": "", "defaultStorageClassName": ""}
            value_file.write_text(json.dumps(values["platform"]))
            omitted = subprocess.run(command, env=OFFLINE_ENV, text=True, capture_output=True)
            self.assertNotEqual(omitted.returncode, 0)
            self.assertIn("sandboxStorage must exactly match", omitted.stderr)
            values["platform"]["cetirun-portal"]["sandboxStorage"]["platformConfigMapName"] = "sandbox-platform"
            values["platform"]["cetirun-portal"]["sandboxStorage"]["platformNamespace"] = "other-system"
            value_file.write_text(json.dumps(values["platform"]))
            failed = subprocess.run(command, env=OFFLINE_ENV, text=True, capture_output=True)
            self.assertNotEqual(failed.returncode, 0)
            self.assertIn("sandboxStorage must exactly match", failed.stderr)


if __name__ == "__main__":
    unittest.main()
