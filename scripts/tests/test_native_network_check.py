# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Synthetic native-network receipts; no Kubernetes or cloud access."""

import copy
import contextlib
import http.server
import importlib.util
import ipaddress
import io
import json
import os
from pathlib import Path
import platform
import shlex
import shutil
import signal
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import Mock, patch


SCRIPT = Path(__file__).resolve().parents[1] / "native-network-check.py"
SPEC = importlib.util.spec_from_file_location("native_network_check", SCRIPT)
check = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(check)
WITNESS_PATH = SCRIPT.parents[1] / "examples/native-sandbox/network-witness.py"
WITNESS_SPEC = importlib.util.spec_from_file_location("native_network_witness", WITNESS_PATH)
witness = importlib.util.module_from_spec(WITNESS_SPEC)
WITNESS_SPEC.loader.exec_module(witness)
SENTINEL = "synthetic-private-response-must-not-print"


def bindings():
    return {role: {"name": role, "uid": "agent-" + role, "podUID": "pod-" + role,
                   "hostname": "workload-" + role, "fingerprint": check.fingerprint({"role": role})}
            for role in ("control", "cidr", "ports")}


def successful_run(phases=check.PHASES, sources=None):
    targets = {"restricted": "192.0.2.10", "outside": "198.51.100.20"}
    health = {name: {"instance": character * 32, "overflow": False, "uptime": 1.0,
                     "tcp": [18080, 18082], "udp": [18081, 18083]}
              for name, character in (("restricted", "a"), ("outside", "b"))}
    selected = bindings() if sources is None else {
        role: check.snapshot(sources, role) for role in ("control", "cidr", "ports")}
    plan = check.make_plan(selected, targets, "gateway.example.com", health)
    events = {name: {"instance": value["instance"], "overflow": False, "uptime": 60.0, "events": []}
              for name, value in health.items()}
    reports = []
    for phase in phases:
        payload = check.probe_payload(plan, phase)

        def exchange(address, protocol, port, run, nonce, timeout=3):
            target = next(name for name, value in targets.items() if value == address)
            denied = (phase == "cidr" and target == "restricted") or (
                phase == "ports" and (protocol, port) in (("TCP", 18080), ("UDP", 18081)))
            if denied:
                return {"outcome": "connect-timeout" if protocol == "TCP" else "receive-timeout"}
            item = {"run": run, "nonce": nonce, "protocol": protocol, "port": port,
                    "instance": health[target]["instance"],
                    "sequence": len(events[target]["events"]) + 1}
            events[target]["events"].append(item)
            return {"outcome": "allowed", "witness": copy.deepcopy(item)}

        with patch.object(check, "exchange", side_effect=exchange), \
                patch.object(check.socket, "gethostname", return_value=payload["hostname"]), \
                patch.object(check.socket, "getaddrinfo", return_value=[("synthetic-address",)]):
            reports.append(check.run_probe(payload))
    return plan, reports, events


