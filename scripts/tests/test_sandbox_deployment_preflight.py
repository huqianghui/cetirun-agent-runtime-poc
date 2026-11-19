# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline deployed-Sandbox checks; every kubectl process is replaced by Python."""

import base64
import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import textwrap
import time
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "sandbox-platform-qualify.py"
SPEC = importlib.util.spec_from_file_location("deployment_preflight", SCRIPT)
qualify = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(qualify)
REAL_POPEN = subprocess.Popen
NAMESPACE = "preflight-test"
CORE = f"/api/v1/namespaces/{NAMESPACE}/"
APPS = f"/apis/apps/v1/namespaces/{NAMESPACE}/"
ADMISSION = "/apis/admissionregistration.k8s.io/v1/"
OPERATOR = APPS + "deployments/selected-controller"
SERVER = APPS + "deployments/selected-admission"
SERVICE = CORE + "services/cetirun-sandbox-admission"
ENDPOINTS = CORE + "endpoints/cetirun-sandbox-admission"
PLATFORM = CORE + "configmaps/selected-platform"
PLACEMENT = CORE + "configmaps/selected-placement"
POD = CORE + "pods/admission-a"
REPLICA_SET = APPS + "replicasets/admission-rs"
CLUSTER = "/api/v1/namespaces/kube-system"
IMAGE = "registry.example.com/operator@sha256:" + "a" * 64
SERVER_IMAGE = "registry.example.com/operator@sha256:" + "b" * 64
CA = base64.b64encode(b"-----BEGIN CERTIFICATE-----\nZml4dHVyZQ==\n-----END CERTIFICATE-----\n").decode()
SENTINEL = "sensitive-synthetic-value.example.com"
POD_WEBHOOK = ADMISSION + "validatingwebhookconfigurations/cetirun-sandbox-pods"
BINDING_RULE = {"apiGroups": [""], "apiVersions": ["v1"], "operations": ["CREATE"],
                "resources": ["pods/binding", "bindings"], "scope": "Namespaced"}


def cleanup_condition(pod=False):
    template = (SCRIPT.parents[1] / "app/cetirun-operator/chart/templates/_sandbox-admission.tpl").read_text()
    helper = "sandboxPodCleanupMatch" if pod else "sandboxCleanupMatch"
    block = template.split('{{- define "cetirun-operator.' + helper + '" -}}', 1)[1].split("{{- end -}}", 1)[0]
    header, expression = block.split("  expression: >-\n", 1)
    expression = textwrap.dedent(expression).strip().replace(
        "{{ . | toJson }}", json.dumps(f"system:serviceaccount:{NAMESPACE}:selected-controller-sa"))
    assert "{{" not in expression
    return {"name": header.strip().removeprefix("- name: "), "expression": expression}


def arguments():
    return [
        "deployment-preflight", "--kubeconfig", "/example/explicit-config", "--context", "selected-context",
        "--cluster-uid", "cluster-uid", "--namespace", NAMESPACE,
        "--operator-deployment", "selected-controller", "--operator-uid", "controller-uid",
        "--operator-image", IMAGE, "--operator-service-account", "selected-controller-sa",
        "--operator-service-account-uid", "controller-sa-uid",
        "--admission-deployment", "selected-admission", "--admission-uid", "admission-uid",
        "--admission-image", SERVER_IMAGE, "--admission-service-account", "selected-admission-sa",
        "--admission-service-account-uid", "admission-sa-uid",
    ]


