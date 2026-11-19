# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline safety and staged-flow tests for the public ingress helper."""

import copy
import fcntl
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import yaml


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("public_ingress", ROOT / "scripts/cetirun_public_ingress.py")
INGRESS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(INGRESS)

SUBSCRIPTION = "11111111-1111-1111-1111-111111111111"
TENANT = "22222222-2222-2222-2222-222222222222"


def config(directory, *, mode="custom", cert_manager="existing"):
    result = {
        "target": {
            "kubeconfig": str(directory / "kubeconfig"),
            "context": "private-context",
            "cluster": "private-cluster",
            "server": "https://private-cluster.example.invalid:443",
            "clusterUID": "cluster-uid",
        },
        "ingress": {
            "namespace": "aks-istio-ingress",
            "service": "aks-istio-ingressgateway-external",
            "release": "cetirun-platform",
            "releaseNamespace": "cetirun-system",
            "gatewayName": "cetirun-acme-http",
        },
        "dns": {"mode": mode},
        "certManager": {"mode": cert_manager},
        "stateDirectory": str(directory / "state"),
        "timeoutSeconds": 30,
    }
    if mode == "custom":
        result["dns"]["domain"] = "portal.example.invalid"
    else:
        result["dns"]["label"] = "portal-ingress"
        result["azure"] = {
            "subscriptionId": SUBSCRIPTION,
            "tenantId": TENANT,
            "resourceGroup": "example-rg",
            "clusterName": "example-aks",
        }
    if cert_manager == "install":
        result["certManager"]["chartArchive"] = str(directory / "cert-manager-v1.21.3.tgz")
    return result


def owned(name, namespace, *, uid="object-uid", release="cetirun-platform", release_namespace="cetirun-system"):
    return {
        "metadata": {
            "name": name,
            "namespace": namespace,
            "uid": uid,
            "annotations": {
                "meta.helm.sh/release-name": release,
                "meta.helm.sh/release-namespace": release_namespace,
            },
            "labels": {"app.kubernetes.io/managed-by": "Helm"},
        },
    }


class StagedIngress(INGRESS.Ingress):
    """No-cloud flow double that retains private-state behavior."""

    def __init__(self, value, scratch):
        super().__init__(value, scratch)
        self.calls = []

    def preflight(self):
        self.calls.append("preflight")
        self.service = {"uid": "service-uid", "ip": "192.0.2.10", "ports": [
            {"port": 80, "protocol": "TCP"}, {"port": 443, "protocol": "TCP"},
        ], "selector": {"istio": "managed"}}
        self.state["service"] = self.service

    def assign_dns(self):
        self.calls.append("dns")
        self.state["domain"] = self.domain
        self.save()

    def render(self, domain=None):
        self.calls.append("render")
        return {"domain": domain}

    def install_cert_manager(self):
        self.calls.append("cert-manager")

    def cert_manager_check(self):
        pass

    def ingress_ownership_check(self):
        pass

    def apply_stage(self, production):
        self.calls.append("production" if production else "staging")

    def until(self, check, message):
        self.calls.append("wait")
        self.assert_true = check()
        if not self.assert_true:
            raise AssertionError(message)

    def certificate_ready(self, stage):
        return stage in ("staging", "production")

    def public_certificate(self):
        self.calls.append("certificate")
        return "a" * 64

    def preserve_service(self):
        self.calls.append("preserve")


