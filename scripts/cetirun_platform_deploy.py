#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Install the default CetiRun platform across its existing Helm releases."""

import argparse
import base64
from collections import Counter
import copy
from datetime import datetime, timezone
import hashlib
import importlib.util
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
from urllib.parse import urlsplit
from uuid import UUID
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

import yaml


ROOT = Path(__file__).resolve().parents[1]
CLUSTER_KINDS = {"Namespace", "CustomResourceDefinition", "ClusterRole", "ClusterRoleBinding",
                 "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration",
                 "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "StorageClass", "PersistentVolume"}
CHARTS = {"istio": "infra/istio", "broker": "app/cetirun-agent-broker/chart",
          "platform": "app/cetirun-platform"}
AGENT_ACCOUNT_FIELDS = ("metadata.name", "spec.agent.type", "spec.team", "spec.sandboxRef",
                        "spec.helm.values.serviceAccount.name", "spec.helm.values.serviceAccount.role",
                        "spec.helm.values.serviceAccount.automountToken")
AGENT_POLICY_MARKERS = (
    '{{with .spec}}{{if .sandboxRef}}protected{{end}}'
    '{{with .modelAccess}}{{if .monthlyBudgetUSD}}protected{{end}}{{end}}{{end}}'
    '{{with .status}}{{with .aiGateway}}{{if .policyRevision}}protected{{end}}{{end}}'
    '{{range $key,$value := .}}{{if or (eq $key "policy") (eq $key "sandbox")}}protected{{end}}{{end}}'
    '{{range .conditions}}{{if eq .type "SandboxReady"}}protected{{end}}{{end}}{{end}}'
    '{{range .metadata.finalizers}}{{if or (eq . "cetirun.microsoft.com/sandbox-hold") '
    '(eq . "cetirun.microsoft.com/sandbox-preparation") (eq . "cetirun.microsoft.com/sandbox-bootstrap")}}protected{{end}}{{end}}'
    '{{range $key,$value := .metadata.annotations}}{{if or (eq $key "cetirun.microsoft.com/sandbox-authorization") '
    '(eq $key "cetirun.microsoft.com/sandbox-reauthorize")}}protected{{end}}{{end}}'
)
CETIADMIN_SOURCE = "cetirun-cetiadmin-config"
CETIADMIN_SOURCE_UID = "cetirun.microsoft.com/cetiadmin-source-uid"
CETIADMIN_FIELDS = {
    "owner": {"alias", "tenantID", "objectID"},
    "kubernetes": {"apiServer", "apiAudience", "caConfigMapName", "apiEgressCIDRs"},
    "model": {"default", "initialMonthlyBudgetUSD", "githubCopilot"},
    "runtime": {"version", "image", "port"},
    "azureWorkloadIdentity": {"enabled", "clientId"},
    "sharedSkills": {"enabled"},
    "monitor": {"enabled", "schedule", "timezone", "deliver", "slackChannel", "slackSecretName"},
}
CETIADMIN_AGENT_FIELDS = ("spec.suspended", "status.phase", "status.accessUrl", "spec.helm.maintenance", "status.helmMaintenance")
CETIADMIN_WRITER_FIELDS = {
    "pods": ("metadata.uid", "metadata.deletionTimestamp", "status.phase"),
    "statefulsets": ("metadata.uid", "spec.replicas", "status.replicas"),
    "deployments": ("metadata.uid", "spec.replicas", "status.replicas"),
    "replicasets": ("metadata.uid", "spec.replicas", "status.replicas"),
    "daemonsets": ("metadata.uid",),
    "jobs": ("metadata.uid", "status.active", "status.conditions"),
    "cronjobs": ("metadata.uid", "spec.suspend", "status.active[*].uid"),
}
# The Operator reads customRuntime strictly and stops at startup on a field it does not know.
OPERATOR_VALUES = ROOT / "app/cetirun-operator/chart/values.yaml"
OBSERVABILITY_VALUES = ROOT / "app/cetirun-observability/chart/values.yaml"
REMOVED_CUSTOM_FIELDS = {"agentManagedModel", "gatewayImages"}
# An error names a failed command by its tool, its operands and these options. Other option
# values can be templates or local files. Tool output can hold rendered Secrets, so it is not shown.
NAMED_OPTIONS = {"--namespace", "--name", "--timeout", "--version", "--dry-run"}
FLAG_OPTIONS = {"--install", "--ignore-not-found", "--reuse-values", "--skip-refresh", "--minify", "-noout",
                "--deployed", "--failed", "--pending", "--uninstalling", "--uninstalled", "--superseded"}
# A chart's own refusal during the offline render, from its fail or required call. Charts
# word these messages with value names, never with Secret material.
CHART_REFUSAL = re.compile(r"execution error at \(([^)\n]+)\): ([^\n]+)")


def step(command):
    """Name a command for an error message, without its output, local files or templates."""
    words = [str(word) for word in command]
    while words[0] == sys.executable or words[0].startswith("-"):
        words = words[1:]
    if words[1:2] in (["--context"], ["--kube-context"]):
        words = words[:1] + words[3:]
    shown, rest = [Path(words[0]).name], iter(words[1:])
    for word in rest:
        if word.split("=", 1)[0] in NAMED_OPTIONS:
            shown.append(word if "=" in word else f"{word} {next(rest, '')}".rstrip())
        elif word.startswith("-"):
            if "=" not in word and word not in FLAG_OPTIONS:
                next(rest, None)
        elif not Path(word).is_absolute():
            shown.append(word)
    return " ".join(shown)
# The optional Skill Marketplace, which the Operator runs. Only the top-level
# skillMarketplace block of the environment file turns its switches on, and it
# turns on both.
SKILL_MARKETPLACE = "cetirun-operator.skillMarketplace"
SKILL_MARKETPLACE_SWITCHES = (f"{SKILL_MARKETPLACE}.enabled", "cetirun-portal.skillMarketplace.enabled")
SKILL_MARKETPLACE_FIELDS = {
    "skillMarketplace": ("repository", "storage"),
    "skillMarketplace.repository": ("url", "ref", "path", "auth", "existingSecret", "syncInterval"),
    "skillMarketplace.storage": ("resourceGroup", "storageAccount", "containerName", "storageClassName"),
}
# Keys that the marketplace reads from the repository credentials Secret.
SKILL_MARKETPLACE_SECRET_KEYS = {"none": (), "token": ("token",), "ssh": ("ssh-privatekey", "known_hosts")}
# The optional ADX-Mon collection of logs, events and metrics into Azure Data
# Explorer. Only the top-level observability block of the environment file
# turns it on. make deploy does not call Azure: the Kusto cluster, its
# databases and the ingestor's identity exist before the deployment.
OBSERVABILITY = "cetirun-observability"
OBSERVABILITY_INGESTOR = "cetirun-adx-mon-ingestor"
OBSERVABILITY_FIELDS = {
    "observability": ("clusterName", "adx", "ingestorClientId"),
    "observability.adx": ("endpoint", "databases"),
    "observability.adx.databases": ("logs", "metrics", "agentLogs"),
}
GUID = r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"
SCP_URL = re.compile(r"[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^/\s]\S*")
DNS_LABEL = r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"
DNS_SUBDOMAIN = rf"(?=.{{1,253}}$){DNS_LABEL}(?:\.{DNS_LABEL})*"
DURATION_UNITS = {"ns": 1e-9, "us": 1e-6, "ms": 1e-3, "s": 1, "m": 60, "h": 3600}


def load_helper(filename):
    spec = importlib.util.spec_from_file_location(filename.replace("-", "_"), ROOT / "scripts" / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


MESH = load_helper("ensure-oauth2-proxy-id-token-header.py")
RELEASE = load_helper("cetirun-release.py")
PUBLIC_TLS = load_helper("cetirun_public_ingress.py")


def mapping(value, name):
    if not isinstance(value, dict):
        raise ValueError(f"{name} must be a mapping")
    return value


def required(data, path):
    value = get_value(data, path)
    if not isinstance(value, str) or not value.strip() or value != value.strip():
        raise ValueError(f"{path} is required and must be a nonempty string")
    return value


def get_value(data, path):
    value = data
    for key in path.split("."):
        value = value.get(key) if isinstance(value, dict) else None
    return value


def merge(base, update):
    result = copy.deepcopy(base)
    for key, value in mapping(update, "values").items():
        result[key] = merge(result[key], value) if isinstance(value, dict) and isinstance(result.get(key), dict) else copy.deepcopy(value)
    return result


def set_value(data, path, value):
    keys = path.split(".")
    for key in keys[:-1]:
        data = data.setdefault(key, {})
        mapping(data, path)
    data[keys[-1]] = value


def walk(value):
    if isinstance(value, dict):
        yield value
        for item in value.values():
            yield from walk(item)
    elif isinstance(value, list):
        for item in value:
            yield from walk(item)


def yaml_file(path):
    return mapping(yaml.load(path.read_text(), Loader=MESH.UniqueKeyLoader), str(path))


def duration_seconds(value):
    """Seconds of a Go duration such as 5m or 1h30m; None for anything else."""
    if not isinstance(value, str):
        return None
    parts = re.findall(r"(\d+(?:\.\d*)?|\.\d+)(ns|us|ms|s|m|h)", value)
    if not parts or "".join(number + unit for number, unit in parts) != value:
        return None
    return sum(float(number) * DURATION_UNITS[unit] for number, unit in parts)


def check_repository_url(url, auth):
    # The marketplace logs this URL and lists it in the catalog that the Portal
    # shows, so it must not carry credentials. Errors never repeat the URL.
    secret = "; keep credentials in the Secret that skillMarketplace.repository.existingSecret names"
    if re.search(r"\s", url):
        raise ValueError("skillMarketplace.repository.url must not contain whitespace")
    if auth == "ssh" and SCP_URL.fullmatch(url):
        return
    parts = urllib.parse.urlsplit(url)
    if (parts.password is not None or (parts.username is not None and parts.scheme != "ssh")
            or parts.query or parts.fragment):
        raise ValueError("skillMarketplace.repository.url must not contain credentials, a query or a fragment" + secret)
    if auth == "ssh":
        if parts.scheme != "ssh" or not parts.hostname:
            raise ValueError("skillMarketplace.repository.auth ssh needs an ssh:// URL or the user@host:path form")
    elif parts.scheme != "https" or not parts.hostname:
        raise ValueError(f"skillMarketplace.repository.auth {auth} needs an https:// URL")


def skill_marketplace(config, platform):
    """Platform values of the optional Skill Marketplace, from its own input block."""
    if "skillMarketplace" not in config:
        for field in SKILL_MARKETPLACE_SWITCHES:
            if get_value(config["platform"].get("values", {}), field):
                raise ValueError(f"platform.values.{field} needs the top-level skillMarketplace block, which configures the Skill Marketplace")
        return dict.fromkeys(SKILL_MARKETPLACE_SWITCHES, False)
    block = mapping(config["skillMarketplace"], "skillMarketplace")
    for name in ("repository", "storage"):
        if block.get(name) is None:
            raise ValueError(f"skillMarketplace.{name} is required")
    repository = mapping(block["repository"], "skillMarketplace.repository")
    storage = mapping(block["storage"], "skillMarketplace.storage")
    for name, section in (("skillMarketplace", block), ("skillMarketplace.repository", repository),
                          ("skillMarketplace.storage", storage)):
        unknown = sorted(map(str, set(section) - set(SKILL_MARKETPLACE_FIELDS[name])))
        if unknown:
            raise ValueError(f"{name}.{unknown[0]} is not supported; set other chart values under platform.values.{SKILL_MARKETPLACE}")

    url = required(config, "skillMarketplace.repository.url")
    auth = repository.get("auth")
    if not isinstance(auth, str) or auth not in SKILL_MARKETPLACE_SECRET_KEYS:
        raise ValueError("skillMarketplace.repository.auth must be none, token or ssh")
    check_repository_url(url, auth)
    secret, keys = repository.get("existingSecret"), SKILL_MARKETPLACE_SECRET_KEYS[auth]
    if not keys and secret not in (None, ""):
        raise ValueError("skillMarketplace.repository.existingSecret is used only with auth token or ssh")
    if keys and not (isinstance(secret, str) and re.fullmatch(DNS_SUBDOMAIN, secret)):
        raise ValueError(f"skillMarketplace.repository.auth {auth} needs existingSecret: the name of a Secret in platform.namespace "
                         f"with the {'key' if len(keys) == 1 else 'keys'} {' and '.join(keys)}")
    defaults = yaml_file(OPERATOR_VALUES)["skillMarketplace"]["repository"]
    optional = {}
    for field, default in (("ref", ""), ("path", ""), ("syncInterval", defaults["syncInterval"])):
        optional[field] = default if repository.get(field) is None else repository[field]
        if not isinstance(optional[field], str):
            raise ValueError(f"skillMarketplace.repository.{field} must be a string")
    if re.search(r"[\s~^:?*\[\\]", optional["ref"]) or ".." in optional["ref"]:
        raise ValueError("skillMarketplace.repository.ref must name a branch, a tag or a full ref")
    if optional["path"].startswith("/") or ".." in optional["path"]:
        raise ValueError("skillMarketplace.repository.path must be relative to the repository root, without ..")
    seconds = duration_seconds(optional["syncInterval"])
    if seconds is None or seconds < 30:
        raise ValueError("skillMarketplace.repository.syncInterval must be a duration of at least 30s, for example 5m")

    for field in SKILL_MARKETPLACE_FIELDS["skillMarketplace.storage"]:
        required(config, f"skillMarketplace.storage.{field}")
    if not re.fullmatch(r"[a-z0-9]{3,24}", storage["storageAccount"]):
        raise ValueError("skillMarketplace.storage.storageAccount must be 3-24 lowercase letters and digits")
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{1,61}[a-z0-9]", storage["containerName"]) or "--" in storage["containerName"]:
        raise ValueError("skillMarketplace.storage.containerName must be a Blob container name")
    if not re.fullmatch(DNS_SUBDOMAIN, storage["storageClassName"]):
        raise ValueError("skillMarketplace.storage.storageClassName must be a StorageClass name")
    shared = config.get("sharedSkills") if isinstance(config.get("sharedSkills"), dict) else {}
    if (storage["storageAccount"], storage["containerName"]) == (shared.get("storageAccount"), shared.get("containerName")):
        raise ValueError("skillMarketplace.storage must name a container other than the sharedSkills container: "
                         "the marketplace writes Team releases into its container")

    # The Operator creates the skills claims only in native Sandbox execution,
    # and its chart refuses the marketplace next to a published skills catalog.
    sources = mapping(platform["cetirun-operator"].get("sandboxSources"), "cetirun-operator.sandboxSources")
    if sources.get("enabled") is not True or sources.get("nativeExecution") is not True:
        raise ValueError("skillMarketplace needs native Sandbox execution: set platform.values.cetirun-operator.sandboxSources "
                         "enabled and nativeExecution to true")
    if sources.get("skillsCatalogConfigMapName"):
        raise ValueError("skillMarketplace replaces the published skills catalog: leave "
                         "platform.values.cetirun-operator.sandboxSources.skillsCatalogConfigMapName empty")
    claim = sources.get("skillsClaimName")
    if not isinstance(claim, str) or not re.fullmatch(DNS_LABEL, claim):
        raise ValueError("skillMarketplace needs cetirun-operator.sandboxSources.skillsClaimName, a DNS label: "
                         "the Operator creates that claim in each Agent namespace")

    namespace = config["platform"]["namespace"]
    values = {"enabled": True, "repository.url": url, "repository.auth": auth, "repository.existingSecret": secret if keys else "",
              **{f"repository.{field}": value for field, value in optional.items()},
              **{f"storage.{field}": storage[field] for field in SKILL_MARKETPLACE_FIELDS["skillMarketplace.storage"]}}
    return {**{f"{SKILL_MARKETPLACE}.{field}": value for field, value in values.items()},
            "cetirun-portal.skillMarketplace.enabled": True, "cetirun-portal.skillMarketplace.catalogNamespace": namespace}


def observability(config):
    """Platform values of the optional ADX-Mon collection, from its own input block."""
    switch = f"{OBSERVABILITY}.enabled"
    if "observability" not in config:
        if get_value(config["platform"].get("values", {}), switch):
            raise ValueError(f"platform.values.{switch} needs the top-level observability block, "
                             "which names the Kusto cluster and the ingestor identity")
        return {switch: False}
    block = mapping(config["observability"], "observability")
    adx = mapping(block.get("adx", {}), "observability.adx")
    databases = mapping(adx.get("databases", {}), "observability.adx.databases")
    for name, section in (("observability", block), ("observability.adx", adx), ("observability.adx.databases", databases)):
        unknown = sorted(map(str, set(section) - set(OBSERVABILITY_FIELDS[name])))
        if unknown:
            raise ValueError(f"{name}.{unknown[0]} is not supported; set other chart values under platform.values.{OBSERVABILITY}")
    cluster = required(config, "observability.clusterName")
    if not re.fullmatch(DNS_LABEL, cluster):
        raise ValueError("observability.clusterName must be a DNS label: every row names this CetiRun cluster with it")
    endpoint = required(config, "observability.adx.endpoint")
    if not re.fullmatch(r"https://[A-Za-z0-9.-]+(?::[0-9]{1,5})?", endpoint):
        raise ValueError("observability.adx.endpoint must be the query URI of the Kusto cluster, "
                         "such as https://<name>.<region>.kusto.windows.net, without a path")
    client = required(config, "observability.ingestorClientId")
    if not re.fullmatch(GUID, client):
        raise ValueError("observability.ingestorClientId must be the client ID of the managed identity that the ingestor uses")
    defaults = yaml_file(OBSERVABILITY_VALUES)["adx"]["databases"]
    names = {}
    for field in OBSERVABILITY_FIELDS["observability.adx.databases"]:
        names[field] = databases.get(field, defaults[field])
        if not isinstance(names[field], str) or not re.fullmatch(r"[A-Za-z][A-Za-z0-9]{0,99}", names[field]):
            raise ValueError(f"observability.adx.databases.{field} must be a database name of letters and digits")
    if len(set(names.values())) != len(names):
        raise ValueError("observability.adx.databases must name three databases: Agent logs need readers of their own")
    return {switch: True, f"{OBSERVABILITY}.clusterName": cluster, f"{OBSERVABILITY}.adx.endpoint": endpoint,
            f"{OBSERVABILITY}.ingestor.clientId": client,
            **{f"{OBSERVABILITY}.adx.databases.{field}": name for field, name in names.items()}}


def resource_name(value, *, label=False, limit=253):
    return (isinstance(value, str) and 0 < len(value) <= limit
            and all(re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", part)
                    for part in value.split(".")) and (not label or "." not in value))


def object_uid(value):
    return isinstance(value, str) and bool(re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}", value))


def nonzero_uuid(value):
    try:
        return isinstance(value, str) and str(UUID(value)).lower() == value.lower() and UUID(value).int != 0
    except (ValueError, AttributeError):
        return False


def forbidden_api_address(address):
    address = getattr(address, "ipv4_mapped", None) or address
    return address.is_loopback or address.is_link_local or address.is_multicast or address.is_unspecified


def validate_cetiadmin(config, operator):
    """Validate deployment inputs without querying identities, credentials or APIs."""
    admin = mapping(operator.get("cetiadmin"), "cetiadmin")
    allowed = set(CETIADMIN_FIELDS) | {"enabled", "legacyDisabledSource", "instanceName", "platformReadNamespaces", "retainedAgentUID", "migrationSource", "hookTimeoutSeconds"}
    if set(admin) - allowed:
        raise ValueError("cetiadmin contains unsupported fields")
    for field, fields in CETIADMIN_FIELDS.items():
        section = mapping(admin.get(field), "cetiadmin." + field)
        if set(section) - fields:
            raise ValueError("cetiadmin." + field + " contains unsupported fields")
    if type(admin.get("enabled")) is not bool:
        raise ValueError("cetiadmin.enabled must be an explicit boolean")
    legacy = admin.get("legacyDisabledSource", False)
    if type(legacy) is not bool or (legacy and admin["enabled"]):
        raise ValueError("cetiadmin.legacyDisabledSource requires a boolean and cetiadmin.enabled=false")
    if not resource_name(admin.get("instanceName"), label=True, limit=44):
        raise ValueError("cetiadmin.instanceName must be a DNS label of at most 44 characters for the openclaw- Helm release")
    timeout = admin.get("hookTimeoutSeconds")
    if type(timeout) is not int or not 1 <= timeout <= 3600:
        raise ValueError("cetiadmin.hookTimeoutSeconds must be an integer from 1 to 3600")
    if admin.get("retainedAgentUID") and not object_uid(admin["retainedAgentUID"]):
        raise ValueError("cetiadmin.retainedAgentUID must be an exact existing Agent UID")
    if not admin["enabled"]:
        return
    owner = admin["owner"]
    if (not re.fullmatch(r"[a-z][a-z0-9-]{0,62}", str(owner.get("alias", "")))
            or owner["alias"] != config["ownerAlias"]):
        raise ValueError(
            "CetiAdmin is enabled by default: cetiadmin.owner.alias must match the explicit platform ownerAlias; "
            "configure its owner, budget and API access, or set cetiadmin.enabled=false"
        )
    if not nonzero_uuid(owner.get("tenantID")) or owner["tenantID"].lower() != config["identity"]["tenantId"].lower():
        raise ValueError("cetiadmin.owner.tenantID must match the explicit platform identity.tenantId")
    if not nonzero_uuid(owner.get("objectID")):
        raise ValueError("cetiadmin.owner.objectID requires the real owner's nonzero Entra object UUID")
    model = admin["model"]
    budget = model.get("initialMonthlyBudgetUSD")
    if (not isinstance(budget, str) or not re.fullmatch(r"(0|[1-9][0-9]*)(\.[0-9]{1,6})?", budget)
            or not math.isfinite(float(budget))):
        raise ValueError("cetiadmin.model.initialMonthlyBudgetUSD must be an explicit nonnegative decimal string")
    if len(required({"cetiadmin": admin}, "cetiadmin.model.default")) > 256:
        raise ValueError("cetiadmin.model.default is too long")
    for field in ("model.githubCopilot", "azureWorkloadIdentity.enabled", "sharedSkills.enabled", "monitor.enabled"):
        if type(get_value(admin, field)) is not bool:
            raise ValueError("cetiadmin." + field + " must be a boolean")
    namespaces = admin.get("platformReadNamespaces", [])
    if not isinstance(namespaces, list) or any(not resource_name(name, label=True, limit=63) for name in namespaces):
        raise ValueError("cetiadmin.platformReadNamespaces must contain valid namespace names")
    access = admin["kubernetes"]
    server = access.get("apiServer")
    try:
        parsed = urlsplit(server)
        if (not isinstance(server, str) or parsed.scheme != "https" or not parsed.hostname
                or parsed.username is not None or parsed.password is not None or parsed.path not in ("", "/")
                or parsed.query or parsed.fragment or parsed.hostname.lower() == "localhost"
                or (parsed.port is not None and parsed.port < 1) or any(char.isspace() for char in server)):
            raise ValueError()
        try:
            address = ipaddress.ip_address(parsed.hostname)
        except ValueError:
            address = None
        if address is not None and forbidden_api_address(address):
            raise ValueError()
        origin = (parsed.hostname.lower(), parsed.port or 443)
        target = urlsplit(config["target"]["server"])
        allowed_origins = {(target.hostname.lower(), target.port or 443)} | {
            (host, 443) for host in ("kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local")}
        if origin not in allowed_origins:
            raise ValueError()
    except (ValueError, TypeError, AttributeError):
        raise ValueError("cetiadmin.kubernetes.apiServer must select this target's HTTPS API origin or its in-cluster Service") from None
    audience = access.get("apiAudience")
    if (not isinstance(audience, str) or not audience or any(char.isspace() for char in audience)
            or audience == operator["connectionsMCP"]["audience"]):
        raise ValueError("cetiadmin.kubernetes.apiAudience must be explicit and distinct from the MCP audience")
    if not resource_name(access.get("caConfigMapName")):
        raise ValueError("cetiadmin.kubernetes.caConfigMapName must name the cluster CA ConfigMap")
    cidrs = access.get("apiEgressCIDRs")
    if not isinstance(cidrs, list) or not cidrs:
        raise ValueError("cetiadmin.kubernetes.apiEgressCIDRs requires verified single-host API CIDRs")
    for value in cidrs:
        try:
            prefix = ipaddress.ip_network(value, strict=True) if isinstance(value, str) and "/" in value else None
            if prefix is None or prefix.prefixlen != prefix.max_prefixlen or forbidden_api_address(prefix.network_address):
                raise ValueError()
        except (ValueError, TypeError):
            raise ValueError("cetiadmin.kubernetes.apiEgressCIDRs must be /32 or /128 and exclude loopback, link-local, multicast and unspecified addresses") from None
    runtime = admin["runtime"]
    for field in ("version", "image"):
        if not isinstance(runtime.get(field), str):
            raise ValueError("cetiadmin.runtime." + field + " must be a string")
    version = runtime.get("version") or config["release"]["version"]
    if version != config["release"]["version"]:
        raise ValueError("cetiadmin.runtime.version must match the Operator/runtime release")
    if runtime.get("port") is not None and (type(runtime["port"]) is not int or runtime["port"] != 8642):
        raise ValueError("cetiadmin.runtime.port must match the Hermes profile port 8642")
    image = runtime.get("image", "")
    if image and (image.startswith("-") or any(char.isspace() for char in image)
                  or not (image.endswith(":" + version) or re.search(r"@sha256:[0-9a-f]{64}$", image))):
        raise ValueError("cetiadmin.runtime.image must select the matching version or a qualified digest")
    identity = admin["azureWorkloadIdentity"]
    if identity["enabled"] and not nonzero_uuid(identity.get("clientId")):
        raise ValueError("cetiadmin.azureWorkloadIdentity.clientId must name an existing managed identity")
    monitor = admin["monitor"]
    if monitor.get("deliver") not in ("local", "slack"):
        raise ValueError("cetiadmin.monitor.deliver must be local or slack")
    try:
        if not monitor.get("timezone") or monitor["timezone"] == "Local":
            raise ValueError()
        ZoneInfo(monitor["timezone"])
    except (ValueError, TypeError, ZoneInfoNotFoundError):
        raise ValueError("cetiadmin.monitor.timezone must be an IANA timezone") from None
    if monitor["enabled"]:
        if not isinstance(monitor.get("schedule"), str) or not monitor["schedule"].strip():
            raise ValueError("cetiadmin.monitor.schedule is required")
        if monitor["deliver"] == "slack" and (not isinstance(monitor.get("slackChannel"), str)
                or not re.fullmatch(r"[CG][A-Z0-9]{8,31}", monitor["slackChannel"])
                or not resource_name(monitor.get("slackSecretName"))):
            raise ValueError("cetiadmin.monitor requires an exact Slack channel and existing Secret name")
    if admin.get("migrationSource") is not None:
        source = mapping(admin["migrationSource"], "cetiadmin.migrationSource")
        if set(source) != {"agent", "namespace"}:
            raise ValueError("cetiadmin.migrationSource requires Agent and namespace identities")
        for field in ("agent", "namespace"):
            item = mapping(source[field], "cetiadmin.migrationSource." + field)
            if (set(item) - {"name", "namespace", "uid"} or item.get("namespace")
                    or not resource_name(item.get("name"), label=field == "namespace") or not object_uid(item.get("uid"))):
                raise ValueError("cetiadmin.migrationSource requires exact names and UIDs, not an inferred source")
        if source["agent"]["name"] == admin["instanceName"]:
            raise ValueError("cetiadmin.migrationSource requires a distinct new instanceName and HOME")


def cetiadmin_config_hash(value):
    """Match cetiadmin.Config JSON field order, defaults and omitempty rules."""
    mapping(value, "cetiadmin configuration")
    for section, fields in CETIADMIN_FIELDS.items():
        if section in value and (not isinstance(value[section], dict) or set(value[section]) - fields):
            raise ValueError("cetiadmin configuration has unsupported fields or types")
    defaults = {
        "enabled": True, "instanceName": "cetiadmin", "owner": {"alias": "", "tenantID": "", "objectID": ""},
        "platformReadNamespaces": [],
        "kubernetes": {"apiServer": "https://kubernetes.default.svc", "apiAudience": "", "caConfigMapName": "kube-root-ca.crt", "apiEgressCIDRs": None},
        "model": {"default": "auto", "initialMonthlyBudgetUSD": "", "githubCopilot": False},
        "runtime": {"version": "", "image": "", "port": None},
        "azureWorkloadIdentity": {"enabled": False, "clientId": ""}, "sharedSkills": {"enabled": False},
        "monitor": {"enabled": False, "schedule": "", "timezone": "UTC", "deliver": "local", "slackChannel": "", "slackSecretName": ""},
        "retainedAgentUID": "", "migrationSource": None,
    }
    if set(value) - (set(defaults) | {"hookTimeoutSeconds", "legacyDisabledSource"}):
        raise ValueError("cetiadmin configuration has unsupported fields")
    canonical = merge(defaults, {key: item for key, item in value.items() if key not in ("hookTimeoutSeconds", "legacyDisabledSource")})
    namespaces = canonical["platformReadNamespaces"]
    if namespaces is not None and (not isinstance(namespaces, list) or any(not isinstance(name, str) for name in namespaces)):
        raise ValueError("cetiadmin.platformReadNamespaces must be a list of namespace names")
    for section, fields in {"runtime": ("version", "image", "port"),
                            "azureWorkloadIdentity": ("clientId",), "monitor": ("schedule", "slackChannel", "slackSecretName")}.items():
        for field in fields:
            if canonical[section].get(field) in (None, "", []):
                canonical[section].pop(field, None)
    for field in ("platformReadNamespaces", "retainedAgentUID", "migrationSource"):
        if not canonical[field]:
            canonical.pop(field)
    if canonical.get("migrationSource"):
        canonical["migrationSource"] = {
            key: {field: canonical["migrationSource"][key][field] for field in ("name", "namespace", "uid")
                  if canonical["migrationSource"][key].get(field)} for key in ("agent", "namespace")}
    encoded = json.dumps(canonical, ensure_ascii=False, separators=(",", ":"))
    for char in ("<", ">", "&", "\u2028", "\u2029"):
        encoded = encoded.replace(char, "\\u" + format(ord(char), "04x"))
    return hashlib.sha256(encoded.encode()).hexdigest()


def cetiadmin_identity(metadata, expected):
    if (not isinstance(metadata, dict) or not isinstance(expected, dict) or not object_uid(expected.get("uid"))
            or not resource_name(expected.get("name")) or not metadata.get("resourceVersion")
            or metadata.get("deletionTimestamp") or any(metadata.get(key, "") != expected.get(key, "") for key in ("name", "namespace", "uid"))):
        raise ValueError("CetiAdmin identity is missing, replaced or terminating")


def cetiadmin_owned(metadata, agent):
    if not any(owner.get("apiVersion") == "cetirun.microsoft.com/v1alpha1" and owner.get("kind") == "Agent"
               and owner.get("name") == agent["name"] and owner.get("uid") == agent["uid"]
               for owner in metadata.get("ownerReferences", [])):
        raise ValueError("CetiAdmin namespace or ServiceAccount ownership changed")


def validate_cetiadmin_writers(kind, rows):
    for row in rows:
        if not row[0]:
            raise ValueError("CetiAdmin writer identity is unavailable")
        stopped = False
        if kind == "pods":
            stopped = not row[1] and row[2] in ("Succeeded", "Failed")
        elif kind in ("statefulsets", "deployments", "replicasets"):
            stopped = row[1] == "0" and row[2] in ("", "0")
        elif kind == "jobs":
            conditions = json.loads(row[2] or "[]")
            stopped = row[1] in ("", "0") and any(item.get("status") == "True" and item.get("type") in ("Complete", "Failed") for item in conditions)
        elif kind == "cronjobs":
            stopped = row[1] == "true" and not row[2]
        if not stopped:
            raise ValueError("CetiAdmin writers have not stopped: " + kind)


def public_tls_config(config):
    tls = mapping(config.get("publicTLS", {}), "publicTLS")
    PUBLIC_TLS.keys(tls, {"enabled", "dns", "azure", "certManager", "acme", "stateDirectory",
                          "timeoutSeconds", "ingressService", "gatewayName"}, "publicTLS")
    if type(tls.get("enabled", False)) is not bool:
        raise ValueError("publicTLS.enabled must be a boolean")
    if "publicTLS" in config.get("platform", {}).get("values", {}):
        raise ValueError("Use the top-level publicTLS configuration; platform.values.publicTLS is reserved")
    if not tls.get("enabled", False):
        return None
    if os.environ.get("HELM_DRIVER", "secret") not in ("", "secret"):
        raise ValueError("publicTLS requires Helm's Secret storage driver")
    if config.get("istio", {}).get("tlsMode", "external") != "external":
        raise ValueError("Managed publicTLS requires istio.tlsMode=external")
    wildcard = get_value(config, "istio.values.gateway.includeWildcardHost")
    if wildcard is not None and wildcard is not False:
        raise ValueError("Managed publicTLS supports one exact hostname; includeWildcardHost must be false")
    result = {key: tls[key] for key in ("dns", "azure", "certManager", "acme", "stateDirectory", "timeoutSeconds") if key in tls}
    result["target"] = config.get("target", {})
    result["ingress"] = {"namespace": required(config, "istio.ingressNamespace"),
                         "release": required(config, "platform.release"),
                         "releaseNamespace": required(config, "platform.namespace"),
                         "service": tls.get("ingressService", "aks-istio-ingressgateway-external"),
                         "gatewayName": tls.get("gatewayName", "cetirun-acme-http")}
    return PUBLIC_TLS.normalize(result)


def configuration(path, *, domain=None, registry=None, version=None, coordinate_origins=None):
    config = copy.deepcopy(path) if isinstance(path, dict) else yaml_file(path)
    release = config.setdefault("release", {})
    if not isinstance(release, dict):
        raise ValueError("release must be an object")
    origins = config.setdefault("_releaseOrigins", {})
    for key, override in (("registry", registry), ("version", version)):
        if override is not None:
            origins[key] = (coordinate_origins or {}).get(key, "explicit override")
        else:
            origins.setdefault(key, "environment file" if key in release else "source default")
    if registry is not None:
        release["registry"] = registry.rstrip("/")
    if version is not None:
        release["version"] = version
    release.setdefault("registry", RELEASE.VERSIONS.DEFAULT_REGISTRY)
    release.setdefault("version", RELEASE.VERSIONS.top_level_yaml(ROOT / "app/cetirun-chart/Chart.yaml", "version"))
    source = copy.deepcopy(config)
    tls_config = public_tls_config(config)
    if tls_config:
        resolved = domain or tls_config["dns"].get("domain")
        if resolved and config.get("domain") and config["domain"] != resolved:
            raise ValueError("domain does not match the publicTLS hostname")
        config["domain"] = resolved or config.get("domain") or "pending-public-tls.example.invalid"
    for field in ("target.context", "target.cluster", "target.server", "target.clusterUID",
                  "release.registry", "release.version", "release.gitSHA", "domain", "identityRealm", "ownerAlias",
                  "istio.namespace", "istio.release", "istio.revision", "istio.controlPlaneNamespace",
                  "istio.controlPlaneDeployment", "istio.ingressNamespace", "istio.ingressDeployment",
                  "istio.ingressServiceAccount", "istio.meshConfigMap", "broker.namespace", "broker.release",
                  "platform.namespace", "platform.release"):
        required(config, field)
    RELEASE.coordinates(argparse.Namespace(registry=config["release"]["registry"], version=config["release"]["version"],
                                           git_sha=config["release"]["gitSHA"], chart_namespace="helm"))
    if type(config["release"].get("clientOpenAPIValidation", True)) is not bool:
        raise ValueError("release.clientOpenAPIValidation must be a boolean")
    release_inventory(config)
    if not re.fullmatch(r"https://[^\s/?#@]+", config["target"]["server"]):
        raise ValueError("target.server must be an HTTPS Kubernetes API origin without embedded credentials")
    if not re.fullmatch(r"[a-z0-9][a-z0-9.-]*\.[a-z0-9-]+", config["domain"]):
        raise ValueError("domain must be a hostname, without a scheme or path")
    for name in CHARTS:
        for field in ("namespace", "release"):
            if not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", config[name][field]):
                raise ValueError(f"{name}.{field} must be a DNS label")
    if len({config[name]["namespace"] for name in CHARTS}) != 3:
        raise ValueError("Istio, Broker credentials, and platform/MCP require separate namespaces")
    if config["istio"].get("mode", "manage") not in ("manage", "external"):
        raise ValueError("istio.mode must be manage or external")
    if config["istio"].get("tlsMode", "external") not in ("external", "retain"):
        raise ValueError("istio.tlsMode must be external or retain for an existing chart-owned certificate")
    if config.get("instances"):
        raise ValueError("Arbitrary Instance creation is not part of this deployment; configure CetiAdmin through cetirun-operator.cetiadmin")
    for field in ("tenantId", "portalClientId", "blueprintClientId", "authorizationClientId", "encryptionClientId", "keyVaultKeyUrl"):
        required(config, f"identity.{field}")
    for field in ("customRuntime.storageClass", "customRuntime.storageSize", "customRuntime.accessMode"):
        required(config, f"platform.values.cetirun-operator.{field}")
    persistence_path = "platform.values.cetirun-ai-gateway.postgresql.bundled.persistence"
    required(config, persistence_path + ".size")
    retained_storage = config["platform"].get("postgresqlStorage")
    if retained_storage is not None:
        retained_storage = mapping(retained_storage, "platform.postgresqlStorage")
        if (set(retained_storage) != {"mode", "storageClassName"}
                or retained_storage.get("mode") != "retain-default"
                or not resource_name(retained_storage.get("storageClassName"))):
            raise ValueError("platform.postgresqlStorage requires mode=retain-default and the retained PVC's storageClassName")
        if get_value(config, persistence_path + ".storageClass") != "":
            raise ValueError("retain-default requires an explicit empty PostgreSQL persistence.storageClass to preserve the existing claim template")
    else:
        required(config, persistence_path + ".storageClass")
    raw_custom = get_value(config, "platform.values.cetirun-operator.customRuntime")
    if type(raw_custom.get("fsGroup")) is not int or not 1 <= raw_custom["fsGroup"] <= 2147483647:
        raise ValueError("customRuntime.fsGroup must explicitly name the qualified non-root storage group")
    unknown = sorted(set(raw_custom) - set(yaml_file(OPERATOR_VALUES)["customRuntime"]))
    if unknown:
        note = " Every approved Custom image uses the AI Gateway binding." if REMOVED_CUSTOM_FIELDS & set(unknown) else ""
        raise ValueError("Remove " + ", ".join(f"customRuntime.{field}" for field in unknown) + " from the environment file: "
                         "the Operator stops at startup on a customRuntime field that it does not know." + note)

    values = {name: merge(yaml_file(ROOT / chart / "values.yaml"), config[name].get("values", {}))
              for name, chart in CHARTS.items()}
    platform = values["platform"]
    for component in ("operator", "portal", "ai-gateway", "connections-mcp", "observability"):
        key = f"cetirun-{component}"
        platform[key] = merge(yaml_file(ROOT / f"app/{key}/chart/values.yaml"), platform.get(key, {}))
    pns, bns, ins = (config[name]["namespace"] for name in ("platform", "broker", "istio"))
    identity, registry, version = config["identity"], config["release"]["registry"], config["release"]["version"]
    domain, revision = config["domain"], config["istio"]["revision"]
    operator_enabled = platform["cetirun-operator"]["enabled"]
    if type(operator_enabled) is not bool:
        raise ValueError("cetirun-operator.enabled must be a boolean")
    if not operator_enabled and (get_value(config, "platform.values.cetirun-operator.cetiadmin.enabled") is not False
            or get_value(config, "platform.values.global.portalChat.enabled") is not False):
        raise ValueError("Operator removal requires explicit cetiadmin.enabled=false and global.portalChat.enabled=false after retained shutdown")
    # The Portal can offer only StorageClasses pinned by the same selected
    # platform source as the Operator. Deployment input may choose a UI
    # preference, but must not redirect that source or invent an approval.
    portal_values = mapping(platform["cetirun-portal"], "cetirun-portal values")
    storage_values = mapping(portal_values.get("sandboxStorage", {}), "cetirun-portal.sandboxStorage")
    requested_portal = mapping(config["platform"].get("values", {}).get("cetirun-portal", {}),
                               "platform.values.cetirun-portal")
    requested_storage = requested_portal.get("sandboxStorage", {})
    requested_storage = mapping(requested_storage, "platform.values.cetirun-portal.sandboxStorage")
    sources = mapping(platform["cetirun-operator"].get("sandboxSources", {}), "cetirun-operator.sandboxSources")
    if operator_enabled and sources.get("enabled") is True:
        source_name = sources.get("platformConfigMapName")
        if not isinstance(source_name, str) or not source_name:
            raise ValueError("enabled cetirun-operator.sandboxSources requires platformConfigMapName")
        for field, expected in (("platformNamespace", pns), ("platformConfigMapName", source_name)):
            requested = requested_storage.get(field, "")
            if requested not in ("", expected):
                raise ValueError(f"platform.values.cetirun-portal.sandboxStorage.{field} conflicts with the selected Operator platform source")
        portal_values["sandboxStorage"] = {
            "platformNamespace": pns,
            "platformConfigMapName": source_name,
            "defaultStorageClassName": storage_values.get("defaultStorageClassName", ""),
        }
    elif requested_storage.get("platformNamespace") or requested_storage.get("platformConfigMapName"):
        raise ValueError("cetirun-portal.sandboxStorage requires enabled cetirun-operator.sandboxSources")
    gateway = f"{config['istio']['ingressNamespace']}/cetirun-gateway"
    broker_http = f"http://cetirun-agent-broker.{bns}.svc.cluster.local:8080"
    assignments = {
        "istio": {"domain": domain, "clientId": identity["portalClientId"], "tenantId": identity["tenantId"],
                  "gateway.namespace": config["istio"]["ingressNamespace"],
                  "egress.namespace": config["istio"]["controlPlaneNamespace"],
                  "portalAuth.clusterHost": f"cetirun-portal.{pns}.svc.cluster.local",
                  "portalAuth.mtls.enabled": True,
                  "portalAuth.mtls.serverIdentity": f"spiffe://cluster.local/ns/{pns}/sa/{platform['cetirun-portal']['serviceAccount']['name']}"},
        "broker": {"image.repository": f"{registry}/cetirun-agent-broker", "image.tag": version,
                   "tls.enabled": True, "istio.sidecarInjection": True, "istio.revision": revision,
                   "portal.namespace": pns, "operator.namespace": pns, "toolService.namespace": pns,
                   "agentIdentity.portalOrigin": f"https://{domain}", "agentIdentity.portalBaseUrl": f"https://{domain}",
                   "agentIdentity.redirectUri": f"https://{domain}/api/v1/connections/callback"},
        "platform": {"global.registry": registry, "global.version": version,
                     "global.portalChat.enabled": operator_enabled, "global.portalChat.identityRealm": config["identityRealm"],
                     "global.memoryContext.image": f"{registry}/cetirun-memory-context:{version}",
                     "istio.enabled": True, "istio.oauth2Proxy.namespace": ins,
                     "cetirun-operator.namespace": pns, "cetirun-operator.domainSuffix": domain,
                     "cetirun-operator.istioRevision": revision, "cetirun-operator.istioGateway": gateway,
                     "cetirun-operator.cetirun.helmValues.networkPolicy.istioIngressNamespace": config["istio"]["ingressNamespace"],
                     "cetirun-operator.customRuntime.enabled": True, "cetirun-operator.memoryContext.enabled": True,
                     "cetirun-operator.sandboxAdmission.enabled": True, "cetirun-operator.sandboxAdmission.enforce": True,
                     "cetirun-operator.connectionsMCP.url": f"https://cetirun-connections-mcp.{pns}.svc.cluster.local/mcp",
                     "cetirun-operator.connectionsMCP.brokerNamespace": bns,
                     "cetirun-operator.connectionsMCP.brokerServiceAccount": values["broker"]["serviceAccount"]["name"] or "cetirun-agent-broker",
                     "cetirun-operator.agentIdentity.enabled": True, "cetirun-operator.agentIdentity.brokerURL": broker_http,
                     "cetirun-operator.agentIdentity.tenantId": identity["tenantId"],
                     "cetirun-operator.agentIdentity.blueprintClientId": identity["blueprintClientId"],
                     "cetirun-portal.istio.host": domain, "cetirun-portal.istio.gateway": gateway,
                     "cetirun-portal.istio.gatewayNamespace": config["istio"]["ingressNamespace"],
                     "cetirun-portal.istio.gatewayServiceAccount": config["istio"]["ingressServiceAccount"],
                     "cetirun-portal.istio.internalAuthMTLS": True,
                     "cetirun-portal.aiGateway.catalogNamespace": pns,
                     "cetirun-portal.features.agentToolsEnabled": True, "cetirun-portal.features.agentIdentityEnabled": True,
                     "cetirun-portal.features.allowedAgentTypes": ["hermes", "openclaw", "custom"],
                     "cetirun-portal.agentIdentity.tenantId": identity["tenantId"],
                     "cetirun-portal.agentIdentity.portalClientId": identity["portalClientId"],
                     "cetirun-portal.agentIdentity.brokerURL": broker_http, "cetirun-portal.agentTools.brokerURL": broker_http,
                     "cetirun-connections-mcp.enabled": True,
                     "cetirun-connections-mcp.image.repository": f"{registry}/cetirun-connections-mcp",
                     "cetirun-connections-mcp.image.tag": version,
                     "cetirun-connections-mcp.broker.baseURL": f"https://cetirun-agent-broker.{bns}.svc.cluster.local",
                     "cetirun-connections-mcp.istio.brokerNamespace": bns,
                     "cetirun-connections-mcp.istio.controlPlaneNamespace": config["istio"]["controlPlaneNamespace"],
                     "cetirun-connections-mcp.istio.sidecarInjection": True, "cetirun-connections-mcp.istio.revision": revision,
                     "cetirun-connections-mcp.portal.baseURL": f"https://{domain}"},
    }
    for field in ("tenantId", "blueprintClientId", "authorizationClientId", "encryptionClientId", "keyVaultKeyUrl"):
        assignments["broker"][f"agentIdentity.{field}"] = identity[field]
    assignments["platform"].update(skill_marketplace(config, platform))
    assignments["platform"].update(observability(config))
    for name, fields in assignments.items():
        for field, value in fields.items():
            supplied = get_value(config[name].get("values", {}), field)
            if field == "global.memoryContext.image" and supplied:
                value = supplied
            elif supplied is not None and supplied != value:
                raise ValueError(f"{name}.values.{field} conflicts with the complete deployment inputs")
            set_value(values[name], field, value)

    operator, portal, ai, mcp = (platform[f"cetirun-{name}"] for name in ("operator", "portal", "ai-gateway", "connections-mcp"))
    runtime_values = mapping(operator["cetirun"]["helmValues"], "cetirun.helmValues")
    for section in (runtime_values, runtime_values.get("hermes", {}), runtime_values.get("openclaw", {})):
        if isinstance(section, dict) and "image" in section:
            raise ValueError("Remove primary image overrides from cetirun.helmValues; publish trusted runtime chart image pins and use spec.agent.image or cetiadmin.runtime.image")
    for valueset, standard in ((values["broker"], "cetirun-agent-broker"), (ai, "cetirun-ai-gateway"), (mcp, "cetirun-connections-mcp")):
        if valueset.get("fullnameOverride") not in (None, "", standard):
            raise ValueError(f"The complete deployment requires the {standard} Service name")
    values["broker"]["portal"]["serviceAccount"] = portal["serviceAccount"]["name"]
    values["broker"]["operator"]["serviceAccount"] = operator["serviceAccount"]["name"]
    values["broker"]["toolService"]["serviceAccount"] = mcp["serviceAccount"]["name"]
    operator["sandboxAdmission"]["portalServiceAccount"] = portal["serviceAccount"]["name"]
    if len({operator["connectionsMCP"]["audience"], mcp["workloadTokenAudience"], values["broker"]["workloadTokenAudience"]}) != 1:
        raise ValueError("Operator, MCP and Broker workload token audiences must match")
    runtime_lists = (operator["connectionsMCP"]["runtimeTypes"], portal["agentTools"]["runtimeTypes"], values["broker"]["toolService"]["runtimeTypes"])
    for runtimes in runtime_lists:
        if not isinstance(runtimes, list) or not runtimes or any(runtime not in ("hermes", "openclaw", "custom") for runtime in runtimes) or len(runtimes) != len(set(runtimes)):
            raise ValueError("MCP runtimeTypes must be nonempty lists of distinct supported runtimes")
    if any(set(runtimes) != set(runtime_lists[0]) for runtimes in runtime_lists[1:]):
        raise ValueError("Operator connectionsMCP.runtimeTypes, Portal agentTools.runtimeTypes and Broker toolService.runtimeTypes must match")
    portal["admins"]["aliases"] = sorted(set([*portal["admins"]["aliases"], config["ownerAlias"]]))
    if values["istio"]["oauth2Proxy"]["mode"] != "entra-id":
        raise ValueError("The complete Agent Identity deployment requires Entra ID; local-oidc cannot supply Broker identity")
    required(values["istio"], "allowedGroups")
    if values["istio"].get("cookieSecret") or values["istio"]["tls"].get("enabled") or values["istio"]["tls"].get("key"):
        raise ValueError("Use existing Gateway TLS and cookie Secrets, not inline cookieSecret or tls material")
    if bns == platform["global"]["registryCredentialNamespace"]:
        raise ValueError("Registry credentials must not share the Broker credential namespace")
    for component in ("operator", "portal", "ai-gateway", "connections-mcp"):
        if component != "operator" and platform[f"cetirun-{component}"].get("enabled", True) is not True:
            raise ValueError(f"The default deployment requires cetirun-{component}.enabled=true")
    if not values["broker"]["identityModes"]["microsoftDelegated"]["enabled"]:
        raise ValueError("The default deployment requires the Broker Auth SDK")
    required(values["broker"], "tls.secretName")
    for field in ("tls.secretName", "broker.clientTLSSecretName"):
        required(mcp, field)
    for field in ("sandboxAdmission.tlsSecretName", "sandboxAdmission.caBundle", "connectionsMCP.caSecretName", "azure.clientId"):
        required(operator, field)
    if not operator["sandboxAdmission"]["adminUsers"] and not operator["sandboxAdmission"]["adminGroups"]:
        raise ValueError("Explicit Kubernetes admission administrators are required")
    custom = operator["customRuntime"]
    if not custom["approvedImages"] or any(not re.fullmatch(r"\S+@sha256:[0-9a-f]{64}", image) for image in custom["approvedImages"]):
        raise ValueError("Custom runtime requires explicitly qualified approvedImages pinned by digest")
    skills = mapping(config.get("sharedSkills"), "sharedSkills")
    for field in ("storageClassName", "resourceGroup", "storageAccount", "containerName"):
        required(skills, field)
    operator["cetirun"]["helmValues"]["skills"] = {**skills, "shared": True}
    for field in ("providerCredentials.existingSecret", "auth.masterKey.existingSecret", "auth.saltKey.existingSecret",
                  "auth.adminUI.passwordSecret.existingSecret", "redis.passwordSecret.existingSecret"):
        required(ai, field)
    if ai["providerCredentials"].get("inline"):
        raise ValueError("Provider credentials must use an existing Secret")
    operator["aiGateway"]["masterKeySecret"] = {"name": ai["auth"]["masterKey"]["existingSecret"], "key": ai["auth"]["masterKey"]["key"]}
    if not ai["postgresql"]["bundled"]["enabled"] or not ai["redis"]["bundled"]["enabled"]:
        raise ValueError("This deployment manages bundled PostgreSQL and Valkey; external database migration needs a separate plan")
    models = {entry.get("model_name"): entry for entry in ai["models"]}
    routing = mapping(config.get("modelRouting", {}), "modelRouting")
    mode = routing.get("mode", "auto")
    if set(routing) - {"mode"} or mode not in ("auto", "direct"):
        raise ValueError("modelRouting.mode must be auto or direct")
    if mode == "direct":
        if not models or any(name in ("auto", "decision-1") for name in models):
            raise ValueError("Direct model routing requires configured models without auto or decision-1")
        if any(not isinstance(entry.get("litellm_params", {}).get("model"), str)
               or not entry["litellm_params"]["model"]
               or entry["litellm_params"]["model"].startswith("auto_router/")
               or entry.get("model_info", {}).get("mode") not in (None, "", "chat", "completion", "responses")
               for entry in ai["models"]):
            raise ValueError("Direct model routing requires callable models without classifiers or auto routers")
        admin = operator["cetiadmin"]
        if admin["enabled"] and admin["model"]["default"] not in models:
            raise ValueError("Direct model routing requires cetiadmin.model.default to name a configured model")
    else:
        auto = models.get("auto", {}).get("litellm_params", {})
        decision = models.get("decision-1", {})
        router = auto.get("complexity_router_config", {})
        if (auto.get("model") != "auto_router/complexity_router" or router.get("classifier_type") != "jev"
                or router.get("jev_classifier_config", {}).get("deployment_name") != "decision-1"):
            raise ValueError("models must configure auto with the existing jev classifier and logical decision-1 deployment")
        if (not decision.get("litellm_params", {}).get("model", "").startswith("azure_ai/")
                or decision.get("model_info", {}).get("base_model") != "azure_ai/Microsoft-Decision-1"
                or decision.get("model_info", {}).get("mode") != "evaluation"):
            raise ValueError("decision-1 needs an explicit azure_ai/<deployment>, base_model azure_ai/Microsoft-Decision-1 and evaluation mode. "
                             "LiteLLM prices the model only by that name, and the Operator does not offer an unpriced classifier to auto")
        decision_params = decision["litellm_params"]
        if not decision_params["model"].removeprefix("azure_ai/"):
            raise ValueError("decision-1 requires a real Azure deployment name")
        for field in ("api_base", "api_key"):
            required(decision_params, field)
        if "api_version" in decision_params:
            required(decision_params, "api_version")
        if set(router.get("tiers", {})) != {"SIMPLE", "MEDIUM", "COMPLEX", "REASONING"}:
            raise ValueError("auto requires explicit SIMPLE, MEDIUM, COMPLEX and REASONING tiers")
    for entry in ai["models"]:
        params = entry.get("litellm_params", {})
        if "api_key" in params and not re.fullmatch(r"os\.environ/[A-Za-z_][A-Za-z0-9_]*", str(params["api_key"])):
            raise ValueError("Model api_key must reference a provider Secret environment variable")
    validate_cetiadmin(config, operator)
    validate_excluded_images(config)
    if tls_config:
        values["platform"].update(PUBLIC_TLS.chart_values(tls_config, config["domain"], production=True, bootstrap=False))
        values["istio"]["tls"]["enabled"] = False
        values["istio"]["gateway"]["includeWildcardHost"] = False
    config["_source"] = source
    return config, values


def release_inventory(config):
    return RELEASE.selected_inventory(RELEASE.inventory(), config["release"].get("excludeImages", []))


def image_references(value, name):
    if isinstance(value, dict):
        return any(image_references(item, name) for item in value.values())
    if isinstance(value, list):
        return any(image_references(item, name) for item in value)
    return isinstance(value, str) and re.search(r"(?:^|/)" + re.escape(name) + r"(?:[:@]|$)", value) is not None


def validate_excluded_images(config):
    excluded = set(config["release"].get("excludeImages", []))
    references = copy.deepcopy(config)
    references["release"].pop("excludeImages", None)
    for name in excluded:
        if image_references(references, name):
            raise ValueError(f"release.excludeImages excludes image still referenced by the deployment: {name}")


class Deployment:
    def __init__(self, config, values, scratch):
        self.config, self.values, self.scratch = config, values, scratch
        self.documents, self.files, self.charts = {}, {}, {}
        self.bootstrap_documents = []
        self.existing_releases, self.namespaces = set(), {}
        self.mesh_uid = None
        self.cetiadmin_stop_pin = None
        self.cetiadmin_migration_receipt = None
        self.ingress = None
        self.defer_public_tls = False
        self.retained_database_pin = None
        # Only immutable references can reuse a successful registry inspection.
        # Tags and chart versions are mutable and are checked on every preflight.
        self.verified_image_digests = set()

    def run(self, command, *, env=None, input_text=None):
        if env is None and self.config["target"].get("kubeconfig"):
            env = {**os.environ, "KUBECONFIG": self.config["target"]["kubeconfig"]}
            if self.config.get("publicTLS", {}).get("enabled"):
                env["HELM_DRIVER"] = "secret"
        result = subprocess.run(command, cwd=ROOT, env=env, input=input_text, text=True, capture_output=True)
        if result.returncode:
            # Name the step, but do not echo Helm-rendered Secrets or external tool response bodies.
            # Only the offline render reports the chart's own refusal.
            refusals = CHART_REFUSAL.findall(result.stderr or "") if list(map(str, command[:2])) == ["helm", "template"] else []
            reason = "".join(f"; {template}: {message.strip()}" for template, message in refusals)
            raise ValueError(f"{step(command)} failed with exit {result.returncode}{reason}; deployment did not complete")
        return result.stdout

    def kubectl(self, *args, input_text=None):
        return self.run(["kubectl", "--context", self.config["target"]["context"], "--request-timeout=15s", *args], input_text=input_text)

    def helm(self, *args):
        return self.run(["helm", "--kube-context", self.config["target"]["context"], *args])

    def resource(self, kind, name, namespace=""):
        args = ["get", kind, name, "--ignore-not-found", "-o", "jsonpath={.metadata}"]
        if namespace:
            args += ["--namespace", namespace]
        raw = self.kubectl(*args)
        return json.loads(raw) if raw.strip() else None

    def selected_fields(self, kind, name, fields, namespace=""):
        projection = "jsonpath=" + "".join("{." + field + '}{"\\n"}' for field in fields)
        args = ["get", kind, name, "-o", projection]
        if namespace:
            args += ["--namespace", namespace]
        rows = self.kubectl(*args).removesuffix("\n").split("\n")
        if len(rows) != len(fields):
            raise ValueError("CetiAdmin selected fields could not be read safely")
        return rows

    def selected_rows(self, kind, fields, namespace="", all_namespaces=False):
        projection = 'jsonpath={range .items[*]}' + '{"\\t"}'.join("{." + field + "}" for field in fields) + '{"\\n"}{end}'
        args = ["get", kind, "-o", projection]
        if all_namespaces:
            args += ["--all-namespaces"]
        elif namespace:
            args += ["--namespace", namespace]
        rows = [row.split("\t") for row in self.kubectl(*args).splitlines()]
        if any(len(row) != len(fields) for row in rows):
            raise ValueError("CetiAdmin selected list fields could not be read safely")
        return rows

    def cetiadmin_binding(self):
        namespace = self.config["platform"]["namespace"]
        binding_name = "cetiadmin-binding-" + hashlib.sha256((namespace + "/" + CETIADMIN_SOURCE).encode()).hexdigest()[:24]
        source, checkpoint = self.resource("configmap", CETIADMIN_SOURCE, namespace), self.resource("configmap", binding_name, namespace)
        if not source or not checkpoint:
            raise ValueError("CetiAdmin requires its retained source and installation checkpoint; absence is not stopped")
        for metadata, name in ((source, CETIADMIN_SOURCE), (checkpoint, binding_name)):
            cetiadmin_identity(metadata, {"name": name, "namespace": namespace, "uid": metadata.get("uid")})
        annotations = source.get("annotations", {})
        if (annotations.get("meta.helm.sh/release-name"), annotations.get("meta.helm.sh/release-namespace")) != (self.config["platform"]["release"], namespace):
            raise ValueError("CetiAdmin source belongs to another release")
        raw = self.selected_fields("configmap", CETIADMIN_SOURCE, (r"data.config\.json",), namespace)[0]
        saved = self.selected_fields("configmap", binding_name, (r"data.binding\.json",), namespace)[0]
        try:
            current, state = json.loads(raw), json.loads(saved)
            cetiadmin_identity(source, state["source"])
            digest = cetiadmin_config_hash(current)
            if (type(state.get("schemaVersion")) is not int or state["schemaVersion"] != 1 or "hookTimeoutSeconds" in current
                    or any(field in state for field in ("targets", "readNamespaces", "accessHash"))
                    or state.get("configHash") != digest
                    or cetiadmin_config_hash(state["configuration"]) != digest
                    or checkpoint.get("annotations", {}).get(CETIADMIN_SOURCE_UID) != source["uid"]
                    or (state.get("recordUID") and state["recordUID"] != checkpoint["uid"])):
                raise ValueError()
        except (ValueError, KeyError, TypeError):
            raise ValueError("CetiAdmin source/configuration/checkpoint binding is not current") from None
        return source, checkpoint, current, state

    def cetiadmin_agent(self, identity):
        if not resource_name(identity.get("name")) or not object_uid(identity.get("uid")) or identity.get("namespace"):
            raise ValueError("CetiAdmin Agent identity is not an exact cluster-scoped name and UID")
        metadata = self.resource("agent", identity.get("name", ""))
        cetiadmin_identity(metadata, identity)
        fields = self.selected_fields("agent", identity["name"], CETIADMIN_AGENT_FIELDS)
        return metadata, {"suspended": fields[0] == "true", "phase": fields[1], "accessUrl": fields[2],
                          "request": json.loads(fields[3] or "null"), "maintenance": json.loads(fields[4] or "null")}

    def check_cetiadmin_writers(self, namespace):
        for kind, fields in CETIADMIN_WRITER_FIELDS.items():
            validate_cetiadmin_writers(kind, self.selected_rows(kind, fields, namespace))

    def check_cetiadmin_grants(self, account):
        username = "system:serviceaccount:" + account["namespace"] + ":" + account["name"]
        for kind in ("rolebindings", "clusterrolebindings"):
            for namespace, encoded in self.selected_rows(kind, ("metadata.namespace", "subjects"), all_namespaces=kind == "rolebindings"):
                for subject in json.loads(encoded or "[]"):
                    if ((subject.get("kind") == "ServiceAccount" and subject.get("name") == account["name"]
                         and (subject.get("namespace") or namespace) == account["namespace"])
                            or (subject.get("kind") == "User" and subject.get("name") == username)):
                        raise ValueError("CetiAdmin management access has not been revoked")

    def check_cetiadmin_maintenance(self):
        fields = ("metadata.generation", "spec.suspended", "spec.helm.maintenance", "status.helmMaintenance")
        for generation, suspended, raw_request, raw_maintenance in self.selected_rows("agents.cetirun.microsoft.com", fields):
            request, maintenance = json.loads(raw_request or "null"), json.loads(raw_maintenance or "null")
            if request is not None or (maintenance is not None and (maintenance.get("phase") != "Released"
                    or (suspended == "true" and str(maintenance.get("observedGeneration")) != generation))):
                raise ValueError("CetiAdmin target Helm maintenance is unresolved")
        markers = tuple(r"metadata.annotations.cetirun\.microsoft\.com/helm-maintenance-" + key
                        for key in ("agent", "agent-uid", "request-id"))
        if any(any(row) for row in self.selected_rows("namespaces", markers)):
            raise ValueError("CetiAdmin target still has a maintenance startup block")

    def check_cetiadmin_migration(self):
        self.cetiadmin_migration_receipt = None
        admin = self.values["platform"]["cetirun-operator"]["cetiadmin"]
        source = admin.get("migrationSource") if admin["enabled"] else None
        if not source:
            return
        metadata, observation = self.cetiadmin_agent(source["agent"])
        namespace = self.resource("namespace", source["namespace"]["name"])
        cetiadmin_identity(namespace, source["namespace"])
        cetiadmin_owned(namespace, source["agent"])
        runtime, account, gateway = self.selected_fields("agent", source["agent"]["name"],
            ("spec.agent.type", "spec.helm.values.serviceAccount.name", "status.aiGateway"))
        model = json.loads(gateway or "null")
        if (runtime not in ("hermes", "openclaw") or not observation["suspended"] or observation["phase"] != "Suspended"
                or observation["accessUrl"] or observation["request"] is not None
                or (observation["maintenance"] is not None and observation["maintenance"].get("phase") != "Released")
                or (model is not None and (model.get("blocked") is not True or model.get("routed", False)))):
            raise ValueError("CetiAdmin migration source must have confirmed retained suspension, blocked model access and resolved maintenance")
        self.check_cetiadmin_writers(namespace["name"])
        if account:
            if not resource_name(account, label=True):
                raise ValueError("CetiAdmin migration source ServiceAccount is invalid")
            sa = self.resource("serviceaccount", account, namespace["name"])
            cetiadmin_identity(sa, {"name": account, "namespace": namespace["name"], "uid": (sa or {}).get("uid")})
            self.check_cetiadmin_grants({"name": account, "namespace": namespace["name"]})
            self.cetiadmin_migration_receipt = metadata

    def check_cetiadmin_stopped(self):
        source, checkpoint, current, state = self.cetiadmin_binding()
        desired = copy.deepcopy(self.values["platform"]["cetirun-operator"]["cetiadmin"])
        desired["runtime"]["version"] = desired["runtime"].get("version") or self.config["release"]["version"]
        if (current.get("enabled") is not False or state.get("phase") != "Disabled" or state.get("hold") is not True
                or state.get("pendingCreate") or state.get("creation") or state.get("bootstrapReleasePending")
                or cetiadmin_config_hash(desired) != state["configHash"]):
            raise ValueError("CetiAdmin disable must already be confirmed for this unchanged configuration")
        operator = self.resource("deployment", "cetirun-operator", source["namespace"])
        if not operator or operator.get("deletionTimestamp") or not operator.get("uid"):
            raise ValueError("CetiAdmin stop requires the current Operator to remain available")
        replicas, raw_status, image, source_name = self.selected_fields("deployment", "cetirun-operator",
            ("spec.replicas", "status", 'spec.template.spec.containers[?(@.name=="manager")].image',
             'spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="CETIRUN_CETIADMIN_CONFIG_MAP")].value'), source["namespace"])
        status = json.loads(raw_status or "{}")
        if (not replicas.isdecimal() or int(replicas) < 1 or operator.get("generation", 0) < 1
                or status.get("observedGeneration") != operator["generation"]
                or any(status.get(field) != int(replicas) for field in ("updatedReplicas", "readyReplicas", "availableReplicas"))
                or not image or image != state.get("operatorImage") or source_name != CETIADMIN_SOURCE):
            raise ValueError("CetiAdmin stop requires the current Operator image and rollout to be Ready")
        self.check_cetiadmin_maintenance()
        agent, namespace, account = (state.get(field) or {} for field in ("agent", "namespace", "serviceAccount"))
        if agent.get("uid"):
            _, observation = self.cetiadmin_agent(agent)
            if observation["accessUrl"]:
                raise ValueError("CetiAdmin retained suspension and route shutdown are not confirmed")
            if namespace.get("uid"):
                if observation["phase"] != "Suspended":
                    raise ValueError("CetiAdmin retained suspension is not confirmed")
                if not resource_name(namespace.get("name"), label=True):
                    raise ValueError("CetiAdmin namespace identity is invalid")
                ns = self.resource("namespace", namespace["name"])
                cetiadmin_identity(ns, namespace)
                cetiadmin_owned(ns, agent)
                if account.get("namespace") != namespace["name"] or not resource_name(account.get("name"), label=True):
                    raise ValueError("CetiAdmin ServiceAccount namespace is not bound")
                sa = self.resource("serviceaccount", account.get("name", ""), namespace["name"])
                cetiadmin_identity(sa, account)
                cetiadmin_owned(sa, agent)
                self.check_cetiadmin_grants(account)
                self.check_cetiadmin_writers(namespace["name"])
            elif not observation["suspended"] or state.get("bootstrapped") or account.get("uid") or state.get("resources"):
                raise ValueError("CetiAdmin partial installation has unconfirmed resources")
        elif state.get("bootstrapped") or state.get("resources") or account.get("uid") or namespace.get("uid"):
            raise ValueError("CetiAdmin has unconfirmed retained identities")
        pin = (source["uid"], source["resourceVersion"], checkpoint["uid"], state["configHash"])
        if self.cetiadmin_stop_pin is not None and self.cetiadmin_stop_pin != pin:
            raise ValueError("CetiAdmin source or installation changed since preflight")
        for metadata in (source, checkpoint):
            latest = self.resource("configmap", metadata["name"], metadata["namespace"])
            if not latest or any(latest.get(field) != metadata.get(field) for field in ("uid", "resourceVersion")):
                raise ValueError("CetiAdmin stop observation changed during readback")
        self.cetiadmin_stop_pin = pin

    def check_operator_removal_controls(self):
        # CetiAdmin shutdown cannot authorize removal of independent governance.
        for kind, name in (("ValidatingWebhookConfiguration", "cetirun-sandbox-admission"),
                           ("ValidatingWebhookConfiguration", "cetirun-sandbox-pods"),
                           ("ValidatingWebhookConfiguration", "cetirun-sandbox-placement"),
                           ("MutatingWebhookConfiguration", "cetirun-sandbox-admission")):
            if self.resource(kind, name):
                raise ValueError("Preserve existing admission protection; Operator removal needs a separate approved governance handover")
        operator = self.current_control("cetirun-operator", "manager", ("CETIRUN_SANDBOX_SOURCES", "CONNECTIONS_MCP_NATIVE_ENABLED"))
        if operator is None:
            raise ValueError("Operator removal requires the existing Operator controls")
        sources = self.control_json(operator, "CETIRUN_SANDBOX_SOURCES")
        if (any(sources.get(field) for field in ("enabled", "nativeExecution", "plannedMigration", "coordinationConfigMapName", "coordinationConfigMapUID"))
                or operator["env"]["CONNECTIONS_MCP_NATIVE_ENABLED"].lower() == "true"):
            raise ValueError("Preserve existing native execution protection before Operator removal")
        coordination = ("CONFIGURATION_COORDINATION_NAMESPACE", "CONFIGURATION_COORDINATION_NAME", "CONFIGURATION_COORDINATION_UID")
        portal = self.current_control("cetirun-portal", "portal", coordination)
        mcp = self.current_control("cetirun-connections-mcp", "cetirun-connections-mcp", ("OPERATOR_NAMESPACE",))
        if (portal and any(portal["env"].values())) or (mcp and mcp["env"]["OPERATOR_NAMESPACE"]):
            raise ValueError("Preserve existing Portal coordination and native MCP identity before Operator removal")

    def render(self):
        offline = {**os.environ, "KUBECONFIG": os.devnull, "HELM_REGISTRY_CONFIG": os.devnull}
        if not self.charts:
            for entry in release_inventory(self.config)["charts"]:
                source = ROOT / entry["path"]
                shutil.copytree(source, self.scratch / entry["path"], ignore=shutil.ignore_patterns("charts", "__pycache__"))
            updates = RELEASE.VERSIONS.chart_updates(self.scratch,
                [entry["path"] for entry in release_inventory(self.config)["charts"]], self.config["release"]["version"])
            for path, content in updates.items():
                path.write_text(content)
            self.run(["helm", "dependency", "update", "--skip-refresh", str(self.scratch / CHARTS["platform"])], env=offline)
        for name, relative in CHARTS.items():
            self.charts[name] = self.scratch / relative
            self.files[name] = self.scratch / f"{name}-values.json"
            actual_values = copy.deepcopy(self.values[name])
            if name == "istio" and self.config["istio"].get("tlsMode") == "retain":
                del actual_values["tls"]
            self.files[name].write_text(json.dumps(actual_values))
            render_values = self.files[name]
            if name == "istio":
                synthetic = {**self.values[name], "cookieSecret": "offline-render-only-not-a-real-key"}
                if self.config["istio"].get("tlsMode") == "retain":
                    synthetic["tls"] = {"enabled": True, "cert": "offline-render-only", "key": "offline-render-only"}
                render_values = self.scratch / "istio-render-only.json"
                render_values.write_text(json.dumps(synthetic))
            section = self.config[name]
            raw = self.run(["helm", "template", section["release"], str(self.charts[name]), "--namespace", section["namespace"],
                            "--kube-version", "1.37.0", "--values", str(render_values)], env=offline)
            self.documents[name] = [doc for doc in yaml.safe_load_all(raw) if doc]
        tls_config = public_tls_config(self.config)
        if tls_config:
            if self.ingress is None:
                self.ingress = PUBLIC_TLS.Ingress(tls_config, self.scratch)
            self.ingress.render(self.config["domain"])
        if not self.values["platform"]["cetirun-operator"]["enabled"]:
            return
        # Render the bootstrap too, before any cluster writes.
        bootstrap = copy.deepcopy(self.values["platform"])
        admission = bootstrap["cetirun-operator"]["sandboxAdmission"]
        admission.update(enforce=False, coordinationConfigMapName="", coordinationConfigMapUID="")
        bootstrap["cetirun-portal"]["configurationCoordination"] = {"namespace": "", "name": "", "uid": ""}
        bootstrap["cetirun-portal"]["sandboxStorage"] = {"platformNamespace": "", "platformConfigMapName": "",
                                                             "defaultStorageClassName": ""}
        bootstrap["cetirun-operator"]["sandboxSources"].update(enabled=False, nativeExecution=False, plannedMigration=False,
            bootstrapOwnedPrerequisites=False, skillsCatalogConfigMapName="", placementConfigMapName="")
        bootstrap["cetirun-operator"]["connectionsMCP"]["nativeEnabled"] = False
        bootstrap["cetirun-portal"]["sandboxRuntimeDefaultsFromController"] = False
        # The Skill Marketplace needs the native sources that the bootstrap turns
        # off; the Operator chart refuses its setting without them.
        bootstrap["cetirun-operator"]["skillMarketplace"]["enabled"] = False
        bootstrap["cetirun-portal"]["skillMarketplace"]["enabled"] = False
        self.files["bootstrap"] = self.scratch / "bootstrap-values.json"
        self.files["bootstrap"].write_text(json.dumps(bootstrap))
        raw = self.run(["helm", "template", self.config["platform"]["release"], str(self.charts["platform"]),
                        "--namespace", self.config["platform"]["namespace"], "--kube-version", "1.37.0",
                        "--values", str(self.files["bootstrap"])], env=offline)
        self.bootstrap_documents = [doc for doc in yaml.safe_load_all(raw) if doc]

    def references(self):
        refs = set()
        for name, documents in self.documents.items():
            generated = {(doc["kind"], doc.get("metadata", {}).get("namespace", self.config[name]["namespace"]), doc["metadata"]["name"]) for doc in documents}
            for doc in documents:
                if self.is_public_tls(doc):
                    continue
                namespace = doc.get("metadata", {}).get("namespace", self.config[name]["namespace"])
                for item in walk(doc):
                    for field in ("secretKeyRef", "secretRef", "configMapKeyRef", "configMapRef"):
                        if field in item:
                            kind = "Secret" if field.startswith("secret") else "ConfigMap"
                            reference = item[field]
                            if (kind, namespace, reference["name"]) not in generated:
                                refs.add((kind, namespace, reference["name"], reference.get("key", "")))
                    if "secretName" in item:
                        if ("Secret", namespace, item["secretName"]) not in generated:
                            refs.add(("Secret", namespace, item["secretName"], ""))
                    for secret in item.get("imagePullSecrets", []):
                        refs.add(("Secret", namespace, secret["name"], ""))
        c, p = self.config, self.values["platform"]
        operator, mcp, broker = p["cetirun-operator"], p["cetirun-connections-mcp"], self.values["broker"]
        admin = operator["cetiadmin"]
        if admin["enabled"] and admin["monitor"]["enabled"] and admin["monitor"]["deliver"] == "slack":
            for key in ("SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"):
                refs.add(("Secret", "cetirun-" + admin["instanceName"], admin["monitor"]["slackSecretName"], key))
        for namespace, name, keys in (
            (c["istio"]["ingressNamespace"], "cetirun-tls", ("tls.crt", "tls.key")),
            (c["broker"]["namespace"], broker["tls"]["secretName"], (broker["tls"]["certKey"], broker["tls"]["keyKey"], broker["tls"]["clientCAKey"])),
            (c["platform"]["namespace"], operator["sandboxAdmission"]["tlsSecretName"], ("tls.crt", "tls.key")),
            (c["platform"]["namespace"], operator["connectionsMCP"]["caSecretName"], ("ca.crt",)),
            (c["platform"]["namespace"], mcp["tls"]["secretName"], ("tls.crt", "tls.key")),
            (c["platform"]["namespace"], mcp["broker"]["clientTLSSecretName"], ("tls.crt", "tls.key", "ca.crt")),
        ):
            for key in keys:
                refs.add(("Secret", namespace, name, key))
        for field in ("platformConfigMapName", "skillsCatalogConfigMapName", "placementConfigMapName"):
            if operator["sandboxSources"].get(field):
                refs.add(("ConfigMap", c["platform"]["namespace"], operator["sandboxSources"][field], ""))
        for field in ("coordinationConfigMapName", "placementConfigMapName"):
            if operator["sandboxAdmission"].get(field):
                refs.add(("ConfigMap", c["platform"]["namespace"], operator["sandboxAdmission"][field], ""))
        ai = p["cetirun-ai-gateway"]
        for entry in ai["models"]:
            for value in entry.get("litellm_params", {}).values():
                if isinstance(value, str) and value.startswith("os.environ/"):
                    refs.add(("Secret", c["platform"]["namespace"], ai["providerCredentials"]["existingSecret"], value.removeprefix("os.environ/")))
        storage_classes = [operator["customRuntime"]["storageClass"], c["sharedSkills"]["storageClassName"],
                           c["platform"].get("postgresqlStorage", {}).get("storageClassName")
                           or ai["postgresql"]["bundled"]["persistence"]["storageClass"]]
        marketplace = operator["skillMarketplace"]
        if marketplace.get("enabled"):
            # The repository credentials stay in an existing Secret of the platform namespace.
            repository = marketplace["repository"]
            for key in SKILL_MARKETPLACE_SECRET_KEYS[repository["auth"]]:
                refs.add(("Secret", c["platform"]["namespace"], repository["existingSecret"], key))
            storage_classes.append(marketplace["storage"]["storageClassName"])
        for storage in storage_classes:
            refs.add(("StorageClass", "", storage, ""))
        return sorted(refs)

    def secret_keys(self, namespace, name):
        output = self.kubectl("get", "secret", name, "--namespace", namespace, "-o",
                              'go-template={{range $key,$value := .data}}{{$key}}{{"\\n"}}{{end}}')
        return set(output.splitlines())

    def public_certificate(self, namespace, name, key):
        # JSONPath limits extraction, not the Kubernetes Secret GET or its RBAC.
        field = key.replace(".", "\\.")
        encoded = self.kubectl("get", "secret", name, "--namespace", namespace, "-o", f"jsonpath={{.data.{field}}}")
        try:
            return base64.b64decode(encoded, validate=True).decode("ascii")
        except (ValueError, UnicodeError) as error:
            raise ValueError(f"Invalid public certificate field {namespace}/{name}:{key}") from error

    def verify_certificate(self, label, certificate, ca, *, hostname=None, client_uri=None):
        leaf_file, ca_file = self.scratch / f"{label}-public.pem", self.scratch / f"{label}-ca.pem"
        leaf_file.write_text(certificate)
        ca_file.write_text(ca)
        command = ["openssl", "verify", "-CAfile", str(ca_file), "-untrusted", str(leaf_file),
                   "-purpose", "sslclient" if client_uri else "sslserver"]
        if hostname:
            command += ["-verify_hostname", hostname]
        self.run([*command, str(leaf_file)])
        if client_uri:
            extensions = self.run(["openssl", "x509", "-in", str(leaf_file), "-noout", "-ext", "subjectAltName"])
            if re.findall(r"\bURI:([^,\s]+)", extensions) != [client_uri]:
                raise ValueError("The Broker client certificate must carry exactly the configured MCP service URI")

    def check_tls(self):
        if shutil.which("openssl") is None:
            raise ValueError("openssl is required for certificate preflight; install it separately")
        c, p, b = self.config, self.values["platform"], self.values["broker"]
        ns, bns = c["platform"]["namespace"], c["broker"]["namespace"]
        operator, mcp = p["cetirun-operator"], p["cetirun-connections-mcp"]
        admission = operator["sandboxAdmission"]
        admission_ca = base64.b64decode(admission["caBundle"], validate=True).decode("ascii")
        self.verify_certificate("admission", self.public_certificate(ns, admission["tlsSecretName"], "tls.crt"), admission_ca,
                                hostname=f"cetirun-sandbox-admission.{ns}.svc")
        self.verify_certificate("mcp", self.public_certificate(ns, mcp["tls"]["secretName"], "tls.crt"),
                                self.public_certificate(ns, operator["connectionsMCP"]["caSecretName"], "ca.crt"),
                                hostname=f"cetirun-connections-mcp.{ns}.svc.cluster.local")
        self.verify_certificate("broker", self.public_certificate(bns, b["tls"]["secretName"], b["tls"]["certKey"]),
                                self.public_certificate(ns, mcp["broker"]["clientTLSSecretName"], "ca.crt"),
                                hostname=f"cetirun-agent-broker.{bns}.svc.cluster.local")
        self.verify_certificate("broker-client", self.public_certificate(ns, mcp["broker"]["clientTLSSecretName"], "tls.crt"),
                                self.public_certificate(bns, b["tls"]["secretName"], b["tls"]["clientCAKey"]),
                                client_uri=f"spiffe://cluster.local/ns/{b['toolService']['namespace']}/sa/{b['toolService']['serviceAccount']}")
        if self.defer_public_tls:
            return
        ingress = self.scratch / "ingress-public.pem"
        ingress.write_text(self.public_certificate(c["istio"]["ingressNamespace"], "cetirun-tls", "tls.crt"))
        self.run(["openssl", "x509", "-in", str(ingress), "-noout", "-checkend", "0"])
        checked = self.run(["openssl", "x509", "-in", str(ingress), "-noout", "-checkhost", c["domain"]])
        if f"Hostname {c['domain']} does match certificate" not in checked.splitlines():
            raise ValueError("The ingress certificate does not cover the configured domain")
        starts = self.run(["openssl", "x509", "-in", str(ingress), "-noout", "-startdate"])
        not_before = datetime.strptime(starts.strip().removeprefix("notBefore="), "%b %d %H:%M:%S %Y %Z").replace(tzinfo=timezone.utc)
        if not_before > datetime.now(timezone.utc):
            raise ValueError("The ingress certificate is not yet valid")

    def check_agent_mcp_conflicts(self):
        if not self.resource("CustomResourceDefinition", "agents.cetirun.microsoft.com"):
            return
        fields = '{"\\t"}'.join("{." + field + "}" for field in AGENT_ACCOUNT_FIELDS)
        output = self.kubectl("get", "agents.cetirun.microsoft.com", "-o", 'jsonpath={range .items[*]}' + fields + '{"\\n"}{end}')
        operator = self.values["platform"]["cetirun-operator"]
        defaults = operator["cetirun"]["helmValues"].get("serviceAccount", {})
        team_policy = {}
        for row in output.splitlines():
            columns = row.split("\t")
            if len(columns) != len(AGENT_ACCOUNT_FIELDS):
                raise ValueError("Cannot inspect existing Agent ServiceAccount settings safely")
            name, runtime, team, sandbox, account, role, token = columns
            if runtime == "custom" or (runtime or "openclaw") not in operator["connectionsMCP"]["runtimeTypes"] or sandbox:
                continue
            account, role = account or defaults.get("name", ""), role or defaults.get("role", "")
            token_required = token.lower() == "true" if token else defaults.get("automountToken") is True
            if not (account or role or token_required):
                continue
            receipt = self.cetiadmin_migration_receipt
            if receipt and receipt["name"] == name:
                current = self.resource("agent", name)
                cetiadmin_identity(current, {key: receipt[key] for key in ("name", "uid")})
                if current["resourceVersion"] != receipt["resourceVersion"]:
                    raise ValueError("CetiAdmin migration source changed after preflight")
                continue
            if self.kubectl("get", "agent", name, "-o", "go-template=" + AGENT_POLICY_MARKERS).strip():
                continue
            if team:
                if team not in team_policy:
                    team_policy[team] = self.kubectl("get", "team", team, "-o",
                        'go-template={{with .spec}}{{if or .agentPolicyRef .sandboxRefs .defaultSandboxRef}}protected{{end}}{{end}}').strip()
                if team_policy[team]:
                    continue
            raise ValueError(f"Agent {name}: MCP would replace its ServiceAccount/API token settings; only an exact stopped CetiAdmin migration source can be exempted")

    def check_excluded_agent_images(self):
        excluded = self.config["release"].get("excludeImages", [])
        if not excluded or not self.resource("CustomResourceDefinition", "agents.cetirun.microsoft.com"):
            return
        fields = ("metadata.name", "spec.agent.type", "spec.agent.image")
        output = self.kubectl("get", "agents.cetirun.microsoft.com", "-o",
                              'jsonpath={range .items[*]}' + '{"\\t"}'.join("{." + field + "}" for field in fields) + '{"\\n"}{end}')
        for row in output.splitlines():
            columns = row.split("\t")
            if len(columns) != len(fields):
                raise ValueError("Cannot inspect existing Agent image settings safely")
            name, runtime, image = columns
            if runtime == "custom" and any(image_references(image, excluded_name) for excluded_name in excluded):
                raise ValueError(f"Existing Custom Agent {name} still references an excluded image; migrate or remove it before deployment")

    def current_control(self, name, container, environment):
        namespace = self.config["platform"]["namespace"]
        if self.resource("Deployment", name, namespace) is None:
            return None
        prefix = '.spec.template.spec.containers[?(@.name=="' + container + '")]'
        args = self.kubectl("get", "deployment", name, "--namespace", namespace, "-o", "jsonpath={" + prefix + ".args}") if container == "admission" else "[]"
        values = {}
        for field in environment:
            encoded = self.kubectl("get", "deployment", name, "--namespace", namespace, "-o",
                                  "jsonpath-as-json={" + prefix + '.env[?(@.name=="' + field + '")]}')
            entries = json.loads(encoded or "[]")
            if not isinstance(entries, list) or len(entries) > 1 or any("valueFrom" in item or not isinstance(item.get("value"), str) for item in entries):
                raise ValueError(f"Cannot inspect existing {field} control value; external references need a separate approved procedure")
            values[field] = entries[0]["value"] if entries else ""
        try:
            arguments = json.loads(args or "[]")
        except ValueError as error:
            raise ValueError(f"Cannot inspect existing {name} control arguments") from error
        if not isinstance(arguments, list) or not all(isinstance(arg, str) for arg in arguments):
            raise ValueError(f"Cannot inspect existing {name} control arguments")
        return {"args": arguments, "env": values}

    def rendered_control(self, name, container):
        deployment = next(doc for doc in self.documents["platform"] if doc["kind"] == "Deployment" and doc["metadata"]["name"] == name)
        spec = next(item for item in deployment["spec"]["template"]["spec"]["containers"] if item["name"] == container)
        return {"args": spec.get("args", []), "env": {item["name"]: item.get("value", "") for item in spec.get("env", [])}}

    @staticmethod
    def control_json(control, field, *, required=False):
        encoded = (control or {}).get("env", {}).get(field, "")
        if not encoded:
            if required:
                raise ValueError("Cannot inspect existing admission configuration; preserve its effective protection before upgrading")
            return {}
        try:
            return mapping(json.loads(encoded), field)
        except (ValueError, TypeError) as error:
            raise ValueError(f"Cannot inspect existing {field} protection configuration") from error

    def check_existing_controls(self):
        platform, operator = self.values["platform"], self.values["platform"]["cetirun-operator"]
        if not operator["enabled"]:
            self.check_operator_removal_controls()
            self.enforced = True
            return
        admission = operator["sandboxAdmission"]
        has_webhook, has_pod_rules = False, False
        for kind, name in (("ValidatingWebhookConfiguration", "cetirun-sandbox-admission"),
                           ("ValidatingWebhookConfiguration", "cetirun-sandbox-pods"),
                           ("ValidatingWebhookConfiguration", "cetirun-sandbox-placement"),
                           ("MutatingWebhookConfiguration", "cetirun-sandbox-admission")):
            metadata = self.resource(kind, name)
            if metadata is None:
                continue
            owner = metadata.get("annotations", {})
            if (owner.get("meta.helm.sh/release-name"), owner.get("meta.helm.sh/release-namespace")) != (self.config["platform"]["release"], self.config["platform"]["namespace"]):
                raise ValueError(f"Refusing to modify admission protection owned by another release: {name}")
            has_webhook = True
            webhooks = json.loads(self.kubectl("get", kind, name, "-o", "jsonpath={.webhooks}"))
            if not isinstance(webhooks, list) or not webhooks:
                raise ValueError(f"Cannot inspect existing admission protection: {name}")
            for webhook in webhooks:
                for rule in webhook.get("rules", []):
                    if any(group in ("", "*") for group in rule.get("apiGroups", [])):
                        has_pod_rules |= any(resource in ("pods", "bindings", "*") or resource.startswith("pods/") for resource in rule.get("resources", []))
        if has_pod_rules and not admission["podsEnabled"]:
            raise ValueError("Preserve existing Pod admission protection with sandboxAdmission.podsEnabled=true")
        old_admission = self.current_control("cetirun-sandbox-admission", "admission", ("CETIRUN_SANDBOX_ADMISSION",))
        if has_webhook and old_admission is None:
            raise ValueError("Cannot inspect existing admission configuration behind the installed webhooks")
        self.enforced = has_webhook or old_admission is not None
        new_admission = self.rendered_control("cetirun-sandbox-admission", "admission")
        if old_admission is not None:
            old = self.control_json(old_admission, "CETIRUN_SANDBOX_ADMISSION", required=True)
            new = self.control_json(new_admission, "CETIRUN_SANDBOX_ADMISSION", required=True)
            for field in ("adminUsers", "adminGroups"):
                if set(old.get(field) or []) != set(new.get(field) or []):
                    raise ValueError(f"Preserve existing admission authority field {field}; changes need a separate approved procedure")
            if old.get("controllerUsername") != new.get("controllerUsername"):
                raise ValueError("Preserve existing admission authority field controllerUsername")

            def assignments(config):
                return {(item["team"], field, identity) for item in config.get("teamAssignments") or []
                        for field in ("users", "groups") for identity in item.get(field) or []}

            if assignments(old) != assignments(new):
                raise ValueError("Preserve existing admission authority field teamAssignments; changes need a separate approved procedure")
            if any(arg.startswith("--admission-pod-state-namespace") for arg in old_admission["args"]) and not admission["podsEnabled"]:
                raise ValueError("Preserve existing Pod admission protection with sandboxAdmission.podsEnabled=true")
            if old.get("validationMode") == "strict" and new.get("validationMode") != "strict":
                raise ValueError("Preserve existing strict admission protection with sandboxAdmission.validationMode=strict")
            for field in ("coordinationConfigMapName", "coordinationConfigMapUID", "placementConfigMapName",
                          "platformNamespace", "platformConfigMapName", "nativeExecution"):
                if old.get(field) and (type(old[field]) is not type(new.get(field)) or old[field] != new.get(field)):
                    raise ValueError(f"Preserve existing admission protection field {field}; changes need a separate approved procedure")

        native_fields = ("CETIRUN_SANDBOX_SOURCES", "CONNECTIONS_MCP_NATIVE_ENABLED", "CONNECTIONS_MCP_URL",
                         "WORKLOAD_TOKEN_AUDIENCE", "CONNECTIONS_MCP_CA_SECRET_NAME",
                         "CONNECTIONS_MCP_BROKER_NAMESPACE", "CONNECTIONS_MCP_BROKER_SERVICE_ACCOUNT")
        old_operator = self.current_control("cetirun-operator", "manager", native_fields)
        new_operator = self.rendered_control("cetirun-operator", "manager")
        old_sources = self.control_json(old_operator, "CETIRUN_SANDBOX_SOURCES")
        new_sources = self.control_json(new_operator, "CETIRUN_SANDBOX_SOURCES")
        for field in ("enabled", "nativeExecution", "plannedMigration", "bootstrapOwnedPrerequisites", "platformConfigMapName",
                      "homeClaimName", "skillsClaimName", "skillsCatalogConfigMapName", "placementConfigMapName",
                      "coordinationConfigMapName", "coordinationConfigMapUID", "admissionServiceAccount", "helpers"):
            if old_sources.get(field) and (type(old_sources[field]) is not type(new_sources.get(field)) or old_sources[field] != new_sources.get(field)):
                raise ValueError(f"Preserve existing native Sandbox protection field {field}; do not reset it through defaults")
        if old_sources.get("enabled") and old_admission is None:
            raise ValueError("Cannot inspect existing admission configuration for active native Sandbox sources")
        if old_operator and old_operator["env"]["CONNECTIONS_MCP_NATIVE_ENABLED"].lower() == "true":
            for field in native_fields[1:]:
                if old_operator["env"][field] != new_operator["env"].get(field, ""):
                    raise ValueError(f"Preserve existing native MCP protection field {field}")
        if old_operator:
            for field in ("CONNECTIONS_MCP_BROKER_NAMESPACE", "CONNECTIONS_MCP_BROKER_SERVICE_ACCOUNT"):
                if old_operator["env"][field] and old_operator["env"][field] != new_operator["env"].get(field, ""):
                    raise ValueError(f"Preserve existing Broker storage identity field {field}, including when native MCP is disabled")

        coordination = ("CONFIGURATION_COORDINATION_NAMESPACE", "CONFIGURATION_COORDINATION_NAME", "CONFIGURATION_COORDINATION_UID")
        old_portal = self.current_control("cetirun-portal", "portal", coordination)
        new_portal = self.rendered_control("cetirun-portal", "portal")
        if old_portal:
            for field in coordination:
                if old_portal["env"][field] and old_portal["env"][field] != new_portal["env"].get(field, ""):
                    raise ValueError(f"Preserve existing Portal coordination protection field {field}")
        old_mcp = self.current_control("cetirun-connections-mcp", "cetirun-connections-mcp", ("OPERATOR_NAMESPACE",))
        if old_mcp and old_mcp["env"]["OPERATOR_NAMESPACE"]:
            current_peer = self.kubectl("get", "deployment", "cetirun-connections-mcp", "--namespace", self.config["platform"]["namespace"],
                "-o", "jsonpath={.spec.template.metadata.labels.sandbox\\.cetirun\\.microsoft\\.com/peer-uid}")
            native = platform["cetirun-connections-mcp"]["nativeSandbox"]
            if not native["enabled"] or native["operatorNamespace"] != old_mcp["env"]["OPERATOR_NAMESPACE"] or not current_peer or native["peerUID"] != current_peer:
                raise ValueError("Preserve existing native MCP peer identity and operator namespace")

    def check_external_istio(self, document):
        kind, metadata = document["kind"], document["metadata"]
        namespace, name = metadata.get("namespace", self.config["istio"]["namespace"]), metadata["name"]

        def read(field):
            return self.kubectl("get", kind, name, "--namespace", namespace, "-o", "jsonpath={" + field + "}")

        def match(field, actual, expected):
            if actual != expected:
                raise ValueError(f"External Istio {kind} {namespace}/{name} {field} differs from the configured wiring")

        if kind in ("Gateway", "AuthorizationPolicy", "EnvoyFilter", "VirtualService", "ServiceEntry", "NetworkPolicy"):
            match("spec", json.loads(read(".spec") or "null"), document["spec"])
        elif kind == "Service" and name in ("oauth2-proxy", "oauth2-proxy-sessions"):
            current = json.loads(read(".spec") or "{}")
            match("selector", current.get("selector"), document["spec"]["selector"])
            match("type", current.get("type", "ClusterIP"), "ClusterIP")
            ports = [{**port, "protocol": port.get("protocol", "TCP")} for port in document["spec"]["ports"]]
            current_ports = [{**port, "protocol": port.get("protocol", "TCP")} for port in current.get("ports", [])]
            match("ports", current_ports, ports)
        elif kind == "Deployment" and name in ("oauth2-proxy", "oauth2-proxy-sessions"):
            template = document["spec"]["template"]
            match("selector", json.loads(read(".spec.selector") or "{}"), document["spec"]["selector"])
            if name == "oauth2-proxy":
                match("serviceAccountName", read(".spec.template.spec.serviceAccountName"), template["spec"]["serviceAccountName"])
            else:
                match("volumes", json.loads(read(".spec.template.spec.volumes") or "[]"), template["spec"]["volumes"])
            labels = json.loads(read(".spec.template.metadata.labels") or "{}")
            match("workload labels", {key: labels.get(key) for key in template["metadata"]["labels"]}, template["metadata"]["labels"])
            container_name = "oauth2-proxy" if name == "oauth2-proxy" else "valkey"
            container = next(item for item in template["spec"]["containers"] if item["name"] == container_name)
            prefix = f'.spec.template.spec.containers[?(@.name=="{container_name}")]'
            match("image", read(prefix + ".image"), container["image"])
            for field in ("command", "args", "ports", "envFrom"):
                match(field, json.loads(read(prefix + "." + field) or "[]"), container.get(field, []))
            match("environment names", sorted(read(prefix + ".env[*].name").split()), sorted(item["name"] for item in container["env"]))
            for entry in container["env"]:
                field = prefix + '.env[?(@.name=="' + entry["name"] + '")]'
                if "valueFrom" in entry:
                    match("environment reference " + entry["name"], json.loads(read(field + ".valueFrom") or "null"), entry["valueFrom"])
                else:
                    match("environment " + entry["name"], read(field + ".value"), entry["value"])
        elif kind == "ServiceAccount" and name == "oauth2-proxy":
            match("Workload Identity client", read(r".metadata.annotations.azure\.workload\.identity/client-id"),
                  metadata.get("annotations", {}).get("azure.workload.identity/client-id", ""))
        elif kind == "PersistentVolumeClaim" and name == "oauth2-proxy-sessions":
            match("accessModes", json.loads(read(".spec.accessModes") or "[]"), document["spec"]["accessModes"])
            if document["spec"].get("storageClassName"):
                match("storageClassName", read(".spec.storageClassName"), document["spec"]["storageClassName"])

    def is_public_tls(self, doc):
        def identity(value):
            metadata = value.get("metadata", {})
            return value.get("apiVersion"), value.get("kind"), metadata.get("namespace"), metadata.get("name")
        return self.ingress is not None and identity(doc) in {identity(value) for value in self.ingress.documents}

    def observability_tls_secret(self):
        """Name of the ADX-Mon ingestor's TLS Secret, as the observability chart names it."""
        chart = self.values["platform"].get(OBSERVABILITY, {})
        name = ((chart.get("ingestor") or {}).get("tls") or {}).get("secretName")
        return name or f"{OBSERVABILITY_INGESTOR}-tls"

    def check_retained_postgresql(self):
        storage = self.config["platform"].get("postgresqlStorage")
        if not storage:
            return
        if "platform" not in self.existing_releases:
            raise ValueError("Retained PostgreSQL requires the existing platform Helm release; this mode cannot install a new release")
        namespace, release = (self.config["platform"][key] for key in ("namespace", "release"))
        name, claim = "cetirun-ai-gateway-postgresql", "data-cetirun-ai-gateway-postgresql-0"
        identities = {}

        def identity(kind, resource, ns=namespace):
            metadata = self.resource(kind, resource, ns)
            if not metadata or not metadata.get("uid") or metadata.get("deletionTimestamp"):
                raise ValueError(f"Retained PostgreSQL requires an existing, non-terminating {kind}")
            identities[(kind, ns, resource)] = metadata["uid"]
            return metadata

        metadata = identity("StatefulSet", name)
        annotations = metadata.get("annotations", {})
        if (annotations.get("meta.helm.sh/release-name"), annotations.get("meta.helm.sh/release-namespace")) != (release, namespace):
            raise ValueError("Retained PostgreSQL StatefulSet belongs to another release")
        identity("PersistentVolumeClaim", claim)
        identity("Secret", name)
        identity("StorageClass", storage["storageClassName"], "")
        desired = next(doc for doc in self.documents["platform"]
                       if doc["kind"] == "StatefulSet" and doc["metadata"]["name"] == name)["spec"]["volumeClaimTemplates"]
        templates = json.loads(self.selected_fields("StatefulSet", name, ("spec.volumeClaimTemplates",), namespace)[0])

        def normalized(templates):
            result = copy.deepcopy(templates)
            for template in result:
                # Kubernetes adds these defaults to embedded PVC templates. Do
                # not discard arbitrary fields, explicit storageClassName, or
                # a nonempty status/metadata field when comparing templates.
                if template.get("metadata", {}).get("creationTimestamp", "missing") is None:
                    del template["metadata"]["creationTimestamp"]
                if template.get("spec", {}).get("volumeMode") == "Filesystem":
                    del template["spec"]["volumeMode"]
                if template.get("status") in ({}, {"phase": "Pending"}):
                    del template["status"]
            return result

        if normalized(templates) != normalized(desired):
            raise ValueError("Retained PostgreSQL volumeClaimTemplates differ from the rendered chart; refusing an immutable storage change")
        raw_spec, phase = self.selected_fields("PersistentVolumeClaim", claim, ("spec", "status.phase"), namespace)
        spec = json.loads(raw_spec)
        expected = desired[0]["spec"]
        if (phase != "Bound" or not resource_name(spec.get("volumeName"))
                or spec.get("storageClassName") != storage["storageClassName"]
                or spec.get("accessModes") != expected["accessModes"]
                or spec.get("volumeMode", "Filesystem") != "Filesystem"
                or spec.get("resources", {}).get("requests") != expected["resources"]["requests"]):
            raise ValueError("Retained PostgreSQL PVC must be Bound with the expected class, access mode, and storage request")
        volume = spec["volumeName"]
        identity("PersistentVolume", volume, "")
        binding, volume_class = self.selected_fields("PersistentVolume", volume, ("spec.claimRef", "spec.storageClassName"))
        binding = json.loads(binding)
        if (binding.get("uid"), binding.get("namespace"), binding.get("name"), volume_class) != (
                identities[("PersistentVolumeClaim", namespace, claim)], namespace, claim, storage["storageClassName"]):
            raise ValueError("Retained PostgreSQL PV does not bind the exact retained PVC")
        for (kind, ns, resource), uid in identities.items():
            current = self.resource(kind, resource, ns) or {}
            if current.get("uid") != uid or current.get("deletionTimestamp"):
                raise ValueError("Retained PostgreSQL identity changed while checking storage")
        snapshot = (identities, templates, spec)
        if self.retained_database_pin is not None and snapshot != self.retained_database_pin:
            raise ValueError("Retained PostgreSQL identity or storage changed during deployment")
        self.retained_database_pin = copy.deepcopy(snapshot)

    def check_kubectl_version(self):
        versions = json.loads(self.kubectl("version", "-o", "json"))
        parsed = {}
        for component in ("clientVersion", "serverVersion"):
            version = versions.get(component, {})
            major = re.fullmatch(r"[0-9]+", str(version.get("major", "")))
            minor = re.fullmatch(r"([0-9]+)\+?", str(version.get("minor", "")))
            if not major or not minor:
                raise ValueError("kubectl version must report parseable clientVersion and serverVersion")
            parsed[component] = (int(major[0]), int(minor[1]))
        client, server = parsed["clientVersion"], parsed["serverVersion"]
        if server < (1, 37):
            raise ValueError("Enforced admission requires Kubernetes 1.37 or newer")
        if client[0] != server[0] or abs(client[1] - server[1]) > 1:
            raise ValueError(f"kubectl {client[0]}.{client[1]} is outside the supported one-minor skew of Kubernetes "
                             f"{server[0]}.{server[1]}; select a compatible kubectl on PATH before retrying")

    def inspect_image(self, reference):
        immutable = re.fullmatch(r"\S+@sha256:[0-9a-f]{64}", reference) is not None
        if immutable and reference in self.verified_image_digests:
            return
        self.run(["docker", "buildx", "imagetools", "inspect", reference])
        if immutable:
            self.verified_image_digests.add(reference)

    def preflight(self):
        c = self.config
        self.existing_releases.clear()
        if self.ingress:
            self.ingress.ingress_ownership_check()
        target = self.kubectl("config", "view", "--minify", "-o", 'jsonpath={.contexts[0].context.cluster}{"\\n"}{.clusters[0].cluster.server}')
        if target.splitlines() != [c["target"]["cluster"], c["target"]["server"]]:
            raise ValueError("The selected context does not match target.cluster and target.server")
        if (self.resource("namespace", "kube-system") or {}).get("uid") != c["target"]["clusterUID"]:
            raise ValueError("target.clusterUID does not match this cluster's kube-system UID")
        self.check_kubectl_version()
        for kind in ("gateways.networking.istio.io", "virtualservices.networking.istio.io", "authorizationpolicies.security.istio.io"):
            if not self.resource("customresourcedefinition", kind):
                raise ValueError(f"Managed Istio prerequisite is missing: {kind}")
        for field in ("controlPlane", "ingress"):
            namespace, deployment = c["istio"][f"{field}Namespace"], c["istio"][f"{field}Deployment"]
            # A missing Deployment would only fail rollout status, whose output is
            # withheld. AKS managed Istio adds the revision to these names.
            if not self.resource("deployment", deployment, namespace):
                revisioned = f"{deployment}-{c['istio']['revision']}"
                hint = f"; AKS managed Istio adds the revision to the name, and {revisioned} exists" \
                    if self.resource("deployment", revisioned, namespace) else ""
                raise ValueError(f"The configured istio.{field}Deployment {namespace}/{deployment} does not exist{hint}")
            self.kubectl("rollout", "status", f"deployment/{deployment}", "--namespace", namespace, "--timeout=180s")
        if not self.resource("serviceaccount", c["istio"]["ingressServiceAccount"], c["istio"]["ingressNamespace"]):
            raise ValueError("The configured Istio ingress ServiceAccount is missing")
        for name in CHARTS:
            section = c[name]
            metadata = self.resource("namespace", section["namespace"])
            self.namespaces[section["namespace"]] = metadata
            label = (metadata or {}).get("labels", {}).get("istio.io/rev")
            if name != "istio" and label and label != c["istio"]["revision"]:
                raise ValueError(f"{section['namespace']} uses another Istio revision; refusing to replace it")
            releases = json.loads(self.helm("list", "--deployed", "--failed", "--pending", "--uninstalling", "--uninstalled", "--superseded",
                                            "--namespace", section["namespace"], "--filter", f"^{section['release']}$", "-o", "json"))
            if releases:
                if releases[0]["status"].startswith("pending-"):
                    raise ValueError(f"Release {section['release']} has an unfinished operation")
                if (name == "platform" and c["platform"].get("postgresqlStorage")
                        and releases[0]["status"] not in ("deployed", "failed")):
                    raise ValueError("Retained PostgreSQL requires a deployed or failed platform Helm release")
                if name == "platform":
                    if self.ingress:
                        if self.ingress.platform_state == "full":
                            self.existing_releases.add(name)
                    else:
                        previous = json.loads(self.helm("get", "values", section["release"], "--namespace", section["namespace"], "-o", "json")) or {}
                        if previous.get("publicTLS", {}).get("enabled"):
                            raise ValueError("Cannot disable managed publicTLS during a platform upgrade; migrate it separately")
                        self.existing_releases.add(name)
                else:
                    self.existing_releases.add(name)
            for doc in self.documents[name]:
                if self.is_public_tls(doc):
                    continue  # Fully qualified API reads and owner/spec checks belong to the TLS phase.
                kind, metadata = doc["kind"], doc["metadata"]
                namespace = "" if kind in CLUSTER_KINDS else metadata.get("namespace", section["namespace"])
                qualified = "gateways.networking.istio.io" if kind == "Gateway" else kind
                current = self.resource(qualified, metadata["name"], namespace)
                if current:
                    annotations = current.get("annotations", {})
                    if (annotations.get("meta.helm.sh/release-name"), annotations.get("meta.helm.sh/release-namespace")) != (section["release"], section["namespace"]):
                        raise ValueError(f"Refusing to adopt {kind} {namespace}/{metadata['name']} from another owner")
                    if name == "platform" and self.ingress and self.ingress.platform_state != "full":
                        raise ValueError("Certificate bootstrap has surviving platform resources; refusing regeneration or downgrade")
                    if kind == "Secret" and not set(doc.get("data", {})).issubset(self.secret_keys(namespace, metadata["name"])):
                        raise ValueError(f"Retained Secret {namespace}/{metadata['name']} is missing required keys; refusing regeneration")
                    if name == "istio" and c["istio"].get("mode", "manage") == "external" and kind != "Secret":
                        self.check_external_istio(doc)
                elif name == "istio" and c["istio"].get("mode", "manage") == "external" and kind != "Secret":
                    raise ValueError(f"External Istio is missing required {kind} {namespace}/{metadata['name']}")
                elif name in self.existing_releases and kind == "Secret":
                    adding_sessions = (name == "istio" and c["istio"].get("mode", "manage") == "manage"
                        and metadata["name"] == "oauth2-proxy-sessions"
                        and not self.resource("Deployment", "oauth2-proxy-sessions", namespace)
                        and not self.resource("PersistentVolumeClaim", "oauth2-proxy-sessions", namespace))
                    # Turning the ADX-Mon collection on for an installed platform adds the
                    # ingestor's TLS Secret; once the ingestor exists, the Secret is kept.
                    adding_observability = (name == "platform" and metadata["name"] == self.observability_tls_secret()
                        and not self.resource("StatefulSet", OBSERVABILITY_INGESTOR, namespace))
                    if not adding_sessions and not adding_observability:
                        raise ValueError(f"Retained Secret {namespace}/{metadata['name']} is missing; refusing regeneration")
        if not self.values["platform"]["cetirun-operator"]["enabled"]:
            if "platform" not in self.existing_releases:
                raise ValueError("Operator removal requires the existing platform release")
            self.check_cetiadmin_stopped()
        self.check_existing_controls()
        self.check_cetiadmin_migration()
        self.check_agent_mcp_conflicts()
        self.check_excluded_agent_images()
        for kind, namespace, name, key in self.references():
            if self.defer_public_tls and (kind, namespace, name) == ("Secret", c["istio"]["ingressNamespace"], "cetirun-tls"):
                continue
            metadata = self.resource(kind, name, namespace)
            if not metadata:
                raise ValueError(f"Required {kind} reference is missing: {namespace}/{name}")
            admission = self.values["platform"]["cetirun-operator"]["sandboxAdmission"]
            if kind == "ConfigMap" and name == admission["coordinationConfigMapName"] and metadata.get("uid") != admission["coordinationConfigMapUID"]:
                raise ValueError("The admission coordination ConfigMap UID does not match the configured authority")
            if kind == "Secret" and key and key not in self.secret_keys(namespace, name):
                raise ValueError(f"Required Secret key is missing: {namespace}/{name}:{key}")
        mesh = self.kubectl("get", "configmap", c["istio"]["meshConfigMap"], "--namespace", c["istio"]["controlPlaneNamespace"], "--ignore-not-found", "-o", "json")
        if mesh.strip():
            document = json.loads(mesh)
            self.mesh_uid = document["metadata"]["uid"]
            _, changed = MESH.configmap_patch(document, self.oauth_service())
            if changed and c["istio"].get("mode", "manage") == "external":
                raise ValueError("External Istio has no matching OAuth2 provider/header configuration")
        else:
            # The helper creates a missing ConfigMap. AKS managed Istio reads only
            # istio-shared-configmap-<revision>, so another name leaves the OAuth2
            # provider out of the mesh.
            shared = f"istio-shared-configmap-{c['istio']['revision']}"
            if shared != c["istio"]["meshConfigMap"] and self.resource("configmap", shared, c["istio"]["controlPlaneNamespace"]):
                raise ValueError(f"The configured istio.meshConfigMap {c['istio']['controlPlaneNamespace']}/{c['istio']['meshConfigMap']} "
                                 f"does not exist, and AKS managed Istio reads {shared}, which exists")
            if c["istio"].get("mode", "manage") == "external":
                raise ValueError("External Istio requires the existing OAuth2 mesh provider")
        if c["istio"].get("mode", "manage") == "external" and "istio" not in self.existing_releases:
            raise ValueError("istio.mode=external requires the named existing CetiRun Istio release")
        tls = self.resource("Secret", "cetirun-tls", c["istio"]["ingressNamespace"])
        owner = (tls or {}).get("annotations", {})
        owns_tls = (owner.get("meta.helm.sh/release-name"), owner.get("meta.helm.sh/release-namespace")) == (c["istio"]["release"], c["istio"]["namespace"])
        if c["istio"].get("tlsMode", "external") == "retain":
            if not owns_tls or "istio" not in self.existing_releases:
                raise ValueError("tlsMode=retain requires the existing release's chart-owned cetirun-tls Secret")
        elif owns_tls and c["istio"].get("mode", "manage") == "manage":
            raise ValueError("The Istio release owns cetirun-tls; use tlsMode=retain or a separately approved ownership migration")
        database = "cetirun-ai-gateway-postgresql"
        if self.resource("PersistentVolumeClaim", f"data-{database}-0", c["platform"]["namespace"]) and not self.resource("Secret", database, c["platform"]["namespace"]):
            raise ValueError("Retained PostgreSQL data has no password Secret; refusing to generate a replacement")
        self.check_retained_postgresql()
        self.check_tls()
        for entry in release_inventory(c)["images"]:
            self.inspect_image(f"{c['release']['registry']}/{entry['name']}:{c['release']['version']}")
        for image in self.values["platform"]["cetirun-operator"]["customRuntime"]["approvedImages"]:
            self.inspect_image(image)
        admin = self.values["platform"]["cetirun-operator"]["cetiadmin"]
        if admin["enabled"] and admin["runtime"].get("image"):
            self.inspect_image(admin["runtime"]["image"])
        for entry in release_inventory(c)["charts"]:
            self.run(["helm", "show", "chart", f"oci://{c['release']['registry']}/helm/{entry['name']}", "--version", c["release"]["version"]])

    def oauth_service(self):
        return f"oauth2-proxy.{self.config['istio']['namespace']}.svc.cluster.local"

    def upgrade(self, name, values=None):
        section = self.config[name]
        if name == "platform":
            self.check_retained_postgresql()
        if name == "platform" and not self.values["platform"]["cetirun-operator"]["enabled"]:
            self.check_cetiadmin_stopped()
        elif name == "platform":
            self.check_cetiadmin_migration()
        print(f"==> Upgrade {section['namespace']}/{section['release']}", flush=True)
        timeout = "10m"
        if name == "platform":
            timeout = str(max(600, self.values["platform"]["cetirun-operator"]["cetiadmin"]["hookTimeoutSeconds"] + 60)) + "s"
        args = ["upgrade", "--install", section["release"], str(self.charts[name]), "--namespace", section["namespace"],
                "--values", str(self.files[values or name]), "--timeout", timeout]
        if name == "istio" and section.get("tlsMode") == "retain":
            args.append("--reuse-values")
        else:
            args.append("--reset-values")
        if not self.config["release"].get("clientOpenAPIValidation", True):
            args.append("--disable-openapi-validation")
        self.helm(*args)
        if name == "platform":
            self.check_retained_postgresql()

    def ready(self, name, documents=None):
        section = self.config[name]
        workloads = [doc for doc in (self.documents[name] if documents is None else documents)
                     if doc["kind"] in ("Deployment", "StatefulSet")]
        workloads.sort(key=lambda doc: (doc["kind"] != "StatefulSet", "valkey" not in doc["metadata"]["name"]))
        for doc in workloads:
            namespace, workload = doc["metadata"].get("namespace", section["namespace"]), doc["metadata"]["name"]
            self.kubectl("rollout", "status", f"{doc['kind'].lower()}/{workload}", "--namespace", namespace, "--timeout=600s")
            if workload in ("cetirun-operator", "cetirun-portal", "cetirun-agent-broker", "cetirun-connections-mcp"):
                selector = ",".join(f"{key}={value}" for key, value in doc["spec"]["selector"]["matchLabels"].items())
                output = self.kubectl("get", "pods", "--namespace", namespace, "--selector", selector, "-o",
                                      'jsonpath={range .items[*]}{.spec.containers[*].name}{" "}{.spec.initContainers[*].name}{"\\n"}{end}')
                if not output.strip() or any("istio-proxy" not in line.split() for line in output.splitlines()):
                    raise ValueError(f"{workload} is missing the required Istio sidecar")
                if workload == "cetirun-agent-broker" and any("auth-sdk" not in line.split() for line in output.splitlines()):
                    raise ValueError("Broker pods are missing the Auth SDK")

    def resolve_public_domain(self, domain):
        self.config, self.values = configuration(self.config["_source"], domain=domain)
        self.render()
        self.preflight()

    def execute(self):
        for key in ("registry", "version"):
            print(f"Deployment {key}: {self.config['release'][key]} "
                  f"(from {self.config['_releaseOrigins'][key]})", flush=True)
        if self.ingress:
            self.ingress.load_state()
            self.ingress.preflight()
            self.defer_public_tls = True
            if self.ingress.domain:
                self.resolve_public_domain(self.ingress.domain)
            else:
                self.preflight()
            result = self.ingress.prepare(resolved=self.resolve_public_domain, complete=self.execute_platform)
            print(json.dumps(result, indent=2))
        else:
            self.execute_platform()

    def execute_platform(self):
        self.defer_public_tls = False
        self.preflight()
        c = self.config
        for name in CHARTS:
            namespace = c[name]["namespace"]
            if self.namespaces[namespace] is None:
                self.kubectl("create", "namespace", namespace)
            if name != "istio" and (self.namespaces[namespace] or {}).get("labels", {}).get("istio.io/rev") != c["istio"]["revision"]:
                self.kubectl("label", "namespace", namespace, f"istio.io/rev={c['istio']['revision']}", "--overwrite=false")
        if c["istio"].get("mode", "manage") == "manage":
            self.upgrade("istio")
        self.ready("istio")
        command = [sys.executable, "-B", str(ROOT / "scripts/ensure-oauth2-proxy-id-token-header.py"),
                   "--context", c["target"]["context"], "--namespace", c["istio"]["controlPlaneNamespace"],
                   "--name", c["istio"]["meshConfigMap"], "--service", self.oauth_service()]
        if self.mesh_uid:
            command += ["--uid", self.mesh_uid]
        if c["istio"].get("mode", "manage") == "manage":
            self.run(command)
        self.upgrade("broker")
        self.ready("broker")
        if not self.values["platform"]["cetirun-operator"]["enabled"]:
            self.upgrade("platform")
            self.ready("platform")
            print("Operator subchart removed after current CetiAdmin stop checks. Retained Agent and HOME data were not deleted.")
            return
        # Never withdraw existing admission protection during an upgrade.
        if not self.enforced:
            self.upgrade("platform", "bootstrap")
            # Wait for what the bootstrap installs: it has no Skill Marketplace.
            self.ready("platform", self.bootstrap_documents)
        else:
            self.kubectl("rollout", "status", "deployment/cetirun-sandbox-admission", "--namespace", c["platform"]["namespace"], "--timeout=180s")
        self.upgrade("platform")
        self.ready("platform")
        probe = {"apiVersion": "cetirun.microsoft.com/v1alpha1", "kind": "Team",
                 "metadata": {"name": "cetirun-admission-deployment-check"}, "spec": {}}
        if c["release"].get("clientOpenAPIValidation", True):
            self.kubectl("create", "--dry-run=server", "-f", "-", input_text=json.dumps(probe))
        else:
            self.kubectl("create", "--raw", "/apis/cetirun.microsoft.com/v1alpha1/teams?dryRun=All&fieldValidation=Strict",
                         "-f", "-", input_text=json.dumps(probe))
        print("Platform releases and their Helm checks are ready, and the admission dry-run succeeded. CetiAdmin lifecycle is owned by the Operator.")
        print("Not qualified by this check: live model/OAuth calls, native Sandbox execution/physical fencing, or end-to-end CetiAdmin operations.")

    def plan(self):
        references = {(kind, namespace, name) for kind, namespace, name, _ in self.references()}
        not_qualified = ["native Sandbox execution and physical fencing", "end-to-end CetiAdmin operations", "live model/OAuth calls"]
        if self.values["platform"]["cetirun-operator"]["skillMarketplace"].get("enabled"):
            # Its readiness does not wait for a repository sync.
            not_qualified.append("Skill Marketplace repository access and Team publication")
        if self.values["platform"][OBSERVABILITY].get("enabled"):
            # The deployment does not reach the Kusto cluster or check the
            # ingestor identity's roles; the ingestor does at its start.
            not_qualified.append("ADX ingestion with the ingestor identity")
        return {"target": {key: self.config["target"][key] for key in ("context", "cluster", "server", "clusterUID")},
                "release": {key: self.config["release"][key] for key in ("registry", "version", "gitSHA")},
                "releaseOrigins": self.config["_releaseOrigins"],
                "publicTLS": self.ingress.render() if self.ingress else {"enabled": False},
                "domain": self.config["domain"],
                "domainIsPlaceholder": bool(self.ingress and not self.ingress.domain and not self.config["_source"].get("domain")),
                "stages": (["full prerequisites except the managed public certificate", "DNS, upstream cert-manager, umbrella certificate bootstrap",
                            "wait for staging and production certificates; full releases never enter certificate-only mode"] if self.ingress else []) + ["verify target, managed Istio, references, ownership and all artifacts",
                           "confirm retained CetiAdmin shutdown before any Operator removal; recheck immediately before its upgrade",
                           "CetiRun Istio/OAuth2 release and mesh provider", "isolated Broker/Auth SDK release",
                           "platform data services, Operator, Portal Chat, MCP, Custom/terminal and Memory bindings",
                           "dedicated CetiAdmin protection; global Sandbox enforcement after admission readiness",
                           "all workloads and CetiAdmin installation checks ready; admission server dry-run"],
                "releases": {name: {"namespace": self.config[name]["namespace"], "release": self.config[name]["release"],
                                     "workloads": [doc["metadata"]["name"] for doc in docs if doc["kind"] in ("Deployment", "StatefulSet")]}
                             for name, docs in self.documents.items()},
                "artifacts": {kind: [entry["name"] for entry in entries] for kind, entries in release_inventory(self.config).items() if kind in ("images", "charts")},
                "referenceCounts": dict(Counter(kind for kind, _, _ in references)),
                "modelRouting": {"mode": self.config.get("modelRouting", {}).get("mode", "auto")},
                "clientOpenAPIValidation": self.config["release"].get("clientOpenAPIValidation", True),
                "cetiadmin": {"enabled": self.values["platform"]["cetirun-operator"]["cetiadmin"]["enabled"],
                              "operatorRemovalRequested": not self.values["platform"]["cetirun-operator"]["enabled"],
                              "stopConfirmed": False},
                "preserves": ["existing Agents and HOME/PVCs except the explicit CetiAdmin retained lifecycle", "OAuth cookie and database keys", "other Helm releases"],
                "notQualified": not_qualified}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, type=Path, help="explicit environment YAML; never sourced as shell code")
    parser.add_argument("--registry", help="override release.registry (or REGISTRY) without editing the environment file")
    parser.add_argument("--version", help="override release.version (or VERSION) without editing the environment file")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--verify", action="store_true", help="read-only public DNS, TLS and Portal verification")
    mode.add_argument("--plan", action="store_true", help="validate inputs and render locally without cluster/registry calls")
    args = parser.parse_args(argv)
    try:
        overrides = {key: getattr(args, key) if getattr(args, key) is not None else os.environ.get(key.upper())
                     for key in ("registry", "version")}
        origins = {key: f"--{key}" if getattr(args, key) is not None else f"environment {key.upper()}"
                   for key in overrides}
        config, values = configuration(args.config, **overrides, coordinate_origins=origins)
        with tempfile.TemporaryDirectory(prefix="cetirun-platform-") as temporary:
            deployment = Deployment(config, values, Path(temporary))
            deployment.render()
            if args.verify:
                if not deployment.ingress:
                    raise ValueError("--verify requires publicTLS.enabled=true")
                result = deployment.ingress.verify()
                expected = config["_source"].get("domain")
                if expected and expected != result["domain"]:
                    raise ValueError("domain does not match the publicTLS hostname")
                print(json.dumps(result, indent=2))
            elif args.plan:
                print(json.dumps(deployment.plan(), indent=2))
            else:
                deployment.execute()
    except yaml.YAMLError as error:
        mark = getattr(error, "problem_mark", None)
        location = f" at line {mark.line + 1}, column {mark.column + 1}" if mark else ""
        print(f"ERROR: Invalid YAML{location}; input contents withheld", file=sys.stderr)
        return 1
    except (ValueError, KeyError, TypeError, OSError, RuntimeError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
