# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Rendered Skill Marketplace contract of a reconciled native Agent."""

import copy
import json
import os
from pathlib import Path
import subprocess
import unittest

import yaml


CHART = Path(__file__).resolve().parents[1]
OFFLINE = {
    **os.environ,
    "KUBECONFIG": os.devnull,
    "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}
TEAM_PATH = "teams/00000000-0000-4000-8000-000000000001"
SELECTION = "sha256:" + "0123456789abcdef" * 4
SELECTION_ENV = "CETIRUN_SKILLS_RELEASE_SELECTION"
CONTEXT_ERROR = "Skill Marketplace skills require the composed Agent context"
SANDBOX_ERROR = "Skill Marketplace skills require a reconciled Sandbox workload"
PAIR_ERROR = (
    "Skill Marketplace skills require both skills.subPath and "
    "skills.marketplace.selectionDigest"
)
SHARED_ERROR = "Skill Marketplace skills require skills.shared"
MOUNT_PATH = "/home/vibe/.aks/skills"


def render(values):
    return subprocess.run(
        [
            "helm", "template", "marketplace-test", str(CHART),
            "--namespace", "example-agent", "--kube-version", "1.37.0",
            "--values", "-",
        ],
        input=json.dumps(values), text=True, capture_output=True,
        env=OFFLINE, timeout=60, check=False,
    )


def reconciled_values(agent_type):
    """Synthetic reconciled native Agent with a separately owned skills claim."""
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
    values = {
        "agentType": agent_type,
        "replicaCount": 1,
        "runtimeClassName": "example-kata",
        "sandboxWorkload": {
            "contractVersion": "v1", "reconciled": True, "executionHeld": False,
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
        "skills": {"shared": True, "existingClaim": "skills"},
        "memoryContext": {"enabled": True, "image": "registry.example.com/memory:1.2.3"},
        "desktop": {"enabled": False},
        "ttyd": {"enabled": False},
        "logger": {"enabled": False},
        "execRestriction": {"enabled": False},
        "openclaw": {
            "image": {"repository": "registry.example.com/openclaw-gateway", "tag": "example"},
            "config": {"browser": {"enabled": False}},
        },
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
    if agent_type == "hermes":
        # The chart requires the Hermes Webapp's public URL; the operator sets it from the platform domain.
        values["hermes"] = {"env": {"HERMES_WEBAPP_PUBLIC_URL": "https://agents.example.com/ui/example-agent"}}
    return values


def marketplace_values(agent_type, sub_path=TEAM_PATH, selection=SELECTION):
    values = reconciled_values(agent_type)
    if sub_path is not None:
        values["skills"]["subPath"] = sub_path
    if selection is not None:
        values["skills"]["marketplace"] = {"selectionDigest": selection}
    return values


def pod_spec(stdout):
    workload = next(
        document for document in yaml.safe_load_all(stdout)
        if document and document["kind"] == "StatefulSet"
    )
    return workload["spec"]["template"]["spec"]


def skills_dirs(stdout, agent_type):
    """The skills directories in the rendered runtime configuration."""
    for document in yaml.safe_load_all(stdout):
        data = (document or {}).get("data") or {}
        if agent_type == "hermes" and "hermes-config.yaml" in data:
            return yaml.safe_load(data["hermes-config.yaml"])["skills"]["external_dirs"]
        if agent_type == "openclaw" and "openclaw.json" in data:
            return json.loads(data["openclaw.json"])["skills"]["load"]["extraDirs"]
    raise AssertionError(f"no {agent_type} configuration in the render")


class SkillMarketplaceChartTests(unittest.TestCase):
    def assert_refused(self, values, error):
        rendered = render(values)
        self.assertNotEqual(rendered.returncode, 0, rendered.stdout)
        self.assertIn(error, rendered.stderr)

    def test_only_the_initializer_mounts_the_team_subtree(self):
        for agent_type in ("openclaw", "hermes"):
            with self.subTest(agent_type=agent_type):
                rendered = render(marketplace_values(agent_type))
                self.assertEqual(rendered.returncode, 0, rendered.stderr)
                pod = pod_spec(rendered.stdout)
                containers = pod["initContainers"] + pod["containers"]
                initializer = next(c for c in containers if c["name"] == "init-home-data")
                self.assertIn({
                    "name": "shared-skills", "mountPath": "/etc/cetirun-shared-skills",
                    "readOnly": True, "subPath": TEAM_PATH,
                }, initializer["volumeMounts"])
                self.assertIn({"name": SELECTION_ENV, "value": SELECTION}, initializer["env"])
                for container in containers:
                    names = [item["name"] for item in container.get("env") or []]
                    mounts = [item["name"] for item in container.get("volumeMounts") or []]
                    owned = container is initializer
                    self.assertEqual(names.count(SELECTION_ENV), int(owned), container["name"])
                    self.assertEqual(mounts.count("shared-skills"), int(owned), container["name"])
                main = pod["containers"][0]
                self.assertNotIn("shared-skills", [m["name"] for m in main["volumeMounts"]])
                # The runtime reads the release that the initializer copied,
                # at skills.mountPath; HOME keeps the personal skills.
                self.assertIn({
                    "name": "context-view", "mountPath": MOUNT_PATH,
                    "subPath": "current/team-skills", "readOnly": True,
                }, main["volumeMounts"])
                self.assertNotIn("/home/vibe/.agents", [m["mountPath"] for m in main["volumeMounts"]])
                volume = next(v for v in pod["volumes"] if v["name"] == "shared-skills")
                self.assertEqual(
                    volume["persistentVolumeClaim"], {"claimName": "skills", "readOnly": True},
                )

    def test_empty_marketplace_values_do_not_change_the_render(self):
        for agent_type in ("openclaw", "hermes"):
            with self.subTest(agent_type=agent_type):
                omitted = render(reconciled_values(agent_type))
                empty = render(marketplace_values(agent_type, sub_path="", selection=""))
                self.assertEqual(omitted.returncode, 0, omitted.stderr)
                self.assertEqual(empty.returncode, 0, empty.stderr)
                self.assertEqual(empty.stdout, omitted.stdout)
                # The chart ConfigMap embeds the init script, which names the
                # variable, so check the containers' environment instead.
                pod = pod_spec(omitted.stdout)
                containers = pod["initContainers"] + pod["containers"]
                names = [item["name"] for c in containers for item in c.get("env") or []]
                self.assertNotIn(SELECTION_ENV, names)
                # Without the marketplace, native Agents mount the claim itself.
                initializer = next(c for c in pod["initContainers"] if c["name"] == "init-home-data")
                self.assertNotIn("shared-skills", [m["name"] for m in initializer["volumeMounts"]])
                self.assertIn({
                    "name": "shared-skills", "mountPath": MOUNT_PATH, "readOnly": True,
                }, pod["containers"][0]["volumeMounts"])

    def test_marketplace_values_require_the_composed_context(self):
        for agent_type in ("openclaw", "hermes"):
            without_context = marketplace_values(agent_type)
            without_context["memoryContext"]["enabled"] = False
            held = marketplace_values(agent_type)
            held["replicaCount"] = 0
            held["sandboxWorkload"]["executionHeld"] = True
            del held["sandboxWorkload"]["reconciled"]
            for name, values in (("memory disabled", without_context), ("held", held)):
                with self.subTest(agent_type=agent_type, case=name):
                    self.assert_refused(values, CONTEXT_ERROR)

    def test_marketplace_values_require_a_reconciled_sandbox(self):
        # An ordinary Agent has a small context view for the copied release.
        for agent_type in ("openclaw", "hermes"):
            with self.subTest(agent_type=agent_type):
                values = marketplace_values(agent_type)
                for name in ("sandboxWorkload", "runtimeClassName"):
                    del values[name]
                values["sidecarScope"] = {"enabled": True}
                self.assert_refused(values, SANDBOX_ERROR)

    def test_marketplace_values_must_be_set_together(self):
        for agent_type in ("openclaw", "hermes"):
            for name, values in (
                ("subPath only", marketplace_values(agent_type, selection=None)),
                ("selectionDigest only", marketplace_values(agent_type, sub_path=None)),
                ("empty selectionDigest", marketplace_values(agent_type, selection="")),
                ("empty subPath", marketplace_values(agent_type, sub_path="")),
            ):
                with self.subTest(agent_type=agent_type, case=name):
                    self.assert_refused(values, PAIR_ERROR)

    def test_marketplace_values_require_shared_skills(self):
        for agent_type in ("openclaw", "hermes"):
            with self.subTest(agent_type=agent_type):
                values = marketplace_values(agent_type)
                values["skills"]["shared"] = False
                self.assert_refused(values, SHARED_ERROR)

    def test_schema_rejects_malformed_marketplace_values(self):
        base = marketplace_values("openclaw")
        for field, value in (
            ("subPath", "teams/../other-team"),
            ("subPath", "/teams/example"),
            ("subPath", "teams/"),
            ("subPath", "other/example"),
            ("subPath", "teams/example/nested"),
            ("selectionDigest", "sha256:" + "A" * 64),
            ("selectionDigest", "sha256:0123"),
            ("selectionDigest", "0123456789abcdef" * 4),
            ("release", "unexpected"),
        ):
            with self.subTest(field=field, value=value):
                values = copy.deepcopy(base)
                if field == "subPath":
                    values["skills"]["subPath"] = value
                else:
                    values["skills"]["marketplace"][field] = value
                rendered = render(values)
                self.assertNotEqual(rendered.returncode, 0, rendered.stdout)
                self.assertIn(field, rendered.stderr)

    def test_runtimes_list_the_team_skills_once(self):
        # The Team's skills are at skills.mountPath and the personal skills in
        # HOME, so each runtime lists every directory once and sees no skill
        # twice.
        for agent_type in ("openclaw", "hermes"):
            with self.subTest(agent_type=agent_type):
                rendered = render(marketplace_values(agent_type))
                self.assertEqual(rendered.returncode, 0, rendered.stderr)
                dirs = skills_dirs(rendered.stdout, agent_type)
                self.assertEqual(dirs.count(MOUNT_PATH), 1, dirs)
                self.assertEqual(dirs.count("/home/vibe/.agents/skills"), 1, dirs)
                self.assertEqual(len(dirs), len(set(dirs)), dirs)


if __name__ == "__main__":
    unittest.main()
