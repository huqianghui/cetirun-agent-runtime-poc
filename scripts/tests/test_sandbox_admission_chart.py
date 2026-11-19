# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline Helm packaging checks; CEL behavior is covered by operator integration tests."""

import base64
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "app/cetirun-operator/chart"
NAME = "cetirun-sandbox-admission"
API_GROUP = "cetirun.microsoft.com"
RESOURCES = ["agents", "teams", "sandboxes", "agentpolicies"]
FINALIZERS = [
    f"{API_GROUP}/finalizer",
    f"{API_GROUP}/sandbox-hold",
    f"{API_GROUP}/sandbox-preparation",
    f"{API_GROUP}/sandbox-bootstrap",
    f"{API_GROUP}/custom-runtime-ownership",
    f"{API_GROUP}/custom-apply-completion",
]
OFFLINE_ENV = {
    **os.environ,
    "KUBECONFIG": os.devnull,
    "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}


@unittest.skipUnless(shutil.which("helm"), "requires Helm 3 or 4")
class SandboxAdmissionChartTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        version = subprocess.run(
            ["helm", "version", "--short"], cwd=ROOT, env=OFFLINE_ENV,
            text=True, capture_output=True, check=True, timeout=30,
        ).stdout
        if not version.startswith(("v3.", "v4.")):
            raise RuntimeError("Sandbox chart tests require Helm 3 or 4 on PATH")
        if shutil.which("openssl") is None:
            raise RuntimeError("Sandbox chart tests require OpenSSL for synthetic CA fixtures")
        # The synthetic key and certificate stay in memory, never in repository files.
        key = subprocess.run(
            ["openssl", "genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256"],
            cwd=ROOT, text=True, capture_output=True, check=True, timeout=30,
        ).stdout
        cls.ca_pem = subprocess.run(
            [
                "openssl", "req", "-new", "-x509", "-key", "/dev/stdin",
                "-subj", "/CN=sandbox-admission.example.com", "-days", "1",
                "-addext", "basicConstraints=critical,CA:TRUE",
            ],
            cwd=ROOT, input=key, text=True, capture_output=True, check=True, timeout=30,
        ).stdout
        cls.ca_bundle = cls.encode_ca(cls.ca_pem)

    @staticmethod
    def encode_ca(pem):
        return base64.b64encode(pem.encode()).decode()

    def server_values(self, **overrides):
        return {
            "enabled": True,
            "tlsSecretName": "synthetic-admission-tls",
            "adminUsers": ["admin@example.com"],
            **overrides,
        }

    def enforced_values(self, **overrides):
        return self.server_values(enforce=True, caBundle=self.ca_bundle, **overrides)

    def helm(self, admission=None, *, values=None, kube_version="1.37.0", lint=False, chart=CHART):
        inputs = dict(values or {})
        # These cases isolate the existing Sandbox contract. CetiAdmin's default
        # enabled path and the shared server are exercised in its own suite.
        if chart.name == "cetirun-platform":
            operator = dict(inputs.get("cetirun-operator", {}))
            operator.setdefault("cetiadmin", {"enabled": False})
            inputs["cetirun-operator"] = operator
        else:
            inputs.setdefault("cetiadmin", {"enabled": False})
        if admission is not None:
            inputs["sandboxAdmission"] = admission
        command = (
            ["helm", "lint", "--strict", str(chart)]
            if lint else ["helm", "template", "admission-test", str(chart)]
        )
        return subprocess.run(
            [
                *command, "--namespace", "synthetic-release-namespace",
                "--kube-version", kube_version, "--values", "-",
            ],
            cwd=ROOT, env=OFFLINE_ENV, input=json.dumps(inputs),
            text=True, capture_output=True, timeout=30,
        )

    def render(self, admission=None, **kwargs):
        result = self.helm(admission, **kwargs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return {
            (document["kind"], document["metadata"]["name"]): document
            for document in yaml.safe_load_all(result.stdout) if document
        }

    def assert_rejected(self, admission, message, **kwargs):
        result = self.helm(admission, **kwargs)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn(message, result.stderr)

    @staticmethod
    def admission_environment(deployment):
        container = deployment["spec"]["template"]["spec"]["containers"][0]
        return next(item for item in container["env"] if item["name"] == "CETIRUN_SANDBOX_ADMISSION")

    def platform_admission_config(self, result):
        self.assertEqual(result.returncode, 0, result.stderr)
        deployment = next(
            document for document in yaml.safe_load_all(result.stdout)
            if document and document["kind"] == "Deployment" and document["metadata"]["name"] == NAME
        )
        return json.loads(self.admission_environment(deployment)["value"])

    def test_default_and_disabled_render_only_existing_operator_resources(self):
        default = self.render(kube_version="1.29.0")
        self.assertEqual(set(default), {
            ("Namespace", "cetirun-registry-credentials"),
            ("Deployment", "cetirun-operator"),
            ("ServiceAccount", "cetirun-operator-controller-manager"),
            ("ClusterRole", "cetirun-operator-manager-role"),
            ("ClusterRole", "cetirun-terminal-exec"),
            ("ClusterRoleBinding", "cetirun-operator-manager-rolebinding"),
            ("ConfigMap", "cetirun-cetiadmin-config"),
            *((kind, "cetirun-cetiadmin-" + mode) for mode in ("check", "stop")
              for kind in ("Job", "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding")),
            *(("CustomResourceDefinition", f"{resource}.{API_GROUP}") for resource in RESOURCES),
        })
        credentials = default[("Namespace", "cetirun-registry-credentials")]
        self.assertEqual(credentials["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertEqual(credentials["metadata"]["labels"]["cetirun.microsoft.com/credential-purpose"], "registry")
        disabled = self.render(
            {
                "enabled": False, "enforce": False, "replicaCount": 1,
                "tlsSecretName": "", "caBundle": "unused",
                "adminUsers": [], "adminGroups": [], "timeoutSeconds": 0,
            },
            kube_version="1.29.0",
        )
        self.assertEqual(disabled, default)

    def test_bootstrap_deploys_server_without_webhooks_or_certificate_issuance(self):
        default = self.render()
        bootstrap = self.render(self.server_values())
        for identity, document in default.items():
            self.assertEqual(bootstrap[identity], document)
        self.assertEqual(set(bootstrap) - set(default), {
            (kind, NAME) for kind in (
                "Deployment", "ServiceAccount", "Service", "ClusterRole",
                "ClusterRoleBinding", "PodDisruptionBudget",
            )
        })

    def test_crds_are_retained_ordinary_templates(self):
        documents = self.render()
        for resource in RESOURCES:
            definition = documents[("CustomResourceDefinition", f"{resource}.{API_GROUP}")]
            self.assertEqual(definition["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
            self.assertNotIn("helm.sh/hook", definition["metadata"]["annotations"])
        result = subprocess.run(
            ["helm", "show", "crds", str(CHART)], cwd=ROOT, env=OFFLINE_ENV,
            text=True, capture_output=True, check=True, timeout=30,
        )
        self.assertEqual(result.stdout.strip(), "")

    def test_validation_mode_is_explicit_without_changing_webhook_failure_policy(self):
        for mode in ("compatible", "strict"):
            with self.subTest(mode=mode):
                documents = self.render(self.enforced_values(validationMode=mode))
                container = documents[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
                config = json.loads(next(item["value"] for item in container["env"] if item["name"] == "CETIRUN_SANDBOX_ADMISSION"))
                self.assertEqual(config["validationMode"], mode)
                for kind in ("MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"):
                    for hook in documents[(kind, NAME)]["webhooks"]:
                        self.assertEqual(hook["failurePolicy"], "Fail")
        for invalid in ("ignore", "", True, 1):
            with self.subTest(invalid=invalid):
                self.assert_rejected(self.server_values(validationMode=invalid), "validationMode must be compatible or strict")

    def test_coordination_pins_existing_state_without_adding_rbac_or_webhooks(self):
        baseline_values = self.enforced_values(podsEnabled=True, validationMode="strict")
        baseline = self.render(baseline_values)
        configured = self.render({
            **baseline_values,
            "coordinationConfigMapName": "sandbox-coordination",
            "coordinationConfigMapUID": "synthetic-coordination-uid",
        })
        self.assertEqual(set(configured), set(baseline))
        container = configured[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
        env = next(item for item in container["env"] if item["name"] == "CETIRUN_SANDBOX_ADMISSION")
        config = json.loads(env["value"])
        self.assertEqual(config.pop("coordinationConfigMapName"), "sandbox-coordination")
        self.assertEqual(config.pop("coordinationConfigMapUID"), "synthetic-coordination-uid")
        previous = baseline[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
        previous_env = next(item for item in previous["env"] if item["name"] == "CETIRUN_SANDBOX_ADMISSION")
        self.assertEqual(config, json.loads(previous_env["value"]))
        env["value"] = previous_env["value"]
        self.assertEqual(configured, baseline)

    def test_coordination_requires_strict_enforcement_and_exact_separate_identity(self):
        valid = self.enforced_values(
            podsEnabled=True, validationMode="strict",
            coordinationConfigMapName="sandbox-coordination",
            coordinationConfigMapUID="synthetic-coordination-uid",
        )
        for override in (
            {"validationMode": "compatible"}, {"enabled": False},
            {"enforce": False}, {"podsEnabled": False},
        ):
            with self.subTest(override=override):
                self.assert_rejected({**valid, **override}, "coordination requires strict")
        for override in (
            {"coordinationConfigMapName": ""},
            {"coordinationConfigMapName": "bad/name"},
            {"coordinationConfigMapName": "x" * 254},
        ):
            with self.subTest(override=override):
                self.assert_rejected({**valid, **override}, "separate valid ConfigMap name")
        for uid in ("", "white space", "line\nbreak", "x" * 129):
            with self.subTest(uid=uid):
                self.assert_rejected(
                    {**valid, "coordinationConfigMapUID": uid},
                    "must pin a bounded exact UID",
                )
        for key in ("coordinationConfigMapName", "coordinationConfigMapUID"):
            for invalid in (42, False, [], {}):
                with self.subTest(key=key, invalid=invalid):
                    self.assert_rejected({**valid, key: invalid}, "name and UID must be strings")
        self.assert_rejected(
            {**valid, "placementConfigMapName": "sandbox-coordination"},
            "separate valid ConfigMap name",
        )

    def test_omitted_coordination_defaults_preserve_disabled_rendering(self):
        with tempfile.TemporaryDirectory(prefix="admission-reused-values-") as directory:
            chart = Path(directory) / "chart"
            shutil.copytree(CHART, chart)
            defaults = yaml.safe_load((chart / "values.yaml").read_text())
            defaults["sandboxAdmission"].pop("coordinationConfigMapName")
            defaults["sandboxAdmission"].pop("coordinationConfigMapUID")
            (chart / "values.yaml").write_text(yaml.safe_dump(defaults))
            for name, admission in (
                ("disabled", None),
                ("serving", self.server_values()),
                ("enforced", self.enforced_values(podsEnabled=True, validationMode="strict")),
            ):
                with self.subTest(mode=name):
                    self.assertEqual(self.render(admission, chart=chart), self.render(admission))

    def test_omitted_portal_service_account_preserves_rendering(self):
        # Upgrades with --reuse-values from an older release carry no value.
        with tempfile.TemporaryDirectory(prefix="admission-reused-values-") as directory:
            chart = Path(directory) / "chart"
            shutil.copytree(CHART, chart)
            defaults = yaml.safe_load((chart / "values.yaml").read_text())
            defaults["sandboxAdmission"].pop("portalServiceAccount")
            (chart / "values.yaml").write_text(yaml.safe_dump(defaults))
            for name, admission in (
                ("disabled", None),
                ("serving", self.server_values()),
                ("enforced", self.enforced_values(podsEnabled=True, validationMode="strict")),
            ):
                with self.subTest(mode=name):
                    self.assertEqual(self.render(admission, chart=chart), self.render(admission))
        # A null value removes the key as well.
        self.assertEqual(
            self.render(self.server_values(portalServiceAccount=None)), self.render(self.server_values()),
        )

    def test_platform_without_the_portal_service_account_value_requires_a_listed_portal(self):
        # An upgrade with --reuse-values from an older release does not apply the
        # platform's default, so the value is empty or, as here, absent: nothing
        # is added, enforced admission still needs the Portal's service account
        # listed, and a listed one stays listed once.
        namespace = "synthetic-release-namespace"
        portal = f"system:serviceaccount:{namespace}:cetirun-portal"
        with tempfile.TemporaryDirectory(prefix="platform-reused-values-") as directory:
            base = Path(directory)
            for relative in (
                "cetirun-platform", "cetirun-operator/chart", "cetirun-portal/chart",
                "cetirun-ai-gateway/chart", "cetirun-connections-mcp/chart", "cetirun-observability/chart",
            ):
                shutil.copytree(ROOT / "app" / relative, base / relative, ignore=shutil.ignore_patterns("charts"))
            for relative, keys in (
                ("cetirun-platform", ("cetirun-operator", "sandboxAdmission")),
                ("cetirun-operator/chart", ("sandboxAdmission",)),
            ):
                defaults = yaml.safe_load((base / relative / "values.yaml").read_text())
                section = defaults
                for key in keys:
                    section = section[key]
                section.pop("portalServiceAccount")
                (base / relative / "values.yaml").write_text(yaml.safe_dump(defaults))
            chart = base / "cetirun-platform"
            subprocess.run(
                ["helm", "dependency", "build", "--skip-refresh", str(chart)],
                cwd=ROOT, env=OFFLINE_ENV, text=True, capture_output=True, check=True, timeout=30,
            )
            values = {
                "istio": {"enabled": True},
                "cetirun-portal": {"istio": {"host": "portal.example.com"}, "aiGateway": {"catalogNamespace": namespace}},
            }
            operator = {"namespace": namespace, "domainSuffix": "platform.example.com"}
            listed = self.helm(chart=chart, values={**values, "cetirun-operator": {
                **operator, "sandboxAdmission": self.enforced_values(adminUsers=["admin@example.com", portal]),
            }})
            self.assertEqual(self.platform_admission_config(listed)["adminUsers"], ["admin@example.com", portal])
            unlisted = self.helm(chart=chart, values={**values, "cetirun-operator": {
                **operator, "sandboxAdmission": self.enforced_values(),
            }})
            self.assertNotEqual(unlisted.returncode, 0, unlisted.stdout)
            self.assertIn(f"cetirun-operator.sandboxAdmission.adminUsers must include {portal}", unlisted.stderr)

    def test_kubernetes_sources_share_platform_config_and_grant_only_policy_writes(self):
        admission = self.enforced_values(podsEnabled=True)
        for namespace, config_map in (
            ("cetirun-system", "sandbox-platform"),
            ("synthetic-platform", "alternate-sandbox-platform"),
        ):
            with self.subTest(namespace=namespace, config_map=config_map):
                values = {"namespace": namespace}
                baseline = self.render(admission, values=values)
                sources = {
                    "enabled": True, "platformConfigMapName": config_map,
                    "homeClaimName": "owned-home", "skillsClaimName": "owned-skills",
                    "bootstrapOwnedPrerequisites": True,
                    "helpers": {"doctor": True, "logger": True, "desktop": False},
                }
                configured = self.render(admission, values={**values, "sandboxSources": sources})
                controller = configured[("Deployment", "cetirun-operator")]
                self.assertEqual(controller["metadata"]["namespace"], namespace)
                env = controller["spec"]["template"]["spec"]["containers"][0]["env"]
                self.assertEqual(next(item for item in env if item["name"] == "OPERATOR_NAMESPACE"), {
                    "name": "OPERATOR_NAMESPACE",
                    "valueFrom": {"fieldRef": {"fieldPath": "metadata.namespace"}},
                })
                source_env = [item for item in env if item["name"] == "CETIRUN_SANDBOX_SOURCES"]
                self.assertEqual(len(source_env), 1)
                self.assertEqual(json.loads(source_env[0]["value"]), {
                    **sources, "admissionServiceAccount": NAME,
                })
                container = configured[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
                config_env = next(item for item in container["env"] if item["name"] == "CETIRUN_SANDBOX_ADMISSION")
                baseline_container = baseline[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
                baseline_config_env = next(
                    item for item in baseline_container["env"] if item["name"] == "CETIRUN_SANDBOX_ADMISSION"
                )
                self.assertEqual(json.loads(config_env["value"]), {
                    **json.loads(baseline_config_env["value"]),
                    "platformNamespace": namespace,
                    "platformConfigMapName": config_map,
                })
                self.assertIn(f"--admission-pod-state-namespace={namespace}", container["args"])
                state_role = configured[("Role", f"{NAME}-pod-state")]
                self.assertEqual(state_role["metadata"]["namespace"], namespace)
                self.assertEqual(state_role["rules"], [{
                    "apiGroups": [""], "resources": ["configmaps"], "verbs": ["get"],
                }])
                binding = configured[("RoleBinding", f"{NAME}-pod-state")]
                self.assertEqual(binding["metadata"]["namespace"], namespace)
                self.assertEqual(binding["roleRef"], {
                    "apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": f"{NAME}-pod-state",
                })
                self.assertEqual(binding["subjects"], [{
                    "kind": "ServiceAccount", "name": NAME, "namespace": namespace,
                }])
                policy_rule = {
                    "apiGroups": ["cilium.io"],
                    "resources": ["ciliumnetworkpolicies"],
                    "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"],
                }
                role_key = ("ClusterRole", "cetirun-operator-manager-role")
                self.assertFalse(any("cilium.io" in rule["apiGroups"] for rule in baseline[role_key]["rules"]))
                rules = configured[role_key]["rules"]
                self.assertEqual(rules.count(policy_rule), 1)
                rules.remove(policy_rule)
                gateway_rule = {
                    "apiGroups": ["networking.istio.io"],
                    "resources": ["gateways"],
                    "verbs": ["get"],
                }
                self.assertEqual(rules.count(gateway_rule), 1)
                self.assertFalse(any("gateways" in rule["resources"] for rule in baseline[role_key]["rules"]))
                rules.remove(gateway_rule)
                env.remove(source_env[0])
                config_env["value"] = baseline_config_env["value"]
                self.assertEqual(configured, baseline)

    def test_kubernetes_sources_placement_adds_only_controller_selection_and_node_get(self):
        admission = self.enforced_values(podsEnabled=True, placementConfigMapName="sandbox-placement")
        sources = {"enabled": True, "platformConfigMapName": "sandbox-platform"}
        baseline = self.render(admission, values={"sandboxSources": sources})
        configured = self.render(admission, values={"sandboxSources": {
            **sources, "placementConfigMapName": "sandbox-placement",
        }})

        def source_environment(documents):
            container = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]
            return next(item for item in container["env"] if item["name"] == "CETIRUN_SANDBOX_SOURCES")

        before, after = source_environment(baseline), source_environment(configured)
        self.assertNotIn("placementConfigMapName", json.loads(before["value"]))
        self.assertEqual(json.loads(after["value"]), {
            **json.loads(before["value"]), "placementConfigMapName": "sandbox-placement",
        })
        node_rule = {"apiGroups": [""], "resources": ["nodes"], "verbs": ["get"]}
        rules = configured[("ClusterRole", "cetirun-operator-manager-role")]["rules"]
        self.assertEqual(rules.count(node_rule), 1)
        self.assertEqual(baseline[("ClusterRole", "cetirun-operator-manager-role")]["rules"].count(node_rule), 1)
        after["value"] = before["value"]
        self.assertEqual(configured, baseline)

    def test_kubernetes_sources_placement_requires_same_protected_registry(self):
        for name in (False, 42, {}, [], "Invalid", "bad/name", "x" * 254):
            with self.subTest(name=name):
                self.assert_rejected(
                    self.enforced_values(podsEnabled=True, placementConfigMapName="sandbox-placement"),
                    "sandboxSources.placementConfigMapName",
                    values={"sandboxSources": {"enabled": True, "platformConfigMapName": "sandbox-platform",
                                              "placementConfigMapName": name}},
                )
        for admission_name, selected, enabled in (
            ("", "sandbox-placement", True),
            ("another-registry", "sandbox-placement", True),
            ("sandbox-placement", "sandbox-placement", False),
            ("sandbox-platform", "sandbox-platform", True),
        ):
            with self.subTest(admission_name=admission_name, selected=selected, enabled=enabled):
                self.assert_rejected(
                    self.enforced_values(podsEnabled=True, placementConfigMapName=admission_name),
                    "sandboxSources.placementConfigMapName",
                    values={"sandboxSources": {"enabled": enabled, "platformConfigMapName": "sandbox-platform",
                                              "placementConfigMapName": selected}},
                )

    def test_kubernetes_sources_skills_catalog_adds_only_controller_setting_and_pv_get(self):
        admission = self.enforced_values(podsEnabled=True)
        sources = {
            "enabled": True, "bootstrapOwnedPrerequisites": True,
            "platformConfigMapName": "sandbox-platform",
        }
        baseline = self.render(admission, values={"sandboxSources": sources})
        configured = self.render(admission, values={"sandboxSources": {
            **sources, "skillsCatalogConfigMapName": "published-skills",
        }})

        def source_environment(documents):
            container = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]
            return next(item for item in container["env"] if item["name"] == "CETIRUN_SANDBOX_SOURCES")

        before, after = source_environment(baseline), source_environment(configured)
        self.assertNotIn("skillsCatalogConfigMapName", json.loads(before["value"]))
        self.assertEqual(json.loads(after["value"]), {
            **json.loads(before["value"]), "skillsCatalogConfigMapName": "published-skills",
        })
        pv_rule = {"apiGroups": [""], "resources": ["persistentvolumes"], "verbs": ["get"]}
        rules = configured[("ClusterRole", NAME)]["rules"]
        self.assertEqual(rules.count(pv_rule), 1)
        rules.remove(pv_rule)
        after["value"] = before["value"]
        self.assertEqual(configured, baseline)

    def test_kubernetes_sources_skills_catalog_rejects_invalid_selection(self):
        for changes, message in (
            ({"enabled": False, "bootstrapOwnedPrerequisites": False}, "requires enabled owned bootstrap"),
            ({"bootstrapOwnedPrerequisites": False}, "requires enabled owned bootstrap"),
            ({"skillsCatalogConfigMapName": True}, "must be a resource name"),
            ({"skillsCatalogConfigMapName": "other/catalog"}, "must be a separate valid ConfigMap name"),
            ({"skillsCatalogConfigMapName": "sandbox-platform"}, "must be a separate valid ConfigMap name"),
        ):
            with self.subTest(changes=changes):
                self.assert_rejected(
                    self.enforced_values(podsEnabled=True), message,
                    values={"sandboxSources": {
                        "enabled": True, "bootstrapOwnedPrerequisites": True,
                        "platformConfigMapName": "sandbox-platform",
                        "skillsCatalogConfigMapName": "published-skills", **changes,
                    }},
                )

    def test_kubernetes_sources_omit_both_admission_platform_fields_when_disabled(self):
        admission = self.enforced_values(podsEnabled=True)
        baseline = self.render(admission)
        container = baseline[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
        config = json.loads(container["env"][0]["value"])
        self.assertNotIn("platformNamespace", config)
        self.assertNotIn("platformConfigMapName", config)
        controller_env = baseline[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]["env"]
        self.assertNotIn("CETIRUN_SANDBOX_SOURCES", [item["name"] for item in controller_env])
        for config_map in ("", "unused-sandbox-platform"):
            with self.subTest(config_map=config_map):
                disabled = self.render(admission, values={"sandboxSources": {
                    "enabled": False, "platformConfigMapName": config_map,
                }})
                self.assertEqual(disabled, baseline)

    def test_kubernetes_sources_bootstrap_defaults_to_false(self):
        documents = self.render(self.enforced_values(podsEnabled=True), values={
            "sandboxSources": {
                "enabled": True, "platformConfigMapName": "sandbox-platform",
            },
        })
        env = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]["env"]
        source_env = next(item for item in env if item["name"] == "CETIRUN_SANDBOX_SOURCES")
        config = json.loads(source_env["value"])
        self.assertNotIn("bootstrapOwnedPrerequisites", config)
        self.assertEqual(set(config), {
            "enabled", "platformConfigMapName", "homeClaimName", "skillsClaimName",
            "helpers", "admissionServiceAccount",
        })

    def test_native_execution_retains_migration_readers_and_cleanup_patch(self):
        admission = self.enforced_values(podsEnabled=True)
        sources = {
            "enabled": True, "platformConfigMapName": "sandbox-platform",
            "bootstrapOwnedPrerequisites": True,
        }
        values = {"sandboxSources": sources, "aiGateway": {"enabled": True}}
        baseline = self.render(admission, values=values)
        configured = self.render(admission, values={
            **values, "sandboxSources": {**sources, "nativeExecution": True},
        })
        env = configured[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]["env"]
        config = json.loads(next(item["value"] for item in env if item["name"] == "CETIRUN_SANDBOX_SOURCES"))
        self.assertIs(config["nativeExecution"], True)
        admission_env = configured[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]["env"]
        admission_config = json.loads(next(item["value"] for item in admission_env if item["name"] == "CETIRUN_SANDBOX_ADMISSION"))
        self.assertIs(admission_config["nativeExecution"], True)
        baseline_env = baseline[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]["env"]
        baseline_config = json.loads(next(item["value"] for item in baseline_env if item["name"] == "CETIRUN_SANDBOX_ADMISSION"))
        self.assertNotIn("nativeExecution", baseline_config)
        role = ("ClusterRole", "cetirun-operator-manager-role")
        added = [rule for rule in configured[role]["rules"] if rule not in baseline[role]["rules"]]
        self.assertEqual(added, [{
            "apiGroups": ["networking.istio.io"], "resources": ["gateways"],
            "verbs": ["get", "list", "watch"],
        }])
        self.assertIn({"apiGroups": [""], "resources": ["nodes"], "verbs": ["get"]}, baseline[role]["rules"])
        removed = [rule for rule in baseline[role]["rules"] if rule not in configured[role]["rules"]]
        self.assertEqual(removed, [{
            "apiGroups": ["networking.istio.io"], "resources": ["gateways"],
            "verbs": ["get"],
        }])
        for documents in (baseline, configured):
            self.assertEqual([
                rule for rule in documents[role]["rules"]
                if any(group == "networking.istio.io" for group in rule["apiGroups"]) and "destinationrules" in rule["resources"]
            ], [{
                "apiGroups": ["networking.istio.io"],
                "resources": ["virtualservices", "sidecars", "destinationrules"],
                "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"],
            }])

    def test_native_finalization_permission_survives_disabling_starts(self):
        admission = self.enforced_values(podsEnabled=True)
        for sources in (
            {"enabled": True, "nativeExecution": True, "bootstrapOwnedPrerequisites": True,
             "platformConfigMapName": "sandbox-platform"},
            {"enabled": True, "nativeExecution": False, "bootstrapOwnedPrerequisites": True,
             "platformConfigMapName": "sandbox-platform"},
            {"enabled": True, "nativeExecution": False, "bootstrapOwnedPrerequisites": True,
             "platformConfigMapName": "sandbox-platform", "skillsClaimName": ""},
            {"enabled": False, "nativeExecution": False},
        ):
            with self.subTest(sources=sources):
                documents = self.render(admission, values={
                    "sandboxSources": sources, "aiGateway": {"enabled": True},
                })
                rules = documents[("ClusterRole", "cetirun-operator-manager-role")]["rules"]
                self.assertIn({
                    "apiGroups": [""], "resources": ["pods"],
                    "verbs": ["get", "list", "delete", "patch"],
                }, rules)
                self.assertEqual([
                    rule for rule in rules
                    if "apps" in rule["apiGroups"] and "replicasets" in rule["resources"]
                ], [{
                    "apiGroups": ["apps"], "resources": ["replicasets"],
                    "verbs": ["get", "list"],
                }])
                self.assertIn({
                    "apiGroups": ["apps"], "resources": ["controllerrevisions"],
                    "verbs": ["get"],
                }, rules)

    def test_native_execution_needs_no_secondary_reconciliation_option(self):
        admission = self.enforced_values(podsEnabled=True)
        sources = {
            "enabled": True, "nativeExecution": True,
            "bootstrapOwnedPrerequisites": True, "platformConfigMapName": "sandbox-platform",
        }
        values = {"sandboxSources": sources, "aiGateway": {"enabled": True}}
        documents = self.render(admission, values=values)
        container = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]
        configured = json.loads(next(item["value"] for item in container["env"] if item["name"] == "CETIRUN_SANDBOX_SOURCES"))
        self.assertTrue(configured["nativeExecution"])
        self.assertNotIn("reconciledExecution", configured)

    def test_removed_reconciled_execution_option_is_rejected(self):
        for enabled in (False, True):
            for setting in (False, True, "true", 1):
                with self.subTest(enabled=enabled, setting=setting):
                    sources = {
                        "enabled": enabled, "nativeExecution": enabled, "reconciledExecution": setting,
                        "bootstrapOwnedPrerequisites": enabled, "platformConfigMapName": "sandbox-platform",
                    }
                    self.assert_rejected(
                        self.enforced_values(podsEnabled=True), "sandboxSources.reconciledExecution is no longer supported",
                        values={"sandboxSources": sources, "aiGateway": {"enabled": True}},
                    )

    def test_portal_controller_defaults_require_the_common_executor(self):
        temporary = tempfile.TemporaryDirectory(prefix="sandbox-platform-chart-")
        self.addCleanup(temporary.cleanup)
        base = Path(temporary.name)
        for relative in (
            "cetirun-platform", "cetirun-operator/chart", "cetirun-portal/chart",
            "cetirun-ai-gateway/chart", "cetirun-connections-mcp/chart", "cetirun-observability/chart",
        ):
            shutil.copytree(ROOT / "app" / relative, base / relative, ignore=shutil.ignore_patterns("charts"))
        chart = base / "cetirun-platform"
        subprocess.run(
            ["helm", "dependency", "build", "--skip-refresh", str(chart)],
            cwd=ROOT, env=OFFLINE_ENV, text=True, capture_output=True, check=True, timeout=30,
        )
        namespace = "synthetic-release-namespace"
        sources = {
            "enabled": True, "nativeExecution": True,
            "bootstrapOwnedPrerequisites": True, "platformConfigMapName": "sandbox-platform",
        }
        operator = {
            "enabled": True, "namespace": namespace, "domainSuffix": "platform.example.com",
            "sandboxSources": sources, "sandboxAdmission": self.enforced_values(podsEnabled=True),
        }
        portal = {
            "istio": {"host": "portal.example.com"},
            "aiGateway": {"catalogNamespace": namespace},
            "sandboxStorage": {"platformNamespace": namespace, "platformConfigMapName": "sandbox-platform"},
        }
        values = {"istio": {"enabled": True}, "cetirun-operator": operator, "cetirun-portal": portal}
        for native in (False, True):
            for controller_defaults in (False, True):
                with self.subTest(native=native, controller_defaults=controller_defaults):
                    result = self.helm(chart=chart, values={
                        **values,
                        "cetirun-operator": {**operator, "sandboxSources": {**sources, "nativeExecution": native}},
                        "cetirun-portal": {**portal, "sandboxRuntimeDefaultsFromController": controller_defaults},
                    })
                    if controller_defaults and not native:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn("sandboxRuntimeDefaultsFromController requires", result.stderr)
                    else:
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(
                            "name: SANDBOX_RUNTIME_DEFAULTS_FROM_CONTROLLER" in result.stdout,
                            controller_defaults,
                        )
                        # The platform chart adds the Portal's service account to
                        # the admission administrators, once and after the others.
                        self.assertEqual(self.platform_admission_config(result)["adminUsers"], [
                            "admin@example.com", f"system:serviceaccount:{namespace}:cetirun-portal",
                        ])
        disabled = self.helm(chart=chart, values={
            **values,
            "cetirun-operator": {"enabled": False},
            "cetirun-portal": {**portal, "sandboxRuntimeDefaultsFromController": True},
        })
        self.assertNotEqual(disabled.returncode, 0)
        self.assertIn("sandboxRuntimeDefaultsFromController requires", disabled.stderr)

    def test_native_empty_skills_claim_name_is_optional_without_extra_grants(self):
        admission = self.enforced_values(podsEnabled=True)
        sources = {
            "enabled": True, "nativeExecution": True,
            "bootstrapOwnedPrerequisites": True, "platformConfigMapName": "sandbox-platform",
        }
        values = {"sandboxSources": sources, "aiGateway": {"enabled": True}}
        baseline = self.render(admission, values=values)
        empty = self.render(admission, values={
            **values, "sandboxSources": {**sources, "skillsClaimName": ""},
        })
        self.assertEqual(set(empty), set(baseline))
        for key in empty:
            if key[0] in ("Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "ServiceAccount"):
                self.assertEqual(empty[key], baseline[key])
        env = empty[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]["env"]
        config = json.loads(next(item["value"] for item in env if item["name"] == "CETIRUN_SANDBOX_SOURCES"))
        self.assertEqual(config["skillsClaimName"], "")
        self.assertNotIn("skillsCatalogConfigMapName", config)
        for change in (
            {"skillsCatalogConfigMapName": "skills-catalog"},
            {"skillsClaimName": "INVALID"}, {"skillsClaimName": 42},
        ):
            with self.subTest(change=change):
                self.assert_rejected(admission, "skillsClaimName", values={
                    **values, "sandboxSources": {**sources, "skillsClaimName": "", **change},
                })

    def test_native_execution_rejects_incomplete_configuration(self):
        sources = {
            "enabled": True, "nativeExecution": True,
            "bootstrapOwnedPrerequisites": True, "platformConfigMapName": "sandbox-platform",
        }
        for change, gateway in (
            ({"enabled": False, "bootstrapOwnedPrerequisites": False}, True),
            ({"bootstrapOwnedPrerequisites": False}, True),
            ({}, False),
            ({"helpers": {"desktop": True}}, True),
            ({"nativeExecution": "true"}, True),
            ({"placementConfigMapName": "sandbox-placement"}, True),
        ):
            with self.subTest(change=change, gateway=gateway):
                self.assert_rejected(
                    self.enforced_values(podsEnabled=True), "sandboxSources.nativeExecution",
                    values={"sandboxSources": {**sources, **change}, "aiGateway": {"enabled": gateway}},
                )

    def test_kubernetes_sources_accept_values_without_the_new_bootstrap_key(self):
        for enabled in (False, True):
            with self.subTest(enabled=enabled):
                documents = self.render(self.enforced_values(podsEnabled=True), values={
                    "sandboxSources": {
                        "enabled": enabled,
                        "platformConfigMapName": "sandbox-platform",
                        "bootstrapOwnedPrerequisites": None,
                    },
                })
                env = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]["env"]
                configs = [item["value"] for item in env if item["name"] == "CETIRUN_SANDBOX_SOURCES"]
                if enabled:
                    self.assertEqual(len(configs), 1)
                    self.assertNotIn("bootstrapOwnedPrerequisites", json.loads(configs[0]))
                else:
                    self.assertEqual(configs, [])

    def test_kubernetes_sources_bootstrap_rejects_invalid_or_disabled_configuration(self):
        for invalid in ("true", 1, "false", []):
            with self.subTest(invalid=invalid):
                self.assert_rejected(
                    self.enforced_values(podsEnabled=True),
                    "sandboxSources.bootstrapOwnedPrerequisites must be a boolean",
                    values={"sandboxSources": {
                        "enabled": True, "platformConfigMapName": "sandbox-platform",
                        "bootstrapOwnedPrerequisites": invalid,
                    }},
                )
        self.assert_rejected(
            self.enforced_values(podsEnabled=True),
            "sandboxSources.bootstrapOwnedPrerequisites requires enabled sources",
            values={"sandboxSources": {"enabled": False, "bootstrapOwnedPrerequisites": True}},
        )

    def test_kubernetes_sources_require_enforced_pod_admission(self):
        values = {"sandboxSources": {
            "enabled": True, "platformConfigMapName": "sandbox-platform",
        }}
        for admission in (None, self.server_values(), self.enforced_values(),
                          self.server_values(podsEnabled=True)):
            with self.subTest(admission=admission):
                self.assert_rejected(
                    admission,
                    "sandboxSources.enabled requires enforced Sandbox configuration and Pod admission",
                    values=values,
                )

    def test_kubernetes_sources_validate_each_deployment_template_in_isolation(self):
        for template, deployment in (("deployment.yaml", "cetirun-operator"), ("sandbox-admission.yaml", NAME)):
            with self.subTest(template=template), tempfile.TemporaryDirectory() as directory:
                chart = Path(directory)
                (chart / "templates").mkdir()
                for relative in ("Chart.yaml", "values.yaml", "templates/_helpers.tpl", "templates/_cetiadmin.tpl", f"templates/{template}"):
                    shutil.copyfile(CHART / relative, chart / relative)
                values = {"sandboxSources": {
                    "enabled": True, "platformConfigMapName": "sandbox-platform",
                }}
                documents = self.render(self.enforced_values(podsEnabled=True), values=values, chart=chart)
                self.assertIn(("Deployment", deployment), documents)
                for field in ("enabled", "enforce", "podsEnabled"):
                    with self.subTest(disabled_admission_field=field):
                        admission = self.enforced_values(podsEnabled=True)
                        admission[field] = False
                        self.assert_rejected(
                            admission,
                            "sandboxSources.enabled requires enforced Sandbox configuration and Pod admission",
                            values=values, chart=chart,
                        )
                self.assert_rejected(
                    self.enforced_values(podsEnabled=True),
                    "platformConfigMapName must be a valid resource name",
                    values={"sandboxSources": {"enabled": True}}, chart=chart,
                )

    def test_kubernetes_sources_reject_invalid_configuration(self):
        cases = [
            ({"enabled": "true"}, "sandboxSources.enabled must be a boolean"),
            ({"platformConfigMapName": ""}, "platformConfigMapName must be a valid resource name"),
            ({"platformConfigMapName": "bad/name"}, "platformConfigMapName must be a valid resource name"),
            ({"platformConfigMapName": 42}, "platformConfigMapName must be a resource name"),
            ({"platformNamespace": "other-platform"}, "unsupported sandboxSources field platformNamespace"),
            ({"homeClaimName": "bad/name"}, "homeClaimName must be a valid resource name"),
            ({"skillsClaimName": "UPPER"}, "skillsClaimName must be a valid resource name"),
            ({"skillsClaimName": 42}, "skillsClaimName must be a resource name"),
            ({"homeClaimName": "skills"}, "HOME and skills must use distinct claims"),
            ({"allowExecution": True}, "unsupported sandboxSources field allowExecution"),
            ({"admissionServiceAccount": "custom"}, "unsupported sandboxSources field admissionServiceAccount"),
            ({"helpers": {"desktop": "true"}}, "helpers.desktop must be a boolean"),
            ({"helpers": {"custom": True}}, "unsupported sandboxSources.helpers field custom"),
        ]
        for overrides, message in cases:
            with self.subTest(overrides=overrides):
                values = {"sandboxSources": {
                    "enabled": True, "platformConfigMapName": "sandbox-platform",
                    **overrides,
                }}
                self.assert_rejected(self.enforced_values(podsEnabled=True), message, values=values)

    def test_read_only_rbac_uses_dedicated_service_account(self):
        documents = self.render(self.server_values())
        account = documents[("ServiceAccount", NAME)]
        self.assertNotIn("annotations", account["metadata"])
        self.assertEqual(account["metadata"]["namespace"], "cetirun-system")
        role = documents[("ClusterRole", NAME)]
        self.assertEqual(role["rules"], [{
            "apiGroups": [API_GROUP], "resources": RESOURCES, "verbs": ["get", "list"],
        }])
        binding = documents[("ClusterRoleBinding", NAME)]
        self.assertEqual(binding["roleRef"], {
            "apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": NAME,
        })
        self.assertEqual(binding["subjects"], [{
            "kind": "ServiceAccount", "name": NAME, "namespace": "cetirun-system",
        }])
        for identity, document in documents.items():
            if identity[0] == "ClusterRoleBinding" and identity[1] != NAME:
                self.assertNotIn(NAME, [subject["name"] for subject in document["subjects"]])

    def test_operator_can_observe_configuration_and_publish_only_status(self):
        role = self.render()[("ClusterRole", "cetirun-operator-manager-role")]
        for resource in ("sandboxes", "agentpolicies"):
            rules = [
                rule for rule in role["rules"]
                if API_GROUP in rule["apiGroups"] and resource in rule["resources"]
            ]
            self.assertEqual(len(rules), 1)
            self.assertEqual(rules[0]["verbs"], ["get", "list", "watch"])
            status_rules = [
                rule for rule in role["rules"]
                if API_GROUP in rule["apiGroups"] and f"{resource}/status" in rule["resources"]
            ]
            self.assertEqual(len(status_rules), 1)
            self.assertEqual(status_rules[0]["verbs"], ["get", "update", "patch"])
        for rule in role["rules"]:
            self.assertIsInstance(rule["apiGroups"], list)
            self.assertIsInstance(rule["resources"], list)
        clusterrole_rules = [
            rule for rule in role["rules"]
            if any(group == "rbac.authorization.k8s.io" for group in rule["apiGroups"])
            and "clusterroles" in rule["resources"]
        ]
        self.assertEqual(clusterrole_rules, [{
            "apiGroups": ["rbac.authorization.k8s.io"],
            "resources": ["clusterroles"], "verbs": ["get"],
        }, {
            "apiGroups": ["rbac.authorization.k8s.io"],
            "resources": ["clusterroles", "clusterrolebindings"],
            "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"],
        }, {
            "apiGroups": ["rbac.authorization.k8s.io"],
            "resources": ["clusterroles"],
            "resourceNames": ["cetirun-terminal-exec"], "verbs": ["bind"],
        }])

    def test_deployment_contract_tls_and_security_contexts(self):
        documents = self.render(self.server_values())
        deployment = documents[("Deployment", NAME)]
        pod = deployment["spec"]["template"]["spec"]
        self.assertEqual(pod["serviceAccountName"], NAME)
        self.assertTrue(pod["automountServiceAccountToken"])
        self.assertFalse(pod["enableServiceLinks"])
        self.assertEqual(pod["terminationGracePeriodSeconds"], 50)
        self.assertEqual(len(pod["containers"]), 1)
        self.assertNotIn("initContainers", pod)
        self.assertNotIn("hostNetwork", pod)
        self.assertNotIn("hostPID", pod)
        container = pod["containers"][0]
        self.assertEqual(container["lifecycle"], {"preStop": {"sleep": {"seconds": 10}}})
        controller = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["image"], controller["image"])
        self.assertEqual(container["args"], [
            "--mode=sandbox-admission",
            "--admission-cert-dir=/var/run/cetirun-admission",
            "--admission-port=9443",
            "--health-probe-bind-address=:8081",
            "--metrics-bind-address=:8080",
            "--leader-elect=false",
        ])
        self.assertEqual([entry["name"] for entry in container["env"]], ["CETIRUN_SANDBOX_ADMISSION"])
        self.assertEqual(json.loads(container["env"][0]["value"]), {
            "adminUsers": ["admin@example.com"],
            "adminGroups": [],
            "controllerUsername": "system:serviceaccount:cetirun-system:cetirun-operator-controller-manager",
            "validationMode": "compatible",
        })
        self.assertNotIn("envFrom", container)
        self.assertEqual(container["volumeMounts"], [{
            "name": "admission-tls", "mountPath": "/var/run/cetirun-admission", "readOnly": True,
        }])
        self.assertEqual(pod["volumes"], [{
            "name": "admission-tls",
            "secret": {
                "secretName": "synthetic-admission-tls",
                "defaultMode": 0o440,
                "items": [{"key": name, "path": name} for name in ("tls.crt", "tls.key")],
            },
        }])
        self.assertEqual(pod["securityContext"], {
            "runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
            "fsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"},
        })
        self.assertEqual(container["securityContext"], {
            "allowPrivilegeEscalation": False,
            "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]},
        })
        self.assertEqual(container["resources"], {
            "requests": {"cpu": "100m", "memory": "128Mi"},
            "limits": {"cpu": "500m", "memory": "256Mi"},
        })
        self.assertEqual(
            {port["name"]: port["containerPort"] for port in container["ports"]},
            {"admission": 9443, "health": 8081, "metrics": 8080},
        )
        for probe, path in (("livenessProbe", "/healthz"), ("readinessProbe", "/readyz")):
            self.assertEqual(container[probe]["httpGet"], {"path": path, "port": "health"})
        service = documents[("Service", NAME)]["spec"]
        self.assertEqual(service["type"], "ClusterIP")
        self.assertEqual(service["selector"], deployment["spec"]["selector"]["matchLabels"])
        self.assertEqual(service["ports"], [{
            "name": "admission", "port": 443, "targetPort": "admission", "protocol": "TCP",
        }])

    def test_high_availability_selectors_and_rollouts(self):
        for replicas in (2, 3, 5):
            with self.subTest(replicas=replicas):
                documents = self.render(self.server_values(replicaCount=replicas))
                deployment = documents[("Deployment", NAME)]["spec"]
                self.assertEqual(deployment["replicas"], replicas)
                self.assertEqual(deployment["strategy"], {
                    "type": "RollingUpdate", "rollingUpdate": {"maxUnavailable": 0, "maxSurge": 1},
                })
                selector = deployment["selector"]
                self.assertEqual(selector, {"matchLabels": {"app.kubernetes.io/name": NAME}})
                for key, value in selector["matchLabels"].items():
                    self.assertEqual(deployment["template"]["metadata"]["labels"][key], value)
                pdb = documents[("PodDisruptionBudget", NAME)]["spec"]
                self.assertEqual(pdb, {"minAvailable": replicas - 1, "selector": selector})
                spreads = deployment["template"]["spec"]["topologySpreadConstraints"]
                self.assertEqual(
                    {spread["topologyKey"] for spread in spreads},
                    {"kubernetes.io/hostname", "topology.kubernetes.io/zone"},
                )
                for spread in spreads:
                    self.assertEqual(spread["maxSkew"], 1)
                    self.assertEqual(spread["labelSelector"], selector)
                    self.assertEqual(spread.get("matchLabelKeys"), ["pod-template-hash"])
                    # Without minDomains, a single eligible hostname is not
                    # inherently unsatisfiable: replicas can share that domain.
                    self.assertNotIn("minDomains", spread)
                self.assertEqual(spreads[0]["whenUnsatisfiable"], "DoNotSchedule")
                self.assertEqual(spreads[1]["whenUnsatisfiable"], "ScheduleAnyway")

    def test_priority_class_is_optional_and_never_created(self):
        for priority in (None, "", "synthetic-admission-critical"):
            with self.subTest(priority=priority):
                overrides = {} if priority is None else {"priorityClassName": priority}
                documents = self.render(self.server_values(**overrides))
                pod = documents[("Deployment", NAME)]["spec"]["template"]["spec"]
                if priority:
                    self.assertEqual(pod["priorityClassName"], priority)
                else:
                    self.assertNotIn("priorityClassName", pod)
                self.assertNotIn("PriorityClass", {kind for kind, _ in documents})
                controller = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]
                self.assertNotIn("priorityClassName", controller)

    def test_admission_placement_does_not_inherit_controller_values(self):
        placement = {
            "nodeSelector": {"example.com/pool": "controller"},
            "tolerations": [{"key": "example.com/controller", "operator": "Exists"}],
            "affinity": {
                "podAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": [{
                    "labelSelector": {"matchLabels": {"app.kubernetes.io/name": "cetirun-operator"}},
                    "topologyKey": "kubernetes.io/hostname",
                }]},
            },
        }
        for overrides in ({}, {"nodeSelector": {}, "affinity": {}, "tolerations": []}):
            with self.subTest(overrides=overrides):
                documents = self.render(self.server_values(**overrides), values=placement)
                pod = documents[("Deployment", NAME)]["spec"]["template"]["spec"]
                controller = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]
                for field, value in placement.items():
                    self.assertNotIn(field, pod)
                    self.assertEqual(controller[field], value)

    def test_placement_and_image_overrides_do_not_import_controller_credentials(self):
        values = {
            "namespace": "synthetic-platform",
            "serviceAccount": {
                "name": "synthetic-controller", "create": False,
                "annotations": {"example.com/controller-only": "true"},
            },
            "image": {
                "repository": "registry.example.com/unused", "tag": "unused",
                "digest": "sha256:" + "1" * 64, "pullPolicy": "Always",
            },
            "global": {"registry": "registry.example.com/runtime", "version": "2.0.0"},
            "imagePullSecrets": [{"name": "synthetic-image-pull"}],
            "acrDockerSecret": "synthetic-controller-registry",
            "azure": {"clientId": "synthetic-controller-client", "tenantId": "synthetic-tenant"},
            "agentIdentity": {
                "enabled": True, "tenantId": "synthetic-tenant",
                "blueprintClientId": "synthetic-blueprint",
            },
            "customRuntime": {"enabled": True},
            "podAnnotations": {
                "azure.workload.identity/inject-proxy-sidecar": "true",
                "example.com/controller-only": "true",
            },
            "nodeSelector": {"kubernetes.io/os": "linux", "example.com/pool": "controller"},
            "tolerations": [{"key": "example.com/controller", "operator": "Exists"}],
            "affinity": {
                "nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
                    "nodeSelectorTerms": [{
                        "matchExpressions": [{
                            "key": "example.com/pool", "operator": "In", "values": ["controller"],
                        }],
                    }],
                }},
            },
        }
        resources = {
            "requests": {"cpu": "200m", "memory": "192Mi"},
            "limits": {"cpu": "1", "memory": "512Mi"},
        }
        placement = {
            "nodeSelector": {"kubernetes.io/os": "linux", "example.com/pool": "admission"},
            "tolerations": [{"key": "example.com/admission", "operator": "Exists"}],
            "affinity": {
                "nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
                    "nodeSelectorTerms": [{
                        "matchExpressions": [{
                            "key": "example.com/pool", "operator": "In", "values": ["admission"],
                        }],
                    }],
                }},
            },
        }
        documents = self.render(
            self.enforced_values(resources=resources, **placement), values=values,
        )
        deployment = documents[("Deployment", NAME)]
        self.assertEqual(deployment["metadata"]["namespace"], "synthetic-platform")
        pod = deployment["spec"]["template"]["spec"]
        self.assertEqual(pod["imagePullSecrets"], values["imagePullSecrets"])
        controller = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]
        for field, value in placement.items():
            self.assertEqual(pod[field], value)
            self.assertEqual(controller[field], values[field])
            self.assertNotEqual(pod[field], controller[field])
        self.assertEqual(pod["serviceAccountName"], NAME)
        metadata = deployment["spec"]["template"]["metadata"]
        self.assertNotIn("azure.workload.identity/use", metadata["labels"])
        self.assertEqual(metadata["annotations"], {"sidecar.istio.io/inject": "false", "adx-mon/scrape": "true",
                                                   "adx-mon/port": "8080", "adx-mon/path": "/metrics"})
        self.assertNotIn("annotations", documents[("ServiceAccount", NAME)]["metadata"])
        container = pod["containers"][0]
        self.assertEqual(container["image"], "registry.example.com/runtime/cetirun-operator:2.0.0@sha256:" + "1" * 64)
        self.assertEqual(container["imagePullPolicy"], "Always")
        self.assertEqual(container["resources"], resources)
        self.assertEqual([entry["name"] for entry in container["env"]], ["CETIRUN_SANDBOX_ADMISSION"])
        config = json.loads(container["env"][0]["value"])
        controller_username = "system:serviceaccount:synthetic-platform:synthetic-controller"
        self.assertEqual(config["controllerUsername"], controller_username)
        self.assertEqual([volume["secret"]["secretName"] for volume in pod["volumes"]], ["synthetic-admission-tls"])
        webhook = documents[("ValidatingWebhookConfiguration", NAME)]["webhooks"][0]
        self.assertIn(f'request.userInfo.username == "{controller_username}"', webhook["matchConditions"][0]["expression"])
        self.assertEqual(webhook["clientConfig"]["service"]["namespace"], "synthetic-platform")

    def test_admin_user_group_or_both_lists_are_serialized_exactly(self):
        for users, groups in (
            (["admin@example.com"], []),
            ([], ["sandbox-admins"]),
            (["admin@example.com", "second-admin@example.com"], ["sandbox-admins"]),
        ):
            with self.subTest(users=users, groups=groups):
                documents = self.render(self.server_values(adminUsers=users, adminGroups=groups))
                container = documents[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
                config = json.loads(container["env"][0]["value"])
                self.assertEqual(config["adminUsers"], users)
                self.assertEqual(config["adminGroups"], groups)

    def test_portal_service_account_is_added_once_after_configured_administrators(self):
        portal = "system:serviceaccount:synthetic-release-namespace:cetirun-portal"
        for admission, values, expected in (
            (self.server_values(), {}, ["admin@example.com", portal]),
            (self.enforced_values(podsEnabled=True), {}, ["admin@example.com", portal]),
            (self.server_values(adminUsers=[], adminGroups=["sandbox-admins"]), {}, [portal]),
            # The Portal runs in the release namespace, which can differ from the operator's.
            (self.server_values(), {"namespace": "synthetic-platform"}, ["admin@example.com", portal]),
            (self.server_values(adminUsers=[portal, "admin@example.com"]), {}, [portal, "admin@example.com"]),
            (self.server_values(adminUsers=["admin@example.com", portal]), {}, ["admin@example.com", portal]),
        ):
            with self.subTest(users=admission["adminUsers"], values=values, enforce=admission.get("enforce", False)):
                baseline = self.render(admission, values=values)
                configured = self.render({**admission, "portalServiceAccount": "cetirun-portal"}, values=values)
                previous_env = self.admission_environment(baseline[("Deployment", NAME)])
                env = self.admission_environment(configured[("Deployment", NAME)])
                config, previous = json.loads(env["value"]), json.loads(previous_env["value"])
                self.assertEqual(config.pop("adminUsers"), expected)
                self.assertEqual(previous.pop("adminUsers"), admission["adminUsers"])
                self.assertEqual(config, previous)
                if expected == admission["adminUsers"]:
                    # Releases that already list it keep the same configuration,
                    # so an upgrade does not roll the admission server.
                    self.assertEqual(env["value"], previous_env["value"])
                env["value"] = previous_env["value"]
                self.assertEqual(configured, baseline)
        # The platform chart sets it while admission stays disabled by default.
        self.assertEqual(self.render({"portalServiceAccount": "cetirun-portal"}), self.render())

    def test_portal_service_account_requires_a_service_account_name(self):
        for invalid in ("Portal", "bad/name", "bad_name", "-portal", "portal.", " cetirun-portal", "x" * 254):
            with self.subTest(invalid=invalid):
                self.assert_rejected(
                    self.server_values(portalServiceAccount=invalid),
                    "sandboxAdmission.portalServiceAccount must be a valid ServiceAccount name",
                )
        for invalid in (42, True, [], {}):
            with self.subTest(invalid=invalid):
                self.assert_rejected(
                    self.server_values(portalServiceAccount=invalid),
                    "sandboxAdmission.portalServiceAccount must be a string",
                )
        # It does not replace the administrator that operations without a Portal
        # page, such as reauthorization, need.
        self.assert_rejected(
            self.server_values(adminUsers=[], adminGroups=[], portalServiceAccount="cetirun-portal"),
            "at least one adminUsers or adminGroups entry",
        )

    def test_enforcing_webhooks_match_only_configuration_operations(self):
        documents = self.render(self.enforced_values())
        for kind, path in (
            ("ValidatingWebhookConfiguration", "/validate-sandbox-configuration"),
            ("MutatingWebhookConfiguration", "/mutate-sandbox-agent"),
        ):
            with self.subTest(kind=kind):
                configuration = documents[(kind, NAME)]
                self.assertEqual(configuration["apiVersion"], "admissionregistration.k8s.io/v1")
                self.assertNotIn("namespace", configuration["metadata"])
                self.assertEqual(len(configuration["webhooks"]), 1)
                webhook = configuration["webhooks"][0]
                self.assertEqual(webhook["admissionReviewVersions"], ["v1"])
                self.assertEqual(webhook["sideEffects"], "None")
                self.assertEqual(webhook["failurePolicy"], "Fail")
                self.assertEqual(webhook["timeoutSeconds"], 10)
                self.assertEqual(webhook["matchPolicy"], "Equivalent")
                self.assertNotIn("namespaceSelector", webhook)
                self.assertNotIn("objectSelector", webhook)
                self.assertEqual(webhook["clientConfig"], {
                    "caBundle": self.ca_bundle,
                    "service": {"name": NAME, "namespace": "cetirun-system", "path": path, "port": 443},
                })
                operations = set()
                for rule in webhook["rules"]:
                    self.assertEqual(rule["apiGroups"], [API_GROUP])
                    self.assertEqual(rule["apiVersions"], ["v1alpha1"])
                    self.assertEqual(rule["scope"], "Cluster")
                    operations.update(
                        (operation, resource)
                        for operation in rule["operations"] for resource in rule["resources"]
                    )
                expected = {("CREATE", "agents"), ("UPDATE", "agents")}
                if kind == "ValidatingWebhookConfiguration":
                    expected = {(operation, resource) for operation in ("CREATE", "UPDATE") for resource in RESOURCES}
                    expected.update(("DELETE", resource) for resource in RESOURCES if resource != "agents")
                else:
                    self.assertEqual(webhook["reinvocationPolicy"], "Never")
                    self.assertEqual(
                        webhook["matchConditions"],
                        documents[("ValidatingWebhookConfiguration", NAME)]["webhooks"][0]["matchConditions"],
                    )
                self.assertEqual(operations, expected)

    def test_trusted_team_assignments_are_serialized_without_inference(self):
        assignments = [
            {"team": "research", "users": ["member@example.com"], "groups": ["researchers"]},
            {"team": "operations", "groups": ["operators"]},
        ]
        documents = self.render(self.server_values(teamAssignments=assignments))
        container = documents[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
        config = json.loads(container["env"][0]["value"])
        self.assertEqual(config["teamAssignments"], assignments)
        for invalid in (
            "research", [{}], ["research"], [{"team": "research"}],
            [{"team": "Invalid", "users": ["member@example.com"]}],
            [{"team": "research", "users": "member@example.com"}],
            [{"team": "research", "users": [], "groups": ""}],
            [{"team": "research", "groups": ["system:authenticated"]}],
            [{"team": "research", "users": ["*"]}],
            [{"team": "research", "users": ["member@example.com"], "admin": True}],
        ):
            with self.subTest(invalid=invalid):
                self.assert_rejected(self.server_values(teamAssignments=invalid), "sandboxAdmission.teamAssignments")

    def test_pod_bootstrap_scopes_state_reads_without_installing_rules(self):
        documents = self.render(self.server_values(podsEnabled=True))
        self.assertIn({
            "apiGroups": ["rbac.authorization.k8s.io"],
            "resources": ["rolebindings", "clusterrolebindings"],
            "verbs": ["list"],
        }, documents[("ClusterRole", NAME)]["rules"])
        self.assertNotIn(("ValidatingWebhookConfiguration", "cetirun-sandbox-pods"), documents)
        self.assertNotIn(("ValidatingAdmissionPolicy", "cetirun-sandbox-namespace"), documents)
        container = documents[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
        self.assertIn("--admission-pod-state-namespace=cetirun-system", container["args"])
        role = documents[("Role", "cetirun-sandbox-admission-pod-state")]
        self.assertEqual(role["metadata"]["namespace"], "cetirun-system")
        self.assertEqual(role["rules"], [{
            "apiGroups": [""], "resources": ["configmaps"], "verbs": ["get"],
        }])
        for rule in documents[("ClusterRole", NAME)]["rules"]:
            self.assertTrue(set(rule["verbs"]) <= {"get", "list"})
            self.assertNotIn("secrets", rule["resources"])
            self.assertNotIn("configmaps", rule["resources"])
            self.assertNotIn("*", rule["resources"])
            self.assertNotIn("*", rule["apiGroups"])
        self.assertEqual([
            rule for rule in documents[("ClusterRole", NAME)]["rules"]
            if any(group == "networking.istio.io" for group in rule["apiGroups"])
        ], [{
            "apiGroups": ["networking.istio.io"],
            "resources": ["gateways", "destinationrules"], "verbs": ["get"],
        }])
        for api_group, resource in (("node.k8s.io", "runtimeclasses"), ("storage.k8s.io", "storageclasses")):
            self.assertIn({
                "apiGroups": [api_group], "resources": [resource], "verbs": ["get"],
            }, documents[("ClusterRole", NAME)]["rules"])
        self.assert_rejected({"podsEnabled": True}, "podsEnabled requires sandboxAdmission.enabled")
        self.assert_rejected(self.server_values(podsEnabled="true"), "podsEnabled must be a boolean")

    def test_planned_migration_is_opt_in_without_removing_cleanup_readers(self):
        admission = self.enforced_values(podsEnabled=True)
        sources = {
            "enabled": True, "platformConfigMapName": "sandbox-platform",
            "bootstrapOwnedPrerequisites": True, "nativeExecution": True,
        }
        values = {"sandboxSources": sources, "aiGateway": {"enabled": True}}
        baseline = self.render(admission, values=values)
        enabled = self.render(admission, values={
            **values, "sandboxSources": {**sources, "plannedMigration": True},
        })

        def config(documents):
            env = documents[("Deployment", "cetirun-operator")]["spec"]["template"]["spec"]["containers"][0]["env"]
            return json.loads(next(item["value"] for item in env if item["name"] == "CETIRUN_SANDBOX_SOURCES"))

        self.assertNotIn("plannedMigration", config(baseline))
        self.assertEqual(config(enabled), {**config(baseline), "plannedMigration": True})
        role = ("ClusterRole", "cetirun-operator-manager-role")
        self.assertEqual(baseline[role], enabled[role])
        disabled = self.render(admission)
        for group, resources, verbs in (
            ("", ["nodes"], ["get"]),
            ("storage.k8s.io", ["csinodes", "csidrivers"], ["get"]),
            ("storage.k8s.io", ["volumeattachments"], ["get", "list", "patch"]),
        ):
            rule = {"apiGroups": [group], "resources": resources, "verbs": verbs}
            self.assertIn(rule, baseline[role]["rules"])
            self.assertIn(rule, disabled[role]["rules"])
        for change in ({"plannedMigration": "true"}, {"plannedMigration": 1},
                       {"plannedMigration": True, "nativeExecution": False}):
            with self.subTest(change=change):
                self.assert_rejected(admission, "sandboxSources.plannedMigration", values={
                    **values, "sandboxSources": {**sources, **change},
                })

    def test_placement_default_off_and_separate_serving_bootstrap(self):
        baseline = self.render(self.enforced_values(podsEnabled=True))
        self.assertNotIn(("ValidatingWebhookConfiguration", "cetirun-sandbox-placement"), baseline)
        self.assertIn({"apiGroups": [""], "resources": ["nodes"], "verbs": ["get"]},
                      baseline[("ClusterRole", NAME)]["rules"])
        for rule in baseline[("ClusterRole", NAME)]["rules"]:
            self.assertNotIn("daemonsets", rule["resources"])
            self.assertNotIn("priorityclasses", rule["resources"])

        documents = self.render(self.server_values(podsEnabled=True, placementConfigMapName="pilot-placement"))
        self.assertNotIn(("ValidatingWebhookConfiguration", "cetirun-sandbox-placement"), documents)
        container = documents[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
        config = json.loads(next(value["value"] for value in container["env"] if value["name"] == "CETIRUN_SANDBOX_ADMISSION"))
        self.assertEqual(config["placementConfigMapName"], "pilot-placement")
        self.assertIn("--admission-pod-state-namespace=cetirun-system", container["args"])
        for group, resource in (("", "nodes"), ("apps", "daemonsets"), ("scheduling.k8s.io", "priorityclasses")):
            self.assertIn({"apiGroups": [group], "resources": [resource], "verbs": ["get"]},
                          documents[("ClusterRole", NAME)]["rules"])

    def test_placement_rules_cover_direct_and_bound_placement_cluster_wide(self):
        documents = self.render(self.enforced_values(podsEnabled=True, placementConfigMapName="pilot-placement"))
        hook = documents[("ValidatingWebhookConfiguration", "cetirun-sandbox-placement")]["webhooks"][0]
        self.assertEqual(hook["failurePolicy"], "Fail")
        self.assertEqual(hook["sideEffects"], "None")
        self.assertNotIn("namespaceSelector", hook)
        self.assertNotIn("objectSelector", hook)
        self.assertEqual(hook["clientConfig"]["service"]["path"], "/validate-sandbox-placement")
        self.assertEqual(hook["rules"], [
            {"operations": ["CREATE", "UPDATE"], "apiGroups": [""], "apiVersions": ["v1"],
             "resources": ["pods", "pods/ephemeralcontainers", "pods/resize"], "scope": "Namespaced"},
            {"operations": ["CREATE"], "apiGroups": [""], "apiVersions": ["v1"],
             "resources": ["pods/binding", "bindings"], "scope": "Namespaced"},
            {"operations": ["UPDATE", "DELETE"], "apiGroups": [""], "apiVersions": ["v1"],
             "resources": ["nodes"], "scope": "Cluster"},
        ])
        expressions = "\n".join(item["expression"] for item in hook["matchConditions"])
        self.assertIn('request.resource.resource == "pods"', expressions)
        self.assertIn('"foregroundDeletion"', expressions)
        for rule in documents[("ClusterRole", NAME)]["rules"]:
            self.assertTrue(set(rule["verbs"]) <= {"get", "list"})
            self.assertNotIn("secrets", rule["resources"])
            self.assertNotIn("configmaps", rule["resources"])
            self.assertNotIn("*", rule["resources"])

    def test_placement_rejects_ambiguous_or_unprotected_configuration(self):
        for name in (False, 42, {}, [], " leading", "Invalid", "bad/name", "x" * 254):
            with self.subTest(name=name):
                self.assert_rejected(self.server_values(podsEnabled=True, placementConfigMapName=name),
                                     "sandboxAdmission.placementConfigMapName")
        self.assert_rejected(self.server_values(placementConfigMapName="pilot-placement"),
                             "requires enabled Sandbox configuration and Pod admission")
        self.assert_rejected({"placementConfigMapName": "pilot-placement"},
                             "requires enabled Sandbox configuration and Pod admission")
        rendered = self.helm(self.enforced_values(podsEnabled=True, placementConfigMapName="pilot-placement"), lint=True)
        self.assertEqual(rendered.returncode, 0, rendered.stdout + rendered.stderr)

    def test_pod_rules_use_immutable_namespace_routing_not_pod_labels(self):
        documents = self.render(self.enforced_values(podsEnabled=True))
        webhook = documents[("ValidatingWebhookConfiguration", "cetirun-sandbox-pods")]["webhooks"][0]
        self.assertEqual(webhook["failurePolicy"], "Fail")
        self.assertEqual(webhook["clientConfig"]["service"]["path"], "/validate-sandbox-pods")
        self.assertEqual(webhook["namespaceSelector"], {
            "matchExpressions": [{"key": f"{API_GROUP}/sandbox-protected", "operator": "Exists"}],
        })
        self.assertNotIn("objectSelector", webhook)
        self.assertEqual(webhook["rules"], [{
            "operations": ["CREATE", "UPDATE"], "apiGroups": [""], "apiVersions": ["v1"],
            "resources": ["pods", "pods/ephemeralcontainers", "pods/resize"], "scope": "Namespaced",
        }, {
            "operations": ["CREATE"], "apiGroups": [""], "apiVersions": ["v1"],
            "resources": ["pods/binding", "bindings"], "scope": "Namespaced",
        }, {
            "operations": ["CONNECT"], "apiGroups": [""], "apiVersions": ["v1"],
            "resources": ["pods/exec", "pods/attach", "pods/portforward"], "scope": "Namespaced",
        }])
        policy = documents[("ValidatingAdmissionPolicy", "cetirun-sandbox-namespace")]["spec"]
        self.assertEqual(policy["failurePolicy"], "Fail")
        self.assertEqual(policy["matchConstraints"]["resourceRules"][0]["resources"],
                         ["namespaces", "namespaces/status", "namespaces/finalize"])
        self.assertEqual(policy["matchConstraints"]["resourceRules"][0]["operations"], ["CREATE", "UPDATE"])
        self.assertEqual(
            documents[("ValidatingAdmissionPolicyBinding", "cetirun-sandbox-namespace")]["spec"],
            {"policyName": "cetirun-sandbox-namespace", "validationActions": ["Deny"]},
        )
        expressions = "\n".join(rule["expression"] for rule in policy["validations"])
        self.assertIn('request.userInfo.username == "system:serviceaccount:cetirun-system:cetirun-operator-controller-manager"', expressions)
        self.assertIn('object.metadata.name != "cetirun-system"', expressions)
        self.assertIn("variables.owners == variables.oldOwners", expressions)

    def test_pod_request_budget_reserves_transport_time(self):
        for timeout in (1, 2, 10, 20, 30):
            with self.subTest(timeout=timeout):
                documents = self.render(self.enforced_values(podsEnabled=True, timeoutSeconds=timeout))
                container = documents[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]
                self.assertIn(f"--admission-pod-timeout={timeout * 800}ms", container["args"])
                webhook = documents[("ValidatingWebhookConfiguration", "cetirun-sandbox-pods")]["webhooks"][0]
                self.assertEqual(webhook["timeoutSeconds"], timeout)
        documents = self.render(self.server_values(podsEnabled=True, timeoutSeconds=0))
        args = documents[("Deployment", NAME)]["spec"]["template"]["spec"]["containers"][0]["args"]
        self.assertFalse(any(arg.startswith("--admission-pod-timeout=") for arg in args))

    def test_pod_cleanup_condition_only_excludes_exact_gc_finalization(self):
        documents = self.render(self.enforced_values(podsEnabled=True))
        webhook = documents[("ValidatingWebhookConfiguration", "cetirun-sandbox-pods")]["webhooks"][0]
        conditions = webhook["matchConditions"]
        self.assertEqual([condition["name"] for condition in conditions], ["garbage-collector-cleanup-only"])
        expression = " ".join(conditions[0]["expression"].split())
        self.assertTrue(expression.startswith("!("), expression)
        self.assertTrue(expression.endswith(")"), expression)
        self.assertLessEqual(len(conditions[0]["expression"]), 5 * 1024)
        for fragment in (
            'request.operation == "UPDATE"',
            'request.resource.group == ""',
            'request.resource.version == "v1"',
            'request.resource.resource == "pods"',
            'request.kind.group == ""',
            'request.kind.version == "v1"',
            'request.kind.kind == "Pod"',
            '(has(request.subResource) ? request.subResource : "") == ""',
            '(has(request.requestSubResource) ? request.requestSubResource : "") == ""',
            '(!has(request.requestResource) || request.requestResource == request.resource)',
            '(!has(request.requestKind) || request.requestKind == request.kind)',
            'object != null && oldObject != null',
            'object.apiVersion == "v1" && object.kind == "Pod"',
            'has(request.namespace) && request.namespace != ""',
            'has(request.name) && request.name != ""',
            'object.metadata.namespace == request.namespace',
            'object.metadata.name == request.name',
            'has(object.metadata.uid) && object.metadata.uid != ""',
            'has(object.metadata.resourceVersion) && object.metadata.resourceVersion != ""',
            'has(oldObject.metadata.deletionTimestamp)',
            'oldObject.metadata.deletionTimestamp != null',
        ):
            self.assertIn(fragment + " &&", expression)
        self.assertIn(
            'request.userInfo.username in [ "system:kube-controller-manager", '
            '"system:serviceaccount:kube-system:generic-garbage-collector" ] &&',
            expression,
        )
        self.assertNotIn("request.userInfo.groups", expression)
        self.assertNotIn("admin@example.com", expression)
        self.assertNotIn("cetirun-operator-controller-manager", expression)
        # Map-wide comparison catches additions/removals, including unknown
        # top-level, spec, status and metadata fields rather than an allowlist.
        for fragment in (
            'object.all(field, field == "metadata" || (field in oldObject && object[field] == oldObject[field]) )',
            'oldObject.all(field, field == "metadata" || field in object)',
            'object.metadata.all(field, field in ["finalizers", "managedFields"] || '
            '(field in oldObject.metadata && object.metadata[field] == oldObject.metadata[field]) )',
            'oldObject.metadata.all(field, field in ["finalizers", "managedFields"] || field in object.metadata )',
            'size(oldObject.metadata.finalizers.filter(finalizer, finalizer == "foregroundDeletion")) == 1',
            '(has(object.metadata.finalizers) ? object.metadata.finalizers : []) == '
            'oldObject.metadata.finalizers.filter(finalizer, finalizer != "foregroundDeletion")',
        ):
            self.assertIn(fragment, expression)
        self.assertEqual(webhook["failurePolicy"], "Fail")

    def test_enforcement_renders_exact_webhook_timeouts_including_bounds(self):
        for timeout in (1, 2, 10, 20, 30):
            with self.subTest(timeout=timeout):
                documents = self.render(self.enforced_values(timeoutSeconds=timeout))
                for kind in ("ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"):
                    webhook = documents[(kind, NAME)]["webhooks"][0]
                    self.assertEqual(webhook["timeoutSeconds"], timeout)
                    self.assertIs(type(webhook["timeoutSeconds"]), int)
                    self.assertEqual(webhook["failurePolicy"], "Fail")

    def test_enforcement_rejects_invalid_webhook_timeouts(self):
        for timeout in (0, -1, 31, 0.5, 1.5, "", "invalid", "1.5", True, False, None, [], {}):
            with self.subTest(timeout=timeout):
                self.assert_rejected(
                    self.enforced_values(timeoutSeconds=timeout),
                    "sandboxAdmission.timeoutSeconds must be an integer between 1 and 30",
                )

    def test_webhook_timeout_is_only_validated_when_enforcing(self):
        self.render({"enabled": False, "enforce": False, "timeoutSeconds": 0}, kube_version="1.29.0")
        documents = self.render(self.server_values(timeoutSeconds=0))
        for kind in ("ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"):
            self.assertNotIn((kind, NAME), documents)

    def test_cleanup_condition_defaults_omitted_request_subresource(self):
        documents = self.render(self.enforced_values())
        condition = documents[("ValidatingWebhookConfiguration", NAME)]["webhooks"][0]["matchConditions"][0]
        expression = " ".join(condition["expression"].split())
        self.assertIn('(has(request.subResource) ? request.subResource : "") == "" &&', expression)
        self.assertNotIn('request.subResource == ""', expression)

    def test_cleanup_expression_is_narrow_and_handles_omitted_optional_fields(self):
        documents = self.render(self.enforced_values())
        conditions = documents[("ValidatingWebhookConfiguration", NAME)]["webhooks"][0]["matchConditions"]
        self.assertEqual([condition["name"] for condition in conditions], ["controller-cleanup-only"])
        expression = " ".join(conditions[0]["expression"].split())
        self.assertTrue(expression.startswith("!("), expression)
        self.assertTrue(expression.endswith(")"), expression)
        self.assertLessEqual(len(conditions[0]["expression"]), 5 * 1024)
        for fragment in (
            'request.operation == "UPDATE"',
            f'request.resource.group == "{API_GROUP}"',
            'request.resource.resource == "agents"',
            '(has(request.subResource) ? request.subResource : "") == ""',
            'request.userInfo.username == "system:serviceaccount:cetirun-system:cetirun-operator-controller-manager"',
            "object != null && oldObject != null",
            "has(oldObject.metadata.deletionTimestamp)",
            "oldObject.metadata.deletionTimestamp != null",
        ):
            self.assertIn(fragment + " &&", expression)
        fields = {
            "apiVersion": '""', "kind": '""', "spec": "{}", "status": "{}",
            "metadata.name": '""', "metadata.namespace": '""', "metadata.uid": '""',
            "metadata.labels": "{}", "metadata.annotations": "{}",
            "metadata.ownerReferences": "[]", "metadata.deletionTimestamp": "null",
            "metadata.deletionGracePeriodSeconds": "null", "metadata.generateName": '""',
            "metadata.creationTimestamp": "null", "metadata.generation": "0",
            "metadata.resourceVersion": '""', "metadata.selfLink": '""',
        }
        for field, default in fields.items():
            self.assertIn(
                f"(has(object.{field}) ? object.{field} : {default}) == "
                f"(has(oldObject.{field}) ? oldObject.{field} : {default}) &&",
                expression,
            )
        old = "(has(oldObject.metadata.finalizers) ? oldObject.metadata.finalizers : [])"
        new = "(has(object.metadata.finalizers) ? object.metadata.finalizers : [])"
        self.assertIn(f"size{new} < size{old} &&", expression)
        self.assertIn(f"{new}.all( finalizer, finalizer in {old} ) &&", expression)
        self.assertIn(f"{old}.exists( finalizer, !(finalizer in {new}) ) &&", expression)
        self.assertIn(f"{old}.all( finalizer, finalizer in {new} || finalizer in [", expression)
        self.assertEqual(re.findall(r'"(cetirun\.microsoft\.com/[^"]+)"', expression), FINALIZERS)
        self.assertEqual(expression.count("||"), 1)
        self.assertNotIn("managedFields", expression)

    def test_enforcement_requires_enabled_server(self):
        values = self.enforced_values()
        values["enabled"] = False
        self.assert_rejected(values, "sandboxAdmission.enforce requires sandboxAdmission.enabled")

    def test_server_requires_high_availability_certificates_and_admins(self):
        for replicas in (0, 1, -1, 1.5, 2.5, "invalid", True):
            with self.subTest(replicas=replicas):
                self.assert_rejected(self.server_values(replicaCount=replicas), "sandboxAdmission.replicaCount")
        for name in ("", " \t"):
            with self.subTest(tlsSecretName=name):
                self.assert_rejected(self.server_values(tlsSecretName=name), "sandboxAdmission.tlsSecretName")
        self.assert_rejected(
            self.server_values(adminUsers=[], adminGroups=[]),
            "at least one adminUsers or adminGroups entry",
        )
        for field in ("adminUsers", "adminGroups"):
            for identities in ([""], [" "], [" admin@example.com"], [12], "admin@example.com", {}):
                with self.subTest(field=field, identities=identities):
                    self.assert_rejected(self.server_values(**{field: identities}), f"sandboxAdmission.{field}")
        # The admission server refuses to start with a blanket group.
        for group in (
            "system:authenticated", "system:unauthenticated",
            "system:serviceaccounts", "system:serviceaccounts:cetirun-system",
        ):
            with self.subTest(blanket_group=group):
                self.assert_rejected(self.server_values(adminGroups=[group]), "sandboxAdmission.adminGroups cannot use")
        self.assert_rejected(
            self.server_values(), "must not use the operator controller service account",
            values={"serviceAccount": {"name": NAME}},
        )
        for values in ({"namespace": ""}, {"serviceAccount": {"name": ""}}):
            with self.subTest(values=values):
                self.assert_rejected(self.server_values(), "controller identity", values=values)

    def test_kubernetes_version_gate_applies_to_bootstrap_and_enforcement(self):
        for version in ("1.29.0", "1.30.0", "1.36.9"):
            with self.subTest(version=version):
                self.render(kube_version=version)
                self.render({"enabled": False, "enforce": False}, kube_version=version)
                for admission in (self.server_values(), self.enforced_values()):
                    with self.subTest(enforcing=admission.get("enforce", False)):
                        self.assert_rejected(admission, "Kubernetes >= 1.37", kube_version=version)
        for version in ("1.37.0", "1.37.1-gke.1", "1.38.0"):
            with self.subTest(version=version):
                self.render(self.server_values(), kube_version=version)
                self.render(self.enforced_values(), kube_version=version)

    def test_enforcement_rejects_missing_or_malformed_ca_bundle(self):
        invalid = (
            "", "not-base64", self.ca_pem,
            self.encode_ca("not a PEM certificate"),
            self.encode_ca("-----BEGIN PUBLIC KEY-----\nU3ludGhldGlj\n-----END PUBLIC KEY-----\n"),
            self.encode_ca("-----BEGIN CERTIFICATE-----\n%%invalid%%\n-----END CERTIFICATE-----\n"),
            self.encode_ca("-----BEGIN CERTIFICATE-----\nA===\n-----END CERTIFICATE-----\n"),
            self.encode_ca("-----BEGIN CERTIFICATE-----\n\n-----END CERTIFICATE-----\n"),
            self.encode_ca("-----BEGIN CERTIFICATE-----\nU3ludGhldGlj\n"),
            self.encode_ca(self.ca_pem + "unexpected trailing content"),
        )
        for bundle in invalid:
            with self.subTest(bundle=bundle[:32]):
                values = self.enforced_values()
                values["caBundle"] = bundle
                self.assert_rejected(values, "sandboxAdmission.caBundle")

    def test_enforcement_accepts_ca_rotation_bundle_and_base64_line_breaks(self):
        for pem in (self.ca_pem * 2, self.ca_pem.replace("\n", "\r\n")):
            with self.subTest(pem_lines=pem.count("\n")):
                encoded = base64.encodebytes(pem.encode()).decode()
                values = self.enforced_values()
                values["caBundle"] = encoded
                documents = self.render(values)
                for kind in ("ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"):
                    actual = documents[(kind, NAME)]["webhooks"][0]["clientConfig"]["caBundle"]
                    self.assertEqual(actual, encoded.replace("\n", ""))
                    self.assertEqual(base64.b64decode(actual).decode(), pem)

    def test_helm_lint_default_bootstrap_enforcing_and_source_values(self):
        for admission, values in (
            (None, {}),
            (self.server_values(), {}),
            (self.enforced_values(), {}),
            (self.enforced_values(portalServiceAccount="cetirun-portal"), {}),
            (self.enforced_values(podsEnabled=True), {"sandboxSources": {
                "enabled": True, "platformConfigMapName": "sandbox-platform",
            }}),
        ):
            with self.subTest(admission=admission, values=values):
                result = self.helm(admission, values=values, lint=True)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
