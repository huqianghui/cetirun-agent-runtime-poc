# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline rendering of the prepared CetiAdmin runtime projection."""

import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


CHART = Path(__file__).resolve().parents[1]
HELM = os.environ.get("HELM_BINARY", "helm")
RELEASE = "openclaw-cetiadmin"
NAMESPACE = "cetirun-cetiadmin"
ASSETS = "/opt/cetirun/cetiadmin"
API_MOUNT = "/var/run/cetirun/kubernetes"
PRIVATE = "synthetic-credential-must-not-render"
BLOCKED = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.169.254/32"]
OFFLINE = {**os.environ, "KUBECONFIG": os.devnull, "HELM_REGISTRY_CONFIG": os.devnull,
           "HELM_REPOSITORY_CONFIG": os.devnull}


def ordinary_values():
    return {"agentType": "hermes", "runtimeClassName": "",
            "hermes": {"env": {"HERMES_WEBAPP_PUBLIC_URL": "https://agents.example.com/ui/cetiadmin"}},
            "secrets": {"existingSecret": "native-auth", "model": "fixture-model",
                        "apiBase": "http://gateway.example.svc.cluster.local:4000/v1"},
            "aiGateway": {"keySecretName": "model-key"}}


def refresh_json(values):
    admin = values["cetiadmin"]
    admin["runtimeConfig"] = json.dumps({"instance": {"name": admin["agentName"], "namespace": NAMESPACE,
                                                    "uid": admin["agentUID"]}, "monitor": admin["monitor"]})
    admin["kubeconfig"] = json.dumps({
        "apiVersion": "v1", "kind": "Config", "current-context": "cetiadmin",
        "clusters": [{"name": "cetiadmin", "cluster": {"server": admin["kubernetes"]["apiServer"],
                                                        "certificate-authority": API_MOUNT + "/ca.crt"}}],
        "contexts": [{"name": "cetiadmin", "context": {"cluster": "cetiadmin", "user": "cetiadmin", "namespace": NAMESPACE}}],
        "users": [{"name": "cetiadmin", "user": {"tokenFile": API_MOUNT + "/token"}}],
    })


def prepared_values():
    values = ordinary_values()
    values.update({
        "runtime": {
            "contractVersion": "v1",
            "agent": {"type": "hermes", "image": "registry.example.com/hermes-gateway:0.1.1",
                      "username": "vibe", "home": "/home/vibe", "port": 8642},
            "ports": [{"name": "api", "number": 8642}, {"name": "webapp", "number": 9120}],
            "http": {"portName": "api", "healthPath": "/health", "readinessPath": "/health", "startupPath": "/health"},
            "configFiles": {"hermes-config.yaml": yaml.safe_dump({
                "model": {"default": "fixture-model"},
                "skills": {"external_dirs": ["/home/vibe/own-skills", ASSETS + "/skills"]},
            })},
        },
        "serviceAccount": {"create": False, "name": "managed-cetiadmin", "role": "", "automountToken": False},
        "cetiadmin": {
            "enabled": True, "agentName": "cetiadmin", "agentUID": "agent-uid-001", "namespaceUID": "namespace-uid-001",
            "sourceUID": "source-uid-001", "configHash": "a" * 64, "serviceAccountName": "managed-cetiadmin",
            "operatorImage": "registry.example.com/cetirun-operator:0.1.1@sha256:" + "b" * 64,
            "kubernetes": {"apiServer": "https://kubernetes.default.svc", "apiAudience": "https://kubernetes.default.svc.cluster.local",
                           "caConfigMapName": "kube-root-ca.crt", "apiEgressCIDRs": ["192.0.2.10/32"]},
            "azureWorkloadIdentity": {"enabled": False},
            "monitor": {"enabled": False, "timezone": "UTC", "deliver": "local"},
        },
        "networkPolicy": {"allowedInternalCIDRs": [{"cidr": "192.0.2.10/32", "ports": [{"protocol": "TCP", "port": 443}]}],
                          "blockedCIDRs": [*BLOCKED, "192.0.2.10/32"]},
        "sidecarScope": {"additionalEgressHosts": ["default/kubernetes.default.svc.cluster.local"]},
    })
    refresh_json(values)
    return values


