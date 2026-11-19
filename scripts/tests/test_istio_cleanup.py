# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline retired-filter cleanup and chart contract checks."""

import argparse
import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("istio_cleanup", ROOT / "scripts/cetirun-istio-cleanup.py")
CLEANUP = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CLEANUP)


def arguments(**changes):
    values = dict(kubeconfig="/synthetic/config", context="synthetic", cluster_uid="cluster-uid",
                  release="example", release_namespace="release-system", legacy_namespace="mesh-system",
                  after_uninstall=False, expected_uid=None, expected_resource_version=None, apply=False)
    return argparse.Namespace(**{**values, **changes})


def legacy_filter():
    patches = []
    for filter_name, prefix in (("tcp_proxy", "egress_blacklist"), ("http_connection_manager", "egress_blacklist_http")):
        patches.append({
            "applyTo": "NETWORK_FILTER",
            "match": {"context": "SIDECAR_OUTBOUND", "listener": {"filterChain": {
                "filter": {"name": "envoy.filters.network." + filter_name}}}},
            "patch": {"operation": "INSERT_BEFORE", "value": {
                "name": "envoy.filters.network.rbac", "typed_config": {
                    "@type": "type.googleapis.com/envoy.extensions.filters.network.rbac.v3.RBAC",
                    "stat_prefix": prefix, "rules": {"action": "DENY", "policies": {
                        "synthetic": {"permissions": [{"requested_server_name": {"exact": "blocked.example.com"}}],
                                      "principals": [{"any": True}]}}}}}},
        })
    return {"apiVersion": "networking.istio.io/v1alpha3", "kind": "EnvoyFilter",
            "metadata": {"name": "example-blacklist", "namespace": "mesh-system", "uid": "filter-uid", "resourceVersion": "10",
                         "labels": {"app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": "example"},
                         "annotations": {"meta.helm.sh/release-name": "example",
                                         "meta.helm.sh/release-namespace": "release-system",
                                         "helm.sh/resource-policy": "keep"}},
            "spec": {"configPatches": patches}}


class API:
    def __init__(self):
        self.value = legacy_filter()
        self.cluster = "cluster-uid"
        self.releases = [{"name": "example", "status": "deployed"}]
        self.manifest = ""
        self.calls = []
        self.delete_error = None
        self.replacement = None

    def run(self, command, document=None):
        self.calls.append((command, document))
        if command[0] == "helm":
            return self.manifest if "manifest" in command else json.dumps(self.releases)
        if "kube-system" in command:
            return json.dumps({"metadata": {"uid": self.cluster}})
        if "delete" in command:
            if self.delete_error:
                raise self.delete_error
            self.value = self.replacement
            return "{}"
        return json.dumps(self.value) if self.value else ""

    def writes(self):
        return [(command, body) for command, body in self.calls if "delete" in command]


