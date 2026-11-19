# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline contract tests for the standalone CetiRun Connections MCP Helm chart."""

import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "app/cetirun-connections-mcp/chart"
OFFLINE = {
    **os.environ,
    "KUBECONFIG": os.devnull,
    "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}

VALUES = {
    "tls": {"secretName": "synthetic-mcp-server-tls"},
    "broker": {
        "baseURL": "https://cetirun-agent-broker.cetirun-credentials.svc.cluster.local",
        "clientTLSSecretName": "synthetic-mcp-broker-client-tls",
    },
    "portal": {"baseURL": "https://portal.example.invalid"},
    "istio": {
        "enabled": True,
        "sidecarInjection": True,
        "revision": "asm-synthetic",
        "brokerNamespace": "cetirun-credentials",
        "controlPlaneNamespace": "aks-istio-system",
        "agentNamespaces": ["cetirun-agent-synthetic"],
    },
}


@unittest.skipUnless(shutil.which("helm"), "requires Helm 3 or 4")
class ConnectionsMCPChartTests(unittest.TestCase):
    def helm(self, values=None, *, namespace="cetirun-tools", lint=False):
        command = ["helm", "lint", "--strict", str(CHART)] if lint else [
            "helm", "template", "cetirun-connections-mcp-test", str(CHART),
            "--namespace", namespace, "--kube-version", "1.37.0",
        ]
        return subprocess.run(
            [*command, "--values", "-"],
            cwd=ROOT,
            env=OFFLINE,
            input=json.dumps(VALUES if values is None else values),
            text=True,
            capture_output=True,
            timeout=30,
            check=False,
        )

    def render(self, values=None, *, namespace="cetirun-tools"):
        lint = self.helm(values, namespace=namespace, lint=True)
        self.assertEqual(lint.returncode, 0, lint.stdout + lint.stderr)
        result = self.helm(values, namespace=namespace)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return [document for document in yaml.safe_load_all(result.stdout) if document]

    def reject(self, values=None, *, namespace="cetirun-tools", message):
        result = self.helm(values, namespace=namespace)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn(message, result.stderr)

    @staticmethod
    def document(objects, kind, name="cetirun-connections-mcp"):
        return next(
            document for document in objects
            if document["kind"] == kind and document["metadata"]["name"] == name
        )

    @staticmethod
    def update(values, path, value):
        updated = copy.deepcopy(values)
        current = updated
        for part in path[:-1]:
            current = current.setdefault(part, {})
        current[path[-1]] = value
        return updated

    def test_standalone_chart_exposes_only_its_https_mcp_workload(self):
        objects = self.render()
        deployments = [document for document in objects if document["kind"] == "Deployment"]
        self.assertEqual([deployment["metadata"]["name"] for deployment in deployments], ["cetirun-connections-mcp"])
        self.assertFalse(any(
            document["kind"] in {"StatefulSet", "DaemonSet", "Job", "CronJob"}
            for document in objects
        ))

        service = self.document(objects, "Service")
        self.assertEqual(service["spec"]["ports"], [{"name": "https", "port": 443, "targetPort": "https"}])
        deployment = deployments[0]
        container = deployment["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(deployment["spec"]["template"]["spec"]["serviceAccountName"], "cetirun-connections-mcp")
        self.assertEqual(service["spec"]["selector"], deployment["spec"]["selector"]["matchLabels"])
        self.assertEqual(service["spec"]["selector"]["app.kubernetes.io/name"], "cetirun-connections-mcp")
        env = {item["name"]: item.get("value") for item in container["env"]}
        self.assertEqual(env["WORKLOAD_TOKEN_AUDIENCE"], "cetirun-connections-mcp")
        self.assertEqual(container["ports"], [{"name": "https", "containerPort": 8443}])
        for probe in (container["readinessProbe"], container["livenessProbe"]):
            self.assertEqual(probe["httpGet"]["scheme"], "HTTPS")
            self.assertEqual(probe["httpGet"]["port"], "https")

        peer_authentication = self.document(objects, "PeerAuthentication")
        self.assertEqual(peer_authentication["spec"]["mtls"], {"mode": "STRICT"})
        authorization = self.document(objects, "AuthorizationPolicy")
        self.assertEqual(authorization["spec"]["rules"][0]["to"], [{"operation": {"ports": ["8443"]}}])

    def test_mcp_and_broker_credentials_are_mounted_as_distinct_read_only_material(self):
        objects = self.render()
        pod = self.document(objects, "Deployment")["spec"]["template"]["spec"]
        container = pod["containers"][0]
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertEqual(
            {mount["name"]: mount for mount in container["volumeMounts"]},
            {
                "tls": {"name": "tls", "mountPath": "/var/run/cetirun/tls", "readOnly": True},
                "broker-tls": {"name": "broker-tls", "mountPath": "/var/run/cetirun/broker-tls", "readOnly": True},
                "kubernetes-api-credential": {"name": "kubernetes-api-credential", "mountPath": "/var/run/cetirun/kubernetes", "readOnly": True},
            },
        )
        volumes = {volume["name"]: volume for volume in pod["volumes"]}
        self.assertEqual(volumes["tls"]["secret"]["secretName"], "synthetic-mcp-server-tls")
        self.assertEqual(volumes["broker-tls"]["secret"]["secretName"], "synthetic-mcp-broker-client-tls")
        self.assertNotIn("subPath", json.dumps(container["volumeMounts"]))
        projected = volumes["kubernetes-api-credential"]["projected"]
        self.assertEqual(projected["defaultMode"], 0o440)
        self.assertEqual(projected["sources"][0]["serviceAccountToken"], {"path": "token", "expirationSeconds": 3600})
        self.assertEqual(projected["sources"][1]["configMap"]["name"], "kube-root-ca.crt")

        env = {item["name"]: item.get("value") for item in container["env"]}
        self.assertEqual(env["LISTEN_ADDRESS"], ":8443")
        self.assertEqual(env["AGENT_BROKER_BASE_URL"], VALUES["broker"]["baseURL"])
        self.assertEqual(env["TLS_CERT_FILE"], "/var/run/cetirun/tls/tls.crt")
        self.assertEqual(env["TLS_KEY_FILE"], "/var/run/cetirun/tls/tls.key")
        self.assertEqual(env["BROKER_TLS_CA_FILE"], "/var/run/cetirun/broker-tls/ca.crt")
        self.assertEqual(env["BROKER_TLS_CERT_FILE"], "/var/run/cetirun/broker-tls/tls.crt")
        self.assertEqual(env["BROKER_TLS_KEY_FILE"], "/var/run/cetirun/broker-tls/tls.key")

    def test_rbac_is_limited_to_token_review_and_named_workload_reads(self):
        objects = self.render()
        self.assertEqual([document for document in objects if document["kind"] == "Role"], [])
        role = self.document(objects, "ClusterRole")
        self.assertEqual(role["rules"], [
            {"apiGroups": ["authentication.k8s.io"], "resources": ["tokenreviews"], "verbs": ["create"]},
            {"apiGroups": [""], "resources": ["pods", "serviceaccounts", "namespaces"], "verbs": ["get"]},
            {"apiGroups": ["cetirun.microsoft.com"], "resources": ["agents"], "verbs": ["get"]},
        ])
        for document in objects:
            if document["kind"] in {"Role", "ClusterRole"}:
                self.assertFalse(any(
                    "secrets" in rule.get("resources", [])
                    for rule in document["rules"]
                ))
        binding = self.document(objects, "ClusterRoleBinding")
        self.assertEqual(binding["roleRef"], {
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "ClusterRole",
            "name": "cetirun-connections-mcp",
        })
        self.assertEqual(binding["subjects"], [{
            "kind": "ServiceAccount", "name": "cetirun-connections-mcp", "namespace": "cetirun-tools",
        }])

    def test_required_trust_and_namespace_boundaries_reject_invalid_values(self):
        cases = (
            (("tls", "secretName"), "", "tls.secretName is required", "cetirun-tools"),
            (("broker", "baseURL"), "http://broker.example.invalid", "broker.baseURL must use HTTPS", "cetirun-tools"),
            (("broker", "clientTLSSecretName"), "", "broker.clientTLSSecretName is required", "cetirun-tools"),
            (("portal", "baseURL"), "http://portal.example.invalid", "portal.baseURL must use HTTPS", "cetirun-tools"),
            (("workloadTokenAudience",), "", "workloadTokenAudience is required", "cetirun-tools"),
            (("serviceAccount", "create"), False, "dedicated ServiceAccount", "cetirun-tools"),
            (("istio", "enabled"), False, "requires Istio", "cetirun-tools"),
            (("istio", "sidecarInjection"), False, "sidecarInjection must be true", "cetirun-tools"),
            (("istio", "revision"), "", "istio.revision is required", "cetirun-tools"),
            (("service", "port"), 80, "service.port must be 443", "cetirun-tools"),
        )
        for path, value, message, namespace in cases:
            with self.subTest(path=".".join(path), value=value):
                self.reject(self.update(VALUES, path, value), namespace=namespace, message=message)
        self.reject(namespace="cetirun-credentials", message="require distinct namespaces")


    def render_stack(self, chart, values, namespace, extra=()):
        return subprocess.run(["helm", "template", "tools-test", str(chart), "-n", namespace, "-f", str(values), *extra], env=OFFLINE, text=True, capture_output=True, timeout=30)

    def test_full_stack_uses_separate_broker_and_native_mcp(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            for relative in ("cetirun-platform", "cetirun-operator/chart", "cetirun-portal/chart", "cetirun-ai-gateway/chart", "cetirun-connections-mcp/chart", "cetirun-agent-broker/chart", "cetirun-observability/chart"):
                shutil.copytree(ROOT / "app" / relative, base / relative, ignore=shutil.ignore_patterns("charts"))
            subprocess.run(["helm", "dependency", "build", "--skip-refresh", str(base / "cetirun-platform")], env=OFFLINE, check=True, capture_output=True, timeout=30)
            chart = base / "cetirun-platform"
            result = self.render_stack(chart, chart / "ci/connections-render-values.yaml", "cetirun-system", ("--set", "global.registry=registry.example.invalid", "--set", "cetirun-operator.cetiadmin.enabled=false"))
            self.assertEqual(result.returncode, 0, result.stderr)
            objects = [value for value in yaml.safe_load_all(result.stdout) if value]
            deployments = {value["metadata"]["name"]: value for value in objects if value["kind"] == "Deployment"}
            self.assertNotIn("cetirun-agent-broker", deployments)
            runtime_version = yaml.safe_load((ROOT / "app/cetirun-chart/Chart.yaml").read_text())["appVersion"]
            self.assertEqual(
                deployments["cetirun-connections-mcp"]["spec"]["template"]["spec"]["containers"][0]["image"],
                f"registry.example.invalid/cetirun-connections-mcp:{runtime_version}",
            )
            operator = next(value for value in objects if value["kind"] == "Deployment" and value["metadata"]["labels"]["app.kubernetes.io/name"] == "cetirun-operator")["spec"]["template"]["spec"]["containers"][0]
            env = {value["name"]: value.get("value") for value in operator["env"]}
            self.assertEqual(env["CONNECTIONS_MCP_CA_SECRET_NAME"], "connections-ca")
            self.assertEqual(env["CONNECTIONS_MCP_URL"], "https://cetirun-connections-mcp.cetirun-system.svc.cluster.local/mcp")
            self.assertEqual(env["WORKLOAD_TOKEN_AUDIENCE"], "cetirun-connections-mcp")
            self.assertIn("AGENT_ID_BROKER_URL", env)
            self.assertEqual(env["AGENT_ID_ENABLED"], "false")

    def test_broker_identity_modes_control_sdk_and_configuration(self):
        chart = ROOT / "app/cetirun-agent-broker/chart"
        common = [
            "agentIdentity.tenantId=11111111-1111-1111-1111-111111111111",
            "agentIdentity.encryptionClientId=22222222-2222-2222-2222-222222222222",
            "agentIdentity.keyVaultKeyUrl=https://example.vault.azure.net/keys/cache",
            "agentIdentity.redirectUri=https://portal.example.com/api/v1/connections/callback",
            "agentIdentity.portalOrigin=https://portal.example.com",
            "agentIdentity.portalBaseUrl=https://portal.example.com",
            "istio.revision=asm-example", "istio.sidecarInjection=true",
        ]
        for autonomous, delegated in ((False, False), (True, False), (False, True), (True, True)):
            with self.subTest(autonomous=autonomous, delegated=delegated):
                flags = common + [
                    f"identityModes.azureAgent.enabled={str(autonomous).lower()}",
                    f"identityModes.microsoftDelegated.enabled={str(delegated).lower()}",
                ]
                if autonomous or delegated:
                    flags.append("agentIdentity.blueprintClientId=33333333-3333-3333-3333-333333333333")
                if delegated:
                    flags.append("agentIdentity.authorizationClientId=44444444-4444-4444-4444-444444444444")
                extra = tuple(arg for flag in flags for arg in ("--set", flag))
                rendered = self.render_stack(chart, chart / "values.yaml", "credentials", extra)
                self.assertEqual(rendered.returncode, 0, rendered.stderr)
                objects = [value for value in yaml.safe_load_all(rendered.stdout) if value]
                pod = next(v for v in objects if v["kind"] == "Deployment")["spec"]["template"]["spec"]
                self.assertEqual(any(c["name"] == "auth-sdk" for c in pod["containers"]), delegated)
                self.assertEqual(any(v["name"] == "auth-sdk-keys" for v in pod["volumes"]), delegated)
                env = {value["name"]: value.get("value") for value in pod["containers"][0]["env"]}
                self.assertEqual(env["AZURE_AGENT_ENABLED"], str(autonomous).lower())
                self.assertEqual(env["MICROSOFT_DELEGATED_ENABLED"], str(delegated).lower())
                self.assertEqual(env["WORKLOAD_TOKEN_AUDIENCE"], "cetirun-connections-mcp")
                self.assertEqual(env["TOOL_SERVICE_ACCOUNT"], "cetirun-connections-mcp")
                if autonomous or delegated:
                    invalid = self.render_stack(chart, chart / "values.yaml", "credentials", (*extra, "--set", "agentIdentity.blueprintClientId="))
                    self.assertNotEqual(invalid.returncode, 0)


if __name__ == "__main__":
    unittest.main()