def render(values, *, release=RELEASE, namespace=NAMESPACE):
    return subprocess.run([
        HELM, "template", release, str(CHART), "--namespace", namespace, "--kube-version", "1.37.0",
        "--api-versions", "networking.istio.io/v1/Sidecar", "--values", "-",
    ], input=json.dumps(values), text=True, capture_output=True, env=OFFLINE, timeout=30, check=False)


def objects(result):
    return [obj for obj in yaml.safe_load_all(result.stdout) if obj]


def statefulset(result):
    return next(obj for obj in objects(result) if obj["kind"] == "StatefulSet")


class CetiAdminChartTests(unittest.TestCase):
    def rendered(self, values):
        result = render(values)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(PRIVATE in result.stdout, "unvalidated fixture credential appeared in the manifest")
        return result

    def rejected(self, values, **kwargs):
        result = render(values, **kwargs)
        self.assertNotEqual(result.returncode, 0, "invalid CetiAdmin input rendered")
        self.assertFalse(PRIVATE in result.stdout + result.stderr, "unvalidated fixture credential appeared in render output")
        return result

    def test_runtime_accepts_projection_without_management_lists(self):
        values = prepared_values()
        self.rendered(values)
        for field in ("scope", "teamNames", "targets", "accessHash", "platformReadNamespaces"):
            self.assertNotIn(field, values["cetiadmin"])

    def test_runtime_rejects_legacy_scope_and_platform_diagnostics(self):
        for enabled in (True, False):
            for field, value in (("scope", {"mode": "all-managed-agents"}),
                                 ("scope", {"mode": "teams", "teamNames": ["example-team"]}),
                                 ("platformReadNamespaces", ["platform"])):
                with self.subTest(enabled=enabled, field=field, value=value):
                    values = prepared_values() if enabled else ordinary_values()
                    values.setdefault("cetiadmin", {})["enabled"] = enabled
                    values["cetiadmin"][field] = value
                    self.assertIn(field, self.rejected(values).stderr)

    def test_default_is_off_and_explicit_false_preserves_render(self):
        base = ordinary_values()
        before = self.rendered(base)
        after = self.rendered({**base, "cetiadmin": {"enabled": False}})
        self.assertEqual(before.stdout, after.stdout)
        pod = statefulset(after)["spec"]["template"]["spec"]
        self.assertNotIn("cetiadmin-assets", {item["name"] for item in pod["volumes"]})
        self.assertNotIn("cetiadmin-kubernetes", {item["name"] for item in pod["volumes"]})
        self.assertNotIn("command", pod["containers"][0])

    def test_main_environment_has_unique_merge_keys(self):
        for enabled in (False, True):
            with self.subTest(cetiadmin=enabled):
                values = prepared_values() if enabled else ordinary_values()
                pod = statefulset(self.rendered(values))["spec"]["template"]["spec"]
                main = next(item for item in pod["containers"] if item["name"] == "cetirun")
                names = [item["name"] for item in main["env"]]
                self.assertEqual(len(names), len(set(names)), "duplicate env names break Helm strategic merge patches")
                self.assertEqual([item for item in main["env"] if item["name"] == "HERMES_PYTHON"], [
                    {"name": "HERMES_PYTHON", "value": "/usr/local/bin/hermes-python"},
                ])

    def test_export_init_is_unprivileged_and_has_only_bounded_assets(self):
        values = prepared_values()
        pod = statefulset(self.rendered(values))["spec"]["template"]["spec"]
        exporter = next(item for item in pod["initContainers"] if item["name"] == "cetiadmin-assets")
        self.assertEqual(exporter["image"], values["cetiadmin"]["operatorImage"])
        self.assertEqual(exporter["command"], ["/manager"])
        self.assertEqual(exporter["args"], ["--mode=cetiadmin-export", "--cetiadmin-output-dir=/cetiadmin-assets"])
        self.assertEqual(exporter["volumeMounts"], [{"name": "cetiadmin-assets", "mountPath": "/cetiadmin-assets"}])
        self.assertFalse(exporter.get("env") or exporter.get("envFrom"))
        self.assertEqual(exporter["securityContext"], {
            "runAsUser": 1024, "runAsGroup": 1024, "runAsNonRoot": True,
            "readOnlyRootFilesystem": True, "allowPrivilegeEscalation": False,
            "capabilities": {"drop": ["ALL"]}, "seccompProfile": {"type": "RuntimeDefault"},
        })
        volume = next(item for item in pod["volumes"] if item["name"] == "cetiadmin-assets")
        self.assertEqual(volume["emptyDir"], {"sizeLimit": "128Mi"})

    def test_only_main_gets_api_token_and_mcp_remains_distinct(self):
        values = prepared_values()
        values["connectionsMCP"] = {"enabled": True, "url": "https://mcp.tools.svc.cluster.local/mcp",
                                    "namespace": "tools", "audience": "connections-proof", "caSecretName": "mcp-ca"}
        result = self.rendered(values)
        pod = statefulset(result)["spec"]["template"]["spec"]
        self.assertEqual(pod["serviceAccountName"], "managed-cetiadmin")
        self.assertIs(pod["automountServiceAccountToken"], False)
        self.assertEqual(pod["securityContext"]["fsGroup"], 1024)
        main = pod["containers"][0]
        self.assertEqual(main["securityContext"]["runAsUser"], 1024)
        self.assertEqual(main["securityContext"]["runAsGroup"], 1024)
        env = {item["name"]: item for item in main["env"]}
        self.assertEqual(env["KUBECONFIG"]["value"], API_MOUNT + "/config")
        self.assertEqual(env["CETIADMIN_AGENT_UID"]["value"], "agent-uid-001")
        mounts = {item["name"]: item for item in main["volumeMounts"]}
        self.assertEqual(mounts["cetiadmin-assets"], {"name": "cetiadmin-assets", "mountPath": ASSETS, "readOnly": True})
        self.assertEqual(mounts["cetiadmin-kubernetes"], {"name": "cetiadmin-kubernetes", "mountPath": API_MOUNT, "readOnly": True})
        volumes = {item["name"]: item for item in pod["volumes"]}
        sources = volumes["cetiadmin-kubernetes"]["projected"]["sources"]
        self.assertEqual(volumes["cetiadmin-kubernetes"]["projected"]["defaultMode"], 0o440)
        self.assertEqual(sources[0]["serviceAccountToken"], {
            "path": "token", "audience": values["cetiadmin"]["kubernetes"]["apiAudience"], "expirationSeconds": 600,
        })
        self.assertEqual(sources[1]["configMap"], {"name": "kube-root-ca.crt", "items": [{"key": "ca.crt", "path": "ca.crt"}]})
        self.assertEqual(sources[2]["configMap"]["items"], [{"key": "cetiadmin-kubeconfig.json", "path": "config"}])
        config_items = volumes["config"]["configMap"]["items"]
        self.assertNotIn("cetiadmin-kubeconfig.json", {item["key"] for item in config_items})
        configmap = next(obj for obj in objects(result) if obj["kind"] == "ConfigMap")
        self.assertEqual({item["key"] for item in config_items}, set(configmap["data"]) - {"cetiadmin-kubeconfig.json"})
        mcp = volumes["connections-identity"]["projected"]["sources"][0]["serviceAccountToken"]
        self.assertEqual(mcp["audience"], "connections-proof")
        for item in [*pod["initContainers"], *pod["containers"][1:]]:
            mounted = {mount["name"] for mount in item.get("volumeMounts", [])}
            self.assertTrue({"cetiadmin-kubernetes", "connections-identity"}.isdisjoint(mounted))
            if item["name"] != "cetiadmin-assets":
                self.assertNotIn("cetiadmin-assets", mounted)
        rbac_kinds = {"ServiceAccount", "RoleBinding", "ClusterRoleBinding", "Role", "ClusterRole"}
        # The pre-existing upgrade hook has its own SA/Role; CetiAdmin adds no grants.
        self.assertEqual([item for item in objects(result) if item["kind"] in rbac_kinds],
                         [item for item in objects(self.rendered(ordinary_values())) if item["kind"] in rbac_kinds])

    def test_runtime_json_and_startup_are_delivered_without_credentials(self):
        values = prepared_values()
        values["cetiadmin"]["monitor"].update(enabled=True, schedule="every 30m", deliver="slack",
                                             slackChannel="C0123456789", slackSecretName="monitor-slack")
        refresh_json(values)
        result = self.rendered(values)
        config = next(item for item in objects(result) if item["kind"] == "ConfigMap")["data"]
        self.assertEqual(json.loads(config["cetiadmin-runtime.json"]), json.loads(values["cetiadmin"]["runtimeConfig"]))
        self.assertEqual(json.loads(config["cetiadmin-kubeconfig.json"]), json.loads(values["cetiadmin"]["kubeconfig"]))
        self.assertEqual(config["hermes-entrypoint.sh"], (CHART / "agents/hermes/bootstrap/hermes-entrypoint.sh").read_text())
        pod = statefulset(result)["spec"]["template"]["spec"]
        main = pod["containers"][0]
        self.assertEqual(main["command"], ["/bin/bash", "/etc/cetirun/hermes-entrypoint.sh"])
        env = {item["name"]: item for item in main["env"]}
        self.assertEqual(env["CETIRUN_CETIADMIN_CONFIG"]["value"], "/etc/cetirun/cetiadmin-runtime.json")
        for name in ("SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"):
            self.assertEqual(env[name], {"name": name, "valueFrom": {"secretKeyRef": {"name": "monitor-slack", "key": name}}})
            for container in [*pod["initContainers"], *pod["containers"][1:]]:
                self.assertNotIn(name, {item["name"] for item in container.get("env", [])})
        self.assertFalse(any(item["kind"] == "Secret" for item in objects(result)))

    def test_exact_network_rules_and_scope_are_kept(self):
        values = prepared_values()
        result = self.rendered(values)
        policy = next(item for item in objects(result) if item["kind"] == "NetworkPolicy")
        api_rule = next(item for item in policy["spec"]["egress"]
                        if item.get("to") == [{"ipBlock": {"cidr": "192.0.2.10/32"}}])
        self.assertEqual(api_rule["ports"], [{"port": 443, "protocol": "TCP"}])
        external = next(item for item in policy["spec"]["egress"] if item.get("to", [{}])[0].get("ipBlock", {}).get("cidr") == "0.0.0.0/0")
        self.assertEqual(set(external["to"][0]["ipBlock"]["except"]), {*BLOCKED, "192.0.2.10/32"})
        scope = next(item for item in objects(result) if item["kind"] == "Sidecar")
        self.assertIn("default/kubernetes.default.svc.cluster.local", scope["spec"]["egress"][0]["hosts"])

    def test_azure_skip_list_uses_all_rendered_auxiliary_names(self):
        values = prepared_values()
        identity = {"enabled": True, "clientId": "11111111-2222-3333-4444-555555555555"}
        values["cetiadmin"]["azureWorkloadIdentity"] = identity
        values["azureWorkloadIdentity"] = identity
        values["initContainers"] = [{"name": "fixture-init", "image": "registry.example.com/helper:1"}]
        values["sidecars"] = [{"name": "fixture-sidecar", "image": "registry.example.com/helper:1"}]
        values["skills"] = {"npm": ["fixture-skill"]}
        result = self.rendered(values)
        template = statefulset(result)["spec"]["template"]
        skip = set(template["metadata"]["annotations"]["azure.workload.identity/skip-containers"].split(";"))
        pod = template["spec"]
        expected = {item["name"] for item in [*pod["initContainers"], *pod["containers"][1:]]}
        self.assertTrue(expected.issubset(skip), expected - skip)
        self.assertTrue({"istio-init", "istio-proxy"}.issubset(skip))
        self.assertNotIn(pod["containers"][0]["name"], skip)
        self.assertEqual(template["metadata"]["labels"]["azure.workload.identity/use"], "true")

    def test_missing_or_mismatched_prepared_inputs_fail(self):
        for key in ("agentName", "agentUID", "namespaceUID", "sourceUID", "configHash", "serviceAccountName", "operatorImage",
                    "runtimeConfig", "kubeconfig", "kubernetes", "monitor", "azureWorkloadIdentity"):
            with self.subTest(missing=key):
                values = prepared_values()
                values["cetiadmin"].pop(key)
                self.rejected(values)
        values = prepared_values()
        values.pop("runtime")
        self.rejected(values)
        for changes in ({"release": "another-release"}, {"namespace": "another-namespace"}):
            self.rejected(prepared_values(), **changes)

    def test_schema_accepts_projection_before_runtime_contribution(self):
        values = prepared_values()
        values.pop("runtime")
        with tempfile.TemporaryDirectory() as directory:
            chart = Path(directory)
            for name in ("Chart.yaml", "values.yaml", "values.schema.json"):
                (chart / name).write_bytes((CHART / name).read_bytes())
            result = subprocess.run(
                [HELM, "template", RELEASE, str(chart), "--namespace", NAMESPACE, "--values", "-"],
                input=json.dumps(values), text=True, capture_output=True, env=OFFLINE, timeout=30,
            )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.rejected(values)

    def test_managed_sa_exception_does_not_enable_arbitrary_byo_mcp(self):
        values = ordinary_values()
        values["connectionsMCP"] = {"enabled": True, "url": "https://mcp.tools.svc.cluster.local/mcp",
                                    "audience": "connections-proof", "caSecretName": "mcp-ca"}
        values["serviceAccount"] = {"create": False, "name": "arbitrary-byo", "automountToken": False}
        self.rejected(values)
        for key, value in (("create", True), ("name", "wrong-account"), ("role", "cluster-admin"), ("automountToken", True)):
            with self.subTest(key=key):
                values = prepared_values()
                values["serviceAccount"][key] = value
                self.rejected(values)

    def test_raw_tokens_wrong_monitor_shape_and_teams_are_rejected(self):
        for key in ("kubeconfig", "runtimeConfig"):
            values = prepared_values()
            obj = json.loads(values["cetiadmin"][key])
            if key == "kubeconfig":
                obj["users"][0]["user"]["token"] = PRIVATE
            else:
                obj["monitor"]["token"] = PRIVATE
            values["cetiadmin"][key] = json.dumps(obj)
            self.rejected(values)
        values = prepared_values()
        values["cetiadmin"]["monitor"]["deliver"] = "teams"
        refresh_json(values)
        self.rejected(values)

    def test_json_binding_drift_and_login_plugins_are_rejected(self):
        for section, name, value in (("instance", "uid", "other-uid"), ("instance", "name", "other-agent"),
                                     ("instance", "namespace", "another-namespace"), ("monitor", "timezone", "UTC-stale")):
            values = prepared_values()
            config = json.loads(values["cetiadmin"]["runtimeConfig"])
            config[section][name] = value
            values["cetiadmin"]["runtimeConfig"] = json.dumps(config)
            self.rejected(values)
        for field, value in (("scope", {"mode": "all-managed-agents"}), ("platformReadNamespaces", ["platform"])):
            values = prepared_values()
            config = json.loads(values["cetiadmin"]["runtimeConfig"])
            config[field] = value
            values["cetiadmin"]["runtimeConfig"] = json.dumps(config)
            self.rejected(values)
        for login in ({"exec": {"command": "unapproved-login"}}, {"auth-provider": {"name": "azure"}},
                      {"client-key-data": PRIVATE}, {"tokenFile": "/home/vibe/token"}):
            values = prepared_values()
            config = json.loads(values["cetiadmin"]["kubeconfig"])
            config["users"][0]["user"].update(login)
            values["cetiadmin"]["kubeconfig"] = json.dumps(config)
            self.rejected(values)
        for key, value in (("server", "https://other.example.com"), ("insecure-skip-tls-verify", True),
                           ("certificate-authority", "/home/vibe/ca.crt"), ("certificate-authority-data", PRIVATE)):
            values = prepared_values()
            config = json.loads(values["cetiadmin"]["kubeconfig"])
            config["clusters"][0]["cluster"][key] = value
            values["cetiadmin"]["kubeconfig"] = json.dumps(config)
            self.rejected(values)

    def test_prepared_cetiadmin_cannot_activate_custom_or_sandbox(self):
        for agent_type in ("openclaw", "custom"):
            values = prepared_values()
            values["agentType"] = agent_type
            values["runtime"]["agent"]["type"] = agent_type
            self.assertIn("CetiAdmin", self.rejected(values).stderr)
        values = prepared_values()
        values["sandboxWorkload"] = {
            "contractVersion": "v1", "reconciled": True, "executionHeld": False,
            "podResources": {}, "helperResources": {}, "securityContext": {"runAsNonRoot": True},
            "scratchSizeLimit": "1Gi", "ownerReferences": [{"apiVersion": "cetirun.microsoft.com/v1alpha1",
                "kind": "Agent", "name": "cetiadmin", "uid": "agent-uid-001"}],
            "podLabels": {}, "dnsConfig": {"nameservers": ["192.0.2.53"]},
            "identityVolumes": [], "identityMounts": [], "identityEnvironment": [],
            "objects": [{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "sandbox-object"}}],
        }
        values.update(runtimeClassName="kata", podAnnotations={"sidecar.istio.io/inject": "false"},
                      sidecarScope={"enabled": False, "additionalEgressHosts": []},
                      persistence={"existingClaim": "sandbox-home", "useStatefulSetVolumeClaim": False},
                      skills={"existingClaim": "sandbox-skills"}, execRestriction={"enabled": False})
        self.assertIn("CetiAdmin", self.rejected(values).stderr)

    def test_extra_values_cannot_rewire_identity_or_assets(self):
        for name in ("KUBECONFIG", "CETIRUN_CETIADMIN_CONFIG", "CETIADMIN_AGENT_UID", "HERMES_TIMEZONE",
                     "HERMES_PYTHON", "HERMES_INSTALL_DIR", "SLACK_BOT_TOKEN", "AZURE_FEDERATED_TOKEN_FILE"):
            values = prepared_values()
            values["extraEnv"] = [{"name": name, "value": PRIVATE}]
            self.rejected(values)
        for path in ("/", "/opt", ASSETS, ASSETS + "/skills", "/var/run", API_MOUNT, "/etc/cetirun"):
            values = prepared_values()
            values["extraVolumeMounts"] = [{"name": "unexpected", "mountPath": path}]
            self.rejected(values)
        for name in ("cetiadmin-assets", "cetiadmin-kubernetes"):
            values = prepared_values()
            values["extraVolumes"] = [{"name": name, "emptyDir": {}}]
            self.rejected(values)
            for field in ("sidecars", "initContainers"):
                values = prepared_values()
                values[field] = [{"name": "unexpected", "image": "registry.example.com/helper:1",
                                  "volumeMounts": [{"name": name, "mountPath": "/leaked"}]}]
                self.rejected(values)
        for field, setting in (("podSecurityContext", "fsGroup"), ("securityContext", "runAsUser"),
                               ("securityContext", "runAsGroup")):
            values = prepared_values()
            values[field] = {setting: 1025}
            self.rejected(values)

    def test_projection_only_azure_and_optional_container_names(self):
        values = prepared_values()
        values["memoryContext"] = {"enabled": False}
        values["desktop"] = {"enabled": True}
        values["logger"] = {"enabled": True}
        values["cetiadmin"]["azureWorkloadIdentity"] = {"enabled": True, "clientId": "11111111-2222-3333-4444-555555555555"}
        values["podAnnotations"] = {"azure.workload.identity/skip-containers": "known-injected-sidecar", "fixture": "retained"}
        template = statefulset(self.rendered(values))["spec"]["template"]
        annotations = template["metadata"]["annotations"]
        self.assertEqual(annotations["fixture"], "retained")
        skipped = set(annotations["azure.workload.identity/skip-containers"].split(";"))
        self.assertTrue({"known-injected-sidecar", "desktop", "logger", "init-home-data", "cetiadmin-assets"}.issubset(skipped))
        self.assertNotIn("memory-assets", skipped)
        self.assertEqual(template["metadata"]["labels"]["azure.workload.identity/use"], "true")
        values["podAnnotations"]["azure.workload.identity/skip-containers"] = "cetirun"
        self.rejected(values)
        values.pop("podAnnotations")
        values["azureWorkloadIdentity"] = {"enabled": True, "clientId": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
        self.rejected(values)

    def test_api_port_and_ipv6_host_rules_are_exact(self):
        values = prepared_values()
        values["cetiadmin"]["kubernetes"]["apiServer"] = "https://kubernetes.default.svc:6443"
        values["cetiadmin"]["kubernetes"]["apiEgressCIDRs"] = ["2001:db8::10/128"]
        values["networkPolicy"] = {"allowedInternalCIDRs": [{"cidr": "2001:db8::10/128", "ports": [{"port": 6443}]}]}
        refresh_json(values)
        policy = next(item for item in objects(self.rendered(values)) if item["kind"] == "NetworkPolicy")
        rule = next(item for item in policy["spec"]["egress"] if item.get("to") == [{"ipBlock": {"cidr": "2001:db8::10/128"}}])
        self.assertEqual(rule["ports"], [{"protocol": "TCP", "port": 6443}])
        for ports in ([{"port": 443}], [{"port": 6443, "protocol": "UDP"}], [{"port": 6443}, {"port": 8443}]):
            changed = copy.deepcopy(values)
            changed["networkPolicy"]["allowedInternalCIDRs"][0]["ports"] = ports
            self.rejected(changed)

    def test_bad_network_and_shared_audience_fail(self):
        for field, value in (("allowedInternalCIDRs", []), ("allowedInternalCIDRs", [{"cidr": "0.0.0.0/0", "ports": [{"port": 443}]}]),
                             ("blockedCIDRs", BLOCKED), ("enabled", False)):
            values = prepared_values()
            values["networkPolicy"][field] = value
            self.rejected(values)
        values = prepared_values()
        values["sidecarScope"]["additionalEgressHosts"] = ["*/*"]
        self.rejected(values)
        values = prepared_values()
        values["cetiadmin"]["kubernetes"]["apiAudience"] = "cetirun-connections-mcp"
        self.rejected(values)

    def test_go_native_config_must_include_assets_without_losing_user_skills(self):
        values = prepared_values()
        result = self.rendered(values)
        configmap = next(item for item in objects(result) if item["kind"] == "ConfigMap")
        native = yaml.safe_load(configmap["data"]["hermes-config.yaml"])
        self.assertEqual(native["skills"]["external_dirs"], ["/home/vibe/own-skills", ASSETS + "/skills"])
        values["runtime"]["configFiles"]["hermes-config.yaml"] = "skills:\n  external_dirs: []\n"
        self.rejected(values)

    def test_final_prepared_runtime_needs_its_resolved_native_image(self):
        values = prepared_values()
        values["runtime"]["agent"].pop("image")
        self.rejected(values)

    def test_final_prepared_json_emits_only_validated_fields(self):
        values = prepared_values()
        expected = json.loads(values["cetiadmin"]["kubeconfig"])
        values["cetiadmin"]["kubeconfig"] = values["cetiadmin"]["kubeconfig"].replace(
            '"user": {', '"user": {"token": "' + PRIVATE + '"}, "user": {', 1,
        )
        configmap = next(item for item in objects(self.rendered(values)) if item["kind"] == "ConfigMap")
        self.assertEqual(json.loads(configmap["data"]["cetiadmin-kubeconfig.json"]), expected)


if __name__ == "__main__":
    unittest.main()
