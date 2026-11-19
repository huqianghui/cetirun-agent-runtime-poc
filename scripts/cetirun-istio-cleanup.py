#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Inspect or remove one retired, Helm-kept CetiRun blacklist after upgrade/uninstall."""

import argparse
import json
import re
import subprocess
import sys
import time

import yaml


RESOURCE = "envoyfilters.networking.istio.io"


def command(args, document=None):
    result = subprocess.run(args, input=None if document is None else json.dumps(document),
                            text=True, capture_output=True, timeout=45, check=False)
    if result.returncode:
        raise ValueError(f"{args[0]} failed; no automatic replay or broader cleanup was attempted")
    return result.stdout


def validate_legacy(value, args, name):
    metadata = value.get("metadata", {})
    annotations, labels = metadata.get("annotations", {}), metadata.get("labels", {})
    if (value.get("apiVersion") != "networking.istio.io/v1alpha3" or value.get("kind") != "EnvoyFilter"
            or metadata.get("namespace") != args.legacy_namespace or metadata.get("name") != name
            or not metadata.get("uid") or not metadata.get("resourceVersion")
            or annotations.get("meta.helm.sh/release-name") != args.release
            or annotations.get("meta.helm.sh/release-namespace") != args.release_namespace
            or annotations.get("helm.sh/resource-policy") != "keep"
            or labels.get("app.kubernetes.io/managed-by") != "Helm"
            or labels.get("app.kubernetes.io/instance") != args.release
            or metadata.get("deletionTimestamp") or metadata.get("finalizers")):
        raise ValueError("legacy filter identity, Helm ownership or deletion state does not match")
    spec = value.get("spec", {})
    if set(spec) != {"configPatches"} or len(spec["configPatches"]) != 2:
        raise ValueError("object is not the retired two-filter blacklist")
    for patch, filter_name, prefix in zip(
        spec["configPatches"],
        ("envoy.filters.network.tcp_proxy", "envoy.filters.network.http_connection_manager"),
        ("egress_blacklist", "egress_blacklist_http"),
    ):
        expected_match = {"context": "SIDECAR_OUTBOUND", "listener": {"filterChain": {"filter": {"name": filter_name}}}}
        insert = patch.get("patch", {})
        value = insert.get("value", {})
        config = value.get("typed_config", {})
        if (set(patch) != {"applyTo", "match", "patch"} or patch["applyTo"] != "NETWORK_FILTER"
                or patch["match"] != expected_match or set(insert) != {"operation", "value"}
                or insert["operation"] != "INSERT_BEFORE" or set(value) != {"name", "typed_config"}
                or value["name"] != "envoy.filters.network.rbac"
                or set(config) != {"@type", "stat_prefix", "rules"}
                or config["@type"] != "type.googleapis.com/envoy.extensions.filters.network.rbac.v3.RBAC"
                or config["stat_prefix"] != prefix or set(config["rules"]) != {"action", "policies"}
                or config["rules"]["action"] != "DENY" or not config["rules"]["policies"]):
            raise ValueError("legacy filter shape changed; review it instead of deleting")


def cleanup(args, run=command):
    for field in ("kubeconfig", "context", "cluster_uid"):
        value = getattr(args, field)
        if not value or value.strip() != value:
            raise ValueError(f"{field} must explicitly identify the approved target")
    for field in ("release", "release_namespace", "legacy_namespace"):
        if len(getattr(args, field)) > 63 or not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?", getattr(args, field)):
            raise ValueError(f"{field} must be a Kubernetes DNS label")
    if args.apply and not (args.expected_uid and args.expected_resource_version):
        raise ValueError("--apply requires the UID and resourceVersion from the reviewed plan")
    kubectl = ["kubectl", "--kubeconfig", args.kubeconfig, "--context", args.context, "--request-timeout=30s"]
    helm = ["helm", "--kubeconfig", args.kubeconfig, "--kube-context", args.context]
    cluster = json.loads(run(kubectl + ["get", "namespace", "kube-system", "-o", "json"]))
    if cluster.get("metadata", {}).get("uid") != args.cluster_uid:
        raise ValueError("cluster UID does not match; no cleanup permitted")
    name = (args.release + "-blacklist")[:63].rstrip("-")
    releases = json.loads(run(helm + ["list", "--all", "--namespace", args.release_namespace,
                                     "--filter", "^" + args.release + "$", "--output", "json"]))
    if args.after_uninstall:
        if releases:
            raise ValueError("release still exists; finish uninstall before removing the kept object")
    else:
        if len(releases) != 1 or releases[0].get("name") != args.release or releases[0].get("status") != "deployed":
            raise ValueError("expected one deployed upgraded release")
        manifest = run(helm + ["get", "manifest", args.release, "--namespace", args.release_namespace])
        for value in yaml.safe_load_all(manifest):
            if (isinstance(value, dict) and value.get("kind") == "EnvoyFilter"
                    and value.get("metadata", {}).get("name") == name
                    and value.get("metadata", {}).get("namespace", args.release_namespace) == args.legacy_namespace):
                raise ValueError("current Helm manifest still manages the blacklist; upgrade first")
    get = kubectl + ["get", RESOURCE, name, "--namespace", args.legacy_namespace, "--ignore-not-found", "-o", "json"]
    raw = run(get)
    if not raw.strip():
        return {"status": "absent", "changed": False}
    value = json.loads(raw)
    validate_legacy(value, args, name)
    metadata = value["metadata"]
    if ((args.expected_uid and args.expected_uid != metadata["uid"])
            or (args.expected_resource_version and args.expected_resource_version != metadata["resourceVersion"])):
        raise ValueError("legacy filter changed after inspection; no deletion submitted")
    receipt = {"status": "verified-plan", "changed": False, "uid": metadata["uid"], "resourceVersion": metadata["resourceVersion"]}
    if not args.apply:
        return receipt
    path = f"/apis/networking.istio.io/v1alpha3/namespaces/{args.legacy_namespace}/envoyfilters/{name}"
    run(kubectl + ["delete", "--raw", path, "-f", "-"],
        {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {
            "uid": metadata["uid"], "resourceVersion": metadata["resourceVersion"]}})
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        raw = run(get)
        if not raw.strip():
            return {**receipt, "status": "removed", "changed": True}
        observed = json.loads(raw)
        if observed["metadata"]["uid"] != metadata["uid"]:
            raise ValueError("a replacement object appeared after deletion; it was not touched")
        time.sleep(1)
    raise ValueError("deletion was submitted but absence is not verified; do not strip finalizers or replay blindly")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("kubeconfig", "context", "cluster-uid", "release", "release-namespace", "legacy-namespace"):
        parser.add_argument("--" + flag, required=True)
    parser.add_argument("--after-uninstall", action="store_true")
    parser.add_argument("--expected-uid")
    parser.add_argument("--expected-resource-version")
    parser.add_argument("--apply", action="store_true", help="delete only the reviewed UID/resourceVersion; default is read-only")
    try:
        print(json.dumps(cleanup(parser.parse_args()), sort_keys=True))
    except (ValueError, TypeError, KeyError, subprocess.TimeoutExpired, OSError, yaml.YAMLError) as error:
        print(f"ERROR: {error if isinstance(error, ValueError) else type(error).__name__ + ': cleanup not verified'}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
