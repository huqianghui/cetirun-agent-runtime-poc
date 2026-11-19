# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline regression coverage for retained storage and repeated preflights."""

import copy
import json
import shutil
import unittest
from unittest.mock import patch

from scripts.tests.test_full_deployment import DEPLOY, DeploymentFixture


class ToolchainTests(unittest.TestCase):
    def test_supported_skew_and_managed_server_suffix(self):
        deployment = DEPLOY.Deployment({}, {}, None)
        for minor in ("36", "37", "38"):
            versions = {"clientVersion": {"major": "1", "minor": minor},
                        "serverVersion": {"major": "1", "minor": "37+"}}
            with self.subTest(minor=minor), patch.object(deployment, "kubectl", return_value=json.dumps(versions)):
                deployment.check_kubectl_version()

    def test_rejects_skew_old_server_and_missing_or_invalid_versions(self):
        cases = (("1", "31", "1", "37", "one-minor skew"),
                 ("1", "39", "1", "37", "one-minor skew"),
                 ("2", "37", "1", "37", "one-minor skew"),
                 ("1", "36", "1", "36", "1.37 or newer"),
                 ("1", "invalid", "1", "37", "parseable"),
                 ("1", "37", "1", "37garbage", "parseable"))
        deployment = DEPLOY.Deployment({}, {}, None)
        for cm, cn, sm, sn, error in cases:
            versions = {"clientVersion": {"major": cm, "minor": cn}, "serverVersion": {"major": sm, "minor": sn}}
            with self.subTest(versions=versions), patch.object(deployment, "kubectl", return_value=json.dumps(versions)):
                with self.assertRaisesRegex(ValueError, error):
                    deployment.check_kubectl_version()
        with patch.object(deployment, "kubectl", return_value='{"serverVersion":{"major":"1","minor":"37"}}'):
            with self.assertRaisesRegex(ValueError, "parseable"):
                deployment.check_kubectl_version()


@unittest.skipUnless(shutil.which("helm"), "requires existing Helm")
class ArtifactPreflightTests(DeploymentFixture, unittest.TestCase):
    def test_incompatible_kubectl_stops_before_artifact_checks_or_writes(self):
        deployment = self.deployment()
        original = deployment.kubectl

        def kubectl(*args, input_text=None):
            if args[0] == "version":
                return json.dumps({"clientVersion": {"major": "1", "minor": "31"},
                                   "serverVersion": {"major": "1", "minor": "37"}})
            return original(*args, input_text=input_text)

        with patch.object(deployment, "kubectl", side_effect=kubectl):
            with self.assertRaisesRegex(ValueError, "one-minor skew"):
                deployment.execute()
        self.assertFalse(deployment.writes)
        self.assertFalse(any(cmd[0] == "docker" for cmd in deployment.commands))

    def test_only_successful_immutable_inspections_are_reused(self):
        deployment = self.deployment()
        deployment.preflight()
        first = list(deployment.commands)
        deployment.commands.clear()
        deployment.preflight()
        immutable = lambda commands: [cmd[-1] for cmd in commands
            if cmd[:4] == ["docker", "buildx", "imagetools", "inspect"] and "@sha256:" in cmd[-1]]
        mutable = lambda commands: [cmd for cmd in commands if
            (cmd[:4] == ["docker", "buildx", "imagetools", "inspect"] and "@sha256:" not in cmd[-1])
            or cmd[:3] == ["helm", "show", "chart"]]
        self.assertTrue(immutable(first))
        self.assertEqual(immutable(deployment.commands), [])
        self.assertEqual(mutable(first), mutable(deployment.commands))
        self.assertTrue(any(cmd[4:6] == ["config", "view"] for cmd in deployment.commands))
        reference = "registry.example.com/changed@sha256:" + "b" * 64
        deployment.fail = reference
        for _ in range(2):
            with self.assertRaisesRegex(ValueError, "Synthetic failure"):
                deployment.inspect_image(reference)
        self.assertNotIn(reference, deployment.verified_image_digests)
        deployment.fail = ""
        deployment.inspect_image(reference)
        self.assertIn(reference, deployment.verified_image_digests)
        deployment.fail = "config view"
        with self.assertRaisesRegex(ValueError, "Synthetic failure"):
            deployment.preflight()
        self.assertFalse(deployment.writes)


