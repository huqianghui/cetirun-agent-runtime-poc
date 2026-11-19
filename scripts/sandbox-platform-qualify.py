#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Explicit-target preflight, lifecycle snapshots, network smoke and receipt checks.

This does not register capabilities, authorize execution, or prove writer fencing.
Deployment preflight reports native single-start configuration as unverified,
not the stock writer-exclusion hold. Bootstrap, Gateway operation and effective
skill selection still require separate evidence; completion is not E2E readiness.
Use acceptance-check --receipt FILE for offline lifecycle evidence assertions.
Lifecycle snapshots are read-only partial observations, not acceptance receipts.
"""

import argparse
import base64
import binascii
from contextlib import contextmanager
from decimal import Decimal
import hashlib
import ipaddress
import json
import os
import re
import selectors
import signal
import stat
import subprocess
import sys
import time
from urllib.parse import urlencode
import uuid


RUN_LABEL = "qualification.cetirun.microsoft.com/run"
PROBE = (
    "import http.client,json,sys\n"
    "results={}\n"
    "for port in (8082,8080,8081):\n"
    " c=http.client.HTTPConnection(sys.argv[1],port,timeout=2)\n"
    " try:\n"
    "  try: c.connect()\n"
    "  except TimeoutError: results[port]=False; continue\n"
    "  c.request('GET','/'); r=c.getresponse()\n"
    "  if r.status!=200 or r.read()!=b'qualification-ok': raise SystemExit('unexpected synthetic server response')\n"
    "  results[port]=True\n"
    " finally: c.close()\n"
    "print(json.dumps([results[port] for port in (8080,8081,8082)]))\n"
)
SERVER = (
    "from http.server import BaseHTTPRequestHandler,HTTPServer\n"
    "import ipaddress,os,socket,threading,time\n"
    "address=ipaddress.ip_address(os.environ['POD_IP'])\n"
    "class Server(HTTPServer):\n"
    " address_family=socket.AF_INET6 if address.version==6 else socket.AF_INET\n"
    "class Handler(BaseHTTPRequestHandler):\n"
    " def do_GET(self):\n"
    "  self.send_response(200);self.end_headers();self.wfile.write(b'qualification-ok')\n"
    " def log_message(self,*args): pass\n"
    "for port in (8080,8081,8082):\n"
    " threading.Thread(target=Server((str(address),port),Handler).serve_forever,daemon=True).start()\n"
    "time.sleep(3600)\n"
)
LISTENER_READINESS = (
    "import os,socket\n"
    "for port in (8080,8081,8082):\n"
    " with socket.create_connection((os.environ['POD_IP'],port),timeout=1): pass\n"
)
UDP_EXCHANGE = (
    "import ipaddress,secrets,socket\n"
    "def exchange(address,port,timeout):\n"
    " ip=ipaddress.ip_address(address)\n"
    " family=socket.AF_INET6 if ip.version==6 else socket.AF_INET\n"
    " payload=b'qualification-'+secrets.token_bytes(16)\n"
    " with socket.socket(family,socket.SOCK_DGRAM) as endpoint:\n"
    "  endpoint.settimeout(timeout)\n"
    "  endpoint.connect((str(ip),port))\n"
    "  if endpoint.send(payload)!=len(payload): raise SystemExit('incomplete synthetic datagram send')\n"
    "  try: response=endpoint.recv(len(payload)+1)\n"
    "  except TimeoutError: return False\n"
    "  if response!=payload: raise SystemExit('unexpected synthetic datagram response')\n"
    " return True\n"
)
UDP_PROBE = UDP_EXCHANGE + (
    "import json,sys\n"
    "results={port:exchange(sys.argv[1],port,2) for port in (8082,8080,8081)}\n"
    "print(json.dumps([results[port] for port in (8080,8081,8082)]))\n"
)
UDP_LISTENER_READINESS = UDP_EXCHANGE + (
    "import os\n"
    "for port in (8080,8081,8082):\n"
    " if not exchange(os.environ['POD_IP'],port,1): raise SystemExit('synthetic datagram listener not ready')\n"
)
UDP_SERVER = (
    "import ipaddress,os,socket,socketserver,threading,time\n"
    "address=ipaddress.ip_address(os.environ['POD_IP'])\n"
    "class Server(socketserver.UDPServer):\n"
    " address_family=socket.AF_INET6 if address.version==6 else socket.AF_INET\n"
    " max_packet_size=128\n"
    "class Handler(socketserver.BaseRequestHandler):\n"
    " def handle(self):\n"
    "  data,endpoint=self.request\n"
    "  endpoint.sendto(data,self.client_address)\n"
    "for port in (8080,8081,8082):\n"
    " threading.Thread(target=Server((str(address),port),Handler).serve_forever,daemon=True).start()\n"
    "time.sleep(3600)\n"
)


class QualificationError(Exception):
    pass


class Kubernetes:
    def __init__(self, kubeconfig, context, cluster_uid):
        if not kubeconfig or not context or not cluster_uid:
            raise QualificationError("An explicit kubeconfig, context and cluster UID are required")
        self.prefix = [
            "kubectl", "--kubeconfig", kubeconfig, "--context", context,
            "--request-timeout=15s",
        ]
        self.cluster_uid = cluster_uid

    def run(self, args, document=None, timeout=30):
        try:
            result = subprocess.run(
                self.prefix + args,
                input=None if document is None else json.dumps(document),
                text=True, capture_output=True, timeout=timeout, check=False,
                env={**os.environ, "KUBECTL_KUBERC": "false"},
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise QualificationError("Explicit-target Kubernetes command unavailable or timed out") from error
        if result.returncode:
            # kubectl diagnostics may include endpoints or authentication data.
            raise QualificationError(f"Kubernetes {args[0]} failed (exit {result.returncode}); no result accepted")
        return result.stdout

    def get(self, resource, name, namespace=None, absent_ok=False):
        args = ["get", resource, name, "-o", "json"]
        if namespace:
            args += ["--namespace", namespace]
        if absent_ok:
            args.append("--ignore-not-found")
        raw = self.run(args)
        if absent_ok and not raw.strip():
            return None
        try:
            result = json.loads(raw)
        except json.JSONDecodeError as error:
            raise QualificationError("Kubernetes returned an invalid JSON object") from error
        if not isinstance(result, dict):
            raise QualificationError("Kubernetes returned an unexpected object shape")
        return result

    def check_target(self):
        observed = self.get("namespace", "kube-system")
        if observed.get("metadata", {}).get("uid") != self.cluster_uid:
            raise QualificationError("Cluster UID mismatch; no test resources may be changed")

    def create(self, document):
        # Use the exact APIs already checked by preflight. Client-side discovery
        # can download the cluster-wide OpenAPI schema before a small create and
        # exceed the command deadline. The API server still validates strictly.
        collections = {
            ("v1", "Namespace"): ("/api/v1", "namespaces", False),
            ("v1", "Pod"): ("/api/v1", "pods", True),
            ("v1", "PersistentVolumeClaim"): ("/api/v1", "persistentvolumeclaims", True),
            ("networking.k8s.io/v1", "NetworkPolicy"): ("/apis/networking.k8s.io/v1", "networkpolicies", True),
            ("cilium.io/v2", "CiliumNetworkPolicy"): ("/apis/cilium.io/v2", "ciliumnetworkpolicies", True),
        }
        if not isinstance(document, dict):
            raise QualificationError("Unsupported synthetic resource")
        api, kind = document.get("apiVersion"), document.get("kind")
        if not isinstance(api, str) or not isinstance(kind, str) or (api, kind) not in collections:
            raise QualificationError("Unsupported synthetic resource")
        prefix, collection, namespaced = collections[(api, kind)]
        if namespaced:
            metadata = document.get("metadata")
            namespace = metadata.get("namespace") if isinstance(metadata, dict) else None
            if not resource_name(namespace, 63) or "." in namespace:
                raise QualificationError("An exact synthetic namespace is required")
            prefix += "/namespaces/" + namespace
        raw = self.run(["create", "--raw", prefix + "/" + collection + "?fieldValidation=Strict", "-f", "-"], document)
        try:
            result = json.loads(raw)
        except json.JSONDecodeError as error:
            raise QualificationError("Resource create returned invalid JSON; creation outcome is unknown") from error
        if not isinstance(result, dict):
            raise QualificationError("Resource create returned an unexpected shape; creation outcome is unknown")
        return result


class PreflightBlocked(QualificationError):
    def __init__(self, reason, status="blocked"):
        super().__init__(reason)
        self.reason = reason
        self.status = status


class SanitizedParser(argparse.ArgumentParser):
    def error(self, message):
        raise PreflightBlocked("invalid-command-line")


def require_preflight(condition, reason, status="blocked"):
    if not condition:
        raise PreflightBlocked(reason, status)


def object_value(value):
    require_preflight(isinstance(value, dict), "unexpected-object-shape", "unknown")
    return value


def array_value(value):
    require_preflight(isinstance(value, list), "unexpected-array-shape", "unknown")
    return value


def exact_text(value, limit=128):
    return isinstance(value, str) and 0 < len(value) <= limit and all("!" <= c <= "~" for c in value)


def resource_name(value, limit=253):
    return (isinstance(value, str) and 0 < len(value) <= limit and
            re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*", value) is not None)


def preflight_json(raw, limit=1024 * 1024):
    def pairs(items):
        value = {}
        for key, item in items:
            require_preflight(key not in value, "duplicate-json-field", "unknown")
            value[key] = item
        return value

    try:
        require_preflight(isinstance(raw, str) and 0 < len(raw.encode("utf-8")) <= limit,
                          "invalid-document-size", "unknown")
        return object_value(json.loads(raw, object_pairs_hook=pairs,
                                       parse_constant=lambda _: object_value(None)))
    except (ValueError, UnicodeError, RecursionError) as error:
        raise PreflightBlocked("invalid-json-document", "unknown") from error


DEPLOYMENT_RESOURCES = {
    "namespace": ("v1", "Namespace", "/api/v1/namespaces"),
    "deployment": ("apps/v1", "Deployment", "/apis/apps/v1/namespaces/{namespace}/deployments"),
    "serviceaccount": ("v1", "ServiceAccount", "/api/v1/namespaces/{namespace}/serviceaccounts"),
    "service": ("v1", "Service", "/api/v1/namespaces/{namespace}/services"),
    "endpoints": ("v1", "Endpoints", "/api/v1/namespaces/{namespace}/endpoints"),
    "pod": ("v1", "Pod", "/api/v1/namespaces/{namespace}/pods"),
    "replicaset": ("apps/v1", "ReplicaSet", "/apis/apps/v1/namespaces/{namespace}/replicasets"),
    "configmap": ("v1", "ConfigMap", "/api/v1/namespaces/{namespace}/configmaps"),
    "validatingwebhookconfiguration": (
        "admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration",
        "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations"),
    "mutatingwebhookconfiguration": (
        "admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration",
        "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations"),
}
ADMISSION_NAME = "cetirun-sandbox-admission"
PINNED_IMAGE = r"[A-Za-z0-9][A-Za-z0-9./:_-]*@sha256:[0-9a-f]{64}"
# Public _sandbox-admission.tpl expressions, with the controller identity replaced by "CONTROLLER".
WEBHOOK_CLEANUP_DIGESTS = {
    "controller-cleanup-only": "14e2e1fad301b5cef75005b61612777176a7d0b62e6dce79f417fcd1b6f007c5",
    "garbage-collector-cleanup-only": "6d6bb441b8b56045e100a4eab716b7e4d5993a22ad3d78bfbac87742e8b4ae73",
}


class DeploymentKubernetes(Kubernetes):
    """Named GETs only; no discovery, auth plugins, retries or diagnostic output."""

    MAX_BYTES = 1024 * 1024
    MAX_COMMANDS = 64
    COMMAND_SECONDS = 10
    TOTAL_SECONDS = 90
    CONFIG_COMMAND = ["config", "view", "--minify", "-o", "json"]

    def __init__(self, kubeconfig, context, cluster_uid):
        super().__init__(kubeconfig, context, cluster_uid)
        self.prefix[-1] = "--request-timeout=5s"
        self.prefix.append("--v=0")
        self.context = context
        self.deadline = time.monotonic() + self.TOTAL_SECONDS
        self.commands = 0
        self.objects = {}

    def run(self, args, document=None, timeout=None):
        allowed = args == self.CONFIG_COMMAND or (
            len(args) == 3 and args[:2] == ["get", "--raw"] and
            (args[2] == "/version" or args[2] in self.objects))
        require_preflight(allowed and document is None and timeout is None, "request-not-allowlisted")
        require_preflight(self.commands < self.MAX_COMMANDS, "command-budget-exhausted", "unknown")
        deadline = min(self.deadline, time.monotonic() + self.COMMAND_SECONDS)
        require_preflight(deadline > time.monotonic(), "deadline-exhausted", "unknown")
        self.commands += 1
        process = None
        try:
            process = subprocess.Popen(
                self.prefix + args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL, start_new_session=True,
                env={**os.environ, "KUBECTL_KUBERC": "false"},
            )
            data = bytearray()
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                while selector.get_map():
                    remaining = deadline - time.monotonic()
                    require_preflight(remaining > 0, "request-timeout", "unknown")
                    for key, _ in selector.select(remaining):
                        chunk = os.read(key.fd, min(65536, self.MAX_BYTES + 1 - len(data)))
                        if not chunk:
                            selector.unregister(key.fileobj)
                            continue
                        data.extend(chunk)
                        require_preflight(len(data) <= self.MAX_BYTES, "response-too-large", "unknown")
            remaining = deadline - time.monotonic()
            require_preflight(remaining > 0, "request-timeout", "unknown")
            require_preflight(process.wait(timeout=remaining) == 0, "kubernetes-request-failed", "unknown")
            return data.decode("utf-8")
        except (OSError, subprocess.TimeoutExpired, UnicodeError) as error:
            raise PreflightBlocked("command-unavailable-timeout-or-invalid-output", "unknown") from error
        finally:
            if process is not None:
                try:
                    # The command may have exited while a child still holds its pipe.
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    process.wait(timeout=1)
                except (OSError, subprocess.TimeoutExpired) as error:
                    raise PreflightBlocked("command-cleanup-unconfirmed", "unknown") from error
                finally:
                    process.stdout.close()

    def credentials(self):
        config = preflight_json(self.run(self.CONFIG_COMMAND))
        require_preflight(config.get("current-context") == self.context, "context-mismatch")
        contexts, users, clusters = (
            array_value(config.get(key)) for key in ("contexts", "users", "clusters"))
        require_preflight(len(contexts) == len(users) == len(clusters) == 1, "unsupported-kubeconfig", "unknown")
        context = object_value(object_value(contexts[0]).get("context"))
        user, cluster = object_value(users[0]), object_value(clusters[0])
        require_preflight(contexts[0].get("name") == self.context and
                          context.get("user") == user.get("name") and context.get("cluster") == cluster.get("name"),
                          "kubeconfig-binding-mismatch")
        auth, connection = object_value(user.get("user")), object_value(cluster.get("cluster"))
        require_preflight(not any(key in auth for key in ("exec", "auth-provider", "tokenFile", "as", "as-groups")),
                          "credential-plugin-or-indirection-not-supported", "unknown")
        require_preflight(isinstance(connection.get("server"), str) and connection["server"].startswith("https://") and
                          not connection.get("insecure-skip-tls-verify"),
                          "verified-https-target-required")
        return config

    def get(self, resource, name, namespace=None, *, uid=None):
        require_preflight(resource in DEPLOYMENT_RESOURCES and resource_name(name), "resource-not-allowlisted")
        api, kind, base = DEPLOYMENT_RESOURCES[resource]
        namespaced = "{namespace}" in base
        require_preflight((namespaced and resource_name(namespace, 63) and "." not in namespace) or
                          (not namespaced and namespace is None), "invalid-resource-scope")
        require_preflight(resource != "namespace" or name == "kube-system", "resource-not-allowlisted")
        path = base.format(namespace=namespace) + "/" + name
        previous = self.objects.get(path)
        self.objects[path] = previous or (resource, name, namespace, uid, None)
        value = preflight_json(self.run(["get", "--raw", path]))
        meta = object_value(value.get("metadata"))
        require_preflight(value.get("apiVersion") == api and value.get("kind") == kind and
                          meta.get("name") == name and meta.get("namespace") == namespace,
                          "resource-identity-mismatch")
        require_preflight(not meta.get("deletionTimestamp"), "resource-terminating")
        identity = (meta.get("uid"), meta.get("resourceVersion"))
        require_preflight(all(exact_text(item) for item in identity), "missing-resource-identity", "unknown")
        require_preflight(uid is None or identity[0] == uid, "resource-uid-mismatch")
        require_preflight(previous is None or previous[4] == identity, "resource-version-drift")
        self.objects[path] = (resource, name, namespace, uid, identity)
        return value

    def check_target(self):
        self.get("namespace", "kube-system", uid=self.cluster_uid)

    def stable(self, config, version):
        require_preflight(self.credentials() == config, "kubeconfig-drift")
        self.check_target()
        require_preflight(preflight_json(self.run(["get", "--raw", "/version"])) == version, "api-version-drift")
        for resource, name, namespace, uid, _ in list(self.objects.values()):
            self.get(resource, name, namespace, uid=uid)
        require_preflight(time.monotonic() < self.deadline, "deadline-exhausted", "unknown")


def deployment_inputs(args):
    require_preflight(os.name == "posix", "posix-subprocess-runner-required", "unknown")
    require_preflight(os.path.isabs(args.kubeconfig) and len(args.kubeconfig) <= 4096 and
                      "\0" not in args.kubeconfig and exact_text(args.context) and exact_text(args.cluster_uid),
                      "invalid-explicit-target")
    require_preflight(resource_name(args.namespace, 63) and "." not in args.namespace, "invalid-namespace")
    require_preflight(not any((args.storage_class, args.runtime_class, args.runtime_handler,
                              args.probe_image, args.allow_test_resources)), "incompatible-mode-arguments")
    for role in ("operator", "admission"):
        require_preflight(resource_name(getattr(args, role + "_deployment")) and
                          resource_name(getattr(args, role + "_service_account")) and
                          exact_text(getattr(args, role + "_uid")) and
                          exact_text(getattr(args, role + "_service_account_uid")), "explicit-workload-pins-required")
        image = getattr(args, role + "_image")
        require_preflight(isinstance(image, str) and len(image) <= 512 and
                          re.fullmatch(PINNED_IMAGE, image), "digest-pinned-workload-images-required")
    require_preflight(args.operator_deployment != args.admission_deployment and
                      args.operator_uid != args.admission_uid and
                      args.operator_service_account != args.admission_service_account and
                      args.operator_service_account_uid != args.admission_service_account_uid,
                      "independent-admission-identity-required")


def deployed_container(spec, role, image, account):
    require_preflight(spec.get("serviceAccountName") == account, "workload-service-account-mismatch")
    containers = array_value(spec.get("containers"))
    name = "manager" if role == "operator" else "admission"
    selected = [object_value(c) for c in containers if object_value(c).get("name") == name]
    require_preflight(len(selected) == 1, "native-container-not-found", "unknown")
    container = selected[0]
    require_preflight(container.get("image") == image, "workload-image-mismatch")
    require_preflight(not container.get("command") and not container.get("envFrom"),
                      "indirect-process-configuration", "unknown")
    flags = {}
    for flag in array_value(container.get("args")):
        require_preflight(isinstance(flag, str) and flag.startswith("--") and "=" in flag,
                          "unsupported-process-arguments", "unknown")
        key, value = flag[2:].split("=", 1)
        require_preflight(key not in flags, "duplicate-process-argument", "unknown")
        flags[key] = value
    require_preflight(flags.get("mode", "operator") == ("operator" if role == "operator" else "sandbox-admission"),
                      "process-mode-mismatch")
    require_preflight(flags.get("version", "false") == "false", "version-only-process")
    return container, flags


def deployed_workload(kube, args, role):
    deployment = kube.get("deployment", getattr(args, role + "_deployment"), args.namespace,
                          uid=getattr(args, role + "_uid"))
    kube.get("serviceaccount", getattr(args, role + "_service_account"), args.namespace,
             uid=getattr(args, role + "_service_account_uid"))
    spec, status = object_value(deployment.get("spec")), object_value(deployment.get("status"))
    replicas = spec.get("replicas")
    require_preflight(type(replicas) is int and (1 if role == "operator" else 2) <= replicas <= 4,
                      "unsupported-replica-count")
    generation = object_value(deployment.get("metadata")).get("generation")
    require_preflight(type(generation) is int and generation > 0 and
                      type(status.get("observedGeneration")) is int and status["observedGeneration"] == generation and
                      all(type(status.get(field)) is int and status[field] == replicas for field in
                          ("replicas", "updatedReplicas", "readyReplicas", "availableReplicas")) and
                      not status.get("unavailableReplicas"), "deployment-not-fully-observed")
    template = object_value(spec.get("template"))
    container, flags = deployed_container(object_value(template.get("spec")), role,
                                         getattr(args, role + "_image"), getattr(args, role + "_service_account"))
    return deployment, template, container, flags


def environment_entry(container, name):
    entries = [object_value(e) for e in array_value(container.get("env")) if object_value(e).get("name") == name]
    require_preflight(len(entries) == 1, "explicit-environment-entry-required", "unknown")
    return entries[0]


def inline_configuration(container, name, limit):
    entry = environment_entry(container, name)
    require_preflight("valueFrom" not in entry, "inline-configuration-required", "unknown")
    return preflight_json(entry.get("value"), limit)


def source_bindings(kube, args, operator, admission):
    namespace = environment_entry(operator[2], "OPERATOR_NAMESPACE")
    account = environment_entry(operator[2], "OPERATOR_SA_NAME")
    require_preflight(("valueFrom" not in namespace and namespace.get("value") == args.namespace) or
                      ("value" not in namespace and namespace.get("valueFrom") in
                       ({"fieldRef": {"fieldPath": "metadata.namespace"}},
                        {"fieldRef": {"apiVersion": "v1", "fieldPath": "metadata.namespace"}})),
                      "operator-namespace-binding-mismatch")
    require_preflight("valueFrom" not in account and account.get("value") == args.operator_service_account,
                      "operator-service-account-binding-mismatch")
    sources = inline_configuration(operator[2], "CETIRUN_SANDBOX_SOURCES", 16 * 1024)
    config = inline_configuration(admission[2], "CETIRUN_SANDBOX_ADMISSION", 16 * 1024)
    require_preflight(sources.get("enabled") is True, "sandbox-sources-disabled")
    native_execution = sources.get("nativeExecution", False)
    require_preflight(type(native_execution) is bool and
                      config.get("nativeExecution", False) is native_execution,
                      "native-execution-mode-mismatch")
    planned_migration = sources.get("plannedMigration", False)
    require_preflight(type(planned_migration) is bool and (not planned_migration or native_execution),
                      "planned-migration-mode-mismatch")
    require_preflight(sources.get("admissionServiceAccount") == args.admission_service_account and
                      config.get("controllerUsername") == f"system:serviceaccount:{args.namespace}:{args.operator_service_account}" and
                      config.get("validationMode") == "strict" and config.get("platformNamespace") == args.namespace,
                      "source-admission-binding-mismatch")
    require_preflight(admission[3].get("admission-pod-state-namespace") == args.namespace,
                      "pod-admission-state-namespace-mismatch")
    users, groups = array_value(config.get("adminUsers")), array_value(config.get("adminGroups"))
    require_preflight(users or groups, "admission-administrators-missing")
    require_preflight(all(isinstance(v, str) and 0 < len(v) <= 256 and v == v.strip() and v.isprintable() and
                          "*" not in v and v != "system:anonymous" for v in users + groups) and
                      not any(v in ("system:authenticated", "system:unauthenticated") or
                              v.startswith("system:serviceaccounts") for v in groups),
                      "unsupported-admission-administrators")
    catalog = sources.get("skillsCatalogConfigMapName", "")
    if native_execution:
        require_preflight(sources.get("bootstrapOwnedPrerequisites") is True, "native-owned-bootstrap-required")
        gateway = environment_entry(operator[2], "AI_GATEWAY_ENABLED")
        require_preflight("valueFrom" not in gateway and gateway.get("value") == "true",
                          "native-gateway-enablement-required")
        require_preflight(object_value(sources.get("helpers", {})).get("desktop", False) is False,
                          "native-desktop-helper-unsupported")
        require_preflight(all(c.get("placementConfigMapName", "") == "" for c in (sources, config)),
                          "native-placement-unsupported")
        require_preflight(catalog == "" or (resource_name(catalog) and catalog != sources.get("platformConfigMapName")),
                          "native-skills-catalog-binding-invalid")
    home, skills = sources.get("homeClaimName"), sources.get("skillsClaimName")
    require_preflight(resource_name(home) and
                      (resource_name(skills) or (native_execution and skills == "" and catalog == "")) and
                      home != skills, "distinct-source-claims-required")
    fields = ("platformConfigMapName",) if native_execution else ("platformConfigMapName", "placementConfigMapName")
    names = [sources.get(key) for key in fields]
    require_preflight(all(resource_name(n) for n in names) and len(set(names)) == len(names) and
                      names == [config.get(key) for key in fields],
                      "platform-bindings-required" if native_execution else "platform-placement-bindings-required")
    for name, key, limit in zip(names, ("platform.json", "placement.json"), (512 * 1024, 256 * 1024)):
        value = kube.get("configmap", name, args.namespace)
        document = preflight_json(object_value(value.get("data")).get(key), limit)
        require_preflight(document.get("version") == "v1", "source-document-version-mismatch")
        if key == "platform.json":
            for field in ("registrations", "platforms", "runtimeClasses", "storageClasses"):
                object_value(document.get(field))
        else:
            for field in ("nodes", "platformDaemonSets"):
                array_value(document.get(field))
    return native_execution, planned_migration


def admission_service(kube, args, admission):
    service = kube.get("service", ADMISSION_NAME, args.namespace)
    spec = object_value(service.get("spec"))
    template = admission[1]
    labels = object_value(object_value(template.get("metadata")).get("labels"))
    selector = object_value(spec.get("selector"))
    require_preflight(selector and all(labels.get(key) == value for key, value in selector.items()),
                      "admission-service-selector-mismatch")
    require_preflight(spec.get("type") == "ClusterIP" and spec.get("clusterIP") not in (None, "", "None") and
                      not spec.get("externalName") and not spec.get("externalIPs"), "admission-service-type-mismatch")
    require_preflight(spec.get("ports") == [
        {"name": "admission", "port": 443, "targetPort": "admission", "protocol": "TCP"}],
        "admission-service-port-mismatch")
    container, flags = admission[2:]
    require_preflight(flags.get("admission-port") == "9443" and
                      flags.get("admission-cert-dir") == "/var/run/cetirun-admission" and
                      any(object_value(p) == {"name": "admission", "containerPort": 9443, "protocol": "TCP"}
                          for p in array_value(container.get("ports"))), "admission-listener-mismatch")
    probe = object_value(object_value(container.get("readinessProbe")).get("httpGet"))
    require_preflight(probe.get("path") == "/readyz" and probe.get("port") == "health" and
                      flags.get("health-probe-bind-address") == ":8081" and
                      any(object_value(p) == {"name": "health", "containerPort": 8081, "protocol": "TCP"}
                          for p in container["ports"]), "admission-readiness-probe-mismatch")
    mounts = array_value(container.get("volumeMounts"))
    volumes = array_value(object_value(template.get("spec")).get("volumes"))
    require_preflight(any(object_value(m) == {"name": "admission-tls", "mountPath": "/var/run/cetirun-admission",
                                             "readOnly": True} for m in mounts), "admission-tls-mount-mismatch")
    tls = [object_value(v) for v in volumes if object_value(v).get("name") == "admission-tls"]
    require_preflight(len(tls) == 1, "admission-tls-volume-missing")
    secret = object_value(tls[0].get("secret"))
    require_preflight(resource_name(secret.get("secretName")) and not secret.get("optional") and
                      secret.get("items") == [{"key": "tls.crt", "path": "tls.crt"}, {"key": "tls.key", "path": "tls.key"}],
                      "admission-tls-reference-mismatch")
    return selector


def controller_owner(value, kind):
    owners = [object_value(o) for o in array_value(object_value(value.get("metadata")).get("ownerReferences"))
              if object_value(o).get("controller") is True]
    require_preflight(len(owners) == 1 and owners[0].get("kind") == kind and
                      owners[0].get("apiVersion") == "apps/v1" and resource_name(owners[0].get("name")) and
                      exact_text(owners[0].get("uid")), "serving-owner-mismatch")
    return owners[0]


def admission_endpoints(kube, args, admission, selector):
    endpoints = kube.get("endpoints", ADMISSION_NAME, args.namespace)
    require_preflight(not object_value(endpoints["metadata"].get("annotations", {})).get("endpoints.kubernetes.io/over-capacity"),
                      "truncated-endpoints", "unknown")
    subsets = array_value(endpoints.get("subsets"))
    require_preflight(0 < len(subsets) <= 4, "serving-endpoints-unavailable")
    addresses = []
    for subset in subsets:
        subset = object_value(subset)
        require_preflight(not subset.get("notReadyAddresses") and subset.get("ports") ==
                          [{"name": "admission", "port": 9443, "protocol": "TCP"}], "serving-endpoints-not-ready")
        addresses.extend(array_value(subset.get("addresses")))
    require_preflight(2 <= len(addresses) <= 4, "unsupported-serving-endpoint-count")
    seen, nodes = set(), set()
    for address in addresses:
        address = object_value(address)
        ref = object_value(address.get("targetRef"))
        require_preflight(ref.get("kind") == "Pod" and ref.get("namespace") == args.namespace and
                          resource_name(ref.get("name")) and exact_text(ref.get("uid")), "serving-pod-reference-mismatch")
        require_preflight(ref["uid"] not in seen, "duplicate-serving-pod")
        seen.add(ref["uid"])
        pod_value = kube.get("pod", ref["name"], args.namespace, uid=ref["uid"])
        spec, status = object_value(pod_value.get("spec")), object_value(pod_value.get("status"))
        container, _ = deployed_container(spec, "admission", args.admission_image, args.admission_service_account)
        require_preflight(all(container.get(field) == admission[2].get(field) for field in
                              ("args", "env", "readinessProbe", "ports")), "serving-pod-configuration-drift")
        # Service-account admission adds a projected volume/mount to real Pods.
        for actual, expected in ((container, admission[2]), (spec, object_value(admission[1].get("spec")))):
            field = "volumeMounts" if actual is container else "volumes"
            selected = [object_value(v) for v in array_value(actual.get(field))
                        if object_value(v).get("name") == "admission-tls"]
            wanted = [v for v in array_value(expected.get(field)) if object_value(v).get("name") == "admission-tls"]
            require_preflight(selected == wanted, "serving-pod-tls-reference-drift")
        labels = object_value(pod_value["metadata"].get("labels"))
        require_preflight(all(labels.get(key) == value for key, value in selector.items()) and
                          status.get("phase") == "Running" and
                          any(object_value(c).get("type") == "Ready" and c.get("status") == "True"
                              for c in array_value(status.get("conditions"))) and
                          address.get("ip") in [object_value(p).get("ip") for p in array_value(status.get("podIPs"))],
                          "serving-pod-not-ready-or-address-mismatch")
        require_preflight(resource_name(spec.get("nodeName")), "serving-pod-placement-unknown", "unknown")
        nodes.add(spec["nodeName"])
        owner = controller_owner(pod_value, "ReplicaSet")
        replica_set = kube.get("replicaset", owner["name"], args.namespace, uid=owner["uid"])
        owner = controller_owner(replica_set, "Deployment")
        require_preflight(owner["name"] == args.admission_deployment and owner["uid"] == args.admission_uid,
                          "serving-deployment-mismatch")
    require_preflight(len(nodes) >= 2, "admission-replicas-not-spread")


def webhook_namespace_selector_matches(actual, expected, allow_aks_system_exclusions=False):
    if actual == expected:
        return True
    if not allow_aks_system_exclusions or not isinstance(actual, dict):
        return False
    if set(actual) != {"matchExpressions"}:
        return False
    expressions = actual["matchExpressions"]
    if not isinstance(expressions, list):
        return False
    required = expected.get("matchExpressions", []) + [
        {"key": "control-plane", "operator": "NotIn", "values": ["true"]},
        {"key": "kubernetes.azure.com/managedby", "operator": "NotIn", "values": ["aks"]},
    ]
    return len(expressions) == len(required) and all(expressions.count(item) == 1 for item in required)


def webhook_cleanup_matches(conditions, controller_username, *, pod=False, required=False):
    if conditions == []:
        return not required
    if (not isinstance(conditions, list) or len(conditions) != 1 or not isinstance(conditions[0], dict) or
            set(conditions[0]) != {"name", "expression"}):
        return False
    name = "garbage-collector-cleanup-only" if pod else "controller-cleanup-only"
    condition = conditions[0]
    expression = condition["expression"]
    if (condition["name"] != name or not isinstance(expression, str) or
            not 0 < len(expression) <= 8192 or not expression.isascii()):
        return False
    if not pod:
        controller = json.dumps(controller_username)
        if expression.count(controller) != 1:
            return False
        expression = expression.replace(controller, '"CONTROLLER"')
    return hashlib.sha256(expression.strip(" \t\r\n").encode("ascii")).hexdigest() == WEBHOOK_CLEANUP_DIGESTS[name]


def webhook_wiring(kube, args, admission, *, native_execution=False, planned_migration=False):
    timeout = admission[3].get("admission-pod-timeout")
    match = re.fullmatch(r"([0-9]{1,5})(ms|s)", timeout) if isinstance(timeout, str) else None
    require_preflight(match, "explicit-admission-request-budget-required", "unknown")
    budget_ms = int(match[1]) * (1000 if match[2] == "s" else 1)
    group = ["cetirun.microsoft.com"]
    pod_resources = ["pods", "pods/ephemeralcontainers", "pods/resize"]
    definitions = (
        ("validating", ADMISSION_NAME, "sandbox-configuration", "validate-sandbox-configuration",
         [(group, ["v1alpha1"], ["CREATE", "UPDATE"], ["agents", "teams", "sandboxes", "agentpolicies"], "Cluster"),
          (group, ["v1alpha1"], ["DELETE"], ["teams", "sandboxes", "agentpolicies"], "Cluster")]),
        ("mutating", ADMISSION_NAME, "sandbox-agent", "mutate-sandbox-agent",
         [(group, ["v1alpha1"], ["CREATE", "UPDATE"], ["agents"], "Cluster")]),
        ("validating", "cetirun-sandbox-pods", "sandbox-pods", "validate-sandbox-pods",
         [([""], ["v1"], ["CREATE", "UPDATE"], pod_resources, "Namespaced"),
          ([""], ["v1"], ["CONNECT"], ["pods/exec", "pods/attach", "pods/portforward"], "Namespaced")]),
    )
    if not native_execution:
        definitions += (
            ("validating", "cetirun-sandbox-placement", "sandbox-placement", "validate-sandbox-placement",
             [([""], ["v1"], ["CREATE", "UPDATE"], pod_resources, "Namespaced"),
              ([""], ["v1"], ["CREATE"], ["pods/binding", "bindings"], "Namespaced"),
              ([""], ["v1"], ["UPDATE", "DELETE"], ["nodes"], "Cluster")]),
        )
    ca = None
    for kind, name, hook_name, path, rules in definitions:
        value = kube.get(kind + "webhookconfiguration", name)
        hooks = array_value(value.get("webhooks"))
        require_preflight(len(hooks) == 1, "unsupported-webhook-layout", "unknown")
        hook = object_value(hooks[0])
        require_preflight(hook.get("name") == hook_name + ".cetirun.microsoft.com" and
                          hook.get("failurePolicy") == "Fail" and hook.get("sideEffects") == "None" and
                          hook.get("matchPolicy") == "Equivalent" and hook.get("admissionReviewVersions") == ["v1"] and
                          type(hook.get("timeoutSeconds")) is int and 1 <= hook["timeoutSeconds"] <= 30,
                          "webhook-fail-closed-wiring-mismatch")
        require_preflight(0 < budget_ms < hook["timeoutSeconds"] * 1000, "admission-request-budget-mismatch")
        client = object_value(hook.get("clientConfig"))
        bundle = client.get("caBundle")
        require_preflight(not client.get("url") and client.get("service") ==
                          {"name": ADMISSION_NAME, "namespace": args.namespace, "port": 443, "path": "/" + path} and
                          exact_text(bundle, 128 * 1024) and (ca is None or bundle == ca),
                          "webhook-client-wiring-mismatch")
        try:
            pem = base64.b64decode(bundle, validate=True).decode("ascii")
        except (binascii.Error, UnicodeError) as error:
            raise PreflightBlocked("webhook-ca-encoding-invalid") from error
        require_preflight(re.fullmatch(
            r"\s*(-----BEGIN CERTIFICATE-----\r?\n[A-Za-z0-9+/=\r\n]+-----END CERTIFICATE-----\s*)+", pem),
            "webhook-ca-encoding-invalid")
        ca = bundle
        expected = [dict(zip(("apiGroups", "apiVersions", "operations", "resources", "scope"), rule)) for rule in rules]
        guarded_pod = False
        rules_match = hook.get("rules") == expected
        if name == "cetirun-sandbox-pods":
            binding = {"apiGroups": [""], "apiVersions": ["v1"], "operations": ["CREATE"],
                       "resources": ["pods/binding", "bindings"], "scope": "Namespaced"}
            guarded_pod = hook.get("rules") == expected[:1] + [binding] + expected[1:]
            rules_match = guarded_pod or (not planned_migration and rules_match)
        namespace_selector = ({"matchExpressions": [{"key": "cetirun.microsoft.com/sandbox-protected", "operator": "Exists"}]}
                              if name == "cetirun-sandbox-pods" else {})
        require_preflight(rules_match and not hook.get("objectSelector") and
                          webhook_namespace_selector_matches(
                              hook.get("namespaceSelector", {}), namespace_selector,
                              getattr(args, "allow_aks_system_exclusions", False)),
                          "webhook-resource-wiring-mismatch")
        require_preflight(webhook_cleanup_matches(
            hook.get("matchConditions", []),
            f"system:serviceaccount:{args.namespace}:{args.operator_service_account}",
            pod=name in ("cetirun-sandbox-pods", "cetirun-sandbox-placement"),
            required=planned_migration or guarded_pod), "webhook-match-conditions-mismatch")
        require_preflight(kind != "mutating" or hook.get("reinvocationPolicy") == "Never",
                          "webhook-reinvocation-mismatch")


def deployment_preflight(args):
    names = ("arguments", "local-credentials", "cluster-identity", "api-version", "operator-deployment",
             "admission-deployment", "source-bindings", "admission-service", "admission-endpoints",
             "webhook-wiring", "stable-observation")
    result = {
        "version": "v1", "mode": "deployment-preflight", "status": "blocked", "qualifiesExecution": False,
        "executionReadiness": "unknown", "stockExecutionHold": "writer-exclusion-unavailable",
        "checks": [{"check": name, "status": "unknown", "reason": "not-checked"} for name in names],
        "unverified": [
            {"check": name, "status": "unknown"} for name in
            ("webhook-tls-and-request-evaluation", "webhook-match-conditions-and-namespace-policy",
             "source-document-semantics-and-namespace-write-protection", "operator-running-pods",
             "runtime-network-storage-and-provider-evidence")
        ],
    }

    @contextmanager
    def step(name):
        check = result["checks"][names.index(name)]
        try:
            yield
        except PreflightBlocked as error:
            check.update(status=error.status, reason=error.reason)
            raise
        except KeyboardInterrupt:
            check.update(status="unknown", reason="cancelled")
            raise
        check.update(status="observed", reason="matched-scoped-check")

    def cancelled(_signum, _frame):
        raise KeyboardInterrupt

    previous = signal.signal(signal.SIGTERM, cancelled)
    code = 1
    try:
        with step("arguments"):
            deployment_inputs(args)
        kube = DeploymentKubernetes(args.kubeconfig, args.context, args.cluster_uid)
        with step("local-credentials"):
            config = kube.credentials()
        with step("cluster-identity"):
            kube.check_target()
        with step("api-version"):
            version = preflight_json(kube.run(["get", "--raw", "/version"]))
            value = version.get("gitVersion")
            match = re.fullmatch(r"v(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?:\+[A-Za-z0-9.-]+)?", value) if isinstance(value, str) else None
            require_preflight(match and (int(match[1]), int(match[2])) >= (1, 37),
                              "stable-kubernetes-1.37-or-newer-required")
        with step("operator-deployment"):
            operator = deployed_workload(kube, args, "operator")
        with step("admission-deployment"):
            admission = deployed_workload(kube, args, "admission")
        with step("source-bindings"):
            native_execution, planned_migration = source_bindings(kube, args, operator, admission)
            if native_execution:
                result["stockExecutionHold"] = "native-single-start-unverified"
                result["unverified"].append({
                    "check": "native-bootstrap-gateway-and-effective-skill-selection", "status": "unknown",
                })
        with step("admission-service"):
            selector = admission_service(kube, args, admission)
        with step("admission-endpoints"):
            admission_endpoints(kube, args, admission, selector)
        with step("webhook-wiring"):
            webhook_wiring(kube, args, admission, native_execution=native_execution, planned_migration=planned_migration)
        with step("stable-observation"):
            kube.stable(config, version)
        result["status"] = "completed"
        code = 0
    except PreflightBlocked:
        pass
    except KeyboardInterrupt:
        code = 130
    finally:
        signal.signal(signal.SIGTERM, previous)
    print(json.dumps(result, sort_keys=True))
    return code


def preflight(kube, runtime_class, runtime_handler, storage_class):
    kube.check_target()
    try:
        version = json.loads(kube.run(["get", "--raw", "/version"]))["gitVersion"]
    except (json.JSONDecodeError, KeyError) as error:
        raise QualificationError("Kubernetes version response is incomplete") from error
    match = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)(?:[-+].*)?", version)
    if not match or int(match[1]) != 1 or int(match[2]) < 37:
        raise QualificationError("Sandbox admission requires the qualified Kubernetes 1.37-or-newer API")
    runtime = kube.get("runtimeclass", runtime_class)
    if runtime.get("handler") != runtime_handler or runtime.get("metadata", {}).get("deletionTimestamp"):
        raise QualificationError("RuntimeClass handler mismatch or terminating RuntimeClass")
    storage = kube.get("storageclass", storage_class)
    if not storage.get("provisioner") or storage.get("metadata", {}).get("deletionTimestamp"):
        raise QualificationError("Selected storage prerequisite is incomplete or terminating")
    for resource in ("ciliumnetworkpolicies.cilium.io", "ciliumendpoints.cilium.io"):
        crd = kube.get("crd", resource)
        if not any(c.get("type") == "Established" and c.get("status") == "True"
                   for c in crd.get("status", {}).get("conditions", [])):
            raise QualificationError("Required Cilium API is not established")
    return {
        "kubernetesVersion": version,
        "runtimeClassHandlerMatched": True,
        "storageClassPresent": True,
        "ciliumAPIsEstablished": True,
    }


def network_scripts(protocol):
    if protocol == "TCP":
        return SERVER, LISTENER_READINESS, PROBE
    if protocol == "UDP":
        return UDP_SERVER, UDP_LISTENER_READINESS, UDP_PROBE
    raise QualificationError("Network smoke protocol must be TCP or UDP")


def pod(namespace, run, role, image, runtime_class, protocol="TCP"):
    server, readiness, _ = network_scripts(protocol)
    spec = {
        "automountServiceAccountToken": False, "restartPolicy": "Never",
        "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532},
        "containers": [{
            "name": role, "image": image, "imagePullPolicy": "IfNotPresent",
            "command": ["python3", "-c", server if role == "server" else "import time;time.sleep(3600)"],
            "securityContext": {
                "allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                "capabilities": {"drop": ["ALL"]}, "seccompProfile": {"type": "RuntimeDefault"},
            },
            "resources": {
                "requests": {"cpu": "100m", "memory": "128Mi"},
                "limits": {"cpu": "1", "memory": "512Mi"},
            },
        }],
    }
    if role == "probe":
        spec["runtimeClassName"] = runtime_class
    else:
        spec["containers"][0]["env"] = [{
            "name": "POD_IP", "valueFrom": {"fieldRef": {"fieldPath": "status.podIP"}},
        }]
        spec["containers"][0]["readinessProbe"] = {
            "exec": {"command": ["python3", "-c", readiness]},
            "periodSeconds": 1, "timeoutSeconds": 5, "failureThreshold": 3,
        }
    return {
        "apiVersion": "v1", "kind": "Pod",
        "metadata": {"name": role, "namespace": namespace, "labels": {RUN_LABEL: run, "role": role}},
        "spec": spec,
    }


def verify_pod_identity(value, expected_uid, runtime_class=None):
    metadata = value.get("metadata", {})
    if metadata.get("uid") != expected_uid or metadata.get("deletionTimestamp"):
        raise QualificationError("Synthetic Pod identity changed or is terminating")
    if runtime_class is not None:
        spec = value.get("spec")
        if not isinstance(spec, dict) or spec.get("runtimeClassName") != runtime_class:
            raise QualificationError("Synthetic probe does not use the requested RuntimeClass")


def wait_ready(kube, namespace, name, expected_uid, timeout=180, *, runtime_class=None):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = kube.get("pod", name, namespace)
        verify_pod_identity(value, expected_uid, runtime_class)
        if value.get("status", {}).get("phase") in ("Failed", "Succeeded"):
            raise QualificationError("Synthetic probe exited before its readiness check")
        if any(c.get("type") == "Ready" and c.get("status") == "True"
               for c in value.get("status", {}).get("conditions", [])):
            return value
        time.sleep(1)
    raise QualificationError("Synthetic probe readiness timed out")


def connectivity(kube, namespace, address, protocol="TCP"):
    _, _, probe = network_scripts(protocol)
    try:
        ipaddress.ip_address(address)
    except ValueError as error:
        raise QualificationError("Synthetic server has an invalid Pod IP") from error
    raw = kube.run(["exec", "--namespace", namespace, "probe", "--", "python3", "-c", probe, address])
    try:
        result = json.loads(raw)
    except json.JSONDecodeError as error:
        raise QualificationError("Network probe output was not valid JSON") from error
    if not isinstance(result, list) or len(result) != 3 or any(type(v) is not bool for v in result):
        raise QualificationError("Network probe output had an unexpected shape")
    return result


def wait_connectivity(kube, namespace, address, expected, timeout=45, *, protocol="TCP"):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if connectivity(kube, namespace, address, protocol) == expected:
            return
        time.sleep(1)
    raise QualificationError("Network behavior did not match the expected allow/deny result")


def cleanup(kube, namespace, uid, run, timeout=120):
    kube.check_target()
    current = kube.get("namespace", namespace, absent_ok=True)
    if current is None:
        return
    metadata = current.get("metadata", {})
    if metadata.get("uid") != uid or metadata.get("labels", {}).get(RUN_LABEL) != run:
        raise QualificationError("Cleanup ownership mismatch; namespace retained")
    if not metadata.get("resourceVersion"):
        raise QualificationError("Cleanup resource version is missing; namespace retained")
    kube.run(
        ["delete", "--raw", "/api/v1/namespaces/" + namespace, "-f", "-"],
        {"apiVersion": "v1", "kind": "DeleteOptions",
         "preconditions": {"uid": uid, "resourceVersion": metadata["resourceVersion"]}},
    )
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        current = kube.get("namespace", namespace, absent_ok=True)
        if current is None:
            return
        if current.get("metadata", {}).get("uid") != uid:
            raise QualificationError("Namespace was replaced during cleanup; replacement retained")
        time.sleep(1)
    raise QualificationError("Cleanup timed out; no finalizers were removed")


def network_smoke(kube, image, runtime_class, result, protocol="TCP"):
    network_scripts(protocol)
    if not re.fullmatch(PINNED_IMAGE, image):
        raise QualificationError("Network probes require an explicit digest-pinned image with Python 3")
    result["networkProtocol"] = protocol
    run = uuid.uuid4().hex
    namespace = "cetirun-sandbox-probe-" + run[:12]
    result["testNamespace"] = namespace
    result["cleanupComplete"] = False
    kube.check_target()
    created = kube.create({
        "apiVersion": "v1", "kind": "Namespace",
        "metadata": {"name": namespace, "labels": {RUN_LABEL: run}},
    })
    uid = created.get("metadata", {}).get("uid")
    if not uid:
        raise QualificationError("Namespace creation identity is unknown; do not adopt or delete by name")
    failure = None
    try:
        pod_uids = {}
        for role in ("server", "probe"):
            created_pod = kube.create(pod(namespace, run, role, image, runtime_class, protocol))
            pod_uid = created_pod.get("metadata", {}).get("uid")
            if not isinstance(pod_uid, str) or not pod_uid.strip():
                raise QualificationError("Synthetic Pod creation identity is unknown; no test results may be accepted")
            verify_pod_identity(created_pod, pod_uid, runtime_class if role == "probe" else None)
            pod_uids[role] = pod_uid
        server = wait_ready(kube, namespace, "server", pod_uids["server"])
        wait_ready(kube, namespace, "probe", pod_uids["probe"], runtime_class=runtime_class)
        address = server.get("status", {}).get("podIP", "")
        if not address:
            raise QualificationError("Synthetic server has no Pod IP")
        wait_connectivity(kube, namespace, address, [True, True, True], protocol=protocol)
        result["networkChecks"] = {"baselineAllPorts": True}
        selector = {"matchLabels": {RUN_LABEL: run, "role": "probe"}}
        kube.create({
            "apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
            "metadata": {"name": "probe-deny", "namespace": namespace},
            "spec": {"endpointSelector": selector,
                     "egress": [{"toEntities": ["all"],
                                 "toPorts": [{"ports": [{"port": "8081", "protocol": protocol}]}]}],
                     "egressDeny": [{"toEntities": ["all"],
                                     "toPorts": [{"ports": [{"port": "8080", "protocol": protocol}]}]}]},
        })
        wait_connectivity(kube, namespace, address, [False, True, False], protocol=protocol)
        result["networkChecks"]["destinationPortDeny"] = True
        result["networkChecks"]["witnessInitiallyBlocked"] = True
        kube.create({
            "apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
            "metadata": {"name": "overlapping-allow", "namespace": namespace},
            "spec": {"podSelector": selector, "policyTypes": ["Egress"], "egress": [{}]},
        })
        # The probe tests the 8082 allow witness before the 8080 deny, so a
        # sample straddling policy application cannot reuse an earlier denial.
        wait_connectivity(kube, namespace, address, [False, True, True], protocol=protocol)
        result["networkChecks"]["overlappingAllowObserved"] = True
        result["networkChecks"]["denyOverOverlappingAllow"] = True
        for role, pod_uid in pod_uids.items():
            verify_pod_identity(kube.get("pod", role, namespace), pod_uid, runtime_class if role == "probe" else None)
        result["networkChecks"]["unchangedServerIdentity"] = True
        result["networkChecks"]["unchangedProbeIdentity"] = True
    except QualificationError as error:
        failure = error
        raise
    finally:
        try:
            cleanup(kube, namespace, uid, run)
        except QualificationError as error:
            if failure is not None:
                raise QualificationError(f"Smoke failed: {failure}; cleanup failed: {error}") from error
            raise
        result["cleanupComplete"] = True


LIFECYCLE_SNAPSHOT_FIELDS = (
    "artifactSet", "owner", "resources", "home", "accounting", "workload", "models", "peer", "extensions",
)
LIFECYCLE_RESOURCE_FIELDS = ("agent", "namespace", "statefulSet", "homePvc", "homePv")
LIFECYCLE_WORKLOAD_FIELDS = ("agent", "pod", "observedPod", "node", "generation", "observedGeneration", "ready")
LIFECYCLE_PEER_FIELDS = ("subject", "agent", "pod", "home", "homePv", "key", "budget", "spend", "ready")
LIFECYCLE_OPERATION_FIELDS = (
    "operation", "path", "requestAccepted", "stopRequested", "stopObserved", "sourceHealthy",
    "sourceTerminated", "sourceNode", "sourcePod", "csiReleased", "csiAttached", "currentWorkloadReady",
    "elapsedSeconds", "limitSeconds", "snapshot",
)
LIFECYCLE_REQUEST_FIELDS = (
    "case", "artifactSet", "response", "outcome", "reportedState", "observedState",
    "authoritativeObservation", "intentBefore", "intentAfter", "appliesBefore", "appliesAfter",
    "operationBefore", "operationAfter", "workload", "retryable", "failureObserved", "elapsedSeconds", "limitSeconds",
)
LIFECYCLE_REQUEST_BINDING_FIELDS = ("id", "agent", "generation", "sourcePod")
LIFECYCLE_REQUEST_WORKLOAD_FIELDS = ("agent", "pod", "observedPod", "generation", "observedGeneration", "ready")
LIFECYCLE_CLEANUP_FIELDS = (
    "artifactSet", "path", "authentication", "subject", "agent", "ordinary", "forced", "remaining",
    "peer", "elapsedSeconds", "limitSeconds",
)
LIFECYCLE_REQUESTS = {
    "duplicate": ("accepted", ("pending", "completed"), 0),
    "stale-uid": ("rejected", ("rejected",), 0),
    "stale-version": ("rejected", ("rejected",), 0),
    "lost-response": ("unknown", ("pending", "completed"), 1),
    "controller-takeover": ("accepted", ("pending", "completed"), 1),
    "suspend-precedence": ("accepted", ("pending", "suspended"), 0),
    "delete-precedence": ("accepted", ("pending", "deleted"), 0),
    "inflight-replacement": ("rejected", ("rejected",), 0),
    "destination-failure": ("accepted", ("pending", "failed"), 1),
}


def require_lifecycle(condition, reason):
    require_preflight(condition, reason, "failed")


def lifecycle_fields(value, names):
    require_preflight(value is not None, "missing-evidence")
    require_lifecycle(isinstance(value, dict), "invalid-evidence-shape")
    require_lifecycle(set(value).issubset(names), "unexpected-evidence-fields")
    require_preflight(set(names).issubset(value), "missing-evidence-fields")
    return value


def lifecycle_array(value, limit=64):
    require_preflight(value is not None, "missing-evidence")
    require_lifecycle(isinstance(value, list) and len(value) <= limit, "invalid-evidence-array")
    return value


def lifecycle_digest(value):
    return isinstance(value, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", value) is not None


def lifecycle_money(value):
    require_lifecycle(isinstance(value, str) and
                      re.fullmatch(r"[0-9]{1,12}(?:\.[0-9]{1,9})?", value) is not None,
                      "invalid-accounting-amount")
    return Decimal(value)


def lifecycle_bounds(value):
    limit, elapsed = value["limitSeconds"], value["elapsedSeconds"]
    require_lifecycle(type(limit) is int and 0 < limit <= 9999 and
                      type(elapsed) in (int, float) and 0 <= elapsed <= limit, "observation-outside-bound")


def lifecycle_case(value):
    lifecycle_fields(value, ("runtime", "execution", "modelSource", "identity", "helpers"))
    for key, choices in (
        ("runtime", ("openclaw", "hermes")), ("execution", ("ordinary", "sandbox-native")),
        ("modelSource", ("shared", "copilot-only", "mixed")),
        ("identity", ("none", "entra-obo", "azure-app-only")),
    ):
        require_preflight(isinstance(value[key], str) and value[key] in choices, "unsupported-case")
    helpers = lifecycle_array(value["helpers"], 5)
    require_preflight(all(isinstance(item, str) and item in
                          ("skills", "memory", "doctor", "logger", "desktop") for item in helpers),
                      "unsupported-helper")
    require_lifecycle(len(set(helpers)) == len(helpers), "duplicate-helper")
    return value


def lifecycle_extension_checks(case):
    checks = {
        "skills": ("skillsReadOnly", "skillsUnchanged"),
        "memory": ("memoryReadWrite", "memoryPrivate"),
        "doctor": ("doctorReady",),
        "logger": ("loggerReady",),
        "desktop": ("desktopOwnerAccess", "desktopPeerDenied"),
    }
    required = [check for helper in case["helpers"] for check in checks[helper]]
    if case["identity"] != "none":
        required += ["identityAuthorized", "identityPeerDenied", "identityRevocationObserved"]
    if case["identity"] == "azure-app-only":
        required += ["mcpAuthorized", "mcpPeerDenied"]
    return required


def lifecycle_request_cases(execution):
    return tuple(name for name in LIFECYCLE_REQUESTS
                 if execution == "sandbox-native" or name not in ("inflight-replacement", "destination-failure"))


def lifecycle_artifact_fingerprint(artifacts, case):
    lifecycle_case(case)
    lifecycle_fields(artifacts, ("sourceCommit", "sourceTree", "images", "charts", "configuration"))
    for key in ("sourceCommit", "sourceTree"):
        require_lifecycle(isinstance(artifacts[key], str) and
                          re.fullmatch(r"[0-9a-f]{40}", artifacts[key]) is not None, "source-pin-required")
    require_lifecycle(lifecycle_digest(artifacts["configuration"]), "configuration-pin-required")
    images = {"operator", "admission", "portal", "runtime", "gateway"}
    charts = {"operator", "portal", "runtime", "gateway"}
    images.update(set(case["helpers"]) - {"skills"})
    if case["modelSource"] != "shared":
        images.add("copilot")
    if case["identity"] != "none":
        images.add("broker")
        charts.add("broker")
    if case["identity"] == "azure-app-only":
        images.add("mcp")
        charts.add("mcp")
    for key, required, allowed in (
        ("images", images, images | {"copilot", "doctor", "logger", "desktop", "memory", "broker", "mcp"}),
        ("charts", charts, charts | {"broker", "mcp"}),
    ):
        pins = artifacts[key]
        require_lifecycle(isinstance(pins, dict) and required.issubset(pins) and
                          set(pins).issubset(allowed) and all(lifecycle_digest(pin) for pin in pins.values()),
                          "digest-pinned-artifacts-required")
    pinned = {"artifacts": artifacts, "case": {**case, "helpers": sorted(case["helpers"])}}
    return "sha256:" + hashlib.sha256(
        json.dumps(pinned, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()


def lifecycle_snapshots(receipt):
    snapshots = [lifecycle_fields(receipt["before"], LIFECYCLE_SNAPSHOT_FIELDS)]
    for operation in lifecycle_array(receipt["operations"]):
        lifecycle_fields(operation, LIFECYCLE_OPERATION_FIELDS)
        snapshots.append(lifecycle_fields(operation["snapshot"], LIFECYCLE_SNAPSHOT_FIELDS))
    return snapshots


def lifecycle_artifacts(receipt):
    fingerprint = lifecycle_artifact_fingerprint(receipt["artifacts"], receipt["case"])
    records = lifecycle_snapshots(receipt)
    records += [lifecycle_fields(item, LIFECYCLE_REQUEST_FIELDS)
                for item in lifecycle_array(receipt["requests"])]
    records.append(lifecycle_fields(receipt["cleanup"], LIFECYCLE_CLEANUP_FIELDS))
    require_lifecycle(all(item["artifactSet"] == fingerprint for item in records), "artifact-set-mismatch")
    return fingerprint


def lifecycle_authorization(receipt):
    authorization = lifecycle_fields(receipt["authorization"],
                                     ("target", "owner", "peer", "provider", "spend", "faults", "ownerControls"))
    require_preflight(all(value is True for value in authorization.values()), "authorization-or-controls-unavailable")


def lifecycle_owners(receipt):
    subjects = None
    for snapshot in lifecycle_snapshots(receipt):
        owner = lifecycle_fields(snapshot["owner"], (
            "path", "authentication", "subject", "peerSubject", "peerReadStatus", "peerMutationStatus",
            "nativeHttpStatus", "nativeWebSocketStatus",
        ))
        pair = (owner["subject"], owner["peerSubject"])
        require_lifecycle(all(lifecycle_digest(item) for item in pair) and pair[0] != pair[1] and
                          (subjects is None or subjects == pair), "distinct-stable-owners-required")
        subjects = pair
        require_lifecycle(owner["path"] in ("owner-rest", "owner-portal") and
                          owner["authentication"] == "oidc", "real-owner-path-required")
        require_lifecycle(all(type(owner[key]) is int for key in
                              ("peerReadStatus", "peerMutationStatus", "nativeHttpStatus", "nativeWebSocketStatus")),
                          "integer-http-status-required")
        require_lifecycle(owner["peerReadStatus"] in (403, 404) and owner["peerMutationStatus"] in (403, 404),
                          "peer-owner-access-not-denied")
        require_lifecycle(owner["nativeHttpStatus"] == 200 and owner["nativeWebSocketStatus"] == 101,
                          "authenticated-native-access-required")


def lifecycle_home(receipt):
    expected = receipt["expectedHome"]
    require_lifecycle(isinstance(expected, dict) and 0 < len(expected) <= 16 and
                      all(isinstance(name, str) and re.fullmatch(r"[a-z][a-z0-9-]{0,31}", name) and
                          lifecycle_digest(value) for name, value in expected.items()), "home-fixture-checksums-required")
    baseline = None
    for snapshot in lifecycle_snapshots(receipt):
        resources = lifecycle_fields(snapshot["resources"], LIFECYCLE_RESOURCE_FIELDS)
        require_lifecycle(all(lifecycle_digest(value) for value in resources.values()) and
                          (baseline is None or resources == baseline), "retained-resource-identity-mismatch")
        baseline = resources
        home = lifecycle_fields(snapshot["home"], ("path", "checksums", "readWrite"))
        require_lifecycle(home["path"] == "owner-application", "allowed-home-path-required")
        require_lifecycle(home["checksums"] == expected and home["readWrite"] is True,
                          "home-content-or-read-write-mismatch")


def lifecycle_accounting(receipt):
    baseline, previous_spend = None, Decimal(0)
    for snapshot in lifecycle_snapshots(receipt):
        accounting = lifecycle_fields(snapshot["accounting"], ("key", "budget", "spend", "spendAfterCall"))
        budget, spend, after_call = (lifecycle_money(accounting[key]) for key in ("budget", "spend", "spendAfterCall"))
        identity = (accounting["key"], budget)
        require_lifecycle(lifecycle_digest(identity[0]) and (baseline is None or identity == baseline),
                          "accounting-key-or-budget-changed")
        require_lifecycle(0 < spend < after_call <= budget and spend >= previous_spend,
                          "nonzero-cumulative-spend-required")
        baseline, previous_spend = identity, after_call


def lifecycle_recovery(receipt):
    snapshots = lifecycle_snapshots(receipt)
    operations = receipt["operations"]
    names = [operation["operation"] for operation in operations]
    require_lifecycle(all(isinstance(name, str) and name in ("restart", "suspend-resume", "migrate")
                          for name in names), "unsupported-operation")
    require_preflight("restart" in names and "suspend-resume" in names, "lifecycle-coverage-missing")
    if receipt["case"]["execution"] == "sandbox-native":
        require_preflight(names.count("migrate") >= 2, "repeated-migration-evidence-missing")
    else:
        require_lifecycle("migrate" not in names, "migration-requires-native-execution")
    sources = {"shared": ("shared",), "copilot-only": ("copilot",), "mixed": ("shared", "copilot")}
    previous = None
    pods = set()
    tool_witnesses = set()
    for snapshot in snapshots:
        resources = lifecycle_fields(snapshot["resources"], LIFECYCLE_RESOURCE_FIELDS)
        workload = lifecycle_fields(snapshot["workload"], LIFECYCLE_WORKLOAD_FIELDS)
        require_lifecycle(lifecycle_digest(workload["pod"]) and lifecycle_digest(workload["node"]) and
                          type(workload["generation"]) is int and workload["generation"] > 0 and
                          workload["ready"] is True, "current-ready-workload-required")
        require_lifecycle(lifecycle_digest(workload["agent"]) and workload["agent"] == resources["agent"] and
                          type(workload["observedGeneration"]) is int and
                          workload["observedGeneration"] == workload["generation"] and
                          workload["observedPod"] == workload["pod"], "current-workload-binding-mismatch")
        require_lifecycle(workload["pod"] not in pods, "fresh-execution-required")
        pods.add(workload["pod"])
        if previous is not None:
            require_lifecycle(workload["generation"] > previous["generation"], "fresh-execution-required")
        previous = workload
        models = lifecycle_fields(snapshot["models"], sources[receipt["case"]["modelSource"]])
        for model in models.values():
            lifecycle_fields(model, ("reply", "expectedToolChecksum", "observedToolChecksum"))
            require_lifecycle(model["reply"] is True and lifecycle_digest(model["expectedToolChecksum"]) and
                              model["expectedToolChecksum"] == model["observedToolChecksum"],
                              "model-reply-and-independent-tool-witness-required")
            require_lifecycle(model["expectedToolChecksum"] not in tool_witnesses, "fresh-tool-witness-required")
            tool_witnesses.add(model["expectedToolChecksum"])
    for index, operation in enumerate(operations):
        lifecycle_bounds(operation)
        require_lifecycle(all(type(operation[key]) is bool for key in
                              ("sourceHealthy", "csiReleased", "csiAttached")), "invalid-operation-observations")
        require_lifecycle(lifecycle_digest(operation["sourceNode"]) and
                          operation["sourceNode"] == snapshots[index]["workload"]["node"] and
                          lifecycle_digest(operation["sourcePod"]) and
                          operation["sourcePod"] == snapshots[index]["workload"]["pod"],
                          "source-workload-mismatch")
        require_lifecycle(all(operation[key] is True for key in
                              ("requestAccepted", "stopRequested", "stopObserved", "sourceTerminated",
                               "currentWorkloadReady")), "acceptance-stop-and-ready-observations-required")
        if operation["operation"] == "migrate":
            require_lifecycle(operation["path"] == "admin-kubernetes" and
                              all(operation[key] is True for key in ("sourceHealthy", "csiReleased", "csiAttached")) and
                              operation["sourceNode"] != snapshots[index + 1]["workload"]["node"],
                              "healthy-source-and-csi-move-required")
        else:
            require_lifecycle(operation["path"] in ("owner-rest", "owner-portal"), "owner-lifecycle-path-required")


def lifecycle_peers(receipt):
    snapshots = lifecycle_snapshots(receipt)
    cleanup = lifecycle_fields(receipt["cleanup"], LIFECYCLE_CLEANUP_FIELDS)
    peers = [snapshot["peer"] for snapshot in snapshots] + [cleanup["peer"]]
    before = snapshots[0]
    owner = lifecycle_fields(before["owner"], (
        "path", "authentication", "subject", "peerSubject", "peerReadStatus", "peerMutationStatus",
        "nativeHttpStatus", "nativeWebSocketStatus",
    ))
    resources = lifecycle_fields(before["resources"], LIFECYCLE_RESOURCE_FIELDS)
    workloads = [lifecycle_fields(snapshot["workload"], LIFECYCLE_WORKLOAD_FIELDS)
                 for snapshot in snapshots]
    accounting = lifecycle_fields(before["accounting"], ("key", "budget", "spend", "spendAfterCall"))
    baseline, previous_spend = None, Decimal(0)
    for peer in peers:
        lifecycle_fields(peer, LIFECYCLE_PEER_FIELDS)
        identity = tuple(peer[key] for key in ("subject", "agent", "pod", "home", "homePv", "key"))
        require_lifecycle(all(lifecycle_digest(value) for value in identity) and
                          peer["subject"] == owner["peerSubject"] != owner["subject"] and
                          peer["agent"] != resources["agent"] and
                          all(peer["pod"] != current["pod"] for current in workloads) and
                          peer["home"] != resources["homePvc"] and peer["homePv"] != resources["homePv"] and
                          peer["key"] != accounting["key"], "independent-peer-required")
        retained = (*identity, lifecycle_money(peer["budget"]))
        spend = lifecycle_money(peer["spend"])
        require_lifecycle((baseline is None or retained == baseline) and peer["ready"] is True and
                          previous_spend <= spend <= retained[-1], "peer-continuity-mismatch")
        baseline, previous_spend = retained, spend


def lifecycle_requests(receipt):
    requests = [lifecycle_fields(item, LIFECYCLE_REQUEST_FIELDS) for item in lifecycle_array(receipt["requests"])]
    expected = lifecycle_request_cases(receipt["case"]["execution"])
    names = [item["case"] for item in requests]
    require_lifecycle(all(isinstance(name, str) and name in expected for name in names), "unexpected-request-case")
    require_lifecycle(len(set(names)) == len(names), "duplicate-request-evidence")
    require_preflight(set(names) == set(expected), "request-coverage-missing")
    snapshot = lifecycle_fields(receipt["before"], LIFECYCLE_SNAPSHOT_FIELDS)
    agent = lifecycle_fields(snapshot["resources"], LIFECYCLE_RESOURCE_FIELDS)["agent"]
    peer_pod = lifecycle_fields(snapshot["peer"], LIFECYCLE_PEER_FIELDS)["pod"]
    for item in requests:
        response, outcomes, maximum = LIFECYCLE_REQUESTS[item["case"]]
        lifecycle_bounds(item)
        require_lifecycle(item["response"] == response and item["outcome"] in outcomes and
                          item["reportedState"] == item["observedState"] == item["outcome"],
                          "request-outcome-mismatch")
        require_preflight(item["authoritativeObservation"] is True, "request-state-unobserved")
        require_lifecycle(lifecycle_digest(item["intentBefore"]) and item["intentBefore"] == item["intentAfter"],
                          "original-intent-not-retained")
        before, after = item["appliesBefore"], item["appliesAfter"]
        require_lifecycle(type(before) is int and type(after) is int and
                          0 <= before <= after <= before + maximum, "unexpected-workload-apply")
        require_lifecycle(item["outcome"] != "completed" or after > 0, "completed-workload-apply-required")
        original = lifecycle_fields(item["operationBefore"], LIFECYCLE_REQUEST_BINDING_FIELDS)
        observed = lifecycle_fields(item["operationAfter"], LIFECYCLE_REQUEST_BINDING_FIELDS)
        for binding in (original, observed):
            require_lifecycle(all(lifecycle_digest(binding[key]) for key in ("id", "agent", "sourcePod")) and
                              type(binding["generation"]) is int and binding["generation"] > 0,
                              "invalid-operation-binding")
        require_lifecycle(original == observed and original["agent"] == agent and
                          original["sourcePod"] != peer_pod, "operation-continuity-mismatch")
        workload = lifecycle_fields(item["workload"], LIFECYCLE_REQUEST_WORKLOAD_FIELDS)
        require_lifecycle(workload["agent"] == agent and type(workload["generation"]) is int and
                          workload["generation"] == original["generation"] and
                          type(workload["observedGeneration"]) is int and
                          0 <= workload["observedGeneration"] <= workload["generation"] and
                          type(workload["ready"]) is bool, "request-workload-binding-mismatch")
        require_lifecycle(workload["pod"] == workload["observedPod"] and
                          (workload["pod"] is None or lifecycle_digest(workload["pod"])) and
                          workload["pod"] != peer_pod and
                          (not workload["ready"] or (workload["pod"] is not None and
                                                    workload["observedGeneration"] == workload["generation"])),
                          "request-workload-binding-mismatch")
        if item["outcome"] == "completed":
            require_lifecycle(workload["ready"] and workload["pod"] != original["sourcePod"],
                              "request-completion-unobserved")
        elif item["outcome"] != "rejected":
            require_lifecycle(not workload["ready"], "request-outcome-workload-mismatch")
            if item["outcome"] in ("suspended", "deleted"):
                require_lifecycle(workload["pod"] is None, "request-shutdown-unobserved")
        require_lifecycle(item["retryable"] is (item["outcome"] == "pending") and
                          item["failureObserved"] is (item["case"] == "destination-failure"),
                          "request-failure-observation-mismatch")


def lifecycle_cleanup(receipt):
    cleanup = lifecycle_fields(receipt["cleanup"], LIFECYCLE_CLEANUP_FIELDS)
    before = lifecycle_snapshots(receipt)[0]
    owner = object_value(before["owner"])
    resources = lifecycle_fields(before["resources"], LIFECYCLE_RESOURCE_FIELDS)
    lifecycle_bounds(cleanup)
    require_lifecycle(lifecycle_digest(cleanup["agent"]) and cleanup["agent"] == resources["agent"],
                      "cleanup-target-mismatch")
    require_lifecycle(cleanup["path"] in ("owner-rest", "owner-portal") and cleanup["authentication"] == "oidc" and
                      lifecycle_digest(cleanup["subject"]) and cleanup["subject"] == owner.get("subject"),
                      "owner-deletion-path-required")
    require_lifecycle(cleanup["ordinary"] is True and cleanup["forced"] is False, "ordinary-finalization-required")
    remaining = lifecycle_fields(cleanup["remaining"],
                                 ("agent", "namespace", "homePvc", "homePv", "attachments",
                                  "controlRecords", "reservations", "gatewayKey"))
    require_lifecycle(all(type(count) is int and count == 0 for count in remaining.values()), "cleanup-incomplete")


def lifecycle_extensions(receipt):
    expected = lifecycle_extension_checks(receipt["case"])
    for snapshot in lifecycle_snapshots(receipt):
        observations = lifecycle_fields(snapshot["extensions"], expected)
        require_lifecycle(all(value is True for value in observations.values()), "extension-witness-mismatch")


def assess_lifecycle_receipt(receipt):
    checks = {
        "artifacts": lifecycle_artifacts, "authorization": lifecycle_authorization,
        "owner-access": lifecycle_owners, "home": lifecycle_home, "accounting": lifecycle_accounting,
        "recovery": lifecycle_recovery, "peers": lifecycle_peers, "requests": lifecycle_requests,
        "cleanup": lifecycle_cleanup, "extensions": lifecycle_extensions,
    }
    result = {
        "version": "v1", "mode": "acceptance-check", "status": "blocked", "evidenceKind": "unknown",
        "qualifiesExecution": False, "liveAcceptance": "not-established",
        "checks": [{"check": name, "status": "blocked", "reason": "not-checked"} for name in checks],
    }
    try:
        lifecycle_fields(receipt, ("version", "evidenceKind", "case", "artifacts", "authorization",
                                   "expectedHome", "before", "operations", "requests", "cleanup"))
        require_lifecycle(receipt["version"] == "v1" and receipt["evidenceKind"] in ("synthetic", "live"),
                          "invalid-evidence-version-or-kind")
        lifecycle_case(receipt["case"])
    except PreflightBlocked as error:
        result.update(status="failed" if error.status == "failed" else "blocked", error=error.reason)
        return result
    result.update(evidenceKind=receipt["evidenceKind"],
                  case={**receipt["case"], "helpers": sorted(receipt["case"]["helpers"])})
    for check in result["checks"]:
        try:
            observed = checks[check["check"]](receipt)
            if check["check"] == "artifacts":
                result["artifactSet"] = observed
            check.update(status="matched", reason="scoped-assertions-matched")
        except PreflightBlocked as error:
            check.update(status="failed" if error.status == "failed" else "blocked", reason=error.reason)
    statuses = {check["status"] for check in result["checks"]}
    result["status"] = "failed" if "failed" in statuses else "blocked" if "blocked" in statuses else "matched"
    return result


def lifecycle_acceptance(argv):
    parser = SanitizedParser(description="Offline assertions over one sanitized, artifact-pinned lifecycle receipt.")
    parser.add_argument("--receipt", required=True, help="local JSON evidence; never an authorization or execution input")
    result = {"version": "v1", "mode": "acceptance-check", "status": "blocked",
              "qualifiesExecution": False, "liveAcceptance": "not-established"}
    try:
        args = parser.parse_args(argv)
    except PreflightBlocked:
        result["error"] = "invalid-command-line; use acceptance-check --help"
        print(json.dumps(result, sort_keys=True))
        return 2
    try:
        with open(args.receipt, "rb", opener=lambda path, flags: os.open(path, flags | os.O_NONBLOCK)) as stream:
            require_preflight(stat.S_ISREG(os.fstat(stream.fileno()).st_mode), "receipt-not-regular-file")
            raw = stream.read(1024 * 1024 + 1)
        result = assess_lifecycle_receipt(preflight_json(raw.decode("utf-8")))
    except (OSError, UnicodeError):
        result["error"] = "receipt-unreadable"
    except PreflightBlocked as error:
        result["error"] = error.reason
    print(json.dumps(result, sort_keys=True))
    return 0 if result["status"] == "matched" else 1


SNAPSHOT_RESOURCES = {
    "cluster": ("v1", "Namespace", "/api/v1/namespaces"),
    "agent": ("cetirun.microsoft.com/v1alpha1", "Agent", "/apis/cetirun.microsoft.com/v1alpha1/agents"),
    "namespace": ("v1", "Namespace", "/api/v1/namespaces"),
    "statefulSet": ("apps/v1", "StatefulSet", "/apis/apps/v1/namespaces/{namespace}/statefulsets"),
    "pod": ("v1", "Pod", "/api/v1/namespaces/{namespace}/pods"),
    "homePvc": ("v1", "PersistentVolumeClaim", "/api/v1/namespaces/{namespace}/persistentvolumeclaims"),
    "homePv": ("v1", "PersistentVolume", "/api/v1/persistentvolumes"),
    "attachment": ("storage.k8s.io/v1", "VolumeAttachment", "/apis/storage.k8s.io/v1/volumeattachments"),
}


def snapshot_regular_file(path):
    require_preflight(isinstance(path, str) and 0 < len(path) <= 4096 and "\0" not in path, "invalid-input-path")
    try:
        info = os.lstat(path)
    except (OSError, ValueError) as error:
        raise PreflightBlocked("input-file-unreadable") from error
    require_preflight(stat.S_ISREG(info.st_mode), "input-not-regular-file")
    return info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns


def snapshot_scope(path, state="ready"):
    snapshot_regular_file(path)
    try:
        with open(path, "rb", opener=lambda p, flags: os.open(p, flags | os.O_NONBLOCK | os.O_NOFOLLOW)) as stream:
            require_preflight(stat.S_ISREG(os.fstat(stream.fileno()).st_mode), "input-not-regular-file")
            scope = preflight_json(stream.read(64 * 1024 + 1).decode("utf-8"), 64 * 1024)
    except (OSError, UnicodeError) as error:
        raise PreflightBlocked("scope-unreadable") from error
    lifecycle_fields(scope, ("version", "run", "case", "artifacts", "resources"))
    require_preflight(scope["version"] == "v1" and resource_name(scope["run"], 63), "invalid-snapshot-scope")
    fingerprint = lifecycle_artifact_fingerprint(scope["artifacts"], scope["case"])
    resources = lifecycle_fields(scope["resources"], tuple(key for key in SNAPSHOT_RESOURCES if key != "cluster"))
    uids = []
    for key, pin in resources.items():
        if pin is None and (key == "attachment" or (key == "pod" and state == "suspended")):
            continue
        fields = ("name", "uid", "resourceVersion", "generation") if key == "agent" else ("name", "uid")
        lifecycle_fields(pin, fields)
        require_preflight(resource_name(pin["name"]) and exact_text(pin["uid"]), "explicit-resource-pins-required")
        uids.append(pin["uid"])
    namespace = resources["namespace"]["name"]
    require_preflight(resource_name(namespace, 63) and "." not in namespace and namespace not in
                      ("default", "kube-system", "kube-public", "kube-node-lease"), "invalid-snapshot-namespace")
    agent = resources["agent"]
    require_preflight(exact_text(agent["resourceVersion"]) and type(agent["generation"]) is int and
                      0 < agent["generation"] < 2**63, "explicit-agent-version-required")
    require_preflight(len(uids) == len(set(uids)), "distinct-resource-identities-required")
    return scope, fingerprint


def snapshot_selector(labels):
    labels = object_value(labels)
    require_preflight(set(labels) == {"app.kubernetes.io/name", "app.kubernetes.io/instance"} and
                      all(resource_name(value, 63) for value in labels.values()),
                      "unsupported-runtime-selector")
    return labels


def snapshot_attachment_unknown(error):
    return error.status == "unknown" and error.reason in (
        "kubernetes-request-failed", "command-unavailable-timeout-or-invalid-output", "request-timeout",
        "response-too-large", "invalid-document-size", "invalid-json-document", "duplicate-json-field",
        "unexpected-object-shape", "missing-resource-identity", "csi-attachment-not-observed",
    )


def snapshot_content(value):
    # Python equates False with 0; stability comparisons must retain JSON types.
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


class SnapshotKubernetes(DeploymentKubernetes):
    """Only pinned objects and two bounded candidate lists in the owned namespace."""

    def __init__(self, args, scope):
        super().__init__(args.kubeconfig, args.context, args.cluster_uid)
        self.scope = scope
        self.state = getattr(args, "state", "ready")
        self.attachment_error = None
        self.pins = {"cluster": {"name": "kube-system", "uid": args.cluster_uid}, **scope["resources"]}
        self.namespace = self.pins["namespace"]["name"]
        self.paths = {
            key: base.format(namespace=self.namespace) + "/" + self.pins[key]["name"]
            for key, (_, _, base) in SNAPSHOT_RESOURCES.items() if self.pins[key] is not None
        }
        self.objects = dict.fromkeys(self.paths.values())
        self.observed = {}
        self.lists = {}

    def run(self, args, document=None, timeout=None):
        require_preflight(args == self.CONFIG_COMMAND or
                          (len(args) == 3 and args[:2] == ["get", "--raw"] and args[2] in self.objects),
                          "request-not-allowlisted")
        return super().run(args, document, timeout)

    def observe(self, key, value):
        api, kind, base = SNAPSHOT_RESOURCES[key]
        meta = object_value(value.get("metadata"))
        pin = self.pins[key]
        require_preflight(value.get("apiVersion") == api and value.get("kind") == kind and
                          meta.get("name") == pin["name"] and
                          meta.get("namespace") == (self.namespace if "{namespace}" in base else None),
                          "resource-identity-mismatch")
        # Incomplete attachment evidence must not hide a known identity/lifecycle conflict.
        if exact_text(meta.get("uid")):
            require_preflight(meta["uid"] == pin["uid"], "resource-uid-mismatch")
        require_preflight(not meta.get("deletionTimestamp"), "resource-terminating")
        if key != "cluster":
            labels = object_value(meta.get("labels", {}))
            require_preflight((key not in ("agent", "namespace") and RUN_LABEL not in labels) or
                              labels.get(RUN_LABEL) == self.scope["run"], "run-label-mismatch")
        require_preflight(exact_text(meta.get("uid")) and exact_text(meta.get("resourceVersion")),
                          "missing-resource-identity", "unknown")
        if key == "agent":
            require_preflight(meta["resourceVersion"] == pin["resourceVersion"] and
                              type(meta.get("generation")) is int and meta["generation"] == pin["generation"],
                              "agent-version-mismatch")
        content = snapshot_content(value)
        require_preflight(key not in self.observed or self.observed[key] == content, "resource-version-drift")
        self.observed[key] = content
        return value

    def get(self, key):
        require_preflight(key in self.paths, "resource-not-allowlisted")
        return self.observe(key, preflight_json(self.run(["get", "--raw", self.paths[key]])))

    def check_target(self):
        self.get("cluster")

    def unique(self, key, labels=None):
        """Require the pinned singleton, or zero namespace Pods when suspended."""
        require_preflight(key in ("statefulSet", "pod"), "resource-not-allowlisted")
        query = {"limit": 2}
        # Suspension must also see Pods with stale or missing selector labels.
        empty_pods = key == "pod" and self.state == "suspended"
        if key == "pod" and not empty_pods:
            labels = snapshot_selector(labels)
            query["labelSelector"] = ",".join(name + "=" + labels[name] for name in sorted(labels))
        else:
            require_preflight(labels is None, "unsupported-runtime-selector")
        api, kind, base = SNAPSHOT_RESOURCES[key]
        path = base.format(namespace=self.namespace) + "?" + urlencode(query)
        self.objects[path] = None
        value = preflight_json(self.run(["get", "--raw", path]))
        require_preflight(value.get("apiVersion") == api and value.get("kind") == kind + "List",
                          "candidate-list-shape-mismatch", "unknown")
        meta = object_value(value.get("metadata"))
        remaining = meta.get("remainingItemCount")
        require_preflight(exact_text(meta.get("resourceVersion")) and meta.get("continue", "") == "" and
                          (remaining is None or (type(remaining) is int and remaining == 0)),
                          "candidate-list-incomplete", "unknown")
        items = array_value(value.get("items"))
        require_preflight(len(items) == (0 if empty_pods else 1),
                          "pod-candidates-not-empty" if empty_pods else "candidate-set-not-singleton")
        if not empty_pods:
            self.observe(key, {"apiVersion": api, "kind": kind, **object_value(items[0])})
        self.lists[key] = labels

    def stable(self, config):
        require_preflight(snapshot_content(self.credentials()) == snapshot_content(config), "kubeconfig-drift")
        for key in tuple(self.observed):
            try:
                self.get(key)
            except PreflightBlocked as error:
                if key != "attachment" or self.state != "suspended" or not snapshot_attachment_unknown(error):
                    raise
                self.attachment_error = error.reason
        for key, labels in tuple(self.lists.items()):
            self.unique(key, labels)
        require_preflight(time.monotonic() < self.deadline, "deadline-exhausted", "unknown")

    def pseudonym(self, kind, value):
        raw = json.dumps([self.cluster_uid, kind, value], separators=(",", ":")).encode("utf-8")
        return "sha256:" + hashlib.sha256(raw).hexdigest()


def snapshot_owner(value, parent, api, kind):
    owners = array_value(object_value(value.get("metadata")).get("ownerReferences"))
    require_preflight(len(owners) == 1, "resource-owner-mismatch")
    owner = object_value(owners[0])
    require_preflight(owner.get("apiVersion") == api and owner.get("kind") == kind and
                      owner.get("controller") is True and owner.get("name") == parent["name"] and
                      owner.get("uid") == parent["uid"], "resource-owner-mismatch")


def snapshot_runtime(spec, image_digest, home_name):
    containers = [object_value(c) for c in array_value(spec.get("containers"))]
    main = [c for c in containers if c.get("name") == "cetirun"]
    require_preflight(len(main) == 1, "main-container-not-singleton")
    image = main[0].get("image")
    require_preflight(isinstance(image, str) and len(image) <= 512 and re.fullmatch(PINNED_IMAGE, image) and
                      image.endswith("@" + image_digest), "runtime-image-pin-mismatch")
    mounts = [object_value(m) for m in array_value(main[0].get("volumeMounts"))
              if object_value(m).get("mountPath") == "/home/vibe"]
    require_preflight(len(mounts) == 1 and mounts[0].get("name") == "data" and
                      mounts[0].get("readOnly", False) is False and
                      not mounts[0].get("subPath") and not mounts[0].get("subPathExpr"),
                      "home-mount-mismatch")
    volumes = [object_value(v) for v in array_value(spec.get("volumes")) if object_value(v).get("name") == "data"]
    require_preflight(len(volumes) == 1, "home-volume-not-singleton")
    claim = object_value(volumes[0].get("persistentVolumeClaim"))
    require_preflight(claim.get("claimName") == home_name and claim.get("readOnly", False) is False,
                      "home-claim-mismatch")
    return image


def snapshot_workload(kube):
    stateful_set = kube.get("statefulSet")
    snapshot_owner(stateful_set, kube.pins["agent"], "cetirun.microsoft.com/v1alpha1", "Agent")
    kube.unique("statefulSet")
    spec, status = object_value(stateful_set.get("spec")), object_value(stateful_set.get("status"))
    generation = stateful_set["metadata"].get("generation")
    current = (type(generation) is int and generation > 0 and
               type(status.get("observedGeneration")) is int and status["observedGeneration"] == generation)
    if kube.state == "suspended":
        require_preflight(current and type(spec.get("replicas")) is int and spec["replicas"] == 0 and
                          type(status.get("replicas")) is int and status["replicas"] == 0 and
                          all(type(status.get(key, 0)) is int and status.get(key, 0) == 0 for key in (
                              "readyReplicas", "currentReplicas", "updatedReplicas", "availableReplicas",
                              "terminatingReplicas",
                          )), "statefulset-not-current-zero")
    else:
        require_preflight(current and type(spec.get("replicas")) is int and spec["replicas"] == 1 and
                          all(type(status.get(key)) is int and status[key] == 1 for key in
                              ("replicas", "readyReplicas", "currentReplicas", "updatedReplicas")) and
                          exact_text(status.get("currentRevision")) and
                          status["currentRevision"] == status.get("updateRevision"), "statefulset-not-current-ready")
    selector = lifecycle_fields(spec.get("selector"), ("matchLabels",))
    if kube.state == "suspended":
        snapshot_selector(selector["matchLabels"])
    template = object_value(spec.get("template"))
    template_labels = object_value(object_value(template.get("metadata")).get("labels"))
    require_preflight(RUN_LABEL not in template_labels or template_labels[RUN_LABEL] == kube.scope["run"],
                      "run-label-mismatch")
    require_preflight(all(template_labels.get(key) == value for key, value in
                          object_value(selector["matchLabels"]).items()), "runtime-selector-mismatch")
    image = snapshot_runtime(object_value(template.get("spec")), kube.scope["artifacts"]["images"]["runtime"],
                             kube.pins["homePvc"]["name"])
    return selector["matchLabels"], image, status.get("currentRevision")


def snapshot_pod(kube, labels, image, revision):
    pod_value = kube.get("pod")
    snapshot_owner(pod_value, kube.pins["statefulSet"], "apps/v1", "StatefulSet")
    kube.unique("pod", labels)
    spec, status = object_value(pod_value.get("spec")), object_value(pod_value.get("status"))
    pod_labels = object_value(pod_value["metadata"].get("labels"))
    require_preflight(all(pod_labels.get(key) == value for key, value in labels.items()) and
                      pod_labels.get("controller-revision-hash") == revision, "pod-revision-or-selector-mismatch")
    require_preflight(snapshot_runtime(spec, kube.scope["artifacts"]["images"]["runtime"],
                                      kube.pins["homePvc"]["name"]) == image, "runtime-image-pin-mismatch")
    ready = [object_value(c) for c in array_value(status.get("conditions", []))
             if object_value(c).get("type") == "Ready"]
    require_preflight(status.get("phase") == "Running" and len(ready) == 1 and ready[0].get("status") == "True" and
                      resource_name(spec.get("nodeName")), "pod-not-current-ready")
    containers = [object_value(c) for c in array_value(status.get("containerStatuses", []))
                  if object_value(c).get("name") == "cetirun"]
    require_preflight(len(containers) == 1 and containers[0].get("ready") is True and
                      isinstance(object_value(containers[0].get("state")).get("running"), dict),
                      "main-container-not-running-ready")
    image_id = containers[0].get("imageID")
    require_preflight(exact_text(image_id, 512), "runtime-image-id-unverified", "unknown")
    image_id = re.sub(r"^(docker-pullable|docker|containerd|cri-o)://", "", image_id.rsplit("@", 1)[-1])
    require_preflight(image_id == kube.scope["artifacts"]["images"]["runtime"],
                      "runtime-image-id-unverified", "unknown")
    return spec["nodeName"]


def snapshot_home(kube):
    pvc, pv = kube.get("homePvc"), kube.get("homePv")
    claim_spec, volume_spec = object_value(pvc.get("spec")), object_value(pv.get("spec"))
    require_preflight(object_value(pvc.get("status")).get("phase") == "Bound" and
                      object_value(pv.get("status")).get("phase") == "Bound", "home-storage-not-bound")
    reference = object_value(volume_spec.get("claimRef"))
    require_preflight(claim_spec.get("volumeName") == kube.pins["homePv"]["name"] and
                      reference.get("name") == kube.pins["homePvc"]["name"] and
                      reference.get("uid") == kube.pins["homePvc"]["uid"] and
                      reference.get("namespace") == kube.namespace and
                      reference.get("kind") == "PersistentVolumeClaim" and
                      claim_spec.get("volumeMode", "Filesystem") == volume_spec.get("volumeMode", "Filesystem") == "Filesystem",
                      "home-storage-binding-mismatch")
    csi = object_value(volume_spec.get("csi"))
    require_preflight(resource_name(csi.get("driver")) and exact_text(csi.get("volumeHandle"), 2048),
                      "csi-volume-identity-missing", "unknown")
    return csi["driver"]


def snapshot_attachment(kube, node, driver):
    attachment = kube.get("attachment")
    spec = object_value(attachment.get("spec"))
    require_preflight(spec.get("attacher") == driver and
                      (resource_name(spec.get("nodeName")) if kube.state == "suspended" else spec.get("nodeName") == node) and
                      object_value(spec.get("source")) == {"persistentVolumeName": kube.pins["homePv"]["name"]},
                      "csi-attachment-binding-mismatch")
    status = object_value(attachment.get("status"))
    require_preflight(status.get("attached") is True and not status.get("attachError") and not status.get("detachError"),
                      "csi-attachment-not-observed", "unknown" if kube.state == "suspended" else "blocked")


def lifecycle_snapshot(argv):
    parser = SanitizedParser(description="Read-only, explicitly pinned current lifecycle facts; never acceptance.")
    for name in ("kubeconfig", "context", "cluster-uid", "scope"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--state", choices=("ready", "suspended"), default="ready",
                        help="current state to observe (default: ready); never proves physical shutdown or release")
    names = ("arguments", "scope-pins", "local-credentials", "cluster-identity", "agent-and-namespace",
             "statefulset", "pod", "home-storage", "csi-attachment", "stable-observation")
    result = {
        "version": "v1", "mode": "lifecycle-snapshot", "status": "blocked",
        "qualifiesExecution": False, "liveAcceptance": "not-established", "snapshot": None,
        "checks": [{"check": name, "status": "unknown", "reason": "not-checked"} for name in names],
        "unverified": [{"check": name, "status": "unknown"} for name in (
            "collector-authenticity", "deployed-source-charts-and-other-artifacts", "effective-case-and-authorization",
            "generation-bound-workload-readiness", "owner-access", "provider-model-and-tool-witnesses",
            "home-content-and-read-write", "nonzero-spend-and-accounting", "peer-continuity",
            "source-termination-csi-release-and-writer-exclusion", "unselected-attachments-and-cleanup",
        )],
    }
    try:
        args = parser.parse_args(argv)
    except PreflightBlocked:
        result["error"] = "invalid-command-line; use lifecycle-snapshot --help"
        print(json.dumps(result, sort_keys=True))
        return 2
    if args.state == "suspended":
        result["state"] = "suspended"

    @contextmanager
    def step(name):
        check = result["checks"][names.index(name)]
        try:
            yield
        except PreflightBlocked as error:
            check.update(status="unknown" if error.status == "unknown" else "blocked", reason=error.reason)
            raise
        except KeyboardInterrupt:
            check.update(status="unknown", reason="cancelled")
            raise
        check.update(status="observed", reason="matched-scoped-check")

    def cancelled(_signum, _frame):
        raise KeyboardInterrupt

    previous = signal.signal(signal.SIGTERM, cancelled)
    code = 1
    try:
        with step("arguments"):
            require_preflight(os.name == "posix", "posix-subprocess-runner-required", "unknown")
            require_preflight(os.path.isabs(args.kubeconfig) and len(args.kubeconfig) <= 4096 and
                              "\0" not in args.kubeconfig and exact_text(args.context) and exact_text(args.cluster_uid),
                              "invalid-explicit-target")
            config_file = snapshot_regular_file(args.kubeconfig)
        with step("scope-pins"):
            scope, fingerprint = snapshot_scope(args.scope, args.state)
        kube = SnapshotKubernetes(args, scope)
        with step("local-credentials"):
            config = kube.credentials()
            auth = config["users"][0]["user"]
            require_preflight(not any(key in auth for key in ("client-key", "client-certificate")),
                              "credential-file-indirection-not-supported", "unknown")
        with step("cluster-identity"):
            kube.check_target()
        with step("agent-and-namespace"):
            agent, namespace = kube.get("agent"), kube.get("namespace")
            snapshot_owner(namespace, kube.pins["agent"], "cetirun.microsoft.com/v1alpha1", "Agent")
            status, spec = object_value(agent.get("status")), object_value(agent.get("spec"))
            require_preflight(status.get("namespace") == kube.namespace, "agent-namespace-mismatch")
            require_preflight(object_value(namespace.get("status")).get("phase") == "Active",
                              "namespace-not-active")
            require_preflight(object_value(spec.get("agent")).get("type") == scope["case"]["runtime"],
                              "agent-runtime-mismatch")
            if args.state == "suspended":
                require_preflight(spec.get("suspended") is True and status.get("phase") == "Suspended" and
                                  type(status.get("observedGeneration")) is int and
                                  status["observedGeneration"] == kube.pins["agent"]["generation"],
                                  "agent-not-current-suspended")
                # The completed suspension path stamps the top-level generation, not each condition.
                conditions = [object_value(c) for c in array_value(status.get("conditions", []))]
                for kind, reason in (("Ready", "InstanceSuspended"), ("PodReady", "Suspended"),
                                     ("IngressReady", "Suspended")):
                    selected = [c for c in conditions if c.get("type") == kind]
                    require_preflight(len(selected) == 1 and selected[0].get("status") == "False" and
                                      selected[0].get("reason") == reason, "agent-suspension-not-observed")
            else:
                require_preflight(spec.get("suspended", False) is False and status.get("phase") == "Ready" and
                                  type(status.get("observedGeneration")) is int and
                                  status["observedGeneration"] == kube.pins["agent"]["generation"], "agent-not-current-ready")
        with step("statefulset"):
            labels, image, revision = snapshot_workload(kube)
        with step("pod"):
            node = None
            if args.state == "suspended":
                kube.unique("pod")
            else:
                node = snapshot_pod(kube, labels, image, revision)
        with step("home-storage"):
            driver = snapshot_home(kube)
        if kube.pins["attachment"] is None:
            result["checks"][names.index("csi-attachment")]["reason"] = "attachment-not-pinned"
        else:
            try:
                with step("csi-attachment"):
                    snapshot_attachment(kube, node, driver)
            except PreflightBlocked as error:
                if args.state != "suspended" or not snapshot_attachment_unknown(error):
                    raise
                kube.attachment_error = error.reason
        with step("stable-observation"):
            require_preflight(snapshot_regular_file(args.kubeconfig) == config_file, "kubeconfig-file-drift")
            try:
                kube.stable(config)
            finally:
                if kube.attachment_error is not None:
                    result["checks"][names.index("csi-attachment")].update(status="unknown", reason=kube.attachment_error)
            require_preflight(snapshot_regular_file(args.kubeconfig) == config_file, "kubeconfig-file-drift")
        identities = {key: kube.pseudonym(key, kube.pins[key]["uid"]) for key in LIFECYCLE_RESOURCE_FIELDS}
        pod_uid = kube.pseudonym("pod", kube.pins["pod"]["uid"]) if kube.pins["pod"] is not None else None
        result["scope"] = {key: kube.pseudonym(key, value) for key, value in
                           (("cluster", args.cluster_uid), ("context", args.context), ("run", scope["run"]))}
        result["snapshot"] = {
            **dict.fromkeys(LIFECYCLE_SNAPSHOT_FIELDS), "artifactSet": fingerprint, "resources": identities,
            "workload": {"agent": identities["agent"], "pod": pod_uid,
                         "observedPod": pod_uid if args.state == "ready" else None,
                         "node": kube.pseudonym("node-name", node) if node is not None else None,
                         "generation": kube.pins["agent"]["generation"],
                         "observedGeneration": None, "ready": None},
        }
        result["reported"] = {"agentObservedGeneration": status["observedGeneration"], "podReady": True}
        if args.state == "suspended":
            result["reported"].update(agentSuspended=True, agentPhase="Suspended", podReady=None,
                                      desiredReplicas=0, currentReplicas=0, podCandidates=0, runtimeImageID=None)
        result["storage"] = {"pvcBound": True, "pvBound": True, "csiReleased": None,
                             "attachment": None, "csiAttached": None}
        if kube.pins["attachment"] is not None and kube.attachment_error is None:
            result["storage"].update(attachment=kube.pseudonym("attachment", kube.pins["attachment"]["uid"]),
                                     csiAttached=True)
            result["status"] = "observed"
            code = 0
    except PreflightBlocked:
        pass
    except KeyboardInterrupt:
        code = 130
    finally:
        signal.signal(signal.SIGTERM, previous)
    print(json.dumps(result, sort_keys=True))
    return code


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    if argv and argv[0] == "acceptance-check":
        return lifecycle_acceptance(argv[1:])
    if argv and argv[0] == "lifecycle-snapshot":
        return lifecycle_snapshot(argv[1:])

    parser = SanitizedParser(description=__doc__)
    parser.add_argument("mode", choices=("preflight", "network-smoke", "deployment-preflight"))
    parser.add_argument("--kubeconfig", required=True)
    parser.add_argument("--context", required=True)
    parser.add_argument("--cluster-uid", required=True, help="Expected kube-system namespace UID")
    parser.add_argument("--runtime-class")
    parser.add_argument("--runtime-handler")
    parser.add_argument("--storage-class")
    parser.add_argument("--probe-image")
    parser.add_argument("--network-protocol", choices=("TCP", "UDP"),
                        help="network-smoke transport (default TCP); not valid for read-only modes")
    parser.add_argument("--allow-test-resources", action="store_true")
    deployment = parser.add_argument_group("deployment-preflight (explicit workload pins required)")
    deployment.add_argument("--allow-aks-system-exclusions", action="store_true",
                            help="also accept the exact two AKS-managed system-namespace exclusions; no other selector changes")
    deployment.add_argument("--namespace")
    for role in ("operator", "admission"):
        for field in ("deployment", "uid", "image", "service-account", "service-account-uid"):
            deployment.add_argument(f"--{role}-{field}")
    try:
        args = parser.parse_args(argv)
    except PreflightBlocked:
        print(json.dumps({"version": "v1", "status": "blocked", "qualifiesExecution": False,
                          "error": "invalid-command-line; use --help"}))
        return 2
    if args.network_protocol is not None and args.mode != "network-smoke":
        print(json.dumps({"version": "v1", "mode": args.mode, "status": "blocked", "qualifiesExecution": False,
                          "error": "--network-protocol requires network-smoke"}))
        return 2
    if args.allow_aks_system_exclusions and args.mode != "deployment-preflight":
        print(json.dumps({"version": "v1", "mode": args.mode, "status": "blocked", "qualifiesExecution": False,
                          "error": "--allow-aks-system-exclusions requires deployment-preflight"}))
        return 2
    if args.mode == "deployment-preflight":
        return deployment_preflight(args)
    args.runtime_class = "kata-vm-isolation" if args.runtime_class is None else args.runtime_class
    args.runtime_handler = "kata" if args.runtime_handler is None else args.runtime_handler
    result = {"version": "v1", "mode": args.mode, "status": "blocked", "qualifiesExecution": False}
    try:
        if not args.storage_class:
            raise QualificationError("An explicit storage class is required")
        if args.mode == "network-smoke" and (not args.allow_test_resources or not args.probe_image):
            raise QualificationError("Network smoke requires explicit test-resource permission and a pinned probe image")
        kube = Kubernetes(args.kubeconfig, args.context, args.cluster_uid)
        result["preflight"] = preflight(kube, args.runtime_class, args.runtime_handler, args.storage_class)
        if args.mode == "network-smoke":
            network_smoke(kube, args.probe_image, args.runtime_class, result, args.network_protocol or "TCP")
        result["status"] = "completed"
    except (QualificationError, ValueError) as error:
        result["error"] = str(error)
        print(json.dumps(result, sort_keys=True))
        return 1
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
