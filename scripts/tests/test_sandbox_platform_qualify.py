# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline tests; no Kubernetes or Azure calls."""

import contextlib
import copy
import errno
import http.client
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import socket
import subprocess
import threading
import time
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "sandbox-platform-qualify.py"
SPEC = importlib.util.spec_from_file_location("sandbox_platform_qualify", SCRIPT)
qualify = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(qualify)
IMAGE = "registry.example.com/probe@sha256:" + "a" * 64


class FakeKubernetes:
    def __init__(self):
        self.created = []
        self.deleted = []
        self.gets = []
        self.target_checks = 0
        self.namespace = None
        self.version = "v1.37.0"
        self.handler = "kata"
        self.address = "192.0.2.10"
        self.responses = [[True, True, True], [False, True, False], [False, True, True]]
        self.probe_calls = 0
        self.crd_ready = True
        self.replace_namespace = False
        self.replace_probe = False
        self.fail_probe = False
        self.pod_uids = {"server": "created-server", "probe": "created-probe"}
        self.pod_specs = {}

    def check_target(self):
        self.target_checks += 1

    def get(self, resource, name, namespace=None, absent_ok=False):
        self.gets.append((resource, name, namespace))
        if resource == "namespace":
            if self.replace_namespace and self.namespace:
                return {"metadata": {**self.namespace["metadata"], "uid": "foreign-uid"}}
            return self.namespace
        if resource == "runtimeclass":
            return {"handler": self.handler}
        if resource == "storageclass":
            return {"provisioner": "csi.example.com"}
        if resource == "crd":
            return {"status": {"conditions": [{"type": "Established", "status": "True" if self.crd_ready else "False"}]}}
        if resource == "pod":
            return {"metadata": {"uid": "changed" if self.replace_probe else self.pod_uids[name]},
                    "spec": copy.deepcopy(self.pod_specs.get(name, {})),
                    "status": {"podIP": self.address, "phase": "Running", "conditions": [{"type": "Ready", "status": "True"}]}}
        raise AssertionError(resource)

    def create(self, value):
        self.created.append(value)
        if value["kind"] == "Namespace":
            self.namespace = {"metadata": {**value["metadata"], "uid": "namespace-uid", "resourceVersion": "1"}}
            return self.namespace
        if value["kind"] == "Pod":
            self.pod_specs[value["metadata"]["name"]] = copy.deepcopy(value["spec"])
            return {**value, "metadata": {**value["metadata"], "uid": self.pod_uids[value["metadata"]["name"]]}}
        return value

    def run(self, args, document=None, timeout=30):
        if args[:2] == ["get", "--raw"]:
            return json.dumps({"gitVersion": self.version})
        if args[0] == "exec":
            self.probe_calls += 1
            if self.fail_probe:
                raise qualify.QualificationError("synthetic execution failure")
            return json.dumps(self.responses.pop(0))
        if args[0] == "delete":
            self.deleted.append((args, document))
            self.namespace = None
            return "{}"
        raise AssertionError(args)


