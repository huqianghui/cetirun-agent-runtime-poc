# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Synthetic lifecycle observations; kubectl is always replaced by a local Python process."""

import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from urllib.parse import urlencode
import uuid


SCRIPT = Path(__file__).resolve().parents[1] / "sandbox-platform-qualify.py"
SPEC = importlib.util.spec_from_file_location("lifecycle_snapshot", SCRIPT)
qualify = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(qualify)
REAL_POPEN = subprocess.Popen
SENTINEL = "sk-dummy-sensitive-synthetic-value.example.com"
NAMESPACE = "snapshot-example"
LABELS = {"app.kubernetes.io/name": "cetirun", "app.kubernetes.io/instance": "selected-runtime"}
IMAGE = "registry.example.com/runtime@sha256:" + "a" * 64


def scope():
    receipt = json.loads((SCRIPT.parent / "tests/fixtures/lifecycle_acceptance.json").read_text())
    resources = {key: {"name": name, "uid": key + "-uid"} for key, name in (
        ("agent", "selected-agent"), ("namespace", NAMESPACE), ("statefulSet", "selected-runtime"),
        ("pod", "selected-runtime-0"), ("homePvc", "selected-home"), ("homePv", "selected-pv"),
        ("attachment", "selected-attachment"),
    )}
    resources["agent"].update(resourceVersion="1", generation=3)
    return {"version": "v1", "run": "selected-run", "case": receipt["case"],
            "artifacts": receipt["artifacts"], "resources": resources}


def owner(key):
    pin = scope()["resources"][key]
    return {"apiVersion": "apps/v1" if key == "statefulSet" else "cetirun.microsoft.com/v1alpha1",
            "kind": "StatefulSet" if key == "statefulSet" else "Agent",
            "name": pin["name"], "uid": pin["uid"], "controller": True}


def paths():
    pins = {"cluster": {"name": "kube-system"}, **scope()["resources"]}
    selected = {key: base.format(namespace=NAMESPACE) + "/" + pins[key]["name"]
                for key, (_, _, base) in qualify.SNAPSHOT_RESOURCES.items()}
    selected["statefulSetList"] = qualify.SNAPSHOT_RESOURCES["statefulSet"][2].format(
        namespace=NAMESPACE) + "?limit=2"
    selected["podList"] = qualify.SNAPSHOT_RESOURCES["pod"][2].format(namespace=NAMESPACE) + "?" + urlencode({
        "limit": 2, "labelSelector": ",".join(name + "=" + LABELS[name] for name in sorted(LABELS)),
    })
    return selected


PATHS = paths()
SUSPENDED_POD_LIST = qualify.SNAPSHOT_RESOURCES["pod"][2].format(namespace=NAMESPACE) + "?limit=2"


def fixture():
    pins = {"cluster": {"name": "kube-system", "uid": "cluster-uid"}, **scope()["resources"]}
    objects = {"config": {
        "current-context": "selected-context",
        "contexts": [{"name": "selected-context", "context": {"cluster": "selected-cluster", "user": "selected-user"}}],
        "users": [{"name": "selected-user", "user": {"token": "REDACTED"}}],
        "clusters": [{"name": "selected-cluster", "cluster": {"server": "https://cluster.example.com"}}],
    }}
    for key, (api, kind, base) in qualify.SNAPSHOT_RESOURCES.items():
        metadata = {**pins[key], "resourceVersion": "1", "annotations": {"synthetic-unused": SENTINEL}}
        if "{namespace}" in base:
            metadata["namespace"] = NAMESPACE
        if key in ("agent", "namespace"):
            metadata["labels"] = {qualify.RUN_LABEL: "selected-run"}
        objects[PATHS[key]] = {"apiVersion": api, "kind": kind, "metadata": metadata}
    objects[PATHS["agent"]].update(
        spec={"agent": {"type": "openclaw"}, "user": {"username": SENTINEL}},
        status={"namespace": NAMESPACE, "phase": "Ready", "observedGeneration": 3, "accessUrl": SENTINEL},
    )
    namespace = objects[PATHS["namespace"]]
    namespace["metadata"]["ownerReferences"] = [owner("agent")]
    namespace["status"] = {"phase": "Active"}
    runtime = {
        "containers": [{"name": "cetirun", "image": IMAGE, "env": [{"name": "UNUSED", "value": SENTINEL}],
                        "volumeMounts": [{"name": "data", "mountPath": "/home/vibe"}]}],
        "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "selected-home"}},
                    {"name": "unused-secret-reference", "secret": {"secretName": SENTINEL}}],
    }
    stateful_set = objects[PATHS["statefulSet"]]
    stateful_set["metadata"].update(ownerReferences=[owner("agent")], generation=2)
    stateful_set.update(
        spec={"replicas": 1, "selector": {"matchLabels": dict(LABELS)},
              "template": {"metadata": {"labels": dict(LABELS)}, "spec": copy.deepcopy(runtime)}},
        status={"observedGeneration": 2, "replicas": 1, "readyReplicas": 1, "currentReplicas": 1,
                "updatedReplicas": 1, "currentRevision": "selected-revision", "updateRevision": "selected-revision"},
    )
    pod = objects[PATHS["pod"]]
    pod["metadata"].update(ownerReferences=[owner("statefulSet")],
                           labels={**LABELS, "controller-revision-hash": "selected-revision"})
    pod.update(
        spec={**copy.deepcopy(runtime), "nodeName": "selected-node"},
        status={"phase": "Running", "conditions": [{"type": "Ready", "status": "True", "message": SENTINEL}],
                "containerStatuses": [{"name": "cetirun", "ready": True, "imageID": IMAGE,
                                       "state": {"running": {"startedAt": "2026-10-23T00:00:00Z"}}}]},
    )
    objects[PATHS["homePvc"]].update(spec={"volumeName": "selected-pv"}, status={"phase": "Bound"})
    objects[PATHS["homePv"]].update(
        spec={"claimRef": {"kind": "PersistentVolumeClaim", "name": "selected-home",
                          "namespace": NAMESPACE, "uid": pins["homePvc"]["uid"]},
              "csi": {"driver": "csi.example.com", "volumeHandle": SENTINEL, "volumeAttributes": {"unused": SENTINEL}}},
        status={"phase": "Bound"},
    )
    objects[PATHS["attachment"]].update(
        spec={"attacher": "csi.example.com", "nodeName": "selected-node",
              "source": {"persistentVolumeName": "selected-pv"}},
        status={"attached": True, "attachmentMetadata": {"unused": SENTINEL}},
    )
    for key in ("statefulSet", "pod"):
        api, kind, _ = qualify.SNAPSHOT_RESOURCES[key]
        objects[PATHS[key + "List"]] = {
            "apiVersion": api, "kind": kind + "List", "metadata": {"resourceVersion": "10"},
            "items": [objects[PATHS[key]]],
        }
    return objects


def suspended_fixture():
    objects = fixture()
    agent = objects[PATHS["agent"]]
    agent["spec"]["suspended"] = True
    agent["status"].update(phase="Suspended", conditions=[
        {"type": kind, "status": "False", "reason": reason, "message": SENTINEL}
        for kind, reason in (("Ready", "InstanceSuspended"), ("PodReady", "Suspended"), ("IngressReady", "Suspended"))
    ])
    workload = objects[PATHS["statefulSet"]]
    workload["spec"]["replicas"] = 0
    workload["status"] = {"observedGeneration": 2, "replicas": 0}
    objects[SUSPENDED_POD_LIST] = objects.pop(PATHS["podList"])
    objects[SUSPENDED_POD_LIST]["items"] = []
    objects.pop(PATHS["pod"])
    return objects


