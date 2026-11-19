# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Keep deployment checks active when client schema downloads are unavailable."""

import copy
import json
import os
import subprocess
import unittest
from unittest.mock import patch

from scripts.tests.test_full_deployment import DEPLOY, DeploymentFixture


class DeploymentTransportTests(DeploymentFixture, unittest.TestCase):
    def test_client_validation_defaults_on_and_requires_boolean(self):
        config, _ = self.configure(self.config)
        self.assertNotIn("clientOpenAPIValidation", config["release"])
        for value in (None, "false", 0, [], {}):
            with self.subTest(value=value), patch.object(DEPLOY.subprocess, "run") as run:
                self.config["release"]["clientOpenAPIValidation"] = value
                with self.assertRaisesRegex(ValueError, "clientOpenAPIValidation must be a boolean"):
                    self.configure(self.config)
                run.assert_not_called()

    def test_default_upgrades_keep_client_schema_validation(self):
        deployment = self.deployment()
        self.assertTrue(deployment.plan()["clientOpenAPIValidation"])
        deployment.execute()
        self.assertTrue(deployment.applied)
        for _, _, command in deployment.applied:
            self.assertNotIn("--disable-openapi-validation", command)

    def transport_deployment(self):
        self.config["release"]["clientOpenAPIValidation"] = False
        return self.deployment()

    def test_explicit_transport_keeps_staging_and_strict_admission_dry_run(self):
        deployment = self.transport_deployment()
        self.assertFalse(deployment.plan()["clientOpenAPIValidation"])
        original = deployment.kubectl
        probes = []

        def kubectl(*args, input_text=None):
            if args[:2] == ("create", "--raw"):
                self.assertEqual(args, ("create", "--raw",
                    "/apis/cetirun.microsoft.com/v1alpha1/teams?dryRun=All&fieldValidation=Strict", "-f", "-"))
                document = json.loads(input_text)
                self.assertEqual(document["apiVersion"], "cetirun.microsoft.com/v1alpha1")
                self.assertEqual(document["kind"], "Team")
                self.assertEqual(document["metadata"]["name"], "cetirun-admission-deployment-check")
                probes.append(document)
                return input_text
            return original(*args, input_text=input_text)

        with patch.object(deployment, "kubectl", side_effect=kubectl):
            deployment.execute()
        self.assertEqual([name for name, _, _ in deployment.applied], ["istio", "broker", "platform", "platform"])
        for _, _, command in deployment.applied:
            self.assertIn("--disable-openapi-validation", command)
        self.assertEqual(len(probes), 1)

    def test_transport_cannot_bypass_failed_preflight_or_admission(self):
        deployment = self.transport_deployment()
        with patch.object(deployment, "preflight", side_effect=ValueError("target or ownership mismatch")):
            with self.assertRaisesRegex(ValueError, "target or ownership mismatch"):
                deployment.execute()
        self.assertFalse(deployment.writes)
        original = deployment.kubectl

        def deny(*args, input_text=None):
            if args[:2] == ("create", "--raw"):
                raise ValueError("admission rejected the dry-run")
            return original(*args, input_text=input_text)

        with patch.object(deployment, "kubectl", side_effect=deny):
            with self.assertRaisesRegex(ValueError, "admission rejected the dry-run"):
                deployment.execute()


