# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Tests for cluster version preflight and Container Insights agent settings.

deploy.sh runs against stand-ins for az and kubectl that log their arguments.
The settings are created on a cluster that has none; settings a cluster
already has are checked and never replaced, since replacing them would drop
the cluster's own collection settings.
"""

import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
DEPLOY = ROOT / "infra" / "cluster" / "deploy.sh"
SETTINGS_FILE = ROOT / "infra" / "cluster" / "container-azm-ms-agentconfig.yaml"
VALIDATOR = ROOT / "scripts" / "validate-cluster-kubernetes.py"

FAKE_BICEP = """#!/usr/bin/env sh
echo "bicep $*" >> "$CALLS_LOG"
[ "$FAKE_BICEP_FAIL" = "1" ] && exit 1
if [ "$1" = "build" ]; then
  cat "$FAKE_TEMPLATE"
else
  cat "$FAKE_BUNDLE"
fi
"""

FAKE_AZ = """#!/usr/bin/env sh
echo "az $*" >> "$CALLS_LOG"
case "$1 $2" in
  "extension add")
    if [ "$REPLACE_PARAMETERS" = "1" ]; then
      echo '{"parameters":{"kubernetesVersion":{"value":"1.34"}}}' > "$PARAM_FILE"
    fi ;;
  "feature show") echo Registered ;;
  "group show") echo westus3 ;;
  "deployment group")
    if [ "$3" = "show" ]; then
      echo example-cluster
    else
      python3 -c '
import json, os, pathlib, stat, sys
args = sys.argv
template_path = pathlib.Path(args[args.index("--template-file") + 1])
params_arg = args[args.index("--parameters") + 1]
assert params_arg.startswith("@"), "Azure CLI JSON parameter files must use @path"
params_path = pathlib.Path(params_arg[1:])
template = json.loads(template_path.read_text())
params = json.loads(params_path.read_text())
version = params["parameters"].get("kubernetesVersion", {}).get("value", template["parameters"]["kubernetesVersion"]["defaultValue"])
assert stat.S_IMODE(template_path.parent.stat().st_mode) == 0o700
assert stat.S_IMODE(template_path.stat().st_mode) == 0o600
assert stat.S_IMODE(params_path.stat().st_mode) == 0o600
with open(os.environ["CALLS_LOG"], "a") as log:
    print("snapshot-version", version, file=log)
' "$@" || exit 1
      echo '{}'
    fi ;;
  "aks show") echo https://issuer.example.invalid/ ;;
esac
exit 0
"""

FAKE_KUBECTL = """#!/usr/bin/env sh
echo "kubectl $*" >> "$CALLS_LOG"
if [ "$1 $2" = "get configmap" ]; then
  [ -n "$FAKE_KUBECTL_FAIL" ] && exit 1
  case "$*" in
    *--ignore-not-found*)
      [ -f "$FAKE_SETTINGS" ] && echo configmap/container-azm-ms-agentconfig
      exit 0 ;;
    *jsonpath*)
      cat "$FAKE_SETTINGS"
      exit 0 ;;
  esac
