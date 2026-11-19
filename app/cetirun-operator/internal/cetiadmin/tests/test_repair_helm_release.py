# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline CLI checks; kubectl is an exact-command synthetic filesystem double."""

import base64
import copy
from decimal import Decimal
import gzip
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import subprocess
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "assets/scripts/repair_helm_release.py"
AGENT = "example-agent"
AGENT_UID = "agent-uid-001"
NAMESPACE = "example-runtime"
NAMESPACE_UID = "namespace-uid-001"
RELEASE = "example-release"
REQUEST = "repair-request-001"
RELEASE_RECORD_NAME = f"sh.helm.release.v1.{RELEASE}.v1"
SENSITIVE = "synthetic-secret-never-print-this"
OTHER_SENSITIVE = "unrelated-synthetic-secret-never-read-this"
HISTORY_FIELDS = (
    ".metadata.name", ".metadata.namespace", ".metadata.uid",
    ".metadata.resourceVersion", ".type", ".metadata.labels.owner",
    ".metadata.labels.name", ".metadata.labels.version", ".metadata.labels.status",
    ".metadata.deletionTimestamp",
)
HISTORY_FORMAT = (
    "jsonpath={range .items[*]}"
    + '{"\\t"}'.join("{" + field + "}" for field in HISTORY_FIELDS)
    + '{"\\n"}{end}'
)


def encoded_release(raw):
    return base64.b64encode(base64.b64encode(gzip.compress(raw, mtime=0))).decode()


def decoded_release(record):
    return gzip.decompress(base64.b64decode(base64.b64decode(record["data"]["release"])))


def object_bytes(obj):
    return (json.dumps(obj, indent=2, ensure_ascii=False) + "\n").encode()


def fixture():
    payload = {
        "name": RELEASE, "namespace": NAMESPACE, "version": 1,
        "config": {
            "token": SENSITIVE,
            "nested": {"info": {"status": "pending-install"}},
            "precise": "EXACT_DECIMAL", "large": 123456789012345678901234567890,
        },
        "info": {"status": "pending-install", "description": "Keep this description", "notes": SENSITIVE},
        "chart": {"metadata": {"name": "example-chart", "version": "0.1.0"}},
        "manifest": "apiVersion: v1\nkind: ConfigMap\n# " + SENSITIVE,
        "hooks": [{"name": "old-hook", "manifest": "# " + SENSITIVE}],
        "unknownFutureField": {"keep": [True, None, "中文"]},
    }
    raw = json.dumps(payload, indent=2, ensure_ascii=False).replace('"EXACT_DECIMAL"', "1.2345678901234567890123456789").encode()
    return {
        "agent": {
            "apiVersion": "cetirun.microsoft.com/v1alpha1", "kind": "Agent",
            "metadata": {"name": AGENT, "uid": AGENT_UID, "resourceVersion": "20", "generation": 7},
            "spec": {"agent": {"type": "hermes"}, "suspended": True,
                     "helm": {"maintenance": {"requestID": REQUEST, "expectedRevision": 1}}},
            "status": {"namespace": NAMESPACE, "helmReleaseName": RELEASE,
                       "helmMaintenance": {"requestID": REQUEST, "observedGeneration": 7,
                                           "revision": 1, "phase": "Held"}},
        },
        "namespace": {
            "apiVersion": "v1", "kind": "Namespace",
            "metadata": {"name": NAMESPACE, "uid": NAMESPACE_UID, "resourceVersion": "30",
                         "ownerReferences": [{"apiVersion": "cetirun.microsoft.com/v1alpha1",
                                              "kind": "Agent", "name": AGENT, "uid": AGENT_UID}]},
            "status": {"phase": "Active"},
        },
        "pods": {"apiVersion": "v1", "kind": "PodList", "metadata": {}, "items": []},
        "jobs": {"apiVersion": "batch/v1", "kind": "JobList", "metadata": {}, "items": []},
        "release_record": {
            "apiVersion": "v1", "kind": "Secret", "type": "helm.sh/release.v1",
            "metadata": {"name": RELEASE_RECORD_NAME, "namespace": NAMESPACE, "uid": "release-secret-uid-001",
                         "resourceVersion": "100", "labels": {"owner": "helm", "name": RELEASE,
                                                               "version": "1", "status": "pending-install", "extra": "keep"},
                         "managedFields": [{"manager": "helm", "operation": "Update", "apiVersion": "v1"}],
                         "annotations": {"example.com/keep": "unchanged"}, "creationTimestamp": "2026-01-01T00:00:00Z"},
            "data": {"release": encoded_release(raw), "extra": base64.b64encode(b"keep").decode()},
        },
        "history": [], "calls": [], "counts": {}, "patches": [], "mode": "success",
    }


