# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline regressions for the explicitly local platform helper."""

import copy
from decimal import Decimal
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("local_platform_probe", Path(__file__).with_name("platform-probe.py"))
probe = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(probe)


class LocalPlatformProbeTests(unittest.TestCase):
    def test_documented_example_matches_local_target_guard(self):
        readme = Path(__file__).with_name("README.md").read_text()
        options = dict(re.findall(r"--(cluster-name|node-container|context)\s+(\S+)", readme))
        args = SimpleNamespace(
            cluster_name=options["cluster-name"], node_container=options["node-container"],
            context=options["context"], cluster_uid_file="/example/cluster-uid",
            kubeconfig="/example/kubeconfig", kubectl="/example/kubectl",
        )
        with patch.object(probe.Path, "read_text", return_value="11111111-2222-3333-4444-555555555555"), \
                patch.object(probe.LocalKubernetes, "command", return_value="https://127.0.0.1:12345"), \
                patch.object(probe.LocalKubernetes, "check_target") as target:
            probe.LocalKubernetes(args)
            target.assert_called_once()

    def test_only_literal_loopback_https_endpoints_are_accepted(self):
        for address in ("https://127.0.0.1:12345", "https://[::1]:12345"):
            probe.local_endpoint(address)
        for address in ("https://example.com:443", "https://192.0.2.1:443",
                        "http://127.0.0.1:12345", "https://user@127.0.0.1:12345",
                        "https://127.0.0.1"):
            with self.subTest(address=address), self.assertRaises(probe.Error):
                probe.local_endpoint(address)

    def test_boolean_controls_are_strict(self):
        self.assertEqual(probe.booleans("[true,false,true]", 3), [True, False, True])
        for raw in ('[1,false,true]', '[true]', '{"allowed":true}', "bad"):
            with self.subTest(raw=raw), self.assertRaises(probe.Error):
                probe.booleans(raw, 3)

    def test_guest_scripts_compile_without_execution(self):
        for script in (probe.GUEST_RESOURCES, probe.FILESYSTEM, probe.DNS_SERVER, probe.DNS_PROBE):
            compile(script, "<synthetic-pod>", "exec")

    def test_host_bounds_need_pod_ancestors_not_global_caps(self):
        uid = "11111111-2222-3333-4444-555555555555"
        unconstrained = [{"ancestors": [
            {"path": "/literal-pod" + uid.replace("-", "_") + ".slice:runtime:id",
             "cpuMax": "max 100000", "memoryMax": "max"},
            {"path": "/", "cpuMax": "125000 100000", "memoryMax": "872415232"},
        ]}]
        bounded = [{"ancestors": [
            {"path": "/kubepods/pod" + uid + "/kata", "cpuMax": "max 100000", "memoryMax": "max"},
            {"path": "/kubepods/pod" + uid, "cpuMax": "125000 100000", "memoryMax": "872415232"},
        ]}]
        self.assertFalse(probe.cgroup_bounds(unconstrained, uid, Decimal("1.25"), Decimal(872415232))[0])
        self.assertTrue(probe.cgroup_bounds(bounded, uid, Decimal("1.25"), Decimal(872415232))[0])
        self.assertFalse(probe.cgroup_bounds(bounded, uid, Decimal("1"), Decimal(872415232))[0])
        self.assertFalse(probe.cgroup_bounds(bounded, uid, Decimal("1.25"), Decimal(536870912))[0])
        self.assertFalse(probe.cgroup_bounds(bounded + unconstrained, uid, Decimal("1.25"), Decimal(872415232))[0])
        self.assertFalse(probe.cgroup_bounds([], uid, Decimal("1.25"), Decimal(872415232))[0])

    def test_quantity_uses_exact_cpu_and_memory_units(self):
        self.assertEqual(probe.quantity("250m"), Decimal("0.250"))
        self.assertEqual(probe.quantity("512Mi") + probe.quantity("320Mi"), 872415232)
        for value in ("max", "-1", "NaN", "1e100"):
            with self.subTest(value=value), self.assertRaises(probe.Error):
                probe.quantity(value)

    def test_port_remapping_preserves_shared_network_probe_order(self):
        script = probe.remap(probe.qualify.PROBE, 24000)
        self.assertIn("(24002,24000,24001)", script)
        self.assertIn("(24000,24001,24002)", script)
        self.assertNotIn("8080", script)

    def client(self):
        kube = object.__new__(probe.LocalKubernetes)
        kube.prefix = ["kubectl", "--request-timeout=90s"]
        kube.namespaces = {"cetirun-local-platform-0123456789ab": ("owned-uid", "run")}
        kube.pods = {}
        kube.volumes = {}
        kube.uncertain_creates = []
        kube.stage_namespaces = []
        kube.deadline = time.monotonic() + 180
        return kube

    def test_no_write_after_target_check_fails(self):
        kube = self.client()
        with patch.object(kube, "check_target", side_effect=probe.Error("wrong UID")), \
                patch.object(kube, "command") as command:
            with self.assertRaisesRegex(probe.Error, "wrong UID"):
                kube.run(["create", "-f", "-"], {})
            command.assert_not_called()

    def test_delete_requires_owned_uid_and_resource_version(self):
        kube = self.client()
        path = "/api/v1/namespaces/cetirun-local-platform-0123456789ab"
        document = {"preconditions": {"uid": "owned-uid", "resourceVersion": "12"}}
        with patch.object(kube, "check_target"), patch.object(kube, "command", return_value="{}") as command:
            for invalid in ({}, {"preconditions": {"uid": "owned-uid"}},
                            {"preconditions": {"uid": "replacement", "resourceVersion": "12"}},
                            {**document, "gracePeriodSeconds": 0}):
                with self.subTest(document=invalid), self.assertRaises(probe.Error):
                    kube.run(["delete", "--raw", path, "-f", "-"], invalid)
            command.assert_not_called()
            kube.run(["delete", "--raw", path, "-f", "-"], document)
            command.assert_called_once()

    def test_protected_or_parent_namespace_exec_is_refused(self):
        kube = self.client()
        with patch.object(kube, "check_target"), patch.object(kube, "command") as command:
            for namespace in ("cetirun-agent", "cetirun-local-probe"):
                with self.subTest(namespace=namespace), self.assertRaises(probe.Error):
                    kube.run(["exec", "--namespace", namespace, "kata-writer", "--", "true"])
            command.assert_not_called()

    def test_replaced_namespace_prevents_exec(self):
        kube = self.client()
        namespace = next(iter(kube.namespaces))
        kube.pods[namespace, "probe"] = ("pod-uid", "kata")
        with patch.object(kube, "get", return_value={
            "metadata": {"uid": "replacement", "labels": {probe.qualify.RUN_LABEL: "run"}},
        }):
            with self.assertRaisesRegex(probe.Error, "ownership changed"):
                kube.check_pod(namespace, "probe")

    def test_create_refuses_agent_namespace_and_non_probe_resources(self):
        kube = self.client()
        with self.assertRaises(probe.Error):
            kube.create({"kind": "Namespace", "metadata": {
                "name": "cetirun-agent", "labels": {probe.qualify.RUN_LABEL: "a" * 32},
            }})
        with self.assertRaises(probe.Error):
            kube.create({"kind": "Agent", "metadata": {"namespace": "cetirun-agent", "name": "agent"}})

    def test_owned_pvc_create_uses_shared_strict_collection(self):
        kube = self.client()
        namespace = next(iter(kube.namespaces))
        document = {
            "apiVersion": "v1", "kind": "PersistentVolumeClaim",
            "metadata": {"name": "home", "namespace": namespace},
            "spec": {"accessModes": ["ReadWriteOncePod"], "storageClassName": "synthetic-csi",
                     "resources": {"requests": {"storage": "1Gi"}}},
        }
        namespace_object = {"metadata": {"uid": "owned-uid", "labels": {probe.qualify.RUN_LABEL: "run"}}}
        response = {**document, "metadata": {**document["metadata"], "uid": "claim-uid"}}
        with patch.object(kube, "get", return_value=namespace_object) as get, \
                patch.object(kube, "check_target") as target, \
                patch.object(kube, "command", return_value=json.dumps(response)) as command:
            self.assertEqual(kube.create(document), response)
        target.assert_called_once()
        get.assert_called_once_with("namespace", namespace)
        command.assert_called_once_with(
            kube.prefix + ["create", "--raw", "/api/v1/namespaces/" + namespace +
                           "/persistentvolumeclaims?fieldValidation=Strict", "-f", "-"],
            document, probe.REQUEST_SECONDS,
        )
        self.assertEqual(kube.uncertain_creates, [])

    def test_owned_pvc_create_rejects_replaced_namespace(self):
        kube = self.client()
        namespace = next(iter(kube.namespaces))
        document = {"apiVersion": "v1", "kind": "PersistentVolumeClaim",
                    "metadata": {"name": "home", "namespace": namespace}}
        with patch.object(kube, "get", return_value={"metadata": {
                "uid": "replacement", "labels": {probe.qualify.RUN_LABEL: "run"}}}), \
                patch.object(kube, "command") as command:
            with self.assertRaisesRegex(probe.Error, "ownership changed"):
                kube.create(document)
        command.assert_not_called()
        self.assertEqual(kube.uncertain_creates, [])

    def test_owned_pvc_create_retains_unknown_creation_receipt(self):
        kube = self.client()
        namespace = next(iter(kube.namespaces))
        document = {"apiVersion": "v1", "kind": "PersistentVolumeClaim",
                    "metadata": {"name": "home", "namespace": namespace}}
        namespace_object = {"metadata": {"uid": "owned-uid", "labels": {probe.qualify.RUN_LABEL: "run"}}}
        with patch.object(kube, "get", return_value=namespace_object), \
                patch.object(kube, "check_target"), \
                patch.object(kube, "command", return_value=json.dumps(document)):
            with self.assertRaisesRegex(probe.Error, "Creation UID unknown"):
                kube.create(document)
        self.assertEqual(kube.uncertain_creates, [{"kind": "PersistentVolumeClaim", "name": "home", "namespace": namespace}])

    def test_command_has_bounded_timeout_and_sanitized_errors(self):
        kube = self.client()
        with patch.object(probe.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "ok", "")) as run:
            self.assertEqual(kube.command(["local-command"], timeout=900), "ok")
            self.assertLessEqual(run.call_args.kwargs["timeout"], 90)
        with patch.object(probe.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "private-endpoint")):
            with self.assertRaises(probe.Error) as failure:
                kube.command(["local-command"])
            self.assertNotIn("private-endpoint", str(failure.exception))
        kube.deadline = time.monotonic() - 1
        with patch.object(probe.subprocess, "run") as run, self.assertRaises(probe.Error):
            kube.command(["local-command"])
        run.assert_not_called()

    def test_failed_stage_still_cleans_and_allows_next_stage(self):
        kube = self.client()
        receipt = {"probes": {}}
        check = {"status": "not-tested", "observed": False}
        namespace = next(iter(kube.namespaces))
        with patch.object(probe.qualify, "cleanup") as cleanup:
            with probe.stage(kube, receipt, "first", [check]):
                kube.stage_namespaces.append(namespace)
                kube.deadline = time.monotonic() - 1
                raise probe.Error("failed first control")
            cleanup.assert_called_once_with(kube, namespace, "owned-uid", "run", timeout=90)
        self.assertFalse(check["observed"])
        self.assertEqual(check["failure"], "failed first control")
        self.assertTrue(receipt["probes"]["first"]["cleanupComplete"])
        with probe.stage(kube, receipt, "independent", []):
            self.assertGreater(kube.deadline, time.monotonic())
        self.assertEqual(receipt["probes"]["independent"]["status"], "completed")

    def test_cleanup_failure_cannot_be_a_capability_pass(self):
        kube = self.client()
        receipt = {"probes": {}}
        check = {"status": "passed", "observed": True}
        with patch.object(probe.qualify, "cleanup", side_effect=probe.Error("replacement retained")):
            with probe.stage(kube, receipt, "owned-cleanup", [check]):
                kube.stage_namespaces.append(next(iter(kube.namespaces)))
        self.assertFalse(check["observed"])
        self.assertFalse(receipt["probes"]["owned-cleanup"]["cleanupComplete"])

    def test_receipt_persists_false_without_weakening_the_profile(self):
        receipt = {"checks": {name: {"observed": False} for name in probe.CAPABILITIES},
                   "qualified": False, "enforcementObservation": False, "restrictiveFailure": False}
        receipt["checks"]["readOnlyRootFilesystem"]["observed"] = True
        with tempfile.TemporaryDirectory() as folder:
            output = Path(folder) / "receipt.json"
            probe.save_receipt(output, copy.deepcopy(receipt))
            saved = json.loads(output.read_text())
            self.assertFalse(saved["baselineRequiredSatisfied"])
            self.assertFalse(saved["capabilities"]["hostResourceLimits"])
            self.assertTrue(saved["capabilities"]["readOnlyRootFilesystem"])
            self.assertFalse(saved["qualified"])
            self.assertEqual(list(Path(folder).iterdir()), [output])

    def test_receipt_path_normalizes_parent_components_before_scope_check(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "repo"
            root.mkdir()
            with patch.object(probe, "ROOT", root):
                self.assertEqual(probe.receipt_path(root / ".." / "receipt.json"),
                                 Path(folder) / "receipt.json")
                with self.assertRaises(probe.Error):
                    probe.receipt_path(root / "receipt.json")
                link = Path(folder) / "link.json"
                link.symlink_to(root / "target.json")
                with self.assertRaises(probe.Error):
                    probe.receipt_path(link)


if __name__ == "__main__":
    unittest.main()