class SandboxPlatformQualificationTests(unittest.TestCase):
    def test_every_command_has_explicit_target_and_deadline(self):
        client = qualify.Kubernetes("/example/test-config", "test-context", "expected-uid")
        with patch.object(qualify.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, '{"metadata":{"uid":"expected-uid"}}')) as run:
            client.check_target()
        args, kwargs = run.call_args
        self.assertEqual(args[0][:7], ["kubectl", "--kubeconfig", "/example/test-config", "--context", "test-context", "--request-timeout=15s", "get"])
        self.assertEqual(kwargs["timeout"], 30)
        self.assertFalse(kwargs["check"])
        self.assertEqual(kwargs["env"]["KUBECTL_KUBERC"], "false")

    def test_create_uses_fixed_collection_and_strict_server_validation(self):
        client = qualify.Kubernetes("/example/test-config", "test-context", "expected")
        cases = [
            ("v1", "Namespace", "/api/v1/namespaces"),
            ("v1", "Pod", "/api/v1/namespaces/probe/pods"),
            ("v1", "PersistentVolumeClaim", "/api/v1/namespaces/probe/persistentvolumeclaims"),
            ("networking.k8s.io/v1", "NetworkPolicy", "/apis/networking.k8s.io/v1/namespaces/probe/networkpolicies"),
            ("cilium.io/v2", "CiliumNetworkPolicy", "/apis/cilium.io/v2/namespaces/probe/ciliumnetworkpolicies"),
        ]
        for api, kind, path in cases:
            with self.subTest(kind=kind):
                document = {"apiVersion": api, "kind": kind, "metadata": {"name": "probe", "namespace": "probe"}}
                response = {**document, "metadata": {**document["metadata"], "uid": "created-uid"}}
                with patch.object(client, "run", return_value=json.dumps(response)) as run:
                    self.assertEqual(client.create(document), response)
                run.assert_called_once_with(["create", "--raw", path + "?fieldValidation=Strict", "-f", "-"], document)

    def test_create_rejects_unknown_resources_and_namespace_path_injection(self):
        client = qualify.Kubernetes("/example/test-config", "test-context", "expected")
        documents = [None, [], {"apiVersion": "v1", "kind": "Secret"},
                     {"apiVersion": "https://example.invalid", "kind": "Namespace"},
                     {"apiVersion": [], "kind": "Pod"}]
        for kind in ("Pod", "PersistentVolumeClaim"):
            for namespace in (None, "", "../other", "probe?dryRun=All", "probe/other", "with.dot", [], "x" * 64):
                documents.append({"apiVersion": "v1", "kind": kind, "metadata": {"namespace": namespace}})
        for document in documents:
            with self.subTest(document=document), patch.object(client, "run") as run:
                with self.assertRaises(qualify.QualificationError):
                    client.create(document)
                run.assert_not_called()

    def test_create_does_not_accept_invalid_server_receipts(self):
        client = qualify.Kubernetes("/example/test-config", "test-context", "expected")
        document = {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "probe"}}
        for response in ("not-json", "[]"):
            with self.subTest(response=response), patch.object(client, "run", return_value=response):
                with self.assertRaises(qualify.QualificationError):
                    client.create(document)

    def test_wrong_cluster_denies_before_other_reads(self):
        client = qualify.Kubernetes("/example/test-config", "test-context", "expected-uid")
        with patch.object(client, "get", return_value={"metadata": {"uid": "different"}}):
            with self.assertRaisesRegex(qualify.QualificationError, "Cluster UID mismatch"):
                qualify.preflight(client, "kata-vm-isolation", "kata", "home")

    def test_upstream_credentials_and_endpoints_not_echoed(self):
        client = qualify.Kubernetes("/example/test-config", "test-context", "expected")
        with patch.object(qualify.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "sensitive-upstream-value")):
            with self.assertRaises(qualify.QualificationError) as raised:
                client.get("namespace", "kube-system")
        self.assertNotIn("sensitive-upstream-value", str(raised.exception))

    def test_timeout_is_explicit_failure(self):
        client = qualify.Kubernetes("/example/test-config", "test-context", "expected")
        with patch.object(qualify.subprocess, "run", side_effect=subprocess.TimeoutExpired([], 1)):
            with self.assertRaisesRegex(qualify.QualificationError, "timed out"):
                client.get("namespace", "kube-system")

    def test_preflight_is_read_only(self):
        kube = FakeKubernetes()
        result = qualify.preflight(kube, "kata-vm-isolation", "kata", "home")
        self.assertTrue(result["ciliumAPIsEstablished"])
        self.assertEqual(kube.created, [])
        self.assertEqual(kube.deleted, [])

    def test_preflight_rejects_incompatible_prerequisites(self):
        for field, value in (("version", "v1.36.9"), ("version", "unknown"), ("handler", "runc"), ("crd_ready", False)):
            kube = FakeKubernetes()
            setattr(kube, field, value)
            with self.subTest(field=field):
                with self.assertRaises(qualify.QualificationError):
                    qualify.preflight(kube, "kata-vm-isolation", "kata", "home")

    def test_pods_are_synthetic_without_credentials_or_host_access(self):
        for role in ("server", "probe"):
            value = qualify.pod("test-namespace", "run", role, IMAGE, "kata-vm-isolation")
            self.assertFalse(value["spec"]["automountServiceAccountToken"])
            self.assertEqual(value["spec"]["restartPolicy"], "Never")
            self.assertNotIn("volumes", value["spec"])
            self.assertNotIn("hostNetwork", value["spec"])
            if role == "probe":
                self.assertEqual(value["spec"]["runtimeClassName"], "kata-vm-isolation")

    def test_server_readiness_checks_every_listener_before_baseline(self):
        server = qualify.pod("test-namespace", "run", "server", IMAGE, "kata-vm-isolation")
        container = server["spec"]["containers"][0]
        self.assertEqual(container["env"], [{
            "name": "POD_IP", "valueFrom": {"fieldRef": {"fieldPath": "status.podIP"}},
        }])
        self.assertEqual(container["readinessProbe"], {
            "exec": {"command": ["python3", "-c", qualify.LISTENER_READINESS]},
            "periodSeconds": 1, "timeoutSeconds": 5, "failureThreshold": 3,
        })
        probe = qualify.pod("test-namespace", "run", "probe", IMAGE, "kata-vm-isolation")
        self.assertNotIn("readinessProbe", probe["spec"]["containers"][0])

        kube = FakeKubernetes()
        original_get, original_run = kube.get, kube.run
        server_reads = 0

        def get(resource, name, namespace=None, absent_ok=False):
            nonlocal server_reads
            value = original_get(resource, name, namespace, absent_ok)
            if resource == "pod" and name == "server":
                server_reads += 1
                if server_reads < 3:
                    value["status"]["conditions"][0]["status"] = "False"
            return value

        def run(args, document=None, timeout=30):
            if args[0] == "exec":
                self.assertGreaterEqual(server_reads, 3, "baseline started before all listeners were ready")
            return original_run(args, document, timeout)

        with patch.object(kube, "get", side_effect=get), patch.object(kube, "run", side_effect=run), \
             patch.object(qualify.time, "sleep") as sleep:
            qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", {})
        self.assertEqual(sleep.call_count, 2)

    def test_listener_readiness_timeout_is_bounded(self):
        kube = FakeKubernetes()
        pending = {"metadata": {"uid": "created-server"},
                   "status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "False"}]}}
        with patch.object(kube, "get", return_value=pending), \
             patch.object(qualify.time, "monotonic", side_effect=[0, 0, 181]), \
             patch.object(qualify.time, "sleep"):
            with self.assertRaisesRegex(qualify.QualificationError, "readiness timed out"):
                qualify.wait_ready(kube, "test-namespace", "server", "created-server")
        self.assertEqual(kube.probe_calls, 0)

    def test_listener_readiness_rejects_each_missing_port(self):
        for missing in (8080, 8081, 8082):
            calls = []

            def connect(address, timeout):
                self.assertEqual(timeout, 1)
                calls.append(address)
                if address[1] == missing:
                    raise ConnectionRefusedError
                return contextlib.nullcontext()

            with self.subTest(port=missing), patch.dict(os.environ, {"POD_IP": "192.0.2.1"}), \
                 patch.object(socket, "create_connection", side_effect=connect):
                with self.assertRaises(ConnectionRefusedError):
                    exec(compile(qualify.LISTENER_READINESS, "<listener-readiness>", "exec"), {})
            self.assertEqual(calls, [("192.0.2.1", port) for port in range(8080, missing + 1)])

    def check_loopback_listeners(self, address, family):
        servers, threads = [], []
        base_server, base_thread = http.server.HTTPServer, threading.Thread
        connect = socket.create_connection

        class LocalServer(base_server):
            def __init__(self, requested, handler):
                self.requested = requested
                # Exercise the actual embedded server with isolated ephemeral ports.
                super().__init__((requested[0], 0), handler)
                servers.append(self)

        def thread(*args, **kwargs):
            value = base_thread(*args, **kwargs)
            threads.append(value)
            return value

        try:
            with patch.dict(os.environ, {"POD_IP": address}), \
                 patch.object(http.server, "HTTPServer", LocalServer), \
                 patch.object(threading, "Thread", side_effect=thread), patch.object(time, "sleep"):
                exec(compile(qualify.SERVER, "<synthetic-server>", "exec"), {})
            self.assertEqual([s.requested for s in servers], [(address, port) for port in (8080, 8081, 8082)])
            self.assertTrue(all(s.address_family == family for s in servers))
            ports = {s.requested[1]: s.server_port for s in servers}

            def mapped_connect(endpoint, timeout, *args, **kwargs):
                self.assertEqual(endpoint[0], address)
                self.assertIn(timeout, (1, 2))
                return connect((address, ports[endpoint[1]]), timeout, *args, **kwargs)

            with patch.dict(os.environ, {"POD_IP": address}), \
                 patch.object(socket, "create_connection", side_effect=mapped_connect), \
                 patch.object(qualify.sys, "argv", ["probe", address]):
                exec(compile(qualify.LISTENER_READINESS, "<listener-readiness>", "exec"), {})
                with contextlib.redirect_stdout(io.StringIO()) as output:
                    exec(compile(qualify.PROBE, "<synthetic-probe>", "exec"), {})
                self.assertEqual(json.loads(output.getvalue()), [True, True, True])
                servers[-1].shutdown()
                servers[-1].server_close()
                with self.assertRaises(ConnectionRefusedError):
                    exec(compile(qualify.LISTENER_READINESS, "<listener-readiness>", "exec"), {})
                with self.assertRaises(ConnectionRefusedError):
                    exec(compile(qualify.PROBE, "<synthetic-probe>", "exec"), {})
        finally:
            for server in servers:
                server.shutdown()
                server.server_close()
            for value in threads:
                value.join(timeout=2)
                self.assertFalse(value.is_alive())

    def test_real_ipv4_listeners_readiness_and_strict_probe(self):
        self.check_loopback_listeners("127.0.0.1", socket.AF_INET)

    def test_real_ipv6_listeners_readiness_and_strict_probe(self):
        try:
            with socket.socket(socket.AF_INET6, socket.SOCK_STREAM) as endpoint:
                endpoint.bind(("::1", 0))
        except OSError as error:
            if error.errno in (errno.EAFNOSUPPORT, errno.EADDRNOTAVAIL):
                self.skipTest("IPv6 loopback is unavailable on this host")
            raise
        self.check_loopback_listeners("::1", socket.AF_INET6)

    def test_smoke_checks_allow_deny_and_cleans_exact_uid(self):
        kube = FakeKubernetes()
        result = {}
        qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
        self.assertTrue(result["cleanupComplete"])
        self.assertTrue(all(result["networkChecks"].values()))
        self.assertTrue(result["networkChecks"]["unchangedServerIdentity"])
        self.assertTrue(result["networkChecks"]["unchangedProbeIdentity"])
        self.assertEqual([obj["kind"] for obj in kube.created],
                         ["Namespace", "Pod", "Pod", "CiliumNetworkPolicy", "NetworkPolicy"])
        _, options = kube.deleted[0]
        self.assertEqual(options["preconditions"], {"uid": "namespace-uid", "resourceVersion": "1"})
        self.assertEqual(kube.target_checks, 2)

    def test_created_pods_cannot_be_replaced_before_readiness(self):
        for role in ("server", "probe"):
            with self.subTest(role=role):
                kube = FakeKubernetes()
                original_get = kube.get

                def get(resource, name, namespace=None, absent_ok=False):
                    value = original_get(resource, name, namespace, absent_ok)
                    if resource == "pod" and name == role:
                        value["metadata"]["uid"] = "replacement-" + role
                        value["spec"] = {"runtimeClassName": "runc"}
                    return value

                result = {}
                with patch.object(kube, "get", side_effect=get):
                    with self.assertRaisesRegex(qualify.QualificationError, "Pod identity"):
                        qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
                self.assertEqual(kube.probe_calls, 0)
                self.assertNotIn("networkChecks", result)
                self.assertTrue(result["cleanupComplete"])

    def test_created_pods_cannot_be_replaced_during_readiness(self):
        for role in ("server", "probe"):
            with self.subTest(role=role):
                kube = FakeKubernetes()
                original_get = kube.get
                reads = 0

                def get(resource, name, namespace=None, absent_ok=False):
                    nonlocal reads
                    value = original_get(resource, name, namespace, absent_ok)
                    if resource == "pod" and name == role:
                        reads += 1
                        if reads == 1:
                            value["status"]["conditions"][0]["status"] = "False"
                        else:
                            value["metadata"]["uid"] = "replacement-" + role
                    return value

                result = {}
                with patch.object(kube, "get", side_effect=get), patch.object(qualify.time, "sleep"):
                    with self.assertRaisesRegex(qualify.QualificationError, "Pod identity"):
                        qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
                self.assertEqual(reads, 2)
                self.assertEqual(kube.probe_calls, 0)
                self.assertTrue(result["cleanupComplete"])

    def test_final_checks_pin_both_created_pods(self):
        for role in ("server", "probe"):
            with self.subTest(role=role):
                kube = FakeKubernetes()
                original_get = kube.get

                def get(resource, name, namespace=None, absent_ok=False):
                    value = original_get(resource, name, namespace, absent_ok)
                    if resource == "pod" and name == role and kube.probe_calls == 3:
                        value["metadata"]["uid"] = "replacement-" + role
                    return value

                result = {}
                with patch.object(kube, "get", side_effect=get):
                    with self.assertRaisesRegex(qualify.QualificationError, "Pod identity"):
                        qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
                self.assertNotIn("unchangedProbeIdentity", result["networkChecks"])
                self.assertNotIn("unchangedServerIdentity", result["networkChecks"])
                self.assertTrue(result["cleanupComplete"])

    def test_missing_creation_uid_never_becomes_readiness_authority(self):
        for role in ("server", "probe"):
            for invalid in (None, "", " ", 42):
                with self.subTest(role=role, uid=invalid):
                    kube = FakeKubernetes()
                    original_create = kube.create

                    def create(value):
                        created = original_create(value)
                        if value["kind"] == "Pod" and value["metadata"]["name"] == role:
                            created["metadata"]["uid"] = invalid
                        return created

                    result = {}
                    with patch.object(kube, "create", side_effect=create):
                        with self.assertRaisesRegex(qualify.QualificationError, "creation identity"):
                            qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
                    self.assertEqual(kube.probe_calls, 0)
                    self.assertTrue(result["cleanupComplete"])

    def test_terminating_created_pod_cannot_satisfy_identity_checks(self):
        value = {"metadata": {"uid": "created-probe", "deletionTimestamp": "2026-01-01T00:00:00Z"}}
        with self.assertRaisesRegex(qualify.QualificationError, "terminating"):
            qualify.verify_pod_identity(value, "created-probe")

    def test_same_uid_runtime_mutation_cannot_complete_smoke(self):
        for phase in ("create", "readiness", "final"):
            for runtime_class in ("runc", "", None):
                with self.subTest(phase=phase, runtime_class=runtime_class):
                    kube = FakeKubernetes()
                    original_create, original_get = kube.create, kube.get

                    def mutate(value):
                        self.assertEqual(value["metadata"]["uid"], "created-probe")
                        if runtime_class is None:
                            value["spec"].pop("runtimeClassName", None)
                        else:
                            value["spec"]["runtimeClassName"] = runtime_class
                        return value

                    def create(value):
                        created = original_create(value)
                        if phase == "create" and value["kind"] == "Pod" and value["metadata"]["name"] == "probe":
                            return mutate(created)
                        return created

                    def get(resource, name, namespace=None, absent_ok=False):
                        value = original_get(resource, name, namespace, absent_ok)
                        if resource == "pod" and name == "probe" and (
                            phase == "readiness" or (phase == "final" and kube.probe_calls == 3)
                        ):
                            return mutate(value)
                        return value

                    result = {}
                    with patch.object(kube, "create", side_effect=create), patch.object(kube, "get", side_effect=get):
                        with self.assertRaisesRegex(qualify.QualificationError, "requested RuntimeClass"):
                            qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
                    self.assertEqual(kube.probe_calls, 3 if phase == "final" else 0)
                    self.assertNotIn("unchangedProbeIdentity", result.get("networkChecks", {}))
                    self.assertTrue(result["cleanupComplete"])
                    if phase == "create":
                        self.assertFalse(any(resource == "pod" for resource, _, _ in kube.gets))

    def test_runtime_checks_use_the_requested_class_not_a_hardcoded_default(self):
        kube = FakeKubernetes()
        result = {}
        qualify.network_smoke(kube, IMAGE, "qualified-kata", result)
        self.assertEqual(kube.pod_specs["probe"]["runtimeClassName"], "qualified-kata")
        self.assertTrue(result["networkChecks"]["unchangedProbeIdentity"])

    def test_precedence_waits_for_allow_policy_witness(self):
        kube = FakeKubernetes()
        kube.responses = [
            [True, True, True], [False, True, False],
            [False, True, False], [False, True, False], [False, True, True],
        ]
        result = {}
        with patch.object(qualify.time, "sleep"):
            qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
        self.assertEqual(kube.probe_calls, 5)
        self.assertTrue(result["networkChecks"]["overlappingAllowObserved"])
        self.assertTrue(result["networkChecks"]["denyOverOverlappingAllow"])

    def test_precedence_without_applied_allow_does_not_pass(self):
        kube = FakeKubernetes()
        kube.responses = [[False, True, False]]
        with patch.object(qualify.time, "monotonic", side_effect=[0, 0, 46]), patch.object(qualify.time, "sleep"):
            with self.assertRaisesRegex(qualify.QualificationError, "did not match"):
                qualify.wait_connectivity(kube, "ns", "192.0.2.1", [False, True, True])

    def test_allow_transition_within_probe_cannot_reuse_old_deny(self):
        state = {"allowApplied": False}
        calls = []

        class Connection:
            status = 200

            def __init__(self, address, port, **kwargs):
                self.port = port

            def connect(self):
                calls.append(self.port)
                if not state["allowApplied"] and self.port != 8081:
                    # Simulate an incorrectly permissive allow becoming active
                    # immediately after the first old-policy connection timeout.
                    state["allowApplied"] = True
                    raise TimeoutError

            def request(self, *args):
                pass

            def getresponse(self):
                return self

            def read(self):
                return b"qualification-ok"

            def close(self):
                pass

        results = []
        with patch.object(http.client, "HTTPConnection", Connection), \
             patch.object(qualify.sys, "argv", ["probe", "192.0.2.1"]):
            for _ in range(2):
                with contextlib.redirect_stdout(io.StringIO()) as output:
                    exec(compile(qualify.PROBE, "<synthetic-probe>", "exec"), {})
                results.append(json.loads(output.getvalue()))
        self.assertEqual(calls, [8082, 8080, 8081] * 2)
        self.assertEqual(results, [[True, True, False], [True, True, True]])
        self.assertNotIn([False, True, True], results)

    def test_probe_rejects_http_errors_after_successful_tcp_connect(self):
        class Connection:
            def __init__(self, *args, **kwargs):
                pass

            def connect(self):
                pass

            def request(self, *args):
                pass

            def getresponse(self):
                return self

            def read(self):
                return b"qualification-ok"

            def close(self):
                pass

        for response_status, body, failure in (
            (503, b"qualification-ok", None),
            (200, b"unexpected", None),
            (200, b"qualification-ok", http.client.BadStatusLine("synthetic")),
            (200, b"qualification-ok", TimeoutError("after connect")),
        ):
            with self.subTest(status=response_status, failure=type(failure).__name__):
                with patch.object(http.client, "HTTPConnection", Connection), \
                     patch.object(Connection, "status", response_status, create=True), \
                     patch.object(Connection, "read", return_value=body), \
                     patch.object(Connection, "getresponse", side_effect=failure, return_value=Connection()), \
                     patch.object(qualify.sys, "argv", ["probe", "192.0.2.1"]), \
                     contextlib.redirect_stdout(io.StringIO()) as output:
                    with self.assertRaises((SystemExit, http.client.HTTPException, TimeoutError)):
                        exec(compile(qualify.PROBE, "<synthetic-probe>", "exec"), {})
                self.assertEqual(output.getvalue(), "")

    def test_probe_counts_only_connection_timeout_as_blocked(self):
        class Connection:
            def __init__(self, *args, **kwargs):
                pass

            def connect(self):
                raise TimeoutError

            def close(self):
                pass

        with patch.object(http.client, "HTTPConnection", Connection), \
             patch.object(qualify.sys, "argv", ["probe", "192.0.2.1"]), \
             contextlib.redirect_stdout(io.StringIO()) as output:
            exec(compile(qualify.PROBE, "<synthetic-probe>", "exec"), {})
        self.assertEqual(json.loads(output.getvalue()), [False, False, False])
        with patch.object(http.client, "HTTPConnection", Connection), \
             patch.object(Connection, "connect", side_effect=ConnectionRefusedError), \
             patch.object(qualify.sys, "argv", ["probe", "192.0.2.1"]):
            with self.assertRaises(ConnectionRefusedError):
                exec(compile(qualify.PROBE, "<synthetic-probe>", "exec"), {})

    def test_probe_execution_error_is_not_network_denial(self):
        kube = FakeKubernetes()
        kube.fail_probe = True
        result = {}
        with self.assertRaisesRegex(qualify.QualificationError, "execution failure"):
            qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
        self.assertTrue(result["cleanupComplete"])
        self.assertNotIn("networkChecks", result)

    def test_malformed_probe_results_fail(self):
        kube = FakeKubernetes()
        for value in ([True], [0, 1], {"allowed": True}):
            kube.responses = [value]
            with self.assertRaises(qualify.QualificationError):
                qualify.connectivity(kube, "ns", "192.0.2.1")

    def test_foreign_namespace_is_never_deleted(self):
        kube = FakeKubernetes()
        kube.create({"kind": "Namespace", "metadata": {"name": "test", "labels": {qualify.RUN_LABEL: "run"}}})
        kube.replace_namespace = True
        with self.assertRaisesRegex(qualify.QualificationError, "ownership"):
            qualify.cleanup(kube, "test", "namespace-uid", "run")
        self.assertEqual(kube.deleted, [])

    def test_both_probe_and_cleanup_errors_remain_visible(self):
        kube = FakeKubernetes()
        kube.fail_probe = True
        kube.replace_namespace = True
        result = {}
        with self.assertRaisesRegex(qualify.QualificationError, "execution failure; cleanup failed: Cleanup ownership"):
            qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
        self.assertFalse(result["cleanupComplete"])
        self.assertEqual(kube.deleted, [])

    def test_mutable_probe_image_is_rejected_without_writes(self):
        kube = FakeKubernetes()
        with self.assertRaisesRegex(qualify.QualificationError, "digest-pinned"):
            qualify.network_smoke(kube, "registry.example.com/probe:latest", "kata-vm-isolation", {})
        self.assertEqual(kube.created, [])

    def test_cli_requires_mutation_opt_in_and_never_qualifies_execution(self):
        args = ["network-smoke", "--kubeconfig", "/example/test-config", "--context", "explicit",
                "--cluster-uid", "expected", "--storage-class", "home", "--probe-image", IMAGE]
        with patch.object(qualify, "Kubernetes") as client, contextlib.redirect_stdout(io.StringIO()) as output:
            code = qualify.main(args)
        self.assertEqual(code, 1)
        self.assertFalse(client.called)
        result = json.loads(output.getvalue())
        self.assertFalse(result["qualifiesExecution"])
        self.assertEqual(result["status"], "blocked")

    def test_successful_preflight_is_not_execution_evidence(self):
        args = ["preflight", "--kubeconfig", "/example/test-config", "--context", "explicit",
                "--cluster-uid", "expected", "--storage-class", "home"]
        with patch.object(qualify, "Kubernetes", return_value=FakeKubernetes()), contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(qualify.main(args), 0)
        result = json.loads(output.getvalue())
        self.assertEqual(result["status"], "completed")
        self.assertIs(result["qualifiesExecution"], False)


if __name__ == "__main__":
    unittest.main()
