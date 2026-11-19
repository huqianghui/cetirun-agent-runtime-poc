# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Exercise the complete entry point with real local Helm and synthetic APIs."""

import copy
import base64
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("full_deployment", ROOT / "scripts/cetirun_platform_deploy.py")
DEPLOY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DEPLOY)
FIXTURE = ROOT / "app/cetirun-platform/ci/deployment-example.yaml"
# Synthetic Skill Marketplace inputs; the example file only shows them commented out.
MARKETPLACE = {
    "repository": {"url": "https://git.example.invalid/example/skills.git", "ref": "main", "path": "skills",
                   "auth": "token", "existingSecret": "skills-repository", "syncInterval": "10m"},
    "storage": {"resourceGroup": "example-marketplace-rg", "storageAccount": "examplemarketplace",
                "containerName": "marketplace", "storageClassName": "example-marketplace"},
}
SSH_URL = "git@git.example.invalid:example/skills.git"
DELETE = object()


def operator_custom_runtime_fields():
    """Return the JSON fields that the Operator's strict customRuntime parser accepts."""
    source = (ROOT / "app/cetirun-operator/internal/controller/custom_provider.go").read_text()
    body = re.search(r"type CustomRuntimeConfig struct \{(.*?)\n\}", source, re.S)[1]
    return set(re.findall(r'json:"([^",]+)', body))


class SyntheticCluster(DEPLOY.Deployment):
    def __init__(self, config, values, scratch):
        super().__init__(config, values, scratch)
        self.commands, self.writes, self.applied = [], [], []
        self.objects, self.keys, self.installed = {}, {}, set()
        self.live = {}
        self.fail = ""
        self.mesh = {"metadata": {"name": config["istio"]["meshConfigMap"], "uid": "mesh-uid", "resourceVersion": "1"},
                     "data": {"mesh": yaml.safe_dump({"extensionProviders": [DEPLOY.MESH._oauth2_proxy_provider(self.oauth_service())], "trustDomain": "example.local"})}}
        # Deployments of the managed Istio that exists before the deployment.
        self.istio_deployments = {(config["istio"]["controlPlaneNamespace"], config["istio"]["controlPlaneDeployment"]),
                                  (config["istio"]["ingressNamespace"], config["istio"]["ingressDeployment"])}
        for namespace in (config["platform"]["namespace"], config["broker"]["namespace"]):
            self.objects[("namespace", "", namespace)] = {"name": namespace, "uid": f"uid-{namespace}", "labels": {}}

    def seed_references(self):
        for kind, namespace, name, key in self.references():
            self.objects[(kind.lower(), namespace, name)] = {"name": name, "uid": f"uid-{name}"}
            if kind == "Secret":
                self.keys.setdefault((namespace, name), set()).add(key)

    def run(self, command, *, env=None, input_text=None):
        self.commands.append(list(map(str, command)))
        if self.fail and self.fail in " ".join(map(str, command)):
            raise ValueError(f"Synthetic failure: {self.fail}")
        if command[0] == "helm" and command[1] in ("template", "dependency"):
            self.assert_offline(env)
            return super().run(command, env=env, input_text=input_text)
        if command[0] == "helm" and command[1:3] == ["show", "chart"]:
            return "name: example\nversion: 0.1.1\n"
        if command[0] == "docker":
            if command[1:4] != ["buildx", "imagetools", "inspect"]:
                raise AssertionError(f"Unexpected Docker mutation: {command}")
            return "Name: synthetic artifact\n"
        if command[0] == "openssl":
            if "-ext" in command:
                return "X509v3 Subject Alternative Name:\nURI:spiffe://cluster.local/ns/cetirun-system/sa/cetirun-connections-mcp\n"
            if "-startdate" in command:
                return "notBefore=Jan  1 00:00:00 2000 GMT\n"
            if "-checkhost" in command:
                return f"Hostname {self.config['domain']} does match certificate\n"
            return "public certificate check passed\n"
        if command[0] == sys.executable:
            if "ensure-oauth2-proxy-id-token-header.py" not in command[2]:
                raise AssertionError(command)
            self.writes.append(command)
            return ""
        if command[0] == "helm":
            if command[1:3] != ["--kube-context", self.config["target"]["context"]]:
                raise AssertionError(f"Unbound Helm: {command}")
            args = command[3:]
            namespace = args[args.index("--namespace") + 1]
            if args[0] == "list":
                release = args[args.index("--filter") + 1].strip("^$")
                return json.dumps([{"name": release, "status": "deployed"}] if release in self.installed else [])
            if args[:2] == ["get", "values"]:
                name = next(name for name in DEPLOY.CHARTS if self.config[name]["release"] == args[2])
                previous = next((values for component, values, _ in reversed(self.applied) if component == name), {})
                return json.dumps(previous)
            if args[:2] == ["upgrade", "--install"]:
                self.writes.append(command)
                release = args[2]
                name = next(name for name in DEPLOY.CHARTS if self.config[name]["release"] == release)
                values = json.loads(Path(args[args.index("--values") + 1]).read_text())
                self.applied.append((name, values, list(command)))
                bootstrap = name == "platform" and not values["cetirun-operator"]["sandboxAdmission"]["enforce"]
                self.installed.add(release)
                for doc in self.documents[name]:
                    if bootstrap and doc["kind"] in ("ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"):
                        continue
                    ns = "" if doc["kind"] in DEPLOY.CLUSTER_KINDS else doc["metadata"].get("namespace", namespace)
                    resource_name = doc["metadata"]["name"]
                    annotations = doc["metadata"].get("annotations", {})
                    # Helm records hook manifests separately. A failed hook Job
                    # remains in the cluster and only carries release ownership
                    # when the chart explicitly renders it, so do not give hooks
                    # the synthetic ownership annotations used for regular
                    # release resources.
                    owner_annotations = (copy.deepcopy(annotations) if "helm.sh/hook" in annotations else {
                        "meta.helm.sh/release-name": release,
                        "meta.helm.sh/release-namespace": namespace,
                    })
                    self.objects[(doc["kind"].lower(), ns, resource_name)] = {
                        "name": resource_name, "uid": f"uid-{resource_name}",
                        "annotations": owner_annotations,
                    }
                    self.live[(doc["kind"].lower(), ns, resource_name)] = copy.deepcopy(doc)
                    if doc["kind"] == "Secret":
                        self.keys.setdefault((ns, resource_name), set(doc.get("data", {})))
                return "Release deployed\n"
            raise AssertionError(f"Unexpected Helm operation: {command}")
        if command[0] != "kubectl" or command[1:4] != ["--context", self.config["target"]["context"], "--request-timeout=15s"]:
            raise AssertionError(f"Unexpected or unbound command: {command}")
        args = command[4:]
        namespace = args[args.index("--namespace") + 1] if "--namespace" in args else ""
        if args[:2] == ["config", "view"]:
            return self.config["target"]["cluster"] + "\n" + self.config["target"]["server"]
        if args[0] == "version":
            return json.dumps({key: {"major": "1", "minor": "37"} for key in ("clientVersion", "serverVersion")})
        if args[:2] == ["rollout", "status"]:
            return "successfully rolled out\n"
        if args[:2] == ["get", "pods"]:
            return "controller broker auth-sdk connections-mcp portal istio-proxy\n"
        if args[:2] == ["get", "agents.cetirun.microsoft.com"]:
            output = args[args.index("-o") + 1]
            fields = (("metadata.name", "spec.agent.type", "spec.agent.image") if "spec.agent.image" in output else
                      ("metadata.name", "spec.agent.type", "spec.team", "spec.sandboxRef",
                       "spec.helm.values.serviceAccount.name", "spec.helm.values.serviceAccount.role",
                       "spec.helm.values.serviceAccount.automountToken"))
            rows = []
            for (kind, _, _), agent in self.objects.items():
                if kind != "agent":
                    continue
                row = [DEPLOY.get_value(agent, field) for field in fields]
                rows.append("\t".join(str(value).lower() if isinstance(value, bool) else str(value or "") for value in row))
            return "\n".join(rows)
        if args[0] == "get":
            kind, name = args[1].lower(), args[2]
            if kind == "gateways.networking.istio.io":
                kind = "gateway"
            output = args[args.index("-o") + 1]
            if kind == "deployment" and "containers[" in output:
                document = self.live.get((kind, namespace, name), {})
                containers = DEPLOY.get_value(document, "spec.template.spec.containers") or []
                import re
                container_name = re.search(r'@\.name=="([^"]+)"', output)[1]
                container = next((item for item in containers if item["name"] == container_name), {})
                for field in ("args", "command", "ports", "envFrom"):
                    if output.endswith("." + field + "}"):
                        return json.dumps(container.get(field, []))
                if output.endswith(".image}"):
                    return container.get("image", "")
                if output.endswith(".env[*].name}"):
                    return " ".join(item["name"] for item in container.get("env", []))
                environment_name = re.findall(r'@\.name=="([^"]+)"', output)[-1]
                entry = next((item for item in container.get("env", []) if item["name"] == environment_name), {})
                if output.endswith(".value}"):
                    return entry.get("value", "")
                if output.endswith(".valueFrom}"):
                    return json.dumps(entry["valueFrom"]) if "valueFrom" in entry else ""
                if output.startswith("jsonpath-as-json="):
                    return json.dumps([entry] if entry else [])
                return json.dumps(entry) if entry else ""
            document = self.live.get((kind, namespace, name), {})
            for field in ("spec", "spec.selector", "spec.template.spec.serviceAccountName", "spec.template.metadata.labels",
                          "spec.template.spec.volumes", "spec.accessModes", "spec.storageClassName"):
                if output == "jsonpath={." + field + "}":
                    value = DEPLOY.get_value(document, field)
                    return value if isinstance(value, str) else json.dumps(value)
            if output == r"jsonpath={.metadata.annotations.azure\.workload\.identity/client-id}":
                return document.get("metadata", {}).get("annotations", {}).get("azure.workload.identity/client-id", "")
            if kind == "deployment" and "peer-uid" in output:
                return (DEPLOY.get_value(self.live.get((kind, namespace, name), {}), "spec.template.metadata.labels") or {}).get("sandbox.cetirun.microsoft.com/peer-uid", "")
            if "webhooks}" in output:
                return json.dumps(self.live.get((kind, namespace, name), {}).get("webhooks", []))
            if kind == "agent" and output.startswith("go-template="):
                agent = self.objects[(kind, namespace, name)]
                return "protected" if DEPLOY.get_value(agent, "spec.sandboxRef") or DEPLOY.get_value(agent, "status.sandbox") else ""
            if kind == "team" and output.startswith("go-template="):
                team = self.objects.get((kind, namespace, name))
                if team is None:
                    raise ValueError("Synthetic missing Team")
                return "protected" if any(DEPLOY.get_value(team, "spec." + field) for field in ("agentPolicyRef", "sandboxRefs", "defaultSandboxRef")) else ""
            if (kind, name) == ("namespace", "kube-system"):
                return json.dumps({"uid": self.config["target"]["clusterUID"]})
            if (kind == "customresourcedefinition" and name.endswith(".istio.io")) or (kind == "serviceaccount" and name == self.config["istio"]["ingressServiceAccount"]):
                return json.dumps({"name": name, "uid": "external-prerequisite"})
            if kind == "deployment" and (namespace, name) in self.istio_deployments:
                return json.dumps({"name": name, "uid": "external-prerequisite"})
            if kind == "configmap" and name == self.config["istio"]["meshConfigMap"]:
                return json.dumps(self.mesh) if self.mesh else ""
            if any(str(arg).startswith("go-template=") for arg in args):
                return "\n".join(sorted(self.keys.get((namespace, name), set())))
            if any(str(arg).startswith("jsonpath={.data.") for arg in args):
                return base64.b64encode(b"synthetic public certificate").decode()
            metadata = self.objects.get((kind, namespace, name))
            return json.dumps(metadata) if metadata else ""
        if args[:2] == ["create", "namespace"]:
            self.writes.append(command)
            self.objects[("namespace", "", args[2])] = {"name": args[2], "uid": "new-namespace", "labels": {}}
            return "created\n"
        if args[:2] == ["label", "namespace"]:
            self.writes.append(command)
            self.objects[("namespace", "", args[2])].setdefault("labels", {})["istio.io/rev"] = args[3].split("=", 1)[1]
            return "labeled\n"
        if args[:2] == ["create", "--dry-run=server"]:
            assert json.loads(input_text)["kind"] == "Team"
            return "validated (server dry run)\n"
        raise AssertionError(f"Unexpected Kubernetes write: {command}")

    @staticmethod
    def assert_offline(env):
        if not env or env.get("KUBECONFIG") != "/dev/null" or env.get("HELM_REGISTRY_CONFIG") != "/dev/null":
            raise AssertionError("Render can use real cluster or registry configuration")


class DeploymentFixture:
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-full-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.config = yaml.safe_load(FIXTURE.read_text())
        self.config["platform"]["values"]["cetirun-operator"]["cetiadmin"] = {"enabled": False}

    def deployment(self, config=None, name="charts"):
        path = self.root / ("environment.json" if name == "charts" else f"{name}.json")
        path.write_text(json.dumps(self.config if config is None else config))
        config, values = DEPLOY.configuration(path)
        deployment = SyntheticCluster(config, values, self.root / name)
        deployment.render()
        deployment.seed_references()
        return deployment

    def configure(self, config):
        path = self.root / "configuration.json"
        path.write_text(json.dumps(config))
        return DEPLOY.configuration(path)


