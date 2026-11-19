#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Repair one Helm 3.22 pending-install revision under a current Held handoff.

Uses context cetiadmin and requires CETIADMIN_AGENT_UID for self-exclusion.
The controller must record status.namespace/status.helmReleaseName before Held.
Requires spec.helm.maintenance requestID/expectedRevision and a matching current
status.helmMaintenance requestID/observedGeneration/revision/phase=Held.
Each request gets one private backup and at most one patch attempt. Replays only
read back evidence; they never resubmit. This does not release maintenance or
resume an Agent. No Helm executable or third-party Python packages are used.
"""

import argparse
import base64
import binascii
import copy
from decimal import Decimal
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import zlib


MAX_ENCODED_BYTES = 2 * 1024 * 1024
MAX_PAYLOAD_BYTES = 8 * 1024 * 1024
MAX_REPLY_BYTES = 12 * 1024 * 1024
MAX_HISTORY = 4096
API = "cetirun.microsoft.com/v1alpha1"
ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}\Z")
NAME = re.compile(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)*\Z")
WHITESPACE = re.compile(r"[ \t\r\n]*")
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


class RepairError(Exception):
    """Only fixed, non-sensitive reason codes cross the CLI boundary."""


def require(condition, reason):
    if not condition:
        raise RepairError(reason)


def object_pairs(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate_json_key")
        result[key] = value
    return result


def invalid_constant(_):
    raise RepairError("invalid_json_number")


JSON = json.JSONDecoder(object_pairs_hook=object_pairs, parse_float=Decimal,
                        parse_constant=invalid_constant)


def read_json(raw):
    try:
        return JSON.decode(raw.decode("utf-8"))
    except (UnicodeError, ValueError, RecursionError):
        raise RepairError("invalid_json") from None


def mapping(value):
    require(isinstance(value, dict), "missing_object")
    return value


def identifier(value):
    return isinstance(value, str) and ID.fullmatch(value) is not None


def positive(value):
    return type(value) is int and value > 0


def metadata(obj, kind, api_version):
    obj = mapping(obj)
    require(obj.get("kind") == kind and obj.get("apiVersion") == api_version, "wrong_object_type")
    meta = mapping(obj.get("metadata"))
    require(identifier(meta.get("uid")) and identifier(meta.get("resourceVersion")), "missing_object_identity")
    require(isinstance(meta.get("name"), str) and len(meta["name"]) <= 253
            and NAME.fullmatch(meta["name"]), "missing_object_name")
    require(not meta.get("deletionTimestamp"), "object_deleting")
    return meta


def kubectl(arguments, patch=None):
    try:
        result = subprocess.run(
            ["kubectl", "--context=cetiadmin", "--request-timeout=15s", *arguments],
            input=patch, stdout=subprocess.PIPE if patch is None else subprocess.DEVNULL,
            stderr=subprocess.DEVNULL, timeout=20, check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise RepairError("kubectl_unavailable_or_timed_out") from None
    require(result.returncode == 0, "kubectl_request_failed")
    data = result.stdout or b""
    require(len(data) <= MAX_REPLY_BYTES, "kubectl_reply_too_large")
    return data


def get_json(resource, name=None, namespace=None):
    args = ["get", resource]
    if name is not None:
        args.append(name)
    if namespace is not None:
        args.extend(["--namespace", namespace])
    if resource == "secret":
        args.append("--show-managed-fields=true")
    raw = kubectl([*args, "-o=json"])
    return read_json(raw), raw


def check_target(args):
    agent, _ = get_json("agents.cetirun.microsoft.com", args.agent)
    meta = metadata(agent, "Agent", API)
    spec, status = mapping(agent.get("spec")), mapping(agent.get("status"))
    require(meta.get("name") == args.agent and meta["uid"] == args.agent_uid, "agent_identity_mismatch")
    require(meta["uid"] != os.environ["CETIADMIN_AGENT_UID"], "self_repair_forbidden")
    runtime = mapping(spec.get("agent")).get("type")
    require(runtime in ("openclaw", "hermes", "custom"), "unsupported_agent_runtime")
    require(spec.get("sandboxRef") in (None, "") and status.get("sandbox") is None, "sandbox_target_forbidden")
    require(spec.get("suspended") is True, "agent_not_suspended")
    require(status.get("namespace") == args.namespace and status.get("helmReleaseName") == args.release,
            "recorded_target_mismatch")
    maintenance = mapping(mapping(spec.get("helm")).get("maintenance"))
    held = mapping(status.get("helmMaintenance"))
    generation = meta.get("generation")
    require(positive(generation) and positive(held.get("observedGeneration"))
            and held["observedGeneration"] == generation and held.get("phase") == "Held"
            and held.get("requestID") == args.request_id == maintenance.get("requestID")
            and positive(held.get("revision")) and positive(maintenance.get("expectedRevision"))
            and held["revision"] == args.revision == maintenance["expectedRevision"], "held_handoff_mismatch")
    migration = spec.get("plannedMigration")
    observed = status.get("plannedMigration")
    if migration is not None or observed is not None:
        observed = mapping(observed)
        require(observed.get("phase") == "Completed" and isinstance(migration, dict)
                and all(observed.get(key) == value for key, value in migration.items()), "migration_not_complete")
    namespace, _ = get_json("namespace", args.namespace)
    ns = metadata(namespace, "Namespace", "v1")
    require(ns.get("name") == args.namespace and ns["uid"] == args.namespace_uid, "namespace_identity_mismatch")
    require(mapping(namespace.get("status")).get("phase") == "Active", "namespace_not_active")
    require(mapping(ns.get("labels", {})).get("cetirun.microsoft.com/sandbox-protected") not in ("true", True),
            "sandbox_namespace_forbidden")
    owners = ns.get("ownerReferences", [])
    expected_owner = {"apiVersion": API, "kind": "Agent", "name": args.agent, "uid": args.agent_uid}
    owned = (isinstance(owners, list) and len(owners) == 1 and isinstance(owners[0], dict)
             and all(owners[0].get(key) == value for key, value in expected_owner.items()))
    annotation = mapping(ns.get("annotations", {})).get("cetirun.microsoft.com/agent-uid")
    if runtime == "custom":
        require(annotation == args.agent_uid and (owners == [] or owned), "custom_namespace_unowned")
    else:
        require(owned and annotation in (None, args.agent_uid), "namespace_unowned")
    return {"agent": args.agent, "agentUID": args.agent_uid, "agentGeneration": generation,
            "namespace": args.namespace, "namespaceUID": args.namespace_uid,
            "release": args.release, "revision": args.revision, "requestID": args.request_id, "runtime": runtime}


def check_stopped(args):
    for resource, kind in (("pods", "Pod"), ("jobs.batch", "Job")):
        objects, _ = get_json(resource, namespace=args.namespace)
        objects = mapping(objects)
        require(objects.get("kind") == kind + "List" and isinstance(objects.get("items"), list)
                and not mapping(objects.get("metadata", {})).get("continue"), "incomplete_writer_inventory")
        for obj in objects["items"]:
            meta = metadata(obj, kind, "v1" if kind == "Pod" else "batch/v1")
            require(meta.get("namespace") == args.namespace, "writer_namespace_mismatch")
            status = mapping(obj.get("status"))
            if kind == "Pod":
                require(status.get("phase") in ("Succeeded", "Failed"), "writer_pod_not_stopped")
                spec = mapping(obj.get("spec"))
                for containers, states in (("containers", "containerStatuses"), ("initContainers", "initContainerStatuses"),
                                           ("ephemeralContainers", "ephemeralContainerStatuses")):
                    declared, observed = spec.get(containers, []), status.get(states, [])
                    require(isinstance(declared, list) and isinstance(observed, list), "unknown_container_state")
                    names = [mapping(item).get("name") for item in declared]
                    require(all(identifier(name) for name in names) and len(set(names)) == len(names), "unknown_container_state")
                    stopped = set()
                    for item in observed:
                        item = mapping(item)
                        state = mapping(item.get("state"))
                        require(set(state) == {"terminated"} and type(mapping(state["terminated"]).get("exitCode")) is int
                                and item.get("name") in names and item["name"] not in stopped, "unknown_container_state")
                        stopped.add(item["name"])
                    require(set(names) == stopped, "unknown_container_state")
                require(bool(spec.get("containers")), "unknown_container_state")
            else:
                conditions = status.get("conditions", [])
                active, terminating = status.get("active", 0), status.get("terminating", 0)
                require(isinstance(conditions, list) and type(active) is int and active == 0
                        and type(terminating) is int and terminating == 0
                        and any(mapping(item).get("type") in ("Complete", "Failed") and item.get("status") == "True"
                                for item in conditions), "hook_or_job_not_stopped")


def latest_revision(args):
    raw = kubectl(["get", "secrets", "--namespace", args.namespace, "-o=" + HISTORY_FORMAT])
    try:
        rows = raw.decode("utf-8").splitlines()
    except UnicodeError:
        raise RepairError("invalid_revision_metadata") from None
    require(len(rows) <= MAX_HISTORY, "revision_inventory_too_large")
    revisions = {}
    prefix = f"sh.helm.release.v1.{args.release}.v"
    for row in rows:
        fields = row.split("\t")
        require(len(fields) == len(HISTORY_FIELDS), "invalid_revision_metadata")
        name, ns, uid, rv, kind, owner, release, version, status, deleting = fields
        if not name.startswith(prefix) and release != args.release:
            continue
        require(kind == "helm.sh/release.v1" and owner == "helm" and release == args.release
                and ns == args.namespace and identifier(uid) and identifier(rv) and not deleting
                and re.fullmatch(r"[1-9][0-9]{0,9}", version), "invalid_revision_metadata")
        revision = int(version)
        require(name == prefix + version and revision not in revisions, "ambiguous_revision_history")
        require(status in ("pending-install", "failed"), "successful_or_uncertain_history")
        revisions[revision] = (uid, rv, status)
    require(revisions and max(revisions) == args.revision and len(revisions) == args.revision
            and min(revisions) == 1, "latest_revision_or_history_gap")
    require(all(value[2] == "failed" for revision, value in revisions.items() if revision != args.revision),
            "unfinished_earlier_revision")
    return revisions[args.revision]


def decode_payload(encoded):
    require(isinstance(encoded, str) and len(encoded) <= MAX_ENCODED_BYTES, "release_encoding_too_large")
    try:
        data = encoded.encode("ascii")
        for _ in range(2):
            decoded = base64.b64decode(data, validate=True)
            require(base64.b64encode(decoded) == data, "noncanonical_base64")
            data = decoded
        require(data.startswith(b"\x1f\x8b\x08"), "release_not_gzip")
        decompressor = zlib.decompressobj(16 + zlib.MAX_WBITS)
        raw = decompressor.decompress(data, MAX_PAYLOAD_BYTES + 1)
        require(len(raw) <= MAX_PAYLOAD_BYTES, "release_payload_too_large")
        require(decompressor.eof and not decompressor.unused_data and not decompressor.unconsumed_tail,
                "incomplete_or_trailing_gzip")
    except (UnicodeError, binascii.Error, zlib.error):
        raise RepairError("invalid_release_encoding") from None
    return mapping(read_json(raw)), raw


def check_secret(secret, args, runtime, allowed_status):
    meta = metadata(secret, "Secret", "v1")
    labels = mapping(meta.get("labels"))
    require(secret.get("type") == "helm.sh/release.v1" and secret.get("immutable") is not True
            and meta.get("name") == args.secret_name and meta.get("namespace") == args.namespace
            and labels.get("owner") == "helm" and labels.get("name") == args.release
            and labels.get("version") == str(args.revision) and labels.get("status") in allowed_status,
            "release_secret_mismatch")
    payload, raw = decode_payload(mapping(secret.get("data")).get("release"))
    require(payload.get("name") == args.release and payload.get("namespace") == args.namespace
            and positive(payload.get("version")) and payload["version"] == args.revision
            and mapping(payload.get("info")).get("status") == labels["status"], "release_payload_mismatch")
    if runtime == "custom":
        binding = mapping(mapping(mapping(payload.get("config")).get("runtime")).get("bindings"))
        require(binding.get("agentName") == args.agent and binding.get("agentUID") == args.agent_uid
                and mapping(binding.get("namespace")).get("uid") == args.namespace_uid
                and binding["namespace"].get("name") == args.namespace, "custom_release_unowned")
    return raw


def snapshot(args):
    context = check_target(args)
    check_stopped(args)
    latest = latest_revision(args)
    secret, raw = get_json("secret", args.secret_name, args.namespace)
    check_secret(secret, args, context["runtime"], ("pending-install", "failed"))
    meta = secret["metadata"]
    require(latest == (meta["uid"], meta["resourceVersion"], meta["labels"]["status"]), "revision_changed_during_read")
    return context, secret, raw


def member_span(text, start, wanted):
    # JSON was already validated. Locate just two direct members without
    # reserializing config numbers, unknown fields, manifests or nested statuses.
    position = start + 1
    while True:
        position = WHITESPACE.match(text, position).end()
        key, position = JSON.raw_decode(text, position)
        position = WHITESPACE.match(text, position).end() + 1
        position = WHITESPACE.match(text, position).end()
        _, end = JSON.raw_decode(text, position)
        if key == wanted:
            return position, end
        position = WHITESPACE.match(text, end).end() + 1


def failed_payload(raw):
    text = raw.decode("utf-8")
    root = len(text) - len(text.lstrip(" \t\r\n"))
    info, _ = member_span(text, root, "info")
    start, end = member_span(text, info, "status")
    return (text[:start] + '"failed"' + text[end:]).encode("utf-8")


def check_backup_path(base):
    require(base.is_absolute() and str(base) != "/", "backup_path_not_absolute")
    for directory in (base, *base.parents):
        try:
            info = directory.lstat()
        except FileNotFoundError:
            require(directory == base, "backup_parent_missing")
            continue
        require(stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode), "backup_path_not_directory")


def recovery_record(context, original, raw):
    return {"schemaVersion": 1, "context": context, "secretUID": original["metadata"]["uid"],
            "resourceVersion": original["metadata"]["resourceVersion"], "originalSHA256": hashlib.sha256(raw).hexdigest()}


def load_backup(recovery):
    descriptor = os.open(recovery, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(descriptor)
        require(stat.S_IMODE(info.st_mode) == 0o700 and info.st_uid == os.geteuid(), "unsafe_recovery_directory")
        require(set(os.listdir(descriptor)) == {"original-secret.json", "request.json"}, "incomplete_or_foreign_backup")
        contents = []
        for name, limit in (("original-secret.json", MAX_REPLY_BYTES), ("request.json", 16384)):
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=descriptor)
            with os.fdopen(fd, "rb") as source:
                info = os.fstat(source.fileno())
                require(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600
                        and info.st_uid == os.geteuid() and info.st_size <= limit, "unsafe_backup_file")
                data = source.read(limit + 1)
                require(len(data) <= limit, "backup_file_too_large")
                contents.append(data)
        return contents[0], read_json(contents[1])
    finally:
        os.close(descriptor)


def save_backup(base, recovery, original_raw, record):
    check_backup_path(base)
    if not base.exists():
        base.mkdir(mode=0o700)
        base.chmod(0o700)
    parent = os.open(base, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.mkdir(recovery.name, 0o700, dir_fd=parent)
        directory = os.open(recovery.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
        try:
            os.fchmod(directory, 0o700)
            for name, data in (("original-secret.json", original_raw),
                               ("request.json", json.dumps(record, sort_keys=True).encode() + b"\n")):
                fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
                with os.fdopen(fd, "wb") as target:
                    os.fchmod(target.fileno(), 0o600)
                    target.write(data)
                    target.flush()
                    os.fsync(target.fileno())
            os.fsync(directory)
        finally:
            os.close(directory)
        os.fsync(parent)
    finally:
        os.close(parent)
    saved, receipt = load_backup(recovery)
    require(saved == original_raw and receipt == record, "backup_readback_mismatch")


def matches_repair(original, current, expected_raw, args, runtime):
    actual_raw = check_secret(current, args, runtime, ("failed",))
    before, after = copy.deepcopy(original), copy.deepcopy(current)
    require(before["metadata"]["uid"] == after["metadata"]["uid"]
            and before["metadata"]["resourceVersion"] != after["metadata"]["resourceVersion"], "repair_identity_not_observed")
    for obj in (before, after):
        obj["metadata"].pop("resourceVersion", None)
        obj["metadata"].pop("managedFields", None)
        obj["data"].pop("release")
    before["metadata"]["labels"]["status"] = "failed"
    require(before == after and actual_raw == expected_raw, "repair_readback_mismatch")


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise RepairError("invalid_arguments")


def arguments(argv):
    parser = Parser(description=__doc__)
    for option in ("agent", "agent-uid", "namespace", "namespace-uid", "release", "revision", "request-id", "backup-dir"):
        parser.add_argument("--" + option, required=True)
    args = parser.parse_args(argv)
    for value, limit in ((args.agent, 253), (args.namespace, 63), (args.release, 53)):
        require(len(value) <= limit and NAME.fullmatch(value), "invalid_target_name")
    require("." not in args.namespace, "invalid_namespace_name")
    require(all(identifier(value) for value in (args.agent_uid, args.namespace_uid, args.request_id)), "invalid_identity_argument")
    require(re.fullmatch(r"[1-9][0-9]{0,9}", args.revision), "invalid_revision")
    args.revision = int(args.revision)
    require(args.revision <= MAX_HISTORY, "revision_inventory_too_large")
    require(identifier(os.environ.get("CETIADMIN_AGENT_UID")), "self_identity_required")
    require(os.path.isabs(args.backup_dir) and os.path.normpath(args.backup_dir) == args.backup_dir,
            "backup_path_not_absolute_and_clean")
    args.backup_dir = Path(args.backup_dir)
    args.secret_name = f"sh.helm.release.v1.{args.release}.v{args.revision}"
    return args


def main(argv=None):
    output = {"status": "refused"}
    try:
        args = arguments(argv)
        output.update(requestID=args.request_id, agentUID=args.agent_uid, namespaceUID=args.namespace_uid,
                      release=args.release, revision=args.revision)
        check_backup_path(args.backup_dir)
        recovery = args.backup_dir / ("repair-" + args.request_id)
        existing = os.path.lexists(recovery)
        context, current, original_raw = snapshot(args)
        if existing:
            original_raw, record = load_backup(recovery)
            original = read_json(original_raw)
            raw = check_secret(original, args, context["runtime"], ("pending-install",))
            require(record == recovery_record(context, original, original_raw), "backup_request_mismatch")
            output.update(status="unknown", backupFile=str(recovery / "original-secret.json"))
            matches_repair(original, current, failed_payload(raw), args, context["runtime"])
            output["status"] = "already-repaired"
        else:
            original = current
            raw = check_secret(original, args, context["runtime"], ("pending-install",))
            expected = failed_payload(raw)
            save_backup(args.backup_dir, recovery, original_raw, recovery_record(context, original, original_raw))
            output["backupFile"] = str(recovery / "original-secret.json")
            fresh_context, fresh, _ = snapshot(args)
            require(fresh_context == context and fresh == original, "target_changed_before_patch")
            encoded = base64.b64encode(base64.b64encode(gzip.compress(expected, compresslevel=9, mtime=0))).decode()
            require(len(encoded) <= MAX_ENCODED_BYTES, "repaired_encoding_too_large")
            patch = [
                {"op": "test", "path": "/metadata/uid", "value": original["metadata"]["uid"]},
                {"op": "test", "path": "/metadata/resourceVersion", "value": original["metadata"]["resourceVersion"]},
                {"op": "replace", "path": "/metadata/labels/status", "value": "failed"},
                {"op": "replace", "path": "/data/release", "value": encoded},
            ]
            output["status"] = "unknown"
            try:
                kubectl(["patch", "secret", args.secret_name, "--namespace", args.namespace,
                         "--type=json", "--patch-file=/dev/stdin", "-o=name"], json.dumps(patch).encode())
            except RepairError:
                pass  # A failed acknowledgement is resolved only by an exact readback.
            current, _ = get_json("secret", args.secret_name, args.namespace)
            matches_repair(original, current, expected, args, context["runtime"])
            output["status"] = "repaired"
        output.update(secretUID=current["metadata"]["uid"], resourceVersion=current["metadata"]["resourceVersion"],
                      releaseStatus="failed")
        print(json.dumps(output, sort_keys=True))
        return 0
    except RepairError as error:
        output["reason"] = str(error)
    except (OSError, ValueError, TypeError, KeyError, RecursionError):
        output["reason"] = "local_io_or_invalid_input"
    print(json.dumps(output, sort_keys=True))
    return 1


if __name__ == "__main__":
    sys.exit(main())
