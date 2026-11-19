# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline Operator/umbrella rendering, not admission or runtime qualification."""

import base64
import copy
import hashlib
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
SOURCE = "cetirun-cetiadmin-config"
ADMISSION = "cetirun-sandbox-admission"
WEBHOOK = "cetirun-cetiadmin-admission"
NAMESPACE = "test-platform"
OFFLINE = {
    **os.environ, "KUBECONFIG": os.devnull, "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}


@unittest.skipUnless(shutil.which("helm") and shutil.which("openssl"), "requires local Helm and OpenSSL")
class CetiAdminOperatorChartTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        key = subprocess.run(
            ["openssl", "genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256"],
            text=True, capture_output=True, check=True, timeout=30,
        ).stdout
        pem = subprocess.run(
            ["openssl", "req", "-new", "-x509", "-key", "/dev/stdin", "-subj", "/CN=example.test",
             "-days", "1", "-addext", "basicConstraints=critical,CA:TRUE"],
            input=key, text=True, capture_output=True, check=True, timeout=30,
        ).stdout
        cls.ca_bundle = base64.b64encode(pem.encode()).decode()

    def values(self):
        return {
            "namespace": NAMESPACE, "domainSuffix": "agents.example.com",
            "cetiadmin": {
                "owner": {"alias": "platform-owner", "tenantID": "11111111-1111-1111-1111-111111111111",
                          "objectID": "22222222-2222-2222-2222-222222222222"},
                "kubernetes": {"apiAudience": "https://kubernetes.default.svc.cluster.local",
                               "apiEgressCIDRs": ["192.0.2.10/32"]},
                "model": {"initialMonthlyBudgetUSD": "100"},
            },
            "sandboxAdmission": {"tlsSecretName": "example-admission-tls", "caBundle": self.ca_bundle,
                                 "adminUsers": ["admin@example.com"], "adminGroups": ["platform-admins"]},
        }

    def helm(self, values, *, chart=CHART, version="1.37.0", lint=False):
        args = ["helm", "lint", "--strict", str(chart)] if lint else ["helm", "template", "platform", str(chart)]
        return subprocess.run(
            [*args, "--namespace", NAMESPACE, "--kube-version", version, "--values", "-"],
            input=json.dumps(values), text=True, capture_output=True, env=OFFLINE, cwd=ROOT, timeout=30,
        )

    def render(self, values, **kwargs):
        result = self.helm(values, **kwargs)
        self.assertEqual(result.returncode, 0, result.stderr)
        return {(obj["kind"], obj["metadata"]["name"]): obj for obj in yaml.safe_load_all(result.stdout) if obj}

    @staticmethod
    def config(objects):
        return json.loads(objects[("ConfigMap", SOURCE)]["data"]["config.json"])

    @staticmethod
    def env(objects, name):
        container = objects[("Deployment", name)]["spec"]["template"]["spec"]["containers"][0]
        return {item["name"]: item.get("value") for item in container["env"]}

    def test_defaults_have_no_agent_allowlist(self):
        defaults = yaml.safe_load((CHART / "values.yaml").read_text())["cetiadmin"]
        config = self.config(self.render(self.values()))
        for field in ("scope", "teamNames", "targets", "accessHash"):
            self.assertNotIn(field, defaults)
            self.assertNotIn(field, config)
        self.assertEqual(defaults["platformReadNamespaces"], [])

    def test_legacy_disabled_source_is_an_explicit_upgrade_only_format(self):
        values = self.values()
        values["cetiadmin"].update(enabled=False, legacyDisabledSource=True,
                                 platformReadNamespaces=["platform-diagnostics"])
        config = self.config(self.render(values))
        self.assertFalse(config["enabled"])
        self.assertNotIn("legacyDisabledSource", config)
        self.assertNotIn("platformReadNamespaces", config)
        self.assertEqual(config["scope"], {"mode": "all-managed-agents", "teamNames": [],
                                         "platformReadNamespaces": ["platform-diagnostics"]})
        values["cetiadmin"]["enabled"] = True
        rejected = self.helm(values)
        self.assertNotEqual(rejected.returncode, 0)
        self.assertIn("requires cetiadmin.enabled=false", rejected.stderr)

    def test_legacy_scope_is_rejected_even_when_disabled(self):
        for enabled in (True, False):
            for scope in (None, {}, {"mode": "all-managed-agents"}, {"mode": "teams", "teamNames": ["example-team"]}):
                with self.subTest(enabled=enabled, scope=scope):
                    inputs = self.values()
                    inputs["cetiadmin"].update(enabled=enabled, scope=scope)
                    result = self.helm(inputs)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("unsupported cetiadmin field scope", result.stderr)

    def test_platform_read_namespaces_are_top_level_diagnostics_only(self):
        inputs = self.values()
        inputs["cetiadmin"]["platformReadNamespaces"] = ["platform-test", "kube-system"]
        config = self.config(self.render(inputs))
        self.assertEqual(config["platformReadNamespaces"], ["platform-test", "kube-system"])
        self.assertNotIn("scope", config)

    def test_platform_diagnostics_require_namespace_labels(self):
        for value in ("platform", None, [42], [""], ["Wrong_Name"], ["platform.example"], ["a" * 64]):
            with self.subTest(value=value):
                inputs = self.values()
                inputs["cetiadmin"]["platformReadNamespaces"] = value
                result = self.helm(inputs)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("cetiadmin.platformReadNamespaces", result.stderr)

    def test_documented_configuration_examples_render_without_agent_allowlists(self):
        path = ROOT / "app/cetirun-platform/README.md"
        examples = []
        for block in re.findall(r"^```yaml\n(.*?)^```", path.read_text(), re.MULTILINE | re.DOTALL):
            document = yaml.safe_load(block)
            admin = document.get("cetiadmin") or document.get("platform", {}).get("values", {}).get("cetirun-operator", {}).get("cetiadmin")
            if admin:
                examples.append(admin)
        self.assertEqual(len(examples), 1)
        inputs = self.values()
        inputs["cetiadmin"] = examples[0]
        config = self.config(self.render(inputs))
        self.assertEqual(config["platformReadNamespaces"], ["cetirun-system"])
        for field in ("scope", "teamNames", "targets", "accessHash"):
            self.assertNotIn(field, config)

    def test_enabled_by_default_but_missing_inputs_fail(self):
        defaults = yaml.safe_load((CHART / "values.yaml").read_text())
        self.assertIs(defaults["cetiadmin"]["enabled"], True)
        result = self.helm({})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cetiadmin.owner", result.stderr)
        for keys, value, message in (
            (("cetiadmin", "owner", "objectID"), "", "cetiadmin.owner.objectID"),
            (("cetiadmin", "model", "initialMonthlyBudgetUSD"), "", "initialMonthlyBudgetUSD"),
            (("cetiadmin", "kubernetes", "apiAudience"), "", "apiAudience"),
            (("cetiadmin", "kubernetes", "apiEgressCIDRs"), [], "apiEgressCIDRs"),
            (("sandboxAdmission", "tlsSecretName"), "", "tlsSecretName"),
            (("sandboxAdmission", "caBundle"), "", "caBundle"),
            (("aiGateway", "enabled"), False, "aiGateway.enabled"),
            (("cetirun", "chart", "version"), "0.0.1", "runtime.version"),
        ):
            with self.subTest(keys=keys):
                inputs = self.values()
                parent = inputs
                for key in keys[:-1]:
                    parent = parent.setdefault(key, {})
                parent[keys[-1]] = value
                result = self.helm(inputs)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn(message, result.stderr)

    def test_one_retained_source_no_agent_team_or_second_runtime(self):
        objects = self.render(self.values())
        source = objects[("ConfigMap", SOURCE)]
        self.assertEqual(source["metadata"]["namespace"], NAMESPACE)
        self.assertEqual(source["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertNotIn("helm.sh/hook", source["metadata"]["annotations"])
        self.assertEqual(set(source["data"]), {"config.json"})
        self.assertEqual(sum(obj["kind"] == "ConfigMap" for obj in objects.values()), 1)
        self.assertFalse(any(obj["kind"] in {"Agent", "Team", "StatefulSet", "PersistentVolumeClaim"} for obj in objects.values()))
        config = self.config(objects)
        self.assertTrue(config["enabled"])
        self.assertEqual(config["instanceName"], "cetiadmin")
        runtime_version = yaml.safe_load((ROOT / "app/cetirun-chart/Chart.yaml").read_text())["version"]
        self.assertEqual(config["runtime"], {"version": runtime_version})
        self.assertEqual(config["model"], {"default": "auto", "githubCopilot": False, "initialMonthlyBudgetUSD": "100"})
        self.assertNotIn("hookTimeoutSeconds", config)
        self.assertFalse(config["monitor"]["enabled"])

    def test_hooks_share_image_exact_source_hash_and_separate_narrow_accounts(self):
        inputs = self.values()
        inputs.update({"global": {"registry": "registry.example.com/runtime", "version": "1.2.3"},
                       "image": {"digest": "sha256:" + "a" * 64}, "azure": {"clientId": "not-in-hooks"},
                       "acrDockerSecret": "not-in-hooks", "imagePullSecrets": [{"name": "example-pull"}]})
        objects = self.render(inputs)
        raw = objects[("ConfigMap", SOURCE)]["data"]["config.json"]
        digest = hashlib.sha256(raw.encode()).hexdigest()
        image = "registry.example.com/runtime/cetirun-operator:1.2.3@sha256:" + "a" * 64
        env = self.env(objects, "cetirun-operator")
        self.assertEqual(env["CETIRUN_CETIADMIN_CONFIG_MAP"], SOURCE)
        self.assertEqual(env["CETIRUN_OPERATOR_IMAGE"], image)
        self.assertEqual(self.config(objects)["runtime"]["version"], "1.2.3")
        for mode, event in (("check", "post-install,post-upgrade"), ("stop", "pre-delete")):
            name = "cetirun-cetiadmin-" + mode
            job = objects[("Job", name)]
            self.assertEqual(job["metadata"]["labels"]["app.kubernetes.io/managed-by"], "Helm")
            self.assertEqual(job["metadata"]["annotations"]["meta.helm.sh/release-name"], "platform")
            self.assertEqual(job["metadata"]["annotations"]["meta.helm.sh/release-namespace"], NAMESPACE)
            self.assertEqual(job["metadata"]["annotations"]["helm.sh/hook"], event)
            self.assertEqual(job["metadata"]["annotations"]["helm.sh/hook-weight"], "10")
            self.assertEqual(job["metadata"]["annotations"]["helm.sh/hook-delete-policy"], "before-hook-creation,hook-succeeded")
            self.assertEqual(job["spec"]["backoffLimit"], 0)
            self.assertEqual(job["spec"]["activeDeadlineSeconds"], 630)
            pod = job["spec"]["template"]["spec"]
            self.assertEqual(pod["serviceAccountName"], name)
            self.assertEqual(pod["restartPolicy"], "Never")
            self.assertEqual(pod["imagePullSecrets"], [{"name": "example-pull"}])
            self.assertNotIn("volumes", pod)
            self.assertNotIn("azure.workload.identity/use", job["spec"]["template"]["metadata"].get("labels", {}))
            container = pod["containers"][0]
            self.assertEqual(container["image"], image)
            self.assertNotIn("env", container)
            for flag in (f"--mode=cetiadmin-{mode}", f"--cetiadmin-namespace={NAMESPACE}",
                         f"--cetiadmin-config-map={SOURCE}", f"--cetiadmin-config-hash={digest}", "--cetiadmin-timeout=600s"):
                self.assertIn(flag, container["args"])
            rules = objects[("Role", name)]["rules"] + objects[("ClusterRole", name)]["rules"]
            writes = [rule for rule in rules if set(rule["verbs"]) - {"get", "list"}]
            expected = [] if mode == "check" else [{"apiGroups": [""], "resources": ["configmaps"],
                                                   "resourceNames": [SOURCE], "verbs": ["patch"]}]
            self.assertEqual(writes, expected)
            self.assertFalse(any("secrets" in rule["resources"] or "*" in rule["resources"] for rule in rules))

    def test_namespace_label_guard_is_independent_and_readable_by_hooks(self):
        inputs = self.values()
        inputs["sandboxAdmission"].update(enabled=False, enforce=False)
        objects = self.render(inputs)
        name = "cetirun-cetiadmin-namespace"
        policy = objects[("ValidatingAdmissionPolicy", name)]["spec"]
        binding = objects[("ValidatingAdmissionPolicyBinding", name)]["spec"]
        self.assertEqual(policy["failurePolicy"], "Fail")
        self.assertEqual(binding, {"policyName": name, "validationActions": ["Deny"]})
        self.assertNotIn("namespaceSelector", policy["matchConstraints"])
        self.assertNotIn("objectSelector", policy["matchConstraints"])
        self.assertEqual(policy["matchConstraints"]["resourceRules"], [{
            "operations": ["CREATE", "UPDATE"], "apiGroups": [""], "apiVersions": ["v1"],
            "resources": ["namespaces", "namespaces/status", "namespaces/finalize"], "scope": "Cluster",
        }])
        expression = policy["validations"][0]["expression"]
        for required in (NAMESPACE, "cetirun-cetiadmin", "oldObject", "control-plane", "kubernetes.azure.com/managedby", "cetirun-operator"):
            self.assertIn(required, expression)
        for mode in ("check", "stop"):
            rules = objects[("ClusterRole", "cetirun-cetiadmin-" + mode)]["rules"]
            guard = next(rule for rule in rules if "validatingadmissionpolicies" in rule["resources"])
            self.assertEqual(guard["resourceNames"], [name])
            self.assertEqual(guard["verbs"], ["get"])
        for kind in ("ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"):
            self.assertNotIn("admissions.enforcer/disabled", objects[(kind, WEBHOOK)]["metadata"].get("annotations", {}))

    def test_independent_fail_closed_admission_keeps_original_allowlist(self):
        inputs = self.values()
        inputs["sandboxAdmission"].update({"enabled": False, "enforce": False, "portalServiceAccount": "cetirun-portal"})
        objects = self.render(inputs)
        config = json.loads(self.env(objects, ADMISSION)["CETIRUN_SANDBOX_ADMISSION"])
        self.assertEqual(config["cetiadmin"], {
            "namespace": NAMESPACE, "configMapName": SOURCE,
            "runtimeUsername": "system:serviceaccount:cetirun-cetiadmin:openclaw-cetiadmin-cetirun",
            "stopUsername": f"system:serviceaccount:{NAMESPACE}:cetirun-cetiadmin-stop",
            "adminUsers": ["admin@example.com"], "adminGroups": ["platform-admins"],
        })
        self.assertEqual(config["adminUsers"], ["admin@example.com", f"system:serviceaccount:{NAMESPACE}:cetirun-portal"])
        self.assertNotIn("nativeExecution", config)
        self.assertNotIn("CETIRUN_SANDBOX_SOURCES", self.env(objects, "cetirun-operator"))
        self.assertNotIn(("ValidatingWebhookConfiguration", ADMISSION), objects)
        hooks = objects[("ValidatingWebhookConfiguration", WEBHOOK)]["webhooks"]
        resources = set()
        for hook in hooks:
            self.assertEqual(hook["failurePolicy"], "Fail")
            self.assertEqual(hook["clientConfig"]["service"], {
                "name": ADMISSION, "namespace": NAMESPACE, "path": "/validate-cetiadmin", "port": 443,
            })
            self.assertEqual(hook["clientConfig"]["caBundle"], self.ca_bundle)
            if hook["name"].startswith("cetiadmin-workloads."):
                self.assertEqual(hook["namespaceSelector"], {"matchLabels": {"app.kubernetes.io/managed-by": "cetirun-operator"}})
            else:
                self.assertNotIn("namespaceSelector", hook)
            self.assertNotIn("objectSelector", hook)
            for rule in hook["rules"]:
                resources.update(rule["resources"])
        self.assertTrue({"configmaps", "namespaces", "namespaces/finalize", "serviceaccounts", "agents",
                         "pods", "pods/exec", "pods/ephemeralcontainers", "statefulsets", "statefulsets/scale",
                         "deployments", "replicasets", "daemonsets", "jobs", "cronjobs"} <= resources)
        workload = next(hook for hook in hooks if hook["name"].startswith("cetiadmin-workloads."))
        expression = " ".join(item["expression"] for item in workload["matchConditions"])
        self.assertIn(f'request.namespace != "{NAMESPACE}"', expression)
        self.assertIn("helm-maintenance-", expression)
        role = objects[("Role", WEBHOOK)]
        config_rule = next(rule for rule in role["rules"] if "configmaps" in rule["resources"])
        self.assertEqual(set(config_rule["resourceNames"]), {
            SOURCE, "cetiadmin-binding-" + hashlib.sha256(f"{NAMESPACE}/{SOURCE}".encode()).hexdigest()[:24],
        })
        for rule in objects[("ClusterRole", ADMISSION)]["rules"]:
            self.assertTrue(set(rule["verbs"]) <= {"get", "list"})
            self.assertNotIn("secrets", rule["resources"])

    def test_governance_mutator_matches_only_machine_and_never_runs_twice(self):
        for enforce in (False, True):
            inputs = self.values()
            inputs["sandboxAdmission"].update({"enabled": enforce, "enforce": enforce})
            objects = self.render(inputs)
            mutators = [hook for (kind, _), obj in objects.items() if kind == "MutatingWebhookConfiguration"
                        for hook in obj["webhooks"] if hook["clientConfig"]["service"]["path"] == "/mutate-sandbox-agent"]
            self.assertEqual(len(mutators), 2 if enforce else 1)
            hook = objects[("MutatingWebhookConfiguration", WEBHOOK)]["webhooks"][0]
            self.assertEqual(hook["failurePolicy"], "Fail")
            self.assertEqual(hook["rules"][0]["operations"], ["CREATE", "UPDATE"])
            self.assertEqual(hook["name"], "cetiadmin-agent.cetirun.microsoft.com")
            self.assertEqual(hook["matchConditions"], [{
                "name": "cetiadmin-caller",
                "expression": 'request.userInfo.username == "system:serviceaccount:cetirun-cetiadmin:openclaw-cetiadmin-cetirun"',
            }])
            if enforce:
                global_hook = objects[("MutatingWebhookConfiguration", ADMISSION)]["webhooks"][0]
                self.assertEqual(global_hook["matchConditions"][-1], {
                    "name": "exclude-cetiadmin-caller",
                    "expression": 'request.userInfo.username != "system:serviceaccount:cetirun-cetiadmin:openclaw-cetiadmin-cetirun"',
                })
                self.assertEqual(global_hook["matchConditions"][0]["name"], "controller-cleanup-only")
                without = copy.deepcopy(inputs)
                without["cetiadmin"] = {"enabled": False}
                old_global = self.render(without)[("MutatingWebhookConfiguration", ADMISSION)]["webhooks"][0]
                reduced = copy.deepcopy(global_hook)
                reduced["matchConditions"].pop()
                self.assertEqual(reduced, old_global)

    def test_maintenance_configmap_creation_is_guarded_in_managed_namespaces(self):
        objects = self.render(self.values())
        hooks = objects[("ValidatingWebhookConfiguration", WEBHOOK)]["webhooks"]
        workload = next(hook for hook in hooks if hook["name"].startswith("cetiadmin-workloads."))
        rules = [rule for rule in workload["rules"]
                 if "" in rule["apiGroups"] and "configmaps" in rule["resources"]]
        self.assertTrue(rules, "missing ConfigMap restoration admission coverage")
        self.assertTrue(any({"CREATE", "UPDATE", "DELETE"} <= set(rule["operations"])
                            and rule["scope"] == "Namespaced" for rule in rules))
        self.assertEqual(workload["namespaceSelector"], {
            "matchLabels": {"app.kubernetes.io/managed-by": "cetirun-operator"},
        })

    def test_machine_and_stop_hook_cannot_be_promoted_to_administrators(self):
        for username in (
            "system:serviceaccount:cetirun-cetiadmin:openclaw-cetiadmin-cetirun",
            f"system:serviceaccount:{NAMESPACE}:cetirun-cetiadmin-stop",
        ):
            inputs = self.values()
            inputs["sandboxAdmission"]["adminUsers"].append(username)
            result = self.helm(inputs)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("must not be included in sandboxAdmission.adminUsers", result.stderr)

    def test_instance_name_fits_helm_release_and_controller_account(self):
        inputs = self.values()
        name = "a" * 39 + "----z"
        inputs["cetiadmin"]["instanceName"] = name
        objects = self.render(inputs)
        account = ("openclaw-" + name + "-cetirun")[:63].rstrip("-")
        caller = f"system:serviceaccount:cetirun-{name}:{account}"
        condition = objects[("MutatingWebhookConfiguration", WEBHOOK)]["webhooks"][0]["matchConditions"][0]
        self.assertEqual(condition["expression"], "request.userInfo.username == " + json.dumps(caller))
        rule = next(rule for rule in objects[("ClusterRole", "cetirun-cetiadmin-check")]["rules"] if "serviceaccounts" in rule["resources"])
        self.assertEqual(rule["resourceNames"], [account])
        for enabled in (True, False):
            for length in (45, 55):
                with self.subTest(enabled=enabled, length=length):
                    inputs["cetiadmin"].update(enabled=enabled, instanceName="a" * length)
                    result = self.helm(inputs)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("cetiadmin.instanceName", result.stderr)

    def test_disabled_fresh_install_keeps_stop_path_without_identity_or_tls(self):
        objects = self.render({"cetiadmin": {"enabled": False}}, version="1.29.0")
        self.assertFalse(self.config(objects)["enabled"])
        self.assertNotIn(("Deployment", ADMISSION), objects)
        self.assertNotIn(("ValidatingWebhookConfiguration", WEBHOOK), objects)
        self.assertIn(("Job", "cetirun-cetiadmin-check"), objects)
        self.assertIn(("Job", "cetirun-cetiadmin-stop"), objects)
        self.assertEqual(self.env(objects, "cetirun-operator")["CETIRUN_CETIADMIN_CONFIG_MAP"], SOURCE)

    def test_disable_upgrade_retains_protection_until_explicit_uninstall(self):
        inputs = self.values()
        before = self.render(inputs)
        inputs["cetiadmin"]["enabled"] = False
        after = self.render(inputs)
        self.assertEqual(set(before), set(after))
        self.assertFalse(self.config(after)["enabled"])
        changed = {key for key in before if before[key] != after[key]}
        self.assertEqual(changed, {("ConfigMap", SOURCE), ("Job", "cetirun-cetiadmin-check"), ("Job", "cetirun-cetiadmin-stop")})
        for kind in ("Service", "Deployment", "ServiceAccount"):
            self.assertEqual(before[(kind, ADMISSION)], after[(kind, ADMISSION)])
        self.assertEqual(before[("ValidatingWebhookConfiguration", WEBHOOK)], after[("ValidatingWebhookConfiguration", WEBHOOK)])
        self.assertEqual(after[("Job", "cetirun-cetiadmin-stop")]["metadata"]["annotations"]["helm.sh/hook"], "pre-delete")
        inputs["sandboxAdmission"]["tlsSecretName"] = ""
        result = self.helm(inputs)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("tlsSecretName", result.stderr)

    def test_invalid_scope_integrations_runtime_and_ca_are_rejected(self):
        for override, message in (
            ({"enabled": "true"}, "enabled"), ({"instanceName": "bad/name"}, "instanceName"),
            ({"scope": {"mode": "teams", "teamNames": []}}, "unsupported cetiadmin field scope"),
            ({"teamNames": ["example-team"]}, "unsupported cetiadmin field teamNames"),
            ({"azureWorkloadIdentity": {"enabled": True}}, "clientId"),
            ({"monitor": {"enabled": True, "deliver": "slack"}}, "slackChannel"),
            ({"monitor": {"deliver": "teams"}}, "deliver"), ({"teams": {"enabled": True}}, "unsupported"),
            ({"assets": {"image": "other:1"}}, "unsupported"),
            ({"runtime": {"version": "0.0.1"}}, "runtime.version"),
            ({"runtime": {"port": 18789}}, "runtime.port"),
            ({"runtime": {"port": 0}}, "runtime.port"),
            ({"runtime": {"version": False}}, "runtime.version"),
            ({"hookTimeoutSeconds": 0}, "hookTimeoutSeconds"),
        ):
            with self.subTest(override=override):
                inputs = self.values()
                inputs["cetiadmin"].update(override)
                result = self.helm(inputs)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
        for bundle in ("bad-base64", base64.b64encode(b"not a PEM certificate").decode()):
            inputs = self.values()
            inputs["sandboxAdmission"]["caBundle"] = bundle
            self.assertIn("caBundle", self.helm(inputs).stderr)
        self.assertIn("Kubernetes >= 1.37", self.helm(self.values(), version="1.36.9").stderr)

    def test_valid_optional_settings_survive_and_lint_passes(self):
        inputs = self.values()
        inputs["cetiadmin"].update({
            "platformReadNamespaces": ["kube-system"],
            "azureWorkloadIdentity": {"enabled": True, "clientId": "33333333-3333-3333-3333-333333333333"},
            "sharedSkills": {"enabled": True},
            "monitor": {"enabled": True, "schedule": "every 30m", "timezone": "Asia/Singapore", "deliver": "slack",
                        "slackChannel": "CEXAMPLE", "slackSecretName": "example-slack"},
        })
        inputs["cetirun"] = {"helmValues": {"skills": {"storageAccount": "exampleskills", "resourceGroup": "example"}}}
        config = self.config(self.render(inputs))
        for name in ("platformReadNamespaces", "azureWorkloadIdentity", "sharedSkills", "monitor"):
            self.assertEqual(config[name], inputs["cetiadmin"][name])
        result = self.helm(inputs, lint=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_umbrella_uses_the_same_source_defaults_and_hook_contract(self):
        with tempfile.TemporaryDirectory(prefix="cetiadmin-umbrella-") as directory:
            base = Path(directory)
            for relative in ("cetirun-platform", "cetirun-operator/chart", "cetirun-portal/chart",
                             "cetirun-ai-gateway/chart", "cetirun-connections-mcp/chart", "cetirun-observability/chart"):
                shutil.copytree(ROOT / "app" / relative, base / relative, ignore=shutil.ignore_patterns("charts"))
            chart = base / "cetirun-platform"
            subprocess.run(["helm", "dependency", "build", "--skip-refresh", str(chart)],
                           env=OFFLINE, capture_output=True, check=True, timeout=30)
            operator = self.values()
            operator["sandboxAdmission"]["portalServiceAccount"] = "cetirun-portal"
            inputs = {"istio": {"enabled": True}, "cetirun-operator": copy.deepcopy(operator),
                      "cetirun-portal": {"istio": {"host": "portal.example.com"}, "aiGateway": {"catalogNamespace": NAMESPACE}}}
            direct, umbrella = self.render(operator), self.render(inputs, chart=chart)
            for key, obj in direct.items():
                if "cetiadmin" in key[1]:
                    self.assertEqual(obj, umbrella[key])
            self.assertEqual(self.env(direct, ADMISSION), self.env(umbrella, ADMISSION))


if __name__ == "__main__":
    unittest.main()
