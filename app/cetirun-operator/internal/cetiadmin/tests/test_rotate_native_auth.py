# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Run the real CLI entry point with a strict, in-memory kubectl double.

All Secret bodies, stdin patches, and simulated cluster state stay in memory.
No native kubectl, credentials, network, or Kubernetes service is used.
"""

import base64
import copy
import io
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "assets/scripts/rotate_native_auth.py"
ANNOTATION = "cetirun.microsoft.com/native-auth-rotation-request-id"
REQUEST = "rotation-example-0001"
PRIVATE = "sk-dummy-private-test-value"
API = "cetirun.microsoft.com/v1alpha1"


def encode(value):
    return base64.b64encode(value.encode()).decode()


def decode(value):
    return base64.b64decode(value, validate=True).decode()


def fixture(runtime="hermes"):
    owner = {"apiVersion": API, "kind": "Agent", "name": "example-agent", "uid": "agent-uid"}
    labels = {"app.kubernetes.io/managed-by": "cetirun-operator", "app.kubernetes.io/instance": "example-agent"}
    return {
        "agent": {
            "apiVersion": API, "kind": "Agent",
            "metadata": {"name": "example-agent", "uid": "agent-uid", "resourceVersion": "10",
                         "generation": 1, "annotations": {"keep.example.com/note": "unchanged"},
                         "finalizers": ["cetirun.microsoft.com/finalizer"]},
            "spec": {"agent": {"type": runtime}, "user": {"alias": "example-agent"},
                     "team": "example-team", "gatewayTokenSecretRef": "native-auth"},
            "status": {"namespace": "cetirun-example-agent", "helmReleaseName": "openclaw-example-agent",
                       "phase": "Ready", "observedGeneration": 1,
                       "conditions": [{"type": "Ready", "status": "True", "observedGeneration": 1}]},
        },
        "namespace": {
            "apiVersion": "v1", "kind": "Namespace",
            "metadata": {"name": "cetirun-example-agent", "uid": "namespace-uid", "resourceVersion": "11",
                         "labels": {**labels, "app.kubernetes.io/name": "cetirun"},
                         "ownerReferences": [owner]},
            "status": {"phase": "Active"},
        },
        "secret": {
            "apiVersion": "v1", "kind": "Secret", "type": "Opaque",
            "metadata": {"name": "native-auth", "namespace": "cetirun-example-agent",
                         "uid": "secret-uid", "resourceVersion": "12", "labels": labels,
                         "annotations": {"keep.example.com/note": "unchanged"}},
            "data": {"API_SERVER_KEY": encode("old-api-key"),
                     "OPENCLAW_GATEWAY_TOKEN": encode("old-openclaw-key"),
                     "CETIRUN_AUTH_KEY": encode("old-auth-key"),
                     "hermes.env": encode("# keep this comment\nAPI_SERVER_KEY=old-api-key\n"
                                          "API_SERVER_CORS_ORIGINS=https://example.com\n"
                                          "BRAVE_API_KEY=" + PRIVATE + "\n"),
                     "CETIRUN_GATEWAY_API_KEY": encode(PRIVATE),
                     "OPENAI_API_KEY": encode(PRIVATE), "GATEWAY_MASTER_KEY": encode(PRIVATE),
                     "BRAVE_API_KEY": encode(PRIVATE), "binary": "AP8="},
        },
    }


class KubectlDouble:
    def __init__(self, case, state):
        self.case = case
        self.state = state
        self.calls = []
        self.fault = ""
        self.now = 0.0
        self.pending_reads = 0
        self.contract_failures = []

    def sleep(self, seconds):
        self.case.assertGreater(seconds, 0)
        self.case.assertLessEqual(seconds, 2)
        self.now += seconds

    def mutations(self, kind=None, field=None):
        calls = [call for call in self.calls if call["verb"] == "patch"]
        if kind:
            calls = [call for call in calls if call["kind"] == kind]
        if field:
            calls = [call for call in calls if any(
                op["op"] != "test" and op["path"] == field for op in call["patch"]
            )]
        return calls

    def complete_restart(self):
        agent = self.state["agent"]
        agent["status"].update(phase="Ready", observedGeneration=agent["metadata"]["generation"],
                               observedRestartRequestID=agent["spec"]["restartRequestID"],
                               conditions=[{"type": "Ready", "status": "True",
                                            "observedGeneration": agent["metadata"]["generation"]}])

    def __call__(self, command, **kwargs):
        try:
            return self.execute(command, **kwargs)
        except (AssertionError, KeyError, IndexError) as error:
            self.contract_failures.append(type(error).__name__)
            raise

    def execute(self, command, **kwargs):
        self.case.assertIsInstance(command, list)
        self.case.assertEqual(command[:3], ["kubectl", "--context", "cetiadmin"])
        self.case.assertFalse(kwargs.get("shell", False))
        self.case.assertGreater(kwargs["timeout"], 0)
        self.case.assertLessEqual(kwargs["timeout"], 600)
        args = command[3:]
        self.case.assertTrue(args[0].startswith("--request-timeout="))
        args = args[1:]
        namespace = None
        if args[:1] == ["--namespace"]:
            namespace, args = args[1], args[2:]
        verb, resource, name, *options = args
        self.case.assertIn(verb, {"get", "patch"})
        kind = {"agents.cetirun.microsoft.com": "agent", "namespace": "namespace", "secret": "secret"}[resource]
        self.case.assertEqual(name, "native-auth" if kind == "secret" else
                              "example-agent" if kind == "agent" else "cetirun-example-agent")
        self.case.assertEqual(namespace, "cetirun-example-agent" if kind == "secret" else None)
        call = {"argv": command, "verb": verb, "kind": kind}
        self.calls.append(call)
        if verb == "get":
            self.case.assertCountEqual(options, ["--output=json", "--ignore-not-found=true"])
            self.case.assertFalse(kwargs.get("input"))
            if self.fault == "read_failure":
                return subprocess.CompletedProcess(command, 1, "", PRIVATE)
            if self.fault == "read_timeout":
                raise subprocess.TimeoutExpired(command, kwargs["timeout"], output=PRIVATE, stderr=PRIVATE)
            if kind == "agent" and self.state["agent"]["spec"].get("restartRequestID") == REQUEST:
                if self.fault != "restart_timeout":
                    self.pending_reads += 1
                    if self.pending_reads >= 2:
                        self.complete_restart()
            body = self.state.get(kind)
            return subprocess.CompletedProcess(command, 0, json.dumps(body) if body else "", "")

        self.case.assertIn(kind, {"agent", "secret"})
        self.case.assertCountEqual(options, ["--type=json", "--patch-file=/dev/stdin", "--output=name"])
        operations = json.loads(kwargs["input"])
        call["patch"] = copy.deepcopy(operations)
        body = self.state[kind]
        tests = {op["path"]: op["value"] for op in operations if op["op"] == "test"}
        self.case.assertEqual(tests["/metadata/uid"], body["metadata"]["uid"])
        self.case.assertEqual(tests["/metadata/resourceVersion"], body["metadata"]["resourceVersion"])
        is_restart = any(op["path"] == "/spec/restartRequestID" and op["op"] != "test" for op in operations)
        mode = "secret" if kind == "secret" else "restart" if is_restart else "intent"
        if self.fault == mode + "_conflict":
            body["metadata"]["resourceVersion"] = str(int(body["metadata"]["resourceVersion"]) + 1)
            return subprocess.CompletedProcess(command, 1, "", "Conflict: " + PRIVATE)
        if self.fault == mode + "_lost_before_write":
            raise subprocess.TimeoutExpired(command, kwargs["timeout"], output=PRIVATE, stderr=PRIVATE)
        after = copy.deepcopy(body)
        for operation in operations:
            parts = [p.replace("~1", "/").replace("~0", "~") for p in operation["path"].split("/")[1:]]
            target = after
            for part in parts[:-1]:
                target = target[part]
            if operation["op"] == "test":
                self.case.assertEqual(target[parts[-1]], operation["value"])
                continue
            self.case.assertIn(operation["op"], {"add", "replace"})
            if parts[:1] == ["data"]:
                self.case.assertEqual(kind, "secret")
                allowed = {"CETIRUN_AUTH_KEY"}
                allowed |= {"API_SERVER_KEY", "hermes.env"} if self.state["agent"]["spec"]["agent"]["type"] == "hermes" else {"OPENCLAW_GATEWAY_TOKEN"}
                self.case.assertEqual(len(parts), 2)
                self.case.assertIn(parts[1], allowed)
            elif parts[:2] == ["metadata", "annotations"]:
                if len(parts) == 2:
                    self.case.assertIsNone(target.get("annotations"))
                    self.case.assertEqual(operation["value"], {ANNOTATION: REQUEST})
                else:
                    self.case.assertEqual(parts[2:], [ANNOTATION])
                    self.case.assertEqual(operation["value"], REQUEST)
            else:
                self.case.assertEqual(kind, "agent")
                self.case.assertEqual(parts, ["spec", "restartRequestID"])
                self.case.assertEqual(operation["value"], REQUEST)
            target[parts[-1]] = operation["value"]
        after["metadata"]["resourceVersion"] = str(int(after["metadata"]["resourceVersion"]) + 1)
        if is_restart:
            after["metadata"]["generation"] += 1
            after["status"]["phase"] = "Provisioning"
            after["status"]["conditions"] = [{"type": "Ready", "status": "False"}]
        self.state[kind] = after
        if mode == "secret" and self.fault == "namespace_replaced_after_rotation":
            self.state["namespace"]["metadata"]["uid"] = "replacement-uid"
        if self.fault == mode + "_lost_after_write":
            raise subprocess.TimeoutExpired(command, kwargs["timeout"], output=PRIVATE, stderr=PRIVATE)
        return subprocess.CompletedProcess(command, 0, resource + "/" + name + "\n", "")


class RotateNativeAuthCLITest(unittest.TestCase):
    def setUp(self):
        self.state = fixture()
        self.kubectl = KubectlDouble(self, self.state)
        self.argv = [str(SCRIPT), "--agent", "example-agent", "--agent-uid", "agent-uid",
                     "--namespace", "cetirun-example-agent", "--namespace-uid", "namespace-uid",
                     "--request-id", REQUEST, "--timeout-seconds", "5"]

    def run_cli(self, *, success=True, extra=(), env=None):
        output, errors = io.StringIO(), io.StringIO()
        with tempfile.TemporaryDirectory(prefix="cetiadmin-auth-test-") as directory:
            before = Path.cwd()
            os.chdir(directory)
            try:
                with patch.dict(os.environ, {"CETIADMIN_AGENT_UID": "cetiadmin-uid", **(env or {})}, clear=True), \
                     patch.object(sys, "argv", [*self.argv, *extra]), \
                     patch.object(sys, "stdout", output), patch.object(sys, "stderr", errors), \
                     patch("subprocess.run", side_effect=self.kubectl), \
                     patch("time.monotonic", side_effect=lambda: self.kubectl.now), \
                     patch("time.sleep", side_effect=self.kubectl.sleep):
                    with self.assertRaises(SystemExit) as result:
                        runpy.run_path(str(SCRIPT), run_name="__main__")
                self.assertEqual(list(Path(directory).iterdir()), [], "helper wrote runtime artifacts")
            finally:
                os.chdir(before)
        self.assertEqual(errors.getvalue(), "")
        self.assertEqual(self.kubectl.contract_failures, [])
        if success:
            self.assertEqual(result.exception.code, 0, output.getvalue())
        else:
            self.assertNotEqual(result.exception.code, 0, output.getvalue())
        self.assertNotIn(PRIVATE, output.getvalue())
        self.assertNotIn(encode(PRIVATE), output.getvalue())
        return json.loads(output.getvalue())

    def test_both_runtimes_rotate_only_native_auth_and_keep_metadata(self):
        for runtime in ("hermes", "openclaw"):
            with self.subTest(runtime=runtime):
                self.kubectl = KubectlDouble(self, fixture(runtime))
                before = copy.deepcopy(self.kubectl.state["secret"])
                result = self.run_cli()
                secret = self.kubectl.state["secret"]
                self.assertEqual(result["status"], "ready")
                self.assertEqual(result["requestID"], REQUEST)
                keys = {"CETIRUN_AUTH_KEY", "API_SERVER_KEY", "hermes.env"} if runtime == "hermes" else {"CETIRUN_AUTH_KEY", "OPENCLAW_GATEWAY_TOKEN"}
                self.assertEqual({k: v for k, v in secret["data"].items() if k not in keys},
                                 {k: v for k, v in before["data"].items() if k not in keys})
                native = "API_SERVER_KEY" if runtime == "hermes" else "OPENCLAW_GATEWAY_TOKEN"
                self.assertNotEqual(secret["data"][native], before["data"][native])
                self.assertNotEqual(secret["data"]["CETIRUN_AUTH_KEY"], before["data"]["CETIRUN_AUTH_KEY"])
                self.assertNotEqual(secret["data"][native], secret["data"]["CETIRUN_AUTH_KEY"])
                self.assertGreaterEqual(len(decode(secret["data"][native])), 32)
                self.assertEqual(secret["metadata"]["labels"], before["metadata"]["labels"])
                self.assertEqual(secret["metadata"]["annotations"], {**before["metadata"]["annotations"], ANNOTATION: REQUEST})
                self.assertEqual(len(self.kubectl.mutations("secret")), 1)
                self.assertEqual(len(self.kubectl.mutations("agent", "/spec/restartRequestID")), 1)
                if runtime == "hermes":
                    self.assertEqual(decode(secret["data"]["hermes.env"]),
                                     decode(before["data"]["hermes.env"]).replace("old-api-key", decode(secret["data"][native])))
                all_argv = json.dumps([call["argv"] for call in self.kubectl.calls])
                for key in secret["data"].values():
                    self.assertNotIn(key, all_argv)
                self.assertNotIn(decode(secret["data"][native]), all_argv)

    def test_same_request_after_success_does_not_write_again(self):
        self.run_cli()
        state = copy.deepcopy(self.kubectl.state)
        writes = len(self.kubectl.mutations())
        self.run_cli()
        self.assertEqual(self.kubectl.state, state)
        self.assertEqual(len(self.kubectl.mutations()), writes)

    def test_lost_secret_ack_resumes_without_rotating_again(self):
        self.kubectl.fault = "secret_lost_after_write"
        self.assertEqual(self.run_cli(success=False)["status"], "unknown")
        data = copy.deepcopy(self.kubectl.state["secret"]["data"])
        self.assertEqual(self.kubectl.mutations("agent", "/spec/restartRequestID"), [])
        self.kubectl.fault = ""
        self.run_cli()
        self.assertEqual(self.kubectl.state["secret"]["data"], data)
        self.assertEqual(len(self.kubectl.mutations("secret")), 1)

    def test_unseen_secret_write_is_not_resubmitted_on_same_request(self):
        self.kubectl.fault = "secret_lost_before_write"
        self.assertEqual(self.run_cli(success=False)["status"], "unknown")
        self.kubectl.fault = ""
        self.assertEqual(self.run_cli(success=False)["status"], "unknown")
        self.assertEqual(len(self.kubectl.mutations("secret")), 1)
        self.assertEqual(self.kubectl.mutations("agent", "/spec/restartRequestID"), [])

    def test_lost_intent_ack_stops_before_secret_write(self):
        self.kubectl.fault = "intent_lost_after_write"
        self.assertEqual(self.run_cli(success=False)["status"], "unknown")
        self.kubectl.fault = ""
        self.assertEqual(self.run_cli(success=False)["status"], "unknown")
        self.assertEqual(self.kubectl.mutations("secret"), [])

    def test_lost_restart_ack_is_observed_without_another_restart(self):
        self.kubectl.fault = "restart_lost_after_write"
        self.assertEqual(self.run_cli(success=False)["status"], "unknown")
        self.kubectl.fault = ""
        self.run_cli()
        self.assertEqual(len(self.kubectl.mutations("secret")), 1)
        self.assertEqual(len(self.kubectl.mutations("agent", "/spec/restartRequestID")), 1)

    def test_secret_version_conflict_is_not_retried(self):
        old = copy.deepcopy(self.state["secret"]["data"])
        self.kubectl.fault = "secret_conflict"
        self.run_cli(success=False)
        self.kubectl.fault = ""
        self.run_cli(success=False)
        self.assertEqual(self.kubectl.state["secret"]["data"], old)
        self.assertEqual(len(self.kubectl.mutations("secret")), 1)

    def test_restart_in_progress_blocks_new_rotation(self):
        self.state["agent"]["spec"]["restartRequestID"] = "other-request-0001"
        self.assertEqual(self.run_cli(success=False)["code"], "restart_in_progress")
        self.assertEqual(self.kubectl.mutations(), [])

    def test_restart_timeout_keeps_original_keys_and_request_on_retry(self):
        self.kubectl.fault = "restart_timeout"
        self.assertEqual(self.run_cli(success=False)["code"], "restart_timeout")
        data = copy.deepcopy(self.kubectl.state["secret"]["data"])
        self.kubectl.fault = ""
        self.run_cli()
        self.assertEqual(self.kubectl.state["secret"]["data"], data)
        self.assertEqual(len(self.kubectl.mutations("secret")), 1)
        self.assertEqual(len(self.kubectl.mutations("agent", "/spec/restartRequestID")), 1)

    def test_missing_secret_is_left_for_operator_reconstruction(self):
        self.state["secret"] = None
        self.assertEqual(self.run_cli(success=False)["code"], "operator_reconstruction_required")
        self.assertEqual(self.kubectl.mutations(), [])

    def test_foreign_identities_or_metadata_prevent_all_writes(self):
        cases = [
            ("agent UID", "agent", "metadata", "uid", "other-uid"),
            ("namespace UID", "namespace", "metadata", "uid", "other-uid"),
            ("namespace ownership", "namespace", "metadata", "ownerReferences", []),
            ("namespace labels", "namespace", "metadata", "labels", {}),
            ("secret labels", "secret", "metadata", "labels", {"app.kubernetes.io/managed-by": "Helm"}),
            ("secret owner", "secret", "metadata", "ownerReferences", [{"apiVersion": API, "kind": "Agent", "name": "example-agent", "uid": "other-uid"}]),
            ("secret namespace", "secret", "metadata", "namespace", "another-namespace"),
            ("Agent namespace", "agent", "status", "namespace", "another-namespace"),
        ]
        for title, kind, section, key, value in cases:
            with self.subTest(case=title):
                self.kubectl = KubectlDouble(self, fixture())
                self.kubectl.state[kind][section][key] = value
                self.run_cli(success=False)
                self.assertEqual(self.kubectl.mutations(), [])

    def test_suspended_deleting_sandbox_and_self_targets_are_rejected(self):
        for mode in ("suspended", "deleting", "namespace_deleting", "secret_deleting", "sandbox", "protected_namespace", "self", "missing_self", "maintenance"):
            with self.subTest(mode=mode):
                self.kubectl = KubectlDouble(self, fixture())
                env = {}
                if mode == "suspended":
                    self.kubectl.state["agent"]["spec"]["suspended"] = True
                elif mode == "sandbox":
                    self.kubectl.state["agent"]["spec"]["sandboxRef"] = "protected"
                elif mode == "protected_namespace":
                    self.kubectl.state["namespace"]["metadata"]["labels"]["cetirun.microsoft.com/sandbox-protected"] = "true"
                elif mode == "maintenance":
                    self.kubectl.state["agent"]["spec"]["helm"] = {"maintenance": {"requestID": "maintenance-0001"}}
                elif mode in {"self", "missing_self"}:
                    env["CETIADMIN_AGENT_UID"] = "agent-uid" if mode == "self" else ""
                else:
                    kind = mode.split("_")[0] if "_" in mode else "agent"
                    self.kubectl.state[kind]["metadata"]["deletionTimestamp"] = "2030-01-01T00:00:00Z"
                self.run_cli(success=False, env=env)
                self.assertEqual(self.kubectl.mutations(), [])

    def test_custom_runtime_and_stale_ready_are_not_rotated(self):
        for mode in ("custom", "stale_generation", "not_ready"):
            with self.subTest(mode=mode):
                self.kubectl = KubectlDouble(self, fixture())
                if mode == "custom":
                    self.kubectl.state["agent"]["spec"]["agent"]["type"] = "custom"
                elif mode == "stale_generation":
                    self.kubectl.state["agent"]["status"]["observedGeneration"] = 0
                else:
                    self.kubectl.state["agent"]["status"]["conditions"][0]["status"] = "False"
                self.run_cli(success=False)
                self.assertEqual(self.kubectl.mutations(), [])

    def test_namespace_replacement_after_rotation_stops_before_restart(self):
        self.kubectl.fault = "namespace_replaced_after_rotation"
        self.run_cli(success=False)
        self.assertEqual(len(self.kubectl.mutations("secret")), 1)
        self.assertEqual(self.kubectl.mutations("agent", "/spec/restartRequestID"), [])

    def test_native_error_contents_never_reach_output(self):
        self.kubectl.fault = "read_failure"
        self.run_cli(success=False)
        self.assertEqual(self.kubectl.mutations(), [])

    def test_read_timeout_is_not_reported_as_an_uncertain_write(self):
        self.kubectl.fault = "read_timeout"
        self.assertEqual(self.run_cli(success=False)["code"], "read_failed")
        self.assertEqual(self.kubectl.mutations(), [])

    def test_absent_annotation_maps_and_matching_secret_owner_are_supported(self):
        self.state["agent"]["metadata"].pop("annotations")
        self.state["secret"]["metadata"].pop("annotations")
        self.state["secret"]["metadata"]["ownerReferences"] = copy.deepcopy(self.state["namespace"]["metadata"]["ownerReferences"])
        self.run_cli()
        for kind in ("agent", "secret"):
            self.assertEqual(self.kubectl.state[kind]["metadata"]["annotations"], {ANNOTATION: REQUEST})

    def test_inconsistent_hermes_key_is_repaired_without_changing_other_env_lines(self):
        self.state["secret"]["data"]["API_SERVER_KEY"] = encode("different-old-value")
        self.state["secret"]["data"]["hermes.env"] = encode("API_SERVER_KEY=stale-value\r\nKEEP=unchanged\r\n")
        self.run_cli()
        data = self.kubectl.state["secret"]["data"]
        self.assertEqual(decode(data["hermes.env"]), "API_SERVER_KEY=" + decode(data["API_SERVER_KEY"]) + "\r\nKEEP=unchanged\r\n")

    def test_damaged_current_request_receipt_is_not_rotated_again(self):
        self.run_cli()
        self.kubectl.state["secret"]["data"]["hermes.env"] = encode("API_SERVER_KEY=other-value\n")
        writes = len(self.kubectl.mutations())
        self.assertEqual(self.run_cli(success=False)["status"], "unknown")
        self.assertEqual(len(self.kubectl.mutations()), writes)

    def test_invalid_native_env_is_rejected_before_recording_intent(self):
        self.state["secret"]["data"]["hermes.env"] = "not-base64"
        self.assertEqual(self.run_cli(success=False)["code"], "invalid_native_auth")
        self.assertEqual(self.kubectl.mutations(), [])

    def test_previous_incomplete_rotation_blocks_a_different_request(self):
        previous = "previous-example-0001"
        self.state["agent"]["metadata"]["annotations"][ANNOTATION] = previous
        self.state["secret"]["metadata"]["annotations"][ANNOTATION] = previous
        self.assertEqual(self.run_cli(success=False)["code"], "previous_rotation_incomplete")
        self.assertEqual(self.kubectl.mutations(), [])

    def test_new_request_can_follow_a_completed_rotation(self):
        previous = "previous-example-0001"
        for kind in ("agent", "secret"):
            self.state[kind]["metadata"]["annotations"][ANNOTATION] = previous
        self.state["agent"]["spec"]["restartRequestID"] = previous
        self.state["agent"]["status"]["observedRestartRequestID"] = previous
        self.run_cli()
        self.assertEqual(len(self.kubectl.mutations("secret")), 1)

    def test_unfinished_migration_blocks_rotation_but_completed_migration_does_not(self):
        request = {"requestID": "migration-example-0001", "target": {"name": "node-a", "uid": "node-uid"}}
        self.state["agent"]["spec"]["plannedMigration"] = request
        self.state["agent"]["status"]["plannedMigration"] = {**request, "phase": "Stopping"}
        self.assertEqual(self.run_cli(success=False)["code"], "migration_in_progress")
        self.assertEqual(self.kubectl.mutations(), [])
        self.state["agent"]["status"]["plannedMigration"]["phase"] = "Completed"
        self.run_cli()

    def test_invalid_arguments_make_no_kubectl_calls(self):
        for extra in (("--context", "other"), ("--timeout-seconds", "0"),
                      ("--timeout-seconds", "601"), ("--request-id", "short")):
            with self.subTest(extra=extra):
                self.kubectl = KubectlDouble(self, fixture())
                self.run_cli(success=False, extra=extra)
                self.assertEqual(self.kubectl.calls, [])

    def test_help_works_without_runtime_identity(self):
        output = io.StringIO()
        with patch.dict(os.environ, {}, clear=True), patch.object(sys, "argv", [str(SCRIPT), "--help"]), \
             patch.object(sys, "stdout", output), patch("subprocess.run", side_effect=AssertionError("unexpected kubectl")):
            with self.assertRaises(SystemExit) as result:
                runpy.run_path(str(SCRIPT), run_name="__main__")
        self.assertEqual(result.exception.code, 0)
        for flag in ("--agent", "--agent-uid", "--namespace", "--namespace-uid", "--request-id", "--timeout-seconds"):
            self.assertIn(flag, output.getvalue())


if __name__ == "__main__":
    unittest.main()