class NativeNetworkCheckTests(unittest.TestCase):
    def test_witness_deadline_bounds_progressing_status_headers_and_body(self):
        body = b'{"valid":true,"padding":"' + b"x" * 24 + b'"}'
        status = b"HTTP/1.1 200 OK\r\n"
        headers = b"Content-Length: " + str(len(body)).encode() + b"\r\nConnection: close\r\n\r\n"
        cases = {
            "status": (b"", [bytes([byte]) for byte in status], headers + body),
            "headers": (status, [bytes([byte]) for byte in b"X-Progress: " + b"x" * 24 + b"\r\n"], headers + body),
            "body": (status + headers, [bytes([byte]) for byte in body], b""),
            "chunked-body": (
                status + b"Transfer-Encoding: chunked\r\nConnection: close\r\n\r\n",
                [b"1\r\n" + bytes([byte]) + b"\r\n" for byte in body], b"0\r\n\r\n",
            ),
        }
        for phase, (prefix, chunks, suffix) in cases.items():
            with self.subTest(phase=phase):
                stop = threading.Event()
                sent = []
                sockets = []
                socket_type = check.WitnessSocket

                def tracked_socket(address, deadline):
                    value = socket_type(address, deadline)
                    sockets.append(value)
                    return value

                class Handler(socketserver.BaseRequestHandler):
                    def handle(self):
                        self.request.settimeout(2)
                        try:
                            request = b""
                            while b"\r\n\r\n" not in request:
                                part = self.request.recv(4096)
                                if not part:
                                    return
                                request += part
                            self.request.sendall(prefix)
                            for chunk in chunks:
                                if stop.wait(0.04):
                                    return
                                self.request.sendall(chunk)
                                sent.append(chunk)
                            self.request.sendall(suffix)
                        except (BrokenPipeError, ConnectionResetError):
                            pass

                with socketserver.ThreadingTCPServer(("127.0.0.1", 0), Handler) as server:
                    thread = threading.Thread(target=server.serve_forever, daemon=True)
                    thread.start()
                    try:
                        started = time.monotonic()
                        with patch.object(check, "WitnessSocket", side_effect=tracked_socket), \
                                self.assertRaisesRegex(check.CheckError, "witness-deadline-exceeded"):
                            check.request_json("127.0.0.1", "/healthz", port=server.server_address[1], timeout=0.4)
                        self.assertLess(time.monotonic() - started, 1.15)
                        self.assertGreaterEqual(len(sent), 2, "the witness must make progress before the deadline")
                        self.assertEqual(len(sockets), 1)
                        self.assertEqual(sockets[0].fileno(), -1, "the timed-out connection must be closed")
                    finally:
                        stop.set()
                        server.shutdown()
                        thread.join(timeout=2)

    def test_connect_send_and_buffered_reads_share_one_deadline(self):
        with patch.object(check.time, "monotonic", side_effect=[10, 10.2, 10.5, 10.7, 11]), \
                patch.object(socket.socket, "connect") as connect, \
                patch.object(socket.socket, "sendall") as send, \
                patch.object(socket.socket, "recv_into", return_value=1) as receive:
            connection = check.WitnessConnection("127.0.0.1", 18082, timeout=1)
            try:
                connection.connect()
                self.assertAlmostEqual(connection.sock.gettimeout(), 0.8)
                connection.sock.sendall(b"x")
                self.assertAlmostEqual(connection.sock.gettimeout(), 0.5)
                self.assertEqual(connection.sock.recv_into(bytearray(1)), 1)
                self.assertAlmostEqual(connection.sock.gettimeout(), 0.3)
                with self.assertRaises(TimeoutError):
                    connection.sock.recv_into(bytearray(1))
                connect.assert_called_once_with(("127.0.0.1", 18082))
                send.assert_called_once_with(b"x", 0)
                receive.assert_called_once()
            finally:
                connection.close()

    def test_witness_socket_uses_literal_ipv4_and_ipv6_without_dns(self):
        for address, family in (("127.0.0.1", socket.AF_INET), ("::1", socket.AF_INET6)):
            if family == socket.AF_INET6 and not socket.has_ipv6:
                continue
            with self.subTest(address=address), patch.object(check.socket, "getaddrinfo") as resolve:
                with check.WitnessSocket(address, time.monotonic() + 1) as endpoint:
                    self.assertEqual(endpoint.family, family)
                resolve.assert_not_called()

    def test_invalid_witness_timeout_fails_before_opening_a_connection(self):
        for timeout in (None, True, 0, -1, float("nan"), float("inf")):
            with self.subTest(timeout=timeout), patch.object(check, "WitnessConnection") as connection:
                with self.assertRaisesRegex(check.CheckError, "invalid-witness-timeout"):
                    check.request_json("192.0.2.10", "/healthz", timeout=timeout)
                connection.assert_not_called()

    def test_response_completed_after_deadline_is_rejected_and_closed(self):
        connection = Mock()
        connection.deadline = 10
        response = connection.getresponse.return_value
        response.status = 200
        response.read.return_value = b'{"valid":true}'
        with patch.object(check, "WitnessConnection", return_value=connection), \
                patch.object(check.time, "monotonic", return_value=10):
            with self.assertRaisesRegex(check.CheckError, "witness-deadline-exceeded"):
                check.request_json("192.0.2.10", "/healthz")
        response.close.assert_called_once()
        connection.close.assert_called_once()

    def test_complete_run_requires_both_controls_and_independent_witnesses(self):
        plan, reports, events = successful_run()
        result = check.verify_observations(plan, reports, events)
        self.assertEqual(result["status"], "passed")
        self.assertEqual(result["checks"], 32)
        self.assertEqual(result["denials"], 8)
        self.assertIs(result["qualifiesExecution"], False)
        self.assertEqual(result["ownerAccess"], "not-tested")
        self.assertEqual(result["modelTool"], "not-tested")
        self.assertNotIn("192.0.2.10", str(result))
        self.assertNotIn("agent-control", str(result))

    def test_missing_control_is_not_a_pass(self):
        plan, reports, events = successful_run()
        with self.assertRaises(check.CheckError):
            check.verify_observations(plan, reports[:-1], events)

    def test_duplicate_rows_cannot_replace_an_expected_check(self):
        plan, reports, events = successful_run()
        reports[0]["checks"][1] = copy.deepcopy(reports[0]["checks"][0])
        with self.assertRaises(check.CheckError):
            check.verify_observations(plan, reports, events)

    def test_a_dead_witness_is_not_a_policy_denial(self):
        plan, reports, events = successful_run()
        reports[0]["checks"][0]["outcome"] = "connect-timeout"
        with self.assertRaises(check.CheckError):
            check.verify_observations(plan, reports, events)

    def test_stale_or_replaced_witness_is_rejected(self):
        for mutation in ("instance", "overflow", "run"):
            plan, reports, events = successful_run()
            if mutation == "instance":
                events["outside"]["instance"] = "c" * 32
            elif mutation == "overflow":
                events["outside"]["overflow"] = True
            else:
                events["outside"]["events"][0]["run"] = "d" * 32
            with self.subTest(mutation=mutation), self.assertRaises(check.CheckError):
                check.verify_observations(plan, reports, events)

    def test_denied_packet_observed_by_witness_fails(self):
        plan, reports, events = successful_run()
        row = next(row for row in reports[1]["checks"] if row["outcome"] == "connect-timeout")
        events["restricted"]["events"].append({
            "run": plan["run"], "nonce": row["nonce"], "protocol": row["protocol"],
            "port": row["port"], "instance": events["restricted"]["instance"], "sequence": 100,
        })
        with self.assertRaises(check.CheckError):
            check.verify_observations(plan, reports, events)

    def test_duplicate_witness_receipt_cannot_hide_a_probe_replay(self):
        plan, reports, events = successful_run()
        events["outside"]["events"].append(copy.deepcopy(events["outside"]["events"][0]))
        with self.assertRaises(check.CheckError):
            check.verify_observations(plan, reports, events)

    def test_wrong_agent_or_modified_plan_is_rejected(self):
        for mutation in ("agent", "plan"):
            plan, reports, events = successful_run()
            if mutation == "agent":
                reports[1]["agentUID"] = "agent-control"
            else:
                plan["dnsName"] = "changed.example.com"
            with self.subTest(mutation=mutation), self.assertRaises(check.CheckError):
                check.verify_observations(plan, reports, events)

    def test_positive_controls_must_bracket_restricted_probes(self):
        plan, reports, events = successful_run(("cidr", "control-before", "ports", "control-after"))
        with self.assertRaisesRegex(check.CheckError, "positive-controls-must-bracket-denials"):
            check.verify_observations(plan, reports, events)

    def test_exact_freshness_threshold_and_bad_clocks(self):
        plan, reports, events = successful_run()
        for value in events.values():
            value["uptime"] = 1 + check.MAX_AGE
        self.assertEqual(check.verify_observations(plan, reports, events)["status"], "passed")
        for age in (1 + check.MAX_AGE + 0.001, 0, True, float("nan"), float("inf")):
            bad = copy.deepcopy(events)
            bad["outside"]["uptime"] = age
            with self.subTest(age=age), self.assertRaises(check.CheckError):
                check.verify_observations(plan, reports, bad)

    def test_missing_or_nonboolean_dns_observation_fails(self):
        for value in (False, 1, "true", None):
            plan, reports, events = successful_run()
            reports[0]["dnsResolved"] = value
            with self.subTest(value=value), self.assertRaises(check.CheckError):
                check.verify_observations(plan, reports, events)

    def test_changed_checker_is_not_reused_for_an_old_run(self):
        plan, reports, events = successful_run()
        with patch.object(check, "checker_digest", return_value="changed"):
            with self.assertRaisesRegex(check.CheckError, "checker-source-changed"):
                check.verify_observations(plan, reports, events)

    def test_checker_pin_includes_the_shared_kubernetes_client_source(self):
        with patch.object(check.Path, "read_bytes", side_effect=[b"checker", b"client-before"]):
            before = check.checker_digest()
        with patch.object(check.Path, "read_bytes", side_effect=[b"checker", b"client-after"]):
            after = check.checker_digest()
        self.assertNotEqual(before, after)

    def test_malformed_nested_plan_fields_fail_explicitly(self):
        for key, value in (("agents", []), ("phases", []), ("targets", [])):
            plan, _, _ = successful_run()
            plan[key] = value
            plan["digest"] = check.fingerprint({name: item for name, item in plan.items() if name != "digest"})
            with self.subTest(key=key), self.assertRaises(check.CheckError):
                check.validate_plan(plan)
        plan, _, _ = successful_run()
        plan["phases"]["cidr"] = "not-an-object"
        plan["digest"] = check.fingerprint({name: item for name, item in plan.items() if name != "digest"})
        with self.assertRaises(check.CheckError):
            check.validate_plan(plan)

    def test_probe_must_execute_in_the_recorded_hostname(self):
        plan, _, _ = successful_run()
        with patch.object(check.socket, "gethostname", return_value="wrong-pod"), \
                patch.object(check, "exchange") as send:
            with self.assertRaisesRegex(check.CheckError, "hostname"):
                check.run_probe(check.probe_payload(plan, "cidr"))
        send.assert_not_called()

    def test_invalid_targets_fail_before_kubernetes_or_witness_calls(self):
        with patch.object(check, "client") as client, \
                patch.object(check, "request_json") as request, \
                contextlib.redirect_stdout(io.StringIO()) as output:
            code = check.main([
                "prepare", "--control-agent", "control", "--cidr-agent", "cidr", "--ports-agent", "ports",
                "--restricted-witness", "https://example.com", "--outside-witness", "198.51.100.20",
                "--dns-name", "gateway.example.com", "--output", "/example/private/plan.json",
                "--kubeconfig", "/example/private/kubeconfig", "--context", "example", "--cluster-uid", "example",
            ])
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(output.getvalue())["reason"], "literal-witness-ip-required")
        client.assert_not_called()
        request.assert_not_called()

    def test_recorded_output_is_bounded_and_unambiguous(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            path.write_text('{"status":"observed","status":"passed"}', encoding="utf-8")
            with self.assertRaisesRegex(check.CheckError, "duplicate-json-field"):
                check.read_file(path)
            path.write_text((check.RESULT_PREFIX + '{"status":"observed"}\n') * 2, encoding="utf-8")
            with self.assertRaisesRegex(check.CheckError, "duplicate-probe-output"):
                check.read_file(path, report=True)
            path.write_text(SENTINEL + "x" * check.MAX_BYTES, encoding="utf-8")
            with self.assertRaisesRegex(check.CheckError, "input-file-too-large") as caught:
                check.read_file(path)
            self.assertNotIn(SENTINEL, str(caught.exception))

    def test_witness_health_cannot_smuggle_additional_fields_into_plan(self):
        plan, _, _ = successful_run()
        plan["witnesses"]["outside"]["unrelated"] = SENTINEL
        with self.assertRaises(check.CheckError) as caught:
            check.make_plan(plan["agents"], plan["targets"], plan["dnsName"], plan["witnesses"])
        self.assertNotIn(SENTINEL, str(caught.exception))

    def test_tcp_timeout_after_connect_is_not_a_policy_denial(self):
        connection = Mock()
        connection.getresponse.side_effect = TimeoutError(SENTINEL)
        with patch.object(check.http.client, "HTTPConnection", return_value=connection):
            with self.assertRaisesRegex(check.CheckError, "tcp-transport-or-service-failure") as caught:
                check.exchange("192.0.2.10", "TCP", 18080, "a" * 32, "b" * 32)
        self.assertNotIn(SENTINEL, str(caught.exception))
        connection.close.assert_called_once()

    def test_connection_refusal_is_not_a_policy_denial(self):
        connection = Mock()
        connection.connect.side_effect = ConnectionRefusedError(SENTINEL)
        with patch.object(check.http.client, "HTTPConnection", return_value=connection):
            with self.assertRaisesRegex(check.CheckError, "tcp-transport-or-service-failure"):
                check.exchange("192.0.2.10", "TCP", 18080, "a" * 32, "b" * 32)

    def test_udp_refusal_is_not_a_policy_denial(self):
        endpoint = Mock()
        endpoint.send.return_value = 65
        endpoint.recv.side_effect = ConnectionRefusedError(SENTINEL)
        with patch.object(check.socket, "socket") as factory:
            factory.return_value.__enter__.return_value = endpoint
            with self.assertRaisesRegex(check.CheckError, "udp-transport-or-service-failure"):
                check.exchange("192.0.2.10", "UDP", 18081, "a" * 32, "b" * 32)

    def test_witness_http_does_not_follow_redirects(self):
        connection = Mock()
        connection.getresponse.return_value.status = 302
        with patch.object(check, "WitnessConnection", return_value=connection) as factory:
            with self.assertRaisesRegex(check.CheckError, "witness-http-status"):
                check.request_json("192.0.2.10", "/healthz")
        factory.assert_called_once()
        connection.getresponse.return_value.read.assert_not_called()
        connection.getresponse.return_value.close.assert_called_once()
        connection.close.assert_called_once()

    def test_real_tcp_udp_receipts_and_capacity_failure(self):
        observations = witness.Observations()
        with http.server.ThreadingHTTPServer(("127.0.0.1", 0), witness.HTTPHandler) as tcp, \
                socketserver.UDPServer(("127.0.0.1", 0), witness.UDPHandler) as udp:
            tcp.observations = udp.observations = observations
            threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in (tcp, udp)]
            for thread in threads:
                thread.start()
            try:
                first = check.exchange("127.0.0.1", "TCP", tcp.server_port, "a" * 32, "b" * 32)
                second = check.exchange("127.0.0.1", "UDP", udp.server_address[1], "a" * 32, "c" * 32)
                self.assertEqual(first["outcome"], "allowed")
                self.assertEqual(second["outcome"], "allowed")
                state = check.request_json("127.0.0.1", "/events/" + "a" * 32, port=tcp.server_port)
                self.assertEqual(state["events"], [first["witness"], second["witness"]])
                with patch.object(witness, "MAX_EVENTS", 2):
                    with self.assertRaisesRegex(check.CheckError, "witness-http-status"):
                        check.exchange("127.0.0.1", "TCP", tcp.server_port, "a" * 32, "d" * 32)
                self.assertTrue(observations.snapshot()["overflow"])
            finally:
                tcp.shutdown()
                udp.shutdown()
                for thread in threads:
                    thread.join(timeout=5)

    def test_witness_peer_filter_is_explicit_and_handles_mapped_ipv4(self):
        networks = [ipaddress.ip_network("192.0.2.10/32")]
        self.assertTrue(witness.allowed_peer("192.0.2.10", networks))
        self.assertTrue(witness.allowed_peer("::ffff:192.0.2.10", networks))
        self.assertFalse(witness.allowed_peer("192.0.2.11", networks))
        self.assertFalse(witness.allowed_peer("::1", networks))

    def test_witness_peer_rejection_is_not_misreported_as_policy_denial(self):
        class Server(witness.PeerFilter, http.server.ThreadingHTTPServer):
            pass

        with Server(("127.0.0.1", 0), witness.HTTPHandler) as server:
            server.allowed_peers = [ipaddress.ip_network("192.0.2.10/32")]
            server.observations = witness.Observations()
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with self.assertRaisesRegex(check.CheckError, "tcp-transport-or-service-failure"):
                    check.exchange("127.0.0.1", "TCP", server.server_port, "a" * 32, "b" * 32)
                self.assertEqual(server.observations.events, [])
            finally:
                server.shutdown()
                thread.join(timeout=5)

    @unittest.skipUnless("fork" in check.multiprocessing.get_all_start_methods(),
                         "owner probe requires a fork-capable runtime")
    def test_generated_command_runs_the_checked_probe_without_kubernetes(self):
        plan, _, _ = successful_run()
        command = check.command(plan, "control-before")
        self.assertNotIn("kubectl", command)
        args = shlex.split(command)
        self.assertEqual(args[:2], ["python3", "-c"])
        payload = json.loads(args[3])
        self.assertEqual(payload["agentUID"], "agent-control")
        result = subprocess.run(args, text=True, capture_output=True, timeout=10, check=False)
        self.assertEqual(result.returncode, 1)
        self.assertNotIn(check.RESULT_PREFIX, result.stdout)
        self.assertIn("no result accepted", result.stderr)