class PublicTLSInputTests(DeploymentFixture, unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.config.pop("domain")
        self.config["target"]["kubeconfig"] = str(self.root / "kubeconfig")
        self.config["publicTLS"] = {
            "enabled": True, "dns": {"mode": "azure-public-ip", "label": "example-cetirun"},
            "azure": {"subscriptionId": "11111111-1111-1111-1111-111111111111",
                      "tenantId": "22222222-2222-2222-2222-222222222222",
                      "resourceGroup": "example-rg", "clusterName": "example-aks"},
            "certManager": {"mode": "existing"}, "stateDirectory": str(self.root / "state"),
        }

    def test_azure_domain_rebuilds_all_consumers_from_the_original_configuration(self):
        with patch.object(DEPLOY.subprocess, "run") as run:
            config, values = self.configure(self.config)
            self.assertEqual(config["domain"], "pending-public-tls.example.invalid")
            actual = "example-cetirun.hash.swedencentral.cloudapp.azure.com"
            resolved, values = DEPLOY.configuration(config["_source"], domain=actual)
            self.assertEqual(resolved["domain"], actual)
            self.assertEqual(values["platform"]["cetirun-operator"]["domainSuffix"], actual)
            self.assertEqual(values["platform"]["cetirun-portal"]["istio"]["host"], actual)
            self.assertEqual(values["platform"]["cetirun-connections-mcp"]["portal"]["baseURL"], "https://" + actual)
            self.assertEqual(values["broker"]["agentIdentity"]["redirectUri"], "https://" + actual + "/api/v1/connections/callback")
            self.assertEqual(values["istio"]["domain"], actual)
            self.assertFalse(values["istio"]["gateway"]["includeWildcardHost"])
            self.assertFalse(values["platform"]["publicTLS"]["bootstrapOnly"])
            self.assertTrue(values["platform"]["publicTLS"]["acme"]["productionCertificateEnabled"])
            self.assertNotIn("domain", resolved["_source"])
            run.assert_not_called()

    def test_custom_domain_and_reserved_configuration_fail_before_tools(self):
        self.config["publicTLS"].pop("azure")
        self.config["publicTLS"]["dns"] = {"mode": "custom", "domain": "portal.example.com"}
        configured, _ = self.configure(self.config)
        self.assertEqual(configured["domain"], "portal.example.com")
        cases = [
            ("domain", "other.example.com", "does not match"),
            ("target.kubeconfig", "relative", "absolute path"),
            ("platform.values.publicTLS", {"bootstrapOnly": True}, "reserved"),
            ("publicTLS.bootstrapOnly", True, "unsupported fields"),
            ("istio.values.gateway.includeWildcardHost", True, "exact hostname"),
            ("istio.tlsMode", "retain", "tlsMode=external"),
            ("publicTLS.certManager.namespace", self.config["platform"]["namespace"], "separate namespace"),
        ]
        for field, value, message in cases:
            with self.subTest(field=field), patch.object(DEPLOY.subprocess, "run") as run:
                invalid = copy.deepcopy(self.config)
                DEPLOY.set_value(invalid, field, value)
                with self.assertRaisesRegex(ValueError, message):
                    self.configure(invalid)
                run.assert_not_called()

    def test_managed_tls_rejects_an_independent_helm_history_driver(self):
        with patch.dict(os.environ, {"HELM_DRIVER": "configmap"}), patch.object(DEPLOY.subprocess, "run") as run:
            with self.assertRaisesRegex(ValueError, "Secret storage driver"):
                self.configure(self.config)
            run.assert_not_called()

    def test_target_kubeconfig_is_used_by_all_live_commands_but_not_offline_rendering(self):
        config, values = self.configure(self.config)
        deployment = DEPLOY.Deployment(config, values, self.root / "render")
        with patch.object(DEPLOY.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "", "")) as run:
            deployment.kubectl("version", "-o", "json")
            deployment.helm("list", "--namespace", "cetirun-system")
            deployment.run(["python3", "mesh-helper.py"])
            for call in run.call_args_list:
                self.assertEqual(call.kwargs["env"]["KUBECONFIG"], config["target"]["kubeconfig"])
                self.assertEqual(call.kwargs["env"]["HELM_DRIVER"], "secret")
            deployment.run(["helm", "template", "example"], env={"KUBECONFIG": os.devnull})
            self.assertEqual(run.call_args.kwargs["env"]["KUBECONFIG"], os.devnull)


if __name__ == "__main__":
    unittest.main()
