# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline rendering checks for public HTTP-01 resources in the platform chart."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "app/cetirun-platform"
DEPENDENCIES = (
    "cetirun-connections-mcp/chart",
    "cetirun-operator/chart",
    "cetirun-portal/chart",
    "cetirun-ai-gateway/chart",
    "cetirun-observability/chart",
)
RELEASE_NAMESPACE = "cetirun-system"
INGRESS_NAMESPACE = "aks-istio-ingress"
DOMAIN = "portal.example.invalid"
PUBLIC_TLS = {
    "enabled": True,
    "bootstrapOnly": True,
    "domain": DOMAIN,
    "ingressNamespace": INGRESS_NAMESPACE,
    "ingressServiceHostname": (
        "aks-istio-ingressgateway-external.aks-istio-ingress.svc.cluster.local"
    ),
}
BOOTSTRAP_DISABLED = {
    "global": {"portalChat": {"enabled": False}},
    "cetirun-connections-mcp": {"enabled": False},
    "cetirun-operator": {"enabled": False},
    "cetirun-portal": {"enabled": False},
    "cetirun-ai-gateway": {"enabled": False},
}
FULL_PLATFORM = {
    "istio": {"enabled": True},
    "cetirun-connections-mcp": {"enabled": False},
    "cetirun-operator": {
        "enabled": True,
        "domainSuffix": "platform.example.invalid",
        "cetiadmin": {"enabled": False},
    },
    "cetirun-portal": {"enabled": True, "istio": {"host": DOMAIN}},
    "cetirun-ai-gateway": {"enabled": True},
}
OFFLINE = {
    **os.environ,
    "KUBECONFIG": os.devnull,
    "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}


@unittest.skipUnless(shutil.which("helm"), "requires Helm 3 or 4")
class PublicIngressChartTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.temporary = tempfile.TemporaryDirectory(prefix="cetirun-public-tls-chart-")
        cls.addClassCleanup(cls.temporary.cleanup)
        base = Path(cls.temporary.name) / "app"
        shutil.copytree(CHART, base / "cetirun-platform", ignore=shutil.ignore_patterns("charts", "__pycache__"))
        for relative in DEPENDENCIES:
            shutil.copytree(ROOT / "app" / relative, base / relative, ignore=shutil.ignore_patterns("charts", "__pycache__"))
        cls.chart = base / "cetirun-platform"
        subprocess.run(
            ["helm", "dependency", "build", "--skip-refresh", str(cls.chart)],
            env=OFFLINE, text=True, capture_output=True, check=True, timeout=30,
        )

    def render(self, values=None, *, platform_values=None, error=None):
        configured = {
            **BOOTSTRAP_DISABLED,
            **(platform_values or {}),
            "publicTLS": {**PUBLIC_TLS, **(values or {})},
        }
        command = [
            "helm", "template", "platform", str(self.chart), "--namespace", RELEASE_NAMESPACE,
            "--values", "-",
        ]
        result = subprocess.run(
            command, input=json.dumps(configured), text=True, capture_output=True,
            env=OFFLINE, timeout=30, check=False,
        )
        if error:
            self.assertNotEqual(result.returncode, 0, "invalid values rendered")
            self.assertIn(error, result.stderr)
            return None
        self.assertEqual(result.returncode, 0, result.stderr)
        return {
            (doc["kind"], doc["metadata"].get("namespace"), doc["metadata"]["name"]): doc
            for doc in yaml.safe_load_all(result.stdout) if doc
        }

    def test_bootstrap_outputs_only_public_tls_objects_on_existing_listener(self):
        objects = self.render()
        self.assertCountEqual([kind for kind, _, _ in objects], [
            "Gateway", "HTTPRoute", "Issuer", "Issuer",
        ])
        gateway = objects[("Gateway", INGRESS_NAMESPACE, "cetirun-acme-http")]
        self.assertEqual(gateway["spec"]["gatewayClassName"], "istio")
        self.assertEqual(gateway["spec"]["addresses"], [{
            "type": "Hostname", "value": PUBLIC_TLS["ingressServiceHostname"],
        }])
        listener = gateway["spec"]["listeners"][0]
        self.assertEqual(listener["hostname"], DOMAIN)
        self.assertEqual(listener["port"], 80)
        self.assertEqual(listener["allowedRoutes"]["namespaces"], {"from": "Same"})
        route = objects[("HTTPRoute", INGRESS_NAMESPACE, "cetirun-http-redirect")]
        self.assertEqual(route["spec"]["parentRefs"], [{
            "name": "cetirun-acme-http", "namespace": INGRESS_NAMESPACE,
            "group": "gateway.networking.k8s.io", "kind": "Gateway", "sectionName": "acme-http",
        }])
        self.assertEqual(route["spec"]["hostnames"], [DOMAIN])

    def test_full_mode_and_different_ingress_namespace_keep_public_resources_scoped(self):
        objects = self.render({
            "bootstrapOnly": False,
            "acme": {"stagingCertificateEnabled": True, "productionCertificateEnabled": True},
            "ingressNamespace": "custom-ingress",
            "ingressServiceHostname": "gateway.custom-ingress.svc.cluster.local",
        }, platform_values=FULL_PLATFORM)
        self.assertIn(("Gateway", "custom-ingress", "cetirun-acme-http"), objects)
        route = objects[("HTTPRoute", "custom-ingress", "cetirun-http-redirect")]
        self.assertEqual(route["spec"]["parentRefs"][0]["namespace"], "custom-ingress")
        for kind, namespace, _ in objects:
            if kind in {"Gateway", "HTTPRoute", "Issuer", "Certificate"}:
                self.assertEqual(namespace, "custom-ingress")

    def test_certificates_are_independent_and_retained(self):
        objects = self.render({"acme": {
            "stagingCertificateEnabled": True,
            "productionCertificateEnabled": True,
        }})
        staged = objects[("Certificate", INGRESS_NAMESPACE, "cetirun-tls-staging")]
        production = objects[("Certificate", INGRESS_NAMESPACE, "cetirun-tls")]
        self.assertEqual(staged["spec"]["secretName"], "cetirun-tls-staging")
        self.assertEqual(production["spec"]["secretName"], "cetirun-tls")
        self.assertEqual(staged["spec"]["issuerRef"]["name"], "cetirun-letsencrypt-staging")
        self.assertEqual(production["spec"]["issuerRef"]["name"], "cetirun-letsencrypt-production")
        for value in (staged, production):
            self.assertEqual(value["metadata"]["annotations"], {"helm.sh/resource-policy": "keep"})
        issuers = [value for (kind, namespace, _), value in objects.items()
                   if kind == "Issuer" and namespace == INGRESS_NAMESPACE]
        for issuer in issuers:
            self.assertEqual(issuer["metadata"]["annotations"], {"helm.sh/resource-policy": "keep"})
            solver = issuer["spec"]["acme"]["solvers"][0]["http01"]
            self.assertNotIn("podTemplate", solver)
            self.assertEqual(
                solver["gatewayHTTPRoute"]["podTemplate"]["metadata"]["annotations"],
                {"sidecar.istio.io/inject": "false"},
            )

    def test_full_mode_requires_both_managed_certificates(self):
        for acme in ({}, {"stagingCertificateEnabled": True}, {"productionCertificateEnabled": True}):
            with self.subTest(acme=acme):
                self.render({"bootstrapOnly": False, "acme": acme}, platform_values=FULL_PLATFORM,
                            error="full publicTLS deployments require both staging and production Certificates")

    def test_bootstrap_render_rejects_a_different_helm_revision(self):
        # helm template uses revision 1. The same equality check runs against
        # Helm's allocated revision during a connected upgrade.
        for expected in (0, 2, -1, "invalid"):
            with self.subTest(expected=expected):
                self.render({"bootstrapRevision": expected}, error="does not match the next Helm revision")
        self.render({"bootstrapRevision": 1})

    def test_bootstrap_and_public_tls_validation_are_strict(self):
        self.render({"enabled": False}, error="bootstrapOnly requires publicTLS.enabled=true")
        self.render({"domain": "*.example.invalid"}, error="without a wildcard")
        self.render({
            "ingressServiceHostname": "gateway.other.svc.cluster.local",
        }, error="in publicTLS.ingressNamespace")
        self.render({"acme": {"stagingCertificateEnabled": "true"}}, error="must be booleans")
        self.render(
            platform_values={"cetirun-ai-gateway": {"enabled": True}},
            error="requires cetirun-connections-mcp",
        )


if __name__ == "__main__":
    unittest.main()
