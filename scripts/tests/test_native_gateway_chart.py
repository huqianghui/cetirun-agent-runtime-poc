# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Render the protected OpenClaw/Gateway packaging with synthetic inputs only."""

import copy
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
RUNTIME_CHART = ROOT / "app/cetirun-chart"
GATEWAY_CHART = ROOT / "app/cetirun-ai-gateway/chart"
OFFLINE = {
    **os.environ,
    "KUBECONFIG": os.devnull,
    "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}
MODEL = {
    "model_name": "example-model",
    "litellm_params": {
        "model": "openai/example-model",
        "api_base": "https://models.example.com/v1",
        "api_key": "os.environ/EXAMPLE_MODEL_KEY",
    },
    "model_info": {"input_cost_per_token": 0.001, "access_groups": ["example-readers"]},
}


def render(chart, values, *, error=None):
    result = subprocess.run(
        [
            "helm", "template", "native-test", str(chart),
            "--namespace", "example-agent", "--kube-version", "1.37.0",
            "--values", "-",
        ],
        input=json.dumps(values), text=True, capture_output=True,
        env=OFFLINE, timeout=30, check=False,
    )
    if error is not None:
        assert result.returncode != 0
        assert error in result.stderr, result.stderr
        return None
    assert result.returncode == 0, result.stderr
    return [document for document in yaml.safe_load_all(result.stdout) if document]


def object_of_kind(objects, kind):
    return next(document for document in objects if document["kind"] == kind)


def native_values():
    security = {
        "runAsUser": 1024, "runAsNonRoot": True,
        "readOnlyRootFilesystem": True, "allowPrivilegeEscalation": False,
        "capabilities": {"drop": ["ALL"]},
        "seccompProfile": {"type": "RuntimeDefault"},
    }
    resources = {
        "requests": {"cpu": "250m", "memory": "512Mi", "ephemeral-storage": "1Gi"},
        "limits": {"cpu": "1", "memory": "1Gi", "ephemeral-storage": "2Gi"},
    }
    return {
        "agentType": "openclaw",
        "replicaCount": 0,
        "runtimeClassName": "example-kata",
        "sandboxWorkload": {
            "contractVersion": "v1", "executionHeld": True,
            "podResources": {
                "requests": {"cpu": "250m", "memory": "512Mi"},
                "limits": {"cpu": "1", "memory": "1Gi"},
            },
            "helperResources": resources,
            "securityContext": security,
            "scratchSizeLimit": "2Gi",
            "ownerReferences": [{
                "apiVersion": "cetirun.microsoft.com/v1alpha1", "kind": "Agent",
                "name": "example-agent", "uid": "example-agent-uid", "controller": True,
            }],
            "podLabels": {"cetirun.microsoft.com/agent-uid": "example-agent-uid"},
            "dnsConfig": {"nameservers": ["192.0.2.53"]},
            "identityVolumes": [], "identityMounts": [], "identityEnvironment": [],
            "objects": [{
                "apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
                "metadata": {"name": "example-compiler-policy"},
                "spec": {
                    "endpointSelector": {
                        "matchLabels": {"cetirun.microsoft.com/agent-uid": "example-agent-uid"},
                    },
                    "ingress": [], "egress": [],
                },
            }],
        },
        "podAnnotations": {"sidecar.istio.io/inject": "false"},
        "sidecarScope": {"enabled": False},
        "serviceAccount": {"automountToken": False},
        "securityContext": security,
        "resources": resources,
        "persistence": {"existingClaim": "example-home", "useStatefulSetVolumeClaim": False},
        "skills": {"shared": True, "existingClaim": "example-skills"},
        "desktop": {"enabled": False},
        "ttyd": {"enabled": False},
        "logger": {"enabled": False},
        "execRestriction": {"enabled": False},
        "openclaw": {
            "image": {"repository": "registry.example.com/openclaw-gateway", "tag": "example"},
            "config": {
                "browser": {"enabled": False},
                "auth": {"profiles": {"openai:personal": {"provider": "openai"}}},
                "models": {"providers": {"other": {"baseUrl": "https://other.example.com"}}},
            },
        },
        "hermes": {"env": {"HERMES_WEBAPP_PUBLIC_URL": "https://example.com/ui/example-agent"}},
        "aiGateway": {
            "enabled": True, "keySecretName": "example-model-key",
            "models": [{"id": "example-model", "name": "Example model"}],
        },
        "networkPolicy": {"enabled": True, "gatewayNamespace": "example-platform"},
        "secrets": {
            "existingSecret": "example-gateway-token",
            "provider": "openai", "model": "example-model",
            "apiBase": "http://gateway.example-platform.svc.cluster.local:4000/v1",
        },
    }


def gateway_models(model, *, integrated, more=()):
    objects = render(GATEWAY_CHART, {"operatorIntegration": integrated, "models": [model, *more]})
    config = yaml.safe_load(object_of_kind(objects, "ConfigMap")["data"]["config.yaml"])
    return config["model_list"]


def gateway_general_settings(values):
    objects = render(GATEWAY_CHART, {"models": [MODEL], **values})
    config = yaml.safe_load(object_of_kind(objects, "ConfigMap")["data"]["config.yaml"])
    return config["general_settings"]


@unittest.skipUnless(shutil.which("helm"), "requires Helm 3 or 4")
class NativeGatewayChartTests(unittest.TestCase):
    def test_only_shared_openclaw_skills_enable_native_polling(self):
        for agent_type in ("openclaw", "hermes"):
            for shared in (False, True):
                for memory in (False, True):
                    with self.subTest(agent_type=agent_type, shared=shared, memory=memory):
                        values = native_values()
                        values.pop("sandboxWorkload")
                        values["sidecarScope"]["enabled"] = True
                        values["podAnnotations"]["sidecar.istio.io/inject"] = "true"
                        values["agentType"] = agent_type
                        values["skills"]["shared"] = shared
                        values["memoryContext"] = {"enabled": memory}
                        pod = object_of_kind(render(RUNTIME_CHART, values), "StatefulSet")["spec"]["template"]["spec"]
                        env = {item["name"]: item.get("value") for item in pod["containers"][0]["env"]}
                        if agent_type == "openclaw" and shared:
                            self.assertEqual(env.get("CHOKIDAR_USEPOLLING"), "true")
                            self.assertEqual(env.get("CHOKIDAR_INTERVAL"), "5000")
                        else:
                            self.assertNotIn("CHOKIDAR_USEPOLLING", env)
                            self.assertNotIn("CHOKIDAR_INTERVAL", env)

    def test_native_openclaw_avoids_ephemeral_node_compile_cache(self):
        for native in (True, False):
            with self.subTest(native=native):
                values = native_values()
                if not native:
                    values["sandboxWorkload"] = None
                    values["sidecarScope"]["enabled"] = True
                objects = render(RUNTIME_CHART, values)
                pod = object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]
                doctor = next(container for container in pod["initContainers"] if container["name"] == "doctor")
                for container in (pod["containers"][0], doctor):
                    env = {item["name"]: item.get("value") for item in container.get("env", [])}
                    if native:
                        self.assertEqual(env["NODE_DISABLE_COMPILE_CACHE"], "1")
                    else:
                        self.assertNotIn("NODE_DISABLE_COMPILE_CACHE", env)

    def test_hermes_webapp_receives_the_explicit_public_url(self):
        values = native_values()
        values["agentType"] = "hermes"
        values["hermes"] = {"env": {"HERMES_WEBAPP_PUBLIC_URL": "https://example.com/ui/example-agent"}}
        objects = render(RUNTIME_CHART, values)
        pod = object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]
        env = {item["name"]: item for item in pod["containers"][0]["env"]}
        self.assertEqual(env["HERMES_WEBAPP_PUBLIC_URL"], {
            "name": "HERMES_WEBAPP_PUBLIC_URL", "value": "https://example.com/ui/example-agent",
        })
        managed = yaml.safe_load(object_of_kind(objects, "ConfigMap")["data"]["hermes-managed.yaml"])
        self.assertEqual(managed["webapp"]["public_url"], "https://example.com/ui/example-agent")

    def test_hermes_rejects_an_empty_public_url(self):
        values = native_values()
        values["agentType"] = "hermes"
        values["hermes"]["env"]["HERMES_WEBAPP_PUBLIC_URL"] = ""
        render(RUNTIME_CHART, values, error="hermes.env.HERMES_WEBAPP_PUBLIC_URL is required for Hermes")

    def test_native_initializers_have_an_init_process_without_changing_their_work(self):
        values = native_values()
        objects = render(RUNTIME_CHART, values)
        pod = object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]
        expected = {
            "init-home-data": ["/bin/sh", "/etc/cetirun/init-home.sh"],
            "doctor": ["openclaw", "doctor", "--fix", "--non-interactive", "--yes"],
        }
        self.assertEqual({container["name"] for container in pod["initContainers"]}, set(expected))
        for container in pod["initContainers"]:
            with self.subTest(container=container["name"]):
                self.assertEqual(container["command"], ["/usr/bin/tini", "-g", "--", *expected[container["name"]]])
                self.assertFalse(container.get("args"))
                self.assertEqual(container["resources"], values["sandboxWorkload"]["helperResources"])
                self.assertEqual(container["securityContext"], values["sandboxWorkload"]["securityContext"])
        self.assertEqual(pod["resources"], values["sandboxWorkload"]["podResources"])
        self.assertEqual(pod["runtimeClassName"], values["runtimeClassName"])
        self.assertEqual(object_of_kind(objects, "StatefulSet")["spec"]["replicas"], 0)
        self.assertFalse(pod["automountServiceAccountToken"])

    def test_managed_gateway_auth_cache_uses_current_authorization(self):
        for integrated in (False, True):
            for replicas in (1, 2):
                with self.subTest(integrated=integrated, replicas=replicas):
                    objects = render(GATEWAY_CHART, {
                        "operatorIntegration": integrated,
                        "replicaCount": replicas,
                        "models": [MODEL],
                    })
                    config = yaml.safe_load(object_of_kind(objects, "ConfigMap")["data"]["config.yaml"])
                    settings = config["general_settings"]
                    if integrated:
                        self.assertIn("user_api_key_cache_ttl", settings)
                        self.assertIs(type(settings["user_api_key_cache_ttl"]), int)
                        self.assertEqual(settings["user_api_key_cache_ttl"], 0)
                        self.assertTrue(settings["enforce_fallback_model_access"])
                    else:
                        self.assertNotIn("user_api_key_cache_ttl", settings)
                    self.assertTrue(config["litellm_settings"]["cache"])
                    self.assertEqual(config["litellm_settings"]["cache_params"]["mode"], "default_off")
                    self.assertIn("redis_host", config["router_settings"])

    def test_governed_proxy_trust_uses_only_compiler_selected_peers(self):
        values = native_values()
        values["openclaw"]["config"]["gateway"] = {
            "trustedProxies": ["0.0.0.0/0", "127.0.0.6"],
        }
        values["sandboxWorkload"]["trustedProxyIPs"] = ["192.0.2.24", "2001:db8::24"]
        objects = render(RUNTIME_CHART, values)
        config = json.loads(object_of_kind(objects, "ConfigMap")["data"]["openclaw.json"])
        self.assertEqual(config["gateway"]["trustedProxies"], ["192.0.2.24", "2001:db8::24"])
        values["sandboxWorkload"]["trustedProxyIPs"] = ["10.244.0.0/16", "fd00:244::/64"]
        objects = render(RUNTIME_CHART, values)
        config = json.loads(object_of_kind(objects, "ConfigMap")["data"]["openclaw.json"])
        self.assertEqual(config["gateway"]["trustedProxies"], ["10.244.0.0/16", "fd00:244::/64"])
        values["sandboxWorkload"]["trustedProxyIPs"] = []
        objects = render(RUNTIME_CHART, values)
        config = json.loads(object_of_kind(objects, "ConfigMap")["data"]["openclaw.json"])
        self.assertEqual(config["gateway"]["trustedProxies"], [])
        values["sandboxWorkload"].pop("trustedProxyIPs")
        objects = render(RUNTIME_CHART, values)
        config = json.loads(object_of_kind(objects, "ConfigMap")["data"]["openclaw.json"])
        self.assertEqual(config["gateway"]["trustedProxies"], ["0.0.0.0/0", "127.0.0.6"])

    def test_governed_proxy_trust_rejects_malformed_internal_values(self):
        for value in ("192.0.2.24", False, [False], [""], ["0.0.0.0/0"], ["localhost"]):
            with self.subTest(value=value):
                values = native_values()
                values["sandboxWorkload"]["trustedProxyIPs"] = value
                render(RUNTIME_CHART, values, error="trustedProxyIPs")

    def test_empty_skills_are_read_only_without_extra_storage_or_grants(self):
        values = native_values()
        values["sandboxWorkload"]["podFinalizers"] = ["cetirun.microsoft.com/native-pod-lifetime"]
        baseline = render(RUNTIME_CHART, values)
        values["sandboxWorkload"]["skillsMode"] = "empty"
        values["skills"] = {"shared": False, "existingClaim": ""}
        documents = render(RUNTIME_CHART, values)
        self.assertEqual(
            [(d["kind"], d["metadata"]["name"]) for d in documents],
            [(d["kind"], d["metadata"]["name"]) for d in baseline],
        )
        self.assertFalse(any(d["kind"] in ("PersistentVolume", "PersistentVolumeClaim") for d in documents))
        expected = copy.deepcopy(object_of_kind(baseline, "StatefulSet"))
        pod = expected["spec"]["template"]["spec"]
        volumes = pod["volumes"]
        index = next(i for i, volume in enumerate(volumes) if volume["name"] == "shared-skills")
        volumes[index] = {"name": "empty-skills", "emptyDir": {}}
        mounts = pod["containers"][0]["volumeMounts"]
        index = next(i for i, mount in enumerate(mounts) if mount["name"] == "shared-skills")
        mounts[index] = {"name": "empty-skills", "mountPath": "/home/vibe/.aks/skills", "readOnly": True}
        pod["containers"][0]["env"] = [
            item for item in pod["containers"][0]["env"]
            if item["name"] not in {"CHOKIDAR_USEPOLLING", "CHOKIDAR_INTERVAL"}
        ]
        self.assertEqual(object_of_kind(documents, "StatefulSet"), expected)
        for kind in ("Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "ServiceAccount", "CiliumNetworkPolicy"):
            self.assertEqual(
                [d for d in documents if d["kind"] == kind],
                [d for d in baseline if d["kind"] == kind],
            )

    def test_empty_skills_reject_inconsistent_internal_values(self):
        for path, value, error in (
            (("sandboxWorkload", "skillsMode"), "unknown", "skillsMode"),
            (("sandboxWorkload", "skillsMode"), False, "skillsMode"),
            (("sandboxWorkload", "podFinalizers"), [], "podFinalizers"),
            (("skills", "shared"), True, "shared"),
            (("skills", "existingClaim"), "example-skills", "existingClaim"),
            (("skills", "npm"), ["example-package"], "npm"),
            (("skills", "clawHub"), ["example-skill"], "clawHub"),
            (("skills", "mountPath"), "/other-skills", "mountPath"),
            (("aiGateway", "enabled"), False, "enabled"),
            (("agentType",), "hermes", "Sandbox empty skills require"),
        ):
            with self.subTest(path=path, value=value):
                values = native_values()
                values["sandboxWorkload"].update({
                    "skillsMode": "empty",
                    "podFinalizers": ["cetirun.microsoft.com/native-pod-lifetime"],
                })
                values["skills"] = {"shared": False, "existingClaim": ""}
                target = values
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = value
                render(RUNTIME_CHART, values, error=error)

    def test_native_probes_use_exec_without_changing_network_controls_or_timing(self):
        values = native_values()
        values["service"] = {"port": 18793, "targetPort": 18793}
        values["openclaw"]["config"]["gateway"] = {"port": 18793}
        paths = {"livenessProbe": "/healthz", "readinessProbe": "/readyz", "startupProbe": "/startupz"}
        for kind in paths:
            values[kind] = {
                "initialDelaySeconds": 2, "periodSeconds": 7, "timeoutSeconds": 3,
                "failureThreshold": 4, "successThreshold": 1,
            }
        objects = render(RUNTIME_CHART, values)
        pod = object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]
        main = pod["containers"][0]
        for kind, path in paths.items():
            with self.subTest(probe=kind):
                self.assertEqual(main[kind], {
                    **values[kind],
                    "exec": {"command": ["node", "/etc/cetirun/health-probe.cjs", "18793", path, "3000"]},
                })
        self.assertEqual(object_of_kind(objects, "CiliumNetworkPolicy"), values["sandboxWorkload"]["objects"][0])
        data = object_of_kind(objects, "ConfigMap")["data"]
        self.assertEqual(data["health-probe.cjs"], (
            RUNTIME_CHART / "agents/openclaw/bootstrap/health-probe.cjs"
        ).read_text())

    def test_default_startup_window_is_scoped_to_common_openclaw(self):
        for agent, reconciled, expected in (
            ("openclaw", True, 60),
            ("openclaw", False, 30),
            ("hermes", True, 30),
            ("hermes", False, 30),
        ):
            with self.subTest(agent=agent, reconciled=reconciled):
                values = native_values()
                values["agentType"] = agent
                if reconciled:
                    values["sandboxWorkload"]["reconciled"] = True
                    values["sandboxWorkload"]["executionHeld"] = False
                objects = render(RUNTIME_CHART, values)
                main = object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]["containers"][0]
                self.assertEqual(main["startupProbe"]["failureThreshold"], expected)
                self.assertEqual(main["startupProbe"]["periodSeconds"], 10)
                self.assertEqual(main["startupProbe"]["timeoutSeconds"], 5)
                self.assertEqual(main["startupProbe"]["initialDelaySeconds"], 10)
                self.assertIn("livenessProbe", main)
                self.assertIn("readinessProbe", main)
                values["startupProbe"] = {"failureThreshold": 30}
                explicit = object_of_kind(render(RUNTIME_CHART, values), "StatefulSet")
                self.assertEqual(explicit["spec"]["template"]["spec"]["containers"][0]["startupProbe"]["failureThreshold"], 30)

    def test_common_openclaw_startup_window_validates_explicit_threshold(self):
        values = native_values()
        values["sandboxWorkload"]["reconciled"] = True
        values["sandboxWorkload"]["executionHeld"] = False
        for threshold in (0, -1, 0.5, "60", True):
            with self.subTest(threshold=threshold):
                values["startupProbe"] = {"failureThreshold": threshold}
                render(RUNTIME_CHART, values, error="failureThreshold")

    def test_disabling_native_probes_does_not_render_a_replacement_probe(self):
        values = native_values()
        for kind in ("livenessProbe", "readinessProbe", "startupProbe"):
            values[kind] = {"enabled": False}
        objects = render(RUNTIME_CHART, values)
        main = object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]["containers"][0]
        self.assertFalse(any(kind in main for kind in ("livenessProbe", "readinessProbe", "startupProbe")))

    def test_held_hermes_keeps_api_startup_and_liveness_separate_from_webapp_readiness(self):
        values = native_values()
        values["agentType"] = "hermes"
        objects = render(RUNTIME_CHART, values)
        main = object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]["containers"][0]
        for kind in ("livenessProbe", "readinessProbe", "startupProbe"):
            with self.subTest(probe=kind):
                command = ["node", "/etc/cetirun/hermes-health-probe.cjs"]
                if kind == "readinessProbe":
                    command = ["/usr/local/bin/hermes-python", "/etc/cetirun/hermes-runtime.py", "readiness"]
                self.assertEqual(main[kind]["exec"]["command"], [
                    *command, "--timeout", str(main[kind]["timeoutSeconds"]),
                ])
                self.assertNotIn("httpGet", main[kind])
        data = object_of_kind(objects, "ConfigMap")["data"]
        self.assertEqual(data["hermes-health-probe.cjs"], (
            RUNTIME_CHART / "agents/hermes/bootstrap/hermes-health-probe.cjs"
        ).read_text())
        self.assertNotIn("health-probe.cjs", data)

    def test_packaged_chart_contains_the_native_runtime_assets(self):
        values = native_values()
        expected = render(RUNTIME_CHART, values)
        with tempfile.TemporaryDirectory(prefix="cetirun-native-chart-") as directory:
            result = subprocess.run(
                ["helm", "package", str(RUNTIME_CHART), "--destination", directory],
                env=OFFLINE, text=True, capture_output=True, timeout=30, check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            packages = list(Path(directory).glob("*.tgz"))
            self.assertEqual(len(packages), 1)
            self.assertEqual(render(packages[0], values), expected)

    def test_native_gateway_renders_without_an_istio_sidecar(self):
        values = native_values()
        objects = render(RUNTIME_CHART, values)
        workload = object_of_kind(objects, "StatefulSet")
        self.assertEqual(workload["spec"]["replicas"], 0)
        self.assertEqual(workload["metadata"]["ownerReferences"], values["sandboxWorkload"]["ownerReferences"])
        pod = workload["spec"]["template"]["spec"]
        self.assertEqual(pod["runtimeClassName"], "example-kata")
        self.assertIs(pod["automountServiceAccountToken"], False)
        self.assertEqual(pod["dnsPolicy"], "None")
        self.assertEqual(pod["dnsConfig"], {"nameservers": ["192.0.2.53"]})
        self.assertFalse(any(item["kind"] in {"Sidecar", "NetworkPolicy", "PersistentVolumeClaim"} for item in objects))
        self.assertEqual(object_of_kind(objects, "CiliumNetworkPolicy"), values["sandboxWorkload"]["objects"][0])
        main = pod["containers"][0]
        env = {item["name"]: item for item in main["env"]}
        self.assertEqual(env["OPENAI_API_KEY"]["valueFrom"]["secretKeyRef"], {
            "name": "example-model-key", "key": "api-key",
        })
        self.assertEqual(main["image"], "registry.example.com/openclaw-gateway:example")
        init = pod["initContainers"]
        initializer = next(item for item in init if item["name"] == "init-home-data")
        self.assertIn({"name": "CETIRUN_NATIVE_SANDBOX", "value": "true"}, initializer["env"])
        for container in init:
            self.assertEqual(container["securityContext"], values["sandboxWorkload"]["securityContext"])
            self.assertEqual(container["resources"], values["sandboxWorkload"]["helperResources"])
        data = object_of_kind(objects, "ConfigMap")["data"]
        config = json.loads(data["openclaw.json"])
        self.assertEqual(config["auth"], {"profiles": {}})
        self.assertEqual(list(config["models"]["providers"]), ["openai"])
        self.assertEqual(config["models"]["providers"]["openai"]["apiKey"], {
            "source": "env", "provider": "default", "id": "OPENAI_API_KEY",
        })
        self.assertEqual(config["agents"]["defaults"]["modelPolicy"], {"allow": ["openai/example-model"]})

    def test_native_invalid_controls_are_not_silently_ignored(self):
        cases = [
            ("sidecarScope", "enabled", True, "Sandbox network controls are protected"),
            ("networkPolicy", "enabled", False, "Sandbox network controls are protected"),
            ("secrets", "apiBase", "", "Sandbox Gateway routing requires secrets.apiBase"),
            ("secrets", "model", "ungranted", "Sandbox default model must be in aiGateway.models"),
            ("aiGateway", "models", [], "Sandbox default model must be in aiGateway.models"),
            ("sandboxWorkload", "executionHeld", False, "Sandbox render plans must retain the execution hold"),
        ]
        for section, key, value, message in cases:
            with self.subTest(section=section, key=key, value=value):
                values = native_values()
                values[section][key] = value
                render(RUNTIME_CHART, values, error=message)

    def test_native_packaging_does_not_release_execution(self):
        values = native_values()
        values["replicaCount"] = 1
        render(RUNTIME_CHART, values, error="Sandbox render plans must retain the execution hold")

    def test_standalone_gateway_replaces_provider_config_and_keeps_init_behavior(self):
        values = native_values()
        values["sandboxWorkload"] = None
        values["sidecarScope"]["enabled"] = True
        objects = render(RUNTIME_CHART, values)
        pod = object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]
        self.assertEqual(pod["containers"][0]["startupProbe"]["failureThreshold"], 30)
        self.assertEqual(pod["containers"][0]["startupProbe"]["periodSeconds"], 10)
        self.assertFalse(any(
            variable["name"] == "CETIRUN_NATIVE_SANDBOX"
            for item in pod["initContainers"] for variable in item.get("env", [])
        ))
        config = json.loads(object_of_kind(objects, "ConfigMap")["data"]["openclaw.json"])
        self.assertEqual(list(config["models"]["providers"]), ["openai"])
        self.assertEqual(config["gateway"]["trustedProxies"], ["127.0.0.6"])
        self.assertNotIn("health-probe.cjs", object_of_kind(objects, "ConfigMap")["data"])
        for kind, path in (
            ("livenessProbe", "/healthz"), ("readinessProbe", "/readyz"), ("startupProbe", "/startupz"),
        ):
            with self.subTest(probe=kind):
                self.assertEqual(pod["containers"][0][kind]["httpGet"], {"path": path, "port": "gateway"})
                self.assertNotIn("exec", pod["containers"][0][kind])
        values["sidecarScope"]["enabled"] = False
        render(RUNTIME_CHART, values, error="aiGateway.enabled requires sidecarScope.enabled")

    def test_integrated_gateway_binds_each_model_name(self):
        # The operator recomputes the group from the model name, so this must
        # stay the plain SHA-256 of model_name.
        binding = hashlib.sha256(MODEL["model_name"].encode()).hexdigest()[:16]
        configuration = hashlib.sha256(
            json.dumps(MODEL, sort_keys=True, separators=(",", ":")).encode()
        ).hexdigest()[:16]
        configured = gateway_models(MODEL, integrated=True)[0]
        self.assertEqual(configured["litellm_params"], MODEL["litellm_params"])
        self.assertEqual(configured["model_info"], {
            **MODEL["model_info"],
            "id": f"cetirun-shared-{binding}-{configuration}",
            "access_groups": ["example-readers", f"cetirun-model-{binding}"],
        })
        self.assertEqual(gateway_models(dict(reversed(list(MODEL.items()))), integrated=True), [configured])
        # Editing the configuration changes the deployment ID only. The group
        # that Agent keys are granted stays, so Agents keep the model.
        changed = copy.deepcopy(MODEL)
        changed["litellm_params"]["api_base"] = "https://replacement.example.com/v1"
        changed["model_info"]["input_cost_per_token"] = 0.002
        replacement = gateway_models(changed, integrated=True)[0]
        self.assertNotEqual(replacement["model_info"]["id"], configured["model_info"]["id"])
        self.assertTrue(replacement["model_info"]["id"].startswith(f"cetirun-shared-{binding}-"))
        self.assertEqual(replacement["model_info"]["access_groups"], configured["model_info"]["access_groups"])
        renamed = {**copy.deepcopy(MODEL), "model_name": "renamed-model"}
        self.assertNotEqual(
            gateway_models(renamed, integrated=True)[0]["model_info"]["access_groups"][-1],
            configured["model_info"]["access_groups"][-1],
        )

    def test_integrated_gateway_entries_of_one_name_share_its_group(self):
        second = copy.deepcopy(MODEL)
        second["litellm_params"]["api_base"] = "https://second.example.com/v1"
        first_info, second_info = (
            entry["model_info"] for entry in gateway_models(MODEL, integrated=True, more=[second])
        )
        self.assertEqual(first_info["access_groups"], second_info["access_groups"])
        self.assertNotEqual(first_info["id"], second_info["id"])

    def test_only_integrated_gateway_reserves_model_bindings(self):
        for model_info in [
            {"id": "operator-cannot-trust-this"},
            {"id": ""},
            {"access_groups": ["cetirun-model-injected"]},
            {"access_groups": ["CetiRun-model-injected"]},
        ]:
            with self.subTest(model_info=model_info):
                model = {**MODEL, "model_info": model_info}
                render(
                    GATEWAY_CHART, {"operatorIntegration": True, "models": [model]},
                    error="reserved",
                )
                self.assertEqual(gateway_models(model, integrated=False), [model])

    def test_standalone_gateway_model_configuration_is_unchanged(self):
        self.assertEqual(gateway_models(MODEL, integrated=False), [MODEL])

    def test_running_gateway_pods_keep_the_configuration_they_started_with(self):
        # A directory mount is updated inside running pods, and LiteLLM's
        # periodic re-read then removed an edited model from the old pods.
        for integrated in (False, True):
            with self.subTest(integrated=integrated):
                objects = render(GATEWAY_CHART, {"operatorIntegration": integrated, "models": [MODEL]})
                deployment = object_of_kind(objects, "Deployment")
                pod = deployment["spec"]["template"]["spec"]
                config_volume = next(volume for volume in pod["volumes"] if "configMap" in volume)
                mounts = [
                    mount
                    for container in [*pod.get("initContainers", []), *pod["containers"]]
                    for mount in container.get("volumeMounts", [])
                    if mount["name"] == config_volume["name"]
                ]
                self.assertEqual(mounts, [{
                    "name": config_volume["name"], "mountPath": "/etc/litellm/config.yaml",
                    "subPath": "config.yaml", "readOnly": True,
                }])
                gateway = next(container for container in pod["containers"] if container["name"] == "gateway")
                self.assertIn("/etc/litellm/config.yaml", gateway["args"])
                self.assertTrue(deployment["spec"]["template"]["metadata"]["annotations"]["checksum/config"])

    def test_token_limits_count_finished_requests_unless_reservation_is_on(self):
        # LiteLLM reserves input plus maximum output before each call unless
        # told not to; OpenClaw's 128,000-token maximum output then refused a
        # long conversation in every minute under a small limit.
        for values, expected in (({}, "false"), ({"tpmReservation": False}, "false"), ({"tpmReservation": True}, "true")):
            with self.subTest(values=values):
                deployment = object_of_kind(render(GATEWAY_CHART, {"models": [MODEL], **values}), "Deployment")
                gateway = next(c for c in deployment["spec"]["template"]["spec"]["containers"] if c["name"] == "gateway")
                env = {item["name"]: item.get("value") for item in gateway["env"]}
                self.assertEqual(env["LITELLM_TPM_TOKEN_RESERVATION_ENABLED"], expected)
        render(GATEWAY_CHART, {"models": [MODEL], "tpmReservation": "yes"}, error="tpmReservation")

    def test_spend_log_stores_content_by_default_and_accepts_an_explicit_opt_out(self):
        for values, expected in (
            ({}, True),
            ({"spendLogs": {"storePrompts": True}}, True),
            ({"spendLogs": {"storePrompts": False}}, False),
            ({"spendLogs": {"storePrompts": None}}, True),
        ):
            with self.subTest(values=values):
                settings = gateway_general_settings(values)
                self.assertIs(settings.get("store_prompts_in_spend_logs"), expected)

    def test_spend_log_cleanup_runs_every_hour_whenever_a_retention_is_set(self):
        # LiteLLM schedules its cleanup only from a string interval, and its
        # default, a day, outlasts pods that configuration changes replace.
        for values in (
            {},
            {"replicaCount": 1, "redis": {"bundled": {"enabled": False}}},
            {"spendLogs": {"enabled": False, "storePrompts": False}},
            {"spendLogs": {"retention": "1h"}},
        ):
            with self.subTest(values=values):
                settings = gateway_general_settings(values)
                self.assertIs(type(settings["maximum_spend_logs_retention_interval"]), str)
                self.assertEqual(settings["maximum_spend_logs_retention_interval"], "1h")
                self.assertEqual(
                    settings["maximum_health_check_retention_period"],
                    settings["maximum_spend_logs_retention_period"],
                )
        settings = gateway_general_settings({"spendLogs": {"retention": ""}})
        for key in (
            "maximum_spend_logs_retention_period",
            "maximum_health_check_retention_period",
            "maximum_spend_logs_retention_interval",
        ):
            with self.subTest(key=key):
                self.assertNotIn(key, settings)