class SyntheticAPIIngress(INGRESS.Ingress):
    """A stateful Kubernetes, Helm, and Azure API double for flow tests."""

    def __init__(self, value, scratch):
        super().__init__(value, scratch)
        self.api_calls = []
        self.live = {}
        self.helm_values = []
        self.azure_public_ip = {
            "id": f"/subscriptions/{SUBSCRIPTION}/resourceGroups/example-nodes/providers/Microsoft.Network/publicIPAddresses/ingress",
            "etag": "etag-1",
            "ipAddress": "192.0.2.10",
            "sku": {"name": "Standard"},
            "publicIPAllocationMethod": "Static",
            "dnsSettings": {},
        }
        self.fail_patch_response = False
        self.controller_config = yaml.safe_load((INGRESS.SUPPORT / "cert-manager-values.yaml").read_text())["config"]
        self.controller_extra_args = []
        self.controller_solver_image = self.lock["acmesolver"]["image"]
        self.cert_manager_status = "deployed"
        self.cert_manager_revision = "1"
        self.cert_manager_values = self.cm_values()
        self.cert_manager_manifest = ""
        self.cert_manager_hooks = ""
        self.cert_manager_hook_phase = "Succeeded"
        self.omit_first_cert_manager_hook_status = False
        self.create_missing_cert_manager_resources = False
        self.account_secret_name = None
        self.http_calls = []
        self.platform_release = "absent"
        self.platform_values = {}
        self.platform_manifest = ""
        self.platform_hooks = ""

    def _key(self, kind, name, namespace):
        return kind, name, namespace

    def render(self, domain=None):
        return super().render(domain)

    def cm_expected_resource(self, doc, existing):
        return copy.deepcopy(doc)

    def _live_document(self, document):
        result = copy.deepcopy(document)
        metadata = result["metadata"]
        metadata["uid"] = metadata["name"] + "-uid"
        metadata["generation"] = 1
        metadata.setdefault("annotations", {}).update({
            "meta.helm.sh/release-name": self.config["ingress"]["release"],
            "meta.helm.sh/release-namespace": self.config["ingress"]["releaseNamespace"],
        })
        metadata.setdefault("labels", {})["app.kubernetes.io/managed-by"] = "Helm"
        if result["kind"] == "Gateway":
            result["status"] = {"conditions": [{"type": "Programmed", "status": "True", "observedGeneration": 1}]}
        elif result["kind"] == "HTTPRoute":
            result["status"] = {"parents": [{
                "parentRef": {"name": self.config["ingress"]["gatewayName"],
                              "namespace": self.config["ingress"]["namespace"], "sectionName": "acme-http"},
                "controllerName": "istio.io/gateway-controller",
                "conditions": [
                    {"type": "Accepted", "status": "True", "observedGeneration": 1},
                    {"type": "ResolvedRefs", "status": "True", "observedGeneration": 1},
                ],
            }]}
        elif result["kind"] == "Certificate":
            result["status"] = {
                "conditions": [{"type": "Ready", "status": "True", "observedGeneration": 1}],
                "notBefore": "2025-01-01T00:00:00Z",
                "renewalTime": "2030-01-01T00:00:00Z",
                "notAfter": "2031-01-01T00:00:00Z",
                "failedIssuanceAttempts": 0,
            }
        elif result["kind"] == "Issuer":
            result["status"] = {"conditions": [{"type": "Ready", "status": "True", "observedGeneration": 1}]}
        return result

    def _deploy_ingress(self, production):
        for document in self.documents:
            if document["kind"] == "Certificate" and document["metadata"]["name"] == INGRESS.CERTIFICATES["production"] and not production:
                continue
            group = document["apiVersion"].split("/", 1)[0]
            kind = document["kind"].lower() + "." + group
            live = self._live_document(document)
            self.live[self._key(kind, live["metadata"]["name"], live["metadata"]["namespace"])] = live
        self.platform_release = "bootstrap"
        self.platform_values = {"publicTLS": {"enabled": True, "bootstrapOnly": True}}
        self.platform_manifest = yaml.safe_dump_all(self.documents)

    def _controller_deployments(self):
        deployments = []
        for role in ("controller", "webhook", "cainjector"):
            container = {"image": self.lock[role]["image"]}
            if role == "controller":
                container.update(args=[
                    "--acme-http01-solver-image=" + self.controller_solver_image,
                    "--config=/etc/cert-manager/config.yaml",
                ] + self.controller_extra_args, volumeMounts=[{"name": "controller-config", "mountPath": "/etc/cert-manager"}])
            spec = {"replicas": 1, "template": {"spec": {"containers": [container]}}}
            if role == "controller":
                spec["template"]["spec"]["volumes"] = [{"name": "controller-config", "configMap": {
                    "name": "controller-config", "items": [{"key": "config.yaml", "path": "config.yaml"}],
                }}]
            deployments.append({"metadata": {"generation": 1}, "spec": spec, "status": {
                "observedGeneration": 1, "updatedReplicas": 1, "readyReplicas": 1, "replicas": 1,
            }})
        return deployments

    def get(self, kind, name, namespace=None, *, metadata_only=False):
        self.api_calls.append(("kubectl-get", kind, name, namespace))
        ingress_namespace = self.config["ingress"]["namespace"]
        if kind == "namespace":
            result = {"metadata": {"uid": "cluster-uid" if name == "kube-system" else "ingress-namespace"}}
        elif kind == "service":
            result = {"metadata": {"uid": "service-uid"}, "spec": {
                "type": "LoadBalancer", "ports": [{"port": 80, "protocol": "TCP"}, {"port": 443, "protocol": "TCP"}],
                "selector": {"istio": "managed"},
            }, "status": {"loadBalancer": {"ingress": [{"ip": "192.0.2.10"}]}}}
        elif kind == "customresourcedefinition":
            result = {"spec": {"versions": [{"name": "v1", "served": True}]}}
        elif kind == "gatewayclass":
            result = {"spec": {"controllerName": "istio.io/gateway-controller"}, "status": {"conditions": [{
                "type": "Accepted", "status": "True",
            }]}}
        elif kind == "configmap" and name == "controller-config":
            result = {"data": {"config.yaml": yaml.safe_dump(self.controller_config)}}
        elif kind == "secret" and name == self.account_secret_name:
            result = {"metadata": {"name": name}}
        elif kind == "secret" and name in INGRESS.CERTIFICATES.values():
            certificate = self.live.get(self._key("certificate.cert-manager.io", name, ingress_namespace))
            result = None if not certificate else {"metadata": {
                "annotations": {"cert-manager.io/certificate-name": name},
                "ownerReferences": [{"kind": "Certificate", "uid": name + "-uid"}],
            }}
        else:
            result = self.live.get(self._key(kind, name, namespace))
        if result is None:
            return None
        result = copy.deepcopy(result)
        return {"metadata": result["metadata"]} if metadata_only else result

    def kube(self, *args, **kwargs):
        self.api_calls.append(("kubectl", args))
        if args[:2] == ("config", "view"):
            return "private-cluster\nhttps://private-cluster.example.invalid:443"
        if args[:2] == ("get", "deployments"):
            return json.dumps({"items": self._controller_deployments()})
        if args[:2] == ("get", "certificates.cert-manager.io"):
            certificates = [value for (kind, _, _), value in self.live.items() if kind == "certificate.cert-manager.io"]
            return json.dumps({"items": certificates})
        raise AssertionError("unexpected kubectl invocation: " + repr(args))

    def helm(self, *args, **kwargs):
        self.api_calls.append(("helm", args))
        if args[:2] == ("version", "--template"):
            return "v3.19.0"
        if args[0] == "list":
            release = args[args.index("--filter") + 1].strip("^").rstrip("$").replace("\\", "")
            if release == self.config["certManager"]["release"]:
                return json.dumps([{"status": self.cert_manager_status, "revision": self.cert_manager_revision,
                                    "chart": "cert-manager-" + self.lock["chart"]["version"]}])
            if release == self.config["ingress"]["release"] and self.platform_release != "absent":
                status = {"failed-full": "failed", "pending": "pending-install"}.get(self.platform_release, "deployed")
                return json.dumps([{"status": status,
                                    "revision": "1", "chart": "cetirun-platform-" + yaml.safe_load((self.chart / "Chart.yaml").read_text())["version"]}])
            return json.dumps([])
        if args[:2] == ("get", "values"):
            if args[2] == self.config["certManager"]["release"]:
                return json.dumps(self.cert_manager_values)
            return json.dumps(self.platform_values)
        if args[:2] == ("get", "manifest"):
            if args[2] == self.config["certManager"]["release"]:
                return self.cert_manager_manifest
            return self.platform_manifest
        if args[:2] == ("get", "hooks"):
            if args[2] == self.config["certManager"]["release"]:
                return self.cert_manager_hooks
            return self.platform_hooks
        if args[0] == "status" and args[1] == self.config["certManager"]["release"]:
            hooks = []
            for index, hook in enumerate(yaml.safe_load_all(self.cert_manager_hooks)):
                if hook:
                    if self.omit_first_cert_manager_hook_status and index == 0:
                        continue
                    hooks.append({"kind": hook["kind"], "name": hook["metadata"]["name"],
                                  "events": hook["metadata"].get("annotations", {}).get("helm.sh/hook", "").split(","),
                                  "last_run": {"phase": self.cert_manager_hook_phase}})
            return json.dumps({"hooks": hooks})
        if args[:2] == ("install", "--replace"):
            if self.create_missing_cert_manager_resources:
                for document in self.cm_documents:
                    if self.cm_hook(document):
                        continue
                    group = document["apiVersion"].split("/", 1)[0] if "/" in document["apiVersion"] else ""
                    kind = document["kind"].lower() + ("." + group if group else "")
                    namespace = None if document["kind"] in INGRESS.CLUSTER_KINDS else document["metadata"].get("namespace", self.config["certManager"]["namespace"])
                    key = self._key(kind, document["metadata"]["name"], namespace)
                    if key not in self.live:
                        created = copy.deepcopy(document)
                        created["metadata"].update({"uid": document["metadata"]["name"] + "-replacement-uid", "annotations": {
                            "meta.helm.sh/release-name": self.config["certManager"]["release"],
                            "meta.helm.sh/release-namespace": self.config["certManager"]["namespace"],
                        }})
                        self.live[key] = created
            self.cert_manager_status = "deployed"
            self.cert_manager_revision = "2"
            return ""
        if args[:2] == ("upgrade", "--install"):
            release = args[2]
            if release == self.config["ingress"]["release"]:
                values_path = Path(args[args.index("--values") + 1])
                values = yaml.safe_load(values_path.read_text())
                self.helm_values.append(values)
                self._deploy_ingress(values["publicTLS"]["acme"]["productionCertificateEnabled"])
            return ""
        raise AssertionError("unexpected Helm invocation: " + repr(args))

    def az(self, *args):
        self.api_calls.append(("az", args))
        if args[:2] == ("account", "show"):
            return {"id": SUBSCRIPTION, "tenantId": TENANT}
        if args[:2] == ("aks", "show"):
            return {"id": f"/subscriptions/{SUBSCRIPTION}/resourceGroups/example-rg/providers/Microsoft.ContainerService/managedClusters/example-aks",
                    "fqdn": "private-cluster.example.invalid", "nodeResourceGroup": "example-nodes"}
        if args[:3] == ("network", "public-ip", "list"):
            return [copy.deepcopy(self.azure_public_ip)]
        if args[:3] == ("network", "public-ip", "show"):
            return copy.deepcopy(self.azure_public_ip)
        if args[:3] == ("network", "public-ip", "update"):
            self.azure_public_ip["dnsSettings"] = {
                "domainNameLabel": self.config["dns"]["label"],
                "fqdn": self.config["dns"]["label"] + ".example.cloudapp.azure.com",
            }
            if self.fail_patch_response:
                raise RuntimeError("synthetic response loss")
            return copy.deepcopy(self.azure_public_ip)
        raise AssertionError("unexpected Azure invocation: " + repr(args))

    def public_certificate(self):
        return hashlib.sha256(b"synthetic-public-certificate").hexdigest()

    def http(self, path, secure=True):
        self.http_calls.append((path, secure))
        answers = {
            ("/", False): (301, "https://" + self.domain + "/", ""),
            ("/", True): (302, "https://login.microsoftonline.com/synthetic?redirect_uri=https%3A%2F%2F" + self.domain
                              + "%2Foauth2%2Fcallback&code_challenge_method=S256&code_challenge=" + "A" * 43, ""),
            ("/api/v1/me", True): (401, None, ""),
            ("/healthz", True): (200, None, "ok"),
            ("/readyz", True): (200, None, "ok"),
        }
        return answers[(path, secure)]


class PublicIngressTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-test-")
        self.directory = Path(self.temporary.name)
        os.chmod(self.directory, 0o700)
        (self.directory / "kubeconfig").write_text("synthetic")
        os.chmod(self.directory / "kubeconfig", 0o600)

    def tearDown(self):
        self.temporary.cleanup()

    def normalized(self, **kwargs):
        source = config(self.directory, **kwargs)
        return INGRESS.normalize(source)

    def pinned_cert_manager_archive(self, ingress):
        """Create only the public metadata needed to test archive pinning."""
        archive = Path(ingress.config["certManager"]["chartArchive"])
        with tarfile.open(archive, "w:gz") as output:
            payload = ("name: cert-manager\nversion: " + ingress.lock["chart"]["version"] + "\n").encode()
            entry = tarfile.TarInfo("cert-manager/Chart.yaml")
            entry.size = len(payload)
            output.addfile(entry, io.BytesIO(payload))
        ingress.lock["chart"]["archiveSHA256"] = hashlib.sha256(archive.read_bytes()).hexdigest()
        return archive

    def failed_cert_manager_recovery(self, scratch):
        ingress = SyntheticAPIIngress(self.normalized(cert_manager="install"), scratch)
        ingress.load_state(write=True)
        archive = self.pinned_cert_manager_archive(ingress)
        resource = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {
            "name": "cert-manager-recovery-test", "namespace": "cert-manager",
            "labels": {"app.kubernetes.io/managed-by": "Helm"},
        }, "data": {"expected": "value"}}
        hook = {"apiVersion": "batch/v1", "kind": "Job", "metadata": {
            "name": "cert-manager-startupapicheck", "namespace": "cert-manager",
            "annotations": {"helm.sh/hook": "post-install", "helm.sh/hook-delete-policy": "before-hook-creation,hook-succeeded"},
        }, "spec": {"template": {"spec": {"restartPolicy": "Never"}}}}
        ingress.cm_documents = [resource, hook]
        live = copy.deepcopy(resource)
        live["metadata"].update({"uid": "cert-manager-recovery-test-uid", "annotations": {
            "meta.helm.sh/release-name": "cert-manager",
            "meta.helm.sh/release-namespace": "cert-manager",
        }, "labels": {"app.kubernetes.io/managed-by": "Helm"}})
        ingress.live[ingress._key("configmap", resource["metadata"]["name"], "cert-manager")] = live
        ingress.cert_manager_manifest = yaml.safe_dump_all([resource])
        ingress.cert_manager_hooks = yaml.safe_dump_all([hook])
        ingress.state.update({
            "steps": [{"name": "cert-manager-install", "status": "uncertain"}],
            "certManagerArchiveSHA256": ingress.lock["chart"]["archiveSHA256"],
            "certManagerInstall": {
                "archiveSHA256": ingress.lock["chart"]["archiveSHA256"],
                "chartVersion": ingress.lock["chart"]["version"],
                "valuesSHA256": INGRESS.document_digest(ingress.cm_values()),
                "renderSHA256": INGRESS.document_digest(ingress.cm_documents),
            },
        })
        ingress.cert_manager_status = "failed"
        return ingress, archive

    def bootstrap_platform(self, scratch):
        value = self.normalized()
        ingress = SyntheticAPIIngress(value, scratch)
        ingress.render(value["dns"]["domain"])
        ingress._deploy_ingress(True)
        ingress.api_groups = set(INGRESS.CERT_CRDS)
        ingress.service = INGRESS.service_identity(ingress.get("service", value["ingress"]["service"], value["ingress"]["namespace"]))
        return ingress

    def test_certificate_discovery_requires_the_exact_crd_name(self):
        with tempfile.TemporaryDirectory() as scratch:
            ingress = SyntheticAPIIngress(self.normalized(), scratch)
            document = {"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
                        "metadata": {"name": "cetirun-tls"}}
            for crd, available in (("prefix.certificates.cert-manager.io", False),
                                   ("certificates.cert-manager.io.example.invalid", False),
                                   ("certificates.cert-manager.io", True)):
                with self.subTest(crd=crd), patch.object(ingress, "get", return_value=None) as get:
                    ingress.api_groups = {crd}
                    ingress.resource_check(document, "cetirun-platform", "cetirun-system")
                    self.assertEqual(get.called, available)
                    ingress.api_calls.clear()
                    ingress.ingress_ownership_check()
                    certificate_lists = [call for call in ingress.api_calls if call[0] == "kubectl"
                                         and call[1][:2] == ("get", "certificates.cert-manager.io")]
                    self.assertEqual(bool(certificate_lists), available)

    def test_public_https_clients_require_tls12_and_keep_certificate_validation(self):
        def check_context(context):
            self.assertGreaterEqual(context.minimum_version, INGRESS.ssl.TLSVersion.TLSv1_2)
            self.assertEqual(context.verify_mode, INGRESS.ssl.CERT_REQUIRED)
            self.assertTrue(context.check_hostname)

        check_context(INGRESS.tls_context())
        with tempfile.TemporaryDirectory() as scratch:
            ingress = INGRESS.Ingress(self.normalized(), scratch)
            with patch.object(INGRESS.http.client, "HTTPSConnection") as connect:
                ingress.http("/readyz")
                check_context(connect.call_args.kwargs["context"])
                connect.return_value.close.assert_called_once()
        with patch.object(INGRESS, "urlopen") as open_url:
            open_url.return_value.__enter__.return_value = io.StringIO('{"Status": 0, "Answer": []}')
            INGRESS.query_dns(INGRESS.PROVIDERS[0], "portal.example.invalid", "A")
            check_context(open_url.call_args.kwargs["context"])

    def test_normalize_is_decision_complete_and_rejects_cross_mode_inputs(self):
        custom = self.normalized()
        self.assertEqual(custom["ingress"]["release"], "cetirun-platform")
        self.assertEqual(custom["ingress"]["releaseNamespace"], "cetirun-system")
        self.assertEqual(custom["certManager"]["namespace"], "cert-manager")
        self.assertEqual(custom["acme"]["email"], "")

        cases = [
            ({"dns": {"mode": "custom", "domain": "portal.example.invalid", "label": "also-a-label"}}, "Custom DNS"),
            ({"dns": {"mode": "custom", "domain": "*.example.invalid"}}, "Custom DNS"),
            ({"dns": {"mode": "custom", "domain": "portal.example.invalid"}, "azure": {"tenantId": TENANT}}, "Custom DNS"),
            ({"dns": {"mode": "azure-public-ip", "label": "portal", "domain": "predicted.example.invalid"}}, "Azure DNS"),
            ({"target": {"server": "http://private-cluster.example.invalid"}}, "HTTPS API origin"),
            ({"certManager": {"mode": "existing", "chartArchive": "/tmp/pinned.tgz"}}, "does not use chartArchive"),
            ({"stateDirectory": str(ROOT / "private-state")}, "outside the repository"),
        ]
        for override, error in cases:
            with self.subTest(override=override):
                candidate = config(self.directory)
                for key, value in override.items():
                    if isinstance(value, dict):
                        candidate.setdefault(key, {}).update(copy.deepcopy(value))
                    else:
                        candidate[key] = copy.deepcopy(value)
                with self.assertRaisesRegex(INGRESS.IngressError, error):
                    INGRESS.normalize(candidate)

    def test_azure_normalization_requires_explicit_identity_and_label(self):
        valid = self.normalized(mode="azure-public-ip")
        self.assertEqual(valid["azure"]["subscriptionId"], SUBSCRIPTION)
        invalid = config(self.directory, mode="azure-public-ip")
        invalid["azure"]["subscriptionId"] = "not-a-uuid"
        with self.assertRaisesRegex(INGRESS.IngressError, "subscriptionId"):
            INGRESS.normalize(invalid)
        invalid = config(self.directory, mode="azure-public-ip")
        invalid["dns"]["label"] = "UPPER"
        with self.assertRaisesRegex(INGRESS.IngressError, "Azure DNS"):
            INGRESS.normalize(invalid)

    def test_bootstrap_values_disable_every_platform_workload(self):
        values = INGRESS.chart_values(self.normalized(), "portal.example.invalid")
        self.assertEqual(values["publicTLS"]["domain"], "portal.example.invalid")
        self.assertTrue(values["publicTLS"]["bootstrapOnly"])
        self.assertFalse(values["cetirun-operator"]["enabled"])
        self.assertFalse(values["cetirun-portal"]["enabled"])
        self.assertFalse(values["istio"]["enabled"])
        self.assertFalse(values["global"]["portalChat"]["enabled"])

    def test_full_and_failed_full_platform_releases_are_never_downgraded(self):
        for release_state in ("full", "failed-full"):
            with self.subTest(release_state=release_state), tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
                ingress = self.bootstrap_platform(scratch)
                ingress.platform_release = release_state
                ingress.platform_values = {"publicTLS": {"enabled": True, "bootstrapOnly": False}}
                writes_before = len(ingress.helm_values)
                ingress.apply_stage(True)
                self.assertEqual(ingress.platform_state, "full")
                self.assertEqual(len(ingress.helm_values), writes_before)

    def test_competing_full_revision_cannot_receive_stale_bootstrap_values(self):
        for window in ("before-render", "before-storage-create"):
            with self.subTest(window=window), tempfile.TemporaryDirectory() as scratch:
                ingress = self.bootstrap_platform(scratch)
                ingress.load_state(write=True)
                original = ingress.helm
                writes = len(ingress.helm_values)
                attempted = []

                def race(*args, **kwargs):
                    if args[:2] != ("upgrade", "--install"):
                        return original(*args, **kwargs)
                    values = yaml.safe_load(Path(args[args.index("--values") + 1]).read_text())
                    expected = values["publicTLS"]["bootstrapRevision"]
                    self.assertEqual(expected, 2)  # latest classified revision was 1
                    attempted.append(expected)
                    ingress.platform_release = "full"
                    ingress.platform_values = {"publicTLS": {"enabled": True, "bootstrapOnly": False}}
                    # Model Helm's documented ordering: allocate/render, then
                    # atomically create the unique revision before any resource
                    # mutation. The actual chart's equality guard is rendered
                    # with Helm in test_public_ingress_chart.
                    if window == "before-render":
                        allocated = 3  # competing full deployment committed revision 2
                        if allocated != expected:
                            raise INGRESS.IngressError("bootstrapRevision does not match the next Helm revision")
                    raise INGRESS.IngressError("Helm revision storage already exists")

                ingress.helm = race
                with self.assertRaisesRegex(INGRESS.IngressError, "revision"):
                    ingress.apply_stage(True)
                self.assertEqual(attempted, [2])
                self.assertEqual(len(ingress.helm_values), writes)
                self.assertEqual(ingress.platform_release, "full")

    def test_missing_or_false_bootstrap_marker_is_conservatively_full(self):
        for marker in (None, False):
            with self.subTest(marker=marker), tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
                ingress = self.bootstrap_platform(scratch)
                ingress.platform_values = {"publicTLS": {"enabled": True}}
                if marker is not None:
                    ingress.platform_values["publicTLS"]["bootstrapOnly"] = marker
                self.assertEqual(ingress.platform_release_state(), "full")

    def test_bootstrap_marker_cannot_authorize_application_resources_or_pending_release(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = self.bootstrap_platform(scratch)
            application = {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {
                "name": "portal", "namespace": ingress.config["ingress"]["namespace"]}}
            ingress.platform_manifest += "---\n" + yaml.safe_dump(application)
            with self.assertRaisesRegex(INGRESS.IngressError, "application or unexpected"):
                ingress.platform_release_state()

            ingress.platform_manifest = yaml.safe_dump_all(ingress.documents)
            ingress.platform_release = "pending"
            with self.assertRaisesRegex(INGRESS.IngressError, "active or unsupported"):
                ingress.platform_release_state()

    def test_service_identity_requires_existing_dual_listener_and_selector(self):
        service = {"metadata": {"uid": "service-uid"}, "spec": {
            "type": "LoadBalancer", "ports": [{"port": 80}, {"port": 443, "protocol": "TCP"}],
            "selector": {"istio": "managed"},
        }, "status": {"loadBalancer": {"ingress": [{"ip": "192.0.2.10"}]}}}
        identity = INGRESS.service_identity(service)
        self.assertEqual(identity["ip"], "192.0.2.10")
        service["spec"]["ports"] = [{"port": 443}]
        with self.assertRaisesRegex(INGRESS.IngressError, "80 and 443"):
            INGRESS.service_identity(service)

    def test_staged_preparation_records_private_state_after_staging_then_production(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = StagedIngress(value, scratch)
            result = ingress.prepare()
        self.assertEqual(ingress.calls[0:3], ["preflight", "dns", "render"])
        self.assertLess(ingress.calls.index("staging"), ingress.calls.index("production"))
        self.assertIn("certificate", ingress.calls)
        self.assertTrue(result["certificateReady"])
        self.assertEqual(result["oauthCallback"], "https://portal.example.invalid/oauth2/callback")
        state = json.loads((self.directory / "state" / "state.json").read_text())
        self.assertTrue(state["prepared"])
        self.assertEqual((self.directory / "state").stat().st_mode & 0o077, 0)

    def test_partial_mutation_records_uncertain_step_and_is_not_silently_replayed(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            ingress.load_state(write=True)
            with self.assertRaisesRegex(RuntimeError, "lost response"):
                ingress.mutate("staging-resources", lambda: (_ for _ in ()).throw(RuntimeError("lost response")))
            saved = json.loads((self.directory / "state" / "state.json").read_text())
            self.assertEqual(saved["lastStep"], {"name": "staging-resources", "status": "uncertain"})
            next_run = INGRESS.Ingress(value, scratch)
            next_run.load_state()
            self.assertEqual(next_run.state["lastStep"]["status"], "uncertain")

    def test_unmanaged_resource_and_tls_secret_are_never_adopted(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            doc = {"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
                   "metadata": {"name": "cetirun-acme-http", "namespace": value["ingress"]["namespace"]}}
            ingress.get = lambda *args, **kwargs: {"metadata": {"annotations": {}, "labels": {}}}
            with self.assertRaisesRegex(INGRESS.IngressError, "ownership conflict"):
                ingress.resource_check(doc, value["ingress"]["release"], value["ingress"]["releaseNamespace"])

            certificate = owned("cetirun-tls", value["ingress"]["namespace"])
            certificate["spec"] = {"secretName": "cetirun-tls"}
            ingress.get = lambda *args, **kwargs: {"metadata": {
                "annotations": {"cert-manager.io/certificate-name": "cetirun-tls"},
                "ownerReferences": [{"kind": "Certificate", "uid": "other-certificate"}],
            }}
            with self.assertRaisesRegex(INGRESS.IngressError, "owner reference"):
                ingress.certificate_secret_check(certificate)

    def test_offline_render_never_uses_target_kubeconfig_or_azure(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            recorded = []

            resources = []
            for kind, name, api_version in (
                ("Gateway", "cetirun-acme-http", "gateway.networking.k8s.io/v1"),
                ("HTTPRoute", "cetirun-http-redirect", "gateway.networking.k8s.io/v1"),
                ("Issuer", "cetirun-letsencrypt-staging", "cert-manager.io/v1"),
                ("Issuer", "cetirun-letsencrypt-production", "cert-manager.io/v1"),
                ("Certificate", "cetirun-tls-staging", "cert-manager.io/v1"),
                ("Certificate", "cetirun-tls", "cert-manager.io/v1"),
            ):
                resources.append({"apiVersion": api_version, "kind": kind,
                                  "metadata": {"name": name, "namespace": value["ingress"]["namespace"]}})

            def fake_run(command, **kwargs):
                recorded.append((command, kwargs))
                self.assertTrue(kwargs["offline"])
                return "" if command[1:3] == ["dependency", "build"] else yaml.safe_dump_all(resources)

            ingress.run = fake_run
            ingress.archive_check = lambda: None
            ingress.render()
        self.assertEqual([command[1:3] for command, _ in recorded], [["dependency", "build"], ["template", value["ingress"]["release"]]])

    def test_dns_check_requires_exact_ipv4_and_no_ipv6(self):
        values = {
            ("https://dns.google/resolve", "A"): {"192.0.2.10"},
            ("https://dns.google/resolve", "AAAA"): set(),
            ("https://cloudflare-dns.com/dns-query", "A"): {"192.0.2.10"},
            ("https://cloudflare-dns.com/dns-query", "AAAA"): set(),
        }
        with patch.object(INGRESS, "query_dns", side_effect=lambda provider, domain, kind: values[(provider, kind)]):
            INGRESS.check_dns("portal.example.invalid", "192.0.2.10")
        values[("https://dns.google/resolve", "AAAA")] = {"2001:db8::1"}
        with patch.object(INGRESS, "query_dns", side_effect=lambda provider, domain, kind: values[(provider, kind)]):
            with self.assertRaisesRegex(INGRESS.IngressError, "without an AAAA"):
                INGRESS.check_dns("portal.example.invalid", "192.0.2.10")

    def test_commands_bind_kubectl_and_azure_to_configured_context_and_subscription(self):
        value = self.normalized(mode="azure-public-ip")
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            commands = []
            ingress.run = lambda command, **kwargs: commands.append(command) or "{}"
            ingress.kube("get", "namespace", "kube-system")
            ingress.az("account", "show")
        self.assertEqual(commands[0][:4], ["kubectl", "--context", "private-context", "--request-timeout=30s"])
        self.assertIn("--subscription", commands[1])
        self.assertEqual(commands[1][commands[1].index("--subscription") + 1], SUBSCRIPTION)
        self.assertNotIn("set", commands[1])

    def test_release_status_keeps_all_statuses_on_helm3_and_uses_helm4_default(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            for version, expects_all in (("v3.19.0", True), ("v4.3.1", False)):
                with self.subTest(version=version):
                    ingress = INGRESS.Ingress(value, scratch)
                    calls = []

                    def helm(*args, **kwargs):
                        calls.append(args)
                        if args[0] == "version":
                            return version
                        self.assertEqual(args[0], "list")
                        return json.dumps([{"status": "failed", "revision": "1"}])

                    ingress.helm = helm
                    self.assertEqual(ingress.release_status("cetirun-platform", "cetirun-system")["status"], "failed")
                    self.assertEqual("--all" in calls[1], expects_all)
                    ingress.release_status("cetirun-platform", "cetirun-system")
                    self.assertEqual(sum(call[0] == "version" for call in calls), 1)

    def test_release_status_rejects_unknown_helm_and_active_operations(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            for version in ("invalid", "v2.17.0", "v5.0.0"):
                with self.subTest(version=version):
                    ingress = INGRESS.Ingress(value, scratch)
                    with patch.object(ingress, "helm", return_value=version) as helm:
                        for _ in range(2):
                            with self.assertRaises(INGRESS.IngressError):
                                ingress.release_status("cetirun-platform", "cetirun-system")
                        self.assertTrue(all(call.args[0] == "version" for call in helm.call_args_list))
            for version in ("v3.19.0", "v4.3.1"):
                for status in ("pending-install", "pending-upgrade", "uninstalling"):
                    with self.subTest(version=version, status=status):
                        ingress = INGRESS.Ingress(value, scratch)
                        with patch.object(ingress, "helm", side_effect=[version, json.dumps([{"status": status}])]):
                            with self.assertRaisesRegex(INGRESS.IngressError, "active or unsupported"):
                                ingress.release_status("cetirun-platform", "cetirun-system")

    def test_azure_preflight_binds_the_exact_aks_and_public_ip(self):
        value = self.normalized(mode="azure-public-ip")
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            ingress.service = {"uid": "service-uid", "ip": "192.0.2.10", "ports": [], "selector": {}}
            public_ip = {
                "id": "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/example-nodes/providers/Microsoft.Network/publicIPAddresses/ingress",
                "etag": "etag-1", "ipAddress": "192.0.2.10", "sku": {"name": "Standard"},
                "publicIPAllocationMethod": "Static",
                "dnsSettings": {"domainNameLabel": "portal-ingress", "fqdn": "portal-ingress.example.cloudapp.azure.com"},
            }
            responses = [
                {"id": SUBSCRIPTION, "tenantId": TENANT},
                {"id": f"/subscriptions/{SUBSCRIPTION}/resourceGroups/example-rg/providers/Microsoft.ContainerService/managedClusters/example-aks",
                 "fqdn": "private-cluster.example.invalid", "nodeResourceGroup": "example-nodes"},
                [public_ip],
            ]
            commands = []
            ingress.az = lambda *args: commands.append(args) or responses.pop(0)
            ingress.azure_check()
        self.assertEqual(ingress.domain, "portal-ingress.example.cloudapp.azure.com")
        self.assertEqual(ingress.state["publicIPId"], public_ip["id"])
        self.assertEqual(commands[1], ("aks", "show", "--resource-group", "example-rg", "--name", "example-aks"))

    def test_azure_preflight_refuses_an_existing_different_dns_label(self):
        value = self.normalized(mode="azure-public-ip")
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            ingress.service = {"uid": "service-uid", "ip": "192.0.2.10", "ports": [], "selector": {}}
            responses = [
                {"id": SUBSCRIPTION, "tenantId": TENANT},
                {"id": f"/subscriptions/{SUBSCRIPTION}/resourceGroups/example-rg/providers/Microsoft.ContainerService/managedClusters/example-aks",
                 "fqdn": "private-cluster.example.invalid", "nodeResourceGroup": "example-nodes"},
                [{"id": "/public-ip", "ipAddress": "192.0.2.10", "sku": {"name": "Standard"},
                  "publicIPAllocationMethod": "Static", "dnsSettings": {"domainNameLabel": "already-owned"}}],
            ]
            ingress.az = lambda *args: responses.pop(0)
            with self.assertRaisesRegex(INGRESS.IngressError, "another DNS label"):
                ingress.azure_check()

    def test_target_preflight_refuses_context_or_cluster_uid_mismatch_before_writes(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            ingress.kube = lambda *args, **kwargs: "other-cluster\nhttps://private-cluster.example.invalid:443"
            with self.assertRaisesRegex(INGRESS.IngressError, "cluster/server mismatch"):
                ingress.target_check()

            ingress.kube = lambda *args, **kwargs: "private-cluster\nhttps://private-cluster.example.invalid:443"
            ingress.get = lambda *args, **kwargs: {"metadata": {"uid": "other-uid"}}
            with self.assertRaisesRegex(INGRESS.IngressError, "UID mismatch"):
                ingress.target_check()

    def test_pinned_cert_manager_archive_requires_exact_checksum_and_metadata(self):
        archive = self.directory / "cert-manager-v1.21.3.tgz"
        with tarfile.open(archive, "w:gz") as output:
            payload = b"name: cert-manager\nversion: v1.21.3\n"
            entry = tarfile.TarInfo("cert-manager/Chart.yaml")
            entry.size = len(payload)
            output.addfile(entry, io.BytesIO(payload))
        value = self.normalized(cert_manager="install")
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            ingress.lock["chart"]["archiveSHA256"] = hashlib.sha256(archive.read_bytes()).hexdigest()
            ingress.archive_check()
            ingress.lock["chart"]["archiveSHA256"] = "0" * 64
            with self.assertRaisesRegex(INGRESS.IngressError, "checksum"):
                ingress.archive_check()

    def test_public_certificate_uses_only_public_tls_data_and_checks_hostname(self):
        value = self.normalized()
        certificate = owned("cetirun-tls", value["ingress"]["namespace"], uid="certificate-uid")
        certificate["spec"] = {"secretName": "cetirun-tls"}
        certificate["status"] = {"observedGeneration": 1, "conditions": [
            {"type": "Ready", "status": "True", "observedGeneration": 1},
        ], "notBefore": "2025-01-01T00:00:00Z", "renewalTime": "2030-01-01T00:00:00Z",
            "notAfter": "2031-01-01T00:00:00Z", "failedIssuanceAttempts": 0}
        certificate["metadata"]["generation"] = 1
        pem = "-----BEGIN CERTIFICATE-----\nsynthetic\n-----END CERTIFICATE-----\n"
        encoded = __import__("base64").b64encode(pem.encode()).decode()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            ingress.domain = "portal.example.invalid"
            ingress.get = lambda kind, *args, **kwargs: certificate if kind == "certificate.cert-manager.io" else None
            ingress.kube = lambda *args, **kwargs: encoded
            ingress.certificate_secret_check = lambda item: self.assertIs(item, certificate)
            commands = []
            ingress.run = lambda command, **kwargs: commands.append(command) or "ok"
            with patch.object(INGRESS.ssl, "PEM_cert_to_DER_cert", return_value=b"public-certificate"):
                fingerprint = ingress.public_certificate()
        self.assertEqual(fingerprint, hashlib.sha256(b"public-certificate").hexdigest())
        self.assertIn("portal.example.invalid", commands[1])
        self.assertNotIn("tls.key", " ".join(map(str, commands)))

    def test_tool_error_does_not_expose_subprocess_output(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            result = subprocess.CompletedProcess(["helm"], 1, stdout="token=private", stderr="secret=private")
            with patch.object(subprocess, "run", return_value=result):
                with self.assertRaisesRegex(INGRESS.IngressError, "tool output is withheld") as failure:
                    ingress.run(["helm", "template"])
        self.assertNotIn("private", str(failure.exception))

    def test_commands_disable_client_go_http2_explicitly(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(value, scratch)
            environments = []

            def run(*args, **kwargs):
                environments.append(kwargs["env"])
                return subprocess.CompletedProcess(args[0], 0, stdout="", stderr="")

            with patch.object(subprocess, "run", side_effect=run):
                ingress.run(["kubectl", "version"])
        self.assertEqual(environments[0]["GODEBUG"], "http2client=0")
        self.assertEqual(environments[0]["DISABLE_HTTP2"], "1")

    def test_custom_domain_prepare_and_rerun_preserve_the_production_certificate(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            with patch.object(INGRESS, "check_dns"):
                result = ingress.prepare()
            self.assertEqual(result["domain"], "portal.example.invalid")
            self.assertEqual([item["publicTLS"]["acme"]["productionCertificateEnabled"] for item in ingress.helm_values], [False, True])
            production_key = ingress._key("certificate.cert-manager.io", INGRESS.CERTIFICATES["production"], value["ingress"]["namespace"])
            production_before = copy.deepcopy(ingress.live[production_key])

            ingress.helm_values.clear()
            with patch.object(INGRESS, "check_dns"):
                rerun = ingress.prepare()

        self.assertTrue(rerun["certificateReady"])
        self.assertTrue(ingress.helm_values)
        self.assertTrue(all(item["publicTLS"]["acme"]["productionCertificateEnabled"] for item in ingress.helm_values))
        self.assertEqual(ingress.live[production_key], production_before)
        self.assertFalse(any(call[0] == "az" for call in ingress.api_calls))

    def test_azure_response_loss_is_recovered_from_the_assigned_dns_label_without_repatching(self):
        value = self.normalized(mode="azure-public-ip")
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            first = SyntheticAPIIngress(value, scratch)
            first.fail_patch_response = True
            with patch.object(INGRESS, "check_dns"):
                with self.assertRaisesRegex(RuntimeError, "response loss"):
                    first.prepare()
            receipt = json.loads((self.directory / "state" / "state.json").read_text())
            self.assertEqual(receipt["lastStep"], {"name": "azure-dns-label", "status": "uncertain"})
            self.assertEqual(first.azure_public_ip["dnsSettings"]["domainNameLabel"], "portal-ingress")

            retry = SyntheticAPIIngress(value, scratch)
            retry.azure_public_ip = first.azure_public_ip
            with patch.object(INGRESS, "check_dns"):
                result = retry.prepare()

        self.assertEqual(result["domain"], "portal-ingress.example.cloudapp.azure.com")
        retry_patches = [call for call in retry.api_calls if call[0] == "az" and call[1][:3] == ("network", "public-ip", "update")]
        self.assertEqual(retry_patches, [])

    def test_existing_cert_manager_controller_requires_config_mount_and_pinned_solver_digest(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            ingress.api_groups = set(INGRESS.CERT_CRDS)
            ingress.check_controller()
            ingress.controller_solver_image = "quay.io/jetstack/cert-manager-acmesolver:v1.21.3"
            with self.assertRaisesRegex(INGRESS.IngressError, "solver"):
                ingress.check_controller()
            ingress.controller_solver_image = ingress.lock["acmesolver"]["image"]
            ingress.controller_config = {"gatewayAPI": {"enabled": False}}
            with self.assertRaisesRegex(INGRESS.IngressError, "Gateway API"):
                ingress.check_controller()

    def test_existing_cert_manager_gateway_api_config_compatibility(self):
        cases = (
            ({"gatewayAPI": {"enabled": True}}, True),
            ({"enableGatewayAPI": True}, True),
            ({"gatewayAPI": {"enabled": True}, "enableGatewayAPI": False}, True),
            ({"gatewayAPI": {"enabled": False}, "enableGatewayAPI": True}, False),
            ({"gatewayAPI": {}, "enableGatewayAPI": True}, True),
            ({"gatewayAPI": None, "enableGatewayAPI": True}, True),
            ({"gatewayAPI": {"enabled": None}, "enableGatewayAPI": True}, True),
            ({"gatewayAPI": {"enabled": False}}, False),
            ({"enableGatewayAPI": False}, False),
            ({"gatewayAPI": {"enabled": "true"}, "enableGatewayAPI": True}, False),
            ({"gatewayAPI": {"enabled": 1}}, False),
            ({"enableGatewayAPI": "true"}, False),
            ({"gatewayAPI": True, "enableGatewayAPI": True}, False),
            ({"gatewayAPI": {}}, False),
            ({}, False),
            (None, False),
        )
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(self.normalized(), scratch)
            ingress.api_groups = set(INGRESS.CERT_CRDS)
            for document, enabled in cases:
                with self.subTest(config=document):
                    ingress.controller_config = document
                    if enabled:
                        ingress.cert_manager_check()
                        self.assertFalse(ingress.cm_needs_install)
                    else:
                        with self.assertRaisesRegex(INGRESS.IngressError, "Gateway API"):
                            ingress.cert_manager_check()

    def test_existing_cert_manager_gateway_api_cli_overrides_config(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(self.normalized(), scratch)
            ingress.api_groups = set(INGRESS.CERT_CRDS)
            for document in ({"gatewayAPI": {"enabled": True}}, {"enableGatewayAPI": True}, {}):
                for args, enabled in ((["--enable-gateway-api=true"], True),
                                      (["--enable-gateway-api"], True),
                                      (["--enable-gateway-api=false"], False)):
                    with self.subTest(config=document, args=args):
                        ingress.controller_config = document
                        ingress.controller_extra_args = args
                        if enabled:
                            ingress.cert_manager_check()
                        else:
                            with self.assertRaisesRegex(INGRESS.IngressError, "Gateway API"):
                                ingress.cert_manager_check()

    def test_preflight_refuses_unmanaged_acme_account_secret(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            ingress.render(value["dns"]["domain"])
            ingress.account_secret_name = "cetirun-letsencrypt-staging-account"
            with self.assertRaisesRegex(INGRESS.IngressError, "ACME account Secret"):
                ingress.ingress_ownership_check()

    def test_failed_cert_manager_release_is_not_recovered_without_our_receipt(self):
        value = self.normalized(cert_manager="install")
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            ingress.api_groups = set(INGRESS.CERT_CRDS)
            ingress.cert_manager_status = "failed"
            with self.assertRaisesRegex(INGRESS.IngressError, "not installed by this preparation"):
                ingress.cert_manager_check()

    def test_failed_pinned_cert_manager_install_with_matching_receipt_can_recover(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.cert_manager_check()
        self.assertTrue(ingress.cm_recovery)
        self.assertTrue(ingress.cm_needs_install)

    def test_failed_first_install_replays_the_post_install_hook_with_install_replace(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.cert_manager_check()
            ingress.install_cert_manager()
        commands = [entry[1] for entry in ingress.api_calls if entry[0] == "helm"]
        replace = [args for args in commands if args[:2] == ("install", "--replace")]
        self.assertEqual(len(replace), 1)
        self.assertNotIn("--force", replace[0])
        self.assertNotIn("--take-ownership", replace[0])
        self.assertEqual(ingress.cert_manager_revision, "2")
        retained = ingress.live[ingress._key("configmap", "cert-manager-recovery-test", "cert-manager")]
        self.assertEqual(retained["metadata"]["uid"], "cert-manager-recovery-test-uid")
        self.assertEqual(ingress.state["certManagerRecovery"]["resourceUIDs"], {
            "v1:ConfigMap:cert-manager/cert-manager-recovery-test": "cert-manager-recovery-test-uid",
        })

    def test_failed_cert_manager_recovery_rejects_a_newer_revision(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.cert_manager_revision = "2"
            with self.assertRaisesRegex(INGRESS.IngressError, "not the first revision"):
                ingress.cert_manager_check()

    def test_failed_cert_manager_recovery_rejects_mismatched_values_or_render(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.cert_manager_values["prometheus"] = {"enabled": True}
            with self.assertRaisesRegex(INGRESS.IngressError, "values differ"):
                ingress.cert_manager_check()

            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.cert_manager_manifest = yaml.safe_dump_all([{**ingress.cm_documents[0], "data": {"unexpected": "change"}}])
            with self.assertRaisesRegex(INGRESS.IngressError, "render differs"):
                ingress.cert_manager_check()

    def test_failed_cert_manager_recovery_rejects_a_changed_retained_resource(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            resource = ingress.live[ingress._key("configmap", "cert-manager-recovery-test", "cert-manager")]
            resource["data"]["expected"] = "changed"
            with self.assertRaisesRegex(INGRESS.IngressError, "retained resource differs"):
                ingress.cert_manager_check()

    def test_failed_cert_manager_recovery_rechecks_before_install_replace(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.cert_manager_check()
            original_check = ingress.cert_manager_check
            checks = 0

            def recheck_with_drift():
                nonlocal checks
                checks += 1
                if checks == 1:
                    ingress.live[ingress._key("configmap", "cert-manager-recovery-test", "cert-manager")]["data"]["expected"] = "changed"
                original_check()

            ingress.cert_manager_check = recheck_with_drift
            with self.assertRaisesRegex(INGRESS.IngressError, "retained resource differs"):
                ingress.install_cert_manager()
        self.assertFalse(any(entry[0] == "helm" and entry[1][:2] == ("install", "--replace") for entry in ingress.api_calls))

    def test_failed_cert_manager_recovery_does_not_repeat_an_uncertain_replace(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.cert_manager_check()
            ingress.state["steps"].append({"name": "cert-manager-install-replace", "status": "uncertain"})
            with self.assertRaisesRegex(INGRESS.IngressError, "replacement outcome is unknown"):
                ingress.install_cert_manager()
        self.assertFalse(any(entry[0] == "helm" and entry[1][:2] == ("install", "--replace") for entry in ingress.api_calls))

    def test_uncertain_cert_manager_replace_reenters_through_read_only_acceptance(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.state["steps"].append({"name": "cert-manager-install-replace", "status": "uncertain"})
            ingress.state["certManagerRecovery"] = {"resourceUIDs": {
                "v1:ConfigMap:cert-manager/cert-manager-recovery-test": "cert-manager-recovery-test-uid",
            }}
            ingress.cert_manager_status = "deployed"
            ingress.cert_manager_revision = "2"
            ingress.api_groups = set(INGRESS.CERT_CRDS)
            ingress.cert_manager_check()
        self.assertEqual(ingress.state["certManagerRecovery"]["acceptedRevision"], 2)
        self.assertFalse(any(entry[0] == "helm" and entry[1][:2] == ("install", "--replace") for entry in ingress.api_calls))

    def test_started_cert_manager_replace_reenters_through_read_only_acceptance(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.state["steps"].append({"name": "cert-manager-install-replace", "status": "started"})
            ingress.state["certManagerRecovery"] = {"resourceUIDs": {
                "v1:ConfigMap:cert-manager/cert-manager-recovery-test": "cert-manager-recovery-test-uid",
            }}
            ingress.cert_manager_status = "deployed"
            ingress.cert_manager_revision = "2"
            ingress.api_groups = set(INGRESS.CERT_CRDS)
            ingress.cert_manager_check()
        self.assertEqual(ingress.state["certManagerRecovery"]["acceptedRevision"], 2)

    def test_cert_manager_replacement_rechecks_live_resources_after_helm_returns(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.cert_manager_check()
            original_helm = ingress.helm

            def helm_with_drift(*args, **kwargs):
                result = original_helm(*args, **kwargs)
                if args[:2] == ("install", "--replace"):
                    ingress.live[ingress._key("configmap", "cert-manager-recovery-test", "cert-manager")]["data"]["expected"] = "changed"
                return result

            ingress.helm = helm_with_drift
            with self.assertRaisesRegex(INGRESS.IngressError, "retained resource differs"):
                ingress.install_cert_manager()

    def test_cert_manager_replacement_creates_and_validates_a_missing_resource(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            del ingress.live[ingress._key("configmap", "cert-manager-recovery-test", "cert-manager")]
            ingress.create_missing_cert_manager_resources = True
            ingress.cert_manager_check()
            ingress.install_cert_manager()
        self.assertIn(ingress._key("configmap", "cert-manager-recovery-test", "cert-manager"), ingress.live)
        self.assertEqual(ingress.state["certManagerRecovery"]["acceptedRevision"], 2)

    def test_cert_manager_replacement_rejects_a_resource_still_missing_after_helm(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            del ingress.live[ingress._key("configmap", "cert-manager-recovery-test", "cert-manager")]
            ingress.cert_manager_check()
            with self.assertRaisesRegex(INGRESS.IngressError, "did not retain managed resource ownership"):
                ingress.install_cert_manager()

    def test_cert_manager_recovery_rejects_a_failed_stored_post_install_hook(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.state["steps"].append({"name": "cert-manager-install-replace", "status": "uncertain"})
            ingress.state["certManagerRecovery"] = {"resourceUIDs": {
                "v1:ConfigMap:cert-manager/cert-manager-recovery-test": "cert-manager-recovery-test-uid",
            }}
            ingress.cert_manager_status = "deployed"
            ingress.cert_manager_revision = "2"
            ingress.cert_manager_hook_phase = "Failed"
            ingress.api_groups = set(INGRESS.CERT_CRDS)
            with self.assertRaisesRegex(INGRESS.IngressError, "post-install startup check did not succeed"):
                ingress.cert_manager_check()

    def test_cert_manager_recovery_rejects_a_missing_stored_post_install_hook(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.state["steps"].append({"name": "cert-manager-install-replace", "status": "uncertain"})
            ingress.state["certManagerRecovery"] = {"resourceUIDs": {
                "v1:ConfigMap:cert-manager/cert-manager-recovery-test": "cert-manager-recovery-test-uid",
            }}
            ingress.cert_manager_status = "deployed"
            ingress.cert_manager_revision = "2"
            ingress.omit_first_cert_manager_hook_status = True
            ingress.api_groups = set(INGRESS.CERT_CRDS)
            with self.assertRaisesRegex(INGRESS.IngressError, "post-install startup check did not succeed"):
                ingress.cert_manager_check()

    def test_cert_manager_dry_run_retains_only_assigned_service_identity_fields(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = INGRESS.Ingress(self.normalized(cert_manager="install"), scratch)
            document = {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "cert-manager", "namespace": "cert-manager"},
                        "spec": {"ports": [{"port": 443}]}}
            existing = {"metadata": {"resourceVersion": "7"}, "spec": {
                "clusterIP": "10.0.0.10", "clusterIPs": ["10.0.0.10"], "ipFamilies": ["IPv4"],
                "ipFamilyPolicy": "SingleStack", "ports": [{"port": 443}],
            }}

            def kube(*args, **kwargs):
                self.assertEqual(args[:2], ("replace", "--dry-run=server"))
                desired = yaml.safe_load(kwargs["input_text"])
                self.assertEqual(desired["metadata"]["resourceVersion"], "7")
                for field in ("clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy"):
                    self.assertEqual(desired["spec"][field], existing["spec"][field])
                return json.dumps(desired)

            ingress.kube = kube
            self.assertEqual(ingress.cm_expected_resource(document, existing)["spec"]["clusterIP"], "10.0.0.10")

    def test_cert_manager_recovery_allows_only_the_exact_aks_webhook_defaults(self):
        ingress = SyntheticAPIIngress(self.normalized(cert_manager="install"), self.directory)
        webhook = {"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration", "metadata": {
            "name": "cert-manager-webhook",
        }, "webhooks": [{"name": "webhook.cert-manager.io", "namespaceSelector": {
            "matchExpressions": [{"key": "cert-manager.io/disable-validation", "operator": "NotIn", "values": ["true"]}],
        }}]}
        actual = copy.deepcopy(webhook)
        actual["webhooks"][0]["namespaceSelector"]["matchExpressions"] += [
            {"key": "control-plane", "operator": "NotIn", "values": ["true"]},
            {"key": "kubernetes.azure.com/managedby", "operator": "NotIn", "values": ["aks"]},
        ]
        self.assertTrue(ingress.cm_resource_matches(webhook, actual))
        actual["webhooks"][0]["namespaceSelector"]["matchExpressions"].append(
            {"key": "unexpected", "operator": "Exists"})
        self.assertFalse(ingress.cm_resource_matches(webhook, actual))
        actual = copy.deepcopy(webhook)
        actual["webhooks"][0]["namespaceSelector"]["matchExpressions"].append(
            {"key": "control-plane", "operator": "NotIn", "values": ["true"]})
        self.assertFalse(ingress.cm_resource_matches(webhook, actual))
        actual["webhooks"][0]["namespaceSelector"]["matchExpressions"] += [
            {"key": "control-plane", "operator": "NotIn", "values": ["true"]},
            {"key": "kubernetes.azure.com/managedby", "operator": "NotIn", "values": ["aks"]},
        ]
        self.assertFalse(ingress.cm_resource_matches(webhook, actual))

    def test_cert_manager_recovery_requires_nonempty_controller_injected_ca_bundles(self):
        ingress = SyntheticAPIIngress(self.normalized(cert_manager="install"), self.directory)
        webhook = {"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration", "metadata": {
            "name": "cert-manager-webhook", "annotations": {"cert-manager.io/inject-ca-from-secret": "cert-manager/cert"},
        }, "webhooks": [{"name": "webhook.cert-manager.io", "clientConfig": {"service": {"name": "webhook"}}}]}
        actual = copy.deepcopy(webhook)
        actual["webhooks"][0]["clientConfig"]["caBundle"] = "Y2E="
        self.assertTrue(ingress.cm_resource_matches(webhook, actual))
        actual["webhooks"][0]["clientConfig"]["caBundle"] = ""
        with self.assertRaisesRegex(INGRESS.IngressError, "no injected CA bundle"):
            ingress.cm_resource_matches(webhook, actual)

    def test_cert_manager_recovery_keeps_template_crds_in_release_manifest_comparison(self):
        ingress = SyntheticAPIIngress(self.normalized(cert_manager="install"), self.directory)
        crd = {"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": {
            "name": "certificates.cert-manager.io",
        }, "spec": {"group": "cert-manager.io"}}
        hook = {"apiVersion": "batch/v1", "kind": "Job", "metadata": {
            "name": "startup", "annotations": {"helm.sh/hook": "post-install"},
        }}
        ingress.cm_documents = [crd, hook]
        self.assertEqual(ingress.cm_manifest_documents(), [crd])

    def test_cert_manager_recovery_accepts_only_the_dynamic_deployment_revision_annotation(self):
        ingress = SyntheticAPIIngress(self.normalized(cert_manager="install"), self.directory)
        deployment = {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {
            "name": "cert-manager", "labels": {"app.kubernetes.io/managed-by": "Helm"},
        }}
        live = copy.deepcopy(deployment)
        live["metadata"].update({"annotations": {
            "meta.helm.sh/release-name": "cert-manager", "meta.helm.sh/release-namespace": "cert-manager",
            "deployment.kubernetes.io/revision": "2",
        }})
        self.assertTrue(ingress.cm_resource_metadata_matches(deployment, live, "cert-manager", "cert-manager"))
        live["metadata"]["annotations"]["unexpected"] = "value"
        self.assertFalse(ingress.cm_resource_metadata_matches(deployment, live, "cert-manager", "cert-manager"))

    def test_failed_cert_manager_recovery_refuses_an_unmanaged_surviving_resource(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            document = {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {
                "name": "cert-manager", "namespace": "cert-manager"}, "spec": {"replicas": 1}}
            ingress.cm_documents = [document]
            ingress.cert_manager_manifest = yaml.safe_dump_all([document])
            ingress.cert_manager_hooks = ""
            ingress.state["certManagerInstall"]["renderSHA256"] = INGRESS.document_digest(ingress.cm_documents)
            ingress.get = lambda *args, **kwargs: {"metadata": {"name": "cert-manager", "annotations": {}, "labels": {}}}
            with self.assertRaisesRegex(INGRESS.IngressError, "ownership conflict"):
                ingress.cert_manager_check()

    def test_failed_cert_manager_recovery_refuses_an_existing_post_install_hook(self):
        hook = {"apiVersion": "batch/v1", "kind": "Job", "metadata": {
            "name": "cert-manager-startupapicheck", "namespace": "cert-manager",
            "labels": {"app": "startupapicheck"}, "annotations": {"helm.sh/hook": "post-install"},
        }, "spec": {"template": {"spec": {"restartPolicy": "Never"}}}}
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress, _ = self.failed_cert_manager_recovery(scratch)
            ingress.cm_documents = [ingress.cm_documents[0], hook]
            ingress.cert_manager_manifest = yaml.safe_dump_all([ingress.cm_documents[0]])
            ingress.cert_manager_hooks = yaml.safe_dump_all([hook])
            ingress.state["certManagerInstall"]["renderSHA256"] = INGRESS.document_digest(ingress.cm_documents)
            original_get = ingress.get
            ingress.get = lambda kind, name, *args, **kwargs: copy.deepcopy(hook) if name == hook["metadata"]["name"] else original_get(kind, name, *args, **kwargs)
            with self.assertRaisesRegex(INGRESS.IngressError, "post-install hook still exists"):
                ingress.cert_manager_check()

    def test_cert_manager_install_journals_before_helm_and_uses_verified_archive_snapshot(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(self.normalized(cert_manager="install"), scratch)
            source_archive = self.pinned_cert_manager_archive(ingress)
            ingress.load_state(write=True)
            calls = []

            def helm(*args, **kwargs):
                calls.append(args)
                self.assertEqual(ingress.state["lastStep"], {"name": "cert-manager-install", "status": "started"})
                self.assertEqual(ingress.state["certManagerArchiveSHA256"], ingress.lock["chart"]["archiveSHA256"])
                return ""

            ingress.helm = helm
            ingress.install_cert_manager()
            archive_snapshot_exists = ingress.cm_archive.is_file()

        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][:2], ("upgrade", "--install"))
        self.assertEqual(Path(calls[0][3]), ingress.cm_archive)
        self.assertNotEqual(ingress.cm_archive, source_archive)
        self.assertTrue(archive_snapshot_exists)
        self.assertEqual(ingress.state["lastStep"], {"name": "cert-manager-install", "status": "completed"})

    def test_prepare_refuses_a_second_local_writer_for_the_same_state_directory(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            ingress.load_state(write=True)
            descriptor = os.open(self.directory / "state" / ".prepare.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with self.assertRaisesRegex(INGRESS.IngressError, "Another ingress preparation"):
                    ingress.prepare()
            finally:
                os.close(descriptor)

    def test_verify_rejects_an_issuer_that_is_no_longer_ready(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            with patch.object(INGRESS, "check_dns"):
                ingress.prepare()
            production_issuer = ingress._key("issuer.cert-manager.io", "cetirun-letsencrypt-production", value["ingress"]["namespace"])
            ingress.live[production_issuer]["status"]["conditions"][0]["status"] = "False"
            with patch.object(INGRESS, "check_dns"):
                with self.assertRaisesRegex(INGRESS.IngressError, "Issuer is not Ready"):
                    ingress.verify()

    def test_preflight_rejects_redirect_and_solver_parent_reference_drift(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            with patch.object(INGRESS, "check_dns"):
                ingress.prepare()
            namespace = value["ingress"]["namespace"]
            route = ingress.live[ingress._key("httproute.gateway.networking.k8s.io", "cetirun-http-redirect", namespace)]
            issuer = ingress.live[ingress._key("issuer.cert-manager.io", "cetirun-letsencrypt-production", namespace)]
            references = [route["spec"]["parentRefs"][0], issuer["spec"]["acme"]["solvers"][0]["http01"]["gatewayHTTPRoute"]["parentRefs"][0]]
            for reference in references:
                for field, invalid in (("namespace", "other"), ("group", "other.example.invalid"), ("kind", "Service")):
                    with self.subTest(resource="redirect" if reference is references[0] else "solver", field=field):
                        original = reference[field]
                        reference[field] = invalid
                        with self.assertRaisesRegex(INGRESS.IngressError, "configuration differs"):
                            ingress.ingress_ownership_check()
                        reference[field] = original

    def test_verify_rejects_stale_gateway_and_unresolved_route_before_tls(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            with patch.object(INGRESS, "check_dns"):
                ingress.prepare()
            namespace = value["ingress"]["namespace"]
            gateway = ingress.live[ingress._key("gateway.gateway.networking.k8s.io", value["ingress"]["gatewayName"], namespace)]
            route = ingress.live[ingress._key("httproute.gateway.networking.k8s.io", "cetirun-http-redirect", namespace)]
            gateway["metadata"]["generation"] += 1
            writes_before = len(ingress.helm_values)
            with patch.object(INGRESS, "check_dns"), patch.object(INGRESS.socket, "create_connection") as connect:
                with self.assertRaisesRegex(INGRESS.IngressError, "Gateway is not Programmed"):
                    ingress.verify()
                gateway["status"]["conditions"][0]["observedGeneration"] = gateway["metadata"]["generation"]
                for condition in route["status"]["parents"][0]["conditions"]:
                    if condition["type"] == "ResolvedRefs":
                        condition["status"] = "False"
                with self.assertRaisesRegex(INGRESS.IngressError, "route is not Accepted with ResolvedRefs"):
                    ingress.verify()
                connect.assert_not_called()
            self.assertEqual(len(ingress.helm_values), writes_before)

    def test_renewal_check_rejects_an_elapsed_certificate_renewal_time(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            certificate = {"status": {
                "notBefore": "2020-01-01T00:00:00Z",
                "renewalTime": "2021-01-01T00:00:00Z",
                "notAfter": "2031-01-01T00:00:00Z",
                "failedIssuanceAttempts": 0,
            }}
            with self.assertRaisesRegex(INGRESS.IngressError, "healthy future renewal"):
                ingress.renewal_check(certificate)

    def test_verify_uses_only_read_apis_after_preparation(self):
        value = self.normalized()
        with tempfile.TemporaryDirectory(prefix="cetirun-public-ingress-scratch-") as scratch:
            ingress = SyntheticAPIIngress(value, scratch)
            with patch.object(INGRESS, "check_dns"):
                ingress.prepare()
            writes_before = len(ingress.helm_values)
            raw = MagicMock()
            raw.__enter__.return_value = raw
            tls = MagicMock()
            tls.__enter__.return_value = tls
            tls.getpeercert.return_value = b"synthetic-public-certificate"
            context = MagicMock()
            context.wrap_socket.return_value = tls
            with patch.object(INGRESS, "check_dns"), \
                 patch.object(INGRESS.socket, "create_connection", return_value=raw), \
                 patch.object(INGRESS.ssl, "create_default_context", return_value=context):
                verified = ingress.verify()
                self.assertEqual(context.minimum_version, INGRESS.ssl.TLSVersion.TLSv1_2)

        self.assertEqual(verified["portalReadiness"], "verified")
        self.assertEqual(len(ingress.helm_values), writes_before)
        self.assertEqual(ingress.http_calls, [("/", False), ("/", True), ("/api/v1/me", True), ("/healthz", True), ("/readyz", True)])


if __name__ == "__main__":
    unittest.main()