class KubectlDouble:
    def __init__(self, kubeconfig):
        self.kubeconfig = str(kubeconfig)
        self.responses = fixture()
        self.calls, self.processes, self.counts, self.scripts = [], [], {}, {}
        self.change = lambda key, count, value: value

    def spawn(self, command, **options):
        assert command[:7] == [
            "kubectl", "--kubeconfig", self.kubeconfig, "--context", "selected-context",
            "--request-timeout=5s", "--v=0",
        ], command
        assert options["stdin"] == subprocess.DEVNULL
        assert options["stdout"] == subprocess.PIPE and options["stderr"] == subprocess.DEVNULL
        assert options["start_new_session"] is True and options["env"]["KUBECTL_KUBERC"] == "false"
        if command[7:] == qualify.DeploymentKubernetes.CONFIG_COMMAND:
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


class LifecycleSnapshotTests(unittest.TestCase):
    def setUp(self):
        self.directory = Path.cwd() / (".lifecycle-snapshot-test-" + uuid.uuid4().hex)
        self.directory.mkdir()
        self.addCleanup(shutil.rmtree, self.directory)
        self.scope_path = self.directory / "scope.json"
        self.scope_path.write_text(json.dumps(scope()))
        self.kubeconfig = self.directory / "synthetic-config.json"
        self.kubeconfig.write_text(json.dumps(fixture()["config"]))

    def arguments(self, state=None):
        args = ["lifecycle-snapshot", "--kubeconfig", str(self.kubeconfig), "--context", "selected-context",
                "--cluster-uid", "cluster-uid", "--scope", str(self.scope_path)]
        return args if state is None else args + ["--state", state]

    def suspended_double(self, *, pod_pinned=False, attachment_pinned=True):
        selected = scope()
        if not pod_pinned:
            selected["resources"]["pod"] = None
        if not attachment_pinned:
            selected["resources"]["attachment"] = None
        self.scope_path.write_text(json.dumps(selected))
        double = KubectlDouble(self.kubeconfig)
        double.responses = suspended_fixture()
        return double

    def run_suspended(self, double):
        return self.run_snapshot(double, self.arguments("suspended"))

    def assert_suspended_blocked(self, double, reason=None):
        return self.assert_blocked(double, reason, self.arguments("suspended"))

    def run_snapshot(self, double=None, args=None):
        double = double or KubectlDouble(self.kubeconfig)
        with patch.object(qualify.subprocess, "Popen", side_effect=double.spawn), \
             patch.object(qualify.subprocess, "run", side_effect=AssertionError("unbounded runner is forbidden")), \
             contextlib.redirect_stdout(io.StringIO()) as stdout, contextlib.redirect_stderr(io.StringIO()) as stderr:
            code = qualify.main(self.arguments() if args is None else args)
        self.assertEqual(stderr.getvalue(), "")
        text = stdout.getvalue()
        self.assertLess(len(text.encode()), 8192)
        for private in (SENTINEL, NAMESPACE, IMAGE, "selected-context", "cluster-uid", "selected-agent",
                        "selected-node", str(self.directory), "REDACTED", "volumeHandle", "accessUrl",
                        "selected-runtime", "selected-home", "selected-pv", '"selected-attachment"', "csi.example.com"):
            self.assertNotIn(private, text)
        self.assertTrue(all(p.poll() is not None for p in double.processes), "subprocess was not reaped")
        result = json.loads(text)
        self.assertIs(result["qualifiesExecution"], False)
        self.assertEqual(result["liveAcceptance"], "not-established")
        return code, result, double

    def assert_blocked(self, double=None, reason=None, args=None):
        code, result, double = self.run_snapshot(double, args)
        self.assertEqual(code, 1)
        self.assertEqual(result["status"], "blocked")
        self.assertIsNone(result["snapshot"])
        if reason:
            self.assertIn(reason, [item["reason"] for item in result["checks"]])
        return result, double

    def test_positive_observation_is_read_only_pinned_and_not_acceptance(self):
        code, result, double = self.run_snapshot()
        self.assertEqual(code, 0)
        self.assertEqual(result["status"], "observed")
        self.assertTrue(all(item["status"] == "observed" for item in result["checks"]))
        self.assertTrue(all(item["status"] == "unknown" for item in result["unverified"]))
        self.assertEqual(set(double.counts), set(double.responses))
        self.assertTrue(all(count == 2 for count in double.counts.values()))
        self.assertEqual(double.calls[1][-1], PATHS["cluster"])
        self.assertLessEqual(len(double.calls), qualify.DeploymentKubernetes.MAX_COMMANDS)
        snapshot = result["snapshot"]
        self.assertEqual(set(snapshot), set(qualify.LIFECYCLE_SNAPSHOT_FIELDS))
        self.assertEqual(snapshot["artifactSet"],
                         qualify.lifecycle_artifact_fingerprint(scope()["artifacts"], scope()["case"]))
        self.assertTrue(all(qualify.lifecycle_digest(pin) for pin in snapshot["resources"].values()))
        self.assertEqual(snapshot["workload"]["agent"], snapshot["resources"]["agent"])
        self.assertEqual(snapshot["workload"]["pod"], snapshot["workload"]["observedPod"])
        self.assertIsNone(snapshot["workload"]["ready"])
        self.assertIsNone(snapshot["workload"]["observedGeneration"])
        self.assertEqual(result["reported"], {"agentObservedGeneration": 3, "podReady": True})
        for key in ("owner", "home", "accounting", "models", "peer", "extensions"):
            self.assertIsNone(snapshot[key])
        self.assertTrue(result["storage"]["csiAttached"])
        self.assertIsNone(result["storage"]["csiReleased"])
        self.assertNotEqual(qualify.assess_lifecycle_receipt(result)["status"], "matched")
        self.assertEqual(self.run_snapshot()[1]["snapshot"], snapshot)

    def test_unpinned_attachment_is_partial_not_absence_or_release(self):
        selected = scope()
        selected["resources"]["attachment"] = None
        self.scope_path.write_text(json.dumps(selected))
        code, result, double = self.run_snapshot()
        self.assertEqual(code, 1)
        self.assertEqual(result["status"], "blocked")
        self.assertIsNotNone(result["snapshot"])
        check = next(item for item in result["checks"] if item["check"] == "csi-attachment")
        self.assertEqual(check, {"check": "csi-attachment", "status": "unknown", "reason": "attachment-not-pinned"})
        self.assertIsNone(result["storage"]["attachment"])
        self.assertIsNone(result["storage"]["csiAttached"])
        self.assertIsNone(result["storage"]["csiReleased"])
        self.assertNotIn(PATHS["attachment"], double.counts)
        self.assertEqual(result["checks"][-1]["status"], "observed")

    def test_missing_source_artifact_or_resource_pins_precede_any_subprocess(self):
        mutations = [
            lambda s: s.pop("run"), lambda s: s["artifacts"].pop("sourceCommit"),
            lambda s: s["artifacts"].pop("sourceTree"),
            lambda s: s["artifacts"].update(sourceCommit="main"),
            lambda s: s["artifacts"]["images"].pop("runtime"),
            lambda s: s["artifacts"]["charts"].update(runtime="latest"),
            lambda s: s["artifacts"].pop("configuration"),
            lambda s: s["resources"]["agent"].pop("generation"),
            lambda s: s["resources"]["agent"].pop("resourceVersion"),
            lambda s: s["resources"]["agent"].update(generation=True),
            lambda s: s["resources"]["homePv"].pop("uid"),
            lambda s: s["resources"].pop("attachment"),
            lambda s: s["resources"]["namespace"].update(name="kube-system"),
            lambda s: s["resources"]["pod"].update(name="../escape"),
            lambda s: s.update(run="x,y=z"), lambda s: s.update(extra=SENTINEL),
        ]
        for index, mutate in enumerate(mutations):
            with self.subTest(index=index):
                selected = scope()
                mutate(selected)
                self.scope_path.write_text(json.dumps(selected))
                _, double = self.assert_blocked()
                self.assertEqual(double.calls, [])

    def test_bad_json_is_bounded_and_sanitized(self):
        for raw in (SENTINEL, "[]", '{"version":"v1","version":"v1"}', '{"run":NaN}',
                    '{"value":"' + SENTINEL * 4096 + '"}'):
            with self.subTest(size=len(raw)):
                self.scope_path.write_text(raw)
                _, double = self.assert_blocked()
                self.assertEqual(double.calls, [])

    def test_nonregular_inputs_never_start_kubectl_or_block_on_a_fifo(self):
        start = time.monotonic()
        directory = self.directory / "directory"
        directory.mkdir()
        fifo = self.directory / "fifo"
        os.mkfifo(fifo)
        symlink = self.directory / "symlink"
        symlink.symlink_to(self.scope_path)
        for option in ("--scope", "--kubeconfig"):
            for path in (directory, fifo, symlink):
                with self.subTest(option=option, path=path.name):
                    args = self.arguments()
                    args[args.index(option) + 1] = str(path)
                    _, double = self.assert_blocked(reason="input-not-regular-file", args=args)
                    self.assertEqual(double.calls, [])
        self.assertLess(time.monotonic() - start, 2)
        args = self.arguments()
        args[-1] = "\0" + SENTINEL
        self.assert_blocked(reason="invalid-input-path", args=args)

    def test_mutation_and_legacy_mode_flags_are_rejected_without_commands(self):
        for extra in (["--allow-test-resources"], ["--network-protocol", "TCP"], ["--receipt", SENTINEL],
                      ["--probe-image", IMAGE], ["--namespace", SENTINEL]):
            with self.subTest(extra=extra[0]):
                code, result, double = self.run_snapshot(args=self.arguments() + extra)
                self.assertEqual(code, 2)
                self.assertIn("invalid-command-line", result["error"])
                self.assertEqual(double.calls, [])
        for option in ("--kubeconfig", "--context", "--cluster-uid", "--scope"):
            args = self.arguments()
            index = args.index(option)
            del args[index:index + 2]
            self.assertEqual(self.run_snapshot(args=args)[0], 2)

    def test_auth_and_wrong_cluster_fail_before_workload_reads(self):
        for key in ("exec", "auth-provider", "tokenFile", "as", "as-groups", "client-key", "client-certificate"):
            double = KubectlDouble(self.kubeconfig)
            double.responses["config"]["users"][0]["user"][key] = SENTINEL
            self.assert_blocked(double)
            self.assertEqual(set(double.counts), {"config"})
        double = KubectlDouble(self.kubeconfig)
        double.responses[PATHS["cluster"]]["metadata"]["uid"] = "other-cluster"
        self.assert_blocked(double, "resource-uid-mismatch")
        self.assertEqual(set(double.counts), {"config", PATHS["cluster"]})

    def test_replaced_identity_and_stale_agent_are_never_adopted(self):
        for key in qualify.SNAPSHOT_RESOURCES:
            with self.subTest(resource=key):
                double = KubectlDouble(self.kubeconfig)
                double.responses[PATHS[key]]["metadata"]["uid"] = "replacement-uid"
                self.assert_blocked(double, "resource-uid-mismatch")
        for field, value in (("generation", 4), ("generation", True), ("resourceVersion", "2")):
            double = KubectlDouble(self.kubeconfig)
            double.responses[PATHS["agent"]]["metadata"][field] = value
            self.assert_blocked(double, "agent-version-mismatch")

    def test_run_labels_and_controller_owners_must_match(self):
        for key in qualify.SNAPSHOT_RESOURCES:
            if key == "cluster":
                continue
            with self.subTest(resource=key):
                double = KubectlDouble(self.kubeconfig)
                double.responses[PATHS[key]]["metadata"].setdefault("labels", {})[qualify.RUN_LABEL] = "another-run"
                self.assert_blocked(double, "run-label-mismatch")
        for key in ("agent", "namespace"):
            double = KubectlDouble(self.kubeconfig)
            double.responses[PATHS[key]]["metadata"]["labels"].clear()
            self.assert_blocked(double, "run-label-mismatch")
        for key in ("namespace", "statefulSet", "pod"):
            double = KubectlDouble(self.kubeconfig)
            double.responses[PATHS[key]]["metadata"]["ownerReferences"][0]["uid"] = "other-owner"
            self.assert_blocked(double, "resource-owner-mismatch")

    def test_multiple_missing_and_paginated_candidates_fail_without_selection(self):
        for key in ("statefulSetList", "podList"):
            for count in (0, 2):
                double = KubectlDouble(self.kubeconfig)
                original = double.responses[PATHS[key]]["items"][0]
                double.responses[PATHS[key]]["items"] = [copy.deepcopy(original) for _ in range(count)]
                self.assert_blocked(double, "candidate-set-not-singleton")
            for field, value in (("continue", SENTINEL), ("remainingItemCount", 1), ("resourceVersion", "")):
                double = KubectlDouble(self.kubeconfig)
                double.responses[PATHS[key]]["metadata"][field] = value
                self.assert_blocked(double, "candidate-list-incomplete")

    def test_typed_list_items_may_omit_type_metadata_but_not_change_identity(self):
        double = KubectlDouble(self.kubeconfig)
        for key in ("statefulSetList", "podList"):
            item = copy.deepcopy(double.responses[PATHS[key]]["items"][0])
            item.pop("apiVersion")
            item.pop("kind")
            double.responses[PATHS[key]]["items"] = [item]
        self.assertEqual(self.run_snapshot(double)[0], 0)
        double.responses[PATHS["podList"]]["items"][0]["metadata"]["uid"] = "other-pod"
        self.assert_blocked(double, "resource-uid-mismatch")

    def test_pending_terminating_and_unready_resources_are_not_ready_or_cleanup(self):
        for key in qualify.SNAPSHOT_RESOURCES:
            double = KubectlDouble(self.kubeconfig)
            double.responses[PATHS[key]]["metadata"]["deletionTimestamp"] = "2026-10-23T00:00:00Z"
            self.assert_blocked(double, "resource-terminating")
        for key, field, value, reason in (
            ("agent", "phase", "Pending", "agent-not-current-ready"),
            ("agent", "observedGeneration", 2, "agent-not-current-ready"),
            ("namespace", "phase", "Terminating", "namespace-not-active"),
            ("statefulSet", "readyReplicas", 0, "statefulset-not-current-ready"),
            ("statefulSet", "observedGeneration", 1, "statefulset-not-current-ready"),
            ("pod", "phase", "Pending", "pod-not-current-ready"),
            ("homePvc", "phase", "Pending", "home-storage-not-bound"),
            ("homePv", "phase", "Released", "home-storage-not-bound"),
            ("attachment", "attached", False, "csi-attachment-not-observed"),
        ):
            double = KubectlDouble(self.kubeconfig)
            double.responses[PATHS[key]]["status"][field] = value
            self.assert_blocked(double, reason)

    def test_home_and_csi_must_link_exactly_without_reading_any_contents(self):
        mutations = [
            ("homePvc", lambda v: v["spec"].update(volumeName="other-volume"), "home-storage-binding-mismatch"),
            ("homePv", lambda v: v["spec"]["claimRef"].update(uid="other-claim"), "home-storage-binding-mismatch"),
            ("pod", lambda v: v["spec"]["volumes"][0]["persistentVolumeClaim"].update(claimName="other-home"),
             "home-claim-mismatch"),
            ("pod", lambda v: v["spec"]["containers"][0]["volumeMounts"][0].update(subPath="other-home"),
             "home-mount-mismatch"),
            ("attachment", lambda v: v["spec"].update(nodeName="source-node"), "csi-attachment-binding-mismatch"),
            ("attachment", lambda v: v["spec"].update(attacher="other.example.com"), "csi-attachment-binding-mismatch"),
            ("attachment", lambda v: v["spec"]["source"].update(persistentVolumeName="other-pv"),
             "csi-attachment-binding-mismatch"),
            ("attachment", lambda v: v["status"].update(attachError={"message": SENTINEL}),
             "csi-attachment-not-observed"),
        ]
        for key, mutate, reason in mutations:
            double = KubectlDouble(self.kubeconfig)
            mutate(double.responses[PATHS[key]])
            self.assert_blocked(double, reason)

    def test_main_image_revisions_and_readiness_cannot_be_filled_from_agent_status(self):
        for replacement in ("registry.example.com/runtime:latest", IMAGE.replace("a" * 64, "b" * 64)):
            double = KubectlDouble(self.kubeconfig)
            double.responses[PATHS["pod"]]["spec"]["containers"][0]["image"] = replacement
            self.assert_blocked(double, "runtime-image-pin-mismatch")
        for mutate, reason in (
            (lambda p: p["metadata"]["labels"].update({"controller-revision-hash": "old-revision"}),
             "pod-revision-or-selector-mismatch"),
            (lambda p: p["status"]["conditions"][0].update(status="False"), "pod-not-current-ready"),
            (lambda p: p["status"]["containerStatuses"][0].update(ready=False), "main-container-not-running-ready"),
            (lambda p: p["status"]["containerStatuses"][0].update(imageID="unknown"), "runtime-image-id-unverified"),
        ):
            double = KubectlDouble(self.kubeconfig)
            mutate(double.responses[PATHS["pod"]])
            self.assert_blocked(double, reason)

    def test_stable_rechecks_detect_named_candidate_and_configuration_drift(self):
        for selected in PATHS.values():
            with self.subTest(path=selected):
                double = KubectlDouble(self.kubeconfig)

                def change(key, count, value):
                    if key == selected and count > 1:
                        if key in (PATHS["statefulSetList"], PATHS["podList"]):
                            value["items"][0]["metadata"]["resourceVersion"] = "new-version"
                        else:
                            value["metadata"]["resourceVersion"] = "new-version"
                    return value

                double.change = change
                self.assert_blocked(double, "agent-version-mismatch" if selected == PATHS["agent"] else "resource-version-drift")
        double = KubectlDouble(self.kubeconfig)
        double.change = lambda key, count, value: {**value, "changed": SENTINEL} if key == "config" and count > 1 else value
        self.assert_blocked(double, "kubeconfig-drift")
        double = KubectlDouble(self.kubeconfig)

        def replace_file(key, count, value):
            if key == PATHS["attachment"]:
                self.kubeconfig.write_text("changed-synthetic-config")
            return value

        double.change = replace_file
        self.assert_blocked(double, "kubeconfig-file-drift")

    def test_runner_allowlist_forbids_discovery_mutation_and_unpinned_gets(self):
        args = SimpleNamespace(kubeconfig=str(self.kubeconfig), context="selected-context", cluster_uid="cluster-uid")
        kube = qualify.SnapshotKubernetes(args, scope())
        with patch.object(qualify.subprocess, "Popen") as popen:
            for args in (
                ["get", "pods", "-A"], ["get", "--raw", "/api/v1/pods"],
                ["get", "--raw", "/apis/storage.k8s.io/v1/volumeattachments"],
                ["get", "--raw", f"/api/v1/namespaces/{NAMESPACE}/secrets/example"], ["get", "--raw", "/version"],
                ["get", "--raw", PATHS["pod"] + "/log"], ["config", "view", "--raw"],
                ["create", "-f", "-"], ["apply", "-f", "-"], ["patch", "agent"], ["delete", "pod"],
                ["exec", "pod"], ["port-forward", "pod"], ["attach", "pod"],
            ):
                with self.assertRaises(qualify.PreflightBlocked):
                    kube.run(args)
            for key in ("secret", "node", "other-agent", "../pod"):
                with self.assertRaises(qualify.PreflightBlocked):
                    kube.get(key)
            popen.assert_not_called()

    def test_command_failure_malformed_output_and_budgets_stay_unknown_sanitized(self):
        for script in (
            f"import sys;sys.stdout.write('{SENTINEL}');sys.stderr.write('{SENTINEL}');sys.exit(1)",
            "print('[]')", "print('{\"metadata\":[]}')", "import sys;sys.stdout.buffer.write(b'\\xff')",
            f"import sys;sys.stdout.write('x'*{qualify.DeploymentKubernetes.MAX_BYTES + 1})",
        ):
            double = KubectlDouble(self.kubeconfig)
            double.scripts[PATHS["pod"]] = script
            result, _ = self.assert_blocked(double)
            self.assertEqual(next(c for c in result["checks"] if c["check"] == "pod")["status"], "unknown")
        with patch.object(qualify.SnapshotKubernetes, "MAX_COMMANDS", 1):
            self.assert_blocked(reason="command-budget-exhausted")
        double = KubectlDouble(self.kubeconfig)
        double.scripts["config"] = "import time;time.sleep(60)"
        start = time.monotonic()
        with patch.object(qualify.SnapshotKubernetes, "COMMAND_SECONDS", 0.1):
            self.assert_blocked(double, "request-timeout")
        self.assertLess(time.monotonic() - start, 3)

    def test_cancellation_restores_signal_handler_and_never_accepts_partial_data(self):
        before = signal.getsignal(signal.SIGTERM)
        double = KubectlDouble(self.kubeconfig)
        double.change = lambda *args: (_ for _ in ()).throw(KeyboardInterrupt())
        code, result, _ = self.run_snapshot(double)
        self.assertEqual(code, 130)
        self.assertIsNone(result["snapshot"])
        self.assertEqual(signal.getsignal(signal.SIGTERM), before)
        self.assertIn("cancelled", [item["reason"] for item in result["checks"]])

    def test_ready_default_and_explicit_selector_have_the_same_output_and_reads(self):
        default = self.run_snapshot()
        explicit = self.run_snapshot(args=self.arguments("ready"))
        self.assertEqual(default[:2], explicit[:2])
        self.assertEqual(default[2].calls, explicit[2].calls)
        self.assertNotIn("state", default[1])
        double = self.suspended_double(pod_pinned=True)
        self.assert_blocked(double, "agent-not-current-ready")

    def test_snapshot_stability_preserves_json_types_but_ignores_object_key_order(self):
        double = KubectlDouble(self.kubeconfig)

        def reorder(key, count, value):
            return dict(reversed(tuple(value.items()))) if count > 1 else value

        double.change = reorder
        self.assertEqual(self.run_snapshot(double)[0], 0)
        for path in (PATHS["statefulSet"], "config"):
            double = KubectlDouble(self.kubeconfig)
            if path == "config":
                double.responses["config"]["clusters"][0]["cluster"]["insecure-skip-tls-verify"] = False

            def change(key, count, value):
                if key == path and count > 1:
                    if path == "config":
                        value["clusters"][0]["cluster"]["insecure-skip-tls-verify"] = 0
                    else:
                        value["status"]["replicas"] = True
                return value

            double.change = change
            self.assert_blocked(double, "kubeconfig-drift" if path == "config" else "resource-version-drift")

    def test_suspended_positive_retains_pins_without_a_runtime_pod_or_image_id(self):
        for pod_pinned in (False, True):
            with self.subTest(pod_pinned=pod_pinned):
                double = self.suspended_double(pod_pinned=pod_pinned)
                code, result, double = self.run_suspended(double)
                self.assertEqual((code, result["status"], result["state"]), (0, "observed", "suspended"))
                self.assertTrue(all(item["status"] == "observed" for item in result["checks"]))
                self.assertTrue(all(item["status"] == "unknown" for item in result["unverified"]))
                self.assertEqual(set(double.counts), set(double.responses))
                self.assertTrue(all(count == 2 for count in double.counts.values()))
                self.assertNotIn(PATHS["pod"], double.counts)
                self.assertNotIn(PATHS["podList"], double.counts)
                self.assertLessEqual(len(double.calls), qualify.DeploymentKubernetes.MAX_COMMANDS)
                snapshot = result["snapshot"]
                self.assertEqual(set(snapshot), set(qualify.LIFECYCLE_SNAPSHOT_FIELDS))
                self.assertEqual(snapshot["artifactSet"],
                                 qualify.lifecycle_artifact_fingerprint(scope()["artifacts"], scope()["case"]))
                self.assertTrue(all(qualify.lifecycle_digest(pin) for pin in snapshot["resources"].values()))
                self.assertEqual(snapshot["workload"]["agent"], snapshot["resources"]["agent"])
                self.assertEqual(qualify.lifecycle_digest(snapshot["workload"]["pod"]), pod_pinned)
                for key in ("observedPod", "node", "observedGeneration", "ready"):
                    self.assertIsNone(snapshot["workload"][key])
                self.assertEqual(result["reported"], {
                    "agentObservedGeneration": 3, "agentSuspended": True, "agentPhase": "Suspended",
                    "podReady": None, "desiredReplicas": 0, "currentReplicas": 0, "podCandidates": 0,
                    "runtimeImageID": None,
                })
                self.assertTrue(result["storage"]["pvcBound"])
                self.assertTrue(result["storage"]["pvBound"])
                self.assertTrue(result["storage"]["csiAttached"])
                self.assertIsNone(result["storage"]["csiReleased"])
                for key in ("owner", "home", "accounting", "models", "peer", "extensions"):
                    self.assertIsNone(snapshot[key])
                self.assertNotEqual(qualify.assess_lifecycle_receipt(result)["status"], "matched")

    def test_suspended_observation_retains_home_identities_across_current_generations(self):
        ready = self.run_snapshot()[1]
        for runtime in ("openclaw", "hermes"):
            for execution in ("ordinary", "sandbox-native"):
                with self.subTest(runtime=runtime, execution=execution):
                    double = self.suspended_double()
                    selected = json.loads(self.scope_path.read_text())
                    selected["case"].update(runtime=runtime, execution=execution)
                    selected["resources"]["agent"].update(generation=4, resourceVersion="2")
                    self.scope_path.write_text(json.dumps(selected))
                    agent = double.responses[PATHS["agent"]]
                    agent["metadata"].update(generation=4, resourceVersion="2")
                    agent["status"]["observedGeneration"] = 4
                    agent["spec"]["agent"]["type"] = runtime
                    code, result, _ = self.run_suspended(double)
                    self.assertEqual(code, 0)
                    self.assertEqual(result["snapshot"]["resources"], ready["snapshot"]["resources"])
                    self.assertEqual(result["snapshot"]["workload"]["generation"], 4)
                    self.assertEqual(result["snapshot"]["artifactSet"],
                                     qualify.lifecycle_artifact_fingerprint(selected["artifacts"], selected["case"]))

    def test_null_pod_requires_explicit_suspended_selection_and_other_pins_remain_required(self):
        self.suspended_double()
        for state in (None, "ready"):
            _, double = self.assert_blocked(args=self.arguments(state))
            self.assertEqual(double.calls, [])
        for key in scope()["resources"]:
            with self.subTest(resource=key):
                double = self.suspended_double()
                selected = json.loads(self.scope_path.read_text())
                selected["resources"].pop(key)
                self.scope_path.write_text(json.dumps(selected))
                self.assert_suspended_blocked(double)
                self.assertEqual(double.calls, [])
        for mutate in (
            lambda s: s["resources"]["agent"].pop("resourceVersion"),
            lambda s: s["resources"]["agent"].pop("generation"),
            lambda s: s["resources"]["homePv"].pop("uid"),
            lambda s: s["resources"]["homePv"].update(uid=s["resources"]["homePvc"]["uid"]),
            lambda s: s["artifacts"]["images"].pop("runtime"),
            lambda s: s["artifacts"].pop("configuration"),
        ):
            double = self.suspended_double()
            selected = json.loads(self.scope_path.read_text())
            mutate(selected)
            self.scope_path.write_text(json.dumps(selected))
            self.assert_suspended_blocked(double)
            self.assertEqual(double.calls, [])
        for state in ("deleted", "migrating", "pending", "Suspended", SENTINEL):
            code, result, double = self.run_snapshot(args=self.arguments(state))
            self.assertEqual(code, 2)
            self.assertIn("invalid-command-line", result["error"])
            self.assertEqual(double.calls, [])

    def test_suspended_intent_requires_current_completed_controller_status(self):
        for field, values in (
            ("suspended", (None, False, "true", 1)),
            ("phase", (None, "Ready", "Pending", "Provisioning", "Deleting", "Stopped")),
            ("observedGeneration", (None, 2, 4, True, "3")),
        ):
            for value in values:
                with self.subTest(field=field, value=value):
                    double = self.suspended_double()
                    agent = double.responses[PATHS["agent"]]
                    container = agent["spec" if field == "suspended" else "status"]
                    if value is None:
                        container.pop(field)
                    else:
                        container[field] = value
                    self.assert_suspended_blocked(double, "agent-not-current-suspended")
                    self.assertNotIn(PATHS["statefulSet"], double.counts)
        for index in range(3):
            for operation in ("missing", "duplicate", "true", "unknown", "unaccepted"):
                with self.subTest(condition=index, operation=operation):
                    double = self.suspended_double()
                    conditions = double.responses[PATHS["agent"]]["status"]["conditions"]
                    if operation == "missing":
                        conditions.pop(index)
                    elif operation == "duplicate":
                        conditions.append(copy.deepcopy(conditions[index]))
                    elif operation == "unaccepted":
                        conditions[index]["reason"] = "Suspended" if index == 0 else "SuspendIngressRemovalFailed"
                    else:
                        conditions[index]["status"] = "True" if operation == "true" else "Unknown"
                    self.assert_suspended_blocked(double, "agent-suspension-not-observed")

    def test_suspended_workload_requires_explicit_current_zero_counts(self):
        for container, field in (
            ("spec", "replicas"), ("status", "replicas"), ("status", "currentReplicas"),
            ("status", "readyReplicas"), ("status", "updatedReplicas"), ("status", "availableReplicas"),
            ("status", "terminatingReplicas"),
        ):
            for value in (1, -1, None, False, "0", 0.0):
                with self.subTest(container=container, field=field, value=value):
                    double = self.suspended_double()
                    double.responses[PATHS["statefulSet"]][container][field] = value
                    self.assert_suspended_blocked(double, "statefulset-not-current-zero")
        for container, field in (("spec", "replicas"), ("status", "replicas"), ("status", "observedGeneration")):
            double = self.suspended_double()
            double.responses[PATHS["statefulSet"]][container].pop(field)
            self.assert_suspended_blocked(double, "statefulset-not-current-zero")
        for generation in (0, 1, 3, True, "2"):
            double = self.suspended_double()
            double.responses[PATHS["statefulSet"]]["status"]["observedGeneration"] = generation
            self.assert_suspended_blocked(double, "statefulset-not-current-zero")
        double = self.suspended_double()
        double.responses[PATHS["statefulSet"]]["status"].update(
            currentReplicas=0, readyReplicas=0, updatedReplicas=0, availableReplicas=0, terminatingReplicas=0,
            currentRevision="old-revision", updateRevision="new-revision",
        )
        self.assertEqual(self.run_suspended(double)[0], 0)

    def test_suspended_missing_workload_and_ambiguous_workload_list_are_blocked(self):
        double = self.suspended_double()
        double.scripts[PATHS["statefulSet"]] = "import sys;sys.exit(1)"
        self.assert_suspended_blocked(double, "kubernetes-request-failed")
        for count in (0, 2):
            double = self.suspended_double()
            workload = double.responses[PATHS["statefulSet"]]
            double.responses[PATHS["statefulSetList"]]["items"] = [copy.deepcopy(workload) for _ in range(count)]
            self.assert_suspended_blocked(double, "candidate-set-not-singleton")
        double = self.suspended_double()
        workload = double.responses[PATHS["statefulSet"]]
        workload["spec"]["selector"]["matchLabels"] = {"other": "selector"}
        self.assert_suspended_blocked(double, "unsupported-runtime-selector")

    def test_suspended_pod_absence_requires_a_complete_namespace_wide_zero_candidate_list(self):
        for phase, labels, terminating, count in (
            ("Running", LABELS, False, 1), ("Pending", {}, False, 1),
            ("Succeeded", {"other": "selector"}, False, 1), ("Running", {}, True, 1),
            ("Running", LABELS, False, 2), ("Running", LABELS, True, 3),
        ):
            with self.subTest(phase=phase, labels=labels, terminating=terminating, count=count):
                double = self.suspended_double(pod_pinned=True)
                pod = fixture()[PATHS["pod"]]
                pod["metadata"]["labels"] = labels
                pod["metadata"]["uid"] = "replacement-pod"
                pod["metadata"]["ownerReferences"][0]["uid"] = "other-workload"
                pod["status"]["phase"] = phase
                if terminating:
                    pod["metadata"]["deletionTimestamp"] = "2026-10-25T00:00:00Z"
                double.responses[SUSPENDED_POD_LIST]["items"] = [copy.deepcopy(pod) for _ in range(count)]
                self.assert_suspended_blocked(double, "pod-candidates-not-empty")
                self.assertNotIn(PATHS["pod"], double.counts)
        for path in (SUSPENDED_POD_LIST, PATHS["statefulSetList"]):
            for field, value in (
                ("continue", SENTINEL), ("continue", None), ("remainingItemCount", 1),
                ("remainingItemCount", -1), ("remainingItemCount", True), ("remainingItemCount", "0"),
                ("resourceVersion", ""),
            ):
                double = self.suspended_double()
                double.responses[path]["metadata"][field] = value
                self.assert_suspended_blocked(double, "candidate-list-incomplete")
        for replacement in ({}, {"kind": "PodList"}, {"items": None},
                            {"apiVersion": "v1", "kind": "PodList", "metadata": {"resourceVersion": "10"}}):
            double = self.suspended_double()
            double.responses[SUSPENDED_POD_LIST] = replacement
            self.assert_suspended_blocked(double)

    def test_suspended_pinned_identity_versions_run_and_owners_are_never_adopted(self):
        for key in qualify.SNAPSHOT_RESOURCES:
            if key == "pod":
                continue
            for field, value, reason in (
                ("uid", "replacement-uid", "resource-uid-mismatch"),
                ("deletionTimestamp", "2026-10-25T00:00:00Z", "resource-terminating"),
            ):
                with self.subTest(resource=key, field=field):
                    double = self.suspended_double()
                    double.responses[PATHS[key]]["metadata"][field] = value
                    self.assert_suspended_blocked(double, reason)
            if key != "cluster":
                double = self.suspended_double()
                double.responses[PATHS[key]]["metadata"].setdefault("labels", {})[qualify.RUN_LABEL] = "other-run"
                self.assert_suspended_blocked(double, "run-label-mismatch")
        for field, value in (("generation", 4), ("generation", True), ("resourceVersion", "2")):
            double = self.suspended_double()
            double.responses[PATHS["agent"]]["metadata"][field] = value
            self.assert_suspended_blocked(double, "agent-version-mismatch")
        for key in ("namespace", "statefulSet"):
            double = self.suspended_double()
            double.responses[PATHS[key]]["metadata"]["ownerReferences"][0]["uid"] = "other-owner"
            self.assert_suspended_blocked(double, "resource-owner-mismatch")

    def test_suspended_template_image_and_retained_home_binding_must_match(self):
        for image in ("registry.example.com/runtime:latest", IMAGE.replace("a" * 64, "b" * 64)):
            double = self.suspended_double()
            double.responses[PATHS["statefulSet"]]["spec"]["template"]["spec"]["containers"][0]["image"] = image
            self.assert_suspended_blocked(double, "runtime-image-pin-mismatch")
        for mutate, reason in (
            (lambda s: s["containers"][0]["volumeMounts"][0].update(readOnly=True), "home-mount-mismatch"),
            (lambda s: s["containers"][0]["volumeMounts"][0].update(subPath="other-home"), "home-mount-mismatch"),
            (lambda s: s["volumes"][0]["persistentVolumeClaim"].update(claimName="other-home"), "home-claim-mismatch"),
        ):
            double = self.suspended_double()
            mutate(double.responses[PATHS["statefulSet"]]["spec"]["template"]["spec"])
            self.assert_suspended_blocked(double, reason)
        for key, mutate, reason in (
            ("homePvc", lambda v: v["spec"].update(volumeName="other-volume"), "home-storage-binding-mismatch"),
            ("homePvc", lambda v: v["status"].update(phase="Pending"), "home-storage-not-bound"),
            ("homePv", lambda v: v["status"].update(phase="Released"), "home-storage-not-bound"),
            ("homePv", lambda v: v["spec"].update(volumeMode="Block"), "home-storage-binding-mismatch"),
            ("homePv", lambda v: v["spec"]["claimRef"].update(uid="other-claim"), "home-storage-binding-mismatch"),
            ("homePv", lambda v: v["spec"]["claimRef"].update(name="other-home"), "home-storage-binding-mismatch"),
            ("homePv", lambda v: v["spec"]["claimRef"].update(namespace="other-namespace"), "home-storage-binding-mismatch"),
        ):
            double = self.suspended_double()
            mutate(double.responses[PATHS[key]])
            self.assert_suspended_blocked(double, reason)

    def test_suspended_unpinned_missing_and_uncertain_attachment_preserve_only_stable_core(self):
        for case in ("unpinned", "missing", "forbidden", "malformed", "no-status", "false", "missing-flag",
                     "unknown-flag", "attach-error", "detach-error"):
            with self.subTest(case=case):
                double = self.suspended_double(attachment_pinned=case != "unpinned")
                attachment = double.responses[PATHS["attachment"]]
                if case in ("missing", "forbidden"):
                    double.scripts[PATHS["attachment"]] = f"import sys;sys.stderr.write('{SENTINEL}');sys.exit(1)"
                elif case == "malformed":
                    double.responses[PATHS["attachment"]] = SENTINEL
                elif case == "no-status":
                    attachment.pop("status")
                elif case == "false":
                    attachment["status"]["attached"] = False
                elif case == "missing-flag":
                    attachment["status"].pop("attached")
                elif case == "unknown-flag":
                    attachment["status"]["attached"] = "unknown"
                elif case in ("attach-error", "detach-error"):
                    attachment["status"]["attachError" if case == "attach-error" else "detachError"] = {"message": SENTINEL}
                code, result, double = self.run_suspended(double)
                self.assertEqual((code, result["status"]), (1, "blocked"))
                self.assertIsNotNone(result["snapshot"])
                self.assertEqual(result["storage"],
                                 {"pvcBound": True, "pvBound": True, "attachment": None,
                                  "csiAttached": None, "csiReleased": None})
                check = next(c for c in result["checks"] if c["check"] == "csi-attachment")
                self.assertEqual(check["status"], "unknown")
                self.assertNotEqual(check["reason"], "not-checked")
                self.assertEqual(result["checks"][-1]["status"], "observed")
                for path in (PATHS["agent"], PATHS["homePvc"], PATHS["homePv"], SUSPENDED_POD_LIST):
                    self.assertEqual(double.counts[path], 2)
                if case == "unpinned":
                    self.assertNotIn(PATHS["attachment"], double.counts)
                elif case in ("missing", "forbidden", "malformed"):
                    self.assertEqual(double.counts[PATHS["attachment"]], 1)

    def test_suspended_attachment_disappearing_on_recheck_remains_partial_not_release(self):
        double = self.suspended_double()

        def disappear(key, count, value):
            if key == PATHS["attachment"] and count == 2:
                double.scripts[key] = "import sys;sys.exit(1)"
            return value

        double.change = disappear
        code, result, double = self.run_suspended(double)
        self.assertEqual((code, result["status"]), (1, "blocked"))
        self.assertIsNotNone(result["snapshot"])
        self.assertIsNone(result["storage"]["csiAttached"])
        self.assertIsNone(result["storage"]["csiReleased"])
        self.assertIn({"check": "csi-attachment", "status": "unknown", "reason": "kubernetes-request-failed"},
                      result["checks"])
        self.assertEqual(double.counts[SUSPENDED_POD_LIST], 2)

    def test_suspended_unknown_attachment_does_not_hide_later_core_drift(self):
        double = self.suspended_double()

        def change(key, count, value):
            if count == 2:
                if key == PATHS["attachment"]:
                    double.scripts[key] = "import sys;sys.exit(1)"
                elif key == SUSPENDED_POD_LIST:
                    value["items"].append(fixture()[PATHS["pod"]])
            return value

        double.change = change
        result, _ = self.assert_suspended_blocked(double, "pod-candidates-not-empty")
        self.assertIn({"check": "csi-attachment", "status": "unknown", "reason": "kubernetes-request-failed"},
                      result["checks"])
        double = self.suspended_double(attachment_pinned=False)
        double.change = lambda key, count, value: {**value, "status": {"phase": "Released"}} \
            if key == PATHS["homePv"] and count == 2 else value
        self.assert_suspended_blocked(double, "resource-version-drift")

    def test_suspended_attachment_binding_errors_and_drift_are_not_partial_success(self):
        for mutate in (
            lambda v: v["spec"].update(attacher="other.example.com"),
            lambda v: v["spec"].update(nodeName=""),
            lambda v: v["spec"]["source"].update(persistentVolumeName="other-volume"),
        ):
            double = self.suspended_double()
            mutate(double.responses[PATHS["attachment"]])
            self.assert_suspended_blocked(double, "csi-attachment-binding-mismatch")
            double.responses[PATHS["attachment"]].pop("status")
            self.assert_suspended_blocked(double, "csi-attachment-binding-mismatch")
        for field, value, reason in (
            ("uid", "replacement-uid", "resource-uid-mismatch"),
            ("deletionTimestamp", "2026-10-25T00:00:00Z", "resource-terminating"),
            ("labels", {qualify.RUN_LABEL: "other-run"}, "run-label-mismatch"),
        ):
            double = self.suspended_double()
            metadata = double.responses[PATHS["attachment"]]["metadata"]
            metadata[field] = value
            metadata.pop("resourceVersion")
            self.assert_suspended_blocked(double, reason)
        for mutate in (
            lambda v: v["metadata"].update(uid="replacement-uid"),
            lambda v: v["metadata"].update(resourceVersion="2"),
            lambda v: v["status"].update(attached=False),
            lambda v: v["spec"].update(nodeName="other-node"),
        ):
            double = self.suspended_double()

            def change(key, count, value):
                if key == PATHS["attachment"] and count == 2:
                    mutate(value)
                return value

            double.change = change
            self.assert_suspended_blocked(double)

    def test_suspended_stable_rechecks_reject_resource_status_and_configuration_drift(self):
        mutations = [
            (PATHS[key], lambda v: v["metadata"].update(resourceVersion="new-version"))
            for key in qualify.SNAPSHOT_RESOURCES if key != "pod"
        ] + [
            (PATHS["agent"], lambda v: v["spec"].update(suspended=False)),
            (PATHS["agent"], lambda v: v["status"]["conditions"][0].update(reason="SuspendIngressRemovalFailed")),
            (PATHS["statefulSet"], lambda v: v["spec"].update(replicas=1)),
            (PATHS["statefulSet"], lambda v: v["status"].update(replicas=1)),
            (PATHS["statefulSet"], lambda v: v["status"].update(replicas=False)),
            (PATHS["homePv"], lambda v: v["spec"]["claimRef"].update(uid="other-claim")),
            (PATHS["statefulSetList"], lambda v: v["items"][0]["metadata"].update(uid="replacement-uid")),
            (SUSPENDED_POD_LIST, lambda v: v["items"].append(fixture()[PATHS["pod"]])),
            (SUSPENDED_POD_LIST, lambda v: v["metadata"].update({"continue": SENTINEL})),
            ("config", lambda v: v.update(changed=SENTINEL)),
        ]
        for path, mutate in mutations:
            with self.subTest(path=path):
                double = self.suspended_double()

                def change(key, count, value):
                    if key == path and count == 2:
                        mutate(value)
                    return value

                double.change = change
                self.assert_suspended_blocked(double)
        double = self.suspended_double()

        def replace_config(key, count, value):
            if key == PATHS["homePv"]:
                self.kubeconfig.write_text("changed-synthetic-config")
            return value

        double.change = replace_config
        self.assert_suspended_blocked(double, "kubeconfig-file-drift")

    def test_suspended_runner_has_no_discovery_or_write_escape(self):
        self.suspended_double()
        args = SimpleNamespace(kubeconfig=str(self.kubeconfig), context="selected-context",
                               cluster_uid="cluster-uid", state="suspended")
        kube = qualify.SnapshotKubernetes(args, json.loads(self.scope_path.read_text()))
        with patch.object(qualify.subprocess, "Popen") as popen:
            for command in (
                ["get", "--raw", PATHS["pod"]], ["get", "--raw", PATHS["podList"]],
                ["get", "--raw", SUSPENDED_POD_LIST.replace("?limit=2", "")],
                ["get", "--raw", SUSPENDED_POD_LIST.replace("?limit=2", "?limit=3")],
                ["get", "--raw", "/api/v1/pods"], ["get", "--raw", "/api/v1/nodes/example"],
                ["get", "--raw", f"/api/v1/namespaces/{NAMESPACE}/secrets/example"],
                ["get", "--raw", "/apis/storage.k8s.io/v1/volumeattachments"],
                ["get", "--raw", PATHS["homePvc"] + "/proxy"], ["watch", "pod"],
                ["exec", "pod"], ["attach", "pod"], ["port-forward", "pod"], ["proxy"],
                ["apply", "-f", "-"], ["patch", "agent"], ["delete", "pod"], ["create", "pod"],
                ["config", "view", "--raw"],
            ):
                with self.subTest(command=command):
                    with self.assertRaises(qualify.PreflightBlocked):
                        kube.run(command)
            with self.assertRaises(qualify.PreflightBlocked):
                kube.unique("pod", LABELS)
            popen.assert_not_called()
        for key in ("exec", "auth-provider", "tokenFile", "client-key", "client-certificate"):
            double = self.suspended_double()
            double.responses["config"]["users"][0]["user"][key] = SENTINEL
            self.assert_suspended_blocked(double)
            self.assertEqual(set(double.counts), {"config"})

    def test_suspended_nonregular_and_malformed_local_inputs_never_start_commands(self):
        self.suspended_double()
        fifo = self.directory / "suspended-fifo"
        os.mkfifo(fifo)
        symlink = self.directory / "suspended-symlink"
        symlink.symlink_to(self.scope_path)
        for option in ("--scope", "--kubeconfig"):
            for path in (fifo, symlink, self.directory):
                args = self.arguments("suspended")
                args[args.index(option) + 1] = str(path)
                _, double = self.assert_blocked(args=args, reason="input-not-regular-file")
                self.assertEqual(double.calls, [])
        for raw in ("[]", '{"version":"v1","version":"v1"}', '{"run":NaN}', "x" * (64 * 1024 + 1)):
            self.scope_path.write_text(raw)
            _, double = self.assert_blocked(args=self.arguments("suspended"))
            self.assertEqual(double.calls, [])

    def test_suspended_command_failures_and_budgets_cannot_be_empty_pod_evidence(self):
        for script in (
            f"import sys;sys.stderr.write('{SENTINEL}');sys.exit(1)", "print('')", "print('[]')",
            "print('{\"apiVersion\":\"v1\",\"apiVersion\":\"v1\"}')",
            "import sys;sys.stdout.buffer.write(b'\\xff')",
            f"import sys;sys.stdout.write('x'*{qualify.DeploymentKubernetes.MAX_BYTES + 1})",
        ):
            double = self.suspended_double()
            double.scripts[SUSPENDED_POD_LIST] = script
            result, _ = self.assert_suspended_blocked(double)
            self.assertEqual(next(c for c in result["checks"] if c["check"] == "pod")["status"], "unknown")
        for setting, value, reason in (("MAX_COMMANDS", 1, "command-budget-exhausted"),
                                       ("TOTAL_SECONDS", 0, "deadline-exhausted")):
            double = self.suspended_double()
            with patch.object(qualify.SnapshotKubernetes, setting, value):
                self.assert_suspended_blocked(double, reason)
        double = self.suspended_double()
        double.scripts[SUSPENDED_POD_LIST] = "import time;time.sleep(60)"
        start = time.monotonic()
        with patch.object(qualify.SnapshotKubernetes, "COMMAND_SECONDS", 0.2):
            self.assert_suspended_blocked(double, "request-timeout")
        self.assertLess(time.monotonic() - start, 3)

    def test_suspended_attachment_timeout_is_partial_but_budget_and_cleanup_failures_are_fatal(self):
        double = self.suspended_double()
        double.scripts[PATHS["attachment"]] = "import time;time.sleep(60)"
        start = time.monotonic()
        with patch.object(qualify.SnapshotKubernetes, "COMMAND_SECONDS", 0.5):
            code, result, _ = self.run_suspended(double)
        self.assertEqual(code, 1)
        self.assertIsNotNone(result["snapshot"])
        self.assertIsNone(result["storage"]["csiReleased"])
        self.assertIn({"check": "csi-attachment", "status": "unknown", "reason": "request-timeout"}, result["checks"])
        self.assertLess(time.monotonic() - start, 5)
        for reason in ("command-budget-exhausted", "deadline-exhausted", "unrecognized-error"):
            double = self.suspended_double()
            with patch.object(qualify, "snapshot_attachment", side_effect=qualify.PreflightBlocked(reason, "unknown")):
                self.assert_suspended_blocked(double, reason)
        double = self.suspended_double()
        real_killpg = os.killpg

        def cleanup(pid, signum):
            if double.calls[-1][-1] == PATHS["attachment"]:
                raise OSError(SENTINEL)
            return real_killpg(pid, signum)

        with patch.object(qualify.os, "killpg", side_effect=cleanup):
            self.assert_suspended_blocked(double, "command-cleanup-unconfirmed")

    def test_suspended_cancellation_reaps_the_process_and_never_reports_a_snapshot(self):
        before = signal.getsignal(signal.SIGTERM)
        double = self.suspended_double()
        double.scripts[SUSPENDED_POD_LIST] = (
            "import os,signal,time;time.sleep(0.05);os.kill(os.getppid(),signal.SIGTERM);time.sleep(60)"
        )
        start = time.monotonic()
        code, result, _ = self.run_suspended(double)
        self.assertEqual(code, 130)
        self.assertIsNone(result["snapshot"])
        self.assertIn({"check": "pod", "status": "unknown", "reason": "cancelled"}, result["checks"])
        self.assertEqual(signal.getsignal(signal.SIGTERM), before)
        self.assertLess(time.monotonic() - start, 3)


if __name__ == "__main__":
    unittest.main()