@unittest.skipUnless(shutil.which("helm"), "requires existing Helm")
class ReleaseCoordinateTests(DeploymentFixture, unittest.TestCase):
    def test_missing_coordinates_use_defaults_and_invalid_release_is_rejected(self):
        self.config["release"].pop("registry")
        self.config["release"].pop("version")
        config, _ = self.configure(self.config)
        self.assertEqual(config["release"]["registry"], "mcr.microsoft.com/aks/ai-runtime/cetirun")
        self.assertEqual(config["release"]["version"], DEPLOY.RELEASE.VERSIONS.top_level_yaml(ROOT / "app/cetirun-chart/Chart.yaml", "version"))
        self.assertEqual(config["_releaseOrigins"], {"registry": "source default", "version": "source default"})
        for invalid in (None, [], "invalid"):
            self.config["release"] = invalid
            with self.assertRaisesRegex(ValueError, "release must be an object"):
                self.configure(self.config)

    def test_explicit_coordinates_render_without_rewriting_environment(self):
        path = self.root / "environment.json"
        path.write_text(json.dumps(self.config))
        before = path.read_bytes()
        registry, version = "registry.example.com:5000/team/test", "1.2.3-dev.1"
        config, values = DEPLOY.configuration(path, registry=registry, version=version)
        self.assertEqual(config["release"]["registry"], registry)
        self.assertEqual(config["release"]["version"], version)
        deployment = SyntheticCluster(config, values, self.root / "render")
        deployment.render()
        self.assertFalse(deployment.writes)
        self.assertEqual(values["platform"]["global"]["registry"], registry)
        self.assertEqual(values["platform"]["global"]["version"], version)
        for name, chart in deployment.charts.items():
            metadata = yaml.safe_load((chart / "Chart.yaml").read_text())
            self.assertEqual(metadata["version"], version, name)
            self.assertEqual(metadata["appVersion"], version, name)
        self.assertEqual(values["broker"]["image"]["repository"], registry + "/cetirun-agent-broker")
        self.assertEqual(values["broker"]["image"]["tag"], version)
        self.assertEqual(path.read_bytes(), before)

    def test_coordinate_override_does_not_clear_conflicting_pins(self):
        self.config["platform"]["values"]["global"] = {"version": "0.1.0"}
        path = self.root / "environment.json"
        path.write_text(json.dumps(self.config))
        with self.assertRaisesRegex(ValueError, "conflicts"):
            DEPLOY.configuration(path, version="1.2.3-dev.1")

    def test_make_and_cli_override_precedence(self):
        path = self.root / "environment.json"
        path.write_text(json.dumps(self.config))
        env = {key: value for key, value in os.environ.items()
               if key not in ("VERSION", "REGISTRY", "MAKEFLAGS", "MFLAGS", "MAKEOVERRIDES", "MAKELEVEL")}
        env.update(KUBECONFIG=os.devnull, HELM_REGISTRY_CONFIG=os.devnull, HELM_REPOSITORY_CONFIG=os.devnull)
        cases = (([], {}, self.config["release"]["registry"], self.config["release"]["version"]),
                 ([], {"VERSION": "1.2.3-dev.1", "REGISTRY": "registry.example.com/env/path"}, "registry.example.com/env/path", "1.2.3-dev.1"),
                 (["VERSION=2.3.4-dev.2", "REGISTRY=registry.example.com/cli/path"],
                  {"VERSION": "1.2.3-dev.1", "REGISTRY": "registry.example.com/env/path"}, "registry.example.com/cli/path", "2.3.4-dev.2"))
        for extra, overrides, registry, version in cases:
            with self.subTest(extra=extra, overrides=overrides):
                result = subprocess.run(["make", "--silent", "deploy-plan", f"DEPLOY_CONFIG={path}", *extra],
                                        cwd=ROOT, env={**env, **overrides}, capture_output=True, text=True, timeout=60)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                plan = json.loads(result.stdout)
                self.assertEqual(plan["release"]["registry"], registry)
                self.assertEqual(plan["release"]["version"], version)
                for key in ("registry", "version"):
                    origin = f"--{key}" if extra else f"environment {key.upper()}" if overrides else "environment file"
                    self.assertEqual(plan["releaseOrigins"][key], origin)

    def test_execute_reports_coordinates_before_any_external_operation(self):
        deployment = self.deployment()
        original = deployment.execute_platform
        with patch("sys.stdout", new_callable=io.StringIO) as stdout:
            def execute_platform():
                for key in ("registry", "version"):
                    self.assertIn(f"Deployment {key}: {deployment.config['release'][key]} (from environment file)", stdout.getvalue())
                self.assertFalse(deployment.writes)
                original()
            with patch.object(deployment, "execute_platform", side_effect=execute_platform):
                deployment.execute()
        self.assertTrue(deployment.writes)