def fixture(*, native=False, planned_migration=False):
    objects = {}

    def put(path, kind, name, *, uid=None, namespace=None, **fields):
        api = "apps/v1" if path.startswith("/apis/apps/") else (
            "admissionregistration.k8s.io/v1" if path.startswith(ADMISSION) else "v1")
        meta = {"name": name, "uid": uid or name + "-uid", "resourceVersion": "1"}
        if namespace is not None:
            meta["namespace"] = namespace
        objects[path] = {"apiVersion": api, "kind": kind, "metadata": meta, **fields}
        return objects[path]

    objects["config"] = {
        "current-context": "selected-context",
        "contexts": [{"name": "selected-context", "context": {"cluster": "selected-cluster", "user": "selected-user"}}],
        "users": [{"name": "selected-user", "user": {"token": "REDACTED"}}],
        "clusters": [{"name": "selected-cluster", "cluster": {"server": "https://cluster.example.com"}}],
    }
    objects["/version"] = {"gitVersion": "v1.37.0", "gitCommit": SENTINEL}
    put(CLUSTER, "Namespace", "kube-system", uid="cluster-uid")
    sources = {
        "enabled": True, "platformConfigMapName": "selected-platform", "placementConfigMapName": "selected-placement",
        "admissionServiceAccount": "selected-admission-sa", "homeClaimName": "home", "skillsClaimName": "skills",
        "helpers": {"doctor": False, "logger": False, "desktop": False},
    }
    config = {
        "controllerUsername": f"system:serviceaccount:{NAMESPACE}:selected-controller-sa",
        "validationMode": "strict", "adminUsers": ["admin@example.com"], "adminGroups": [],
        "platformNamespace": NAMESPACE, "platformConfigMapName": "selected-platform",
        "placementConfigMapName": "selected-placement",
    }
    if native:
        sources.update(nativeExecution=True, bootstrapOwnedPrerequisites=True, skillsClaimName="")
        config["nativeExecution"] = True
        sources.pop("placementConfigMapName")
        config.pop("placementConfigMapName")
    if planned_migration:
        sources["plannedMigration"] = True
    operator = {
        "name": "manager", "image": IMAGE, "args": ["--leader-elect=true", "--domain-suffix=example.com"],
        "env": [
            {"name": "OPERATOR_NAMESPACE", "valueFrom": {"fieldRef": {"apiVersion": "v1", "fieldPath": "metadata.namespace"}}},
            {"name": "OPERATOR_SA_NAME", "value": "selected-controller-sa"},
            {"name": "CETIRUN_SANDBOX_SOURCES", "value": json.dumps(sources)},
        ],
    }
    if native:
        operator["env"].append({"name": "AI_GATEWAY_ENABLED", "value": "true"})
    admission = {
        "name": "admission", "image": SERVER_IMAGE,
        "args": ["--mode=sandbox-admission", "--admission-port=9443",
                 "--admission-cert-dir=/var/run/cetirun-admission", "--health-probe-bind-address=:8081",
                 f"--admission-pod-state-namespace={NAMESPACE}", "--admission-pod-timeout=4000ms"],
        "env": [{"name": "CETIRUN_SANDBOX_ADMISSION", "value": json.dumps(config)}],
        "ports": [{"name": "admission", "containerPort": 9443, "protocol": "TCP"},
                  {"name": "health", "containerPort": 8081, "protocol": "TCP"}],
        "readinessProbe": {"httpGet": {"path": "/readyz", "port": "health"}, "periodSeconds": 10},
        "volumeMounts": [{"name": "admission-tls", "mountPath": "/var/run/cetirun-admission", "readOnly": True}],
    }
    tls_volume = {"name": "admission-tls", "secret": {
        "secretName": "synthetic-tls-reference", "defaultMode": 288,
        "items": [{"key": "tls.crt", "path": "tls.crt"}, {"key": "tls.key", "path": "tls.key"}],
    }}
    for path, name, uid, account, account_uid, container, replicas in (
        (OPERATOR, "selected-controller", "controller-uid", "selected-controller-sa", "controller-sa-uid", operator, 1),
        (SERVER, "selected-admission", "admission-uid", "selected-admission-sa", "admission-sa-uid", admission, 2),
    ):
        spec = {"serviceAccountName": account, "containers": [container], "volumes": [tls_volume] if replicas == 2 else []}
        value = put(path, "Deployment", name, uid=uid, namespace=NAMESPACE, spec={
            "replicas": replicas, "selector": {"matchLabels": {"app": name}},
            "template": {"metadata": {"labels": {"app": name}}, "spec": spec},
        }, status={"observedGeneration": 3, **{key: replicas for key in
                                             ("replicas", "updatedReplicas", "readyReplicas", "availableReplicas")}})
        value["metadata"]["generation"] = 3
        put(CORE + "serviceaccounts/" + account, "ServiceAccount", account, uid=account_uid, namespace=NAMESPACE)
    put(PLATFORM, "ConfigMap", "selected-platform", namespace=NAMESPACE, data={"platform.json": json.dumps({
        "version": "v1", "registrations": {}, "platforms": {}, "runtimeClasses": {}, "storageClasses": {},
    })})
    if not native:
        put(PLACEMENT, "ConfigMap", "selected-placement", namespace=NAMESPACE, data={"placement.json": json.dumps({
            "version": "v1", "nodes": [], "platformDaemonSets": [],
        })})
    put(SERVICE, "Service", "cetirun-sandbox-admission", namespace=NAMESPACE, spec={
        "type": "ClusterIP", "clusterIP": "192.0.2.100", "selector": {"app": "selected-admission"},
        "ports": [{"name": "admission", "port": 443, "targetPort": "admission", "protocol": "TCP"}],
    })
    replica_set = put(REPLICA_SET, "ReplicaSet", "admission-rs", namespace=NAMESPACE)
    replica_set["metadata"]["ownerReferences"] = [{
        "apiVersion": "apps/v1", "kind": "Deployment", "name": "selected-admission",
        "uid": "admission-uid", "controller": True,
    }]
    addresses = []
    for index, letter in enumerate(("a", "b"), 1):
        name = "admission-" + letter
        spec = copy.deepcopy(objects[SERVER]["spec"]["template"]["spec"])
        spec["nodeName"] = "node-" + letter
        spec["volumes"].append({"name": "kube-api-access-example", "projected": {"sources": [
            {"serviceAccountToken": {"expirationSeconds": 3600, "path": "token"}},
        ]}})
        spec["containers"][0]["volumeMounts"].append({
            "name": "kube-api-access-example", "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount", "readOnly": True,
        })
        pod = put(CORE + "pods/" + name, "Pod", name, namespace=NAMESPACE, spec=spec,
                  status={"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}],
                          "podIPs": [{"ip": f"192.0.2.{index}"}]})
        pod["metadata"].update(labels={"app": "selected-admission"}, ownerReferences=[{
            "apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "admission-rs",
            "uid": "admission-rs-uid", "controller": True,
        }])
        addresses.append({"ip": f"192.0.2.{index}", "targetRef": {
            "kind": "Pod", "namespace": NAMESPACE, "name": name, "uid": name + "-uid",
        }})
    put(ENDPOINTS, "Endpoints", "cetirun-sandbox-admission", namespace=NAMESPACE, subsets=[{
        "ports": [{"name": "admission", "port": 9443, "protocol": "TCP"}], "addresses": addresses,
    }])
    group, pods = ["cetirun.microsoft.com"], ["pods", "pods/ephemeralcontainers", "pods/resize"]
    for resource, kind, name, hook, route, rules in (
        ("validatingwebhookconfigurations", "ValidatingWebhookConfiguration", "cetirun-sandbox-admission",
         "sandbox-configuration", "validate-sandbox-configuration",
         [(group, ["v1alpha1"], ["CREATE", "UPDATE"], ["agents", "teams", "sandboxes", "agentpolicies"], "Cluster"),
          (group, ["v1alpha1"], ["DELETE"], ["teams", "sandboxes", "agentpolicies"], "Cluster")]),
        ("mutatingwebhookconfigurations", "MutatingWebhookConfiguration", "cetirun-sandbox-admission",
         "sandbox-agent", "mutate-sandbox-agent",
         [(group, ["v1alpha1"], ["CREATE", "UPDATE"], ["agents"], "Cluster")]),
        ("validatingwebhookconfigurations", "ValidatingWebhookConfiguration", "cetirun-sandbox-pods",
         "sandbox-pods", "validate-sandbox-pods",
         [([""], ["v1"], ["CREATE", "UPDATE"], pods, "Namespaced"),
          ([""], ["v1"], ["CONNECT"], ["pods/exec", "pods/attach", "pods/portforward"], "Namespaced")]),
        ("validatingwebhookconfigurations", "ValidatingWebhookConfiguration", "cetirun-sandbox-placement",
         "sandbox-placement", "validate-sandbox-placement",
         [([""], ["v1"], ["CREATE", "UPDATE"], pods, "Namespaced"),
          ([""], ["v1"], ["CREATE"], ["pods/binding", "bindings"], "Namespaced"),
          ([""], ["v1"], ["UPDATE", "DELETE"], ["nodes"], "Cluster")]),
    ):
        if native and hook == "sandbox-placement":
            continue
        webhook = {
            "name": hook + ".cetirun.microsoft.com", "failurePolicy": "Fail", "sideEffects": "None",
            "matchPolicy": "Equivalent", "admissionReviewVersions": ["v1"], "timeoutSeconds": 5,
            "clientConfig": {"caBundle": CA, "service": {
                "name": "cetirun-sandbox-admission", "namespace": NAMESPACE, "port": 443, "path": "/" + route,
            }},
            "rules": [dict(zip(("apiGroups", "apiVersions", "operations", "resources", "scope"), rule)) for rule in rules],
            "matchConditions": [cleanup_condition(hook in ("sandbox-pods", "sandbox-placement"))],
        }
        if resource == "mutatingwebhookconfigurations":
            webhook["reinvocationPolicy"] = "Never"
        if name == "cetirun-sandbox-pods":
            if planned_migration:
                webhook["rules"].insert(1, copy.deepcopy(BINDING_RULE))
            webhook["namespaceSelector"] = {
                "matchExpressions": [{"key": "cetirun.microsoft.com/sandbox-protected", "operator": "Exists"}],
            }
        put(ADMISSION + resource + "/" + name, kind, name, webhooks=[webhook])
    return objects


