# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Platform wiring of the Skill Marketplace, which the Operator chart runs."""

import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
OFFLINE = {
    **os.environ,
    "KUBECONFIG": os.devnull,
    "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}
CHARTS = ("cetirun-platform", "cetirun-operator/chart", "cetirun-portal/chart", "cetirun-ai-gateway/chart",
          "cetirun-connections-mcp/chart", "cetirun-observability/chart")
NAMESPACE = "cetirun-system"
WORKER = "cetirun-skill-marketplace"
# Render-only PEM framing, not a certificate.
CA_BUNDLE = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tClFRPT0KLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQ=="
MINIMAL = {
    "istio": {"enabled": True},
    "cetirun-operator": {"namespace": NAMESPACE, "domainSuffix": "agents.example.invalid", "cetiadmin": {"enabled": False}},
    "cetirun-portal": {"istio": {"host": "portal.example.invalid"}, "aiGateway": {"catalogNamespace": NAMESPACE}},
}
STORAGE = {"resourceGroup": "example-skills-rg", "storageAccount": "exampleskills", "containerName": "marketplace"}
# Synthetic native Sandbox platform with the marketplace switched on.
VALUES = {
    "istio": {"enabled": True},
    "global": {"registry": "registry.example.invalid", "version": "9.8.7"},
    "cetirun-operator": {
        "namespace": NAMESPACE, "domainSuffix": "agents.example.invalid", "cetiadmin": {"enabled": False},
        "sandboxAdmission": {
            "enabled": True, "enforce": True, "podsEnabled": True, "tlsSecretName": "sandbox-admission-tls",
            "caBundle": CA_BUNDLE, "adminUsers": ["admin@example.invalid"],
        },
        "sandboxSources": {
            "enabled": True, "nativeExecution": True, "bootstrapOwnedPrerequisites": True,
            "platformConfigMapName": "sandbox-platform",
        },
        "skillMarketplace": {
            "enabled": True,
            "repository": {
                "url": "https://git.example.invalid/example/skills.git", "auth": "token",
                "existingSecret": "skills-repository",
            },
            "storage": STORAGE,
        },
    },
    "cetirun-portal": {
        "istio": {"host": "portal.example.invalid"}, "aiGateway": {"catalogNamespace": NAMESPACE},
        "sandboxStorage": {"platformNamespace": NAMESPACE, "platformConfigMapName": "sandbox-platform"},
        "skillMarketplace": {"enabled": True, "catalogNamespace": NAMESPACE},
    },
}


def changed(*changes):
    values = copy.deepcopy(VALUES)
    for path, value in changes:
        section = values
        for key in path[:-1]:
            section = section.setdefault(key, {})
        section[path[-1]] = value
    return values


@unittest.skipUnless(shutil.which("helm"), "requires Helm 3 or 4")
class SkillMarketplacePlatformTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        directory = tempfile.TemporaryDirectory(prefix="cetirun-marketplace-platform-")
        cls.addClassCleanup(directory.cleanup)
        base = Path(directory.name)
        for relative in CHARTS:
            shutil.copytree(ROOT / "app" / relative, base / relative, ignore=shutil.ignore_patterns("charts"))
        cls.chart = base / "cetirun-platform"
        subprocess.run(["helm", "dependency", "build", "--skip-refresh", str(cls.chart)],
                       env=OFFLINE, check=True, capture_output=True, timeout=60)

    def helm(self, values, *, namespace=NAMESPACE, lint=False):
        command = ["helm", "lint", "--strict", str(self.chart)] if lint else [
            "helm", "template", "cetirun-platform", str(self.chart), "--kube-version", "1.37.0"]
        return subprocess.run([*command, "--namespace", namespace, "--values", "-"], input=json.dumps(values),
                              env=OFFLINE, text=True, capture_output=True, timeout=60, check=False)

    def render(self, values, **kwargs):
        result = self.helm(values, **kwargs)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return [document for document in yaml.safe_load_all(result.stdout) if document]

    @staticmethod
    def find(objects, kind, name):
        return next(doc for doc in objects if doc["kind"] == kind and doc["metadata"]["name"] == name)

    def environment(self, objects, name):
        deployment = self.find(objects, "Deployment", name)
        return {item["name"]: item.get("value") for item in deployment["spec"]["template"]["spec"]["containers"][0].get("env", [])}

    def worker(self, objects):
        return self.find(objects, "Deployment", WORKER)["spec"]["template"]["spec"]

    def worker_storage(self, objects):
        """The names of the worker's claim and volume, which name each other."""
        claims = [doc for doc in objects if doc["kind"] == "PersistentVolumeClaim" and doc["metadata"]["name"].startswith(WORKER)]
        volumes = [doc for doc in objects if doc["kind"] == "PersistentVolume" and doc["metadata"]["name"].startswith(WORKER)]
        self.assertEqual((len(claims), len(volumes)), (1, 1))
        claim, volume = claims[0], volumes[0]
        self.assertEqual(claim["spec"]["volumeName"], volume["metadata"]["name"])
        self.assertEqual(volume["spec"]["claimRef"]["name"], claim["metadata"]["name"])
        return claim["metadata"]["name"], volume["metadata"]["name"]

    def test_a_storage_change_renders_new_worker_storage(self):
        names = self.worker_storage(self.render(VALUES))
        self.assertEqual(self.worker_storage(self.render(VALUES)), names)
        storage = ("cetirun-operator", "skillMarketplace", "storage")
        for field, value in (("containerName", "moved"), ("storageAccount", "otherskills"), ("resourceGroup", "other-rg"),
                             ("storageClassName", "azureblob-nfs-standard"), ("size", "200Gi")):
            with self.subTest(field=field):
                other = self.worker_storage(self.render(changed(((*storage, field), value))))
                self.assertNotEqual(other[0], names[0])
                self.assertNotEqual(other[1], names[1])

    def test_the_operator_runs_the_worker_in_its_namespace(self):
        lint = self.helm(VALUES, lint=True)
        self.assertEqual(lint.returncode, 0, lint.stdout + lint.stderr)
        objects = self.render(VALUES)
        deployment = self.find(objects, "Deployment", WORKER)
        self.assertEqual(deployment["metadata"]["namespace"], NAMESPACE)
        # The selector of the former marketplace chart: an upgrade replaces the Pods in place.
        self.assertEqual(deployment["spec"]["selector"]["matchLabels"],
                         {"app.kubernetes.io/name": WORKER, "app.kubernetes.io/instance": "cetirun-platform"})
        self.assertEqual(deployment["spec"]["strategy"], {"type": "Recreate"})
        pod = deployment["spec"]["template"]["spec"]
        container = pod["containers"][0]
        # The worker is the second binary of the operator image.
        operator = self.find(objects, "Deployment", "cetirun-operator")["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["image"], operator["image"])
        self.assertEqual(container["image"], "registry.example.invalid/cetirun-operator:9.8.7")
        self.assertEqual(container["command"], ["/skill-marketplace"])
        self.assertIn(f"--namespace={NAMESPACE}", container["args"])
        self.assertIn("--repository-url=https://git.example.invalid/example/skills.git", container["args"])
        self.assertEqual(deployment["spec"]["template"]["metadata"]["annotations"]["sidecar.istio.io/inject"], "false")
        # The non-root process must be able to read the 0440 credentials Secret.
        self.assertEqual((pod["securityContext"]["runAsUser"], pod["securityContext"]["fsGroup"]), (65532, 65532))
        self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
        volumes = {volume["name"]: volume for volume in pod["volumes"]}
        self.assertEqual(volumes["credentials"]["secret"]["secretName"], "skills-repository")
        self.assertEqual(volumes["credentials"]["secret"]["items"], [{"key": "token", "path": "token"}])
        # The storage is part of the names of the worker's volume and claim.
        claim_name, volume_name = self.worker_storage(objects)
        self.assertEqual(volumes["storage"]["persistentVolumeClaim"]["claimName"], claim_name)
        # Room for one pack of the repository limit, 256 MiB, and 64 MiB more.
        self.assertEqual(volumes["tmp"]["emptyDir"]["sizeLimit"], 256 * 2**20 + 64 * 2**20)
        # The worker's read-write volume of the container. Helm does not keep it or its claim: a
        # storage change renders new ones, and the upgrade removes these. The container keeps its data.
        volume = self.find(objects, "PersistentVolume", volume_name)
        self.assertEqual(volume["spec"]["csi"]["volumeAttributes"], {**STORAGE, "protocol": "nfs"})
        self.assertEqual(volume["spec"]["persistentVolumeReclaimPolicy"], "Retain")
        self.assertEqual(volume["spec"]["claimRef"]["name"], claim_name)
        self.assertNotIn("helm.sh/resource-policy", volume["metadata"].get("annotations") or {})
        claim = self.find(objects, "PersistentVolumeClaim", claim_name)
        self.assertEqual(claim["metadata"]["namespace"], NAMESPACE)
        self.assertNotIn("helm.sh/resource-policy", claim["metadata"].get("annotations") or {})
        # The Operator gets the storage of the Agents' claims.
        config = json.loads(self.environment(objects, "cetirun-operator")["CETIRUN_SKILL_MARKETPLACE"])
        self.assertEqual(config, {"enabled": True, "storage": {**STORAGE, "storageClassName": "azureblob-nfs-premium"}})
        portal = self.environment(objects, "cetirun-portal")
        self.assertEqual(portal["SKILL_MARKETPLACE_ENABLED"], "true")
        self.assertEqual(portal["SKILL_MARKETPLACE_CATALOG_NAMESPACE"], NAMESPACE)

    def test_the_worker_can_only_read_the_teams_and_write_the_catalog(self):
        objects = self.render(VALUES)
        account = self.find(objects, "ServiceAccount", WORKER)
        self.assertFalse(account["automountServiceAccountToken"])
        bindings = [doc for doc in objects if doc["kind"] in ("RoleBinding", "ClusterRoleBinding")
                    and any(subject.get("name") == WORKER for subject in doc.get("subjects", []))]
        self.assertEqual([(doc["kind"], doc["roleRef"]["name"]) for doc in bindings], [("RoleBinding", f"{WORKER}-catalog")])
        role = self.find(objects, "Role", f"{WORKER}-catalog")
        self.assertEqual(role["metadata"]["namespace"], NAMESPACE)
        self.assertEqual(role["rules"], [
            {"apiGroups": [""], "resources": ["configmaps"], "resourceNames": ["cetirun-skill-marketplace-teams"], "verbs": ["get"]},
            {"apiGroups": [""], "resources": ["configmaps"], "resourceNames": ["cetirun-skill-marketplace"], "verbs": ["get", "update"]},
        ])

    def test_default_platform_has_no_marketplace(self):
        objects = self.render(MINIMAL)
        self.assertFalse([doc for doc in objects if doc["metadata"]["name"].startswith(WORKER)])
        self.assertNotIn("CETIRUN_SKILL_MARKETPLACE", self.environment(objects, "cetirun-operator"))
        self.assertNotIn("SKILL_MARKETPLACE_ENABLED", self.environment(objects, "cetirun-portal"))

    def test_limits_reach_the_worker(self):
        def args(values):
            return [arg for arg in self.worker(self.render(values))["containers"][0]["args"] if arg.startswith("--max-")]

        # The defaults are the worker's own.
        self.assertEqual(args(VALUES), [
            "--max-repository-size=256Mi", "--max-repository-objects=100000", "--max-skill-files=500",
            "--max-skill-size=5Mi", "--max-skill-md-size=256Ki", "--max-skill-depth=8", "--max-description-length=1024",
        ])
        limits = {"maxRepositorySize": "1Gi", "maxRepositoryObjects": 5000, "maxSkillFiles": 1000, "maxSkillSize": "20Mi",
                  "maxSkillMDSize": "524288", "maxSkillDepth": 12, "maxDescriptionLength": 2048}
        values = changed((("cetirun-operator", "skillMarketplace", "limits"), limits))
        self.assertEqual(args(values), [
            "--max-repository-size=1Gi", "--max-repository-objects=5000", "--max-skill-files=1000",
            "--max-skill-size=20Mi", "--max-skill-md-size=524288", "--max-skill-depth=12", "--max-description-length=2048",
        ])
        volumes = {volume["name"]: volume for volume in self.worker(self.render(values))["volumes"]}
        self.assertEqual(volumes["tmp"]["emptyDir"]["sizeLimit"], 2**30 + 64 * 2**20)
        for field, value in (("maxSkillFiles", 0), ("maxSkillDepth", 1.5), ("maxDescriptionLength", "many"),
                             ("maxRepositoryObjects", -1), ("maxSkillSize", "5MB"), ("maxRepositorySize", "0Mi"),
                             ("maxSkillMDSize", "-1Ki")):
            with self.subTest(field=field, value=value):
                result = self.helm(changed((("cetirun-operator", "skillMarketplace", "limits", field), value)))
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn(f"skillMarketplace.limits.{field} must be a", result.stderr)

    def test_ssh_and_anonymous_repositories(self):
        ssh = changed((("cetirun-operator", "skillMarketplace", "repository"),
                       {"url": "git@git.example.invalid:example/skills.git", "auth": "ssh", "existingSecret": "skills-ssh"}))
        volumes = {volume["name"]: volume for volume in self.worker(self.render(ssh))["volumes"]}
        self.assertEqual(volumes["credentials"]["secret"]["items"],
                         [{"key": "ssh-privatekey", "path": "ssh-privatekey"}, {"key": "known_hosts", "path": "known_hosts"}])
        anonymous = changed((("cetirun-operator", "skillMarketplace", "repository"),
                             {"url": "https://git.example.invalid/example/skills.git", "auth": "none"}))
        pod = self.worker(self.render(anonymous))
        self.assertNotIn("credentials", [volume["name"] for volume in pod["volumes"]])
        self.assertNotIn("credentials", [mount["name"] for mount in pod["containers"][0]["volumeMounts"]])

    def test_invalid_marketplace_values_are_refused(self):
        marketplace = ("cetirun-operator", "skillMarketplace")
        cases = (
            ((*marketplace, "repository", "url"), "", "skillMarketplace.repository.url is required"),
            ((*marketplace, "repository", "auth"), "basic", "skillMarketplace.repository.auth must be none, token or ssh"),
            ((*marketplace, "repository", "existingSecret"), "", "skillMarketplace.repository.existingSecret is required"),
            ((*marketplace, "repository", "url"), "https://user:secret@git.example.invalid/x.git", "must not contain credentials"),
            ((*marketplace, "repository", "url"), "https://git.example.invalid/x.git?token=synthetic", "must not contain a query or a fragment"),
            ((*marketplace, "repository", "url"), "https://git.example.invalid/x.git#synthetic", "must not contain a query or a fragment"),
            ((*marketplace, "repository", "url"), "http://git.example.invalid/x.git", "must use https://"),
            ((*marketplace, "storage", "storageAccount"), "Example", "storageAccount must be 3-24 lowercase"),
            ((*marketplace, "storage", "containerName"), "a--b", "containerName must be a valid Blob container name"),
            ((*marketplace, "storage", "resourceGroup"), "rg.", "resourceGroup must be a resource group name"),
            ((*marketplace, "storage", "containerName"), "", "skillMarketplace.storage.containerName is required"),
            ((*marketplace, "extra"), True, "unsupported skillMarketplace field extra"),
            (("cetirun-operator", "sandboxSources", "skillsClaimName"), "", "requires sandboxSources.skillsClaimName"),
            (("cetirun-operator", "sandboxSources", "nativeExecution"), False, "requires sandboxSources.enabled and sandboxSources.nativeExecution"),
        )
        for path, value, message in cases:
            with self.subTest(path=".".join(path), value=value):
                result = self.helm(changed((path, value)))
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn(message, result.stderr)

    def test_inconsistent_wiring_is_refused(self):
        other = "other-namespace"
        cases = (
            (("cetirun-portal", "skillMarketplace", "enabled"), False,
             "cetirun-portal.skillMarketplace.enabled must be true when cetirun-operator.skillMarketplace.enabled is"),
            (("cetirun-portal", "skillMarketplace", "catalogNamespace"), other,
             f"cetirun-portal.skillMarketplace.catalogNamespace is {other}, but cetirun-operator.namespace is {NAMESPACE}"),
            (("cetirun-operator", "skillMarketplace", "enabled"), False,
             "cetirun-portal.skillMarketplace.enabled requires the bundled Operator with skillMarketplace.enabled"),
        )
        for path, value, message in cases:
            with self.subTest(path=".".join(path)):
                result = self.helm(changed((path, value)))
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn(message, result.stderr)


if __name__ == "__main__":
    unittest.main()