class DeploymentInputTests(DeploymentFixture, unittest.TestCase):
    def test_optional_image_exclusion_selects_artifacts(self):
        config = copy.deepcopy(self.config)
        config["release"]["excludeImages"] = ["cetirun-vscode"]
        approved = config["platform"]["values"]["cetirun-operator"]["customRuntime"]["approvedImages"]
        config["platform"]["values"]["cetirun-operator"]["customRuntime"]["approvedImages"] = [
            image for image in approved if "/cetirun-vscode@" not in image
        ]
        deployment = self.deployment(config, "optional-image")
        artifacts = deployment.plan()["artifacts"]
        self.assertEqual(len(artifacts["images"]), 13)
        self.assertNotIn("cetirun-vscode", artifacts["images"])
        deployment.preflight()
        inspected = [command[-1] for command in deployment.commands if command[:4] == ["docker", "buildx", "imagetools", "inspect"]]
        excluded = f"{config['release']['registry']}/cetirun-vscode:{config['release']['version']}"
        self.assertNotIn(excluded, inspected)
        self.assertEqual(sum(reference.endswith(":" + config["release"]["version"]) for reference in inspected), 13)
        self.assertFalse(any(command[4:6] == ["get", "agents.cetirun.microsoft.com"] for command in deployment.commands))

    def test_optional_image_exclusion_rejects_referenced_image_before_tools(self):
        config = copy.deepcopy(self.config)
        config["release"]["excludeImages"] = ["cetirun-vscode"]
        with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
            with self.assertRaisesRegex(ValueError, "excludes image still referenced.*cetirun-vscode"):
                self.configure(config)

    def test_current_custom_config_does_not_require_retired_model_fields(self):
        config = copy.deepcopy(self.config)
        custom = config["platform"]["values"]["cetirun-operator"]["customRuntime"]
        custom.pop("agentManagedModel", None)
        custom.pop("gatewayImages", None)
        before = copy.deepcopy(config)
        with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("configuration ran a command")):
            _, values = self.configure(config)
        emitted = values["platform"]["cetirun-operator"]["customRuntime"]
        self.assertEqual(set(emitted), {
            "enabled", "approvedImages", "imageUsers", "storageClass", "storageSize",
            "accessMode", "fsGroup", "context",
        })
        self.assertTrue(emitted["enabled"])
        self.assertEqual(emitted["approvedImages"], custom["approvedImages"])
        self.assertEqual(config, before)

    def test_retired_custom_model_fields_fail_before_external_commands(self):
        for field, value in (("agentManagedModel", False), ("agentManagedModel", True),
                             ("gatewayImages", ["registry.example.com/approved@sha256:" + "a" * 64])):
            with self.subTest(field=field, value=value):
                config = copy.deepcopy(self.config)
                custom = config["platform"]["values"]["cetirun-operator"]["customRuntime"]
                custom.pop("agentManagedModel", None)
                custom.pop("gatewayImages", None)
                custom[field] = value
                with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("configuration ran a command")):
                    with self.assertRaisesRegex(ValueError, f"Remove customRuntime.{field} from the environment file"):
                        self.configure(config)

    def test_invalid_yaml_does_not_echo_credentials(self):
        marker = "sk-synthetic-marker"
        path = self.root / "malformed.yaml"
        path.write_text("providerKey: '" + marker + "\n")
        result = subprocess.run(
            [sys.executable, "-B", str(ROOT / "scripts/cetirun_platform_deploy.py"), "--config", str(path), "--plan"],
            env={"PATH": "/no-tools"}, capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 1)
        self.assertNotIn(marker, result.stdout + result.stderr)
        self.assertIn("YAML", result.stderr)

    def test_invalid_rendered_yaml_does_not_echo_values(self):
        marker = "sk-rendered-marker"
        with self.assertRaises(yaml.YAMLError) as failure:
            yaml.safe_load("api_key: '" + marker)
        fixture = self.root / "explicit-disabled.json"
        fixture.write_text(json.dumps(self.config))
        with patch.object(DEPLOY.Deployment, "render", side_effect=failure.exception), \
                patch("sys.stdout", new_callable=io.StringIO) as stdout, \
                patch("sys.stderr", new_callable=io.StringIO) as stderr:
            self.assertEqual(DEPLOY.main(["--config", str(fixture), "--plan"]), 1)
        self.assertNotIn(marker, stdout.getvalue() + stderr.getvalue())
        self.assertIn("YAML", stderr.getvalue())

    def test_target_server_rejects_embedded_credentials_before_tools(self):
        for authority in ("user@kubernetes.example.invalid", "user:password@kubernetes.example.invalid"):
            with self.subTest(authority=authority):
                config = copy.deepcopy(self.config)
                config["target"]["server"] = "https://" + authority
                with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
                    with self.assertRaisesRegex(ValueError, "target.server"):
                        self.configure(config)

    def test_cli_failure_has_nonzero_status_and_no_success_message(self):
        file = self.root / "invalid.yaml"
        file.write_text("target: {}\n")
        result = subprocess.run([sys.executable, "-B", str(ROOT / "scripts/cetirun_platform_deploy.py"), "--config", str(file)],
                                env={"PATH": "/no-tools"}, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("target.context", result.stderr)
        self.assertNotIn("ready", result.stdout)

    def test_custom_runtime_fields_unknown_to_the_operator_stop_before_tools(self):
        image = self.config["platform"]["values"]["cetirun-operator"]["customRuntime"]["approvedImages"][0]
        for field, value, removed in (("agentManagedModel", True, True), ("gatewayImages", [image], True),
                                      ("imageAllowlist", [image], False)):
            with self.subTest(field=field):
                config = copy.deepcopy(self.config)
                config["platform"]["values"]["cetirun-operator"]["customRuntime"][field] = value
                with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
                    with self.assertRaisesRegex(ValueError, f"Remove customRuntime.{field} from the environment file") as failure:
                        self.configure(config)
                self.assertEqual("AI Gateway" in str(failure.exception), removed)

    def test_operator_chart_custom_runtime_has_only_fields_the_operator_accepts(self):
        chart = yaml.safe_load((ROOT / "app/cetirun-operator/chart/values.yaml").read_text())["customRuntime"]
        self.assertLessEqual(set(chart), operator_custom_runtime_fields())

    def test_primary_runtime_image_overrides_fail_before_external_commands(self):
        for field in ("image", "hermes.image", "openclaw.image"):
            for value in ({}, {"digest": "sha256:" + "a" * 64}):
                with self.subTest(field=field, value=value):
                    config = copy.deepcopy(self.config)
                    DEPLOY.set_value(config["platform"]["values"]["cetirun-operator"], "cetirun.helmValues." + field, value)
                    with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
                        with self.assertRaisesRegex(ValueError, "Remove primary image overrides"):
                            self.configure(config)

    def test_helper_image_pins_remain_valid(self):
        config = copy.deepcopy(self.config)
        for field in ("desktop.image.digest", "ttyd.image.digest"):
            DEPLOY.set_value(config["platform"]["values"]["cetirun-operator"], "cetirun.helmValues." + field, "sha256:" + "a" * 64)
        _, values = self.configure(config)
        for field in ("desktop.image.digest", "ttyd.image.digest"):
            self.assertEqual(DEPLOY.get_value(values["platform"]["cetirun-operator"]["cetirun"]["helmValues"], field), "sha256:" + "a" * 64)

    def test_failed_command_names_the_step_without_output_files_or_context(self):
        config, values = self.configure(self.config)
        deployment = DEPLOY.Deployment(config, values, self.root)
        marker = "synthetic-tool-output"
        helper = [sys.executable, "-B", str(ROOT / "scripts/ensure-oauth2-proxy-id-token-header.py"), "--context", "example-context",
                  "--namespace", "aks-istio-system", "--name", "istio-shared-configmap-asm-example", "--service", "oauth2-proxy.example"]
        cases = (
            (lambda: deployment.kubectl("rollout", "status", "deployment/cetirun-operator", "--namespace", "cetirun-system", "--timeout=600s"),
             "kubectl rollout status deployment/cetirun-operator --namespace cetirun-system --timeout=600s failed with exit 1;"),
            (lambda: deployment.kubectl("get", "secret", "gateway-provider-keys", "--namespace", "cetirun-system", "-o", "go-template={{.data}}"),
             "kubectl get secret gateway-provider-keys --namespace cetirun-system failed with exit 1;"),
            (lambda: deployment.helm("upgrade", "--install", "cetirun-platform", str(self.root / "app/cetirun-platform"), "--namespace",
                                     "cetirun-system", "--values", str(self.root / "platform-values.json"), "--timeout", "10m"),
             "helm upgrade cetirun-platform --namespace cetirun-system --timeout 10m failed with exit 1;"),
            (lambda: deployment.run(helper),
             "ensure-oauth2-proxy-id-token-header.py --namespace aks-istio-system --name istio-shared-configmap-asm-example failed with exit 1;"),
        )
        with patch.object(DEPLOY.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, marker, marker)):
            for call, expected in cases:
                with self.subTest(expected=expected):
                    with self.assertRaises(ValueError) as failure:
                        call()
                    message = str(failure.exception)
                    self.assertTrue(message.startswith(expected), message)
                    for private in (marker, "example-context", str(self.root), "{{", "oauth2-proxy.example"):
                        self.assertNotIn(private, message)

    def test_offline_render_names_the_charts_refusal_and_nothing_else(self):
        config, values = self.configure(self.config)
        deployment = DEPLOY.Deployment(config, values, self.root)
        refusal = ("Error: execution error at (cetirun-platform/charts/cetirun-ai-gateway/templates/configmap.yaml:21:4): "
                   "auto router auto sends requests to elsewhere, which is not a model configured in models.\n"
                   "Use --debug flag to render out invalid YAML\n")
        render = ["helm", "template", "cetirun-platform", str(self.root / "app/cetirun-platform"), "--namespace", "cetirun-system",
                  "--values", str(self.root / "platform-values.json")]
        with patch.object(DEPLOY.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", refusal)):
            with self.assertRaises(ValueError) as failure:
                deployment.run(render)
            self.assertEqual(str(failure.exception),
                             "helm template cetirun-platform --namespace cetirun-system failed with exit 1; "
                             "cetirun-platform/charts/cetirun-ai-gateway/templates/configmap.yaml:21:4: auto router auto "
                             "sends requests to elsewhere, which is not a model configured in models.; deployment did not complete")
            # A live Helm or kubectl call keeps its output to itself, a chart refusal included.
            with self.assertRaises(ValueError) as failure:
                deployment.helm("upgrade", "--install", "cetirun-platform", str(self.root / "app/cetirun-platform"))
            self.assertNotIn("elsewhere", str(failure.exception))
        other = "Error: YAML parse error on cetirun-platform/templates/secret.yaml: sk-synthetic-marker\n"
        with patch.object(DEPLOY.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", other)):
            with self.assertRaises(ValueError) as failure:
                deployment.run(render)
            self.assertNotIn("sk-synthetic-marker", str(failure.exception))


@unittest.skipUnless(shutil.which("helm"), "requires existing Helm for real local chart rendering")
class FullDeploymentTests(DeploymentFixture, unittest.TestCase):

    def test_rendered_operator_custom_config_matches_current_inputs(self):
        config = copy.deepcopy(self.config)
        custom = config["platform"]["values"]["cetirun-operator"]["customRuntime"]
        custom.pop("agentManagedModel", None)
        custom.pop("gatewayImages", None)
        deployment = self.deployment(config)
        operator = next(document for document in deployment.documents["platform"]
                        if document["kind"] == "Deployment" and document["metadata"]["name"] == "cetirun-operator")
        manager = next(container for container in operator["spec"]["template"]["spec"]["containers"]
                       if container["name"] == "manager")
        raw = next(item["value"] for item in manager["env"] if item["name"] == "CETIRUN_CUSTOM_RUNTIME")
        self.assertEqual(json.loads(raw), deployment.values["platform"]["cetirun-operator"]["customRuntime"])
        self.assertNotIn("agentManagedModel", json.loads(raw))
        self.assertNotIn("gatewayImages", json.loads(raw))
        self.assertFalse(deployment.writes)

    def test_plan_omits_credential_references_and_extra_input_fields(self):
        marker = "private-provider-reference"
        self.config["platform"]["values"]["cetirun-ai-gateway"]["providerCredentials"]["existingSecret"] = marker
        self.config["target"]["credential"] = "private-target-value"
        self.config["release"]["credential"] = "private-release-value"
        deployment = self.deployment()
        plan = deployment.plan()
        encoded = json.dumps(plan)
        for private in (marker, "private-target-value", "private-release-value"):
            self.assertNotIn(private, encoded)
        self.assertEqual(set(plan["target"]), {"context", "cluster", "server", "clusterUID"})
        self.assertEqual(set(plan["release"]), {"registry", "version", "gitSHA"})
        self.assertGreater(plan["referenceCounts"]["Secret"], 0)
        self.assertGreater(plan["referenceCounts"]["StorageClass"], 0)
        self.assertEqual(deployment.writes, [])

    def test_release_query_flags_are_accepted_by_local_helm(self):
        deployment = self.deployment()
        deployment.preflight()
        queries = [command for command in deployment.commands if command[:4] == ["helm", "--kube-context", "example-context", "list"]]
        self.assertEqual(len(queries), 3)
        for command in queries:
            result = subprocess.run([*command, "--help"], capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("--pending", command)
        self.assertEqual(deployment.writes, [])

    def test_pending_release_stops_before_writes(self):
        deployment = self.deployment()
        original = deployment.run
        def run(command, **kwargs):
            if command[:4] == ["helm", "--kube-context", "example-context", "list"]:
                return json.dumps([{"name": "cetirun-istio", "status": "pending-upgrade"}])
            return original(command, **kwargs)
        deployment.run = run
        with self.assertRaisesRegex(ValueError, "unfinished operation"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def external_deployment(self, name):
        deployment = self.deployment(name=name)
        deployment.upgrade("istio")
        namespace = deployment.config["istio"]["namespace"]
        deployment.objects[("namespace", "", namespace)] = {"name": namespace, "uid": "existing-istio-namespace"}
        deployment.config["istio"]["mode"] = "external"
        deployment.writes.clear()
        deployment.applied.clear()
        return deployment

    def test_external_istio_missing_objects_fail_before_writes(self):
        for kind in ("AuthorizationPolicy", "Gateway", "EnvoyFilter", "VirtualService", "Deployment", "Service", "ServiceAccount"):
            with self.subTest(kind=kind):
                deployment = self.external_deployment("external-missing-" + kind)
                document = next(doc for doc in deployment.documents["istio"] if doc["kind"] == kind)
                key = (kind.lower(), document["metadata"]["namespace"], document["metadata"]["name"])
                deployment.objects.pop(key)
                deployment.live.pop(key)
                with self.assertRaisesRegex(ValueError, "External Istio.*missing"):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])

    def test_external_istio_public_spec_drift_fails_before_writes(self):
        mutations = {
            "Gateway": lambda spec: spec["servers"][0].update(hosts=["different.example.invalid"]),
            "AuthorizationPolicy": lambda spec: spec.update(action="ALLOW"),
            "EnvoyFilter": lambda spec: spec.update(configPatches=[]),
            "VirtualService": lambda spec: spec.update(hosts=["different.example.invalid"]),
            "Service": lambda spec: spec.update(selector={"app": "different-proxy"}),
        }
        for kind, mutate in mutations.items():
            with self.subTest(kind=kind):
                deployment = self.external_deployment("external-drift-" + kind)
                document = next(doc for doc in deployment.documents["istio"] if doc["kind"] == kind)
                current = deployment.live[(kind.lower(), document["metadata"]["namespace"], document["metadata"]["name"])]
                mutate(current["spec"])
                with self.assertRaisesRegex(ValueError, "External Istio.*(differ|mismatch)"):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])

    def test_external_oauth2_wiring_drift_fails_before_writes(self):
        for field in ("args", "image", "command", "serviceAccountName", "cookieReference", "environment", "envFrom", "workloadIdentity", "serviceAccountIdentity"):
            with self.subTest(field=field):
                deployment = self.external_deployment("external-oauth-" + field)
                current = deployment.live[("deployment", "cetirun-istio", "oauth2-proxy")]
                spec = current["spec"]["template"]["spec"]
                container = next(item for item in spec["containers"] if item["name"] == "oauth2-proxy")
                if field == "args":
                    container["args"].append("--skip-auth-route=GET=^/api/")
                elif field == "image":
                    container["image"] = "registry.example.com/different-proxy:1"
                elif field == "command":
                    container["command"] = ["/different-entrypoint"]
                elif field == "serviceAccountName":
                    spec["serviceAccountName"] = "different-proxy"
                elif field == "cookieReference":
                    entry = next(item for item in container["env"] if item["name"] == "COOKIE_SECRET")
                    entry["valueFrom"]["secretKeyRef"]["name"] = "different-cookie"
                elif field == "environment":
                    container["env"].append({"name": "OAUTH2_PROXY_SKIP_AUTH_ROUTE", "value": "GET=^/api/"})
                elif field == "envFrom":
                    container["envFrom"] = [{"configMapRef": {"name": "different-proxy-config"}}]
                elif field == "workloadIdentity":
                    current["spec"]["template"]["metadata"]["labels"]["azure.workload.identity/use"] = "false"
                else:
                    deployment.live[("serviceaccount", "cetirun-istio", "oauth2-proxy")]["metadata"]["annotations"]["azure.workload.identity/client-id"] = "different-client"
                with self.assertRaisesRegex(ValueError, "External Istio.*(differ|mismatch)"):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])

    def test_matching_external_istio_remains_read_only(self):
        deployment = self.external_deployment("external-matching")
        service = deployment.live[("service", "cetirun-istio", "oauth2-proxy")]
        service["spec"].update(type="ClusterIP", clusterIP="10.0.0.1")
        service["spec"]["ports"][0]["protocol"] = "TCP"
        before = {key: copy.deepcopy(doc) for key, doc in deployment.live.items()}
        deployment.execute()
        self.assertNotIn("istio", [name for name, _, _ in deployment.applied])
        self.assertFalse(any(command[0] == sys.executable for command in deployment.writes))
        for key, document in before.items():
            self.assertEqual(deployment.live[key], document)
        cookie_reads = [command[-1] for command in deployment.commands if "oauth2-proxy-credentials" in command and "-o" in command]
        self.assertTrue(cookie_reads)
        self.assertTrue(all(".data." not in output and output != "json" for output in cookie_reads))

    def test_external_istio_read_failure_stops_before_writes(self):
        deployment = self.external_deployment("external-read-failure")
        deployment.fail = "jsonpath={.spec}"
        with self.assertRaisesRegex(ValueError, "Synthetic failure"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_default_plan_covers_all_releases_and_supported_components(self):
        deployment = self.deployment()
        plan = deployment.plan()
        self.assertEqual(len(plan["artifacts"]["images"]), 14)
        self.assertEqual(len(plan["artifacts"]["charts"]), 9)
        self.assertEqual(set(plan["releases"]["istio"]["workloads"]), {"oauth2-proxy", "oauth2-proxy-sessions"})
        self.assertEqual(plan["releases"]["broker"]["namespace"], "cetirun-credentials")
        self.assertEqual(set(plan["releases"]["platform"]["workloads"]), {
            "cetirun-operator", "cetirun-portal", "cetirun-ai-gateway", "cetirun-ai-gateway-postgresql",
            "cetirun-ai-gateway-valkey", "cetirun-connections-mcp", "cetirun-sandbox-admission"})
        platform = deployment.values["platform"]
        self.assertTrue(platform["global"]["portalChat"]["enabled"])
        self.assertTrue(platform["cetirun-operator"]["customRuntime"]["enabled"])
        self.assertFalse(platform["cetirun-operator"]["sandboxSources"]["nativeExecution"])
        self.assertEqual(platform["cetirun-operator"]["connectionsMCP"]["runtimeTypes"], ["hermes", "openclaw"])
        # The Skill Marketplace is optional and off without its input block.
        for switch in DEPLOY.SKILL_MARKETPLACE_SWITCHES:
            self.assertIs(DEPLOY.get_value(platform, switch), False, switch)
        self.assertNotIn("Skill Marketplace repository access and Team publication", plan["notQualified"])
        # A new installation waits for the same workloads after its bootstrap.
        self.assertEqual({doc["metadata"]["name"] for doc in deployment.bootstrap_documents if doc["kind"] in ("Deployment", "StatefulSet")},
                         set(plan["releases"]["platform"]["workloads"]))
        self.assertFalse(deployment.writes)
        self.assertTrue(all(c[0] == "helm" for c in deployment.commands))

    def test_missing_input_or_namespace_conflict_makes_no_external_call(self):
        for path in ("target.context", "release.gitSHA", "ownerAlias", "sharedSkills.storageAccount",
                     "identity.portalClientId", "platform.values.cetirun-operator.sandboxAdmission.tlsSecretName",
                     "platform.values.cetirun-operator.customRuntime.storageClass"):
            with self.subTest(path=path):
                config = copy.deepcopy(self.config)
                parts = path.split(".")
                target = config
                for key in parts[:-1]:
                    target = target[key]
                target.pop(parts[-1])
                file = self.root / "invalid.json"
                file.write_text(json.dumps(config))
                with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
                    self.assertEqual(DEPLOY.main(["--config", str(file)]), 1)
        self.config["broker"]["namespace"] = "cetirun-system"
        with self.assertRaisesRegex(ValueError, "separate namespaces"):
            self.deployment()

    def test_install_then_repeat_preserves_objects_and_keeps_enforcement(self):
        deployment = self.deployment()
        deployment.execute()
        self.assertEqual([name for name, _, _ in deployment.applied], ["istio", "broker", "platform", "platform"])
        bootstrap, final = [values for name, values, _ in deployment.applied if name == "platform"]
        self.assertFalse(bootstrap["cetirun-operator"]["sandboxAdmission"]["enforce"])
        self.assertTrue(final["cetirun-operator"]["sandboxAdmission"]["enforce"])
        for _, values, command in deployment.applied:
            self.assertNotIn("offline-render-only", json.dumps(values))
            self.assertNotIn("--take-ownership", command)
            self.assertNotIn("--force", command)
        first_keys = copy.deepcopy(deployment.keys)
        deployment.applied.clear()
        deployment.writes.clear()
        deployment.execute()
        self.assertEqual([name for name, _, _ in deployment.applied], ["istio", "broker", "platform"])
        self.assertEqual(first_keys, deployment.keys)
        self.assertFalse(any(c[0] == "kubectl" for c in deployment.writes))
        self.assertTrue(all("current-context" not in c and "use-context" not in c for c in deployment.commands))
        self.assertTrue(all("--context" in c for c in deployment.commands if c[0] == "kubectl"))

    def test_missing_reference_or_foreign_owner_stops_before_writes(self):
        deployment = self.deployment()
        missing = ("secret", "cetirun-system", "connections-mcp-ca")
        removed = deployment.objects.pop(missing)
        with self.assertRaisesRegex(ValueError, "reference is missing"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])
        deployment.objects[missing] = removed
        deployment.objects[("deployment", "cetirun-system", "cetirun-portal")] = {"annotations": {"meta.helm.sh/release-name": "another-release"}}
        with self.assertRaisesRegex(ValueError, "Refusing to adopt"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def cetiadmin_hook(self, deployment, mode="check"):
        name = "cetirun-cetiadmin-" + mode
        return next(doc for doc in deployment.documents["platform"]
                    if doc["kind"] == "Job" and doc["metadata"]["name"] == name)

    def test_retained_failed_cetiadmin_hook_with_exact_chart_ownership_retries(self):
        deployment = self.deployment()
        job = self.cetiadmin_hook(deployment)
        namespace, name = job["metadata"]["namespace"], job["metadata"]["name"]
        deployment.objects[("job", namespace, name)] = {
            "name": name,
            "uid": "retained-cetiadmin-check-uid",
            "annotations": copy.deepcopy(job["metadata"]["annotations"]),
            "status": {"failed": 1},
        }
        deployment.execute()
        self.assertTrue(any(release == "platform" for release, _, _ in deployment.applied))
        current = deployment.objects[("job", namespace, name)]
        self.assertEqual(current["annotations"], job["metadata"]["annotations"])

    def test_retained_cetiadmin_hook_from_another_release_or_namespace_stops_before_writes(self):
        for field, value in (("meta.helm.sh/release-name", "another-release"),
                             ("meta.helm.sh/release-namespace", "another-namespace")):
            with self.subTest(field=field):
                deployment = self.deployment(name=field.rsplit("/", 1)[-1])
                job = self.cetiadmin_hook(deployment)
                namespace, name = job["metadata"]["namespace"], job["metadata"]["name"]
                annotations = copy.deepcopy(job["metadata"]["annotations"])
                annotations[field] = value
                deployment.objects[("job", namespace, name)] = {
                    "name": name, "uid": "foreign-cetiadmin-check-uid", "annotations": annotations,
                }
                with self.assertRaisesRegex(ValueError, "Refusing to adopt Job"):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])

    def test_failed_rollout_stops_later_releases(self):
        deployment = self.deployment()
        deployment.fail = "rollout status deployment/cetirun-agent-broker"
        with self.assertRaisesRegex(ValueError, "Synthetic failure"):
            deployment.execute()
        self.assertEqual([name for name, _, _ in deployment.applied], ["istio", "broker"])

    def test_missing_retained_secret_is_not_regenerated(self):
        deployment = self.deployment()
        deployment.execute()
        deployment.writes.clear()
        deployment.objects.pop(("secret", "cetirun-istio", "oauth2-proxy-credentials"))
        with self.assertRaisesRegex(ValueError, "refusing regeneration"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_cookie_upgrade_can_add_sessions_without_regenerating_existing_secrets(self):
        config = copy.deepcopy(self.config)
        config["istio"].setdefault("values", {})["oauth2Proxy"] = {"sessionStore": {"type": "cookie"}}
        deployment = self.deployment(config, "cookie-upgrade")
        deployment.execute()
        before = copy.deepcopy(deployment.keys)
        updated = self.deployment(name="redis-upgrade")
        for field in ("objects", "keys", "installed", "live", "mesh"):
            setattr(updated, field, copy.deepcopy(getattr(deployment, field)))
        deployment = updated
        deployment.execute()
        for key, value in before.items():
            self.assertEqual(deployment.keys[key], value)
        self.assertIn(("secret", "cetirun-istio", "oauth2-proxy-sessions"), deployment.objects)
        deployment.objects.pop(("secret", "cetirun-istio", "oauth2-proxy-sessions"))
        deployment.writes.clear()
        with self.assertRaisesRegex(ValueError, "refusing regeneration"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_chart_owned_tls_requires_explicit_retention(self):
        deployment = self.deployment()
        deployment.objects[("secret", "aks-istio-ingress", "cetirun-tls")]["annotations"] = {
            "meta.helm.sh/release-name": "cetirun-istio", "meta.helm.sh/release-namespace": "cetirun-istio"}
        with self.assertRaisesRegex(ValueError, "tlsMode=retain"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_retained_tls_reuses_values_without_reading_or_sending_key_material(self):
        self.config["istio"]["tlsMode"] = "retain"
        deployment = self.deployment()
        deployment.installed.add("cetirun-istio")
        for doc in deployment.documents["istio"]:
            if doc["kind"] != "Secret":
                continue
            ns = doc["metadata"].get("namespace", "cetirun-istio")
            name = doc["metadata"]["name"]
            deployment.objects[("secret", ns, name)] = {"annotations": {
                "meta.helm.sh/release-name": "cetirun-istio", "meta.helm.sh/release-namespace": "cetirun-istio"}}
            deployment.keys[(ns, name)] = set(doc["data"])
        deployment.execute()
        _, values, command = deployment.applied[0]
        self.assertIn("--reuse-values", command)
        self.assertNotIn("tls", values)
        self.assertEqual(values["cookieSecret"], "")
        self.assertNotIn("offline-render-only", json.dumps(values))

    def test_full_entry_rejects_disabled_or_disconnected_components(self):
        self.config["platform"]["values"]["cetirun-connections-mcp"]["enabled"] = False
        with self.assertRaisesRegex(ValueError, "conflicts with the complete deployment"):
            self.deployment()

    def test_rendered_custom_runtime_has_only_fields_the_operator_accepts(self):
        deployment = self.deployment()
        operator = next(doc for doc in deployment.documents["platform"]
                        if doc["kind"] == "Deployment" and doc["metadata"]["name"] == "cetirun-operator")
        environment = {item["name"]: item.get("value") for container in operator["spec"]["template"]["spec"]["containers"]
                       for item in container.get("env", [])}
        rendered = json.loads(environment["CETIRUN_CUSTOM_RUNTIME"])
        self.assertIs(rendered["enabled"], True)
        self.assertLessEqual(set(rendered), operator_custom_runtime_fields())

    def test_plan_reports_the_gateway_charts_refusal(self):
        auto = next(model for model in self.config["platform"]["values"]["cetirun-ai-gateway"]["models"] if model["model_name"] == "auto")
        auto["litellm_params"]["complexity_router_config"]["tiers"]["SIMPLE"] = "elsewhere"
        with self.assertRaisesRegex(ValueError, r"^helm template cetirun-platform .*auto router auto sends requests to elsewhere"):
            self.deployment()

    def test_cli_reports_a_rollout_failure_as_failure(self):
        deployment = self.deployment()
        deployment.fail = "rollout status deployment/cetirun-agent-broker"
        deployment.render = lambda: None
        file = self.root / "environment.json"
        with patch.object(DEPLOY, "Deployment", return_value=deployment):
            self.assertEqual(DEPLOY.main(["--config", str(file)]), 1)

    def test_context_mismatch_fails_before_any_write(self):
        deployment = self.deployment()
        original = deployment.run
        def run(command, **kwargs):
            if command[0] == "kubectl" and "view" in command:
                return "other-cluster\nhttps://other.example.invalid"
            return original(command, **kwargs)
        deployment.run = run
        with self.assertRaisesRegex(ValueError, "does not match"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_missing_istio_deployment_is_named_before_rollout_status(self):
        istio = self.config["istio"]
        # AKS managed Istio names the ingress gateway Deployment with the revision.
        istio["ingressDeployment"] = "aks-istio-ingressgateway-external"
        revisioned = f"aks-istio-ingressgateway-external-{istio['revision']}"
        deployment = self.deployment()
        deployment.istio_deployments = {(istio["controlPlaneNamespace"], istio["controlPlaneDeployment"]),
                                        (istio["ingressNamespace"], revisioned)}
        with self.assertRaisesRegex(ValueError, "istio.ingressDeployment aks-istio-ingress/aks-istio-ingressgateway-external "
                                                f"does not exist; AKS managed Istio adds the revision to the name, and {revisioned} exists$"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])
        self.assertNotIn("deployment/aks-istio-ingressgateway-external", [arg for command in deployment.commands for arg in command])
        # Without a Deployment of the revision, the message names only the input.
        deployment.istio_deployments = {(istio["ingressNamespace"], revisioned)}
        with self.assertRaisesRegex(ValueError, f"istio.controlPlaneDeployment {istio['controlPlaneNamespace']}/"
                                                f"{istio['controlPlaneDeployment']} does not exist$"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_mesh_configmap_without_the_revision_stops_before_writes(self):
        istio = self.config["istio"]
        istio["meshConfigMap"] = "istio-shared-configmap"
        shared = f"istio-shared-configmap-{istio['revision']}"
        deployment = self.deployment()
        deployment.mesh = None
        deployment.objects[("configmap", istio["controlPlaneNamespace"], shared)] = {"name": shared, "uid": "uid-shared"}
        with self.assertRaisesRegex(ValueError, f"istio.meshConfigMap {istio['controlPlaneNamespace']}/istio-shared-configmap "
                                                f"does not exist, and AKS managed Istio reads {shared}, which exists$"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])
        # Without that ConfigMap, the helper creates the configured one, as before.
        deployment.objects.pop(("configmap", istio["controlPlaneNamespace"], shared))
        deployment.execute()
        self.assertTrue(any("ensure-oauth2-proxy-id-token-header.py" in " ".join(map(str, command))
                            and "istio-shared-configmap" in command for command in deployment.writes))


@unittest.skipUnless(shutil.which("helm"), "requires existing Helm")
class DeploymentRevisionTests(DeploymentFixture, unittest.TestCase):
    def test_internal_owner_auth_uses_actual_portal_mesh_identity(self):
        config = copy.deepcopy(self.config)
        config["platform"]["namespace"] = "dedicated-platform"
        config["platform"]["values"].setdefault("cetirun-portal", {})["serviceAccount"] = {"name": "dedicated-portal"}
        _, values = self.configure(config)
        self.assertTrue(values["platform"]["cetirun-portal"]["istio"]["internalAuthMTLS"])
        self.assertEqual(values["istio"]["portalAuth"]["mtls"], {
            "enabled": True,
            "serverIdentity": "spiffe://cluster.local/ns/dedicated-platform/sa/dedicated-portal",
        })

    def test_conflicting_internal_owner_transport_fails_before_tools(self):
        for section, field, value in (
            ("istio", "portalAuth.mtls.enabled", False),
            ("istio", "portalAuth.mtls.serverIdentity", "spiffe://cluster.local/ns/other/sa/other"),
            ("platform", "cetirun-portal.istio.internalAuthMTLS", False),
        ):
            with self.subTest(field=field):
                config = copy.deepcopy(self.config)
                DEPLOY.set_value(config[section]["values"], field, value)
                with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
                    with self.assertRaisesRegex(ValueError, "conflicts with the complete deployment"):
                        self.configure(config)

    def test_default_mcp_broker_storage_identity_matches_actual_release(self):
        for explicit_name in ("dedicated-broker", ""):
            with self.subTest(account=explicit_name):
                config = copy.deepcopy(self.config)
                config["broker"]["namespace"] = "dedicated-credentials"
                config["broker"]["values"]["serviceAccount"] = {"name": explicit_name}
                _, values = self.configure(config)
                mcp = values["platform"]["cetirun-operator"]["connectionsMCP"]
                self.assertEqual(mcp["brokerNamespace"], "dedicated-credentials")
                self.assertEqual(mcp["brokerServiceAccount"], explicit_name or "cetirun-agent-broker")
                self.assertEqual(mcp["runtimeTypes"], ["hermes", "openclaw"])

    def test_conflicting_mcp_broker_storage_inputs_fail_before_tools(self):
        for field in ("brokerNamespace", "brokerServiceAccount"):
            with self.subTest(field=field):
                config = copy.deepcopy(self.config)
                config["platform"]["values"]["cetirun-operator"]["connectionsMCP"][field] = "another-broker"
                with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
                    with self.assertRaisesRegex(ValueError, "conflicts with the complete deployment"):
                        self.configure(config)

    def direct_config(self):
        config = copy.deepcopy(self.config)
        config["modelRouting"] = {"mode": "direct"}
        config["platform"]["values"]["cetirun-ai-gateway"]["models"] = config["platform"]["values"]["cetirun-ai-gateway"]["models"][:2]
        return config

    def test_direct_models_render_without_classifier_and_keep_platform_controls(self):
        deployment = self.deployment(self.direct_config(), "direct-models")
        values = deployment.values["platform"]
        self.assertEqual(deployment.plan()["modelRouting"], {"mode": "direct"})
        self.assertEqual([model["model_name"] for model in values["cetirun-ai-gateway"]["models"]], ["gpt-mini", "gpt-main"])
        self.assertTrue(values["cetirun-operator"]["sandboxAdmission"]["enforce"])
        self.assertTrue(values["cetirun-operator"]["agentIdentity"]["enabled"])
        self.assertTrue(values["cetirun-ai-gateway"]["blockUnpricedModels"])
        self.assertTrue(values["global"]["portalChat"]["enabled"])
        deployment.preflight()
        self.assertFalse(deployment.writes)

    def test_direct_models_require_explicit_mode(self):
        config = self.direct_config()
        del config["modelRouting"]
        with self.assertRaisesRegex(ValueError, "models must configure auto"):
            self.configure(config)

    def test_direct_models_reject_missing_models_classifiers_and_routers(self):
        configs = []
        empty = self.direct_config()
        empty["platform"]["values"]["cetirun-ai-gateway"]["models"] = []
        configs.append(empty)
        for field, value in (("model_name", "auto"), ("model_name", "decision-1"),
                             ("litellm_params.model", "auto_router/complexity_router"),
                             ("litellm_params.model", ""), ("model_info.mode", "evaluation"),
                             ("model_info.mode", "embedding"), ("model_info.mode", "rerank")):
            config = self.direct_config()
            DEPLOY.set_value(config["platform"]["values"]["cetirun-ai-gateway"]["models"][0], field, value)
            configs.append(config)
        for config in configs:
            with self.subTest(config=config), patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("Tool before validation")):
                with self.assertRaisesRegex(ValueError, "Direct model routing"):
                    self.configure(config)

    def test_model_routing_rejects_unknown_modes_and_fields(self):
        for routing in ({"mode": "heuristic"}, {"mode": False}, {"mode": "direct", "skipChecks": True}, "direct"):
            with self.subTest(routing=routing):
                config = self.direct_config()
                config["modelRouting"] = routing
                with self.assertRaisesRegex(ValueError, "modelRouting"):
                    self.configure(config)

    def test_direct_cetiadmin_requires_a_configured_default_model(self):
        config = self.direct_config()
        admin = yaml.safe_load(FIXTURE.read_text())["platform"]["values"]["cetirun-operator"]["cetiadmin"]
        config["platform"]["values"]["cetirun-operator"]["cetiadmin"] = admin
        with self.assertRaisesRegex(ValueError, "cetiadmin.model.default"):
            self.configure(config)
        admin["model"]["default"] = "gpt-main"
        models = config["platform"]["values"]["cetirun-ai-gateway"]["models"]
        models[1]["model_info"]["mode"] = "embedding"
        with self.assertRaisesRegex(ValueError, "Direct model routing requires callable models"):
            self.configure(config)
        models[1]["model_info"]["mode"] = "responses"
        _, values = self.configure(config)
        self.assertTrue(values["platform"]["cetirun-operator"]["cetiadmin"]["enabled"])

    def test_decision_accepts_the_priced_base_model_and_optional_api_version(self):
        for api_version in (None, "2024-05-01-preview"):
            with self.subTest(api_version=api_version):
                config = copy.deepcopy(self.config)
                model = config["platform"]["values"]["cetirun-ai-gateway"]["models"][2]
                self.assertEqual(model["model_info"]["base_model"], "azure_ai/Microsoft-Decision-1")
                model["litellm_params"].pop("api_version")
                if api_version:
                    model["litellm_params"]["api_version"] = api_version
                _, values = self.configure(config)
                self.assertEqual(values["platform"]["cetirun-ai-gateway"]["models"][2], model)

    def test_decision_rejects_wrong_model_or_missing_provider_inputs(self):
        for field, value in (("model_info.base_model", "azure_ai/Other-Model"), ("model_info.base_model", "Microsoft-Decision-1"),
                             ("model_info.mode", "chat"),
                             ("litellm_params.model", "azure_ai/"), ("litellm_params.api_base", ""),
                             ("litellm_params.api_key", ""), ("litellm_params.api_key", "os.environ/"),
                             ("litellm_params.api_version", False)):
            with self.subTest(field=field, value=value):
                config = copy.deepcopy(self.config)
                model = config["platform"]["values"]["cetirun-ai-gateway"]["models"][2]
                DEPLOY.set_value(model, field, value)
                with self.assertRaises(ValueError):
                    self.configure(config)

    def test_mcp_runtime_allowlists_must_agree_before_tools(self):
        for component, field in (("broker", "toolService.runtimeTypes"), ("cetirun-portal", "agentTools.runtimeTypes"),
                                 ("cetirun-operator", "connectionsMCP.runtimeTypes")):
            with self.subTest(component=component):
                config = copy.deepcopy(self.config)
                values = config["broker"]["values"] if component == "broker" else config["platform"]["values"].setdefault(component, {})
                DEPLOY.set_value(values, field, ["hermes"])
                with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
                    with self.assertRaisesRegex(ValueError, "runtimeTypes"):
                        self.configure(config)

    def test_explicit_custom_mcp_runtime_renders_for_all_components(self):
        config = copy.deepcopy(self.config)
        runtime_lists = (
            (config["broker"]["values"], "toolService.runtimeTypes", ["custom", "hermes", "openclaw"]),
            (config["platform"]["values"].setdefault("cetirun-operator", {}), "connectionsMCP.runtimeTypes", ["openclaw", "custom", "hermes"]),
            (config["platform"]["values"].setdefault("cetirun-portal", {}), "agentTools.runtimeTypes", ["hermes", "openclaw", "custom"]),
        )
        for values, field, runtimes in runtime_lists:
            DEPLOY.set_value(values, field, runtimes)
        deployment = self.deployment(config, "custom-mcp")
        expected = {
            ("broker", "cetirun-agent-broker", "broker"): "custom,hermes,openclaw",
            ("platform", "cetirun-operator", "manager"): "openclaw,custom,hermes",
            ("platform", "cetirun-portal", "portal"): "hermes,openclaw,custom",
        }
        for (release, name, container), value in expected.items():
            document = next(doc for doc in deployment.documents[release]
                            if doc["kind"] == "Deployment" and doc["metadata"]["name"] == name)
            actual = next(item["value"] for item in next(item for item in document["spec"]["template"]["spec"]["containers"]
                                                            if item["name"] == container)["env"]
                          if item["name"] == "CONNECTIONS_MCP_RUNTIME_TYPES")
            self.assertEqual(actual, value)

    def test_custom_mcp_runtime_lists_require_matching_supported_unique_values(self):
        cases = (
            ("mismatch", "broker", "toolService.runtimeTypes", ["custom"]),
            ("unknown", "cetirun-operator", "connectionsMCP.runtimeTypes", ["hermes", "unknown"]),
            ("empty", "cetirun-portal", "agentTools.runtimeTypes", []),
            ("duplicate", "broker", "toolService.runtimeTypes", ["hermes", "custom", "custom"]),
        )
        for name, component, field, runtimes in cases:
            with self.subTest(name=name):
                config = copy.deepcopy(self.config)
                values = config["broker"]["values"] if component == "broker" else config["platform"]["values"].setdefault(component, {})
                DEPLOY.set_value(values, field, runtimes)
                with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
                    with self.assertRaisesRegex(ValueError, "runtimeTypes"):
                        self.configure(config)

    def add_agent(self, deployment, *, account=None, image="", runtime="openclaw", sandbox_ref=""):
        deployment.objects[("customresourcedefinition", "", "agents.cetirun.microsoft.com")] = {
            "annotations": {"meta.helm.sh/release-name": "cetirun-platform", "meta.helm.sh/release-namespace": "cetirun-system"}}
        deployment.objects[("agent", "", "operations-fixture")] = {
            "metadata": {"name": "operations-fixture"},
            "spec": {"agent": {"type": runtime, "image": image}, "sandboxRef": sandbox_ref,
                     "user": {"email": "must-not-read@example.invalid"},
                     "helm": {"values": {"serviceAccount": account or {}, "secret": "must-not-read"}}}}

    def test_existing_ordinary_agent_custom_sa_and_token_stop_before_writes(self):
        for account in ({"name": "operations-account", "automountToken": True}, {"role": "view"}, {"automountToken": True}):
            with self.subTest(account=account):
                deployment = self.deployment(name=f"agent-{len(account)}-{account.get('name', account.get('role', 'token'))}")
                self.add_agent(deployment, account=account)
                with self.assertRaisesRegex(ValueError, "CetiAdmin.*ServiceAccount|ServiceAccount.*CetiAdmin"):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])
                reads = [command for command in deployment.commands if command[0] == "kubectl"
                         and command[4:6] == ["get", "agents.cetirun.microsoft.com"]]
                self.assertEqual(len(reads), 1)
                self.assertFalse(any(".spec.user" in part or part == "json" for command in reads for part in command))

    def test_existing_ordinary_custom_agent_is_not_an_mcp_conflict(self):
        config = copy.deepcopy(self.config)
        for values, field in ((config["broker"]["values"], "toolService.runtimeTypes"),
                              (config["platform"]["values"].setdefault("cetirun-operator", {}), "connectionsMCP.runtimeTypes"),
                              (config["platform"]["values"].setdefault("cetirun-portal", {}), "agentTools.runtimeTypes")):
            DEPLOY.set_value(values, field, ["custom"])
        deployment = self.deployment(config, "ordinary-custom")
        self.assertEqual(deployment.values["broker"]["toolService"]["runtimeTypes"], ["custom"])
        self.assertEqual(deployment.values["platform"]["cetirun-operator"]["connectionsMCP"]["runtimeTypes"], ["custom"])
        self.assertEqual(deployment.values["platform"]["cetirun-portal"]["agentTools"]["runtimeTypes"], ["custom"])
        self.add_agent(deployment, runtime="custom", account={"name": "custom-account", "automountToken": True})
        deployment.preflight()
        self.assertFalse(any(command[4:6] == ["get", "agent"] for command in deployment.commands if command[0] == "kubectl"))

    def test_retained_custom_agent_cannot_reference_an_excluded_image(self):
        candidate = copy.deepcopy(self.config)
        candidate["release"]["excludeImages"] = ["cetirun-vscode"]
        approved = candidate["platform"]["values"]["cetirun-operator"]["customRuntime"]["approvedImages"]
        candidate["platform"]["values"]["cetirun-operator"]["customRuntime"]["approvedImages"] = [
            image for image in approved if "/cetirun-vscode@" not in image
        ]
        deployment = self.upgrade_fixture(self.config, candidate, "excluded-vscode")
        self.add_agent(deployment, runtime="custom", image="registry.example.com/cetirun-vscode@sha256:" + "a" * 64)
        with self.assertRaisesRegex(ValueError, "Existing Custom Agent operations-fixture still references an excluded image"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_consistent_hermes_only_preserves_openclaw_account(self):
        self.config["broker"]["values"]["toolService"] = {"runtimeTypes": ["hermes"]}
        self.config["platform"]["values"]["cetirun-operator"]["connectionsMCP"]["runtimeTypes"] = ["hermes"]
        self.config["platform"]["values"]["cetirun-portal"] = {"agentTools": {"runtimeTypes": ["hermes"]}}
        deployment = self.deployment()
        self.add_agent(deployment, account={"name": "operations-account", "automountToken": True})
        deployment.execute()
        self.assertEqual(deployment.values["platform"]["cetirun-operator"]["connectionsMCP"]["runtimeTypes"], ["hermes"])

    def test_no_conflict_and_protected_sandbox_do_not_block(self):
        for account, sandbox in (({}, ""), ({"name": "protected-account", "automountToken": True}, "registered-sandbox")):
            with self.subTest(sandbox=sandbox):
                deployment = self.deployment(name="no-conflict-" + (sandbox or "ordinary"))
                self.add_agent(deployment, account=account, sandbox_ref=sandbox)
                deployment.execute()

    def guarded_config(self, *, pods=True, strict=True, coordination=True, native=False):
        config = copy.deepcopy(self.config)
        op = config["platform"]["values"]["cetirun-operator"]
        op["sandboxAdmission"].update(podsEnabled=pods, validationMode="strict" if strict else "compatible")
        if coordination:
            op["sandboxAdmission"].update(coordinationConfigMapName="configuration-coordination", coordinationConfigMapUID="uid-configuration-coordination")
            config["platform"]["values"]["cetirun-portal"] = {"configurationCoordination": {
                "namespace": "cetirun-system", "name": "configuration-coordination", "uid": "uid-configuration-coordination"}}
        if native:
            op["sandboxSources"] = {"enabled": True, "nativeExecution": True, "plannedMigration": True,
                                     "bootstrapOwnedPrerequisites": True, "platformConfigMapName": "sandbox-platform"}
        return config

    def upgrade_fixture(self, old_config, new_config, suffix):
        old = self.deployment(old_config, "old-" + suffix)
        old.upgrade("platform")
        new = self.deployment(new_config, "new-" + suffix)
        new.objects.update(copy.deepcopy(old.objects))
        new.keys.update(copy.deepcopy(old.keys))
        new.live.update(copy.deepcopy(old.live))
        new.installed.update(old.installed)
        return new

    def test_bootstrap_clears_only_inactive_source_references(self):
        for field in ("skillsCatalogConfigMapName", "placementConfigMapName"):
            with self.subTest(field=field):
                config = self.guarded_config(native=True)
                operator = config["platform"]["values"]["cetirun-operator"]
                sources = operator["sandboxSources"]
                sources[field] = "qualified-registry"
                if field == "placementConfigMapName":
                    sources.update(nativeExecution=False, plannedMigration=False)
                    operator["sandboxAdmission"][field] = "qualified-registry"
                deployment = self.deployment(config, "bootstrap-reference-" + field)
                final = copy.deepcopy(deployment.values["platform"])
                deployment.execute()
                applied = [values for name, values, _ in deployment.applied if name == "platform"]
                self.assertEqual(len(applied), 2)
                self.assertFalse(applied[0]["cetirun-operator"]["sandboxSources"]["enabled"])
                self.assertEqual(applied[0]["cetirun-operator"]["sandboxSources"][field], "")
                self.assertEqual(applied[1], final)
                upgraded = self.upgrade_fixture(config, config, "retained-reference-" + field)
                upgraded.execute()
                self.assertEqual([values for name, values, _ in upgraded.applied if name == "platform"], [final])

    def test_existing_admission_identities_cannot_change_through_defaults(self):
        for field in ("adminUsers", "adminGroups", "teamAssignments", "controllerUsername"):
            with self.subTest(field=field):
                old = self.guarded_config()
                admission = old["platform"]["values"]["cetirun-operator"]["sandboxAdmission"]
                if field == "adminUsers":
                    admission[field].append("retained-admin@example.invalid")
                elif field == "adminGroups":
                    admission[field] = ["retained-admins"]
                elif field == "teamAssignments":
                    admission[field] = [{"team": "retained-team", "groups": ["retained-members"]}]
                else:
                    old["platform"]["values"]["cetirun-operator"]["serviceAccount"] = {"name": "retained-controller"}
                deployment = self.upgrade_fixture(old, self.guarded_config(), "authority-" + field)
                with self.assertRaisesRegex(ValueError, "Preserve existing admission.*" + field):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])

    def test_admission_authority_reordering_is_not_a_change(self):
        old = self.guarded_config()
        admission = old["platform"]["values"]["cetirun-operator"]["sandboxAdmission"]
        admission["adminUsers"].append("retained-admin@example.invalid")
        admission["adminGroups"] = ["operators", "auditors"]
        admission["teamAssignments"] = [{"team": "retained-team", "users": ["member-a", "member-b"], "groups": ["retained-members"]}]
        candidate = copy.deepcopy(old)
        reordered = candidate["platform"]["values"]["cetirun-operator"]["sandboxAdmission"]
        reordered["adminUsers"].reverse()
        reordered["adminGroups"].reverse()
        reordered["teamAssignments"] = [
            {"team": "retained-team", "users": ["member-b"]},
            {"team": "retained-team", "users": ["member-a"], "groups": ["retained-members"]},
        ]
        deployment = self.upgrade_fixture(old, candidate, "authority-reordering")
        deployment.execute()
        self.assertEqual([name for name, _, _ in deployment.applied], ["istio", "broker", "platform"])

    def test_admission_identity_expansion_and_team_remap_are_not_preservation(self):
        old = self.guarded_config()
        old["platform"]["values"]["cetirun-operator"]["sandboxAdmission"]["teamAssignments"] = [
            {"team": "retained-team", "groups": ["retained-members"]}]
        for field in ("adminUsers", "teamAssignments"):
            with self.subTest(field=field):
                candidate = copy.deepcopy(old)
                admission = candidate["platform"]["values"]["cetirun-operator"]["sandboxAdmission"]
                if field == "adminUsers":
                    admission[field].append("additional-admin@example.invalid")
                else:
                    admission[field][0]["team"] = "another-team"
                deployment = self.upgrade_fixture(old, candidate, "authority-expansion-" + field)
                with self.assertRaisesRegex(ValueError, "Preserve existing admission.*" + field):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])

    def test_ordinary_mcp_broker_storage_identity_is_preserved(self):
        for field in ("namespace", "serviceAccount"):
            with self.subTest(field=field):
                old = copy.deepcopy(self.config)
                if field == "namespace":
                    old["broker"]["namespace"] = "retained-credentials"
                else:
                    old["broker"]["values"]["serviceAccount"] = {"name": "retained-broker"}
                old["platform"]["values"]["cetirun-operator"]["connectionsMCP"].update(
                    brokerNamespace=old["broker"]["namespace"],
                    brokerServiceAccount=old["broker"]["values"].get("serviceAccount", {}).get("name", "cetirun-agent-broker"))
                deployment = self.upgrade_fixture(old, self.config, "ordinary-broker-" + field)
                self.assertFalse(deployment.values["platform"]["cetirun-operator"]["connectionsMCP"]["nativeEnabled"])
                with self.assertRaisesRegex(ValueError, "Preserve existing.*Broker"):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])
                retained = self.upgrade_fixture(old, old, "retained-broker-" + field)
                retained.execute()

    def test_upgrade_cannot_drop_pod_admission_strict_mode_or_coordination(self):
        cases = [
            ("pods", self.guarded_config(strict=False, coordination=False), self.config),
            ("strict", self.guarded_config(pods=False, coordination=False), self.config),
            ("coordination", self.guarded_config(), self.guarded_config(coordination=False)),
            ("native", self.guarded_config(native=True), self.guarded_config()),
        ]
        for name, old, new in cases:
            with self.subTest(protection=name):
                deployment = self.upgrade_fixture(old, new, name)
                with self.assertRaisesRegex(ValueError, "preserve|Preserve|protection"):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])

    def test_explicit_preserved_strong_configuration_upgrades_without_bootstrap(self):
        strong = self.guarded_config(native=True)
        deployment = self.upgrade_fixture(strong, strong, "preserved")
        deployment.execute()
        applied = [values for name, values, _ in deployment.applied if name == "platform"]
        self.assertEqual(len(applied), 1)
        admission = applied[0]["cetirun-operator"]["sandboxAdmission"]
        self.assertTrue(admission["podsEnabled"])
        self.assertEqual(admission["validationMode"], "strict")
        self.assertEqual(admission["coordinationConfigMapUID"], "uid-configuration-coordination")
        self.assertTrue(applied[0]["cetirun-operator"]["sandboxSources"]["nativeExecution"])
        rules = [rule for doc in deployment.documents["platform"] for webhook in doc.get("webhooks", []) for rule in webhook.get("rules", [])]
        self.assertTrue(any("pods" in rule.get("resources", []) for rule in rules))

    def test_missing_live_admission_configuration_is_not_assumed_compatible(self):
        strong = self.guarded_config()
        deployment = self.upgrade_fixture(strong, strong, "missing-config")
        document = deployment.live[("deployment", "cetirun-system", "cetirun-sandbox-admission")]
        document["spec"]["template"]["spec"]["containers"][0]["env"] = []
        with self.assertRaisesRegex(ValueError, "admission.*configuration|configuration.*admission"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_external_native_control_reference_is_not_assumed_disabled(self):
        strong = self.guarded_config(native=True)
        deployment = self.upgrade_fixture(strong, strong, "external-control")
        document = deployment.live[("deployment", "cetirun-system", "cetirun-operator")]
        env = document["spec"]["template"]["spec"]["containers"][0]["env"]
        entry = next(item for item in env if item["name"] == "CETIRUN_SANDBOX_SOURCES")
        entry.pop("value")
        entry["valueFrom"] = {"configMapKeyRef": {"name": "external-control", "key": "configuration"}}
        with self.assertRaisesRegex(ValueError, "control|protection"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_pod_rules_in_existing_configuration_webhook_are_preserved(self):
        deployment = self.upgrade_fixture(self.config, self.config, "pod-rules")
        webhook = deployment.live[("validatingwebhookconfiguration", "", "cetirun-sandbox-admission")]
        webhook["webhooks"][0]["rules"].append({"apiGroups": [""], "resources": ["pods"], "operations": ["CREATE"]})
        with self.assertRaisesRegex(ValueError, "Pod admission protection"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_existing_agent_read_failure_does_not_skip_conflict_preflight(self):
        deployment = self.deployment()
        self.add_agent(deployment, account={"name": "operations-account"})
        deployment.fail = "get agents.cetirun.microsoft.com"
        with self.assertRaisesRegex(ValueError, "Synthetic failure"):
            deployment.execute()
        self.assertEqual(deployment.writes, [])

    def test_native_mcp_configuration_and_peer_identity_cannot_be_reset(self):
        strong = self.guarded_config(native=True)
        operator = strong["platform"]["values"]["cetirun-operator"]
        operator["connectionsMCP"].update(nativeEnabled=True, brokerNamespace="cetirun-credentials", brokerServiceAccount="cetirun-agent-broker")
        strong["platform"]["values"]["cetirun-connections-mcp"]["nativeSandbox"] = {
            "enabled": True, "operatorNamespace": "cetirun-system", "peerUID": "registered-peer"}
        for field in ("operator", "peer"):
            with self.subTest(field=field):
                candidate = copy.deepcopy(strong)
                if field == "operator":
                    candidate["platform"]["values"]["cetirun-operator"]["connectionsMCP"]["nativeEnabled"] = False
                else:
                    candidate["platform"]["values"]["cetirun-connections-mcp"]["nativeSandbox"]["peerUID"] = "different-peer"
                deployment = self.upgrade_fixture(strong, candidate, "native-mcp-" + field)
                with self.assertRaisesRegex(ValueError, "Preserve existing native MCP"):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])
        deployment = self.upgrade_fixture(strong, strong, "native-mcp-preserved")
        deployment.execute()


class SkillMarketplaceTests(DeploymentFixture, unittest.TestCase):
    def marketplace_config(self, **repository):
        config = copy.deepcopy(self.config)
        operator = config["platform"]["values"]["cetirun-operator"]
        # The marketplace needs native Sandbox execution, which the example does not enable.
        operator["sandboxAdmission"]["podsEnabled"] = True
        operator["sandboxSources"] = {"enabled": True, "nativeExecution": True, "bootstrapOwnedPrerequisites": True,
                                      "platformConfigMapName": "sandbox-platform"}
        config["skillMarketplace"] = copy.deepcopy(MARKETPLACE)
        config["skillMarketplace"]["repository"].update(repository)
        return config

    def refused(self, config, message):
        with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
            with self.assertRaisesRegex(ValueError, message) as failure:
                self.configure(config)
        return str(failure.exception)

    @staticmethod
    def environment(documents, name):
        deployment = next(doc for doc in documents if doc["kind"] == "Deployment" and doc["metadata"]["name"] == name)
        return {item["name"]: item.get("value") for item in deployment["spec"]["template"]["spec"]["containers"][0].get("env", [])}

    def test_block_sets_the_marketplace_operator_and_portal_values(self):
        _, values = self.configure(self.marketplace_config())
        platform = values["platform"]
        marketplace = platform["cetirun-operator"]["skillMarketplace"]
        self.assertIs(marketplace["enabled"], True)
        for section in ("repository", "storage"):
            self.assertEqual({key: marketplace[section][key] for key in MARKETPLACE[section]}, MARKETPLACE[section])
        # The worker runs the operator image; there is no other image or chart.
        self.assertNotIn("image", marketplace)
        self.assertNotIn("cetirun-skill-marketplace", platform)
        self.assertEqual(platform["cetirun-portal"]["skillMarketplace"], {"enabled": True, "catalogNamespace": "cetirun-system"})

    def test_optional_fields_take_the_chart_defaults(self):
        config = self.marketplace_config(auth="none")
        for field in ("ref", "path", "syncInterval", "existingSecret"):
            config["skillMarketplace"]["repository"].pop(field)
        config["platform"]["values"]["cetirun-operator"]["sandboxSources"]["skillsClaimName"] = "team-skills"
        _, values = self.configure(config)
        marketplace = values["platform"]["cetirun-operator"]["skillMarketplace"]
        self.assertEqual({key: marketplace["repository"][key] for key in ("ref", "path", "syncInterval", "auth", "existingSecret")},
                         {"ref": "", "path": "", "syncInterval": "5m", "auth": "none", "existingSecret": ""})
        # The Operator names the Agents' claims after its own setting.
        self.assertEqual(values["platform"]["cetirun-operator"]["sandboxSources"]["skillsClaimName"], "team-skills")

    def test_invalid_inputs_are_refused_before_tools(self):
        marker = "synthetic-credential-marker"
        repository, storage = "skillMarketplace.repository", "skillMarketplace.storage"
        sources = "platform.values.cetirun-operator.sandboxSources"
        cases = [
            ({f"{repository}.url": DELETE}, r"skillMarketplace\.repository\.url is required"),
            ({"skillMarketplace.storage": DELETE}, r"skillMarketplace\.storage is required"),
            *(({f"{storage}.{field}": DELETE}, rf"skillMarketplace\.storage\.{field} is required") for field in MARKETPLACE["storage"]),
            ({f"{repository}.url": f"https://user:{marker}@git.example.invalid/skills.git"}, "must not contain credentials"),
            ({f"{repository}.url": f"https://{marker}@git.example.invalid/skills.git"}, "must not contain credentials"),
            ({f"{repository}.url": f"https://git.example.invalid/skills.git?private_token={marker}"}, "must not contain credentials"),
            ({f"{repository}.url": f"ssh://git:{marker}@git.example.invalid/skills.git", f"{repository}.auth": "ssh"},
             "must not contain credentials"),
            ({f"{repository}.existingSecret": DELETE}, "auth token needs existingSecret: .* with the key token$"),
            ({f"{repository}.url": SSH_URL, f"{repository}.auth": "ssh", f"{repository}.existingSecret": ""},
             "auth ssh needs existingSecret: .* with the keys ssh-privatekey and known_hosts$"),
            ({f"{repository}.auth": "none"}, "existingSecret is used only with auth token or ssh"),
            ({f"{repository}.auth": "basic"}, "auth must be none, token or ssh"),
            ({f"{repository}.auth": DELETE}, "auth must be none, token or ssh"),
            ({f"{repository}.url": "http://git.example.invalid/skills.git"}, "auth token needs an https:// URL"),
            ({f"{repository}.url": SSH_URL}, "auth token needs an https:// URL"),
            ({f"{repository}.auth": "ssh"}, r"auth ssh needs an ssh:// URL or the user@host:path form"),
            ({f"{repository}.syncInterval": "10s"}, "at least 30s"),
            ({f"{repository}.syncInterval": "5 minutes"}, "at least 30s"),
            ({f"{repository}.ref": "main..other"}, "ref must name a branch"),
            ({f"{repository}.path": "/skills"}, "path must be relative"),
            ({f"{repository}.tokenUsername": "oauth2"}, r"repository\.tokenUsername is not supported"),
            ({f"{storage}.storageAccount": "Example-Account"}, "storageAccount must be 3-24"),
            ({f"{storage}.storageAccount": "exampleskills", f"{storage}.containerName": "published-skills"},
             "other than the sharedSkills container"),
            ({f"{sources}.enabled": False, f"{sources}.nativeExecution": False, f"{sources}.bootstrapOwnedPrerequisites": False},
             "needs native Sandbox execution"),
            ({f"{sources}.nativeExecution": False}, "needs native Sandbox execution"),
            ({f"{sources}.skillsCatalogConfigMapName": "published-skills-catalog"}, "replaces the published skills catalog"),
            ({f"{sources}.skillsClaimName": ""}, "skillsClaimName, a DNS label"),
            ({"platform.values.cetirun-operator.skillMarketplace.storage.containerName": "other-container"},
             r"cetirun-operator\.skillMarketplace\.storage\.containerName conflicts with the complete deployment inputs"),
            ({"platform.values.cetirun-portal.skillMarketplace.catalogNamespace": "other-namespace"},
             r"cetirun-portal\.skillMarketplace\.catalogNamespace conflicts with the complete deployment inputs"),
        ]
        for changes, message in cases:
            with self.subTest(changes=list(changes), message=message):
                config = self.marketplace_config()
                for path, value in changes.items():
                    if value is DELETE:
                        parent, key = path.rsplit(".", 1)
                        DEPLOY.get_value(config, parent).pop(key)
                    else:
                        DEPLOY.set_value(config, path, value)
                self.assertNotIn(marker, self.refused(config, message))

    def test_platform_values_cannot_turn_the_marketplace_on_without_its_block(self):
        for switch in DEPLOY.SKILL_MARKETPLACE_SWITCHES:
            with self.subTest(switch=switch):
                config = copy.deepcopy(self.config)
                DEPLOY.set_value(config["platform"]["values"], switch, True)
                self.refused(config, "needs the top-level skillMarketplace block")

    def test_commented_example_block_is_valid(self):
        lines = FIXTURE.read_text().splitlines()
        block = []
        for line in lines[lines.index("# skillMarketplace:"):]:
            if not line.startswith("#"):
                break
            block.append(line[2:])
        config = self.marketplace_config()
        config["skillMarketplace"] = yaml.safe_load("\n".join(block))["skillMarketplace"]
        _, values = self.configure(config)
        marketplace = values["platform"]["cetirun-operator"]["skillMarketplace"]
        self.assertEqual(marketplace["repository"]["existingSecret"], "skills-repository")
        self.assertNotEqual(marketplace["storage"]["containerName"], config["sharedSkills"]["containerName"])

    @unittest.skipUnless(shutil.which("helm"), "requires existing Helm for real local chart rendering")
    def test_bootstrap_has_no_marketplace_and_the_final_release_waits_for_it(self):
        deployment = self.deployment(self.marketplace_config(), "marketplace")

        def workloads(documents):
            return {doc["metadata"]["name"] for doc in documents if doc["kind"] in ("Deployment", "StatefulSet")}

        self.assertIn("cetirun-skill-marketplace", workloads(deployment.documents["platform"]))
        self.assertEqual(workloads(deployment.bootstrap_documents),
                         workloads(deployment.documents["platform"]) - {"cetirun-skill-marketplace"})
        workload = next(doc for doc in deployment.documents["platform"]
                        if doc["kind"] == "Deployment" and doc["metadata"]["name"] == "cetirun-skill-marketplace")
        self.assertEqual(workload["metadata"]["namespace"], "cetirun-system")
        container = workload["spec"]["template"]["spec"]["containers"][0]
        # The worker is the second binary of the operator image.
        self.assertEqual(container["image"], "registry.example.com/cetirun-operator:0.1.1")
        self.assertEqual(container["command"], ["/skill-marketplace"])
        for argument in ("--repository-url=https://git.example.invalid/example/skills.git", "--repository-ref=main",
                         "--repository-path=skills", "--auth=token", "--sync-interval=10m", "--namespace=cetirun-system"):
            self.assertIn(argument, container["args"])
        volume = next(doc for doc in deployment.documents["platform"] if doc["kind"] == "PersistentVolume")
        self.assertEqual(volume["spec"]["csi"]["volumeAttributes"],
                         {"resourceGroup": "example-marketplace-rg", "storageAccount": "examplemarketplace",
                          "containerName": "marketplace", "protocol": "nfs"})
        self.assertEqual(volume["spec"]["storageClassName"], "example-marketplace")
        storage = {"resourceGroup": "example-marketplace-rg", "storageAccount": "examplemarketplace",
                   "containerName": "marketplace", "storageClassName": "example-marketplace"}
        for documents, enabled in ((deployment.bootstrap_documents, False), (deployment.documents["platform"], True)):
            operator = self.environment(documents, "cetirun-operator")
            portal = self.environment(documents, "cetirun-portal")
            self.assertEqual(json.loads(operator["CETIRUN_SKILL_MARKETPLACE"]) if enabled else operator.get("CETIRUN_SKILL_MARKETPLACE"),
                             {"enabled": True, "storage": storage} if enabled else None)
            self.assertEqual(portal.get("SKILL_MARKETPLACE_ENABLED"), "true" if enabled else None)
            self.assertEqual(portal.get("SKILL_MARKETPLACE_CATALOG_NAMESPACE"), "cetirun-system" if enabled else None)
        plan = deployment.plan()
        self.assertIn("cetirun-skill-marketplace", plan["releases"]["platform"]["workloads"])
        self.assertIn("Skill Marketplace repository access and Team publication", plan["notQualified"])

        deployment.execute()
        bootstrap, final = [values for name, values, _ in deployment.applied if name == "platform"]
        for switch in DEPLOY.SKILL_MARKETPLACE_SWITCHES:
            self.assertIs(DEPLOY.get_value(bootstrap, switch), False, switch)
            self.assertIs(DEPLOY.get_value(final, switch), True, switch)
        upgrades = [index for index, command in enumerate(deployment.commands)
                    if command[:5] == ["helm", "--kube-context", "example-context", "upgrade", "--install"]
                    and "cetirun-platform" in command]
        rollouts = [index for index, command in enumerate(deployment.commands)
                    if "deployment/cetirun-skill-marketplace" in command]
        self.assertEqual(len(upgrades), 2)
        self.assertEqual(len(rollouts), 1)
        self.assertGreater(rollouts[0], upgrades[1])
        # The bootstrap stage still waits for the workloads it installs.
        operator_rollouts = [index for index, command in enumerate(deployment.commands)
                             if "deployment/cetirun-operator" in command]
        self.assertEqual(len(operator_rollouts), 2)
        self.assertTrue(upgrades[0] < operator_rollouts[0] < upgrades[1] < operator_rollouts[1])

    @unittest.skipUnless(shutil.which("helm"), "requires existing Helm for real local chart rendering")
    def test_preflight_checks_the_repository_secret_keys_and_the_storage_class(self):
        for auth, url, secret, keys in (("token", MARKETPLACE["repository"]["url"], "skills-repository", ("token",)),
                                        ("ssh", SSH_URL, "skills-deploy-key", ("ssh-privatekey", "known_hosts")),
                                        ("none", MARKETPLACE["repository"]["url"], None, ())):
            deployment = self.deployment(self.marketplace_config(auth=auth, url=url, existingSecret=secret), "preflight-" + auth)
            references = deployment.references()
            self.assertIn(("StorageClass", "", "example-marketplace", ""), references)
            self.assertEqual({key for kind, _, name, key in references if kind == "Secret" and name == secret},
                             {"", *keys} if secret else set())
            for missing in (*keys, None):
                with self.subTest(auth=auth, missing=missing):
                    if missing:
                        deployment.keys[("cetirun-system", secret)].discard(missing)
                        expected = f"Required Secret key is missing: cetirun-system/{secret}:{missing}"
                    else:
                        removed = deployment.objects.pop(("storageclass", "", "example-marketplace"))
                        expected = "Required StorageClass reference is missing: /example-marketplace"
                    with self.assertRaisesRegex(ValueError, re.escape(expected)):
                        deployment.execute()
                    self.assertEqual(deployment.writes, [])
                    if missing:
                        deployment.keys[("cetirun-system", secret)].add(missing)
                    else:
                        deployment.objects[("storageclass", "", "example-marketplace")] = removed


# Synthetic ADX-Mon inputs; the example file only shows them commented out.
OBSERVABILITY = {
    "clusterName": "example",
    "adx": {"endpoint": "https://example.australiaeast.kusto.windows.net"},
    "ingestorClientId": "55555555-5555-5555-5555-555555555555",
}


class ObservabilityTests(DeploymentFixture, unittest.TestCase):
    def observability_config(self):
        config = copy.deepcopy(self.config)
        config["observability"] = copy.deepcopy(OBSERVABILITY)
        return config

    def refused(self, config, message):
        with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("External command before validation")):
            with self.assertRaisesRegex(ValueError, message):
                self.configure(config)

    def test_without_the_block_it_is_off(self):
        _, values = self.configure(self.config)
        self.assertIs(values["platform"]["cetirun-observability"]["enabled"], False)

    def test_block_sets_the_chart_values_with_default_databases(self):
        _, values = self.configure(self.observability_config())
        chart = values["platform"]["cetirun-observability"]
        self.assertIs(chart["enabled"], True)
        self.assertEqual(chart["clusterName"], "example")
        self.assertEqual(chart["adx"]["endpoint"], OBSERVABILITY["adx"]["endpoint"])
        self.assertEqual(chart["adx"]["databases"], {"logs": "CetiRunLogs", "metrics": "CetiRunMetrics", "agentLogs": "CetiRunAgents"})
        self.assertEqual(chart["ingestor"]["clientId"], OBSERVABILITY["ingestorClientId"])

    def test_invalid_inputs_are_refused_before_tools(self):
        cases = [
            ({"observability.clusterName": DELETE}, r"observability\.clusterName is required"),
            ({"observability.clusterName": "Example_Cluster"}, "must be a DNS label"),
            ({"observability.adx.endpoint": DELETE}, r"observability\.adx\.endpoint is required"),
            ({"observability.adx.endpoint": "http://example.kusto.windows.net"}, "query URI of the Kusto cluster"),
            ({"observability.adx.endpoint": "https://example.kusto.windows.net/CetiRunLogs"}, "without a path"),
            ({"observability.ingestorClientId": "example-client"}, "client ID of the managed identity"),
            ({"observability.adx.databases.logs": "cetirun-logs"}, "database name of letters and digits"),
            ({"observability.adx.databases.agentLogs": "CetiRunLogs"}, "must name three databases"),
            ({"observability.adx.ingestionUri": "https://ingest-example.kusto.windows.net"}, r"observability\.adx\.ingestionUri is not supported"),
            ({"platform.values.cetirun-observability.clusterName": "other"},
             r"cetirun-observability\.clusterName conflicts with the complete deployment inputs"),
        ]
        for changes, message in cases:
            with self.subTest(changes=list(changes), message=message):
                config = self.observability_config()
                for path, value in changes.items():
                    if value is DELETE:
                        parent, key = path.rsplit(".", 1)
                        DEPLOY.get_value(config, parent).pop(key)
                    else:
                        DEPLOY.set_value(config, path, value)
                self.refused(config, message)

    def test_platform_values_cannot_turn_it_on_without_its_block(self):
        config = copy.deepcopy(self.config)
        DEPLOY.set_value(config["platform"]["values"], "cetirun-observability.enabled", True)
        self.refused(config, "needs the top-level observability block")

    def test_commented_example_block_is_valid(self):
        lines = FIXTURE.read_text().splitlines()
        block = []
        for line in lines[lines.index("# observability:"):]:
            if not line.startswith("#"):
                break
            block.append(line[2:])
        config = copy.deepcopy(self.config)
        config["observability"] = yaml.safe_load("\n".join(block))["observability"]
        _, values = self.configure(config)
        self.assertIs(values["platform"]["cetirun-observability"]["enabled"], True)

    @unittest.skipUnless(shutil.which("helm"), "requires existing Helm for real local chart rendering")
    def test_turning_it_on_later_adds_the_ingestor_secret_once(self):
        deployment = self.deployment(name="observability-off")
        deployment.execute()
        updated = self.deployment(self.observability_config(), "observability-on")
        for field in ("objects", "keys", "installed", "live", "mesh"):
            setattr(updated, field, copy.deepcopy(getattr(deployment, field)))
        updated.execute()
        secret = ("secret", "cetirun-system", "cetirun-adx-mon-ingestor-tls")
        self.assertIn(secret, updated.objects)
        self.assertIn(("statefulset", "cetirun-system", "cetirun-adx-mon-ingestor"), updated.objects)
        # Once the ingestor exists, a lost Secret is not generated again.
        updated.objects.pop(secret)
        updated.writes.clear()
        with self.assertRaisesRegex(ValueError, "cetirun-adx-mon-ingestor-tls is missing; refusing regeneration"):
            updated.execute()
        self.assertEqual(updated.writes, [])

    @unittest.skipUnless(shutil.which("helm"), "requires existing Helm for real local chart rendering")
    def test_render_uses_the_release_images_and_needs_no_external_reference(self):
        deployment = self.deployment(self.observability_config(), "observability")
        documents = deployment.documents["platform"]
        workloads = {(doc["kind"], doc["metadata"]["name"]): doc for doc in documents
                     if doc["kind"] in ("Deployment", "StatefulSet", "DaemonSet")}
        for kind, name in (("DaemonSet", "cetirun-adx-mon-collector"), ("StatefulSet", "cetirun-adx-mon-ingestor"),
                           ("Deployment", "cetirun-telemetry"), ("Deployment", "cetirun-kube-state-metrics")):
            self.assertIn((kind, name), workloads)
            self.assertEqual(workloads[(kind, name)]["metadata"]["namespace"], "cetirun-system")
        images = {name: workloads[(kind, name)]["spec"]["template"]["spec"]["containers"][0]["image"]
                  for kind, name in workloads if name in ("cetirun-adx-mon-collector", "cetirun-adx-mon-ingestor", "cetirun-telemetry")}
        self.assertEqual(images, {"cetirun-adx-mon-collector": "registry.example.com/cetirun-observability:0.1.1",
                                  "cetirun-adx-mon-ingestor": "registry.example.com/cetirun-observability:0.1.1",
                                  "cetirun-telemetry": "registry.example.com/cetirun-operator:0.1.1"})
        # The ingestor's TLS Secret is part of the release, not an input.
        self.assertFalse([reference for reference in deployment.references() if "adx-mon" in reference[2]])
        plan = deployment.plan()
        self.assertIn("cetirun-adx-mon-ingestor", plan["releases"]["platform"]["workloads"])
        self.assertIn("ADX ingestion with the ingestor identity", plan["notQualified"])
        bootstrap = {doc["metadata"]["name"] for doc in deployment.bootstrap_documents if doc["kind"] == "DaemonSet"}
        self.assertIn("cetirun-adx-mon-collector", bootstrap)