@unittest.skipUnless("fork" in check.multiprocessing.get_all_start_methods(),
                     "owner probe requires a fork-capable runtime")
class NativeNetworkSupervisionTests(unittest.TestCase):
    def test_generated_command_emits_one_complete_successful_result(self):
        plan, reports, _ = successful_run()
        original_source = check.inspect.getsource

        def source(function):
            if function is check.run_probe:
                return f"def run_probe(payload):\n return {reports[0]!r}\n"
            return original_source(function)

        with patch.object(check.inspect, "getsource", side_effect=source):
            args = shlex.split(check.command(plan, "control-before"))
        result = subprocess.run(args, text=True, capture_output=True, timeout=5, check=False)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stderr, "")
        self.assertEqual(result.stdout.count(check.RESULT_PREFIX), 1)
        self.assertEqual(json.loads(result.stdout.removeprefix(check.RESULT_PREFIX)), reports[0])

    def test_successful_result_and_large_output_are_preserved_and_reaped(self):
        _, reports, _ = successful_run()
        for result in (reports[0], {"status": "observed", "padding": "x" * (256 * 1024)}):
            with self.subTest(size=len(json.dumps(result))):
                previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}
                children = check.multiprocessing.active_children()
                with patch.object(check, "run_probe", return_value=result), \
                        patch.object(check, "PROBE_TIMEOUT", 2):
                    self.assertEqual(check.supervised_probe({}), result)
                self.assertEqual(check.multiprocessing.active_children(), children)
                for sig, handler in previous.items():
                    self.assertEqual(signal.getsignal(sig), handler)

    def test_deadline_closes_sockets_and_reaps_worker(self):
        sockets = []
        pids = []
        socket_pair = check.socket.socketpair
        process_type = check.multiprocessing.get_context("fork").Process
        original_start = process_type.start
        previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}

        def pair():
            endpoints = socket_pair()
            sockets.extend(endpoints)
            return endpoints

        def start(worker):
            original_start(worker)
            pids.append(worker.pid)

        with patch.object(check, "run_probe", side_effect=lambda _: time.sleep(5)), \
                patch.object(check, "PROBE_TIMEOUT", 0.1), \
                patch.object(check.socket, "socketpair", side_effect=pair), \
                patch.object(process_type, "start", start):
            started = time.monotonic()
            with self.assertRaisesRegex(check.CheckError, "probe-deadline-exceeded"):
                check.supervised_probe({})
            self.assertLess(time.monotonic() - started, 1)
        self.assertEqual(len(pids), 1)
        with self.assertRaises(ChildProcessError):
            os.waitpid(pids[0], os.WNOHANG)
        self.assertEqual([endpoint.fileno() for endpoint in sockets], [-1, -1])
        for sig, handler in previous.items():
            self.assertEqual(signal.getsignal(sig), handler)

    def test_unavailable_supervision_fails_before_network(self):
        with patch.object(check.multiprocessing, "get_all_start_methods", return_value=[]), \
                patch.object(check, "run_probe") as probe, \
                patch.object(check.socket, "socketpair") as pair:
            with self.assertRaisesRegex(check.CheckError, "probe-supervision-unavailable"):
                check.supervised_probe({})
            probe.assert_not_called()
            pair.assert_not_called()

    def test_worker_result_must_be_bounded_observed_and_exit_successfully(self):
        for result, reason in (
            ({"status": "failed"}, "probe-worker-result-invalid"),
            ({"status": "observed", "padding": "x" * check.MAX_BYTES}, "probe-worker-failed"),
        ):
            with self.subTest(reason=reason), patch.object(check, "run_probe", return_value=result):
                with self.assertRaisesRegex(check.CheckError, reason):
                    check.supervised_probe({})
        original = check.probe_worker

        def fail_after_result(*args):
            original(*args)
            os._exit(1)

        with patch.object(check, "run_probe", return_value={"status": "observed"}), \
                patch.object(check, "probe_worker", fail_after_result):
            with self.assertRaisesRegex(check.CheckError, "probe-worker-failed"):
                check.supervised_probe({})

    def test_cancellation_during_process_start_still_reaps_worker(self):
        process_type = check.multiprocessing.get_context("fork").Process
        original_start = process_type.start
        for signum in (signal.SIGINT, signal.SIGTERM):
            with self.subTest(signum=signum):
                pids = []
                previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}

                def start(worker):
                    original_start(worker)
                    pids.append(worker.pid)
                    os.kill(os.getpid(), signum)

                with patch.object(process_type, "start", start), \
                        patch.object(check, "run_probe", side_effect=lambda _: time.sleep(5)):
                    with self.assertRaises(SystemExit) as exited:
                        check.supervised_probe({})
                self.assertEqual(exited.exception.code, 128 + signum)
                self.assertEqual(len(pids), 1)
                with self.assertRaises(ChildProcessError):
                    os.waitpid(pids[0], os.WNOHANG)
                for sig, handler in previous.items():
                    self.assertEqual(signal.getsignal(sig), handler)

    def test_generated_command_sanitizes_worker_and_start_failures(self):
        plan, _, _ = successful_run()
        args = shlex.split(check.command(plan, "control-before"))
        process_type = check.multiprocessing.get_context("fork").Process
        failures = (
            patch.object(check.socket, "gethostname", side_effect=RuntimeError(SENTINEL)),
            patch.object(process_type, "start", side_effect=OSError(SENTINEL)),
        )
        for failure in failures:
            previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}
            with failure, patch.object(sys, "argv", ["-c", args[3]]), \
                    contextlib.redirect_stdout(io.StringIO()) as output, \
                    contextlib.redirect_stderr(io.StringIO()) as errors:
                with self.assertRaises(SystemExit) as exited:
                    exec(args[2], {})
            self.assertEqual(exited.exception.code, 1)
            self.assertEqual(output.getvalue(), "")
            self.assertEqual(errors.getvalue(), "Native network probe failed; no result accepted.\n")
            for sig, handler in previous.items():
                self.assertEqual(signal.getsignal(sig), handler)


