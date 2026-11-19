# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Exercise the shipped CLI against synthetic, disk-backed native modules.

The fixtures implement the cron.jobs API of the pinned Hermes source. They do
not qualify Hermes itself, its scheduler, Slack, or a running CetiAdmin.
"""

import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "assets/scripts/cron_sync.py"
HERMES_SHA = "b200f5bc60e31588ba2cb60e288029c4a54ba5fa"
PRIVATE_TEXT = "sk-dummy-must-not-appear"

NATIVE_JOBS = r'''
import json
import os
from datetime import datetime, timedelta
from pathlib import Path
from zoneinfo import ZoneInfo

home = Path(os.environ["HERMES_HOME"])
store = home / "fixture-native-jobs.json"
trace = home / "fixture-native-calls.json"
fault = os.environ.get("CETIADMIN_FIXTURE_FAULT", "")

def record(action, **fields):
    calls = json.loads(trace.read_text()) if trace.exists() else []
    calls.append({"action": action, "timezone": os.environ.get("HERMES_TIMEZONE"), **fields})
    trace.write_text(json.dumps(calls))
    if fault == "noisy":
        print("sk-dummy-must-not-appear", flush=True)
        os.write(2, b"sk-dummy-must-not-appear\n")

record("import")

def load():
    return json.loads(store.read_text()) if store.exists() else []

def save(jobs):
    store.write_text(json.dumps(jobs))

def parse_schedule(schedule):
    record("parse")
    if schedule in ("every 30m", "every 10m", "every 2h"):
        minutes = {"every 30m": 30, "every 10m": 10, "every 2h": 120}[schedule]
        return {"kind": "interval", "minutes": minutes, "display": schedule}
    if schedule == "0 9 * * *":
        return {"kind": "cron", "expr": schedule, "display": schedule}
    if schedule == "in 30m":
        return {"kind": "once", "run_at": "2030-01-01T09:30:00+00:00"}
    raise ValueError("bad schedule: sk-dummy-must-not-appear")

def next_run(schedule):
    clock = datetime(2030, 1, 1, 9, tzinfo=ZoneInfo(os.environ["HERMES_TIMEZONE"]))
    return (clock + timedelta(minutes=schedule.get("minutes", 0))).isoformat()

def list_jobs(include_disabled=False):
    record("list", include_disabled=include_disabled)
    if fault == "list_failure":
        raise RuntimeError("sk-dummy-must-not-appear")
    return [j for j in load() if include_disabled or j["enabled"]]

def get_job(job_id):
    record("get", job_id=job_id)
    if fault == "missing_readback":
        return None
    job = next((j for j in load() if j["id"] == job_id), None)
    if job and fault == "bad_readback":
        job["deliver"] = "slack:CUNRELATED"
    if job and fault == "no_next_run" and job["enabled"]:
        job["next_run_at"] = None
    return job

def create_job(prompt, schedule, name=None, repeat=None, deliver=None,
               origin=None, skill=None, skills=None, model=None, provider=None,
               base_url=None, script=None, context_from=None, enabled_toolsets=None,
               workdir=None, no_agent=False, attach_to_session=None,
               monitor_script=None, monitor_url=None, reasoning_effort=None,
               failure_deliver=None, paused=False, paused_reason=None, pinned=False):
    record("create", paused=paused)
    if fault == "create_before_write":
        raise RuntimeError("sk-dummy-must-not-appear")
    parsed = parse_schedule(schedule)
    job = {
        "id": "0123456789ab", "name": name, "prompt": prompt.strip(),
        "schedule": parsed, "schedule_display": parsed["display"],
        "skills": skills or ([skill] if skill else []), "skill": skill or (skills or [None])[0],
        "repeat": {"times": repeat, "completed": 0},
        "enabled": not paused, "state": "paused" if paused else "scheduled",
        "paused_at": "2030-01-01T00:00:00+00:00" if paused else None,
        "paused_reason": paused_reason, "deliver": deliver, "origin": origin,
        "failure_deliver": failure_deliver, "next_run_at": None if paused else next_run(parsed),
        "script": script, "no_agent": no_agent, "monitor_script": monitor_script,
        "monitor_url": monitor_url, "context_from": context_from,
        "attach_to_session": attach_to_session, "model": model,
    }
    jobs = load() + [job]
    if fault == "create_collision":
        jobs.append({**job, "id": "abcdef012345"})
    save(jobs)
    if fault == "create_after_write":
        raise RuntimeError("sk-dummy-must-not-appear")
    if fault == "create_bad_id":
        return {**job, "id": "../../sk-dummy-must-not-appear"}
    return job

def update_job(job_id, updates):
    record("update", job_id=job_id, fields=sorted(updates))
    jobs = load()
    for job in jobs:
        if job["id"] != job_id:
            continue
        if fault == "ignore_update":
            return job
        job.update(updates)
        if "schedule" in updates:
            job["schedule"] = parse_schedule(updates["schedule"])
            job["schedule_display"] = job["schedule"]["display"]
            if job["state"] != "paused":
                job["next_run_at"] = next_run(job["schedule"])
        save(jobs)
        if fault == "update_after_write":
            raise RuntimeError("sk-dummy-must-not-appear")
        return job
    return None

def pause_job(job_id, reason=None):
    record("pause", job_id=job_id)
    return update_job(job_id, {"enabled": False, "state": "paused",
                              "paused_at": "2030-01-01T00:00:00+00:00", "paused_reason": reason})

def resume_job(job_id):
    record("resume", job_id=job_id)
    job = get_job(job_id)
    if not job:
        return None
    if fault == "ignore_resume":
        return job
    return update_job(job_id, {"enabled": True, "state": "scheduled",
                              "paused_at": None, "paused_reason": None,
                              "next_run_at": next_run(job["schedule"])})

def remove_job(job_id):
    record("remove", job_id=job_id)
    raise AssertionError("monitor synchronization must not remove native jobs")
'''


class CronSyncCLITest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="cetiadmin-cron-test-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.home = self.directory / "runtime-home"
        self.home.mkdir(mode=0o700)
        self.install = self.directory / "native-install"
        (self.install / "cron").mkdir(parents=True)
        (self.install / "install-stamp.json").write_text(json.dumps({
            "commit": HERMES_SHA, "baseVersion": "0.22.7", "updateMechanism": "external", "distribution": "docker"
        }))
        (self.install / "hermes_cli").mkdir()
        (self.install / "hermes_cli/__init__.py").write_text("")
        (self.install / "hermes_cli/version_info.py").write_text(
            "from types import SimpleNamespace\n"
            "def get_version_info():\n"
            f"    return SimpleNamespace(base_version='0.22.7', commit='{HERMES_SHA}', source='docker')\n"
        )
        (self.install / "cron/__init__.py").write_text("")
        (self.install / "cron/jobs.py").write_text(NATIVE_JOBS)
        (self.install / "hermes_time.py").write_text(
            'import os\nfrom zoneinfo import ZoneInfo\n'
            'def get_timezone():\n    return ZoneInfo(os.environ["HERMES_TIMEZONE"])\n'
        )
        self.config_path = self.directory / "monitor.json"
        self.config = {
            "instance": {"name": "cetiadmin", "namespace": "cetirun-cetiadmin", "uid": "agent-uid-1"},
            "monitor": {
                "enabled": True, "schedule": "every 30m", "timezone": "UTC",
                "deliver": "local", "slackChannel": "", "slackSecretName": "",
            },
        }
        self.state_dir = self.home / "cetiadmin"
        self.binding = self.state_dir / "cron-binding.json"
        self.store = self.home / "fixture-native-jobs.json"
        self.trace = self.home / "fixture-native-calls.json"
        self.environment = {
            "PATH": os.defpath, "PYTHONDONTWRITEBYTECODE": "1",
            "HERMES_HOME": str(self.home), "HERMES_INSTALL_DIR": str(self.install),
        }

    def run_cli(self, *, fault="", extra=(), env=None, success=True):
        self.config_path.write_text(json.dumps(self.config))
        environment = {**self.environment, "CETIADMIN_FIXTURE_FAULT": fault, **(env or {})}
        result = subprocess.run(
            [sys.executable, "-B", str(SCRIPT), "--config", str(self.config_path), *extra],
            cwd=self.directory, env=environment, capture_output=True, text=True, timeout=10,
        )
        self.assertNotIn(PRIVATE_TEXT, result.stdout + result.stderr)
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stderr, "", result.stderr)
        return json.loads(result.stdout)

    def jobs(self):
        return json.loads(self.store.read_text()) if self.store.exists() else []

    def calls(self, action):
        calls = json.loads(self.trace.read_text()) if self.trace.exists() else []
        return [call for call in calls if call["action"] == action]

    def change_job(self, **fields):
        jobs = self.jobs()
        jobs[0].update(fields)
        self.store.write_text(json.dumps(jobs))

    def slack(self):
        self.config["monitor"].update(
            deliver="slack", slackChannel="C012EXAMPLE", slackSecretName="monitor-slack"
        )
        self.environment.update(SLACK_BOT_TOKEN=PRIVATE_TEXT, SLACK_APP_TOKEN="xapp-dummy")

    def test_help_needs_no_runtime_or_credentials(self):
        result = subprocess.run(
            [sys.executable, "-B", str(SCRIPT), "--help"],
            env={"PATH": os.defpath}, capture_output=True, text=True, timeout=10,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--config", result.stdout)
        self.assertIn("--state-dir", result.stdout)

    def test_create_then_repeat_keeps_one_id_and_next_run(self):
        first = self.run_cli()
        before = self.jobs()[0]
        saved = self.binding.read_bytes()
        second = self.run_cli()
        self.assertEqual(first["jobId"], second["jobId"])
        self.assertEqual(second["status"], "unchanged")
        self.assertEqual(self.binding.read_bytes(), saved)
        self.assertEqual(self.jobs(), [before])
        self.assertEqual(len(self.calls("create")), 1)
        self.assertTrue(self.calls("create")[0]["paused"])
        self.assertTrue(all(c["include_disabled"] for c in self.calls("list")))
        self.assertEqual(before["skills"], ["cetiadmin"])
        self.assertEqual(before["deliver"], "local")
        self.assertEqual(before["state"], "scheduled")
        self.assertIsNotNone(before["next_run_at"])
        self.assertEqual(self.binding.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.state_dir.stat().st_mode & 0o777, 0o700)

    def test_schedule_change_edits_known_job_and_keeps_native_history(self):
        first = self.run_cli()
        paths = [self.home / "cron/output" / first["jobId"] / "previous.md",
                 self.home / "cron/notepad" / (first["jobId"] + ".json")]
        for path in paths:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("keep history")
        self.change_job(model="retained-model", repeat={"times": None, "completed": 7})
        self.config["monitor"]["schedule"] = "every 10m"
        result = self.run_cli()
        self.assertEqual(result["status"], "updated")
        self.assertEqual(result["jobId"], first["jobId"])
        self.assertEqual(self.jobs()[0]["schedule"]["minutes"], 10)
        self.assertEqual(self.jobs()[0]["repeat"]["completed"], 7)
        self.assertEqual(self.jobs()[0]["model"], "retained-model")
        self.assertEqual(len(self.calls("create")), 1)
        self.assertEqual(self.calls("remove"), [])
        for path in paths:
            self.assertEqual(path.read_text(), "keep history")

    def test_disable_and_resume_only_the_bound_job(self):
        first = self.run_cli()
        other = {**self.jobs()[0], "id": "abcdef012345", "name": "other-job"}
        self.store.write_text(json.dumps([self.jobs()[0], other]))
        self.config["monitor"]["enabled"] = False
        self.assertEqual(self.run_cli()["status"], "paused")
        self.assertFalse(self.jobs()[0]["enabled"])
        self.assertEqual(self.jobs()[1], other)
        self.run_cli()
        self.assertEqual(len(self.calls("pause")), 1)
        self.config["monitor"].update(enabled=True, schedule="every 2h")
        resumed = self.run_cli()
        self.assertEqual(resumed["jobId"], first["jobId"])
        self.assertEqual(self.jobs()[0]["schedule"]["minutes"], 120)
        self.assertTrue(self.jobs()[0]["enabled"])
        self.assertEqual(self.jobs()[1], other)

    def test_disabled_first_start_does_not_create_a_job(self):
        self.config["monitor"]["enabled"] = False
        self.assertEqual(self.run_cli()["status"], "disabled")
        self.assertEqual(self.jobs(), [])
        self.assertFalse(self.binding.exists())

    def test_disabled_monitor_accepts_omitted_optional_go_fields(self):
        self.config["monitor"] = {"enabled": False, "timezone": "UTC", "deliver": "local"}
        self.assertEqual(self.run_cli()["status"], "disabled")
        self.assertEqual(self.jobs(), [])

    def test_enabled_local_monitor_accepts_omitted_slack_fields(self):
        self.config["monitor"].pop("slackChannel")
        self.config["monitor"].pop("slackSecretName")
        self.assertEqual(self.run_cli()["status"], "created")
        self.assertEqual(self.jobs()[0]["deliver"], "local")

    def test_disable_does_not_need_slack_credentials(self):
        self.slack()
        self.run_cli()
        self.environment.pop("SLACK_BOT_TOKEN")
        self.environment.pop("SLACK_APP_TOKEN")
        self.config["monitor"]["enabled"] = False
        self.assertEqual(self.run_cli()["status"], "paused")
        self.assertEqual(self.jobs()[0]["deliver"], "slack:C012EXAMPLE")

    def test_unknown_same_name_including_paused_case_variant_is_a_conflict(self):
        self.store.write_text(json.dumps([{
            "id": "abcdef012345", "name": "CetiAdmin-Monitor", "enabled": False,
        }]))
        before = self.store.read_bytes()
        for enabled in (True, False):
            self.config["monitor"]["enabled"] = enabled
            self.assertEqual(self.run_cli(success=False)["code"], "job_conflict")
        self.assertEqual(self.store.read_bytes(), before)
        self.assertFalse(self.binding.exists())
        self.assertEqual(self.calls("create"), [])

    def test_conflicting_same_name_is_not_removed_or_ignored_with_a_binding(self):
        self.run_cli()
        other = {**self.jobs()[0], "id": "abcdef012345"}
        self.store.write_text(json.dumps([self.jobs()[0], other]))
        before = self.store.read_bytes()
        self.assertEqual(self.run_cli(success=False)["code"], "job_conflict")
        self.assertEqual(self.store.read_bytes(), before)
        self.assertEqual(self.calls("remove"), [])

    def test_binding_identity_change_never_adopts_old_job(self):
        self.run_cli()
        before = self.binding.read_bytes(), self.store.read_bytes()
        self.config["instance"]["uid"] = "agent-uid-replacement"
        self.assertEqual(self.run_cli(success=False)["code"], "state_conflict")
        self.assertEqual((self.binding.read_bytes(), self.store.read_bytes()), before)

    def test_missing_bound_job_never_recreates_it(self):
        self.run_cli()
        self.store.write_text("[]")
        for enabled in (True, False):
            self.config["monitor"]["enabled"] = enabled
            self.assertEqual(self.run_cli(success=False)["code"], "job_missing")
        self.assertEqual(self.jobs(), [])
        self.assertEqual(len(self.calls("create")), 1)

    def test_renamed_known_job_is_not_taken_over(self):
        self.run_cli()
        self.change_job(name="user-renamed-job")
        before = self.store.read_bytes()
        self.assertEqual(self.run_cli(success=False)["code"], "job_conflict")
        self.assertEqual(self.store.read_bytes(), before)

    def test_uncertain_create_is_not_retried_even_when_no_job_is_visible(self):
        self.assertEqual(self.run_cli(fault="create_before_write", success=False)["code"], "create_uncertain")
        self.assertEqual(self.run_cli(success=False)["code"], "create_uncertain")
        self.assertEqual(self.jobs(), [])
        self.assertEqual(len(self.calls("create")), 1)

    def test_uncertain_create_after_write_preserves_paused_job_without_adoption(self):
        self.assertEqual(self.run_cli(fault="create_after_write", success=False)["code"], "create_uncertain")
        self.assertEqual(self.run_cli(success=False)["code"], "create_uncertain")
        self.assertEqual(len(self.jobs()), 1)
        self.assertFalse(self.jobs()[0]["enabled"])
        self.assertEqual(self.calls("resume"), [])

    def test_invalid_create_id_does_not_escape_state_or_duplicate(self):
        self.assertEqual(self.run_cli(fault="create_bad_id", success=False)["code"], "create_uncertain")
        self.assertEqual(self.run_cli(success=False)["code"], "create_uncertain")
        self.assertEqual(len(self.jobs()), 1)
        self.assertEqual(len(self.calls("create")), 1)

    def test_create_readback_collision_stays_paused(self):
        self.assertEqual(self.run_cli(fault="create_collision", success=False)["code"], "job_conflict")
        self.assertTrue(all(not job["enabled"] for job in self.jobs()))
        self.assertEqual(self.calls("resume"), [])

    def test_lost_readback_retains_returned_id_for_inspection(self):
        self.assertEqual(self.run_cli(fault="missing_readback", success=False)["code"], "job_missing")
        self.assertEqual(json.loads(self.binding.read_text())["jobId"], self.jobs()[0]["id"])
        self.assertFalse(self.jobs()[0]["enabled"])
        self.run_cli()
        self.assertEqual(len(self.calls("create")), 1)

    def test_unconfirmed_edit_is_an_error(self):
        self.run_cli()
        self.config["monitor"]["schedule"] = "every 10m"
        self.assertEqual(self.run_cli(fault="ignore_update", success=False)["code"], "readback_mismatch")
        self.assertEqual(len(self.calls("create")), 1)

    def test_unconfirmed_resume_is_an_error(self):
        self.assertEqual(self.run_cli(fault="ignore_resume", success=False)["code"], "readback_mismatch")
        self.assertFalse(self.jobs()[0]["enabled"])

    def test_schedule_readback_requires_a_native_next_run(self):
        self.assertEqual(self.run_cli(fault="no_next_run", success=False)["code"], "readback_mismatch")

    def test_native_error_is_safe_and_not_retried_in_the_same_invocation(self):
        self.run_cli()
        self.config["monitor"]["schedule"] = "every 10m"
        self.assertEqual(self.run_cli(fault="update_after_write", success=False)["code"], "native_error")
        self.run_cli()
        self.assertEqual(self.jobs()[0]["schedule"]["minutes"], 10)
        self.assertEqual(len(self.calls("create")), 1)

    def test_invalid_timezone_fails_before_native_import(self):
        self.config["monitor"]["timezone"] = "Not/A-Timezone"
        self.assertEqual(self.run_cli(success=False)["code"], "invalid_timezone")
        self.assertEqual(self.calls("import"), [])
        self.assertFalse(self.binding.exists())

    def test_timezone_change_uses_native_clock_and_does_not_edit_global_config(self):
        self.config["monitor"]["schedule"] = "0 9 * * *"
        first = self.run_cli(env={"HERMES_TIMEZONE": "Pacific/Honolulu"})
        self.config["monitor"]["timezone"] = "Asia/Singapore"
        second = self.run_cli()
        self.assertEqual(second["jobId"], first["jobId"])
        self.assertEqual(second["timezone"], "Asia/Singapore")
        self.assertTrue(second["nextRunAt"].endswith("+08:00"))
        self.assertEqual(self.calls("import")[-1]["timezone"], "Asia/Singapore")
        self.assertEqual(self.run_cli()["status"], "unchanged")

    def test_slack_destination_is_explicit_and_changes_in_place(self):
        self.slack()
        first = self.run_cli(env={"SLACK_HOME_CHANNEL": "CPREVIOUS"})
        self.assertEqual(self.jobs()[0]["deliver"], "slack:C012EXAMPLE")
        self.assertEqual(self.jobs()[0]["failure_deliver"], "slack:C012EXAMPLE")
        self.assertIsNone(self.jobs()[0]["origin"])
        self.config["monitor"]["slackChannel"] = "C098EXAMPLE"
        second = self.run_cli()
        self.assertEqual(second["jobId"], first["jobId"])
        self.assertEqual(self.jobs()[0]["deliver"], "slack:C098EXAMPLE")
        self.assertEqual(self.jobs()[0]["failure_deliver"], "slack:C098EXAMPLE")
        self.assertNotIn(PRIVATE_TEXT, self.binding.read_text() + self.store.read_text())

    def test_missing_slack_parameters_never_fall_back(self):
        for field in ("slackChannel", "slackSecretName"):
            with self.subTest(field=field):
                self.slack()
                self.config["monitor"][field] = ""
                self.assertEqual(self.run_cli(success=False)["code"], "invalid_slack")
                self.assertEqual(self.jobs(), [])
        self.assertEqual(self.calls("create"), [])

    def test_missing_slack_credentials_never_read_retained_env(self):
        self.slack()
        (self.home / ".env").write_text("SLACK_BOT_TOKEN=sk-dummy-retained\nSLACK_APP_TOKEN=xapp-retained\n")
        for field in ("SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"):
            with self.subTest(field=field):
                self.slack()
                self.environment.pop(field)
                self.assertEqual(self.run_cli(success=False)["code"], "slack_unavailable")
        self.assertEqual(self.calls("create"), [])

    def test_slack_rejects_channel_lists_or_implicit_targets(self):
        self.slack()
        for channel in ("", "local", "origin", "#general", "C012EXAMPLE,slack:COTHER", "C012EXAMPLE:123"):
            with self.subTest(channel=channel):
                self.config["monitor"]["slackChannel"] = channel
                self.assertEqual(self.run_cli(success=False)["code"], "invalid_slack")

    def test_invalid_config_and_schedule_do_not_echo_input(self):
        for field, value, code in (
            ("enabled", "true", "invalid_config"),
            ("deliver", "teams", "invalid_config"),
            ("token", PRIVATE_TEXT, "invalid_config"),
            ("schedule", PRIVATE_TEXT, "invalid_schedule"),
            ("schedule", "in 30m", "invalid_schedule"),
        ):
            with self.subTest(field=field, value=value):
                previous = dict(self.config["monitor"])
                self.config["monitor"][field] = value
                self.assertEqual(self.run_cli(success=False)["code"], code)
                self.config["monitor"] = previous
        self.assertEqual(self.calls("create"), [])

    def test_native_stdout_and_stderr_are_not_forwarded(self):
        self.run_cli(fault="noisy")
        self.assertEqual(self.run_cli(fault="list_failure", success=False)["code"], "native_error")

    def test_unrelated_home_files_and_native_database_are_not_overwritten(self):
        files = ("config.yaml", ".env", "SOUL.md", "USER.md", "sessions.db",
                 "cron/jobs.json", "skills/personal/SKILL.md", "cetiadmin/monitor-state.json")
        for name in files:
            path = self.home / name
            path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
            path.write_text("unrelated synthetic content: " + name)
        before = {name: (self.home / name).read_bytes() for name in files}
        self.run_cli()
        self.config["monitor"]["schedule"] = "every 10m"
        self.run_cli()
        self.config["monitor"]["enabled"] = False
        self.run_cli()
        self.assertEqual({name: (self.home / name).read_bytes() for name in files}, before)

    def test_unrecognized_binding_file_is_not_overwritten(self):
        self.state_dir.mkdir(mode=0o700)
        self.binding.write_text('{"unrelated": true}')
        self.binding.chmod(0o600)
        before = self.binding.read_bytes()
        self.assertEqual(self.run_cli(success=False)["code"], "state_conflict")
        self.assertEqual(self.binding.read_bytes(), before)
        self.assertEqual(self.calls("create"), [])

    def test_symlink_state_directory_and_binding_are_rejected(self):
        outside = self.directory / "unrelated"
        outside.mkdir(mode=0o700)
        self.state_dir.symlink_to(outside, target_is_directory=True)
        self.assertEqual(self.run_cli(success=False)["code"], "unsafe_state")
        self.assertEqual(list(outside.iterdir()), [])
        self.state_dir.unlink()
        self.state_dir.mkdir(mode=0o700)
        target = outside / "keep.json"
        target.write_text("keep")
        self.binding.symlink_to(target)
        self.assertEqual(self.run_cli(success=False)["code"], "unsafe_state")
        self.assertEqual(target.read_text(), "keep")

    def test_private_state_override_has_bounded_writes(self):
        override = self.directory / "private" / "monitor"
        self.run_cli(extra=("--state-dir", str(override)))
        self.assertTrue((override / "cron-binding.json").is_file())
        self.assertFalse(self.binding.exists())
        self.assertEqual(sorted(path.name for path in override.iterdir()), ["cron-binding.json"])

    def test_shared_state_lock_fails_without_waiting_or_creating(self):
        self.state_dir.mkdir(mode=0o700)
        descriptor = os.open(self.state_dir, os.O_RDONLY | os.O_DIRECTORY)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.assertEqual(self.run_cli(success=False)["code"], "sync_busy")
        finally:
            os.close(descriptor)
        self.assertEqual(self.calls("create"), [])

    def test_runtime_locations_and_native_pin_are_required(self):
        for field in ("HERMES_HOME", "HERMES_INSTALL_DIR"):
            with self.subTest(field=field):
                value = self.environment.pop(field)
                self.assertEqual(self.run_cli(success=False)["code"], "invalid_runtime")
                self.environment[field] = value
        (self.install / "install-stamp.json").write_text("not-the-pinned-version")
        self.assertEqual(self.run_cli(success=False)["code"], "native_version_mismatch")
        self.assertEqual(self.calls("import"), [])


if __name__ == "__main__":
    unittest.main()
