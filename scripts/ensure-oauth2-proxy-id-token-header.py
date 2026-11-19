#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Safely reconcile CetiRun's oauth2-proxy provider in the Istio mesh config."""

import argparse
import json
import os
import subprocess
import tempfile

import yaml
from yaml.constructor import ConstructorError
from yaml.resolver import BaseResolver

MAX_PATCH_ATTEMPTS = 5


class UniqueKeyLoader(yaml.SafeLoader):
    """Safe YAML loader that rejects ambiguous duplicate mapping keys."""


def _construct_unique_mapping(loader, node, deep=False):
    loader.flatten_mapping(node)
    mapping = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        try:
            duplicate = key in mapping
        except TypeError as exc:
            raise ConstructorError(
                "while constructing a mapping",
                node.start_mark,
                "found an unhashable key",
                key_node.start_mark,
            ) from exc
        if duplicate:
            raise ConstructorError(
                "while constructing a mapping",
                node.start_mark,
                f"found duplicate key {key!r}",
                key_node.start_mark,
            )
        mapping[key] = loader.construct_object(value_node, deep=deep)
    return mapping


UniqueKeyLoader.add_constructor(
    BaseResolver.DEFAULT_MAPPING_TAG, _construct_unique_mapping
)


def _oauth2_proxy_provider(service="oauth2-proxy.cetirun-istio.svc.cluster.local") -> dict:
    return {
        "name": "oauth2-proxy",
        "envoyExtAuthzHttp": {
            "service": service,
            "port": 4180,
            "includeRequestHeadersInCheck": [
                "cookie",
                "authorization",
                "x-auth-request-redirect",
            ],
            "headersToUpstreamOnAllow": [
                "x-auth-request-user",
                "x-auth-request-email",
                "x-auth-request-preferred-username",
                "authorization",
            ],
            "headersToDownstreamOnDeny": ["set-cookie", "location"],
            "headersToDownstreamOnAllow": ["set-cookie"],
            "statusOnError": "403",
        },
    }


def _load_mesh(mesh_text: str) -> dict:
    if not isinstance(mesh_text, str):
        raise RuntimeError("Istio mesh configuration is invalid")
    if not mesh_text.strip():
        return {}
    try:
        documents = list(yaml.load_all(mesh_text, Loader=UniqueKeyLoader))
    except yaml.YAMLError as exc:
        raise RuntimeError("Istio mesh configuration is invalid") from exc
    if len(documents) != 1:
        raise RuntimeError("Istio mesh configuration must contain one document")
    mesh = documents[0]
    if mesh is None:
        return {}
    if not isinstance(mesh, dict):
        raise RuntimeError("Istio mesh configuration must be a mapping")
    return mesh


def _dump_mesh(mesh: dict) -> str:
    return yaml.safe_dump(mesh, default_flow_style=False, sort_keys=False)


def _ensure_provider(mesh_text: str, add_missing: bool, service=None) -> tuple[str, bool]:
    mesh = _load_mesh(mesh_text)
    providers = mesh.get("extensionProviders")
    if providers is None:
        providers = []
        mesh["extensionProviders"] = providers
    if not isinstance(providers, list):
        raise RuntimeError("Istio extensionProviders configuration must be a list")
    if not all(isinstance(provider, dict) for provider in providers):
        raise RuntimeError("Istio extensionProviders entries must be mappings")

    matches = [
        provider for provider in providers if provider.get("name") == "oauth2-proxy"
    ]
    if len(matches) > 1 or (not matches and not add_missing):
        raise RuntimeError("oauth2-proxy extension provider was not resolved exactly")
    if not matches:
        providers.append(_oauth2_proxy_provider(service) if service else _oauth2_proxy_provider())
        return _dump_mesh(mesh), True

    ext_authz = matches[0].get("envoyExtAuthzHttp")
    if not isinstance(ext_authz, dict):
        raise RuntimeError("oauth2-proxy ext_authz configuration is invalid")
    if service and ext_authz.get("service") != service:
        raise RuntimeError("oauth2-proxy extension provider points to another service; review the mesh configuration before deployment")
    headers = ext_authz.get("headersToUpstreamOnAllow")
    if headers is None:
        headers = []
        ext_authz["headersToUpstreamOnAllow"] = headers
    if not isinstance(headers, list) or not all(isinstance(item, str) for item in headers):
        raise RuntimeError("oauth2-proxy upstream header configuration is invalid")
    changed = False
    if "authorization" not in headers:
        headers.append("authorization")
        changed = True
    if add_missing:
        downstream = ext_authz.setdefault("headersToDownstreamOnAllow", [])
        if not isinstance(downstream, list) or not all(isinstance(item, str) for item in downstream):
            raise RuntimeError("oauth2-proxy downstream header configuration is invalid")
        if "set-cookie" not in downstream:
            downstream.append("set-cookie")
            changed = True
    return (_dump_mesh(mesh), True) if changed else (mesh_text, False)


