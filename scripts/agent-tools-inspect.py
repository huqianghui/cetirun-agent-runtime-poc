#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Read-only inventory of CetiRun Agent tool Connection and Binding record metadata.

The command receives only selected metadata projected by kubectl JSONPath.
Kubectl can fetch full Secret objects to perform this client-side projection;
normal Secret list permission is required. This script does not receive,
decrypt, or print their payloads. Display metadata is not authorization evidence.
"""

import argparse
from dataclasses import asdict, dataclass
import json
import os
from pathlib import Path
import subprocess
import sys
from typing import Iterable, Optional


DEFAULT_NAMESPACE = "cetirun-credentials"
REQUEST_TIMEOUT = "15s"
COMMAND_TIMEOUT_SECONDS = 20
MAX_METADATA_BYTES = 8 * 1024 * 1024
PREFIX = "agenttools.cetirun.microsoft.com/"


class InspectionError(Exception):
    """A sanitized, non-sensitive inspection failure."""


@dataclass(frozen=True)
class Agent:
    name: str
    uid: str
    namespace: str


@dataclass(frozen=True)
class SecretRecord:
    name: str
    record_id: str
    owner_tenant: str
    owner_object: str
    provider: str = ""
    account_id: str = ""
    mode: str = ""
    instance_uid: str = ""
    connection_id: str = ""
    instance_name_snapshot: str = ""
    instance_namespace_snapshot: str = ""
    namespace: str = ""
    record_type: str = ""


def _field(kind: str) -> str:
    """Return a JSONPath field for a fixed, reviewed metadata key."""
    if kind == "name":
        return ".metadata.name"
    keys = {
        "id": "id", "owner_tenant": "owner-tenant", "owner_object": "owner-object",
        "provider": "provider", "account_id": "account-id", "mode": "mode",
        "instance_uid": "instance-uid", "connection_id": "connection-id",
    }
    annotations = {
        "instance_name_snapshot": "instance-name",
        "instance_namespace_snapshot": "instance-namespace",
    }
    if kind in keys:
        return ".metadata.labels." + (PREFIX + keys[kind]).replace(".", r"\.")
    if kind in annotations:
        return ".metadata.annotations." + (PREFIX + annotations[kind]).replace(".", r"\.")
    raise AssertionError(kind)


def _jsonpath(fields: Iterable[str]) -> str:
    values = list(fields)
    # All fields are fixed above. Do not add arbitrary metadata maps here:
    # records can carry unrelated annotations and this utility is intentionally
    # a narrow projection.
    chunks = ["{range .items[*]}"]
    for index, value in enumerate(values):
        if index:
            chunks.append('{"\\t"}')
        chunks.append("{" + _field(value) + "}")
    chunks.extend(['{"\\n"}', "{end}"])
    return "jsonpath=" + "".join(chunks)


class Kubectl:
    def __init__(self, kubeconfig: str, context: Optional[str]):
        self.kubeconfig = kubeconfig
        self.context = context

    def metadata_list(self, resource: str, namespace: Optional[str], selector: Optional[str], fields: Iterable[str]) -> str:
        command = ["kubectl", "--kubeconfig", self.kubeconfig]
        if self.context:
            command.extend(["--context", self.context])
        command.extend(["--request-timeout=" + REQUEST_TIMEOUT, "get", resource])
        if namespace:
            command.extend(["--namespace", namespace])
        if selector:
            command.extend(["--selector", selector])
        command.extend(["--output", _jsonpath(fields)])
        environment = os.environ.copy()
        # Do not persist kubectl preferences as a side effect of inspection.
        environment["KUBECTL_KUBERC"] = "false"
        try:
            completed = subprocess.run(
                command,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                timeout=COMMAND_TIMEOUT_SECONDS,
                check=False,
                env=environment,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise InspectionError("kubectl metadata query failed") from error
        if completed.returncode != 0:
            raise InspectionError("kubectl metadata query failed")
        if len(completed.stdout.encode("utf-8")) > MAX_METADATA_BYTES:
            raise InspectionError("metadata result exceeds the 8 MiB inspection limit")
        return completed.stdout


def _rows(output: str, width: int, record_kind: str) -> list[list[str]]:
    rows: list[list[str]] = []
    for line in output.splitlines():
        if not line:
            continue
        columns = line.split("\t")
        if len(columns) != width:
            raise InspectionError("kubectl returned malformed " + record_kind + " metadata")
        rows.append(columns)
    return rows


def list_agents(kubectl: Kubectl) -> list[Agent]:
    fields = ("name", "uid", "namespace")
    # Agent is cluster-scoped; status.namespace is its workload namespace.
    path = "jsonpath={range .items[*]}{.metadata.name}{\"\\t\"}{.metadata.uid}{\"\\t\"}{.status.namespace}{\"\\n\"}{end}"
    command = ["kubectl", "--kubeconfig", kubectl.kubeconfig]
    if kubectl.context:
        command.extend(["--context", kubectl.context])
    command.extend(["--request-timeout=" + REQUEST_TIMEOUT, "get", "agents.cetirun.microsoft.com", "--output", path])
    environment = os.environ.copy()
    environment["KUBECTL_KUBERC"] = "false"
    try:
        completed = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                   text=True, timeout=COMMAND_TIMEOUT_SECONDS, check=False, env=environment)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise InspectionError("kubectl Agent metadata query failed") from error
    if completed.returncode != 0:
        raise InspectionError("kubectl Agent metadata query failed")
    if len(completed.stdout.encode("utf-8")) > MAX_METADATA_BYTES:
        raise InspectionError("Agent metadata result exceeds the 8 MiB inspection limit")
    return [Agent(*row) for row in _rows(completed.stdout, len(fields), "Agent") if row[0] and row[1]]


def list_records(kubectl: Kubectl, namespace: str, record_type: str) -> list[SecretRecord]:
    fields = (
        "name", "id", "owner_tenant", "owner_object", "provider", "account_id", "mode",
        "instance_uid", "connection_id", "instance_name_snapshot", "instance_namespace_snapshot",
    )
    output = kubectl.metadata_list(
        "secrets", namespace, PREFIX + "record-type=" + record_type, fields,
    )
    return [SecretRecord(*row, namespace=namespace, record_type=record_type)
            for row in _rows(output, len(fields), record_type)]


def agent_reference(uid: str, agents: dict[str, Agent], snapshot_name: str = "", snapshot_namespace: str = "") -> dict:
    if uid and uid in agents:
        agent = agents[uid]
        return {"uid": uid, "name": agent.name, "namespace": agent.namespace, "resolution": "live"}
    if uid:
        # Snapshot values are display-only. In particular, never find a new
        # Agent by this name: its UID would be a different security principal.
        return {
            "uid": uid,
            "name": snapshot_name or None,
            "namespace": snapshot_namespace or None,
            "resolution": "deleted/unresolved",
        }
    return {"uid": None, "name": None, "namespace": None, "resolution": "unassigned/unknown"}


def _owner(record: SecretRecord) -> dict:
    return {"tenantId": record.owner_tenant or None, "objectId": record.owner_object or None}


def _same_owner(left: SecretRecord, right: SecretRecord) -> bool:
    return bool(left.owner_tenant and left.owner_object) and (
        left.owner_tenant, left.owner_object
    ) == (right.owner_tenant, right.owner_object)


def _record_key(record: SecretRecord) -> tuple[str, str]:
    """Secret identity: an opaque record ID is only unique within its namespace."""
    return (record.namespace, record.record_id)


def _connection_match(candidates: Iterable[SecretRecord], connection_id: str, owner: SecretRecord) -> tuple[Optional[SecretRecord], str]:
    """Find one owner-matched Connection without crossing namespace identities."""
    identified = [record for record in candidates if record.record_id == connection_id]
    matched = [record for record in identified if _same_owner(record, owner)]
    if len(matched) == 1:
        return matched[0], "present"
    if len(matched) > 1:
        return None, "ambiguous/unresolved"
    if identified:
        return None, "owner-mismatch"
    return None, "missing/unresolved"


def inventory(agents: list[Agent], central_namespace: str, connections: list[SecretRecord],
              bindings: list[SecretRecord], references: list[SecretRecord], selected_uids: set[str],
              reference_scope: str, unqueried_agents: list[dict]) -> dict:
    """Relate credential records without treating an Agent-local reference as a credential.

    Connection IDs are opaque and can recur in different namespaces.  A local
    Binding first resolves an owner-matched local Connection.  Otherwise it must
    have an owner-matched local connection-reference before it can resolve to a
    central Connection. Bindings in the central namespace are not valid Agent
    associations.
    """
    agent_by_uid = {agent.uid: agent for agent in agents}
    displayed_bindings = [binding for binding in bindings if not selected_uids or binding.instance_uid in selected_uids]
    central_connections = [record for record in connections if record.namespace == central_namespace]
    local_connections: dict[str, list[SecretRecord]] = {}
    local_references: dict[str, list[SecretRecord]] = {}
    for connection in connections:
        if connection.namespace != central_namespace:
            local_connections.setdefault(connection.namespace, []).append(connection)
    for reference in references:
        local_references.setdefault(reference.namespace, []).append(reference)

    def resolve(binding: SecretRecord) -> tuple[Optional[SecretRecord], Optional[SecretRecord], str]:
        if binding.namespace == central_namespace:
            return None, None, "invalid-binding-namespace"
        connection, status = _connection_match(local_connections.get(binding.namespace, []), binding.connection_id, binding)
        if connection or status in ("owner-mismatch", "ambiguous/unresolved"):
            return connection, None, status
        reference, reference_status = _connection_match(local_references.get(binding.namespace, []), binding.connection_id, binding)
        if reference is not None:
            # A reference is valid only when its two fixed labels agree. It is
            # credential-free and never becomes a Connection record itself.
            if reference.connection_id != reference.record_id:
                return None, reference, "malformed-reference/unresolved"
            connection, central_status = _connection_match(central_connections, reference.connection_id, binding)
            return connection, reference, "referenced-central/" + central_status
        if reference_status in ("owner-mismatch", "ambiguous/unresolved"):
            return None, None, "reference-" + reference_status
        # There is no local reference. Do not use a central record for a new
        # local Binding.
        return None, None, "reference-missing/unresolved"

    resolved = [(binding, *resolve(binding)) for binding in displayed_bindings]
    consumers_by_connection: dict[tuple[str, str], list[SecretRecord]] = {}
    for binding, connection, _, _ in resolved:
        if connection is not None:
            consumers_by_connection.setdefault(_record_key(connection), []).append(binding)

    displayed_references = references
    displayed_connections = connections
    if selected_uids:
        # A selected Agent must not see unrelated central delegated accounts
        # merely because they share the central credential namespace. Retain
        # only its own references, its bound independent Connections, and the
        # Connections reached by its Bindings or valid local references.
        displayed_references = [reference for reference in references if reference.instance_uid in selected_uids]
        selected_connection_keys = set(consumers_by_connection)
        for connection in connections:
            if connection.instance_uid in selected_uids:
                selected_connection_keys.add(_record_key(connection))
        for reference in displayed_references:
            if reference.connection_id != reference.record_id:
                continue
            connection, status = _connection_match(central_connections, reference.connection_id, reference)
            if connection is not None and status == "present":
                selected_connection_keys.add(_record_key(connection))
        displayed_connections = [connection for connection in connections if _record_key(connection) in selected_connection_keys]

    rendered_connections = []
    for connection in sorted(displayed_connections, key=lambda item: (item.namespace, item.record_id, item.name)):
        consumer_bindings = consumers_by_connection.get(_record_key(connection), [])
        consumers = [agent_reference(binding.instance_uid, agent_by_uid, binding.instance_name_snapshot, binding.instance_namespace_snapshot)
                     for binding in sorted(consumer_bindings, key=lambda item: (item.instance_uid, item.namespace, item.record_id))]
        binding = agent_reference(connection.instance_uid, agent_by_uid, connection.instance_name_snapshot, connection.instance_namespace_snapshot)
        if connection.instance_uid:
            relation = "independent-identity-binding"
        elif consumers:
            relation = "shared-or-delegated-via-binding" if connection.mode == "delegated" else "binding-reference/mode-unknown"
        else:
            relation = "unassigned/unknown"
        rendered_connections.append({
            "secretNamespace": connection.namespace or None,
            "secretName": connection.name,
            "recordId": connection.record_id or None,
            "provider": connection.provider or None,
            "mode": connection.mode or None,
            "accountId": connection.account_id or None,
            "owner": _owner(connection),
            "credentialStorage": "central" if connection.namespace == central_namespace else "agent-local",
            "relation": relation,
            "binding": binding if connection.instance_uid else None,
            "bindingConsumers": consumers,
        })

    rendered_references = []
    for reference in sorted(displayed_references, key=lambda item: (item.namespace, item.record_id, item.name)):
        target, status = _connection_match(central_connections, reference.connection_id, reference)
        if reference.connection_id != reference.record_id:
            status = "malformed-reference/unresolved"
            target = None
        rendered_references.append({
            "secretNamespace": reference.namespace or None,
            "secretName": reference.name,
            "recordId": reference.record_id or None,
            "connectionId": reference.connection_id or None,
            "provider": reference.provider or None,
            "mode": reference.mode or None,
            "owner": _owner(reference),
            "credentialStorage": "none/reference",
            "agent": agent_reference(reference.instance_uid, agent_by_uid, reference.instance_name_snapshot, reference.instance_namespace_snapshot),
            "centralConnection": {
                "secretNamespace": target.namespace if target else None,
                "secretName": target.name if target else None,
                "recordId": reference.connection_id or None,
                "resolution": status,
            },
        })

    rendered_bindings = []
    relations = []
    for binding, connection, reference, connection_resolution in sorted(resolved, key=lambda item: (item[0].namespace, item[0].instance_uid, item[0].record_id)):
        agent = agent_reference(binding.instance_uid, agent_by_uid, binding.instance_name_snapshot, binding.instance_namespace_snapshot)
        connection_summary = {
            "recordId": binding.connection_id or None,
            "secretNamespace": connection.namespace if connection else None,
            "secretName": connection.name if connection else None,
            "resolution": connection_resolution,
        }
        reference_summary = None if reference is None else {
            "secretNamespace": reference.namespace or None,
            "secretName": reference.name,
            "recordId": reference.record_id or None,
        }
        rendered_bindings.append({
            "secretNamespace": binding.namespace or None,
            "secretName": binding.name,
            "recordId": binding.record_id or None,
            "connection": connection_summary,
            "reference": reference_summary,
            "owner": _owner(binding),
            "agent": agent,
            "validity": "unknown/not evaluated (encrypted record)",
        })
        relations.append({
            "bindingSecretNamespace": binding.namespace or None,
            "bindingRecordId": binding.record_id or None,
            "bindingSecretName": binding.name,
            "referenceSecretNamespace": reference.namespace if reference else None,
            "referenceSecretName": reference.name if reference else None,
            "connectionSecretNamespace": connection.namespace if connection else None,
            "connectionRecordId": binding.connection_id or None,
            "connectionSecretName": connection.name if connection else None,
            "connectionResolution": connection_resolution,
            "agent": agent,
        })
    displayed_agents = agents if not selected_uids else [agent for agent in agents if agent.uid in selected_uids]
    return {
        "scope": {
            "centralCredentialNamespace": central_namespace,
            "referenceCoverage": reference_scope,
            "unqueriedAgents": unqueried_agents,
        },
        "agents": [asdict(agent) for agent in sorted(displayed_agents, key=lambda item: (item.name, item.uid))],
        "connections": rendered_connections,
        "connectionReferences": rendered_references,
        "bindings": rendered_bindings,
        "relations": relations,
        "validity": "unknown/not evaluated: binding payload is encrypted; record presence is not an active permission decision",
    }


def _cell(value: object) -> str:
    if value is None:
        return "-"
    return "".join(character if character.isprintable() else " " for character in str(value))


def render_table(result: dict) -> str:
    lines = [
        "SCOPE: central credential namespace=" + _cell(result["scope"]["centralCredentialNamespace"]),
        "REFERENCE COVERAGE: " + _cell(result["scope"]["referenceCoverage"]),
        "UNQUERIED AGENTS: " + (", ".join(
            _cell(agent["name"]) + " (" + _cell(agent["uid"]) + ": " + _cell(agent["reason"]) + ")"
            for agent in result["scope"]["unqueriedAgents"]
        ) or "(none)"),
        "", "CONNECTIONS", "SECRET NAMESPACE\tSECRET\tRECORD ID\tSTORAGE\tPROVIDER\tMODE\tRELATION\tAGENT UID\tAGENT NAME\tWORKLOAD NAMESPACE\tREFERENCED BY AGENTS",
    ]
    for connection in result["connections"]:
        binding = connection["binding"] or {"uid": None, "name": None, "namespace": None}
        lines.append("\t".join(_cell(value) for value in (
            connection["secretNamespace"], connection["secretName"], connection["recordId"], connection["credentialStorage"],
            connection["provider"], connection["mode"], connection["relation"],
            binding["uid"], binding["name"], binding["namespace"],
            ", ".join(sorted({consumer["name"] or consumer["uid"] or "unknown" for consumer in connection["bindingConsumers"]})) or None,
        )))
    if not result["connections"]:
        lines.append("(none)")
    lines.extend(["", "CONNECTION REFERENCES (credential-free, non-authoritative)", "SECRET NAMESPACE\tSECRET\tRECORD ID\tCENTRAL CONNECTION ID\tCENTRAL CONNECTION\tSTATUS\tAGENT UID\tAGENT NAME\tRESOLUTION"])
    for reference in result["connectionReferences"]:
        agent = reference["agent"]
        central = reference["centralConnection"]
        lines.append("\t".join(_cell(value) for value in (
            reference["secretNamespace"], reference["secretName"], reference["recordId"], reference["connectionId"],
            central["secretName"], central["resolution"], agent["uid"], agent["name"], agent["resolution"],
        )))
    if not result["connectionReferences"]:
        lines.append("(none)")
    lines.extend(["", "BINDINGS", "SECRET NAMESPACE\tSECRET\tRECORD ID\tCONNECTION ID\tCONNECTION NAMESPACE\tCONNECTION\tCONNECTION STATUS\tAGENT UID\tAGENT NAME\tWORKLOAD NAMESPACE\tRESOLUTION\tVALIDITY"])
    for binding in result["bindings"]:
        agent = binding["agent"]
        connection = binding["connection"]
        lines.append("\t".join(_cell(value) for value in (
            binding["secretNamespace"], binding["secretName"], binding["recordId"], connection["recordId"], connection["secretNamespace"],
            connection["secretName"], connection["resolution"],
            agent["uid"], agent["name"], agent["namespace"], agent["resolution"], binding["validity"],
        )))
    if not result["bindings"]:
        lines.append("(none)")
    lines.extend(["", "RELATIONS", "BINDING NAMESPACE\tBINDING ID\tREFERENCE NAMESPACE\tREFERENCE\tCONNECTION NAMESPACE\tCONNECTION ID\tAGENT UID\tAGENT NAME\tRESOLUTION"])
    for relation in result["relations"]:
        agent = relation["agent"]
        lines.append("\t".join(_cell(value) for value in (
            relation["bindingSecretNamespace"], relation["bindingRecordId"], relation["referenceSecretNamespace"], relation["referenceSecretName"],
            relation["connectionSecretNamespace"], relation["connectionRecordId"], agent["uid"], agent["name"], relation["connectionResolution"],
        )))
    if not result["relations"]:
        lines.append("(none)")
    lines.append("\nValidity: unknown/not evaluated; encrypted binding payloads were not decrypted. References have no credential and may include revoked or disabled bindings.")
    return "\n".join(lines)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--kubeconfig", required=True, help="explicit kubeconfig; this command never changes its context")
    result.add_argument("--context", help="optional context within --kubeconfig")
    result.add_argument("--namespace", default=DEFAULT_NAMESPACE, help="central delegated credential Secret namespace (default: %(default)s)")
    selected = result.add_mutually_exclusive_group()
    selected.add_argument("--agent", help="live Agent name; queries only its namespace plus the central credential namespace")
    selected.add_argument("--agent-uid", help="Agent UID; uses its live namespace when present, otherwise central records only")
    result.add_argument("--output", choices=("table", "json"), default="table")
    return result


def main(argv: Optional[list[str]] = None) -> int:
    args = parser().parse_args(argv)
    kubeconfig = str(Path(args.kubeconfig))
    kubectl = Kubectl(kubeconfig, args.context)
    try:
        agents = list_agents(kubectl)
        selected_uids: set[str] = set()
        if args.agent:
            matches = [agent for agent in agents if agent.name == args.agent]
            if not matches:
                raise InspectionError("no live Agent has the requested name; use --agent-uid for a deleted Agent")
            selected_uids.add(matches[0].uid)
        elif args.agent_uid:
            selected_uids.add(args.agent_uid)
        selected_agents = agents
        if selected_uids:
            selected_agents = [agent for agent in agents if agent.uid in selected_uids]
        namespaces = set()
        unqueried_agents = []
        for agent in selected_agents:
            if not agent.namespace:
                # Pending Agents cannot have local records queried safely. Do
                # not turn this into a synthetic empty namespace result.
                unqueried_agents.append({
                    "uid": agent.uid,
                    "name": agent.name,
                    "reason": "status.namespace is empty",
                })
                continue
            if agent.namespace != args.namespace:
                namespaces.add(agent.namespace)
        connections = list_records(kubectl, args.namespace, "connection")
        bindings: list[SecretRecord] = []
        references: list[SecretRecord] = []
        for namespace in sorted(namespaces):
            # An absent or forbidden namespace stops inspection. It is never
            # represented as an empty record set because that would conceal an
            # incomplete authorization inventory.
            connections.extend(list_records(kubectl, namespace, "connection"))
            bindings.extend(list_records(kubectl, namespace, "binding"))
            references.extend(list_records(kubectl, namespace, "connection-reference"))
        if not selected_uids:
            reference_scope = (
                "live Agent namespaces with status.namespace queried; Pending/no-namespace Agents are listed as unqueried"
                if unqueried_agents else "all live Agent namespaces queried"
            )
        elif selected_agents:
            reference_scope = (
                "central namespace only; selected live Agent has no workload namespace"
                if unqueried_agents else "selected live Agent namespace plus central namespace queried"
            )
        else:
            reference_scope = "central namespace only; requested Agent UID is not currently live"
        result = inventory(agents, args.namespace, connections, bindings, references, selected_uids, reference_scope, unqueried_agents)
        if args.output == "json":
            print(json.dumps(result, sort_keys=True, indent=2))
        else:
            print(render_table(result))
        return 0
    except InspectionError as error:
        print("agent-tools-inspect: " + str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