class MeshTargetTests(unittest.TestCase):
    def test_pinned_uid_rejects_replacement_and_deletion_without_patch(self):
        for document in (None, {"metadata": {"uid": "replacement", "resourceVersion": "1"}, "data": {"mesh": ""}}):
            calls = []
            def runner(command, **_kwargs):
                calls.append(command)
                return subprocess.CompletedProcess(command, 0, stdout=json.dumps(document) if document else "", stderr="")
            with self.assertRaisesRegex(RuntimeError, "pinned Istio ConfigMap"):
                DEPLOY.MESH.reconcile_configmap("mesh-system", "selected-mesh", runner=runner, uid="original")
            self.assertEqual(len(calls), 1)
            self.assertEqual(calls[0][1], "get")

    def test_resource_version_retry_preserves_other_mesh_fields_and_uid_pin(self):
        reads, patches = [], []
        def runner(command, **_kwargs):
            if command[1] == "get":
                reads.append(command)
                doc = {"metadata": {"uid": "original", "resourceVersion": str(len(reads))},
                       "data": {"mesh": f"trustDomain: concurrent-{len(reads)}.example\ndefaultConfig:\n  tracing: {{}}\n"}}
                return subprocess.CompletedProcess(command, 0, stdout=json.dumps(doc), stderr="")
            patches.append(json.loads(Path(command[-1]).read_text()))
            return subprocess.CompletedProcess(command, 1 if len(patches) == 1 else 0, stdout="", stderr="Conflict")
        DEPLOY.MESH.reconcile_configmap("mesh-system", "selected-mesh", runner=runner,
                                       service="oauth2-proxy.identity.svc.cluster.local", uid="original")
        self.assertEqual([item["metadata"] for item in patches], [
            {"resourceVersion": "1", "uid": "original"}, {"resourceVersion": "2", "uid": "original"}])
        mesh = yaml.safe_load(patches[-1]["data"]["mesh"])
        self.assertEqual(mesh["trustDomain"], "concurrent-2.example")
        self.assertEqual(mesh["defaultConfig"], {"tracing": {}})
        self.assertEqual(mesh["extensionProviders"][0]["envoyExtAuthzHttp"]["service"], "oauth2-proxy.identity.svc.cluster.local")

    def test_mesh_cli_binds_the_explicit_context(self):
        document = {"metadata": {"uid": "original", "resourceVersion": "1"},
                    "data": {"mesh": yaml.safe_dump({"extensionProviders": [DEPLOY.MESH._oauth2_proxy_provider()]})}}
        with patch.object(sys, "argv", ["mesh", "--context", "target-context", "--namespace", "mesh-system", "--name", "selected-mesh", "--uid", "original"]):
            with patch.object(DEPLOY.MESH.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, stdout=json.dumps(document))) as run:
                self.assertEqual(DEPLOY.MESH.main(), 0)
        command = run.call_args.args[0]
        self.assertEqual(command[:4], ["kubectl", "--context", "target-context", "--request-timeout=15s"])
        self.assertNotIn("use-context", command)

    def test_other_provider_service_is_not_overwritten(self):
        document = {"metadata": {"resourceVersion": "1"}, "data": {"mesh": yaml.safe_dump({
            "extensionProviders": [DEPLOY.MESH._oauth2_proxy_provider("other.identity.svc.cluster.local")]})}}
        with self.assertRaisesRegex(RuntimeError, "another service"):
            DEPLOY.MESH.configmap_patch(document, "oauth2-proxy.identity.svc.cluster.local")