fi
exit 0
"""


def shipped_log_settings():
    """The log-data-collection-settings text of the shipped settings file."""
    lines = SETTINGS_FILE.read_text(encoding="utf-8").splitlines()
    start = next(i for i, line in enumerate(lines) if line.startswith("  log-data-collection-settings:"))
    block = []
    for line in lines[start + 1:]:
        if line.strip() and not line.startswith("    "):
            break
        block.append(line[4:])
    return "\n".join(block) + "\n"


class ClusterDeployTests(unittest.TestCase):
    def run_deploy(self, existing_settings=None, kubectl_fails=False, *,
                   parameters=None, default_version="1.37", bicep_fails=False,
                   json_parameters=False, replace_parameters=False):
        with tempfile.TemporaryDirectory() as directory:
            temp = pathlib.Path(directory)
            for name, body in (("az", FAKE_AZ), ("kubectl", FAKE_KUBECTL), ("bicep", FAKE_BICEP)):
                stub = temp / name
                stub.write_text(body, encoding="utf-8")
                stub.chmod(0o755)
            calls = temp / "calls.log"
            calls.write_text("", encoding="utf-8")
            template = json.dumps({"parameters": {"kubernetesVersion": {"defaultValue": default_version}}})
            parameter_text = json.dumps({"parameters": parameters or {}})
            (temp / "template.json").write_text(template, encoding="utf-8")
            (temp / "bundle.json").write_text(json.dumps({
                "templateJson": template, "parametersJson": parameter_text,
            }), encoding="utf-8")
            parameter_file = temp / ("example.json" if json_parameters else "example.bicepparam")
            parameter_file.write_text(parameter_text, encoding="utf-8")
            environment = {
                **os.environ,
                "PATH": f"{temp}{os.pathsep}{os.environ['PATH']}",
                "CALLS_LOG": str(calls),
                "FAKE_SETTINGS": str(temp / "settings.toml"),
                "RESOURCE_GROUP": "example-rg",
                "LOCATION": "westus3",
                "PARAM_FILE": str(parameter_file),
                "FAKE_TEMPLATE": str(temp / "template.json"),
                "FAKE_BUNDLE": str(temp / "bundle.json"),
                "FAKE_BICEP_FAIL": "1" if bicep_fails else "0",
                "FAKE_KUBECTL_FAIL": "1" if kubectl_fails else "",
                "REPLACE_PARAMETERS": "1" if replace_parameters else "0",
                "TMPDIR": str(temp),
            }
            if existing_settings is not None:
                (temp / "settings.toml").write_text(existing_settings, encoding="utf-8")
            result = subprocess.run(["bash", str(DEPLOY)], capture_output=True, text=True, env=environment,
                                    check=False)
            self.assertFalse(list(temp.glob("tmp.*")), "Deployment snapshots must be removed on exit")
            if replace_parameters:
                self.assertEqual(json.loads(parameter_file.read_text())["parameters"]["kubernetesVersion"]["value"], "1.34")
            return result, calls.read_text(encoding="utf-8").splitlines()

    def assert_preflight_blocked(self, result, calls):
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("stable Kubernetes 1.37 or newer", result.stderr)
        self.assertFalse([c for c in calls if c.startswith(("az ", "kubectl "))], calls)

    def test_default_is_checked_before_any_azure_call(self):
        result, calls = self.run_deploy()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("preflight passed: 1.37", result.stdout)
        self.assertTrue(calls[0].startswith("bicep build-params "), calls)
        self.assertIn(f"--bicep-file {ROOT / 'infra/cluster/main.bicep'}", calls[0])
        self.assertIn("--no-restore", calls[0])
        self.assertIn("--stdout", calls[0])

    def test_an_outdated_template_default_also_stops_before_writes(self):
        self.assert_preflight_blocked(*self.run_deploy(default_version="1.34"))

    def test_old_prerelease_and_unresolved_overrides_stop_before_writes(self):
        versions = ("1.34", "1.34.12", "1.36.99", "1.9.9", "1.37.0-alpha.1",
                    "1.38.0-rc.1", "", "latest", "v1.37.0", "1.37.0+build", "1.037",
                    "[parameters('version')]", "private-version-sentinel", None, 137, {})
        for use_json in (False, True):
            for version in versions:
                with self.subTest(version=version, json_parameters=use_json):
                    result, calls = self.run_deploy(parameters={"kubernetesVersion": {"value": version}},
                                                   json_parameters=use_json)
                    self.assert_preflight_blocked(result, calls)
                    self.assertNotIn("private-version-sentinel", result.stdout + result.stderr)

    def test_stable_minimum_patch_and_newer_overrides_are_accepted(self):
        for use_json in (False, True):
            for version in ("1.37", "1.37.0", "1.37.12", "1.38", "1.100.0", "2.0.0"):
                with self.subTest(version=version, json_parameters=use_json):
                    result, calls = self.run_deploy(parameters={"kubernetesVersion": {"value": version}},
                                                   json_parameters=use_json)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn(f"preflight passed: {version}", result.stdout)
                    self.assertTrue(any(c.startswith("az deployment group create") for c in calls), calls)

    def test_compiler_failure_stops_before_writes(self):
        self.assert_preflight_blocked(*self.run_deploy(bicep_fails=True))

    def test_reference_cannot_silently_select_the_default(self):
        self.assert_preflight_blocked(*self.run_deploy(parameters={"kubernetesVersion": {"reference": {}}}))

    def test_malformed_json_parameters_stop_before_writes(self):
        self.assert_preflight_blocked(*self.run_deploy(parameters=["unexpected"], json_parameters=True))

    def test_deployment_uses_validated_snapshot_after_source_is_replaced(self):
        for use_json in (False, True):
            with self.subTest(json_parameters=use_json):
                result, calls = self.run_deploy(parameters={"kubernetesVersion": {"value": "1.38.1"}},
                                               json_parameters=use_json, replace_parameters=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual([c for c in calls if c.startswith("snapshot-version")],
                                 ["snapshot-version 1.38.1", "snapshot-version 1.38.1"])

    def assert_no_settings_written(self, calls):
        self.assertFalse([c for c in calls if c.startswith(("kubectl create", "kubectl apply", "kubectl patch",
                                                           "kubectl replace", "kubectl edit"))], calls)

    def test_a_cluster_without_settings_gets_them_before_anything_else(self):
        result, calls = self.run_deploy()
        self.assertEqual(result.returncode, 0, result.stderr)
        created = [i for i, c in enumerate(calls) if c == f"kubectl create -f {SETTINGS_FILE}"]
        self.assertEqual(len(created), 1, calls)
        gateway_api = [i for i, c in enumerate(calls) if c.startswith("az aks update")]
        self.assertTrue(gateway_api and created[0] < gateway_api[0], calls)

    def test_the_shipped_settings_pass_the_check(self):
        result, calls = self.run_deploy(existing_settings=shipped_log_settings())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_no_settings_written(calls)
        self.assertIn("left unchanged", result.stdout)
        self.assertNotIn("multiline", result.stderr)

    def test_existing_settings_that_turn_collection_off_are_left_unchanged(self):
        settings = textwrap.dedent("""\
            [log_collection_settings]
               [log_collection_settings.stdout]
                  enabled = true
                  exclude_namespaces = ["kube-system", "team-a"]
               [log_collection_settings.env_var]
                  enabled=false # set by the cluster's owner
            """)
        result, calls = self.run_deploy(existing_settings=settings)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_no_settings_written(calls)
        self.assertIn("multiline", result.stderr)
        self.assertTrue([c for c in calls if c.startswith("az aks update")], calls)

    def test_existing_settings_that_keep_collection_on_stop_the_script(self):
        cases = {
            "collection on": "[log_collection_settings.env_var]\n   enabled = true\n",
            "no env_var section": "[log_collection_settings.stdout]\n   exclude_namespaces = [\"team-a\"]\n",
            "off in another section": "[log_collection_settings.stdout]\n   enabled = false\n",
            "no log settings": "",
        }
        for name, settings in cases.items():
            with self.subTest(name):
                result, calls = self.run_deploy(existing_settings=settings)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn("left unchanged", result.stderr)
                self.assert_no_settings_written(calls)
                self.assertFalse([c for c in calls if c.startswith("az aks update")], calls)

    def test_settings_that_cannot_be_read_stop_the_script(self):
        result, calls = self.run_deploy(kubectl_fails=True)
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assert_no_settings_written(calls)
        self.assertFalse([c for c in calls if c.startswith("az aks update")], calls)


@unittest.skipUnless(shutil.which("bicep"), "Bicep CLI is required for compilation regression tests")
class CompiledVersionTests(unittest.TestCase):
    def test_real_template_default_and_computed_overrides(self):
        with tempfile.TemporaryDirectory() as directory:
            temp = pathlib.Path(directory)
            template = ROOT / "infra/cluster/main.bicep"
            params = temp / "example.bicepparam"
            required = f"using '{os.path.relpath(template, temp)}'\n" + textwrap.dedent("""\
                param resourceGroupName = 'example-rg'
                param location = 'westus3'
                param clusterName = 'example-cluster'
                param dnsPrefix = 'example-cluster'
                param nodeResourceGroup = 'example-rg-nodes'
                """)
            for extra, expected in (
                ("", "1.37"),
                ("var selected = '1.34.12'\nparam kubernetesVersion = selected\n", None),
                ("param kubernetesVersion = '1.38.1'\n", "1.38.1"),
            ):
                with self.subTest(extra=extra):
                    params.write_text(required + extra, encoding="utf-8")
                    result = subprocess.run(
                        [sys.executable, "-B", str(VALIDATOR), "--template", str(template),
                         "--parameters", str(params)], capture_output=True, text=True, check=False,
                    )
                    if expected is None:
                        self.assertEqual(result.returncode, 1, result.stdout)
                        self.assertIn("stable Kubernetes 1.37 or newer", result.stderr)
                    else:
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertIn(f"preflight passed: {expected}", result.stdout)
            self.assertEqual(list(temp.iterdir()), [params], "Compilation must not leave generated files")


if __name__ == "__main__":
    unittest.main()