@unittest.skipUnless(sys.platform.startswith("linux") and platform.libc_ver()[0] == "glibc" and
                     shutil.which("cc"), "Linux/glibc and a C compiler are required for native DNS tests")
class NativeNetworkDNSDeadlineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-network-dns-")
        cls.addClassCleanup(temporary.cleanup)
        cls.library = Path(temporary.name) / "stalled-dns.so"
        subprocess.run([
            "cc", "-shared", "-fPIC", "-Wall", "-Wextra", "-Werror",
            str(Path(__file__).with_name("fixtures") / "stalled_dns.c"),
            "-o", str(cls.library), "-lresolv",
        ], capture_output=True, text=True, timeout=15, check=True)

    def run_stalled_resolver(self, signum=None, deadline=1):
        wrapper = r'''
import ctypes, http.client, json, os, pathlib, socket, sys
program, raw, library, port, pid_file = sys.argv[1:]
payload = json.loads(raw)
native = ctypes.CDLL(library)
native.cetirun_test_dns.argtypes = [ctypes.c_ushort]
native.cetirun_test_dns.restype = ctypes.c_int
original = socket.getaddrinfo
def resolve(host, service, *args, **kwargs):
    assert host == "nonresponding.example.invalid"
    assert native.cetirun_test_dns(int(port)) == 0
    pathlib.Path(pid_file).write_text(str(os.getpid()))
    return original(host + ".", service, *args, **kwargs)
class Marker:
    status = 200
    def __init__(self, *_args, **_kwargs): pass
    def connect(self): pass
    def request(self, method, path): self.nonce = path.rsplit("/", 1)[1]
    def getresponse(self): return self
    def read(self, _limit):
        return json.dumps({"run": payload["run"], "nonce": self.nonce, "protocol": "TCP",
                           "port": 18082, "instance": "b" * 32, "sequence": 1}).encode()
    def close(self): pass
socket.gethostname = lambda: payload["hostname"]
socket.getaddrinfo = resolve
http.client.HTTPConnection = Marker
sys.argv = ["-c", raw]
exec(program)
'''
        plan, _, _ = successful_run()
        plan["dnsName"] = "nonresponding.example.invalid"
        plan["digest"] = check.fingerprint({key: value for key, value in plan.items() if key != "digest"})
        with patch.object(check, "PROBE_TIMEOUT", 30 if signum else deadline):
            command = shlex.split(check.command(plan, "control-before"))
        with tempfile.TemporaryDirectory(prefix="cetirun-network-worker-") as directory, \
                socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as dns:
            dns.bind(("127.0.0.1", 0))
            dns.settimeout(5)
            pid_file = Path(directory) / "worker.pid"
            started = time.monotonic()
            process = subprocess.Popen([
                sys.executable, "-B", "-c", wrapper, *command[2:], str(self.library),
                str(dns.getsockname()[1]), str(pid_file),
            ], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                text=True, start_new_session=True)
            try:
                query, sender = dns.recvfrom(512)
                self.assertEqual(sender[0], "127.0.0.1")
                self.assertIn(b"\rnonresponding\x07example\x07invalid\x00", query)
                worker_pid = int(pid_file.read_text())
                if signum is not None:
                    started = time.monotonic()
                    process.send_signal(signum)
                budget = 3 if signum else deadline + 2
                try:
                    stdout, stderr = process.communicate(timeout=budget)
                except subprocess.TimeoutExpired:
                    raise AssertionError("the generated probe did not bound the stalled native resolver") from None
                self.assertLess(time.monotonic() - started, budget)
                self.assertEqual(stdout, "")
                self.assertEqual(stderr, "Native network probe failed; no result accepted.\n")
                self.assertNotEqual(worker_pid, process.pid)
                with self.assertRaises(ProcessLookupError):
                    os.kill(worker_pid, 0)
                return process.returncode
            finally:
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGKILL)
                process.communicate(timeout=5)

    def test_native_getaddrinfo_deadline_kills_and_reaps_worker(self):
        self.assertEqual(check.PROBE_TIMEOUT, 60)
        self.assertEqual(self.run_stalled_resolver(), 1)

    def test_sigterm_during_native_getaddrinfo_kills_and_reaps_worker(self):
        self.assertEqual(self.run_stalled_resolver(signal.SIGTERM), 143)

    def test_sigint_during_native_getaddrinfo_kills_and_reaps_worker(self):
        self.assertEqual(self.run_stalled_resolver(signal.SIGINT), 130)