@unittest.skipUnless(shutil.which("node"), "requires Node for the runtime health probe")
class NativeHealthProbeTests(unittest.TestCase):
    def setUp(self):
        self.status = 200
        self.stall = False
        self.requests = []
        self.release = threading.Event()
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                fixture.requests.append((self.path, self.headers))
                if fixture.stall:
                    fixture.release.wait(timeout=60)
                    return
                self.send_response(fixture.status)
                if fixture.status == 302:
                    self.send_header("Location", "/must-not-follow")
                self.send_header("Content-Length", "0")
                self.end_headers()

            def log_message(self, *_args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop_server)

    def stop_server(self):
        self.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        self.assertFalse(self.thread.is_alive())

    def probe(self, path="/healthz", *, port=None, timeout=1000, args=None, env=None):
        if args is None:
            args = [str(port or self.server.server_port), path, str(timeout)]
        # Node can take seconds to start on a busy CI runner. The tests check the
        # probe's own deadline; this timeout only stops a probe that hangs.
        return subprocess.run(
            ["node", str(RUNTIME_CHART / "agents/openclaw/bootstrap/health-probe.cjs"), *args],
            env={**OFFLINE, "OPENAI_API_KEY": "sk-dummy", **(env or {})},
            text=True, capture_output=True, timeout=30, check=False,
        )

    def test_each_health_endpoint_uses_loopback_without_credentials(self):
        for path in ("/health", "/healthz", "/readyz", "/startupz"):
            with self.subTest(path=path):
                result = self.probe(path)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, "")
                observed_path, headers = self.requests[-1]
                self.assertEqual(observed_path, path)
                self.assertEqual(headers["Host"], f"127.0.0.1:{self.server.server_port}")
                self.assertIsNone(headers["Authorization"])
                self.assertIsNone(headers["Cookie"])

    def test_http_status_controls_health_without_following_redirects(self):
        for status in (200, 204, 302, 399, 400, 401, 500, 503):
            with self.subTest(status=status):
                self.status = status
                count = len(self.requests)
                result = self.probe()
                self.assertEqual(result.returncode, 0 if 200 <= status < 400 else 1)
                self.assertEqual(len(self.requests), count + 1)
                if status >= 400:
                    self.assertIn(f"HTTP {status}", result.stderr)

    def test_connection_failure_is_unhealthy(self):
        with socket.socket() as reserved:
            reserved.bind(("127.0.0.1", 0))
            result = self.probe(port=reserved.getsockname()[1])
        self.assertEqual(result.returncode, 1)
        self.assertIn("connection failed", result.stderr)

    def test_request_deadline_is_bounded(self):
        self.stall = True
        started = time.monotonic()
        result = self.probe(timeout=100)
        elapsed = time.monotonic() - started
        self.assertEqual(result.returncode, 1)
        self.assertIn("request timed out", result.stderr)
        self.assertGreaterEqual(elapsed, 0.1)
        # The stalled server would hold the request for 60 seconds.
        self.assertLess(elapsed, 20)

    def test_proxy_environment_cannot_redirect_the_probe(self):
        with socket.socket() as reserved:
            reserved.bind(("127.0.0.1", 0))
            proxy = f"http://127.0.0.1:{reserved.getsockname()[1]}"
            result = self.probe(env={
                "HTTP_PROXY": proxy, "HTTPS_PROXY": proxy, "ALL_PROXY": proxy,
                "http_proxy": proxy, "https_proxy": proxy, "all_proxy": proxy,
                "NO_PROXY": "", "no_proxy": "", "NODE_USE_ENV_PROXY": "1",
            })
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.requests), 1)
        self.assertEqual(self.requests[0][0], "/healthz")

    def test_invalid_arguments_fail_without_a_request(self):
        for args in (
            [], ["0", "/healthz", "1000"], ["65536", "/healthz", "1000"],
            ["not-a-port", "/healthz", "1000"], ["18789", "https://example.com", "1000"],
            ["18789", "/healthz", "0"], ["18789", "/healthz", "2147483648"],
            ["18789", "/healthz", "1000", "extra"],
        ):
            with self.subTest(args=args):
                result = self.probe(args=args)
                self.assertEqual(result.returncode, 1)
                self.assertIn("invalid probe arguments", result.stderr)
        self.assertEqual(self.requests, [])


if __name__ == "__main__":
    unittest.main()
