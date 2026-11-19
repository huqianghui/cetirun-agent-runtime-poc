# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline checks for the optional Portal Chat installation contract."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
OFFLINE = {**os.environ, "KUBECONFIG": os.devnull, "HELM_REGISTRY_CONFIG": os.devnull, "HELM_REPOSITORY_CONFIG": os.devnull}


@unittest.skipUnless(shutil.which("helm"), "requires Helm")
class PortalChatChartTests(unittest.TestCase):
    def render(self, chart, values, *, success=True):
        result = subprocess.run(
            ["helm", "template", "chat-test", str(chart), "--namespace", "test-platform", "--kube-version", "1.37.0", "--values", "-"],
            input=json.dumps(values), text=True, capture_output=True, env=OFFLINE, timeout=30, check=False,
        )
        if not success:
            self.assertNotEqual(result.returncode, 0)
            return result.stderr
        self.assertEqual(result.returncode, 0, result.stderr)
        return [value for value in yaml.safe_load_all(result.stdout) if value]

    @staticmethod
    def deployment_env(objects, name):
        deployment = next(value for value in objects if value["kind"] == "Deployment" and value["metadata"]["name"] == name)
        return {item["name"]: item.get("value", item.get("valueFrom")) for item in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}

    def test_portal_defaults_do_not_enable_chat_or_add_storage(self):
        objects = self.render(ROOT / "app/cetirun-portal/chart", {"istio": {"host": "portal.example.com"}})
        env = self.deployment_env(objects, "cetirun-portal")
        self.assertEqual(env["FEATURE_CUSTOM_CHAT_ENABLED"], "false")
        self.assertFalse(any(value["kind"] in {"PersistentVolumeClaim", "StatefulSet"} for value in objects))
        roles = [value for value in objects if value["kind"] == "ClusterRole"]
        self.assertEqual(len(roles), 1)
        self.assertFalse(any("networkpolicies" in rule["resources"] for role in roles for rule in role["rules"]))
        self.assertFalse(any(value["kind"] == "Role" and value["metadata"]["name"].endswith("-chat-mtls") for value in objects))

    def test_suspension_reader_is_scoped_without_chat(self):
        objects = self.render(ROOT / "app/cetirun-portal/chart", {
            "istio": {"host": "portal.example.com"},
            "chat": {"sandboxStateNamespace": "operator-state"},
        })
        role = next(value for value in objects if value["kind"] == "Role" and value["metadata"]["name"] == "cetirun-portal-suspension-state")
        self.assertEqual(role["metadata"]["namespace"], "operator-state")
        self.assertEqual(role["rules"], [{"apiGroups": [""], "resources": ["configmaps"], "verbs": ["get"]}])
        binding = next(value for value in objects if value["kind"] == "RoleBinding" and value["metadata"]["name"] == role["metadata"]["name"])
        self.assertEqual(binding["metadata"]["namespace"], "operator-state")
        self.assertEqual(binding["roleRef"], {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": role["metadata"]["name"]})
        self.assertEqual(binding["subjects"], [{"kind": "ServiceAccount", "name": "cetirun-portal", "namespace": "test-platform"}])

    def test_enabled_portal_has_scoped_reader_and_no_retry_route(self):
        objects = self.render(ROOT / "app/cetirun-portal/chart", {"istio": {"host": "portal.example.com"}, "chat": {"enabled": True, "identityRealm": "synthetic"}})
        env = self.deployment_env(objects, "cetirun-portal")
        self.assertEqual(env["FEATURE_CUSTOM_CHAT_ENABLED"], "true")
        self.assertEqual(json.loads(env["CHAT_CALLER_LABELS"]), {"app.kubernetes.io/name": "cetirun-portal", "app.kubernetes.io/instance": "chat-test"})
        route = next(value for value in objects if value["kind"] == "VirtualService")["spec"]["http"][0]
        self.assertEqual(route["retries"]["attempts"], 0)
        self.assertEqual(route["timeout"], "610s")
        role = next(value for value in objects if value["kind"] == "ClusterRole" and value["metadata"]["name"] == "cetirun-portal-chat")
        self.assertEqual(role["rules"], [
            {"apiGroups": [""], "resources": ["services"], "verbs": ["get"]},
            {"apiGroups": ["networking.k8s.io"], "resources": ["networkpolicies"], "verbs": ["get"]},
            {"apiGroups": ["cilium.io"], "resources": ["ciliumnetworkpolicies"], "verbs": ["get"]},
            {"apiGroups": ["networking.istio.io"], "resources": ["virtualservices"], "verbs": ["get"]},
        ])
        binding = next(value for value in objects if value["kind"] == "ClusterRoleBinding" and value["metadata"]["name"] == "cetirun-portal-chat")
        self.assertEqual(binding["roleRef"]["name"], role["metadata"]["name"])
        self.assertEqual(binding["subjects"], [{"kind": "ServiceAccount", "name": "cetirun-portal", "namespace": "test-platform"}])
        state_role = next(value for value in objects if value["kind"] == "Role" and value["metadata"]["name"] == "cetirun-portal-chat-sandbox-state")
        self.assertEqual(state_role["metadata"]["namespace"], "cetirun-system")
        self.assertEqual(state_role["rules"], [{"apiGroups": [""], "resources": ["configmaps"], "verbs": ["get"]}])
        state_binding = next(value for value in objects if value["kind"] == "RoleBinding" and value["metadata"]["name"] == state_role["metadata"]["name"])
        self.assertEqual(state_binding["metadata"]["namespace"], "cetirun-system")
        self.assertEqual(state_binding["subjects"], binding["subjects"])
        self.assertEqual(env["SANDBOX_STATE_NAMESPACE"], "cetirun-system")
        tls_role = next(value for value in objects if value["kind"] == "Role" and value["metadata"]["name"] == "cetirun-portal-chat-mtls")
        self.assertEqual(tls_role["metadata"]["namespace"], "test-platform")
        self.assertEqual(tls_role["rules"], [{"apiGroups": ["networking.istio.io"], "resources": ["destinationrules"], "verbs": ["get"]}])
        tls_binding = next(value for value in objects if value["kind"] == "RoleBinding" and value["metadata"]["name"] == tls_role["metadata"]["name"])
        self.assertEqual(tls_binding["metadata"]["namespace"], "test-platform")
        self.assertEqual(tls_binding["roleRef"], {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": tls_role["metadata"]["name"]})
        self.assertEqual(tls_binding["subjects"], binding["subjects"])

    def test_explicit_state_namespace_is_bound_in_rbac_and_environment(self):
        objects = self.render(ROOT / "app/cetirun-portal/chart", {
            "istio": {"host": "portal.example.com"},
            "chat": {"enabled": True, "identityRealm": "synthetic", "sandboxStateNamespace": "operator-state"},
        })
        env = self.deployment_env(objects, "cetirun-portal")
        self.assertEqual(env["SANDBOX_STATE_NAMESPACE"], "operator-state")
        readers = [value for value in objects if value["kind"] in {"Role", "RoleBinding"} and value["metadata"]["name"] == "cetirun-portal-chat-sandbox-state"]
        self.assertEqual(len(readers), 2)
        self.assertTrue(all(value["metadata"]["namespace"] == "operator-state" for value in readers))
        tls_readers = [value for value in objects if value["kind"] in {"Role", "RoleBinding"} and value["metadata"]["name"] == "cetirun-portal-chat-mtls"]
        self.assertEqual(len(tls_readers), 2)
        self.assertTrue(all(value["metadata"]["namespace"] == "test-platform" for value in tls_readers))

    def test_enabled_portal_requires_identity_realm(self):
        error = self.render(ROOT / "app/cetirun-portal/chart", {"istio": {"host": "portal.example.com"}, "chat": {"enabled": True}}, success=False)
        self.assertIn("identityRealm", error)

    def test_umbrella_generates_the_same_caller_as_the_portal(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-chat-charts-") as scratch:
            base = Path(scratch)
            for relative in ("cetirun-platform", "cetirun-operator/chart", "cetirun-portal/chart", "cetirun-ai-gateway/chart", "cetirun-connections-mcp/chart", "cetirun-agent-broker/chart", "cetirun-observability/chart"):
                shutil.copytree(ROOT / "app" / relative, base / relative, ignore=shutil.ignore_patterns("charts", ".bin", "node_modules"))
            subprocess.run(["helm", "dependency", "build", str(base / "cetirun-platform"), "--skip-refresh"], env=OFFLINE, capture_output=True, text=True, check=True, timeout=30)
            objects = self.render(base / "cetirun-platform", {
                "istio": {"enabled": True},
                "global": {"portalChat": {"enabled": True, "identityRealm": "synthetic"}},
                "cetirun-operator": {"domainSuffix": "agents.example.com", "namespace": "test-platform", "cetiadmin": {"enabled": False}},
                "cetirun-portal": {"istio": {"host": "portal.example.com"}, "aiGateway": {"catalogNamespace": "test-platform"}},
            })
            portal = self.deployment_env(objects, "cetirun-portal")
            self.assertEqual(portal["SANDBOX_STATE_NAMESPACE"], "test-platform")
            operator = self.deployment_env(objects, "cetirun-operator")
            access = json.loads(operator["CETIRUN_CHAT_ACCESS"])
            self.assertTrue(access["enabled"])
            self.assertEqual(access["callers"], [{"namespace": "test-platform", "podLabels": json.loads(portal["CHAT_CALLER_LABELS"])}])


if __name__ == "__main__":
    unittest.main()