@unittest.skipUnless(shutil.which("openssl") and shutil.which("helm"), "requires existing OpenSSL and Helm")
class PublicTLSPreflightTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix="cetirun-public-tls-test-")
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.directory = Path(cls.temporary.name)
        cls.serial = 10
        for name in ("issuer", "other-issuer"):
            cls.openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", f"/CN={name}",
                        "-addext", "basicConstraints=critical,CA:TRUE", "-keyout", str(cls.directory / f"{name}.key"),
                        "-out", str(cls.directory / f"{name}.pem"))

    @staticmethod
    def openssl(*args):
        subprocess.run(["openssl", *args], check=True, capture_output=True, text=True, timeout=30)

    @classmethod
    def certificate(cls, *, host=None, uri=None, days="1", client=False):
        cls.serial += 1
        prefix = cls.directory / str(cls.serial)
        extension = f"URI:{uri}" if uri else f"DNS:{host}"
        cls.openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=synthetic-test",
                    "-addext", f"subjectAltName={extension}", "-addext", f"extendedKeyUsage={'clientAuth' if client else 'serverAuth'}",
                    "-keyout", f"{prefix}.key", "-out", f"{prefix}.csr")
        cls.openssl("x509", "-req", "-in", f"{prefix}.csr", "-CA", str(cls.directory / "issuer.pem"),
                    "-CAkey", str(cls.directory / "issuer.key"), "-set_serial", str(cls.serial),
                    "-days", days, "-copy_extensions", "copy", "-out", f"{prefix}.pem")
        return Path(f"{prefix}.pem").read_text()

    def deployment(self, name):
        directory = self.directory / name
        directory.mkdir()
        fixture = yaml.safe_load(FIXTURE.read_text())
        fixture["platform"]["values"]["cetirun-operator"]["cetiadmin"] = {"enabled": False}
        path = directory / "environment.json"
        path.write_text(json.dumps(fixture))
        config, values = DEPLOY.configuration(path)
        deployment = SyntheticCluster(config, values, directory / "charts")
        deployment.render()
        deployment.seed_references()
        synthetic_run = deployment.run
        def run(command, **kwargs):
            if command[0] == "openssl":
                return DEPLOY.Deployment.run(deployment, command, **kwargs)
            return synthetic_run(command, **kwargs)
        deployment.run = run
        return deployment

    def test_bad_ca_wrong_san_and_expired_certificate_all_stop_before_writes(self):
        host = "cetirun-sandbox-admission.cetirun-system.svc"
        ca = (self.directory / "issuer.pem").read_text()
        for name, leaf, issuer in (
            ("bad-ca", self.certificate(host=host), (self.directory / "other-issuer.pem").read_text()),
            ("wrong-san", self.certificate(host="other.example.invalid"), ca),
            ("expired", self.certificate(host=host, days="0"), ca),
        ):
            with self.subTest(case=name):
                deployment = self.deployment(name)
                deployment.values["platform"]["cetirun-operator"]["sandboxAdmission"]["caBundle"] = base64.b64encode(issuer.encode()).decode()
                deployment.public_certificate = lambda *_args, leaf=leaf: leaf
                with self.assertRaisesRegex(ValueError, "openssl verify failed"):
                    deployment.execute()
                self.assertEqual(deployment.writes, [])

    def test_server_and_client_checks_use_the_existing_distinct_identity_contracts(self):
        deployment = self.deployment("valid")
        ca = (self.directory / "issuer.pem").read_text()
        host = "cetirun-sandbox-admission.cetirun-system.svc"
        deployment.verify_certificate("valid-server", self.certificate(host=host), ca, hostname=host)
        uri = "spiffe://cluster.local/ns/cetirun-system/sa/cetirun-connections-mcp"
        client = self.certificate(uri=uri, client=True)
        deployment.verify_certificate("valid-client", client, ca, client_uri=uri)
        with self.assertRaisesRegex(ValueError, "exactly the configured MCP service URI"):
            deployment.verify_certificate("other-client", client, ca, client_uri=uri + "-other")
        self.assertEqual(deployment.writes, [])

    def test_ingress_hostname_mismatch_is_rejected(self):
        deployment = self.deployment("wrong-ingress")
        leaf = self.certificate(host="other.example.invalid")
        deployment.public_certificate = lambda *_args: leaf
        deployment.verify_certificate = lambda *_args, **_kwargs: None
        with self.assertRaisesRegex(ValueError, "does not cover"):
            deployment.check_tls()
        self.assertEqual(deployment.writes, [])

    def test_current_matching_ingress_certificate_is_accepted(self):
        deployment = self.deployment("valid-ingress")
        leaf = self.certificate(host=deployment.config["domain"])
        deployment.public_certificate = lambda *_args: leaf
        deployment.verify_certificate = lambda *_args, **_kwargs: None
        deployment.check_tls()
        self.assertEqual(deployment.writes, [])

    def test_expired_ingress_certificate_is_rejected(self):
        deployment = self.deployment("expired-ingress")
        leaf = self.certificate(host=deployment.config["domain"], days="0")
        deployment.public_certificate = lambda *_args: leaf
        deployment.verify_certificate = lambda *_args, **_kwargs: None
        with self.assertRaisesRegex(ValueError, "openssl x509 failed"):
            deployment.check_tls()
        self.assertEqual(deployment.writes, [])


if __name__ == "__main__":
    unittest.main()