class IstioCleanupTests(unittest.TestCase):
    def test_default_plan_reads_only_the_explicit_target(self):
        api = API()
        result = CLEANUP.cleanup(arguments(), api.run)
        self.assertEqual(result, {"status": "verified-plan", "changed": False, "uid": "filter-uid", "resourceVersion": "10"})
        self.assertEqual(api.writes(), [])
        for command, _ in api.calls:
            self.assertIn("/synthetic/config", command)
            self.assertIn("synthetic", command)

    def test_delete_requires_reviewed_identity_and_verifies_absence(self):
        api = API()
        result = CLEANUP.cleanup(arguments(apply=True, expected_uid="filter-uid", expected_resource_version="10"), api.run)
        self.assertEqual(result["status"], "removed")
        self.assertTrue(result["changed"])
        writes = api.writes()
        self.assertEqual(len(writes), 1)
        self.assertIn("/apis/networking.istio.io/v1alpha3/namespaces/mesh-system/envoyfilters/example-blacklist", writes[0][0])
        self.assertEqual(writes[0][1]["preconditions"], {"uid": "filter-uid", "resourceVersion": "10"})

    def test_uninstall_requires_release_absence(self):
        api = API()
        with self.assertRaisesRegex(ValueError, "release still exists"):
            CLEANUP.cleanup(arguments(after_uninstall=True), api.run)
        api.releases = []
        result = CLEANUP.cleanup(arguments(after_uninstall=True, apply=True, expected_uid="filter-uid",
                                           expected_resource_version="10"), api.run)
        self.assertEqual(result["status"], "removed")

    def test_already_absent_is_idempotent_without_deletion(self):
        api = API()
        api.value = None
        self.assertEqual(CLEANUP.cleanup(arguments(), api.run), {"status": "absent", "changed": False})
        self.assertEqual(api.writes(), [])

    def test_invalid_context_or_ownership_never_deletes(self):
        for name, mutate in {
            "cluster": lambda api: setattr(api, "cluster", "other"),
            "release still renders filter": lambda api: setattr(api, "manifest", yaml.safe_dump(api.value)),
            "release failed": lambda api: setattr(api, "releases", [{"name": "example", "status": "failed"}]),
            "no release": lambda api: setattr(api, "releases", []),
            "foreign name": lambda api: api.value["metadata"].update(name="unrelated"),
            "foreign namespace": lambda api: api.value["metadata"].update(namespace="other"),
            "foreign kind": lambda api: api.value.update(kind="ServiceEntry"),
            "foreign owner": lambda api: api.value["metadata"]["annotations"].update({"meta.helm.sh/release-name": "other"}),
            "foreign release namespace": lambda api: api.value["metadata"]["annotations"].update({"meta.helm.sh/release-namespace": "other"}),
            "missing keep": lambda api: api.value["metadata"]["annotations"].pop("helm.sh/resource-policy"),
            "wrong manager": lambda api: api.value["metadata"]["labels"].update({"app.kubernetes.io/managed-by": "Other"}),
            "wrong instance": lambda api: api.value["metadata"]["labels"].update({"app.kubernetes.io/instance": "other"}),
            "replacement UID": lambda api: api.value["metadata"].update(uid="other"),
            "changed version": lambda api: api.value["metadata"].update(resourceVersion="11"),
            "deleting": lambda api: api.value["metadata"].update(deletionTimestamp="2026-01-01T00:00:00Z"),
            "finalizer": lambda api: api.value["metadata"].update(finalizers=["synthetic"]),
            "scoped replacement": lambda api: api.value["spec"].update(workloadSelector={"labels": {"app": "other"}}),
            "extra patch": lambda api: api.value["spec"]["configPatches"].append(copy.deepcopy(api.value["spec"]["configPatches"][0])),
            "wrong context": lambda api: api.value["spec"]["configPatches"][0]["match"].update(context="GATEWAY"),
            "wrong operation": lambda api: api.value["spec"]["configPatches"][0]["patch"].update(operation="REPLACE"),
        }.items():
            with self.subTest(name=name):
                api = API()
                mutate(api)
                with self.assertRaises(ValueError):
                    CLEANUP.cleanup(arguments(apply=True, expected_uid="filter-uid", expected_resource_version="10"), api.run)
                self.assertEqual(api.writes(), [])

    def test_unreviewed_apply_fails_before_external_calls(self):
        api = API()
        with self.assertRaises(ValueError):
            CLEANUP.cleanup(arguments(apply=True), api.run)
        self.assertEqual(api.calls, [])

    def test_empty_target_never_uses_default_context(self):
        for field in ("kubeconfig", "context", "cluster_uid"):
            with self.subTest(field=field):
                api = API()
                with self.assertRaises(ValueError):
                    CLEANUP.cleanup(arguments(**{field: ""}), api.run)
                self.assertEqual(api.calls, [])

    def test_delete_conflict_or_lost_reply_is_not_replayed(self):
        for error in (ValueError("conflict"), subprocess.TimeoutExpired("kubectl", 45)):
            with self.subTest(error=type(error).__name__):
                api = API()
                api.delete_error = error
                with self.assertRaises(type(error)):
                    CLEANUP.cleanup(arguments(apply=True, expected_uid="filter-uid", expected_resource_version="10"), api.run)
                self.assertEqual(len(api.writes()), 1)

    def test_post_delete_replacement_is_not_touched(self):
        api = API()
        api.replacement = legacy_filter()
        api.replacement["metadata"]["uid"] = "replacement"
        with self.assertRaisesRegex(ValueError, "replacement"):
            CLEANUP.cleanup(arguments(apply=True, expected_uid="filter-uid", expected_resource_version="10"), api.run)
        self.assertEqual(len(api.writes()), 1)

    @unittest.skipUnless(shutil.which("helm"), "requires Helm")
    def test_chart_removes_only_enforcement_and_rejects_retired_values(self):
        command = ["helm", "template", "example", str(ROOT / "infra/istio"), "--namespace", "release-system",
                   "--set", "clientId=synthetic", "--set", "tenantId=synthetic", "--set", "allowedGroups=synthetic"]
        env = {**os.environ, "KUBECONFIG": os.devnull}
        result = subprocess.run(command, capture_output=True, text=True, env=env, check=True)
        objects = [item for item in yaml.safe_load_all(result.stdout) if item]
        self.assertFalse(any(item["metadata"]["name"].endswith("-blacklist") for item in objects))
        self.assertTrue(any(item["kind"] == "ServiceEntry" for item in objects))
        self.assertTrue(any(item["metadata"]["name"].endswith("-accesslog-format") for item in objects))
        self.assertTrue(any(item["kind"] == "AuthorizationPolicy" for item in objects))
        for value in ("true", "false"):
            result = subprocess.run(command + ["--set", "egress.blacklist.enabled=" + value],
                                    capture_output=True, text=True, env=env, check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("egress.blacklist has been removed", result.stderr)


if __name__ == "__main__":
    unittest.main()