@unittest.skipUnless(shutil.which("helm"), "requires existing Helm")
class RetainedPostgresqlTests(DeploymentFixture, unittest.TestCase):
    def retained(self):
        self.config["platform"]["postgresqlStorage"] = {"mode": "retain-default", "storageClassName": "default"}
        self.config["platform"]["values"]["cetirun-ai-gateway"]["postgresql"]["bundled"]["persistence"]["storageClass"] = ""
        deployment = self.deployment()
        namespace, release = (deployment.config["platform"][key] for key in ("namespace", "release"))
        deployment.installed.add(release)
        deployment.existing_releases.add("platform")
        for doc in deployment.documents["platform"]:
            if doc["kind"] == "Secret":
                secret_name = doc["metadata"]["name"]
                deployment.objects[("secret", namespace, secret_name)] = {"uid": "uid-" + secret_name, "annotations": {
                    "meta.helm.sh/release-name": release, "meta.helm.sh/release-namespace": namespace}}
                deployment.keys[(namespace, secret_name)] = set(doc.get("data", {}))
        name, claim = "cetirun-ai-gateway-postgresql", "data-cetirun-ai-gateway-postgresql-0"
        templates = copy.deepcopy(next(doc for doc in deployment.documents["platform"]
            if doc["kind"] == "StatefulSet" and doc["metadata"]["name"] == name)["spec"]["volumeClaimTemplates"])
        templates[0]["metadata"]["creationTimestamp"] = None
        templates[0]["spec"]["volumeMode"] = "Filesystem"
        templates[0]["status"] = {"phase": "Pending"}
        self.state = {"templates": templates,
            "spec": {**copy.deepcopy(templates[0]["spec"]), "storageClassName": "default", "volumeName": "retained-volume"},
            "phase": "Bound", "binding": {"name": claim, "namespace": namespace, "uid": "claim-uid"}, "class": "default"}
        objects = {
            ("statefulset", namespace, name): {"uid": "database-uid", "annotations": {
                "meta.helm.sh/release-name": release, "meta.helm.sh/release-namespace": namespace}},
            ("persistentvolumeclaim", namespace, claim): {"uid": "claim-uid"},
            ("secret", namespace, name): {"uid": "password-uid", "annotations": {
                "meta.helm.sh/release-name": release, "meta.helm.sh/release-namespace": namespace}},
            ("persistentvolume", "", "retained-volume"): {"uid": "volume-uid"},
            ("storageclass", "", "default"): {"uid": "class-uid"},
        }
        deployment.objects.update(objects)
        deployment.keys[(namespace, name)] = {"password", "database-url"}
        original = deployment.selected_fields

        def fields(kind, name, selected, namespace=""):
            if kind == "StatefulSet" and selected == ("spec.volumeClaimTemplates",):
                return [json.dumps(self.state["templates"])]
            if kind == "PersistentVolumeClaim" and selected == ("spec", "status.phase"):
                return [json.dumps(self.state["spec"]), self.state["phase"]]
            if kind == "PersistentVolume" and selected == ("spec.claimRef", "spec.storageClassName"):
                return [json.dumps(self.state["binding"]), self.state["class"]]
            return original(kind, name, selected, namespace)

        self.addCleanup(patch.stopall)
        patch.object(deployment, "selected_fields", side_effect=fields).start()
        return deployment

    def test_explicit_mode_preserves_omitted_template_class_and_checks_actual_class(self):
        deployment = self.retained()
        self.assertNotIn("storageClassName", self.state["templates"][0]["spec"])
        self.assertIn(("StorageClass", "", "default", ""), deployment.references())
        self.assertNotIn(("StorageClass", "", "", ""), deployment.references())
        deployment.preflight()
        deployment.check_retained_postgresql()
        self.assertFalse(deployment.writes)

    def test_blank_class_requires_explicit_retention_inputs(self):
        self.retained()
        cases = (None, {}, {"mode": "retain-default"}, {"mode": "auto", "storageClassName": "default"},
                 {"mode": "retain-default", "storageClassName": ""})
        for storage in cases:
            with self.subTest(storage=storage):
                config = copy.deepcopy(self.config)
                config["platform"]["postgresqlStorage"] = storage
                with self.assertRaises(ValueError):
                    self.configure(config)
        self.config["platform"]["values"]["cetirun-ai-gateway"]["postgresql"]["bundled"]["persistence"]["storageClass"] = "default"
        with self.assertRaisesRegex(ValueError, "explicit empty"):
            self.configure(self.config)

    def test_claim_template_and_bound_volume_changes_are_rejected(self):
        deployment = self.retained()
        initial = copy.deepcopy(self.state)
        changes = (("templates.0.spec.storageClassName", "default"),
                   ("templates.0.spec.resources.requests.storage", "99Gi"),
                   ("templates.0.metadata.annotations", {"unexpected": "change"}),
                   ("spec.storageClassName", "other"), ("spec.accessModes", ["ReadWriteMany"]),
                   ("spec.resources.requests.storage", "99Gi"), ("phase", "Pending"),
                   ("binding.uid", "replacement"), ("class", "other"))
        for path, value in changes:
            with self.subTest(path=path):
                self.state = copy.deepcopy(initial)
                target = self.state
                parts = path.split(".")
                for part in parts[:-1]:
                    target = target[int(part)] if isinstance(target, list) else target[part]
                target[parts[-1]] = value
                with self.assertRaisesRegex(ValueError, "Retained PostgreSQL"):
                    deployment.check_retained_postgresql()
                self.assertFalse(deployment.writes)

    def test_identities_are_pinned_across_preflight_and_upgrade(self):
        deployment = self.retained()
        deployment.check_retained_postgresql()
        keys = [key for key in deployment.objects if key[0] in
                ("statefulset", "persistentvolumeclaim", "persistentvolume", "secret", "storageclass")
                and deployment.objects[key]["uid"] in ("database-uid", "claim-uid", "volume-uid", "password-uid", "class-uid")]
        self.assertEqual(len(keys), 5)
        for key in keys:
            with self.subTest(kind=key[0]):
                original = deployment.objects[key]["uid"]
                deployment.objects[key]["uid"] = "replacement"
                with self.assertRaisesRegex(ValueError, "Retained PostgreSQL"):
                    deployment.upgrade("platform")
                self.assertFalse(deployment.writes)
                deployment.objects[key]["uid"] = original

    def test_missing_and_foreign_storage_fail_before_any_upgrade(self):
        deployment = self.retained()
        key = next(key for key in deployment.objects if key[0] == "statefulset")
        metadata = deployment.objects.pop(key)
        with self.assertRaisesRegex(ValueError, "existing, non-terminating"):
            deployment.upgrade("platform")
        metadata["annotations"]["meta.helm.sh/release-name"] = "another-release"
        deployment.objects[key] = metadata
        with self.assertRaisesRegex(ValueError, "another release"):
            deployment.upgrade("platform")
        self.assertFalse(deployment.writes)

    def test_retention_cannot_adopt_orphaned_storage_as_a_new_release(self):
        deployment = self.retained()
        deployment.installed.clear()
        with self.assertRaisesRegex(ValueError, "existing platform Helm release"):
            deployment.preflight()
        self.assertFalse(deployment.writes)

    def test_retention_rejects_uninstalled_or_superseded_release_history(self):
        deployment = self.retained()
        original = deployment.helm
        release = deployment.config["platform"]["release"]
        for status in ("uninstalled", "superseded", "uninstalling"):
            with self.subTest(status=status):
                def helm(*args):
                    if args[0] == "list" and args[args.index("--filter") + 1] == "^" + release + "$":
                        return json.dumps([{"name": release, "status": status}])
                    return original(*args)

                with patch.object(deployment, "helm", side_effect=helm):
                    with self.assertRaisesRegex(ValueError, "deployed or failed"):
                        deployment.preflight()
                self.assertFalse(deployment.writes)

    def test_storage_is_rechecked_after_helm_returns(self):
        deployment = self.retained()
        deployment.check_retained_postgresql()
        key = next(key for key in deployment.objects if key[0] == "persistentvolume")

        def replace_volume(*args):
            deployment.objects[key]["uid"] = "replaced-during-upgrade"
            return ""

        with patch.object(deployment, "helm", side_effect=replace_volume) as helm:
            with self.assertRaisesRegex(ValueError, "identity or storage changed"):
                deployment.upgrade("platform")
        helm.assert_called_once()
        self.assertNotIn("--force", helm.call_args.args)

    def test_replacement_during_storage_read_is_rejected(self):
        deployment = self.retained()
        original = deployment.selected_fields
        key = next(key for key in deployment.objects if key[0] == "statefulset")

        def replace_statefulset(*args):
            result = original(*args)
            deployment.objects[key]["uid"] = "replaced-during-read"
            return result

        with patch.object(deployment, "selected_fields", side_effect=replace_statefulset):
            with self.assertRaisesRegex(ValueError, "identity changed while checking"):
                deployment.check_retained_postgresql()
        self.assertIsNone(deployment.retained_database_pin)
        self.assertFalse(deployment.writes)


if __name__ == "__main__":
    unittest.main()