class KubectlDouble:
    def __init__(self, *, native=False, planned_migration=False):
        self.responses = fixture(native=native, planned_migration=planned_migration)
        self.calls = []
        self.counts = {}
        self.processes = []
        self.scripts = {}
        self.change = lambda key, count, value: value

    def spawn(self, command, **options):
        assert command[:7] == [
            "kubectl", "--kubeconfig", "/example/explicit-config", "--context", "selected-context",
            "--request-timeout=5s", "--v=0",
        ], command
        assert options["stdin"] == subprocess.DEVNULL
        assert options["stdout"] == subprocess.PIPE and options["stderr"] == subprocess.DEVNULL
        assert options["start_new_session"] is True and options["env"]["KUBECTL_KUBERC"] == "false"
        if command[7:] == ["config", "view", "--minify", "-o", "json"]:
            key = "config"
        else:
            assert len(command) == 10 and command[7:9] == ["get", "--raw"], command
            key = command[9]
        assert key in self.responses, key
        self.calls.append(command)
        self.counts[key] = self.counts.get(key, 0) + 1
        value = self.change(key, self.counts[key], copy.deepcopy(self.responses[key]))
        raw = value if isinstance(value, str) else json.dumps(value)
        script = self.scripts.get(key, "import sys;sys.stdout.write(sys.argv[1])")
        process = REAL_POPEN([sys.executable, "-B", "-c", script, raw], **options)
        self.processes.append(process)
        return process


