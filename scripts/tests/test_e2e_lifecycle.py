# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Exercise the Bash lifecycle with synthetic command doubles, never a cluster."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/e2e-test.sh"
SENTINEL = "synthetic-upstream-value-must-not-be-printed"
FAKE_TOOL = r'''
import copy,json,os,pathlib,sys
tool=pathlib.Path(sys.argv[0]).name
args=sys.argv[1:]
state_path=pathlib.Path(os.environ["FAKE_STATE"])
state=json.loads(state_path.read_text())
mode=os.environ.get("FAKE_MODE","normal")
prefix=["--kubeconfig",os.environ["E2E_KUBECONFIG"]]
prefix += ["--context",os.environ["E2E_CONTEXT"],"--request-timeout=15s"] if tool=="kubectl" else ["--kube-context",os.environ["E2E_CONTEXT"]]
assert args[:len(prefix)]==prefix,(tool,args)
args=args[len(prefix):]
body=json.load(sys.stdin) if args[:1]==["create"] or (args[:1]==["delete"] and "-f" in args) else None
record={"tool":tool,"args":args,"body":body}
if "-p" in args: record["patch"]=json.loads(args[args.index("-p")+1])
with open(os.environ["FAKE_LOG"],"a") as log: log.write(json.dumps(record)+"\n")
def save(): state_path.write_text(json.dumps(state))
def fail():
    save()
    print("synthetic-upstream-value-must-not-be-printed",file=sys.stderr)
    sys.exit(1)
def emit(value):
    save()
    if value is not None: print(json.dumps(value))
    sys.exit(0)
if tool=="helm": emit({})
if args[:2]==["config","view"]:
    config={"current-context":os.environ["E2E_CONTEXT"],
      "contexts":[{"name":os.environ["E2E_CONTEXT"],"context":{"cluster":"synthetic","user":"synthetic"}}],
      "clusters":[{"name":"synthetic","cluster":{"server":"https://cluster.example.com"}}],
      "users":[{"name":"synthetic","user":{"token":"REDACTED"}}]}
    if mode=="exec-plugin": config["users"][0]["user"]["exec"]={"command":"never-run"}
    if mode=="insecure": config["clusters"][0]["cluster"]["insecure-skip-tls-verify"]=True
    emit(config)
if args[:3]==["get","namespace","kube-system"]:
    if mode=="cluster-read-error": fail()
    emit({"metadata":{"uid":"wrong" if mode=="wrong-cluster" else os.environ["E2E_CLUSTER_UID"]}})
if args[:2]==["get","agent"]:
    if mode=="agent-read-error" or mode=="initial-read-error": fail()
    value=copy.deepcopy(state.get("agent"))
    if value and mode=="replaced-agent":
        value["metadata"]["uid"]="replacement-agent"
    if value and mode=="missing-run-label":
        value["metadata"]["labels"]={}
    if value and "-o" in args and args[args.index("-o")+1].startswith("jsonpath"):
        print("synthetic status")
        sys.exit(0)
    emit(value)
if args[:2]==["get","namespace"]:
    if mode=="namespace-read-error" and state.get("created"): fail()
    value=copy.deepcopy(state.get("namespace"))
    if value and mode=="foreign-namespace":
        value["metadata"]["ownerReferences"][0]["uid"]="foreign-agent"
    emit(value)
if args[:1]==["create"]:
    assert body["kind"]=="Agent" and body["metadata"]["name"]==os.environ["TEST_ALIAS"]
    assert state.get("agent") is None
    body["metadata"].update(uid="created-agent",resourceVersion="1")
    body["status"]={"phase":"Failed" if mode=="main-failure" else "Ready"}
    state["agent"]=copy.deepcopy(body)
    state["namespace"]={"metadata":{"name":"cetirun-"+body["metadata"]["name"],"uid":"created-namespace",
      "resourceVersion":"1","ownerReferences":[{"apiVersion":body["apiVersion"],"kind":"Agent",
      "name":body["metadata"]["name"],"uid":"created-agent"}]}}
    state["created"]=True
    if mode=="lost-create": fail()
    if mode=="missing-create-uid": del body["metadata"]["uid"]
    if mode=="wrong-create-label": body["metadata"]["labels"]={}
    emit(body)
if args[:2]==["patch","agent"]:
    assert args[2]==os.environ["TEST_ALIAS"] and "--type=json" in args
    patch=record["patch"]
    if mode=="patch-conflict":
        state["agent"]["metadata"]["resourceVersion"]="changed"
    metadata=state["agent"]["metadata"]
    if patch[:2]!=[{"op":"test","path":"/metadata/uid","value":metadata["uid"]},
                   {"op":"test","path":"/metadata/resourceVersion","value":metadata["resourceVersion"]}]:
        fail()
    assert len(patch)==3 and patch[2]["op"]=="add" and patch[2]["path"]=="/spec/suspended"
    state["agent"]["spec"]["suspended"]=patch[2]["value"]
    metadata["resourceVersion"]=str(int(metadata["resourceVersion"])+1)
    emit(state["agent"])
if args[:2]==["delete","--raw"]:
    assert args[2]=="/apis/cetirun.microsoft.com/v1alpha1/agents/"+os.environ["TEST_ALIAS"]
    assert body["preconditions"]=={"uid":state["agent"]["metadata"]["uid"],
                                 "resourceVersion":state["agent"]["metadata"]["resourceVersion"]}
    assert body["propagationPolicy"]=="Background"
    if mode=="delete-conflict": fail()
    if mode=="cleanup-timeout":
        state["agent"]["metadata"]["deletionTimestamp"]="2026-01-01T00:00:00Z"
        emit({})
    state["agent"]=None
    if mode=="namespace-replacement":
        state["namespace"]["metadata"]["uid"]="replacement-namespace"
    else:
        state["namespace"]=None
    if mode=="lost-delete": fail()
    emit({})
if args[:2]==["get","crd"]: emit({})
if args[:2]==["get","pods"]:
    print("pod/synthetic")
    sys.exit(0)
raise AssertionError((tool,args))
'''


