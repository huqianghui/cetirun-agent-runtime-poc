# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Rendered contracts for the native CetiRun Connections MCP integration."""

import json
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml


CHART = Path(__file__).resolve().parents[1]
TARGET = "https://cetirun-connections-mcp.cetirun-tools.svc.cluster.local/mcp"


class ConnectionsMCPChartTests(unittest.TestCase):
    def render(self, agent_type, *extra):
        command = [
            "helm",
            "template",
            "cetirun",
            str(CHART),
            "--set",
            f"agentType={agent_type}",
            "--set-string",
            "secrets.model=lint-model",
            "--set-string",
            "secrets.apiBase=http://gateway.example.svc.cluster.local:4000/v1",
            "--set-string",
            "aiGateway.keySecretName=lint-model-key",
            "--set-string",
            "hermes.env.HERMES_WEBAPP_PUBLIC_URL=https://agents.example.com/ui/cetirun",
            "--set-json",
            'aiGateway.models=[{"id":"lint-model","name":"Lint model"}]',
            "--set",
            "connectionsMCP.enabled=true",
            "--set-string",
            f"connectionsMCP.url={TARGET}",
            "--set-string",
            "connectionsMCP.audience=cetirun-connections-mcp",
            "--set-string",
            "connectionsMCP.caSecretName=cetirun-connections-mcp-ca",
            "--set",
            "serviceAccount.create=true",
            "--set-string",
            "serviceAccount.name=agent-workload",
            "--set",
            "serviceAccount.automountToken=false",
            "--set",
            "networkPolicy.enabled=true",
            "--set",
            "sidecarScope.enabled=true",
            *extra,
        ]
        if agent_type == "openclaw":
            command.extend(["--set-string", "secrets.openclawGatewayToken=lint-token"])
        return subprocess.run(command, check=False, capture_output=True, text=True)

    def test_common_serviceaccount_names_follow_kubernetes_subdomains(self):
        common = {
            "contractVersion": "v1", "reconciled": True, "executionHeld": False,
            "podResources": {}, "helperResources": {},
            "securityContext": {"runAsNonRoot": True}, "scratchSizeLimit": "1Gi",
            "ownerReferences": [{"apiVersion": "cetirun.microsoft.com/v1alpha1",
                                 "kind": "Agent", "name": "synthetic-agent", "uid": "synthetic-uid"}],
            "podLabels": {"sandbox.cetirun.microsoft.com/agent-uid": "synthetic-uid"},
            "dnsConfig": {"nameservers": ["10.0.0.10"]},
            "identityVolumes": [], "identityMounts": [], "identityEnvironment": [],
            "objects": [{"apiVersion": "v1", "kind": "ConfigMap",
                         "metadata": {"name": "sandbox-object"}}],
        }
        names = [
            ("approved.identity", True), ("a" * 64, True),
            ("approved." + "a" * 63 + ".identity", True), ("a" * 253, True),
            ("", False), ("Uppercase", False), ("a..b", False), ("-a", False),
            ("a.-b", False), ("a-.b", False), ("a.", False),
            ("a_b", False), ("a/b", False), ("a" * 254, False),
        ]
        for agent_type in ("openclaw", "hermes"):
            for name, valid in names:
                with self.subTest(agent_type=agent_type, name=name):
                    rendered = self.render(
                        agent_type, "--set-json", "sandboxWorkload=" + json.dumps(common),
                        "--set", "runtimeClassName=kata",
                        "--set-string", "podAnnotations.sidecar\\.istio\\.io/inject=false",
                        "--set", "sidecarScope.enabled=false",
                        "--set", "persistence.existingClaim=sandbox-home",
                        "--set", "persistence.useStatefulSetVolumeClaim=false",
                        "--set", "skills.existingClaim=sandbox-skills",
                        "--set", "execRestriction.enabled=false",
                        "--set", "serviceAccount.create=false",
                        "--set-string", "serviceAccount.name=" + name,
                    )
                    self.assertEqual(rendered.returncode == 0, valid, rendered.stderr)
                    if valid:
                        workload = next(doc for doc in yaml.safe_load_all(rendered.stdout)
                                        if doc and doc.get("kind") == "StatefulSet")
                        self.assertEqual(
                            workload["spec"]["template"]["spec"]["serviceAccountName"], name,
                        )

    def test_ordinary_connections_serviceaccount_name_limit_is_unchanged(self):
        rendered = self.render("hermes", "--set-string", "serviceAccount.name=" + "a" * 64)
        self.assertNotEqual(rendered.returncode, 0)
        self.assertIn("ServiceAccount", rendered.stderr)

    def test_native_connections_mcp_has_projected_identity_ca_and_no_broker_path(self):
        for agent_type in ("openclaw", "hermes"):
            with self.subTest(agent_type=agent_type):
                rendered = self.render(agent_type)
                self.assertEqual(rendered.returncode, 0, rendered.stderr)
                output = rendered.stdout
                for expected in (
                    "serviceAccountName: agent-workload",
                    "automountServiceAccountToken: false",
                    "mountPath: /var/run/cetirun/connections",
                    "mountPath: /var/run/cetirun/connections-ca",
                    "defaultMode: 0440",
                    'audience: "cetirun-connections-mcp"',
                    "expirationSeconds: 600",
                    'secretName: "cetirun-connections-mcp-ca"',
                    "key: ca.crt",
                    f'value: "{TARGET}"',
                    "kubernetes.io/metadata.name: cetirun-tools",
                    "app.kubernetes.io/name: cetirun-connections-mcp",
                ):
                    self.assertIn(expected, output)
                self.assertNotIn("CETIRUN_AGENT_BROKER_URL", output)
                self.assertNotIn("cetirun-agent-broker", output)
                self.assertNotIn("cetirun-credentials", output)

    def test_native_configs_use_streamable_http_without_static_auth_headers(self):
        expectations = {
            "openclaw": ('"transport": "streamable-http"', '"sslVerify": true'),
            "hermes": ("transport: streamable-http", "strict_redirect_headers: true"),
        }
        for agent_type, expected_values in expectations.items():
            with self.subTest(agent_type=agent_type):
                rendered = self.render(agent_type)
                self.assertEqual(rendered.returncode, 0, rendered.stderr)
                self.assertIn(TARGET, rendered.stdout)
                for expected in expected_values:
                    self.assertIn(expected, rendered.stdout)
                self.assertNotIn("Authorization: Bearer", rendered.stdout)
                self.assertNotIn('"headers"', rendered.stdout)

    def test_connections_mcp_preserves_native_memory_integration(self):
        for agent_type in ("openclaw", "hermes"):
            with self.subTest(agent_type=agent_type):
                rendered = self.render(
                    agent_type, "--set", "memoryContext.enabled=true",
                )
                self.assertEqual(rendered.returncode, 0, rendered.stderr)
                for expected in (
                    "CETIRUN_MEMORY_", "cetirun-memory-context",
                    "mountPath: /var/run/cetirun/connections",
                    "mountPath: /var/run/cetirun/connections-ca", TARGET,
                ):
                    self.assertIn(expected, rendered.stdout)

    def test_connections_mcp_preserves_other_native_servers(self):
        custom_server = {"url": "https://mcp.example.com/mcp", "timeout": 45}
        for agent_type in ("hermes", "openclaw"):
            for enabled in (True, False):
                with self.subTest(agent_type=agent_type, enabled=enabled):
                    servers = {
                        "example": custom_server,
                        "cetirun-connections": {
                            "url": "https://old.example.com/mcp",
                            "headers": {"X-Example": "old"},
                        },
                    }
                    native = ({"mcp_servers": servers} if agent_type == "hermes"
                              else {"mcp": {"servers": servers, "enabled": True}})
                    with tempfile.TemporaryDirectory() as directory:
                        values = Path(directory) / "values.yaml"
                        values.write_text(yaml.safe_dump({
                            agent_type: {"config": native},
                            "connectionsMCP": {"enabled": enabled},
                        }), encoding="utf-8")
                        rendered = self.render(
                            agent_type, "-f", str(values),
                            "--set", f"connectionsMCP.enabled={str(enabled).lower()}",
                        )
                    self.assertEqual(rendered.returncode, 0, rendered.stderr)
                    filename = ("hermes-config.yaml" if agent_type == "hermes"
                                else "openclaw.json")
                    configmap = next(
                        doc for doc in yaml.safe_load_all(rendered.stdout)
                        if doc and doc.get("kind") == "ConfigMap"
                        and filename in doc.get("data", {})
                    )
                    config = yaml.safe_load(configmap["data"][filename])
                    actual = (config["mcp_servers"] if agent_type == "hermes"
                              else config["mcp"]["servers"])
                    self.assertEqual(actual["example"], custom_server)
                    if agent_type == "openclaw":
                        self.assertTrue(config["mcp"]["enabled"])
                    if enabled:
                        expected = {"url": TARGET, "transport": "streamable-http"}
                        if agent_type == "hermes":
                            expected.update(strict_redirect_headers=True, ssl_verify=True)
                        else:
                            expected["sslVerify"] = True
                        self.assertEqual(actual["cetirun-connections"], expected)
                    else:
                        self.assertNotIn("cetirun-connections", actual)

    def test_custom_rejects_identity_and_connections_mcp(self):
        for extra, expected in (
            ([], "Custom runtimes do not support CetiRun Connections MCP"),
            (["--set", "connectionsMCP.enabled=false", "--set", "agentIdentity.enabled=true"],
             "Custom runtimes do not support Agent Identity"),
        ):
            with self.subTest(expected=expected):
                rendered = self.render("custom", *extra)
                self.assertNotEqual(rendered.returncode, 0)
                self.assertIn(expected, rendered.stderr)

    def test_connections_mcp_rejects_a_held_sandbox_workload(self):
        with tempfile.TemporaryDirectory() as directory:
            values = Path(directory) / "sandbox-values.yaml"
            values.write_text(
                """\
replicaCount: 0
runtimeClassName: gvisor
podAnnotations:
  sidecar.istio.io/inject: "false"
sidecarScope:
  enabled: false
persistence:
  existingClaim: sandbox-home
  useStatefulSetVolumeClaim: false
skills:
  existingClaim: sandbox-skills
sandboxWorkload:
  contractVersion: v1
  executionHeld: true
  podResources: {}
  helperResources: {}
  securityContext:
    runAsNonRoot: true
  scratchSizeLimit: 1Gi
  ownerReferences:
    - apiVersion: v1
      kind: ConfigMap
      name: sandbox-owner
      uid: 00000000-0000-0000-0000-000000000000
  podLabels: {}
  dnsConfig:
    nameservers:
      - 10.0.0.10
  identityVolumes: []
  identityMounts: []
  identityEnvironment: []
  objects:
    - apiVersion: v1
      kind: ConfigMap
      metadata:
        name: sandbox-object
""",
                encoding="utf-8",
            )
            rendered = self.render(
                "hermes", "-f", str(values), "--set", "sidecarScope.enabled=false"
            )
        self.assertNotEqual(rendered.returncode, 0)
        self.assertIn(
            "Sandbox identities must use compiler-owned explicit projections",
            rendered.stderr,
        )