class FakeSources:
    def __init__(self):
        self.reads = []
        self.target_checks = 0
        api = "cetirun.microsoft.com/v1alpha1"

        def value(kind, name, spec, version=api):
            return {"apiVersion": version, "kind": kind, "metadata": {
                "name": name, "uid": "uid-" + name, "generation": 1}, "spec": spec}

        self.objects = {}
        for role in ("control", "cidr", "ports"):
            agent = value("Agent", role, {"agent": {"type": "openclaw"}, "team": "team-" + role,
                                         "sandboxRef": "sandbox"})
            agent["status"] = {"phase": "Ready"}
            self.objects["agent", role] = agent
            namespace = "cetirun-" + role
            ns = value("Namespace", namespace, {}, "v1")
            ns["metadata"].update(labels={check.PROTECTED: "true"}, ownerReferences=[{
                "apiVersion": api, "kind": "Agent", "name": role, "uid": "uid-" + role}])
            self.objects["namespace", namespace] = ns
            pod = value("Pod", "pod-" + role, {"runtimeClassName": "kata"}, "v1")
            pod["metadata"].update(namespace=namespace, labels={check.AGENT_UID: "uid-" + role})
            pod["status"] = {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}],
                             "containerStatuses": [{"name": "openclaw", "ready": True, "restartCount": 0,
                                                    "imageID": "example.com/runtime@sha256:" + "a" * 64}]}
            self.objects["pods", namespace] = {"kind": "List", "items": [pod]}
            self.objects["team", "team-" + role] = value("Team", "team-" + role, {"agentPolicyRef": "policy-" + role})
            network = {}
            if role == "cidr":
                network["blockedCIDRs"] = ["192.0.2.0/24"]
            elif role == "ports":
                network["blockedPorts"] = [{"port": 18080, "protocol": "TCP"}, {"port": 18081, "protocol": "UDP"}]
            self.objects["agentpolicy", "policy-" + role] = value("AgentPolicy", "policy-" + role, {"network": network})
        self.objects["sandbox", "sandbox"] = value("Sandbox", "sandbox", {"type": "kata"})
        runtime = value("RuntimeClass", "kata", {}, "node.k8s.io/v1")
        runtime["handler"] = "kata-example"
        self.objects["runtimeclass", "kata"] = runtime

    def check_target(self):
        self.target_checks += 1

    def get(self, resource, name):
        self.reads.append((resource, name))
        return copy.deepcopy(self.objects[resource, name])

    def run(self, args):
        if args[:3] != ["get", "pods", "--namespace"] or args[4:] != ["-o", "json"]:
            raise AssertionError("only the bounded Pod read is allowed")
        self.reads.append(("pods", args[3]))
        return json.dumps(self.objects["pods", args[3]])