def ensure_header(mesh_text: str) -> tuple[str, bool]:
    return _ensure_provider(mesh_text, add_missing=False)


def ensure_provider(mesh_text: str, service=None) -> tuple[str, bool]:
    return _ensure_provider(mesh_text, add_missing=True, service=service)


def configmap_patch(configmap: dict, service=None) -> tuple[dict, bool]:
    resource_version = configmap.get("metadata", {}).get("resourceVersion")
    if not isinstance(resource_version, str) or not resource_version:
        raise RuntimeError("Istio ConfigMap resourceVersion is unavailable")
    mesh, changed = ensure_provider(configmap.get("data", {}).get("mesh", ""), service)
    return {
        "metadata": {"resourceVersion": resource_version},
        "data": {"mesh": mesh},
    }, changed


def reconcile_configmap(namespace: str, name: str, runner=subprocess.run, service=None, uid=None) -> None:
    for attempt in range(MAX_PATCH_ATTEMPTS):
        raw = runner(
            [
                "kubectl", "get", "configmap", name, "--namespace", namespace,
                "--ignore-not-found", "-o", "json",
            ],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        if getattr(raw, "returncode", 0) != 0:
            raise RuntimeError("Istio ConfigMap could not be read safely")
        if not raw.stdout.strip():
            if uid:
                raise RuntimeError("The pinned Istio ConfigMap was deleted; refusing recreation")
            mesh, _ = ensure_provider("", service)
            document = {
                "apiVersion": "v1",
                "kind": "ConfigMap",
                "metadata": {"name": name, "namespace": namespace},
                "data": {"mesh": mesh},
            }
            descriptor, filename = tempfile.mkstemp(
                prefix="cetirun-istio-mesh-create-", suffix=".json"
            )
            try:
                os.fchmod(descriptor, 0o600)
                with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
                    json.dump(document, handle)
                result = runner(
                    ["kubectl", "create", "--filename", filename],
                    check=False,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.PIPE,
                    text=True,
                )
            finally:
                try:
                    os.unlink(filename)
                except FileNotFoundError:
                    pass
            if result.returncode == 0:
                return
            if "AlreadyExists" in result.stderr and attempt + 1 < MAX_PATCH_ATTEMPTS:
                continue
            raise RuntimeError("Istio ConfigMap could not be created safely")
        configmap = json.loads(raw.stdout)
        if uid and configmap.get("metadata", {}).get("uid") != uid:
            raise RuntimeError("The pinned Istio ConfigMap was replaced; refusing to modify it")
        patch, changed = configmap_patch(configmap, service)
        if uid:
            patch["metadata"]["uid"] = uid
        if not changed:
            return
        descriptor, filename = tempfile.mkstemp(prefix="cetirun-istio-mesh-", suffix=".json")
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
                json.dump(patch, handle)
            result = runner(
                ["kubectl", "patch", "configmap", name, "--namespace", namespace, "--type", "merge", "--patch-file", filename],
                check=False,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.PIPE,
                text=True,
            )
        finally:
            try:
                os.unlink(filename)
            except FileNotFoundError:
                pass
        if result.returncode == 0:
            return
        if (
            not any(reason in result.stderr for reason in ("Conflict", "NotFound"))
            or attempt + 1 == MAX_PATCH_ATTEMPTS
        ):
            raise RuntimeError("Istio ConfigMap could not be updated safely")
    raise AssertionError("unreachable")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--name", required=True)
    parser.add_argument("--context", help="explicit Kubernetes context; never changes current-context")
    parser.add_argument("--service", help="expected oauth2-proxy service FQDN")
    parser.add_argument("--uid", help="pin an existing ConfigMap UID; reject deletion or replacement")
    args = parser.parse_args()
    def runner(command, **kwargs):
        if args.context:
            command = [command[0], "--context", args.context, "--request-timeout=15s", *command[1:]]
        return subprocess.run(command, **kwargs)
    reconcile_configmap(args.namespace, args.name, runner=runner, service=args.service, uid=args.uid)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