class DeploymentPreflightTests(unittest.TestCase):
    def test_aks_selector_exclusions_require_explicit_opt_in_and_exact_clauses(self):
        required = {"matchExpressions": [{"key": "cetirun.microsoft.com/sandbox-protected", "operator": "Exists"}]}
        additions = [
            {"key": "control-plane", "operator": "NotIn", "values": ["true"]},
            {"key": "kubernetes.azure.com/managedby", "operator": "NotIn", "values": ["aks"]},
        ]
        for expected in ({}, required):
            clauses = copy.deepcopy(expected.get("matchExpressions", []) + additions)
            actual = {"matchExpressions": clauses}
            self.assertFalse(qualify.webhook_namespace_selector_matches(actual, expected))
            self.assertTrue(qualify.webhook_namespace_selector_matches(actual, expected, True))
            self.assertTrue(qualify.webhook_namespace_selector_matches({"matchExpressions": list(reversed(clauses))}, expected, True))
            invalid = [
                None, {"matchLabels": {"skip": "true"}}, {"matchExpressions": []},
                {"matchExpressions": clauses[:-1]}, {"matchExpressions": clauses + [clauses[-1]]},
                {"matchExpressions": clauses, "matchLabels": {}},
            ]
            for field, value in (("key", "tenant-controlled"), ("operator", "In"), ("values", ["aks", "extra"])):
                changed = copy.deepcopy(clauses)
                changed[-1][field] = value
                invalid.append({"matchExpressions": changed})
            for selector in invalid:
                with self.subTest(expected=expected, selector=selector):
                    self.assertFalse(qualify.webhook_namespace_selector_matches(selector, expected, True))
        self.assertTrue(qualify.webhook_namespace_selector_matches(required, required))

    def test_aks_selector_option_does_not_apply_to_other_modes(self):
        with contextlib.redirect_stdout(io.StringIO()) as output:
            code = qualify.main(["preflight", "--kubeconfig", "/example/config", "--context", "example",
                                 "--cluster-uid", "example-uid", "--allow-aks-system-exclusions"])
        self.assertEqual(code, 2)
        self.assertIn("requires deployment-preflight", json.loads(output.getvalue())["error"])

    def test_exact_aks_exclusions_pass_only_opted_in_complete_preflight(self):
        for native in (False, True):
            for enabled in (False, True):
                with self.subTest(native=native, enabled=enabled):
                    double = KubectlDouble(native=native)
                    for value in double.responses.values():
                        for hook in value.get("webhooks", []):
                            selector = hook.setdefault("namespaceSelector", {})
                            selector.setdefault("matchExpressions", []).extend([
                                {"key": "control-plane", "operator": "NotIn", "values": ["true"]},
                                {"key": "kubernetes.azure.com/managedby", "operator": "NotIn", "values": ["aks"]},
                            ])
                    args = arguments() + (["--allow-aks-system-exclusions"] if enabled else [])
                    code, result, _ = self.run_preflight(double, args)
                    self.assertEqual(code, 0 if enabled else 1)
                    if enabled:
                        self.assertTrue(all(c["status"] == "observed" for c in result["checks"]))
                        self.assertEqual(result["executionReadiness"], "unknown")
                    else:
                        self.assertIn("webhook-resource-wiring-mismatch", [c["reason"] for c in result["checks"]])

    def run_preflight(self, double=None, args=None):
        double = double or KubectlDouble()
        with patch.object(qualify.subprocess, "Popen", side_effect=double.spawn), \
             contextlib.redirect_stdout(io.StringIO()) as stdout, contextlib.redirect_stderr(io.StringIO()) as stderr:
            code = qualify.main(arguments() if args is None else args)
        self.assertEqual(stderr.getvalue(), "")
        text = stdout.getvalue()
        self.assertLess(len(text.encode()), 4096)
        self.assertNotIn(SENTINEL, text)
        self.assertTrue(all(p.poll() is not None for p in double.processes), "subprocess was not reaped")
        result = json.loads(text)
        self.assertIs(result["qualifiesExecution"], False)
        return code, result, double

    def assert_blocked(self, double, reason=None):
        code, result, double = self.run_preflight(double)
        self.assertEqual(code, 1)
        self.assertEqual(result["status"], "blocked")
        if reason:
            self.assertIn(reason, [check["reason"] for check in result["checks"]])
        return result

    def update_configuration(self, double, path, update):
        name = "CETIRUN_SANDBOX_SOURCES" if path == OPERATOR else "CETIRUN_SANDBOX_ADMISSION"
        env = double.responses[path]["spec"]["template"]["spec"]["containers"][0]["env"]
        entry = next(e for e in env if e["name"] == name)
        value = json.loads(entry["value"])
        update(value)
        entry["value"] = json.dumps(value)
        if path == SERVER:
            for pod in ("admission-a", "admission-b"):
                env = double.responses[CORE + "pods/" + pod]["spec"]["containers"][0]["env"]
                next(e for e in env if e["name"] == name)["value"] = entry["value"]

    def test_exact_target_and_named_gets_complete_only_scoped_observations(self):
        code, result, double = self.run_preflight()
        self.assertEqual(code, 0)
        self.assertEqual(result["status"], "completed")
        self.assertEqual(result["executionReadiness"], "unknown")
        self.assertEqual(result["stockExecutionHold"], "writer-exclusion-unavailable")
        self.assertEqual(set(result), {"version", "mode", "status", "qualifiesExecution", "executionReadiness",
                                       "stockExecutionHold", "checks", "unverified"})
        self.assertEqual(len(result["checks"]), 11)
        self.assertEqual(len(result["unverified"]), 5)
        self.assertTrue(all(check["status"] == "observed" for check in result["checks"]))
        self.assertTrue(all(check["status"] == "unknown" for check in result["unverified"]))
        self.assertEqual(set(double.counts), set(double.responses))
        self.assertTrue(all(count >= 2 for count in double.counts.values()))
        self.assertLessEqual(len(double.calls), qualify.DeploymentKubernetes.MAX_COMMANDS)
        self.assertEqual(double.calls[1][-1], CLUSTER)
        self.assertEqual(double.calls[2][-1], "/version")
        output = json.dumps(result)
        for value in (NAMESPACE, IMAGE, SERVER_IMAGE, "192.0.2.", "admin@example.com", "selected-context", "cluster-uid", CA):
            self.assertNotIn(value, output)

    def test_native_profile_is_scoped_without_claiming_execution_or_skill_selection(self):
        code, result, double = self.run_preflight(KubectlDouble(native=True))
        self.assertEqual(code, 0)
        self.assertEqual(result["status"], "completed")
        self.assertEqual(result["executionReadiness"], "unknown")
        self.assertEqual(result["stockExecutionHold"], "native-single-start-unverified")
        self.assertEqual(set(result), {"version", "mode", "status", "qualifiesExecution", "executionReadiness",
                                       "stockExecutionHold", "checks", "unverified"})
        self.assertEqual(len(result["checks"]), 11)
        self.assertTrue(all(check["status"] == "observed" for check in result["checks"]))
        self.assertEqual(len(result["unverified"]), 6)
        self.assertTrue(all(check["status"] == "unknown" for check in result["unverified"]))
        self.assertIn({"check": "native-bootstrap-gateway-and-effective-skill-selection", "status": "unknown"},
                      result["unverified"])
        self.assertEqual(set(double.counts), set(double.responses))
        self.assertTrue(all(count >= 2 for count in double.counts.values()))
        self.assertLessEqual(len(double.calls), qualify.DeploymentKubernetes.MAX_COMMANDS)
        self.assertNotIn(PLACEMENT, double.counts)
        self.assertNotIn(ADMISSION + "validatingwebhookconfigurations/cetirun-sandbox-placement", double.counts)
        output = json.dumps(result)
        for value in (NAMESPACE, IMAGE, SERVER_IMAGE, "192.0.2.", "admin@example.com", "selected-context", "cluster-uid", CA):
            self.assertNotIn(value, output)

    def test_planned_migration_requires_both_binding_apis(self):
        for resources in (["pods/binding", "bindings"], ["pods/binding"], ["bindings"], []):
            with self.subTest(resources=resources):
                double = KubectlDouble(native=True, planned_migration=True)
                rules = double.responses[POD_WEBHOOK]["webhooks"][0]["rules"]
                if resources:
                    rules[1]["resources"] = resources
                else:
                    rules.pop(1)
                code, result, _ = self.run_preflight(double)
                self.assertEqual(code, 0 if len(resources) == 2 else 1)
                self.assertEqual(result["executionReadiness"], "unknown")

    def test_non_migration_supports_only_exact_legacy_and_guarded_pod_rules(self):
        for native in (False, True):
            for bindings in (False, True):
                with self.subTest(native=native, bindings=bindings):
                    double = KubectlDouble(native=native)
                    if bindings:
                        double.responses[POD_WEBHOOK]["webhooks"][0]["rules"].insert(1, copy.deepcopy(BINDING_RULE))
                    self.assertEqual(self.run_preflight(double)[0], 0)
        for mutate in (
            lambda rules: rules[1]["resources"].append("secrets"),
            lambda rules: rules[1]["operations"].append("UPDATE"),
            lambda rules: rules.append(copy.deepcopy(rules[1])),
        ):
            double = KubectlDouble(native=True, planned_migration=True)
            mutate(double.responses[POD_WEBHOOK]["webhooks"][0]["rules"])
            self.assert_blocked(double, "webhook-resource-wiring-mismatch")

    def test_planned_migration_flag_must_be_boolean_and_native(self):
        for native, value in ((True, "true"), (True, 1), (True, None), (False, True)):
            with self.subTest(native=native, value=value):
                double = KubectlDouble(native=native)
                self.update_configuration(double, OPERATOR, lambda c: c.update(plannedMigration=value))
                self.assert_blocked(double, "planned-migration-mode-mismatch")

    def test_cleanup_conditions_are_exact_public_predicates(self):
        paths = (
            ADMISSION + "validatingwebhookconfigurations/cetirun-sandbox-admission",
            ADMISSION + "mutatingwebhookconfigurations/cetirun-sandbox-admission",
            POD_WEBHOOK, ADMISSION + "validatingwebhookconfigurations/cetirun-sandbox-placement",
        )
        for path in paths:
            for mutation in (
                lambda conditions: conditions[0].update(expression="false"),
                lambda conditions: conditions[0].update(name="unreviewed-condition"),
                lambda conditions: conditions.append(copy.deepcopy(conditions[0])),
            ):
                with self.subTest(path=path, mutation=mutation):
                    double = KubectlDouble()
                    mutation(double.responses[path]["webhooks"][0]["matchConditions"])
                    self.assert_blocked(double, "webhook-match-conditions-mismatch")
        for replacement in ("system:serviceaccount:other:controller", "CONTROLLER"):
            double = KubectlDouble()
            condition = double.responses[paths[0]]["webhooks"][0]["matchConditions"][0]
            condition["expression"] = condition["expression"].replace(
                f"system:serviceaccount:{NAMESPACE}:selected-controller-sa", replacement)
            self.assert_blocked(double, "webhook-match-conditions-mismatch")

    def test_legacy_condition_absence_does_not_authorize_guarded_migration(self):
        double = KubectlDouble(native=True)
        for value in double.responses.values():
            for hook in value.get("webhooks", []):
                hook.pop("matchConditions")
        self.assertEqual(self.run_preflight(double)[0], 0)
        for path in (ADMISSION + "validatingwebhookconfigurations/cetirun-sandbox-admission",
                     ADMISSION + "mutatingwebhookconfigurations/cetirun-sandbox-admission", POD_WEBHOOK):
            with self.subTest(path=path):
                double = KubectlDouble(native=True, planned_migration=True)
                double.responses[path]["webhooks"][0].pop("matchConditions")
                self.assert_blocked(double, "webhook-match-conditions-mismatch")
        double = KubectlDouble(native=True)
        hook = double.responses[POD_WEBHOOK]["webhooks"][0]
        hook["rules"].insert(1, copy.deepcopy(BINDING_RULE))
        hook["matchConditions"] = []
        self.assert_blocked(double, "webhook-match-conditions-mismatch")

    def test_migration_keeps_aks_selector_opt_in_narrow(self):
        for enabled, complete in ((False, True), (True, True), (True, False)):
            with self.subTest(enabled=enabled, complete=complete):
                double = KubectlDouble(native=True, planned_migration=True)
                for value in double.responses.values():
                    for hook in value.get("webhooks", []):
                        hook.setdefault("namespaceSelector", {}).setdefault("matchExpressions", []).extend([
                            {"key": "control-plane", "operator": "NotIn", "values": ["true"]},
                            {"key": "kubernetes.azure.com/managedby", "operator": "NotIn", "values": ["aks"]},
                        ])
                if not complete:
                    double.responses[POD_WEBHOOK]["webhooks"][0]["rules"][1]["resources"].pop()
                code, _, _ = self.run_preflight(
                    double, arguments() + (["--allow-aks-system-exclusions"] if enabled else []))
                self.assertEqual(code, 0 if enabled and complete else 1)

    def test_native_optional_bindings_and_configured_skills_are_not_selection_evidence(self):
        for skills, catalog in (("", ""), ("skills", ""), ("skills", "selected-skills-catalog")):
            with self.subTest(skills=skills, catalog=catalog):
                double = KubectlDouble(native=True)
                self.update_configuration(double, OPERATOR, lambda c: c.update(
                    skillsClaimName=skills, skillsCatalogConfigMapName=catalog, placementConfigMapName=""))
                self.update_configuration(double, SERVER, lambda c: c.update(placementConfigMapName=""))
                code, result, double = self.run_preflight(double)
                self.assertEqual(code, 0)
                self.assertEqual(result["executionReadiness"], "unknown")
                self.assertIn({"check": "native-bootstrap-gateway-and-effective-skill-selection", "status": "unknown"},
                              result["unverified"])
                self.assertEqual(set(double.counts), set(double.responses))

    def test_native_execution_flags_must_be_explicit_matching_booleans(self):
        for path in (OPERATOR, SERVER):
            for value in (False, None, "true", "false", 1, 0, [], {}):
                with self.subTest(path=path, value=value):
                    double = KubectlDouble(native=True)
                    self.update_configuration(double, path, lambda c: c.update(nativeExecution=value))
                    self.assert_blocked(double, "native-execution-mode-mismatch")
                    self.assertNotIn(PLATFORM, double.counts)
            double = KubectlDouble(native=True)
            self.update_configuration(double, path, lambda c: c.pop("nativeExecution"))
            self.assert_blocked(double, "native-execution-mode-mismatch")
        for value in ("true", "false", 1, 0, None):
            with self.subTest(matching_invalid=value):
                double = KubectlDouble()
                for path in (OPERATOR, SERVER):
                    self.update_configuration(double, path, lambda c: c.update(nativeExecution=value))
                self.assert_blocked(double, "native-execution-mode-mismatch")

    def test_native_requires_owned_bootstrap_and_no_desktop_helper(self):
        for value in (False, None, "true", 1):
            with self.subTest(bootstrap=value):
                double = KubectlDouble(native=True)
                self.update_configuration(double, OPERATOR, lambda c: c.update(bootstrapOwnedPrerequisites=value))
                self.assert_blocked(double, "native-owned-bootstrap-required")
                self.assertNotIn(PLATFORM, double.counts)
        double = KubectlDouble(native=True)
        self.update_configuration(double, OPERATOR, lambda c: c.pop("bootstrapOwnedPrerequisites"))
        self.assert_blocked(double, "native-owned-bootstrap-required")
        for value in (True, None, "false", 0):
            with self.subTest(desktop=value):
                double = KubectlDouble(native=True)
                self.update_configuration(double, OPERATOR, lambda c: c["helpers"].update(desktop=value))
                self.assert_blocked(double, "native-desktop-helper-unsupported")

    def test_native_requires_explicit_inline_gateway_enablement(self):
        for value in ("false", "True", " true ", "", True, 1, None):
            with self.subTest(value=value):
                double = KubectlDouble(native=True)
                env = double.responses[OPERATOR]["spec"]["template"]["spec"]["containers"][0]["env"]
                next(e for e in env if e["name"] == "AI_GATEWAY_ENABLED")["value"] = value
                self.assert_blocked(double, "native-gateway-enablement-required")
                self.assertNotIn(PLATFORM, double.counts)
        for change in ("missing", "duplicate", "indirect"):
            with self.subTest(change=change):
                double = KubectlDouble(native=True)
                env = double.responses[OPERATOR]["spec"]["template"]["spec"]["containers"][0]["env"]
                entry = next(e for e in env if e["name"] == "AI_GATEWAY_ENABLED")
                if change == "missing":
                    env.remove(entry)
                elif change == "duplicate":
                    env.append(copy.deepcopy(entry))
                else:
                    entry["valueFrom"] = {"secretKeyRef": {"name": "never-read", "key": "enabled"}}
                self.assert_blocked(double)
                self.assertNotIn(PLATFORM, double.counts)
                self.assertFalse(any("/secrets/" in c[-1] for c in double.calls))

    def test_native_rejects_placement_in_either_process_before_followup_reads(self):
        for path in (OPERATOR, SERVER):
            for value in ("selected-placement", None, False, [], {}):
                with self.subTest(path=path, value=value):
                    double = KubectlDouble(native=True)
                    self.update_configuration(double, path, lambda c: c.update(placementConfigMapName=value))
                    self.assert_blocked(double, "native-placement-unsupported")
                    self.assertNotIn(PLATFORM, double.counts)
                    self.assertNotIn(SERVICE, double.counts)

    def test_native_claims_and_catalog_bindings_fail_closed(self):
        for value in (None, False, 1, "INVALID", "home", [], {}):
            with self.subTest(skills=value):
                double = KubectlDouble(native=True)
                self.update_configuration(double, OPERATOR, lambda c: c.update(skillsClaimName=value))
                self.assert_blocked(double, "distinct-source-claims-required")
        double = KubectlDouble(native=True)
        self.update_configuration(double, OPERATOR, lambda c: c.pop("skillsClaimName"))
        self.assert_blocked(double, "distinct-source-claims-required")
        double = KubectlDouble(native=True)
        self.update_configuration(double, OPERATOR, lambda c: c.update(skillsCatalogConfigMapName="selected-catalog"))
        self.assert_blocked(double, "distinct-source-claims-required")
        for value in (None, False, 1, "INVALID", "selected-platform", [], {}):
            with self.subTest(catalog=value):
                double = KubectlDouble(native=True)
                self.update_configuration(double, OPERATOR, lambda c: c.update(
                    skillsClaimName="skills", skillsCatalogConfigMapName=value))
                self.assert_blocked(double, "native-skills-catalog-binding-invalid")
                self.assertNotIn(PLATFORM, double.counts)

    def test_explicitly_disabled_native_mode_keeps_the_held_path(self):
        double = KubectlDouble()
        for path in (OPERATOR, SERVER):
            self.update_configuration(double, path, lambda c: c.update(nativeExecution=False))
        code, result, double = self.run_preflight(double)
        self.assertEqual(code, 0)
        self.assertEqual(result["stockExecutionHold"], "writer-exclusion-unavailable")
        self.assertEqual(len(result["unverified"]), 5)
        self.assertEqual(set(double.counts), set(double.responses))
        for path, update, reason in (
            (OPERATOR, {"skillsClaimName": ""}, "distinct-source-claims-required"),
            (OPERATOR, {"placementConfigMapName": ""}, "platform-placement-bindings-required"),
            (SERVER, {"placementConfigMapName": ""}, "platform-placement-bindings-required"),
        ):
            with self.subTest(path=path, update=update):
                double = KubectlDouble()
                self.update_configuration(double, path, lambda c: c.update(update))
                self.assert_blocked(double, reason)

    def test_native_profile_still_requires_platform_common_webhooks_and_stable_reads(self):
        for update, reason in (
            ({"platformConfigMapName": "other-platform"}, "platform-bindings-required"),
            ({"homeClaimName": ""}, "distinct-source-claims-required"),
        ):
            double = KubectlDouble(native=True)
            self.update_configuration(double, OPERATOR, lambda c: c.update(update))
            self.assert_blocked(double, reason)
        double = KubectlDouble(native=True)
        double.responses[PLATFORM]["data"]["platform.json"] = '{"version":"v2"}'
        self.assert_blocked(double, "source-document-version-mismatch")
        for path in (
            ADMISSION + "validatingwebhookconfigurations/cetirun-sandbox-admission",
            ADMISSION + "mutatingwebhookconfigurations/cetirun-sandbox-admission",
            ADMISSION + "validatingwebhookconfigurations/cetirun-sandbox-pods",
        ):
            with self.subTest(webhook=path):
                double = KubectlDouble(native=True)
                double.responses[path]["webhooks"][0]["failurePolicy"] = "Ignore"
                self.assert_blocked(double, "webhook-fail-closed-wiring-mismatch")
        for selected in (OPERATOR, SERVER, PLATFORM, POD):
            with self.subTest(drift=selected):
                double = KubectlDouble(native=True)

                def change(key, count, value):
                    if key == selected and count > 1:
                        value["metadata"]["resourceVersion"] = "changed"
                    return value

                double.change = change
                self.assert_blocked(double, "resource-version-drift")

    def test_every_input_pin_is_mandatory_before_any_command(self):
        args = arguments()
        for index in range(1, len(args), 2):
            with self.subTest(option=args[index]):
                code, result, double = self.run_preflight(args=args[:index] + args[index + 2:])
                self.assertIn(code, (1, 2))
                self.assertEqual(result["status"], "blocked")
                self.assertEqual(double.calls, [])

    def test_invalid_arguments_and_mutation_options_are_sanitized_without_commands(self):
        for extra in (["--allow-test-resources"], ["--probe-image", IMAGE], ["--storage-class", "home"],
                      ["--runtime-class", "kata"], ["--execution-ready", SENTINEL]):
            with self.subTest(extra=extra):
                code, _, double = self.run_preflight(args=arguments() + extra)
                self.assertIn(code, (1, 2))
                self.assertEqual(double.calls, [])
        for option, value in (("--kubeconfig", "relative-config"), ("--namespace", "../other"),
                              ("--operator-image", "registry.example.com/operator:latest"),
                              ("--operator-service-account", "selected-admission-sa")):
            args = arguments()
            args[args.index(option) + 1] = value
            code, _, double = self.run_preflight(args=args)
            self.assertEqual(code, 1)
            self.assertEqual(double.calls, [])

    def test_auth_plugins_are_rejected_before_any_cluster_request(self):
        for key in ("exec", "auth-provider", "tokenFile", "as", "as-groups"):
            double = KubectlDouble()
            double.responses["config"]["users"][0]["user"][key] = SENTINEL
            self.assert_blocked(double, "credential-plugin-or-indirection-not-supported")
            self.assertEqual(set(double.counts), {"config"})

    def test_context_binding_and_https_must_match(self):
        for change in (
            lambda c: c.update({"current-context": SENTINEL}),
            lambda c: c["contexts"][0]["context"].update(user="foreign"),
            lambda c: c["clusters"][0]["cluster"].update({"insecure-skip-tls-verify": True}),
            lambda c: c["clusters"][0]["cluster"].update(server="http://cluster.example.com"),
        ):
            double = KubectlDouble()
            change(double.responses["config"])
            self.assert_blocked(double)
            self.assertEqual(set(double.counts), {"config"})

    def test_wrong_cluster_fails_before_version_or_workload_reads(self):
        double = KubectlDouble()
        double.responses[CLUSTER]["metadata"]["uid"] = "other-cluster"
        self.assert_blocked(double, "resource-uid-mismatch")
        self.assertEqual(set(double.counts), {"config", CLUSTER})

    def test_version_minimum_is_not_a_runtime_qualification(self):
        for version in ("v1.36.9", "v1.37.0-alpha.1", SENTINEL, None, {}, 137):
            double = KubectlDouble()
            double.responses["/version"]["gitVersion"] = version
            self.assert_blocked(double, "stable-kubernetes-1.37-or-newer-required")
            self.assertEqual(set(double.counts), {"config", CLUSTER, "/version"})
        double = KubectlDouble()
        double.responses["/version"]["gitVersion"] = "v1.38.1+vendor.1"
        self.assertEqual(self.run_preflight(double)[0], 0)

    def test_deployment_identity_image_mode_and_rollout_fail_closed(self):
        changes = (
            lambda d: d["metadata"].update(uid=SENTINEL),
            lambda d: d["metadata"].update(deletionTimestamp="2026-01-01T00:00:00Z"),
            lambda d: d["status"].update(observedGeneration=2),
            lambda d: d["status"].update(readyReplicas=1),
            lambda d: d["spec"].update(replicas=1),
            lambda d: d["spec"]["template"]["spec"].update(serviceAccountName="foreign"),
            lambda d: d["spec"]["template"]["spec"]["containers"][0].update(image=IMAGE),
            lambda d: d["spec"]["template"]["spec"]["containers"][0].update(command=["foreign"]),
            lambda d: d["spec"]["template"]["spec"]["containers"][0].update(envFrom=[{"secretRef": {"name": "never-read"}}]),
            lambda d: d["spec"]["template"]["spec"]["containers"][0]["args"].append("--mode=operator"),
        )
        for index, change in enumerate(changes):
            with self.subTest(change=index):
                double = KubectlDouble()
                change(double.responses[SERVER])
                self.assert_blocked(double)
                self.assertNotIn(SERVICE, double.counts)
        double = KubectlDouble()
        double.responses[CORE + "serviceaccounts/selected-admission-sa"]["metadata"]["uid"] = "replacement"
        self.assert_blocked(double, "resource-uid-mismatch")
        double = KubectlDouble()
        double.responses[OPERATOR]["status"]["readyReplicas"] = True
        self.assert_blocked(double, "deployment-not-fully-observed")

    def test_source_bindings_do_not_dereference_secrets_or_unknown_inputs(self):
        for path, env_name, update in (
            (OPERATOR, "CETIRUN_SANDBOX_SOURCES", {"enabled": False}),
            (OPERATOR, "CETIRUN_SANDBOX_SOURCES", {"placementConfigMapName": "other-placement"}),
            (SERVER, "CETIRUN_SANDBOX_ADMISSION", {"controllerUsername": SENTINEL}),
            (SERVER, "CETIRUN_SANDBOX_ADMISSION", {"platformNamespace": "other-namespace"}),
            (SERVER, "CETIRUN_SANDBOX_ADMISSION", {"validationMode": "compatible"}),
            (SERVER, "CETIRUN_SANDBOX_ADMISSION", {"adminGroups": ["system:authenticated"]}),
        ):
            with self.subTest(update=update):
                double = KubectlDouble()
                env = double.responses[path]["spec"]["template"]["spec"]["containers"][0]["env"]
                entry = next(e for e in env if e["name"] == env_name)
                value = json.loads(entry["value"])
                entry["value"] = json.dumps({**value, **update})
                self.assert_blocked(double)
                self.assertNotIn(SERVICE, double.counts)
        double = KubectlDouble()
        entry = double.responses[SERVER]["spec"]["template"]["spec"]["containers"][0]["env"][0]
        entry.pop("value")
        entry["valueFrom"] = {"secretKeyRef": {"name": "never-read", "key": "config"}}
        result = self.assert_blocked(double, "inline-configuration-required")
        self.assertEqual(next(c for c in result["checks"] if c["check"] == "source-bindings")["status"], "unknown")
        self.assertFalse(any("/secrets/" in c[-1] for c in double.calls))

    def test_source_document_versions_and_shapes_are_not_silently_accepted(self):
        for document in ('{"version":"v2"}', '{"version":"v1"}', '{"version":"v1","version":"v1"}',
                         "[]", "{", None, "\ud800"):
            double = KubectlDouble()
            double.responses[PLATFORM]["data"]["platform.json"] = document
            self.assert_blocked(double)
            self.assertNotIn(SERVICE, double.counts)

    def test_service_selectors_listener_probe_and_tls_references_are_checked(self):
        for path, change in (
            (SERVICE, lambda d: d["spec"].update(selector={"app": "foreign"})),
            (SERVICE, lambda d: d["spec"]["ports"][0].update(targetPort=8443)),
            (SERVICE, lambda d: d["spec"].update(type="ExternalName", externalName=SENTINEL)),
            (SERVER, lambda d: d["spec"]["template"]["spec"]["containers"][0]["readinessProbe"]["httpGet"].update(path="/not-ready")),
            (SERVER, lambda d: d["spec"]["template"]["spec"]["volumes"][0]["secret"].update(optional=True)),
        ):
            double = KubectlDouble()
            change(double.responses[path])
            self.assert_blocked(double)
            self.assertNotIn(ENDPOINTS, double.counts)

    def test_serving_endpoints_must_be_bounded_and_owned_by_selected_deployment(self):
        changes = (
            (ENDPOINTS, lambda d: d["subsets"][0]["addresses"].extend([copy.deepcopy(d["subsets"][0]["addresses"][0])] * 3)),
            (ENDPOINTS, lambda d: d["subsets"][0].update(notReadyAddresses=[{"ip": "192.0.2.9"}])),
            (ENDPOINTS, lambda d: d["subsets"][0]["addresses"][0]["targetRef"].update(namespace="other")),
            (POD, lambda d: d["metadata"].update(uid="replaced")),
            (POD, lambda d: d["spec"].update(nodeName="node-b")),
            (POD, lambda d: d["spec"]["containers"][0].update(image=IMAGE)),
            (POD, lambda d: d["status"]["conditions"][0].update(status="False")),
            (POD, lambda d: d["status"].update(podIPs=[{"ip": "192.0.2.200"}])),
            (REPLICA_SET, lambda d: d["metadata"]["ownerReferences"][0].update(uid="foreign-deployment")),
        )
        for index, (path, change) in enumerate(changes):
            with self.subTest(change=index):
                double = KubectlDouble()
                change(double.responses[path])
                self.assert_blocked(double)
                self.assertFalse(any("webhookconfigurations/" in c[-1] for c in double.calls))

    def test_webhook_wiring_errors_are_blockers_but_cel_evaluation_stays_unknown(self):
        path = ADMISSION + "validatingwebhookconfigurations/cetirun-sandbox-placement"
        for update in (
            lambda h: h.update(failurePolicy="Ignore"),
            lambda h: h.update(timeoutSeconds=4),
            lambda h: h.update(rules=[]),
            lambda h: h.update(namespaceSelector={"matchLabels": {"skip": "true"}}),
            lambda h: h["clientConfig"]["service"].update(namespace="foreign"),
            lambda h: h["clientConfig"].update(url="https://external.example.com"),
            lambda h: h["clientConfig"].update(caBundle=SENTINEL),
        ):
            double = KubectlDouble()
            update(double.responses[path]["webhooks"][0])
            self.assert_blocked(double)
        double = KubectlDouble()
        double.responses[path]["webhooks"][0]["matchConditions"][0]["expression"] = "false"
        result = self.assert_blocked(double, "webhook-match-conditions-mismatch")
        self.assertIn({"check": "webhook-match-conditions-and-namespace-policy", "status": "unknown"}, result["unverified"])
        self.assertEqual(result["executionReadiness"], "unknown")

    def test_source_capability_booleans_cannot_become_execution_evidence(self):
        double = KubectlDouble()
        platform = json.loads(double.responses[PLATFORM]["data"]["platform.json"])
        platform["registrations"]["kata"] = {"Available": True, "Capabilities": {"MicroVMIsolation": True}}
        double.responses[PLATFORM]["data"]["platform.json"] = json.dumps(platform)
        code, result, _ = self.run_preflight(double)
        self.assertEqual(code, 0)
        self.assertIs(result["qualifiesExecution"], False)
        self.assertEqual(result["executionReadiness"], "unknown")
        self.assertIn({"check": "runtime-network-storage-and-provider-evidence", "status": "unknown"}, result["unverified"])

    def test_second_reads_detect_uid_resource_version_and_api_drift(self):
        for selected, field in ((OPERATOR, "uid"), (SERVER, "resourceVersion"), (POD, "resourceVersion"),
                                (PLACEMENT, "resourceVersion"), (ENDPOINTS, "uid"), (CLUSTER, "uid")):
            double = KubectlDouble()

            def change(key, count, value):
                if key == selected and count > 1:
                    value["metadata"][field] = "changed"
                return value

            double.change = change
            self.assert_blocked(double)
        for selected in ("config", "/version"):
            double = KubectlDouble()

            def change(key, count, value):
                if key == selected and count > 1:
                    value["changed"] = SENTINEL
                return value

            double.change = change
            self.assert_blocked(double, "kubeconfig-drift" if selected == "config" else "api-version-drift")

    def test_get_allowlist_cannot_be_used_for_discovery_or_other_methods(self):
        kube = qualify.DeploymentKubernetes("/example/explicit-config", "selected-context", "cluster-uid")
        with patch.object(qualify.subprocess, "Popen") as popen:
            for command in (
                ["get", "pods", "-A"], ["get", "--raw", "/api/v1/namespaces/preflight-test/secrets/example"],
                ["get", "--raw", "/api"], ["get", "--raw", "/version?x=1"], ["create", "-f", "-"],
                ["exec", "pod"], ["attach", "pod"], ["port-forward", "pod"], ["delete", "pod"],
            ):
                with self.assertRaises(qualify.PreflightBlocked):
                    kube.run(command)
            for resource in ("secret", "node", "agent", "runtimeclass", "storageclass"):
                with self.assertRaises(qualify.PreflightBlocked):
                    kube.get(resource, "example")
            with self.assertRaises(qualify.PreflightBlocked):
                kube.get("namespace", "unrelated")
            with self.assertRaises(qualify.PreflightBlocked):
                kube.get("pod", "../escape", NAMESPACE)
            popen.assert_not_called()

    def test_command_errors_and_malformed_bodies_never_escape_to_output(self):
        for script in (
            f"import sys;sys.stdout.write('{SENTINEL}');sys.stderr.write('{SENTINEL}');sys.exit(1)",
            f"print('{SENTINEL}')",
            "print('[]')",
            "print('{\"metadata\": []}')",
            "import sys;sys.stdout.buffer.write(b'\\xff')",
        ):
            double = KubectlDouble()
            double.scripts[CLUSTER] = script
            self.assert_blocked(double)
        with patch.object(qualify.subprocess, "Popen", side_effect=OSError(SENTINEL)), \
             contextlib.redirect_stdout(io.StringIO()) as stdout:
            self.assertEqual(qualify.main(arguments()), 1)
        self.assertNotIn(SENTINEL, stdout.getvalue())

    def test_response_cap_includes_exact_boundary_and_reaps_oversized_writer(self):
        for extra in (0, 1):
            double = KubectlDouble()
            double.scripts["config"] = (
                f"import sys;sys.stdout.write('x'*{qualify.DeploymentKubernetes.MAX_BYTES + extra})"
            )
            kube = qualify.DeploymentKubernetes("/example/explicit-config", "selected-context", "cluster-uid")
            with patch.object(qualify.subprocess, "Popen", side_effect=double.spawn):
                if extra:
                    with self.assertRaisesRegex(qualify.PreflightBlocked, "response-too-large"):
                        kube.run(kube.CONFIG_COMMAND)
                else:
                    self.assertEqual(len(kube.run(kube.CONFIG_COMMAND)), kube.MAX_BYTES)
            self.assertTrue(all(p.poll() is not None for p in double.processes))

    def test_command_count_and_total_deadline_are_hard_limits(self):
        with patch.object(qualify.DeploymentKubernetes, "MAX_COMMANDS", 0):
            result = self.assert_blocked(KubectlDouble(), "command-budget-exhausted")
            self.assertEqual(result["checks"][1]["status"], "unknown")
        kube = qualify.DeploymentKubernetes("/example/explicit-config", "selected-context", "cluster-uid")
        kube.deadline = time.monotonic() - 1
        with patch.object(qualify.subprocess, "Popen") as popen, self.assertRaisesRegex(
                qualify.PreflightBlocked, "deadline-exhausted"):
            kube.run(kube.CONFIG_COMMAND)
        popen.assert_not_called()

    def test_real_subprocess_timeout_is_bounded_and_reaped(self):
        double = KubectlDouble()
        double.scripts["config"] = "import time;time.sleep(60)"
        start = time.monotonic()
        with patch.object(qualify.DeploymentKubernetes, "COMMAND_SECONDS", 0.1):
            self.assert_blocked(double, "request-timeout")
        self.assertLess(time.monotonic() - start, 3)

    def test_interrupt_and_sigterm_cancel_without_followup_requests(self):
        previous = signal.getsignal(signal.SIGTERM)

        def terminate(_timeout):
            os.kill(os.getpid(), signal.SIGTERM)

        for cancellation in (KeyboardInterrupt, terminate):
            double = KubectlDouble()
            double.scripts["config"] = "import time;time.sleep(60)"
            with patch.object(qualify.selectors.DefaultSelector, "select", side_effect=cancellation):
                code, result, double = self.run_preflight(double)
            self.assertEqual(code, 130)
            self.assertEqual(result["checks"][1]["reason"], "cancelled")
            self.assertEqual(len(double.calls), 1)
            self.assertEqual(signal.getsignal(signal.SIGTERM), previous)


if __name__ == "__main__":
    unittest.main()