@unittest.skipUnless(shutil.which("bash") and shutil.which("jq"), "Bash and jq are required")
class E2ELifecycleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="cetirun-e2e-offline-")
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.state = self.directory / "state.json"
        self.state.write_text(json.dumps({"agent": None, "namespace": None}))
        self.log = self.directory / "commands.jsonl"
        config = self.directory / "synthetic-config"
        config.write_text("synthetic fixture, not a kubeconfig")
        for name in ("kubectl", "helm"):
            executable = self.directory / name
            executable.write_text(f"#!{sys.executable}\n{FAKE_TOOL}")
            executable.chmod(0o700)
        self.env = {
            **os.environ,
            "PATH": str(self.directory) + os.pathsep + os.environ["PATH"],
            "E2E_KUBECONFIG": str(config),
            "E2E_CONTEXT": "explicit-fixture",
            "E2E_CLUSTER_UID": "cluster-uid",
            "KUBECONFIG": "/must-not-select-ambient-context",
            "TEST_ALIAS": "e2e-fixture",
            "TEST_NAMESPACE": "operator-fixture",
            "PORTAL_NAMESPACE": "portal-fixture",
            "AGENT_TYPE": "openclaw",
            "READY_TIMEOUT": "1",
            "CLEANUP_TIMEOUT": "1",
            "FAKE_LOG": str(self.log),
            "FAKE_STATE": str(self.state),
        }

    def run_functions(self, command, *, mode="normal", env=None):
        result = subprocess.run(
            ["bash", "-c", 'source "$1"; validate_inputs; ' + command, "fixture", str(SCRIPT)],
            env={**self.env, "FAKE_MODE": mode, **(env or {})},
            stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=15,
        )
        self.assertNotIn(SENTINEL, result.stdout + result.stderr)
        return result

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def mutations(self):
        return [entry for entry in self.calls() if entry["args"][0] in ("create", "patch", "delete", "apply")]

    def test_explicit_target_required_before_any_client(self):
        for key, value in (("E2E_KUBECONFIG", ""), ("E2E_CONTEXT", ""), ("E2E_CLUSTER_UID", ""),
                           ("E2E_KUBECONFIG", "relative"), ("TEST_ALIAS", "invalid/name"),
                           ("READY_TIMEOUT", "0"), ("CLEANUP_TIMEOUT", "10000")):
            with self.subTest(key=key, value=value):
                result = self.run_functions("check_target", env={key: value})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.calls(), [])

    def test_wrappers_pin_kubectl_and_helm_without_ambient_context(self):
        result = self.run_functions("check_client_config; check_target; helm version")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([item["tool"] for item in self.calls()], ["kubectl", "kubectl", "helm"])

    def test_plugins_or_insecure_config_are_rejected_locally(self):
        for mode in ("exec-plugin", "insecure"):
            with self.subTest(mode=mode):
                self.log.unlink(missing_ok=True)
                result = self.run_functions("check_client_config; check_target", mode=mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual([item["args"][:2] for item in self.calls()], [["config", "view"]])

    def test_wrong_or_unavailable_cluster_cannot_mutate(self):
        for mode in ("wrong-cluster", "cluster-read-error"):
            with self.subTest(mode=mode):
                result = self.run_functions("create_instance", mode=mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.mutations(), [])

    def test_existing_agent_or_namespace_is_never_adopted_or_removed(self):
        for resource in ("agent", "namespace"):
            with self.subTest(resource=resource):
                self.state.write_text(json.dumps({resource: {"metadata": {"name": "existing"}}}))
                result = self.run_functions("trap finish EXIT; create_instance")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("already exists", result.stderr)
                self.assertEqual(self.mutations(), [])

    def test_failed_initial_read_is_not_treated_as_absence(self):
        result = self.run_functions("trap finish EXIT; create_instance", mode="initial-read-error")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.mutations(), [])

    def test_successful_create_and_conditional_cleanup(self):
        result = self.run_functions("create_instance; cleanup")
        self.assertEqual(result.returncode, 0, result.stderr)
        writes = self.mutations()
        self.assertEqual([entry["args"][0] for entry in writes], ["create", "delete"])
        self.assertEqual(writes[1]["body"]["preconditions"], {"uid": "created-agent", "resourceVersion": "1"})
        self.assertEqual(writes[1]["args"][2], "/apis/cetirun.microsoft.com/v1alpha1/agents/e2e-fixture")
        self.assertIn("Cleanup verified", result.stdout)
        self.assertEqual(json.loads(self.state.read_text())["namespace"], None)

    def test_lost_or_unconfirmed_create_response_never_deletes_by_name(self):
        for mode in ("lost-create", "missing-create-uid", "wrong-create-label"):
            with self.subTest(mode=mode):
                self.state.write_text(json.dumps({"agent": None, "namespace": None}))
                self.log.unlink(missing_ok=True)
                result = self.run_functions("trap finish EXIT; create_instance", mode=mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual([entry["args"][0] for entry in self.mutations()], ["create"])
                self.assertIsNotNone(json.loads(self.state.read_text())["agent"])
                self.assertNotIn("Cleanup verified", result.stdout)

    def test_suspend_updates_pin_uid_and_current_version(self):
        result = self.run_functions("create_instance; set_suspended true; set_suspended false; cleanup")
        self.assertEqual(result.returncode, 0, result.stderr)
        patches = [item["patch"] for item in self.calls() if "patch" in item]
        self.assertEqual([p[0]["value"] for p in patches], ["created-agent", "created-agent"])
        self.assertEqual([p[1]["value"] for p in patches], ["1", "2"])
        self.assertEqual([p[2]["value"] for p in patches], [True, False])
        self.assertEqual(self.mutations()[-1]["body"]["preconditions"]["resourceVersion"], "3")

    def test_patch_conflict_does_not_rebase_or_retry(self):
        result = self.run_functions("create_instance; set_suspended true", mode="patch-conflict")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([item["args"][0] for item in self.mutations()], ["create", "patch"])
        self.assertNotIn("suspended", json.loads(self.state.read_text())["agent"]["spec"])

    def test_replaced_agent_or_changed_label_blocks_lifecycle_and_cleanup(self):
        for mode in ("replaced-agent", "missing-run-label"):
            for operation in ("set_suspended true", "cleanup", "owned_agent_phase"):
                with self.subTest(mode=mode, operation=operation):
                    self.state.write_text(json.dumps({"agent": None, "namespace": None}))
                    self.log.unlink(missing_ok=True)
                    result = self.run_functions("create_instance; " + operation, mode=mode)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual([entry["args"][0] for entry in self.mutations()], ["create"])

    def test_foreign_namespace_is_never_deleted(self):
        result = self.run_functions("create_instance; cleanup", mode="foreign-namespace")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([entry["args"][0] for entry in self.mutations()], ["create"])
        self.assertIn("namespace retained", result.stderr)

    def test_namespace_replacement_during_cleanup_is_preserved(self):
        result = self.run_functions("create_instance; cleanup", mode="namespace-replacement")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([entry["args"][0] for entry in self.mutations()], ["create", "delete"])
        self.assertEqual(json.loads(self.state.read_text())["namespace"]["metadata"]["uid"], "replacement-namespace")
        self.assertNotIn("Cleanup verified", result.stdout)

    def test_delete_failure_or_unknown_response_is_not_replayed(self):
        for mode in ("delete-conflict", "lost-delete"):
            with self.subTest(mode=mode):
                self.state.write_text(json.dumps({"agent": None, "namespace": None}))
                self.log.unlink(missing_ok=True)
                result = self.run_functions("create_instance; cleanup", mode=mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual([entry["args"][0] for entry in self.mutations()], ["create", "delete"])
                self.assertIn("without replay", result.stderr)
                self.assertNotIn("Cleanup verified", result.stdout)

    def test_cleanup_timeout_changes_success_exit_without_forcing_resources(self):
        result = self.run_functions("trap finish EXIT; create_instance", mode="cleanup-timeout")
        self.assertEqual(result.returncode, 1)
        self.assertIn("cleanup timed out", result.stderr)
        self.assertEqual([entry["args"][0] for entry in self.mutations()], ["create", "delete"])
        self.assertIsNotNone(json.loads(self.state.read_text())["namespace"])
        self.assertNotIn("E2E PASSED", result.stdout)

    def test_observation_error_is_not_cleanup_success(self):
        result = self.run_functions("create_instance; cleanup", mode="namespace-read-error")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([entry["args"][0] for entry in self.mutations()], ["create"])
        self.assertNotIn("Cleanup verified", result.stdout)

    def test_exit_trap_preserves_failure_and_signal_status(self):
        for status in (7, 130, 143):
            with self.subTest(status=status):
                self.state.write_text(json.dumps({"agent": None, "namespace": None}))
                self.log.unlink(missing_ok=True)
                result = self.run_functions(f"trap finish EXIT; create_instance; exit {status}")
                self.assertEqual(result.returncode, status)
                self.assertIn("Cleanup verified", result.stdout)
                self.assertNotIn("E2E PASSED", result.stdout)

    def test_success_exit_requires_observed_cleanup(self):
        result = self.run_functions("trap finish EXIT; create_instance")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("E2E PASSED (controller cleanup verified)", result.stdout)
        self.assertIsNone(json.loads(self.state.read_text())["agent"])
        self.assertIsNone(json.loads(self.state.read_text())["namespace"])

    def test_main_failure_uses_owned_cleanup(self):
        result = subprocess.run(["bash", str(SCRIPT)], env={**self.env, "FAKE_MODE": "main-failure"},
                                stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=15)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn(SENTINEL, result.stdout + result.stderr)
        self.assertEqual([entry["args"][0] for entry in self.mutations()], ["create", "delete"])
        self.assertIn("Cleanup verified", result.stdout)

    def test_workflow_keeps_live_jobs_disabled_and_supplies_explicit_inputs(self):
        source = (ROOT / ".github/workflows/e2e.yml").read_text()
        self.assertEqual(source.count("if: false # E2E temporarily disabled"), 6)
        section = source.split("\n  e2e-test:", 1)[1]
        for name in ("E2E_KUBECONFIG", "E2E_CONTEXT", "E2E_CLUSTER_UID"):
            self.assertIn(name, section)
        self.assertNotIn("TEST_ALIAS=johnreese", section)
        self.assertNotIn('$HOME/.kube/config', section)


if __name__ == "__main__":
    unittest.main()
