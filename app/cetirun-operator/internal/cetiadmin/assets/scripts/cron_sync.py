#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bind one CetiAdmin monitor to the pinned Hermes native cron API.

Run before the Hermes scheduler starts. The runtime must also export the
returned timezone as HERMES_TIMEZONE for that scheduler and stop on failure.
This CLI changes only its own process environment and private binding file;
cron.jobs owns all native job storage, output, and notepad handling.
"""

import argparse
from contextlib import contextmanager
from datetime import datetime
import fcntl
import importlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError


HERMES_SHA = "b200f5bc60e31588ba2cb60e288029c4a54ba5fa"
JOB_NAME = "cetiadmin-monitor"
JOB_ID = re.compile(r"[0-9a-f]{12}")
DNS_LABEL = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?")
UID = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}")
SLACK_CHANNEL = re.compile(r"[CG][A-Z0-9]{8,31}")
MAX_JSON_BYTES = 65536
MONITOR_FIELDS = {"enabled", "schedule", "timezone", "deliver", "slackChannel", "slackSecretName"}
BINDING_FIELDS = {"version", "instance", "hermesHome", "phase", "jobId", "timezone"}


class SyncError(Exception):
    """A fixed, public error code, never a native exception or input value."""


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise SyncError("invalid_arguments")


def json_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate field")
        value[key] = item
    return value


def read_json(path, code):
    try:
        with path.open("rb") as stream:
            raw = stream.read(MAX_JSON_BYTES + 1)
        if len(raw) > MAX_JSON_BYTES:
            raise ValueError("too large")
        value = json.loads(raw, object_pairs_hook=json_object)
        if not isinstance(value, dict):
            raise ValueError("expected object")
        return value
    except (OSError, ValueError, UnicodeError):
        raise SyncError(code) from None


def read_config(path):
    value = read_json(path, "invalid_config")
    if set(value) != {"instance", "monitor"}:
        raise SyncError("invalid_config")
    instance, monitor = value["instance"], value["monitor"]
    if not isinstance(instance, dict) or set(instance) != {"name", "namespace", "uid"}:
        raise SyncError("invalid_config")
    for key, pattern in (("name", DNS_LABEL), ("namespace", DNS_LABEL), ("uid", UID)):
        if not isinstance(instance[key], str) or not pattern.fullmatch(instance[key]):
            raise SyncError("invalid_config")
    if (not isinstance(monitor, dict) or set(monitor) - MONITOR_FIELDS
            or not {"enabled", "timezone", "deliver"}.issubset(monitor)):
        raise SyncError("invalid_config")
    for key in ("schedule", "slackChannel", "slackSecretName"):
        monitor.setdefault(key, "")
    if not isinstance(monitor["enabled"], bool):
        raise SyncError("invalid_config")
    for key in MONITOR_FIELDS - {"enabled"}:
        text = monitor[key]
        if not isinstance(text, str) or len(text) > 256 or text != text.strip():
            raise SyncError("invalid_config")
        if any(ord(char) < 32 for char in text):
            raise SyncError("invalid_config")
    if ((monitor["enabled"] and not monitor["schedule"])
            or monitor["deliver"] not in {"local", "slack"}):
        raise SyncError("invalid_config")
    try:
        ZoneInfo(monitor["timezone"])
    except (ValueError, ZoneInfoNotFoundError):
        raise SyncError("invalid_timezone") from None
    if monitor["enabled"] and monitor["deliver"] == "slack":
        secret = monitor["slackSecretName"]
        if (not SLACK_CHANNEL.fullmatch(monitor["slackChannel"]) or not secret
                or len(secret) > 253 or not all(DNS_LABEL.fullmatch(part) for part in secret.split("."))):
            raise SyncError("invalid_slack")
        if not all(os.environ.get(key, "").strip() for key in ("SLACK_BOT_TOKEN", "SLACK_APP_TOKEN")):
            raise SyncError("slack_unavailable")
    return value


def runtime_directory(name):
    raw = os.environ.get(name, "")
    path = Path(raw)
    if not raw or not path.is_absolute() or not path.is_dir() or path == Path("/"):
        raise SyncError("invalid_runtime")
    return path.resolve()


def prepare_state_directory(path, home, install):
    if (not path.is_absolute() or ".." in path.parts or path == home or path in home.parents
            or path.is_relative_to(install)):
        raise SyncError("unsafe_state")
    for directory in (*reversed(path.parents), path):
        if directory.is_symlink():
            raise SyncError("unsafe_state")
        directory.mkdir(mode=0o700, exist_ok=True)
    info = path.stat()
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) & 0o077:
        raise SyncError("unsafe_state")
    return path


@contextmanager
def state_lock(directory):
    descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise SyncError("sync_busy") from None
        yield descriptor
    finally:
        os.close(descriptor)


def read_binding(path, instance, home):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return None
    if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) & 0o077):
        raise SyncError("unsafe_state")
    saved = read_json(path, "state_conflict")
    if (set(saved) != BINDING_FIELDS or type(saved["version"]) is not int or saved["version"] != 1
            or saved["instance"] != instance or saved["hermesHome"] != str(home)
            or saved["phase"] not in {"creating", "bound"} or not isinstance(saved["timezone"], str)):
        raise SyncError("state_conflict")
    if saved["phase"] == "creating":
        raise SyncError("create_uncertain")
    if not isinstance(saved["jobId"], str) or not JOB_ID.fullmatch(saved["jobId"]):
        raise SyncError("state_conflict")
    return saved


def save_binding(path, binding, directory_fd):
    descriptor, name = tempfile.mkstemp(prefix=".cron-binding-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(binding, stream, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        os.fsync(directory_fd)
    finally:
        if os.path.exists(name):
            os.unlink(name)


@contextmanager
def quiet_native_output():
    """Do not forward upstream logging, including writes to file descriptors."""
    sys.stdout.flush()
    sys.stderr.flush()
    saved = os.dup(1), os.dup(2)
    try:
        with open(os.devnull, "w") as sink:
            os.dup2(sink.fileno(), 1)
            os.dup2(sink.fileno(), 2)
            try:
                yield
            finally:
                sys.stdout.flush()
                sys.stderr.flush()
    finally:
        for destination, descriptor in zip((1, 2), saved):
            os.dup2(descriptor, destination)
            os.close(descriptor)


def native_call(function, *args, error="native_error", **kwargs):
    try:
        return function(*args, **kwargs)
    except (Exception, SystemExit):
        raise SyncError(error) from None


def load_native(install, timezone):
    try:
        stamp = json.loads((install / "install-stamp.json").read_text(encoding="utf-8"))
        if (stamp.get("commit") != HERMES_SHA or stamp.get("baseVersion") != "0.22.7"
                or stamp.get("updateMechanism") != "external" or stamp.get("distribution") != "docker"):
            raise ValueError()
    except (OSError, UnicodeError, ValueError):
        raise SyncError("native_version_mismatch") from None
    os.environ["HERMES_TIMEZONE"] = timezone
    sys.dont_write_bytecode = True
    sys.path.insert(0, str(install))
    identity = native_call(importlib.import_module, "hermes_cli.version_info")
    version = native_call(identity.get_version_info)
    if (not Path(identity.__file__).resolve().is_relative_to(install)
            or version.base_version != "0.22.7" or version.commit != HERMES_SHA or version.source != "docker"):
        raise SyncError("native_version_mismatch")
    jobs = native_call(importlib.import_module, "cron.jobs")
    clock = native_call(importlib.import_module, "hermes_time")
    if any(not Path(module.__file__).resolve().is_relative_to(install) for module in (jobs, clock)):
        raise SyncError("native_version_mismatch")
    if getattr(native_call(clock.get_timezone), "key", None) != timezone:
        raise SyncError("invalid_timezone")
    return jobs


def known_job(native, job_id):
    jobs = native_call(native.list_jobs, include_disabled=True)
    if not isinstance(jobs, list) or any(not isinstance(job, dict) for job in jobs):
        raise SyncError("native_error")
    same_name = [job for job in jobs if str(job.get("name", "")).casefold() == JOB_NAME]
    if any(job.get("id") != job_id for job in same_name) or len(same_name) > 1:
        raise SyncError("job_conflict")
    if job_id is None:
        return None
    job = native_call(native.get_job, job_id)
    if job is None:
        raise SyncError("job_missing")
    if (not isinstance(job, dict) or job.get("id") != job_id or job.get("name") != JOB_NAME
            or sum(item.get("id") == job_id for item in jobs) != 1):
        raise SyncError("job_conflict")
    return job


def desired_job(config, state_dir):
    prompt_path = Path(__file__).resolve().parents[1] / "cron/cetirun-monitor-cron.md"
    try:
        prompt = prompt_path.read_text(encoding="utf-8").strip()
    except (OSError, UnicodeError):
        raise SyncError("missing_prompt") from None
    if not prompt:
        raise SyncError("missing_prompt")
    context = {**config, "stateDirectory": str(state_dir)}
    prompt += "\n\n## Bound monitor input\n\n```json\n" + json.dumps(context, sort_keys=True) + "\n```"
    monitor = config["monitor"]
    deliver = "slack:" + monitor["slackChannel"] if monitor["deliver"] == "slack" else "local"
    return {"name": JOB_NAME, "prompt": prompt, "skills": ["cetiadmin"], "skill": "cetiadmin",
            "deliver": deliver, "failure_deliver": deliver, "origin": None, "attach_to_session": False}


def verify_definition(job, desired, schedule):
    if job.get("schedule") != schedule or any(job.get(key) != value for key, value in desired.items()):
        raise SyncError("readback_mismatch")


def sync(config, home, install, state_dir):
    monitor = config["monitor"]
    path = state_dir / "cron-binding.json"
    with state_lock(state_dir) as directory_fd:
        binding = read_binding(path, config["instance"], home)
        native = load_native(install, monitor["timezone"])
        job = known_job(native, binding["jobId"] if binding else None)
        result = {"status": "disabled", "timezone": monitor["timezone"]}
        if not monitor["enabled"]:
            if job is not None:
                result.update(status="paused", jobId=binding["jobId"])
                if job.get("enabled") is not False or job.get("state") != "paused":
                    native_call(native.pause_job, binding["jobId"], reason="CetiAdmin monitor disabled")
                    job = known_job(native, binding["jobId"])
                if job.get("enabled") is not False or job.get("state") != "paused":
                    raise SyncError("readback_mismatch")
            return result

        schedule = native_call(native.parse_schedule, monitor["schedule"], error="invalid_schedule")
        if not isinstance(schedule, dict) or schedule.get("kind") not in {"interval", "cron"}:
            raise SyncError("invalid_schedule")
        desired = desired_job(config, state_dir)
        created = binding is None
        if created:
            binding = {"version": 1, "instance": config["instance"], "hermesHome": str(home),
                       "phase": "creating", "jobId": None, "timezone": monitor["timezone"]}
            # A crash after native create must never authorize a second create.
            save_binding(path, binding, directory_fd)
            new_job = native_call(native.create_job, schedule=monitor["schedule"], **desired,
                                  paused=True, paused_reason="CetiAdmin binding awaiting readback",
                                  error="create_uncertain")
            job_id = new_job.get("id") if isinstance(new_job, dict) else None
            if not isinstance(job_id, str) or not JOB_ID.fullmatch(job_id):
                raise SyncError("create_uncertain")
            binding.update(phase="bound", jobId=job_id)
            save_binding(path, binding, directory_fd)
            job = known_job(native, job_id)
            verify_definition(job, desired, schedule)
            if job.get("enabled") is not False or job.get("state") != "paused":
                raise SyncError("readback_mismatch")

        if (any(job.get(key) for key in ("script", "no_agent", "monitor_script", "monitor_url"))
                or not isinstance(job.get("repeat"), dict) or job["repeat"].get("times") is not None
                or not isinstance(job.get("enabled"), bool) or job.get("state") not in {"scheduled", "paused"}):
            raise SyncError("job_conflict")
        updates = {key: value for key, value in desired.items() if job.get(key) != value}
        timezone_changed = binding["timezone"] != monitor["timezone"]
        if job.get("schedule") != schedule or timezone_changed:
            updates["schedule"] = monitor["schedule"]
            if job.get("state") == "paused":
                updates["next_run_at"] = None
        changed = bool(updates)
        if updates:
            native_call(native.update_job, binding["jobId"], updates)
            job = known_job(native, binding["jobId"])
        verify_definition(job, desired, schedule)
        if not job["enabled"] or job.get("state") == "paused" or job.get("paused_at"):
            native_call(native.resume_job, binding["jobId"])
            job = known_job(native, binding["jobId"])
            changed = True
        verify_definition(job, desired, schedule)
        if job.get("enabled") is not True or job.get("state") != "scheduled" or job.get("paused_at"):
            raise SyncError("readback_mismatch")
        try:
            next_run = datetime.fromisoformat(job["next_run_at"])
            if next_run.tzinfo is None:
                raise ValueError("missing timezone")
        except (KeyError, TypeError, ValueError):
            raise SyncError("readback_mismatch") from None
        if timezone_changed:
            binding["timezone"] = monitor["timezone"]
            save_binding(path, binding, directory_fd)
        result.update(status="created" if created else "updated" if changed else "unchanged",
                      jobId=binding["jobId"], nextRunAt=next_run.isoformat())
        return result


def main():
    parser = Parser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True,
                        help="JSON file with instance {name, namespace, uid} and the six monitor fields")
    parser.add_argument("--state-dir", type=Path,
                        help="private absolute directory (default: HERMES_HOME/cetiadmin)")
    try:
        args = parser.parse_args()
        config = read_config(args.config)
        home = runtime_directory("HERMES_HOME")
        install = runtime_directory("HERMES_INSTALL_DIR")
        state_dir = prepare_state_directory(args.state_dir or home / "cetiadmin", home, install)
        with quiet_native_output():
            result = sync(config, home, install, state_dir)
    except SyncError as error:
        print(json.dumps({"status": "error", "code": str(error)}))
        return 1
    except (Exception, KeyboardInterrupt):
        print(json.dumps({"status": "error", "code": "sync_failed"}))
        return 1
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