class NativeNetworkSourceTests(unittest.TestCase):
    def test_prepare_and_verify_report_deadline_failure_without_private_diagnostics(self):
        for mode in ("prepare", "verify"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                kube = FakeSources()
                common = ["--kubeconfig", "/example/private/kubeconfig",
                          "--context", "example", "--cluster-uid", "example-cluster"]
                if mode == "prepare":
                    args = ["prepare", *common, "--control-agent", "control",
                            "--cidr-agent", "cidr", "--ports-agent", "ports",
                            "--restricted-witness", "192.0.2.10", "--outside-witness", "198.51.100.20",
                            "--dns-name", "gateway.example.com", "--output", str(root / "plan.json")]
                else:
                    plan, reports, _ = successful_run(sources=kube)
                    plan["clusterUID"] = "example-cluster"
                    plan["digest"] = check.fingerprint({key: value for key, value in plan.items() if key != "digest"})
                    (root / "plan.json").write_text(json.dumps(plan), encoding="utf-8")
                    args = ["verify", *common, "--plan", str(root / "plan.json")]
                    for index, report in enumerate(reports):
                        report["planDigest"] = plan["digest"]
                        path = root / f"report-{index}.json"
                        path.write_text(json.dumps(report), encoding="utf-8")
                        args.extend(("--result", str(path)))
                connection = Mock()
                connection.getresponse.side_effect = TimeoutError(SENTINEL)
                with patch.object(check, "client", return_value=kube), \
                        patch.object(check, "WitnessConnection", return_value=connection), \
                        contextlib.redirect_stdout(io.StringIO()) as output, \
                        contextlib.redirect_stderr(io.StringIO()) as errors:
                    code = check.main(args)
                self.assertEqual(code, 1)
                self.assertEqual(json.loads(output.getvalue()), {
                    "status": "failed", "reason": "witness-deadline-exceeded", "qualifiesExecution": False,
                })
                self.assertNotIn(SENTINEL, output.getvalue() + errors.getvalue())
                self.assertNotIn("192.0.2.10", output.getvalue() + errors.getvalue())
                connection.close.assert_called_once()
                if mode == "prepare":
                    self.assertFalse((root / "plan.json").exists(), "a timed-out witness must not produce a plan")

    def test_actual_selected_policies_are_checked_without_writes_or_secret_reads(self):
        kube = FakeSources()
        selected = {role: check.snapshot(kube, role) for role in ("control", "cidr", "ports")}
        check.policy_matrix(selected, {"restricted": "192.0.2.10", "outside": "198.51.100.20"})
        check.verify_bindings(kube, selected)
        self.assertEqual(kube.target_checks, 1)
        self.assertFalse(any("secret" in resource for resource, _ in kube.reads))

    def test_kubectl_list_and_api_pod_list_shapes_validate_each_pod(self):
        for kind in ("List", "PodList"):
            kube = FakeSources()
            kube.objects["pods", "cetirun-control"]["kind"] = kind
            self.assertEqual(check.snapshot(kube, "control")["uid"], "uid-control")
            kube.objects["pods", "cetirun-control"]["items"][0]["kind"] = "Service"
            with self.subTest(kind=kind), self.assertRaisesRegex(check.CheckError, "resource-kind-mismatch"):
                check.snapshot(kube, "control")

    def test_changed_source_pod_image_runtime_or_namespace_is_rejected(self):
        for changed in ("agent", "pod", "policy", "image", "restart", "runtime", "namespace"):
            kube = FakeSources()
            selected = {"control": check.snapshot(kube, "control")}
            pod = kube.objects["pods", "cetirun-control"]["items"][0]
            if changed == "agent":
                kube.objects["agent", "control"]["metadata"]["uid"] = "replacement"
            elif changed == "pod":
                pod["metadata"]["uid"] = "replacement"
            elif changed == "policy":
                kube.objects["agentpolicy", "policy-control"]["spec"]["network"]["blockedCIDRs"] = ["192.0.2.0/24"]
            elif changed == "image":
                pod["status"]["containerStatuses"][0]["imageID"] = "example.com/runtime@sha256:" + "b" * 64
            elif changed == "restart":
                pod["status"]["containerStatuses"][0]["restartCount"] = 1
            elif changed == "runtime":
                kube.objects["runtimeclass", "kata"]["overhead"] = {"podFixed": {"memory": "1Gi"}}
            else:
                kube.objects["namespace", "cetirun-control"]["metadata"]["labels"]["control-plane"] = "true"
            with self.subTest(changed=changed), self.assertRaises(check.CheckError):
                check.verify_bindings(kube, selected)

    def test_wrong_policy_matrix_or_shared_binding_is_rejected(self):
        kube = FakeSources()
        selected = {role: check.snapshot(kube, role) for role in ("control", "cidr", "ports")}
        for bad in ("target", "policy"):
            changed = copy.deepcopy(selected)
            targets = {"restricted": "192.0.2.10", "outside": "198.51.100.20"}
            if bad == "target":
                targets["outside"] = "192.0.2.11"
            else:
                changed["cidr"]["policyUID"] = changed["control"]["policyUID"]
            with self.subTest(bad=bad), self.assertRaises(check.CheckError):
                check.policy_matrix(changed, targets)

    def test_cli_rechecks_sources_after_fetching_witness_evidence(self):
        kube = FakeSources()
        plan, reports, events = successful_run(sources=kube)
        plan["clusterUID"] = "example-cluster"
        plan["digest"] = check.fingerprint({key: value for key, value in plan.items() if key != "digest"})
        for report in reports:
            report["planDigest"] = plan["digest"]

        def request(address, path):
            target = next(name for name, value in plan["targets"].items() if value == address)
            if target == "outside":
                kube.objects["pods", "cetirun-control"]["items"][0]["metadata"]["uid"] = "replacement"
            return events[target]

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "plan.json").write_text(json.dumps(plan), encoding="utf-8")
            args = ["verify", "--plan", str(root / "plan.json"), "--kubeconfig", "/example/kubeconfig",
                    "--context", "example", "--cluster-uid", "example-cluster"]
            for index, report in enumerate(reports):
                path = root / f"report-{index}.json"
                path.write_text(json.dumps(report), encoding="utf-8")
                args.extend(("--result", str(path)))
            with patch.object(check, "client", return_value=kube), \
                    patch.object(check, "request_json", side_effect=request), \
                    contextlib.redirect_stdout(io.StringIO()) as output:
                code = check.main(args)
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(output.getvalue())["reason"], "agent-or-configuration-changed")


if __name__ == "__main__":
    unittest.main()