def kubectl_double():
    state_path = Path(os.environ["REPAIR_TEST_STATE"])
    state = json.loads(state_path.read_text())
    argv = sys.argv[1:]
    state["calls"].append(argv)

    def save():
        state_path.write_text(json.dumps(state))

    def fail():
        save()
        print(SENSITIVE, file=sys.stderr)
        print(SENSITIVE)
        raise SystemExit(1)

    if argv[:2] != ["--context=cetiadmin", "--request-timeout=15s"]:
        fail()
    args = argv[2:]
    record_name = f"sh.helm.release.v1.{RELEASE}.v{state.get('requested_revision', 1)}"
    commands = {
        "agent": ["get", "agents.cetirun.microsoft.com", AGENT, "-o=json"],
        "namespace": ["get", "namespace", NAMESPACE, "-o=json"],
        "pods": ["get", "pods", "--namespace", NAMESPACE, "-o=json"],
        "jobs": ["get", "jobs.batch", "--namespace", NAMESPACE, "-o=json"],
        "history": ["get", "secrets", "--namespace", NAMESPACE, "-o=" + HISTORY_FORMAT],
        "release_record": ["get", "secret", record_name, "--namespace", NAMESPACE, "--show-managed-fields=true", "-o=json"],
        "patch": ["patch", "secret", record_name, "--namespace", NAMESPACE,
                  "--type=json", "--patch-file=/dev/stdin", "-o=name"],
    }
    operation = next((key for key, command in commands.items() if command == args), None)
    if operation is None:
        fail()
    count = state["counts"][operation] = state["counts"].get(operation, 0) + 1
    if operation == state.get("fail_read"):
        fail()
    if operation == "agent" and count == 2 and state["mode"] == "stale-held":
        state["agent"]["metadata"]["generation"] += 1
    if operation == "history":
        records = [*state["history"], state["release_record"], {
            "metadata": {"name": "unrelated-auth", "namespace": NAMESPACE,
                         "uid": "unrelated-uid", "resourceVersion": "9",
                         "annotations": {"kubectl.kubernetes.io/last-applied-configuration": OTHER_SENSITIVE}},
            "type": "Opaque", "data": {"token": OTHER_SENSITIVE},
        }]
        if state.get("empty_history"):
            records = []
        rows = []
        for record in records:
            values = []
            for field in HISTORY_FIELDS:
                value = record
                for part in field.lstrip(".").split("."):
                    value = value.get(part, "") if isinstance(value, dict) else ""
                values.append(str(value))
            rows.append("\t".join(values) + "\n")
        save()
        sys.stdout.write("".join(rows))
    elif operation != "patch":
        save()
        if operation == "release_record" and state["patches"] and state["mode"] == "unreadable-result":
            fail()
        sys.stdout.buffer.write(object_bytes(state[operation]))
    else:
        if any(state["counts"].get(key, 0) < 2 for key in ("agent", "namespace", "pods", "jobs", "history", "release_record")):
            fail()
        patch = json.load(sys.stdin)
        if not isinstance(patch, list) or any(item["op"] not in ("test", "replace") for item in patch):
            fail()
        replacements = {(item["op"], item["path"]) for item in patch if item["op"] != "test"}
        if replacements != {("replace", "/metadata/labels/status"), ("replace", "/data/release")}:
            fail()
        tests = {item["path"]: item["value"] for item in patch if item["op"] == "test"}
        original = state["release_record"]
        if tests.get("/metadata/uid") != original["metadata"]["uid"] or tests.get("/metadata/resourceVersion") != original["metadata"]["resourceVersion"]:
            fail()
        backups = list(Path(state["backup_dir"]).rglob("original-secret.json"))
        if len(backups) != 1 or backups[0].read_bytes() != object_bytes(original):
            fail()
        if stat.S_IMODE(backups[0].stat().st_mode) != 0o600 or stat.S_IMODE(backups[0].parent.stat().st_mode) != 0o700:
            fail()
        state["patches"].append([{"op": item["op"], "path": item["path"]} for item in patch])
        if state["mode"] in ("conflict", "replaced"):
            original["metadata"]["resourceVersion"] = "102"
            if state["mode"] == "replaced":
                original["metadata"]["uid"] = "replacement-secret-uid"
            fail()
        if state["mode"] == "unacknowledged-unapplied":
            fail()
        updated = copy.deepcopy(original)
        for item in patch:
            target = updated
            parts = item["path"].strip("/").split("/")
            for part in parts[:-1]:
                target = target[part]
            if item["op"] == "test":
                if target[parts[-1]] != item["value"]:
                    fail()
            else:
                target[parts[-1]] = item["value"]
        updated["metadata"]["resourceVersion"] = "101"
        updated["metadata"]["managedFields"] = [{"manager": "kubectl-patch", "operation": "Update"}]
        if state["mode"] == "altered-result":
            updated["metadata"]["annotations"]["example.com/keep"] = "changed"
        if state["mode"] == "label-only":
            updated["data"]["release"] = original["data"]["release"]
        state["release_record"] = updated
        save()
        if state["mode"] == "lost-ack":
            fail()
        print("secret/" + RELEASE_RECORD_NAME)


class RepairHelmReleaseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-helm-repair-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.backups = self.root / "backups"
        self.refusal_case = 0
        self.state_path = self.root / "state.json"
        self.state = fixture()
        self.state["backup_dir"] = str(self.backups)
        self.save()
        binary_dir = self.root / "bin"
        binary_dir.mkdir()
        kubectl = binary_dir / "kubectl"
        kubectl.write_text(f"#!{sys.executable}\nimport runpy\nrunpy.run_path({str(Path(__file__).resolve())!r}, run_name='kubectl_double')\n")
        kubectl.chmod(0o755)
        self.env = {"PATH": str(binary_dir), "REPAIR_TEST_STATE": str(self.state_path),
                    "CETIADMIN_AGENT_UID": "admin-agent-uid", "PYTHONDONTWRITEBYTECODE": "1"}
        self.arguments = ["--agent", AGENT, "--agent-uid", AGENT_UID, "--namespace", NAMESPACE,
                          "--namespace-uid", NAMESPACE_UID, "--release", RELEASE,
                          "--revision", "1", "--request-id", REQUEST, "--backup-dir", str(self.backups)]

    def save(self):
        self.state["backup_dir"] = str(self.backups)
        self.state_path.write_text(json.dumps(self.state))

    def read_state(self):
        return json.loads(self.state_path.read_text())

    def run_cli(self, *, success=True, arguments=None):
        result = subprocess.run([sys.executable, "-B", str(SCRIPT), *(self.arguments if arguments is None else arguments)],
                                env=self.env, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        observed = self.read_state()
        for sensitive in (SENSITIVE, OTHER_SENSITIVE, self.state["release_record"]["data"]["release"],
                          observed["release_record"]["data"]["release"]):
            if not isinstance(sensitive, str) or not sensitive:
                continue
            self.assertNotIn(sensitive, result.stdout + result.stderr)
            self.assertNotIn(sensitive, json.dumps(observed["calls"]))
        return result

    def assert_refused(self):
        self.refusal_case += 1
        self.backups = self.root / f"refused-{self.refusal_case}"
        self.arguments[self.arguments.index("--backup-dir") + 1] = str(self.backups)
        self.save()
        result = self.run_cli(success=False)
        self.assertEqual(json.loads(result.stdout)["status"], "refused")
        self.assertEqual(self.read_state()["patches"], [])
        self.assertFalse(list(self.backups.rglob("original-secret.json")))

    def test_help_needs_no_identity_or_kubectl(self):
        self.env.pop("CETIADMIN_AGENT_UID")
        result = self.run_cli(arguments=["--help"])
        self.assertIn("--namespace-uid", result.stdout)
        self.assertIn("--request-id", result.stdout)
        self.assertEqual(self.read_state()["calls"], [])

    def test_repair_preserves_original_and_every_non_status_field(self):
        original = copy.deepcopy(self.state["release_record"])
        previous_umask = os.umask(0)
        try:
            result = self.run_cli()
        finally:
            os.umask(previous_umask)
        output = json.loads(result.stdout)
        self.assertEqual(output["status"], "repaired")
        backup = Path(output["backupFile"])
        self.assertTrue(backup.is_relative_to(self.backups))
        self.assertEqual(backup.read_bytes(), object_bytes(original))
        self.assertEqual(stat.S_IMODE(backup.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(backup.parent.stat().st_mode), 0o700)
        current = self.read_state()["release_record"]
        before = json.loads(decoded_release(original), parse_float=Decimal)
        after = json.loads(decoded_release(current), parse_float=Decimal)
        before["info"]["status"] = "failed"
        self.assertEqual(after, before)
        expected_raw = decoded_release(original).replace(
            b'"status": "pending-install",\n    "description":', b'"status": "failed",\n    "description":'
        )
        self.assertEqual(decoded_release(current), expected_raw)
        for key in ("resourceVersion", "managedFields"):
            current["metadata"].pop(key, None)
            original["metadata"].pop(key, None)
        original["metadata"]["labels"]["status"] = "failed"
        original["data"]["release"] = current["data"]["release"]
        self.assertEqual(current, original)
        self.assertEqual(len(self.read_state()["patches"]), 1)

    def test_missing_and_stale_maintenance_are_rejected(self):
        changes = (
            ("spec", "helm", "maintenance"), ("status", "helmMaintenance"),
            ("status", "namespace"), ("status", "helmReleaseName"),
        )
        for fields in changes:
            with self.subTest(missing=fields):
                self.state = fixture()
                target = self.state["agent"]
                for field in fields[:-1]:
                    target = target[field]
                del target[fields[-1]]
                self.assert_refused()
        for field, value in (("phase", "Requested"), ("requestID", "other-request"),
                             ("observedGeneration", 6), ("observedGeneration", True), ("revision", 2)):
            with self.subTest(field=field, value=value):
                self.state = fixture()
                self.state["agent"]["status"]["helmMaintenance"][field] = value
                self.assert_refused()

    def test_conflict_never_retries_or_overwrites_recovery(self):
        self.state["mode"] = "conflict"
        self.save()
        first = self.run_cli(success=False)
        self.assertEqual(json.loads(first.stdout)["status"], "unknown")
        backups = {path: path.read_bytes() for path in self.backups.rglob("*") if path.is_file()}
        self.run_cli(success=False)
        self.assertEqual(len(self.read_state()["patches"]), 1)
        self.assertEqual({path: path.read_bytes() for path in self.backups.rglob("*") if path.is_file()}, backups)

    def test_lost_ack_is_resolved_by_readback_and_repeat_is_observational(self):
        self.state["mode"] = "lost-ack"
        self.save()
        first = json.loads(self.run_cli().stdout)
        self.assertEqual(first["status"], "repaired")
        original = Path(first["backupFile"]).read_bytes()
        again = json.loads(self.run_cli().stdout)
        self.assertEqual(again["status"], "already-repaired")
        self.assertEqual(Path(again["backupFile"]).read_bytes(), original)
        self.assertEqual(len(self.read_state()["patches"]), 1)

    def test_failed_secret_without_original_backup_is_not_recognized(self):
        record = self.state["release_record"]
        raw = decoded_release(record).replace(b'"pending-install"', b'"failed"')
        record["data"]["release"] = encoded_release(raw)
        record["metadata"]["labels"]["status"] = "failed"
        self.assert_refused()

    def test_target_and_namespace_identity_boundaries(self):
        for fields, value in (
            (("agent", "metadata", "uid"), "wrong-uid"),
            (("agent", "metadata", "name"), "wrong-agent"),
            (("agent", "metadata", "deletionTimestamp"), "2026-01-01T00:00:00Z"),
            (("agent", "metadata", "generation"), True),
            (("agent", "spec", "agent", "type"), "unknown-runtime"),
            (("agent", "spec", "sandboxRef"), "protected"),
            (("agent", "status", "sandbox"), {}),
            (("agent", "spec", "suspended"), False),
            (("agent", "spec", "helm", "maintenance", "requestID"), "other-request"),
            (("agent", "spec", "helm", "maintenance", "expectedRevision"), 2),
            (("agent", "status", "namespace"), "wrong-namespace"),
            (("agent", "status", "helmReleaseName"), "wrong-release"),
            (("namespace", "metadata", "uid"), "wrong-uid"),
            (("namespace", "metadata", "name"), "wrong-namespace"),
            (("namespace", "metadata", "deletionTimestamp"), "2026-01-01T00:00:00Z"),
            (("namespace", "status", "phase"), "Terminating"),
            (("namespace", "metadata", "ownerReferences"), []),
            (("namespace", "metadata", "ownerReferences"), [{"uid": "foreign"}]),
            (("namespace", "metadata", "labels"), {"cetirun.microsoft.com/sandbox-protected": "true"}),
            (("namespace", "metadata", "annotations"), {"cetirun.microsoft.com/agent-uid": "foreign"}),
        ):
            with self.subTest(fields=fields, value=value):
                self.state = fixture()
                target = self.state
                for field in fields[:-1]:
                    target = target[field]
                target[fields[-1]] = value
                self.assert_refused()

    def test_self_identity_must_be_present_and_different(self):
        for self_uid in (None, AGENT_UID):
            with self.subTest(self_uid=self_uid):
                self.state = fixture()
                if self_uid is None:
                    self.env.pop("CETIADMIN_AGENT_UID", None)
                else:
                    self.env["CETIADMIN_AGENT_UID"] = self_uid
                self.assert_refused()

    def test_invalid_arguments_make_no_kubectl_call(self):
        for flag, value in (("--revision", "0"), ("--revision", "-1"), ("--revision", "01"),
                            ("--request-id", "../escape"), ("--agent", "bad/name"),
                            ("--namespace", "wrong.namespace"), ("--backup-dir", "relative")):
            with self.subTest(flag=flag):
                arguments = list(self.arguments)
                arguments[arguments.index(flag) + 1] = value
                self.run_cli(success=False, arguments=arguments)
                self.assertEqual(self.read_state()["calls"], [])
        self.run_cli(success=False, arguments=[])
        self.assertEqual(self.read_state()["calls"], [])

    def custom_target(self):
        self.state["agent"]["spec"]["agent"]["type"] = "custom"
        ns = self.state["namespace"]["metadata"]
        ns.pop("ownerReferences")
        ns["annotations"] = {"cetirun.microsoft.com/agent-uid": AGENT_UID}
        payload = json.loads(decoded_release(self.state["release_record"]))
        payload["config"]["runtime"] = {"bindings": {"agentName": AGENT, "agentUID": AGENT_UID,
                                                       "namespace": {"name": NAMESPACE, "uid": NAMESPACE_UID}}}
        self.state["release_record"]["data"]["release"] = encoded_release(json.dumps(payload).encode())

    def test_ordinary_custom_uses_existing_namespace_and_release_bindings(self):
        self.custom_target()
        self.save()
        self.assertEqual(json.loads(self.run_cli().stdout)["status"], "repaired")

    def test_ordinary_openclaw_and_complete_failed_history_are_supported(self):
        self.state["agent"]["spec"]["agent"]["type"] = "openclaw"
        previous = copy.deepcopy(self.state["release_record"])
        previous["metadata"]["labels"]["status"] = "failed"
        previous["metadata"]["uid"] = "previous-secret-uid"
        self.second_revision()
        self.state["history"] = [previous]
        self.save()
        result = json.loads(self.run_cli().stdout)
        self.assertEqual(result["status"], "repaired")
        self.assertEqual(result["revision"], 2)
        self.assertEqual(self.read_state()["history"], [previous])
        for path in self.backups.rglob("*"):
            if path.is_file():
                self.assertNotIn(OTHER_SENSITIVE.encode(), path.read_bytes())

    def test_custom_foreign_namespace_or_payload_binding_is_rejected(self):
        for field in ("namespace_annotation", "agentName", "agentUID", "namespace"):
            with self.subTest(field=field):
                self.state = fixture()
                self.custom_target()
                if field == "namespace_annotation":
                    self.state["namespace"]["metadata"]["annotations"].clear()
                else:
                    payload = json.loads(decoded_release(self.state["release_record"]))
                    binding = payload["config"]["runtime"]["bindings"]
                    binding[field] = {"uid": "foreign"} if field == "namespace" else "foreign"
                    self.state["release_record"]["data"]["release"] = encoded_release(json.dumps(payload).encode())
                self.assert_refused()

    def stopped_pod(self):
        return {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": "old-writer", "namespace": NAMESPACE, "uid": "pod-uid", "resourceVersion": "40"},
                "spec": {"containers": [{"name": "main"}]},
                "status": {"phase": "Succeeded", "containerStatuses": [
                    {"name": "main", "state": {"terminated": {"exitCode": 0}}}]}}

    def stopped_job(self):
        return {"apiVersion": "batch/v1", "kind": "Job",
                "metadata": {"name": "old-hook", "namespace": NAMESPACE, "uid": "job-uid", "resourceVersion": "50",
                             "annotations": {"helm.sh/hook": "pre-install"}},
                "status": {"active": 0, "conditions": [{"type": "Complete", "status": "True"}]}}

    def test_terminal_pods_and_hooks_are_allowed(self):
        self.state["pods"]["items"] = [self.stopped_pod()]
        self.state["jobs"]["items"] = [self.stopped_job()]
        self.save()
        self.run_cli()

    def test_running_terminating_and_unknown_writers_are_rejected(self):
        for phase in ("Running", "Pending", "Unknown", None):
            with self.subTest(phase=phase):
                self.state = fixture()
                pod = self.stopped_pod()
                pod["status"]["phase"] = phase
                self.state["pods"]["items"] = [pod]
                self.assert_refused()
        for kind in ("terminating", "missing-state", "running-container", "extra-running-container"):
            with self.subTest(kind=kind):
                self.state = fixture()
                pod = self.stopped_pod()
                if kind == "terminating":
                    pod["metadata"]["deletionTimestamp"] = "2026-01-01T00:00:00Z"
                elif kind == "missing-state":
                    pod["status"]["containerStatuses"] = []
                elif kind == "running-container":
                    pod["status"]["containerStatuses"][0]["state"] = {"running": {}}
                else:
                    pod["status"]["ephemeralContainerStatuses"] = [{"name": "unknown", "state": {"running": {}}}]
                self.state["pods"]["items"] = [pod]
                self.assert_refused()

    def test_active_terminating_and_uncertain_hook_jobs_are_rejected(self):
        for field, value in (("active", 1), ("active", False), ("terminating", 1), ("conditions", [])):
            with self.subTest(field=field, value=value):
                self.state = fixture()
                job = self.stopped_job()
                job["status"][field] = value
                self.state["jobs"]["items"] = [job]
                self.assert_refused()

    def test_revision_must_be_latest_and_history_must_not_have_gaps(self):
        self.state["history"] = [copy.deepcopy(self.state["release_record"])]
        later = self.state["history"][0]
        later["metadata"]["name"] = f"sh.helm.release.v1.{RELEASE}.v2"
        later["metadata"]["labels"]["version"] = "2"
        later["metadata"]["labels"]["status"] = "failed"
        self.assert_refused()
        self.state = fixture()
        self.state["empty_history"] = True
        self.assert_refused()

    def second_revision(self):
        self.arguments[self.arguments.index("--revision") + 1] = "2"
        self.state["requested_revision"] = 2
        self.state["agent"]["spec"]["helm"]["maintenance"]["expectedRevision"] = 2
        self.state["agent"]["status"]["helmMaintenance"]["revision"] = 2
        record = self.state["release_record"]
        record["metadata"]["name"] = f"sh.helm.release.v1.{RELEASE}.v2"
        record["metadata"]["labels"]["version"] = "2"
        payload = json.loads(decoded_release(record))
        payload["version"] = 2
        record["data"]["release"] = encoded_release(json.dumps(payload).encode())

    def test_prior_success_or_uncertain_history_is_not_repairable(self):
        for status in ("deployed", "superseded", "uninstalled", "pending-upgrade", "pending-rollback", "unknown"):
            with self.subTest(status=status):
                self.state = fixture()
                previous = copy.deepcopy(self.state["release_record"])
                previous["metadata"]["labels"]["status"] = status
                previous["metadata"]["uid"] = "previous-secret-uid"
                self.second_revision()
                self.state["history"] = [previous]
                self.assert_refused()
        self.state = fixture()
        self.second_revision()
        self.assert_refused()

    def test_bad_encoding_and_decompression_are_rejected_before_backup(self):
        raw = decoded_release(self.state["release_record"])
        double = lambda data: base64.b64encode(base64.b64encode(data)).decode()
        cases = {
            "outer-base64": "!invalid!", "inner-base64": base64.b64encode(b"!invalid!").decode(),
            "legacy-uncompressed": double(raw), "truncated-gzip": double(gzip.compress(raw)[:-4]),
            "trailing-gzip": double(gzip.compress(raw) + b"extra"),
            "multiple-members": double(gzip.compress(raw) + gzip.compress(b"{}")),
            "invalid-json": encoded_release(b"{"), "non-object": encoded_release(b"null"),
            "duplicate-key": encoded_release(raw.replace(b'"version": 1,', b'"version": 1, "version": 1,')),
            "non-json-number": encoded_release(raw.replace(b"1.2345678901234567890123456789", b"NaN")),
            "encoded-limit": "A" * (2 * 1024 * 1024 + 1),
            "decompressed-limit": encoded_release(b'{"oversized":"' + b"x" * (8 * 1024 * 1024) + b'"}'),
        }
        for name, data in cases.items():
            with self.subTest(name=name):
                self.state = fixture()
                self.state["release_record"]["data"]["release"] = data
                self.assert_refused()

    def test_secret_and_payload_identity_must_match(self):
        for field, value in (("name", "wrong-release"), ("namespace", "wrong-namespace"),
                             ("version", 2), ("version", True), ("info", {"status": "pending-upgrade"})):
            with self.subTest(field=field):
                self.state = fixture()
                payload = json.loads(decoded_release(self.state["release_record"]))
                payload[field] = value
                self.state["release_record"]["data"]["release"] = encoded_release(json.dumps(payload).encode())
                self.assert_refused()
        for field, value in (("owner", "foreign"), ("name", "other-release"), ("version", "01"), ("status", "deployed")):
            with self.subTest(label=field):
                self.state = fixture()
                self.state["release_record"]["metadata"]["labels"][field] = value
                self.assert_refused()
        self.state = fixture()
        self.state["release_record"]["type"] = "Opaque"
        self.assert_refused()

    def test_read_failures_never_mean_an_empty_healthy_inventory(self):
        for resource in ("agent", "namespace", "pods", "jobs", "history", "release_record"):
            with self.subTest(resource=resource):
                self.state = fixture()
                self.state["fail_read"] = resource
                self.assert_refused()

    def test_changed_handoff_before_patch_is_rejected(self):
        self.state["mode"] = "stale-held"
        self.save()
        result = self.run_cli(success=False)
        self.assertEqual(json.loads(result.stdout)["status"], "refused")
        self.assertEqual(self.read_state()["patches"], [])
        self.assertEqual(len(list(self.backups.rglob("original-secret.json"))), 1)

    def test_unapplied_unknown_write_is_not_repeated(self):
        self.state["mode"] = "unacknowledged-unapplied"
        self.save()
        for _ in range(2):
            self.assertEqual(json.loads(self.run_cli(success=False).stdout)["status"], "unknown")
        self.assertEqual(len(self.read_state()["patches"]), 1)

    def test_failed_readback_can_be_recognized_later_without_another_write(self):
        self.state["mode"] = "unreadable-result"
        self.save()
        self.assertEqual(json.loads(self.run_cli(success=False).stdout)["status"], "unknown")
        self.state = self.read_state()
        self.state["mode"] = "success"
        self.save()
        self.assertEqual(json.loads(self.run_cli().stdout)["status"], "already-repaired")
        self.assertEqual(len(self.read_state()["patches"]), 1)

    def test_inconsistent_success_or_replacement_is_not_reported_as_repaired(self):
        for mode in ("altered-result", "label-only", "replaced"):
            with self.subTest(mode=mode):
                self.state = fixture()
                self.state["mode"] = mode
                self.backups = self.root / mode
                self.arguments[self.arguments.index("--backup-dir") + 1] = str(self.backups)
                self.save()
                self.assertEqual(json.loads(self.run_cli(success=False).stdout)["status"], "unknown")
                self.assertEqual(len(self.read_state()["patches"]), 1)

    def test_foreign_or_incomplete_recovery_directory_is_never_overwritten(self):
        recovery = self.backups / ("repair-" + REQUEST)
        recovery.mkdir(parents=True, mode=0o700)
        sentinel = recovery / "unrelated.txt"
        sentinel.write_text("keep unrelated files")
        self.run_cli(success=False)
        self.assertEqual(list(recovery.iterdir()), [sentinel])
        self.assertEqual(sentinel.read_text(), "keep unrelated files")
        self.assertEqual(self.read_state()["patches"], [])

    def test_symlinked_backup_root_is_rejected_before_kubectl(self):
        outside = self.root / "outside"
        outside.mkdir()
        self.backups.symlink_to(outside, target_is_directory=True)
        self.run_cli(success=False)
        self.assertEqual(list(outside.iterdir()), [])
        self.assertEqual(self.read_state()["calls"], [])

    def test_replay_requires_matching_original_receipt_and_private_files(self):
        result = json.loads(self.run_cli().stdout)
        original = Path(result["backupFile"])
        receipt = original.parent / "request.json"
        saved = receipt.read_bytes()
        wrong = json.loads(saved)
        wrong["context"]["requestID"] = "foreign-request"
        receipt.write_text(json.dumps(wrong))
        self.run_cli(success=False)
        self.assertEqual(len(self.read_state()["patches"]), 1)
        self.assertEqual(json.loads(receipt.read_bytes()), wrong)
        receipt.write_bytes(saved)
        original.chmod(0o644)
        self.run_cli(success=False)
        self.assertEqual(stat.S_IMODE(original.stat().st_mode), 0o644)
        self.assertEqual(len(self.read_state()["patches"]), 1)

    def test_replay_refuses_corrupt_or_symlinked_original_backup(self):
        result = json.loads(self.run_cli().stdout)
        original = Path(result["backupFile"])
        raw = original.read_bytes()
        changed = json.loads(raw)
        changed["metadata"]["annotations"]["example.com/keep"] = "changed-backup"
        original.write_bytes(object_bytes(changed))
        self.run_cli(success=False)
        self.assertEqual(original.read_bytes(), object_bytes(changed))
        original.unlink()
        outside = self.root / "unrelated-original.json"
        outside.write_bytes(raw)
        original.symlink_to(outside)
        self.run_cli(success=False)
        self.assertTrue(original.is_symlink())
        self.assertEqual(outside.read_bytes(), raw)
        self.assertEqual(len(self.read_state()["patches"]), 1)

    def test_backup_write_failure_prevents_any_patch(self):
        if os.geteuid() == 0:
            self.skipTest("requires an unprivileged process for directory permissions")
        self.backups.mkdir(mode=0o500)
        self.addCleanup(self.backups.chmod, 0o700)
        self.assertEqual(json.loads(self.run_cli(success=False).stdout)["status"], "refused")
        self.assertEqual(list(self.backups.iterdir()), [])
        self.assertEqual(self.read_state()["patches"], [])


if __name__ == "kubectl_double":
    kubectl_double()
elif __name__ == "__main__":
    unittest.main()
