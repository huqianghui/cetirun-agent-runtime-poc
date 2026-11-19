# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline UDP probe tests; no cluster, credentials or external endpoints."""

import contextlib
import errno
import io
import json
import os
import socket
import socketserver
import threading
import time
import unittest
from unittest.mock import patch

from test_sandbox_platform_qualify import FakeKubernetes, IMAGE, qualify


class EchoSocket:
    def __init__(self, family, kind):
        self.family, self.kind = family, kind
        self.payload = None

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False

    def settimeout(self, timeout):
        self.timeout = timeout

    def connect(self, endpoint):
        self.endpoint = endpoint

    def send(self, payload):
        self.payload = payload
        return len(payload)

    def recv(self, maximum):
        if maximum != len(self.payload) + 1:
            raise AssertionError("UDP response must be read with a bounded length")
        return self.payload


class SandboxUDPProbeTests(unittest.TestCase):
    def run_probe(self, address="192.0.2.10"):
        with patch.object(qualify.sys, "argv", ["probe", address]), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            exec(compile(qualify.UDP_PROBE, "<synthetic-udp-probe>", "exec"), {})
        return json.loads(output.getvalue())

    def test_udp_pods_keep_existing_isolation_and_select_matching_programs(self):
        server = qualify.pod("synthetic", "run", "server", IMAGE, "approved-kata", "UDP")
        container = server["spec"]["containers"][0]
        self.assertEqual(container["command"], ["python3", "-c", qualify.UDP_SERVER])
        self.assertEqual(container["readinessProbe"]["exec"]["command"],
                         ["python3", "-c", qualify.UDP_LISTENER_READINESS])
        self.assertEqual(container["readinessProbe"]["timeoutSeconds"], 5)
        for role in ("server", "probe"):
            value = qualify.pod("synthetic", "run", role, IMAGE, "approved-kata", "UDP")
            self.assertFalse(value["spec"]["automountServiceAccountToken"])
            self.assertEqual(value["spec"]["restartPolicy"], "Never")
            self.assertNotIn("volumes", value["spec"])
            self.assertNotIn("hostNetwork", value["spec"])
            self.assertTrue(value["spec"]["containers"][0]["securityContext"]["readOnlyRootFilesystem"])
        probe = qualify.pod("synthetic", "run", "probe", IMAGE, "approved-kata", "UDP")
        self.assertEqual(probe["spec"]["runtimeClassName"], "approved-kata")
        self.assertNotIn("readinessProbe", probe["spec"]["containers"][0])

    def test_tcp_programs_and_default_policy_are_unchanged(self):
        self.assertEqual(qualify.network_scripts("TCP"),
                         (qualify.SERVER, qualify.LISTENER_READINESS, qualify.PROBE))
        kube, result = FakeKubernetes(), {}
        qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result)
        self.assertEqual(result["networkProtocol"], "TCP")
        policy = next(item for item in kube.created if item["kind"] == "CiliumNetworkPolicy")
        for direction in ("egress", "egressDeny"):
            self.assertEqual(policy["spec"][direction][0]["toPorts"][0]["ports"][0]["protocol"], "TCP")

    def test_udp_policy_and_probe_use_the_same_protocol(self):
        kube, result = FakeKubernetes(), {}
        original = kube.run
        commands = []

        def run(args, *values, **kwargs):
            if args[0] == "exec":
                commands.append(args)
            return original(args, *values, **kwargs)

        with patch.object(kube, "run", side_effect=run):
            qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result, "UDP")
        self.assertEqual(result["networkProtocol"], "UDP")
        self.assertTrue(all(result["networkChecks"].values()))
        self.assertTrue(result["cleanupComplete"])
        self.assertEqual(len(commands), 3)
        self.assertTrue(all(command[-2] == qualify.UDP_PROBE for command in commands))
        policy = next(item for item in kube.created if item["kind"] == "CiliumNetworkPolicy")
        self.assertEqual(policy["spec"]["egress"][0]["toPorts"][0]["ports"],
                         [{"port": "8081", "protocol": "UDP"}])
        self.assertEqual(policy["spec"]["egressDeny"][0]["toPorts"][0]["ports"],
                         [{"port": "8080", "protocol": "UDP"}])
        self.assertEqual(kube.deleted[0][1]["preconditions"],
                         {"uid": "namespace-uid", "resourceVersion": "1"})

    def test_udp_errors_still_trigger_owned_cleanup(self):
        kube, result = FakeKubernetes(), {}
        kube.fail_probe = True
        with self.assertRaisesRegex(qualify.QualificationError, "execution failure"):
            qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result, "UDP")
        self.assertTrue(result["cleanupComplete"])
        self.assertNotIn("networkChecks", result)

    def test_udp_waits_for_overlapping_allow_witness(self):
        kube, result = FakeKubernetes(), {}
        kube.responses = [[True, True, True], [False, True, False],
                          [False, True, False], [False, True, True]]
        with patch.object(qualify.time, "sleep"):
            qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", result, "UDP")
        self.assertEqual(kube.probe_calls, 4)
        self.assertTrue(result["networkChecks"]["overlappingAllowObserved"])
        self.assertTrue(result["networkChecks"]["denyOverOverlappingAllow"])

    def test_probe_uses_connected_bounded_sockets_and_fresh_payloads(self):
        for address, family in (("192.0.2.10", socket.AF_INET), ("2001:db8::10", socket.AF_INET6)):
            sockets = []

            def make_socket(*args):
                endpoint = EchoSocket(*args)
                sockets.append(endpoint)
                return endpoint

            with self.subTest(address=address), patch.object(socket, "socket", side_effect=make_socket):
                self.assertEqual(self.run_probe(address), [True, True, True])
            self.assertEqual([value.endpoint for value in sockets],
                             [(address, port) for port in (8082, 8080, 8081)])
            self.assertTrue(all(value.family == family and value.kind == socket.SOCK_DGRAM for value in sockets))
            self.assertTrue(all(value.timeout == 2 for value in sockets))
            self.assertEqual(len({value.payload for value in sockets}), 3)
            self.assertTrue(all(value.payload.startswith(b"qualification-") and len(value.payload) == 30
                                for value in sockets))

    def test_only_receive_timeout_counts_as_no_response(self):
        with patch.object(socket, "socket", EchoSocket), \
                patch.object(EchoSocket, "recv", side_effect=TimeoutError):
            self.assertEqual(self.run_probe(), [False, False, False])
        for method, error in (
            ("connect", TimeoutError), ("send", TimeoutError),
            ("connect", OSError), ("recv", ConnectionRefusedError),
        ):
            with self.subTest(method=method, error=error), \
                    patch.object(socket, "socket", EchoSocket), \
                    patch.object(EchoSocket, method, side_effect=error), \
                    contextlib.redirect_stdout(io.StringIO()) as output:
                with self.assertRaises(error):
                    self.run_probe()
                self.assertEqual(output.getvalue(), "")

    def test_wrong_empty_oversized_or_old_payload_fails_not_denies(self):
        for response in (b"", b"wrong", b"x" * 31, b"qualification-" + b"a" * 16):
            with self.subTest(length=len(response)), patch.object(socket, "socket", EchoSocket), \
                    patch.object(EchoSocket, "recv", return_value=response), \
                    patch("secrets.token_bytes", return_value=b"b" * 16):
                with self.assertRaisesRegex(SystemExit, "unexpected synthetic datagram"):
                    self.run_probe()
        with patch.object(socket, "socket", EchoSocket), patch.object(EchoSocket, "send", return_value=1):
            with self.assertRaisesRegex(SystemExit, "incomplete synthetic datagram"):
                self.run_probe()

    def test_readiness_requires_an_echo_from_every_port(self):
        endpoints = []

        class MissingListener(EchoSocket):
            def connect(self, endpoint):
                super().connect(endpoint)
                endpoints.append(endpoint)

            def recv(self, maximum):
                if self.endpoint[1] == missing:
                    raise TimeoutError
                return super().recv(maximum)

        for missing in (8080, 8081, 8082):
            endpoints.clear()
            with self.subTest(port=missing), patch.dict(os.environ, {"POD_IP": "192.0.2.10"}), \
                    patch.object(socket, "socket", MissingListener):
                with self.assertRaisesRegex(SystemExit, "listener not ready"):
                    exec(compile(qualify.UDP_LISTENER_READINESS, "<udp-readiness>", "exec"), {})
            self.assertEqual(endpoints, [("192.0.2.10", port) for port in range(8080, missing + 1)])

    def test_allow_transition_cannot_reuse_earlier_deny(self):
        state = {"allow": False}
        calls = []

        class TransitionSocket(EchoSocket):
            def recv(self, maximum):
                calls.append(self.endpoint[1])
                if not state["allow"] and self.endpoint[1] != 8081:
                    state["allow"] = True
                    raise TimeoutError
                return super().recv(maximum)

        with patch.object(socket, "socket", TransitionSocket):
            samples = [self.run_probe(), self.run_probe()]
        self.assertEqual(calls, [8082, 8080, 8081] * 2)
        self.assertEqual(samples, [[True, True, False], [True, True, True]])
        self.assertNotIn([False, True, True], samples)

    def check_loopback(self, address, family):
        servers, threads = [], []
        base_server, base_thread, base_socket = socketserver.UDPServer, threading.Thread, socket.socket

        class LocalServer(base_server):
            def __init__(self, requested, handler):
                self.requested = requested
                super().__init__((requested[0], 0), handler)
                servers.append(self)

        def thread(*args, **kwargs):
            value = base_thread(*args, **kwargs)
            threads.append(value)
            return value

        try:
            with patch.dict(os.environ, {"POD_IP": address}), \
                    patch.object(socketserver, "UDPServer", LocalServer), \
                    patch.object(threading, "Thread", side_effect=thread), patch.object(time, "sleep"):
                exec(compile(qualify.UDP_SERVER, "<synthetic-udp-server>", "exec"), {})
            self.assertEqual([value.requested for value in servers],
                             [(address, port) for port in (8080, 8081, 8082)])
            self.assertTrue(all(value.address_family == family for value in servers))
            ports = {value.requested[1]: value.server_address[1] for value in servers}

            class MappedSocket(base_socket):
                def connect(self, endpoint):
                    if endpoint[0] != address or endpoint[1] not in ports:
                        raise AssertionError("unexpected loopback target")
                    return super().connect((address, ports[endpoint[1]]))

            with patch.dict(os.environ, {"POD_IP": address}), patch.object(socket, "socket", MappedSocket):
                exec(compile(qualify.UDP_LISTENER_READINESS, "<udp-readiness>", "exec"), {})
                self.assertEqual(self.run_probe(address), [True, True, True])
        finally:
            for server in servers:
                server.shutdown()
                server.server_close()
            for value in threads:
                value.join(timeout=2)
                self.assertFalse(value.is_alive())

    def test_real_ipv4_udp_readiness_and_echo(self):
        self.check_loopback("127.0.0.1", socket.AF_INET)

    def test_real_ipv6_udp_readiness_and_echo(self):
        try:
            with socket.socket(socket.AF_INET6, socket.SOCK_DGRAM) as endpoint:
                endpoint.bind(("::1", 0))
        except OSError as error:
            if error.errno in (errno.EAFNOSUPPORT, errno.EADDRNOTAVAIL):
                self.skipTest("IPv6 loopback is unavailable")
            raise
        self.check_loopback("::1", socket.AF_INET6)

    def test_cli_protocol_selection_preserves_explicit_permission_and_nonqualification(self):
        args = ["network-smoke", "--kubeconfig", "/example/test-config", "--context", "explicit",
                "--cluster-uid", "expected", "--storage-class", "home", "--probe-image", IMAGE,
                "--network-protocol", "UDP"]
        with patch.object(qualify, "Kubernetes") as client, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(qualify.main(args), 1)
            self.assertFalse(client.called)
        with patch.object(qualify, "Kubernetes", return_value=FakeKubernetes()), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(qualify.main(args + ["--allow-test-resources"]), 0)
        result = json.loads(output.getvalue())
        self.assertEqual(result["networkProtocol"], "UDP")
        self.assertIs(result["qualifiesExecution"], False)
        self.assertTrue(result["cleanupComplete"])

    def test_protocol_flag_is_rejected_for_readonly_modes_before_client_setup(self):
        for mode in ("preflight", "deployment-preflight"):
            for protocol in ("TCP", "UDP"):
                args = [mode, "--kubeconfig", "/example/test-config", "--context", "explicit",
                        "--cluster-uid", "expected", "--network-protocol", protocol]
                with self.subTest(mode=mode, protocol=protocol), \
                        patch.object(qualify, "Kubernetes") as client, \
                        patch.object(qualify, "DeploymentKubernetes") as deployment, \
                        contextlib.redirect_stdout(io.StringIO()) as output:
                    self.assertEqual(qualify.main(args), 2)
                    self.assertFalse(client.called)
                    self.assertFalse(deployment.called)
                self.assertIs(json.loads(output.getvalue())["qualifiesExecution"], False)

    def test_unknown_protocol_is_not_an_implicit_tcp_fallback(self):
        for protocol in ("SCTP", "udp", ""):
            with self.subTest(protocol=protocol):
                kube = FakeKubernetes()
                with self.assertRaisesRegex(qualify.QualificationError, "TCP or UDP"):
                    qualify.network_smoke(kube, IMAGE, "kata-vm-isolation", {}, protocol)
                self.assertEqual(kube.created, [])
                with patch.object(qualify, "Kubernetes") as client, \
                        contextlib.redirect_stdout(io.StringIO()) as output:
                    code = qualify.main(["network-smoke", "--kubeconfig", "/example/test-config",
                                         "--context", "explicit", "--cluster-uid", "expected",
                                         "--network-protocol", protocol])
                self.assertEqual(code, 2)
                self.assertFalse(client.called)
                self.assertEqual(json.loads(output.getvalue())["status"], "blocked")


if __name__ == "__main__":
    unittest.main()
