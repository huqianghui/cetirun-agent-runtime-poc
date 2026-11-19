# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline composition checks for managed public TLS and the platform deployer."""

import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import yaml

from scripts.tests import test_full_deployment as full
from scripts.tests import test_public_ingress as ingress_tests


class BoundIngress(ingress_tests.SyntheticAPIIngress):
    """Use the ingress fake's TLS APIs with the deployment fake's app state."""

    def __init__(self, config, scratch, cluster):
        super().__init__(config, scratch)
        self.cluster = cluster

    def kube(self, *args, **kwargs):
        if args[:2] == ("config", "view"):
            target = self.config["target"]
            return target["cluster"] + "\n" + target["server"]
        return super().kube(*args, **kwargs)

    def get(self, kind, name, namespace=None, **kwargs):
        if kind == "namespace" and name == "kube-system":
            return {"metadata": {"uid": self.config["target"]["clusterUID"]}}
        return super().get(kind, name, namespace, **kwargs)

    def _deploy_ingress(self, production):
        super()._deploy_ingress(production)
        if production:
            namespace = self.config["ingress"]["namespace"]
            self.cluster.objects[("secret", namespace, "cetirun-tls")] = {"name": "cetirun-tls", "uid": "cert-manager-cetirun-tls"}
            self.cluster.keys[(namespace, "cetirun-tls")] = {"tls.crt", "tls.key"}
        # Keep the deployment fake's Helm view coherent with the independently
        # rendered certificate-only revision.
        self.cluster.installed.add(self.config["ingress"]["release"])
        self.cluster.public_tls_release = {
            "values": copy.deepcopy(self.platform_values),
            "manifest": self.platform_manifest,
            "state": self.platform_release,
        }

    def az(self, *args):
        result = super().az(*args)
        if args[:2] == ("aks", "show"):
            result["fqdn"] = self.config["target"]["server"].split("://", 1)[1].split(":", 1)[0]
        return result


class PublicTLSDeploymentTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="cetirun-public-tls-deployment-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def deployment(self, *, mode="custom", registry=None, version=None):
        source = yaml.safe_load(full.FIXTURE.read_text())
        source["platform"]["values"]["cetirun-operator"]["cetiadmin"] = {"enabled": False}
        kubeconfig = self.root / "admin.kubeconfig"
        kubeconfig.write_text("synthetic")
        source["target"]["kubeconfig"] = str(kubeconfig)
        tls = {"enabled": True, "certManager": {"mode": "existing"},
               "stateDirectory": str(self.root / "state")}
        if mode == "custom":
            tls["dns"] = {"mode": "custom", "domain": source["domain"]}
        else:
            source.pop("domain")
            tls["dns"] = {"mode": "azure-public-ip", "label": "portal-ingress"}
            tls["azure"] = {"subscriptionId": ingress_tests.SUBSCRIPTION, "tenantId": ingress_tests.TENANT,
                            "resourceGroup": "example-rg", "clusterName": "example-aks"}
        source["publicTLS"] = tls
        config, values = full.DEPLOY.configuration(copy.deepcopy(source), registry=registry, version=version)
        cluster = full.SyntheticCluster(config, values, self.root / "charts")
        cluster.public_tls_release = {"values": {}, "manifest": "", "state": "absent"}
        cluster_run = cluster.run

        def run(command, **kwargs):
            if command[:3] == ["helm", "--kube-context", cluster.config["target"]["context"]]:
                args = command[3:]
                record = cluster.public_tls_release
                if args and args[0] == "list" and record["state"] != "absent":
                    release = args[args.index("--filter") + 1].strip("^$")
                    if release == cluster.config["platform"]["release"]:
                        return json.dumps([{"name": release, "status": "failed" if record["state"] == "failed-full" else "deployed",
                                            "revision": "1", "chart": "cetirun-platform-0.1.1"}])
                if args[:2] == ["get", "values"] and record["state"] != "absent":
                    return json.dumps(record["values"])
                if args[:2] == ["get", "manifest"] and record["state"] != "absent":
                    return record["manifest"]
                if args[:2] == ["get", "hooks"] and record["state"] != "absent":
                    return ""
            return cluster_run(command, **kwargs)

        cluster.run = run
        captured = {}

        def factory(tls_config, scratch):
            result = BoundIngress(tls_config, scratch, cluster)
            captured["ingress"] = result
            return result

        with patch.object(full.DEPLOY.PUBLIC_TLS, "Ingress", side_effect=factory):
            cluster.render()
        cluster.seed_references()
        # This output must be created by cert-manager during the helper stage,
        # never accepted as a pre-existing deployment reference.
        cluster.objects.pop(("secret", config["istio"]["ingressNamespace"], "cetirun-tls"), None)
        cluster.keys.pop((config["istio"]["ingressNamespace"], "cetirun-tls"), None)
        ingress = captured["ingress"]
        original_upgrade = cluster.upgrade

        def upgrade(name, *args):
            if name == getattr(cluster, "fail_upgrade_name", None):
                raise ValueError("Synthetic upgrade interruption: " + name)
            original_upgrade(name, *args)
            if name == "platform":
                values = next(item[1] for item in reversed(cluster.applied) if item[0] == "platform")
                ingress.platform_release = "full"
                ingress.platform_values = copy.deepcopy(values)
                ingress.platform_manifest = yaml.safe_dump_all(cluster.documents["platform"])
                cluster.public_tls_release = {"values": copy.deepcopy(values), "manifest": ingress.platform_manifest, "state": "full"}

        cluster.upgrade = upgrade
        return cluster, ingress, factory

    def test_custom_domain_bootstrap_hands_off_to_the_full_platform_release(self):
        cluster, ingress, factory = self.deployment()
        with patch.object(ingress_tests.INGRESS, "check_dns"):
            cluster.execute()
        platform = [values for name, values, _ in cluster.applied if name == "platform"]
        # Deployment's normal admission bootstrap is followed by one full
        # platform release; public TLS itself did not become an extra app run.
        self.assertEqual(len(platform), 2)
        self.assertEqual(platform[-1]["publicTLS"]["domain"], "portal.example.invalid")
        self.assertFalse(platform[-1]["publicTLS"]["bootstrapOnly"])
        self.assertEqual(ingress.platform_release, "full")
        self.assertEqual(cluster.config["domain"], "portal.example.invalid")

    def test_azure_returned_fqdn_is_propagated_into_the_full_platform_release(self):
        cluster, ingress, factory = self.deployment(mode="azure")
        with patch.object(ingress_tests.INGRESS, "check_dns"):
            cluster.execute()
        platform = [values for name, values, _ in cluster.applied if name == "platform"][-1]
        self.assertEqual(cluster.config["domain"], "portal-ingress.example.cloudapp.azure.com")
        self.assertEqual(platform["publicTLS"]["domain"], "portal-ingress.example.cloudapp.azure.com")

    def test_domain_resolution_preserves_release_coordinate_overrides(self):
        registry, version = "registry.example.com:5000/team/test", "1.2.3-dev.1"
        cluster, ingress, _ = self.deployment(mode="azure", registry=registry, version=version)
        with patch.object(ingress_tests.INGRESS, "check_dns"):
            cluster.execute()
        self.assertEqual(cluster.config["domain"], "portal-ingress.example.cloudapp.azure.com")
        self.assertEqual(cluster.config["release"]["registry"], registry)
        self.assertEqual(cluster.config["release"]["version"], version)
        self.assertEqual(cluster.config["_releaseOrigins"], {"registry": "explicit override", "version": "explicit override"})
        for component, values, _ in cluster.applied:
            if component == "platform":
                self.assertEqual(values["global"]["registry"], registry)
                self.assertEqual(values["global"]["version"], version)
            elif component == "broker":
                self.assertEqual(values["image"]["repository"], registry + "/cetirun-agent-broker")
                self.assertEqual(values["image"]["tag"], version)
        for chart in cluster.charts.values():
            metadata = yaml.safe_load((chart / "Chart.yaml").read_text())
            self.assertEqual(metadata["version"], version)
            self.assertEqual(metadata["appVersion"], version)

    def test_full_or_failed_full_retry_preserves_the_tls_resources_without_a_minimal_upgrade(self):
        cluster, ingress, _ = self.deployment()
        with patch.object(ingress_tests.INGRESS, "check_dns"):
            cluster.execute()
        before = copy.deepcopy(ingress.live)
        retained_secrets = {key: copy.deepcopy(value) for key, value in cluster.objects.items() if key[0] == "secret"}
        for state in ("full", "failed-full"):
            ingress.platform_release = state
            cluster.public_tls_release["state"] = state
            writes = len(ingress.helm_values)
            applied = len(cluster.applied)
            with patch.object(ingress_tests.INGRESS, "check_dns"):
                cluster.execute()
            self.assertEqual(len(ingress.helm_values), writes)
            self.assertEqual(ingress.live, before)
            for key, metadata in retained_secrets.items():
                self.assertEqual(cluster.objects[key], metadata)
            platform = [values for name, values, _ in cluster.applied[applied:] if name == "platform"]
            self.assertEqual(len(platform), 1)
            self.assertFalse(platform[0]["publicTLS"]["bootstrapOnly"])
            self.assertTrue(platform[0]["cetirun-operator"]["sandboxAdmission"]["enforce"])

    def test_missing_internal_tls_reference_blocks_fresh_azure_before_dns_or_release_writes(self):
        cluster, ingress, factory = self.deployment(mode="azure")
        namespace = cluster.config["platform"]["namespace"]
        secret = cluster.values["platform"]["cetirun-operator"]["sandboxAdmission"]["tlsSecretName"]
        cluster.objects.pop(("secret", namespace, secret))
        cluster.keys.pop((namespace, secret), None)
        writes_before = len(cluster.writes)
        with self.assertRaises(ValueError):
            cluster.execute()
        azure_updates = [call for call in ingress.api_calls if call[0] == "az" and call[1][:3] == ("network", "public-ip", "update")]
        self.assertEqual(azure_updates, [])
        self.assertEqual(cluster.writes[writes_before:], [])

    def test_interrupted_certificate_bootstrap_retries_with_retained_tls_ownership(self):
        cluster, ingress, factory = self.deployment()
        cluster.fail_upgrade_name = "istio"
        with patch.object(ingress_tests.INGRESS, "check_dns"):
            with self.assertRaisesRegex(ValueError, "Synthetic upgrade interruption"):
                cluster.execute()
        retained = copy.deepcopy(ingress.live)
        self.assertEqual(ingress.platform_release, "bootstrap")
        values = json.loads(cluster.run(["helm", "--kube-context", cluster.config["target"]["context"], "get", "values",
                                         cluster.config["platform"]["release"], "--namespace", cluster.config["platform"]["namespace"]]))
        self.assertTrue(values["publicTLS"]["bootstrapOnly"])
        cluster.fail_upgrade_name = None
        with patch.object(ingress_tests.INGRESS, "check_dns"):
            cluster.execute()
        self.assertEqual(ingress.platform_release, "full")
        for key, resource in retained.items():
            self.assertIn(key, ingress.live)
            self.assertEqual(ingress.live[key]["metadata"]["uid"], resource["metadata"]["uid"])


if __name__ == "__main__":
    unittest.main()