class ConnectionsMCPServiceChartTests(unittest.TestCase):
    def render(self, *extra):
        return subprocess.run([
            "helm", "template", "connections",
            str(CHART.parent / "cetirun-connections-mcp" / "chart"),
            "--namespace", "cetirun-tools",
            "--set", "tls.secretName=synthetic-serving-tls",
            "--set", "broker.clientTLSSecretName=synthetic-broker-tls",
            "--set", "broker.baseURL=https://broker.example.com",
            "--set", "istio.sidecarInjection=true",
            "--set", "istio.revision=asm-1-28",
            *extra,
        ], check=False, capture_output=True, text=True)

    def test_native_peer_preserves_application_tls_and_named_authority_reads(self):
        rendered = self.render(
            "--set", "nativeSandbox.enabled=true",
            "--set", "nativeSandbox.operatorNamespace=cetirun-system",
            "--set", "nativeSandbox.peerUID=synthetic-connections-peer",
        )
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        documents = [doc for doc in yaml.safe_load_all(rendered.stdout) if doc]
        by_kind = {doc["kind"]: doc for doc in documents}
        peer_label = "sandbox.cetirun.microsoft.com/peer-uid"
        pod = by_kind["Deployment"]["spec"]["template"]
        self.assertEqual(
            pod["metadata"]["labels"][peer_label], "synthetic-connections-peer",
        )
        self.assertEqual(
            by_kind["Service"]["spec"]["selector"][peer_label],
            pod["metadata"]["labels"][peer_label],
        )
        environment = {item["name"]: item.get("value")
                       for item in pod["spec"]["containers"][0]["env"]}
        self.assertEqual(environment["OPERATOR_NAMESPACE"], "cetirun-system")
        self.assertIn("TLS_CERT_FILE", environment)
        self.assertIn("BROKER_TLS_CERT_FILE", environment)
        self.assertEqual(
            by_kind["PeerAuthentication"]["spec"]["portLevelMtls"],
            {"8443": {"mode": "DISABLE"}},
        )
        self.assertEqual(
            by_kind["AuthorizationPolicy"]["spec"]["rules"],
            [{"to": [{"operation": {"ports": ["8443"]}}]}],
        )
        self.assertEqual(
            by_kind["DestinationRule"]["spec"]["trafficPolicy"]["tls"],
            {"mode": "DISABLE"},
        )
        self.assertEqual(
            by_kind["DestinationRule"]["spec"]["host"],
            by_kind["Service"]["metadata"]["name"] + ".cetirun-tools.svc.cluster.local",
        )
        rules = by_kind["ClusterRole"]["rules"]
        authority = [rule for rule in rules
                     if rule["resources"] == ["validatingwebhookconfigurations"]]
        self.assertEqual(authority, [{
            "apiGroups": ["admissionregistration.k8s.io"],
            "resources": ["validatingwebhookconfigurations"],
            "resourceNames": ["cetirun-sandbox-pods"],
            "verbs": ["get"],
        }])
        self.assertFalse(any("secrets" in rule["resources"] for rule in rules))
        for rule in rules:
            if "pods" in rule["resources"]:
                self.assertEqual(rule["verbs"], ["get"])
        admission_egress = by_kind["NetworkPolicy"]["spec"]["egress"][0]
        self.assertEqual(
            admission_egress["to"][0]["namespaceSelector"]["matchLabels"],
            {"kubernetes.io/metadata.name": "cetirun-system"},
        )
        self.assertEqual(
            admission_egress["ports"],
            [{"protocol": "TCP", "port": 443}, {"protocol": "TCP", "port": 9443}],
        )

    def test_default_keeps_strict_mesh_and_no_native_authority_permission(self):
        rendered = self.render()
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        documents = {doc["kind"]: doc for doc in yaml.safe_load_all(rendered.stdout)
                     if doc}
        authentication = documents["PeerAuthentication"]["spec"]
        self.assertEqual(authentication["mtls"], {"mode": "STRICT"})
        self.assertNotIn("portLevelMtls", authentication)
        self.assertNotIn("validatingwebhookconfigurations", rendered.stdout)
        self.assertNotIn("DestinationRule", documents)

    def test_native_configuration_rejects_incomplete_or_wrong_purpose_inputs(self):
        for extra, error in (
            ([], "nativeSandbox.peerUID"),
            (["--set", "nativeSandbox.peerUID=synthetic-peer",
              "--set", "nativeSandbox.operatorNamespace=invalid/namespace"],
             "nativeSandbox.operatorNamespace"),
            (["--set", "nativeSandbox.peerUID=synthetic-peer",
              "--set", "workloadTokenAudience=api://AzureADTokenExchange"],
             "Connections requires its own purpose audience"),
        ):
            with self.subTest(error=error):
                rendered = self.render("--set", "nativeSandbox.enabled=true", *extra)
                self.assertNotEqual(rendered.returncode, 0)
                self.assertIn(error, rendered.stderr)


if __name__ == "__main__":
    unittest.main()
