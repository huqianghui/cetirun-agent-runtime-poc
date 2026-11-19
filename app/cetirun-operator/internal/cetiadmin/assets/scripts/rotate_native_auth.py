#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Rotate one ordinary Agent's native authentication, then observe retained restart.

Only native kubectl with context cetiadmin is used. CETIADMIN_AGENT_UID must
identify the caller. The same request-ID annotation on Agent (intent) and
Secret (applied rotation) permits recovery without storing credentials or a
local journal. An intent without an applied receipt is unknown, not permission
to retry a Secret write. Admission must protect this annotation on both kinds.
"""

import argparse
import base64
import binascii
import json
import os
import re
import secrets
import subprocess
import sys
import time


ROTATION_REQUEST_ANNOTATION = "cetirun.microsoft.com/native-auth-rotation-request-id"
AGENT_API = "cetirun.microsoft.com/v1alpha1"
AGENT_RESOURCE = "agents.cetirun.microsoft.com"
MANAGED_BY = "app.kubernetes.io/managed-by"
INSTANCE_LABEL = "app.kubernetes.io/instance"
DNS_LABEL = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?")
UID = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}")
REQUEST_ID = re.compile(r"[A-Za-z0-9_-]{16,128}")
ENV_KEY = re.compile(r"^[ \t]*(?:export[ \t]+)?API_SERVER_KEY[ \t]*=(.*)$")


class RotationError(Exception):
    def __init__(self, code, status="error"):
        self.code, self.status = code, status


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise RotationError("invalid_arguments")


class Kubectl:
    def __init__(self, args):
        self.args = args
        self.deadline = time.monotonic() + args.timeout_seconds

    def call(self, verb, resource, name, *, namespace=None, operations=None):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise RotationError("operation_timeout", "pending")
        command = ["kubectl", "--context", "cetiadmin",
                   f"--request-timeout={min(10, remaining):.3f}s"]
        if namespace:
            command += ["--namespace", namespace]
        command += [verb, resource, name]
        if operations is None:
            command += ["--output=json", "--ignore-not-found=true"]
        else:
            command += ["--type=json", "--patch-file=/dev/stdin", "--output=name"]
        try:
            result = subprocess.run(
                command, input=json.dumps(operations) if operations is not None else None,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                timeout=min(15, remaining), check=False,
            )
        except FileNotFoundError:
            raise RotationError("kubectl_unavailable") from None
        except (OSError, subprocess.SubprocessError):
            raise RotationError("write_outcome_unknown" if operations is not None else "read_failed",
                                "unknown" if operations is not None else "error") from None
        if result.returncode:
            raise RotationError("write_outcome_unknown" if operations is not None else "read_failed",
                                "unknown" if operations is not None else "error")
        if operations is not None:
            return
        if not result.stdout.strip():
            code = "operator_reconstruction_required" if resource == "secret" else "target_missing"
            raise RotationError(code)
        try:
            body = json.loads(result.stdout)
            if not isinstance(body, dict):
                raise ValueError()
            return body
        except (ValueError, TypeError):
            raise RotationError("invalid_response") from None

    def get(self, resource, name, namespace=None):
        return self.call("get", resource, name, namespace=namespace)

    def patch(self, resource, body, changes, namespace=None):
        metadata = body["metadata"]
        operations = [
            {"op": "test", "path": "/metadata/uid", "value": metadata["uid"]},
            {"op": "test", "path": "/metadata/resourceVersion", "value": metadata["resourceVersion"]},
            *changes,
        ]
        self.call("patch", resource, metadata["name"], namespace=namespace, operations=operations)


def metadata(body, kind, name, uid=None, namespace=None):
    meta = body.get("metadata")
    if (body.get("kind") != kind or body.get("apiVersion") != (AGENT_API if kind == "Agent" else "v1")
            or not isinstance(meta, dict) or meta.get("name") != name
            or not isinstance(meta.get("uid"), str) or not UID.fullmatch(meta["uid"])
            or (uid is not None and meta["uid"] != uid)
            or not isinstance(meta.get("resourceVersion"), str) or not meta["resourceVersion"]
            or meta.get("namespace") not in (namespace, "" if namespace is None else namespace)):
        raise RotationError("target_conflict")
    if meta.get("deletionTimestamp"):
        raise RotationError("target_deleting")
    for key in ("annotations", "labels"):
        if meta.get(key) is not None and not isinstance(meta[key], dict):
            raise RotationError("invalid_response")
    return meta


def owned_by_agent(owners, args):
    return (isinstance(owners, list) and len(owners) == 1 and isinstance(owners[0], dict)
            and all(owners[0].get(key) == value for key, value in {
                "apiVersion": AGENT_API, "kind": "Agent", "name": args.agent, "uid": args.agent_uid,
            }.items()))


def marker(body):
    value = (body["metadata"].get("annotations") or {}).get(ROTATION_REQUEST_ANNOTATION, "")
    if not isinstance(value, str) or (value and not REQUEST_ID.fullmatch(value)):
        raise RotationError("request_conflict")
    return value


def marker_change(body, request_id):
    if body["metadata"].get("annotations") is None:
        return {"op": "add", "path": "/metadata/annotations", "value": {ROTATION_REQUEST_ANNOTATION: request_id}}
    escaped = ROTATION_REQUEST_ANNOTATION.replace("~", "~0").replace("/", "~1")
    return {"op": "add", "path": "/metadata/annotations/" + escaped, "value": request_id}


def target(client, expected=None):
    args = client.args
    agent = client.get(AGENT_RESOURCE, args.agent)
    meta = metadata(agent, "Agent", args.agent, args.agent_uid)
    spec, status = agent["spec"], agent.get("status") or {}
    runtime = spec["agent"]["type"]
    alias = spec["user"]["alias"]
    secret_name = spec.get("gatewayTokenSecretRef", "")
    if (runtime not in {"hermes", "openclaw"} or not isinstance(alias, str) or not DNS_LABEL.fullmatch(alias)
            or not isinstance(secret_name, str) or not secret_name or len(secret_name) > 253
            or not all(DNS_LABEL.fullmatch(part) for part in secret_name.split("."))):
        raise RotationError("unsupported_target")
    if status.get("namespace") != args.namespace or args.namespace != "cetirun-" + args.agent:
        raise RotationError("target_conflict")
    if (spec.get("suspended") or status.get("phase") in {"Suspended", "Deleting"}
            or spec.get("sandboxRef") or status.get("sandbox")
            or any(str(value).startswith("cetirun.microsoft.com/sandbox-") for value in meta.get("finalizers", []))):
        raise RotationError("target_disabled")
    migration, observed = spec.get("plannedMigration"), status.get("plannedMigration")
    if (migration or observed) and (not migration or not observed or observed.get("phase") != "Completed"
                                  or any(migration.get(key) != observed.get(key) for key in ("requestID", "target"))):
        raise RotationError("migration_in_progress")
    if ((spec.get("helm") or {}).get("maintenance")
            or (status.get("helmMaintenance") or {}).get("phase") in {"Requested", "Held", "Reapplying", "Blocked"}):
        raise RotationError("maintenance_in_progress")
    if expected and (runtime, alias, secret_name) != expected:
        raise RotationError("target_changed")
    namespace = client.get("namespace", args.namespace)
    ns_meta = metadata(namespace, "Namespace", args.namespace, args.namespace_uid)
    labels = ns_meta.get("labels") or {}
    if (not owned_by_agent(ns_meta.get("ownerReferences"), args)
            or labels.get(MANAGED_BY) != "cetirun-operator" or labels.get(INSTANCE_LABEL) != alias
            or labels.get("app.kubernetes.io/name") != "cetirun"
            or labels.get("cetirun.microsoft.com/sandbox-protected") == "true"
            or (namespace.get("status") or {}).get("phase") != "Active"):
        raise RotationError("namespace_conflict")
    return agent, (runtime, alias, secret_name)


def native_secret(client, identity, uid=None):
    runtime, alias, name = identity
    secret = client.get("secret", name, client.args.namespace)
    meta = metadata(secret, "Secret", name, uid, client.args.namespace)
    labels, owners = meta.get("labels") or {}, meta.get("ownerReferences")
    if (secret.get("type") != "Opaque" or secret.get("immutable")
            or labels.get(MANAGED_BY) != "cetirun-operator" or labels.get(INSTANCE_LABEL) != alias
            or (labels.get("cetirun.microsoft.com/agent-uid") not in (None, client.args.agent_uid))
            or (owners and not owned_by_agent(owners, client.args))):
        raise RotationError("secret_conflict")
    if not isinstance(secret.get("data"), dict) or (runtime == "hermes" and "hermes.env" not in secret["data"]):
        raise RotationError("operator_reconstruction_required")
    return secret


def ready(agent):
    meta, status = agent["metadata"], agent.get("status") or {}
    conditions = [c for c in status.get("conditions", []) if c.get("type") == "Ready"]
    return (type(meta.get("generation")) is int and meta["generation"] > 0
            and status.get("phase") == "Ready" and status.get("observedGeneration") == meta["generation"]
            and len(conditions) == 1 and conditions[0].get("status") == "True"
            and conditions[0].get("observedGeneration", meta["generation"]) == meta["generation"])


def restart_ids(agent):
    requested = agent["spec"].get("restartRequestID", "")
    observed = (agent.get("status") or {}).get("observedRestartRequestID", "")
    if any(not isinstance(value, str) or (value and not REQUEST_ID.fullmatch(value)) for value in (requested, observed)):
        raise RotationError("request_conflict")
    return requested, observed


def decode(value):
    try:
        return base64.b64decode(value, validate=True).decode("utf-8")
    except (ValueError, TypeError, UnicodeError, binascii.Error):
        raise RotationError("invalid_native_auth") from None


def replace_env_key(value, key):
    lines, found = [], False
    for line in decode(value).splitlines(keepends=True):
        if ENV_KEY.fullmatch(line.rstrip("\r\n")):
            ending = "\r\n" if line.endswith("\r\n") else "\n" if line.endswith("\n") else ""
            line, found = "API_SERVER_KEY=" + key + ending, True
        lines.append(line)
    text = "".join(lines)
    if not found:
        text += ("" if not text or text.endswith("\n") else "\n") + "API_SERVER_KEY=" + key + "\n"
    return text


def verify_receipt(secret, runtime, request_id, expected=None):
    if marker(secret) != request_id:
        raise RotationError("rotation_receipt_missing", "unknown")
    data = secret["data"]
    native = "API_SERVER_KEY" if runtime == "hermes" else "OPENCLAW_GATEWAY_TOKEN"
    key, auth = decode(data.get(native)), decode(data.get("CETIRUN_AUTH_KEY"))
    if not key or not auth or (expected and any(data.get(k) != v for k, v in expected.items())):
        raise RotationError("rotation_receipt_conflict", "unknown")
    if runtime == "hermes":
        values = [match.group(1) for line in decode(data.get("hermes.env")).splitlines()
                  if (match := ENV_KEY.fullmatch(line))]
        if not values or any(value != key for value in values):
            raise RotationError("rotation_receipt_conflict", "unknown")


def rotate(client, result):
    args = client.args
    agent, identity = target(client)
    secret = native_secret(client, identity)
    secret_uid = secret["metadata"]["uid"]
    intent, applied = marker(agent), marker(secret)
    requested, observed = restart_ids(agent)
    if requested != observed and requested != args.request_id:
        raise RotationError("restart_in_progress")
    if intent == args.request_id:
        verify_receipt(secret, identity[0], args.request_id)
    else:
        if applied == args.request_id or requested == args.request_id or intent != applied:
            raise RotationError("request_conflict")
        if intent and (requested != intent or observed != intent):
            raise RotationError("previous_rotation_incomplete")
        if not ready(agent):
            raise RotationError("target_not_ready")
        native = "API_SERVER_KEY" if identity[0] == "hermes" else "OPENCLAW_GATEWAY_TOKEN"
        new_key = secrets.token_urlsafe(32)
        values = {native: new_key, "CETIRUN_AUTH_KEY": secrets.token_urlsafe(32)}
        if identity[0] == "hermes":
            values["hermes.env"] = replace_env_key(secret["data"]["hermes.env"], new_key)
        encoded = {key: base64.b64encode(value.encode()).decode() for key, value in values.items()}
        client.patch(AGENT_RESOURCE, agent, [marker_change(agent, args.request_id)])
        current, _ = target(client, identity)
        if marker(current) != args.request_id or restart_ids(current) != (requested, observed) or not ready(current):
            raise RotationError("target_changed", "unknown")
        changes = [{"op": "add", "path": "/data/" + key, "value": value} for key, value in encoded.items()]
        client.patch("secret", secret, [*changes, marker_change(secret, args.request_id)], args.namespace)
        secret = native_secret(client, identity, secret_uid)
        verify_receipt(secret, identity[0], args.request_id, encoded)
    result["rotation"] = "confirmed"
    submitted = False
    while time.monotonic() < client.deadline:
        agent, _ = target(client, identity)
        if marker(agent) != args.request_id:
            raise RotationError("request_conflict")
        requested, observed = restart_ids(agent)
        if requested == args.request_id:
            submitted = True
            if observed == requested and ready(agent):
                verify_receipt(native_secret(client, identity, secret_uid), identity[0], args.request_id)
                result.update(status="ready", restart="ready")
                return
        elif submitted:
            raise RotationError("restart_outcome_unknown", "unknown")
        elif requested != observed:
            raise RotationError("restart_in_progress")
        elif ready(agent):
            verify_receipt(native_secret(client, identity, secret_uid), identity[0], args.request_id)
            client.patch(AGENT_RESOURCE, agent, [{"op": "add", "path": "/spec/restartRequestID", "value": args.request_id}])
            submitted = True
            continue
        remaining = client.deadline - time.monotonic()
        if remaining <= 0:
            break
        time.sleep(min(1, remaining))
    raise RotationError("restart_timeout", "pending")


def main():
    parser = Parser(description=__doc__)
    for name in ("agent", "agent-uid", "namespace", "namespace-uid", "request-id"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--timeout-seconds", type=int, default=120, help="overall deadline, 1–600 seconds (default: 120)")
    result = {"status": "error"}
    try:
        args = parser.parse_args()
        for value, pattern in ((args.agent, DNS_LABEL), (args.namespace, DNS_LABEL),
                               (args.agent_uid, UID), (args.namespace_uid, UID), (args.request_id, REQUEST_ID)):
            if not pattern.fullmatch(value):
                raise RotationError("invalid_arguments")
        if not 1 <= args.timeout_seconds <= 600:
            raise RotationError("invalid_arguments")
        caller = os.environ.get("CETIADMIN_AGENT_UID", "")
        if not UID.fullmatch(caller):
            raise RotationError("caller_identity_missing")
        if caller == args.agent_uid:
            raise RotationError("self_target_rejected")
        result.update(agent=args.agent, agentUID=args.agent_uid, namespace=args.namespace,
                      namespaceUID=args.namespace_uid, requestID=args.request_id)
        rotate(Kubectl(args), result)
    except RotationError as error:
        result.update(status=error.status, code=error.code)
    except (Exception, KeyboardInterrupt):
        result.update(status="unknown", code="operation_interrupted")
    print(json.dumps(result, sort_keys=True))
    return 0 if result["status"] == "ready" else 1


if __name__ == "__main__":
    sys.exit(main())
