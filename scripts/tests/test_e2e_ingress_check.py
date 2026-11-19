# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Native ingress checks use synthetic responses and ephemeral loopback TLS."""

import contextlib
import http.client
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch
from urllib.parse import urlencode


SCRIPT = Path(__file__).resolve().parents[1] / "e2e-ingress-check.py"
SPEC = importlib.util.spec_from_file_location("e2e_ingress_check", SCRIPT)
check = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(check)
ORIGIN = "https://portal.example.com"
CLIENT = "00000000-0000-4000-8000-000000000001"
SENTINEL = "synthetic-private-response-must-not-print"


def challenge(origin=ORIGIN, **overrides):
    query = {"client_id": CLIENT, "response_type": "code",
             "redirect_uri": origin + "/oauth2/callback", "state": SENTINEL}
    query.update(overrides)
    return "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?" + urlencode(query, doseq=True)


def arguments(origin=ORIGIN):
    return ["--origin", origin, "--agent-name", "synthetic-agent", "--client-id", CLIENT]


class Response:
    def __init__(self, status=302, headers=None):
        self.status = status
        self.headers = headers if headers is not None else [("Location", challenge())]
        self.msg = http.client.HTTPMessage()
        self.closed = False

    def getheaders(self):
        return self.headers

    def close(self):
        self.closed = True

    def read(self, *args):
        raise AssertionError("Challenge inspection must not read response bodies")


class Connection:
    def __init__(self, response):
        self.response = response
        self.requests = []
        self.closed = False

    def request(self, *args, **kwargs):
        self.requests.append((args, kwargs))

    def getresponse(self):
        return self.response

    def close(self):
        self.closed = True


class IngressCheckTests(unittest.TestCase):
    def invoke(self, args, *, response=None, error=None):
        connection = Connection(response or Response())
        with patch.object(check, "supervised_probe", side_effect=check.inspect), \
                patch.object(check.http.client, "HTTPSConnection", return_value=connection) as factory, \
                contextlib.redirect_stdout(io.StringIO()) as stdout, \
                contextlib.redirect_stderr(io.StringIO()) as stderr:
            if error is None:
                code = check.main(args)
            else:
                with patch.object(connection, "getresponse", side_effect=error):
                    code = check.main(args)
        raw = stdout.getvalue()
        self.assertLess(len(raw), 512)
        self.assertNotIn(SENTINEL, raw + stderr.getvalue())
        self.assertNotIn("portal.example.com", raw)
        result = json.loads(raw)
        self.assertIs(result["qualifiesExecution"], False)
        self.assertEqual(result["authenticatedAccess"], "not-tested")
        return code, result, connection, factory

    def test_exact_native_path_uses_verified_tls_without_credentials_or_redirects(self):
        response = Response(headers=[("Location", challenge()), ("Set-Cookie", SENTINEL)])
        code, result, connection, factory = self.invoke(arguments(), response=response)
        self.assertEqual(code, 0)
        self.assertEqual(result["status"], "observed")
        self.assertEqual(connection.requests, [(("GET", "/ui/synthetic-agent/"), {
            "headers": {"Accept": "text/html", "User-Agent": "cetirun-e2e-ingress-check", "Connection": "close"},
        })])
        args, kwargs = factory.call_args
        self.assertEqual(args, ("portal.example.com", 443))
        self.assertEqual(kwargs["timeout"], 10)
        self.assertTrue(kwargs["context"].check_hostname)
        self.assertEqual(kwargs["context"].verify_mode, ssl.CERT_REQUIRED)
        self.assertTrue(connection.closed)
        self.assertTrue(response.closed)
        self.assertEqual(factory.call_count, 1)

    def test_nonchallenge_status_never_counts_as_authenticated_access(self):
        for status in (200, 301, 303, 307, 308, 400, 401, 403, 404, 426, 500, 503):
            with self.subTest(status=status):
                code, result, connection, _ = self.invoke(arguments(), response=Response(status))
                self.assertEqual(code, 1)
                self.assertEqual(result["reason"], "unexpected-http-status")
                self.assertTrue(connection.closed)

    def test_redirect_endpoint_must_be_exact_public_https_authorize_endpoint(self):
        valid = challenge()
        for location in (
            valid.replace("https://", "http://", 1),
            valid.replace("login.microsoftonline.com", "login.microsoftonline.com.example.com"),
            valid.replace("login.microsoftonline.com", "login.microsoftonline.com@outside.example.com"),
            valid.replace("login.microsoftonline.com", "user@login.microsoftonline.com"),
            valid.replace("login.microsoftonline.com", "login.microsoftonline.com:444"),
            valid.replace("/authorize?", "/logout?"),
            valid.replace("/oauth2/v2.0", "/oauth2/v1.0"),
            valid + "#fragment",
            "//login.microsoftonline.com/common/oauth2/v2.0/authorize",
            "/oauth2/start",
            valid.replace("common", "common/extra"),
            valid.replace("/common/", "/./"),
            valid.replace("/common/", "/../"),
            valid.replace("/common/", "/%63ommon/"),
            valid + "\r\n" + SENTINEL,
            valid.replace("https://", "https:\\\\", 1),
        ):
            with self.subTest(location=location):
                code, result, connection, factory = self.invoke(arguments(), response=Response(headers=[("Location", location)]))
                self.assertEqual(code, 1)
                self.assertEqual(result["status"], "blocked")
                self.assertEqual(len(connection.requests), 1)
                self.assertEqual(factory.call_count, 1)

    def test_client_callback_and_response_type_cannot_be_missing_ambiguous_or_changed(self):
        for changes in (
            {"client_id": CLIENT + "-suffix"},
            {"client_id": [CLIENT, CLIENT]},
            {"client_id": ""},
            {"response_type": ["code", "token"]},
            {"response_type": "token"},
            {"redirect_uri": ORIGIN + "/other"},
            {"redirect_uri": "https://outside.example.com/oauth2/callback"},
            {"redirect_uri": [ORIGIN + "/oauth2/callback"] * 2},
            {"redirect_uri": ORIGIN + "/oauth2/callback?unexpected=true"},
        ):
            with self.subTest(changes=changes):
                code, _, _, _ = self.invoke(arguments(), response=Response(headers=[("Location", challenge(**changes))]))
                self.assertEqual(code, 1)

    def test_missing_duplicate_oversized_and_malformed_headers_fail(self):
        for headers in (
            [],
            [("Location", challenge()), ("location", challenge())],
            [("Location", "x" * (check.MAX_LOCATION_BYTES + 1))],
            [("Location", challenge()), ("X-Synthetic", "x" * check.MAX_HEADERS_BYTES)],
            [("Location", challenge() + "&" + "&".join(f"a{i}=x" for i in range(40)))],
            [("Location", challenge() + "&missing-equals")],
        ):
            with self.subTest(headers=len(headers)):
                code, _, _, _ = self.invoke(arguments(), response=Response(headers=headers))
                self.assertEqual(code, 1)

    def test_raw_parser_defects_cannot_hide_duplicate_or_oversized_headers(self):
        for suffix in (
            b"Location: " + challenge().encode() + b"\r\n",
            b"X-Synthetic: " + b"x" * check.MAX_HEADERS_BYTES + b"\r\n",
        ):
            with self.subTest(suffix=suffix[:10]):
                raw = b"HTTP/1.1 302 Found\r\nLocation: " + challenge().encode() + \
                    b"\r\nbad header: value\r\n" + suffix + b"\r\n"

                class Socket:
                    def makefile(self, _mode):
                        return io.BytesIO(raw)

                response = http.client.HTTPResponse(Socket())
                response.begin()
                self.assertTrue(response.msg.defects)
                code, result, _, _ = self.invoke(arguments(), response=response)
                self.assertEqual(code, 1)
                self.assertEqual(result["reason"], "invalid-response-headers")

    def test_inputs_reject_credentials_paths_or_implicit_targets_before_network(self):
        for origin in (
            "", "http://portal.example.com", ORIGIN + "/ui/other", ORIGIN + "?token=" + SENTINEL,
            "https://user:password@portal.example.com", ORIGIN + "#fragment", "//portal.example.com",
            "https://portal.example.com:0", "https://portal.example.com:65536",
            "https://portal.example.com:bad", "https://portal.example.com\\other",
            "https://portal.example.com\n", "https://%70ortal.example.com",
        ):
            with self.subTest(origin=origin):
                code, _, _, factory = self.invoke(arguments(origin))
                self.assertEqual(code, 2)
                factory.assert_not_called()
        for suffix in (["--agent-name", "../other"], ["--client-id", SENTINEL],
                       ["--ca-file", "relative.pem"], ["--unexpected", SENTINEL]):
            with self.subTest(suffix=suffix):
                code, _, _, factory = self.invoke(arguments() + suffix)
                self.assertEqual(code, 2)
                factory.assert_not_called()

    def test_expected_tls_timeout_http_and_io_errors_are_sanitized(self):
        for error, reason in (
            (ssl.SSLCertVerificationError(SENTINEL), "tls-verification-failed"),
            (ssl.SSLError(SENTINEL), "tls-connection-failed"),
            (TimeoutError(SENTINEL), "deadline-exceeded"),
            (OSError(SENTINEL), "request-failed"),
            (http.client.BadStatusLine(SENTINEL), "request-failed"),
        ):
            with self.subTest(error=type(error).__name__):
                code, result, connection, _ = self.invoke(arguments(), error=error)
                self.assertEqual(code, 1)
                self.assertEqual(result["reason"], reason)
                self.assertTrue(connection.closed)

    def test_ambient_tls_key_logging_is_not_enabled(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-no-keylog-") as directory:
            keylog = Path(directory) / "must-not-create"
            with patch.dict(os.environ, {"SSLKEYLOGFILE": str(keylog)}):
                code, _, _, factory = self.invoke(arguments())
            self.assertEqual(code, 0)
            self.assertIsNone(factory.call_args.kwargs["context"].keylog_filename)
            self.assertFalse(keylog.exists())

    def test_cancellation_has_explicit_exit_and_restores_handlers(self):
        for error, code in ((KeyboardInterrupt(), 130), (check.Terminated("cancelled"), 143)):
            with self.subTest(code=code):
                alarm, term = signal.getsignal(signal.SIGALRM), signal.getsignal(signal.SIGTERM)
                actual, result, _, _ = self.invoke(arguments(), error=error)
                self.assertEqual(actual, code)
                self.assertEqual(result["reason"], "cancelled")
                self.assertEqual(signal.getsignal(signal.SIGALRM), alarm)
                self.assertEqual(signal.getsignal(signal.SIGTERM), term)
                self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))

    def test_absolute_deadline_interrupts_a_slow_header_response(self):
        connection = Connection(Response())
        with patch.object(check, "TOTAL_TIMEOUT", 0.05), \
                patch.object(check.http.client, "HTTPSConnection", return_value=connection), \
                patch.object(connection, "getresponse", side_effect=lambda: time.sleep(5)), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            start = time.monotonic()
            code = check.main(arguments())
            elapsed = time.monotonic() - start
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(output.getvalue())["reason"], "deadline-exceeded")
        self.assertLess(elapsed, 1)

    def test_worker_failure_is_sanitized_and_signal_handlers_are_restored(self):
        previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}
        with patch.object(check, "inspect", side_effect=RuntimeError(SENTINEL)), \
                contextlib.redirect_stdout(io.StringIO()) as stdout, \
                contextlib.redirect_stderr(io.StringIO()) as stderr:
            code = check.main(arguments())
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(stdout.getvalue())["reason"], "worker-failed")
        self.assertNotIn(SENTINEL, stdout.getvalue() + stderr.getvalue())
        for sig, handler in previous.items():
            self.assertEqual(signal.getsignal(sig), handler)

    def test_unavailable_process_boundary_fails_before_network(self):
        with patch.object(check.multiprocessing, "get_all_start_methods", return_value=[]), \
                patch.object(check.http.client, "HTTPSConnection") as factory, \
                contextlib.redirect_stdout(io.StringIO()) as output:
            code = check.main(arguments())
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(output.getvalue())["reason"], "deadline-unavailable")
        factory.assert_not_called()

    def test_worker_start_failure_is_sanitized_and_signal_handlers_are_restored(self):
        previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}
        context = check.multiprocessing.get_context("fork")
        with patch.object(context.Process, "start", side_effect=OSError(SENTINEL)), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            code = check.main(arguments())
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(output.getvalue())["reason"], "request-failed")
        self.assertNotIn(SENTINEL, output.getvalue())
        for sig, handler in previous.items():
            self.assertEqual(signal.getsignal(sig), handler)

    def test_cancellation_during_worker_start_still_reaps_the_worker(self):
        context = check.multiprocessing.get_context("fork")
        original_start = context.Process.start
        for signum, expected in ((signal.SIGINT, 130), (signal.SIGTERM, 143)):
            with self.subTest(signum=signum):
                pids = []
                previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}

                def start(worker):
                    original_start(worker)
                    pids.append(worker.pid)
                    os.kill(os.getpid(), signum)

                with patch.object(context.Process, "start", start), \
                        patch.object(check, "inspect", side_effect=lambda *args: time.sleep(5)), \
                        contextlib.redirect_stdout(io.StringIO()) as output:
                    code = check.main(arguments())
                self.assertEqual(code, expected)
                self.assertEqual(json.loads(output.getvalue())["reason"], "cancelled")
                self.assertEqual(len(pids), 1)
                with self.assertRaises(ChildProcessError):
                    os.waitpid(pids[0], os.WNOHANG)
                for sig, handler in previous.items():
                    self.assertEqual(signal.getsignal(sig), handler)

    def test_help_does_not_start_a_worker(self):
        with patch.object(check, "supervised_probe") as probe, \
                contextlib.redirect_stdout(io.StringIO()) as output, \
                self.assertRaises(SystemExit) as exited:
            check.main(["--help"])
        self.assertEqual(exited.exception.code, 0)
        self.assertIn("--origin", output.getvalue())
        probe.assert_not_called()


@unittest.skipUnless(shutil.which("bash") and shutil.which("jq"), "Bash and jq are required")
class IngressRunnerTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-ingress-runner-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.log = self.directory / "commands.jsonl"
        config = self.directory / "fixture-config"
        config.write_text("synthetic, not a kubeconfig")
        script = f"#!{sys.executable}\n" + r'''
import json,os,pathlib,sys
tool=pathlib.Path(sys.argv[0]).name
with open(os.environ["COMMAND_LOG"],"a") as output:
    output.write(json.dumps({"tool":tool,"args":sys.argv[1:]})+"\n")
if tool=="python3":
    print(json.dumps({"status":"observed","qualifiesExecution":False,"authenticatedAccess":"not-tested"}))
    sys.exit(int(os.environ.get("CHECK_EXIT","0")))
if tool=="kubectl":
    raise AssertionError("The routing check must not read an Agent Secret or exec")
assert tool=="helm"
args=sys.argv[1:]
assert args[:4]==["--kubeconfig",os.environ["E2E_KUBECONFIG"],"--kube-context","explicit-fixture"]
if os.environ.get("VALUES_MODE")=="unavailable":
    print("synthetic-private-response-must-not-print",file=sys.stderr)
    sys.exit(1)
if os.environ.get("VALUES_MODE")=="empty":
    print("{}")
elif args[4:7]==["get","values","cetirun-operator"]:
    print(json.dumps({"domainSuffix":"portal.example.com"}))
elif args[4:7]==["get","values","cetirun-istio"]:
    print(json.dumps({"clientId":"00000000-0000-4000-8000-000000000001"}))
else:
    raise AssertionError(args)
'''
        for name in ("kubectl", "helm", "python3"):
            path = self.directory / name
            path.write_text(script)
            path.chmod(0o700)
        self.environment = {**os.environ, "PATH": str(self.directory) + os.pathsep + os.environ["PATH"],
                            "E2E_KUBECONFIG": str(config), "E2E_CONTEXT": "explicit-fixture",
                            "E2E_CLUSTER_UID": "synthetic-cluster", "TEST_ALIAS": "synthetic-agent",
                            "COMMAND_LOG": str(self.log), "DOMAIN": "", "E2E_OIDC_CLIENT_ID": "",
                            "E2E_CA_FILE": ""}

    def invoke(self, **overrides):
        result = subprocess.run(["bash", "-c", 'source "$1"; validate_inputs; check_ingress',
                                 "fixture", str(SCRIPT.with_name("e2e-test.sh"))],
                                env={**self.environment, **overrides}, capture_output=True, text=True, timeout=10)
        calls = [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []
        self.assertNotIn(SENTINEL, result.stdout + result.stderr)
        self.assertTrue(all(call["tool"] != "kubectl" for call in calls))
        return result, calls

    def test_runner_passes_explicit_origin_agent_client_and_ca_without_credentials(self):
        ca = self.directory / "ca.pem"
        ca.write_text("synthetic CA path only; no network in this test")
        result, calls = self.invoke(DOMAIN="portal.example.com", E2E_OIDC_CLIENT_ID=CLIENT, E2E_CA_FILE=str(ca))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0]["tool"], "python3")
        self.assertEqual(calls[0]["args"], ["-B", str(SCRIPT), "--origin", ORIGIN,
                                          "--agent-name", "synthetic-agent", "--client-id", CLIENT,
                                          "--ca-file", str(ca)])

    def test_runner_uses_targeted_release_values_without_guessing_a_domain(self):
        result, calls = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([call["tool"] for call in calls], ["helm", "helm", "python3"])
        self.assertIn(ORIGIN, calls[-1]["args"])
        self.assertNotIn("https://synthetic-agent.portal.example.com", calls[-1]["args"])

    def test_unavailable_or_empty_release_values_fail_before_http(self):
        for mode in ("unavailable", "empty"):
            with self.subTest(mode=mode):
                self.log.unlink(missing_ok=True)
                result, calls = self.invoke(VALUES_MODE=mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(call["tool"] == "python3" for call in calls))

    def test_probe_failure_is_preserved(self):
        result, _ = self.invoke(DOMAIN="portal.example.com", E2E_OIDC_CLIENT_ID=CLIENT, CHECK_EXIT="1")
        self.assertEqual(result.returncode, 1)


@unittest.skipUnless(sys.platform.startswith("linux") and platform.libc_ver()[0] == "glibc" and
                     shutil.which("cc"), "Linux/glibc and a C compiler are required for native DNS tests")
class NativeDNSDeadlineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-ingress-dns-")
        cls.addClassCleanup(temporary.cleanup)
        cls.library = Path(temporary.name) / "stalled-dns.so"
        subprocess.run([
            "cc", "-shared", "-fPIC", "-Wall", "-Wextra", "-Werror",
            str(Path(__file__).with_name("fixtures") / "stalled_dns.c"),
            "-o", str(cls.library), "-lresolv",
        ], capture_output=True, text=True, timeout=15, check=True)

    def run_stalled_resolver(self, signum=None, deadline=1):
        wrapper = r'''
import ctypes, os, pathlib, runpy, socket, sys
probe = runpy.run_path(sys.argv[1])
probe["main"].__globals__["TOTAL_TIMEOUT"] = float(sys.argv[5])
native = ctypes.CDLL(sys.argv[2])
native.cetirun_test_dns.argtypes = [ctypes.c_ushort]
native.cetirun_test_dns.restype = ctypes.c_int
original = socket.getaddrinfo
def resolve(host, port, *args, **kwargs):
    assert host == "nonresponding.example.invalid"
    assert native.cetirun_test_dns(int(sys.argv[3])) == 0
    pathlib.Path(sys.argv[4]).write_text(str(os.getpid()))
    return original(host + ".", port, *args, **kwargs)
socket.getaddrinfo = resolve
sys.exit(probe["main"]([
    "--origin", "https://nonresponding.example.invalid", "--agent-name", "synthetic-agent",
    "--client-id", "00000000-0000-4000-8000-000000000001",
]))
'''
        with tempfile.TemporaryDirectory(prefix="cetirun-dns-worker-") as directory, \
                socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as dns:
            dns.bind(("127.0.0.1", 0))
            dns.settimeout(5)
            pid_file = Path(directory) / "worker.pid"
            started = time.monotonic()
            process = subprocess.Popen([
                sys.executable, "-B", "-c", wrapper, str(SCRIPT), str(self.library),
                str(dns.getsockname()[1]), str(pid_file), "30" if signum else str(deadline),
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
                stdout, stderr = process.communicate(timeout=budget)
                self.assertLess(time.monotonic() - started, budget)
                self.assertEqual(stderr, "")
                self.assertLess(len(stdout), 512)
                self.assertNotIn("nonresponding.example.invalid", stdout)
                result = json.loads(stdout)
                self.assertEqual(result["status"], "blocked")
                self.assertIs(result["qualifiesExecution"], False)
                self.assertEqual(result["authenticatedAccess"], "not-tested")
                self.assertNotEqual(worker_pid, process.pid)
                with self.assertRaises(ProcessLookupError):
                    os.kill(worker_pid, 0)
                return process.returncode, result["reason"]
            finally:
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGKILL)
                process.communicate(timeout=5)

    def test_native_getaddrinfo_deadline_kills_and_reaps_worker(self):
        self.assertEqual(check.TOTAL_TIMEOUT, 30)
        self.assertEqual(self.run_stalled_resolver(), (1, "deadline-exceeded"))

    def test_sigterm_during_native_getaddrinfo_kills_and_reaps_worker(self):
        self.assertEqual(self.run_stalled_resolver(signal.SIGTERM), (143, "cancelled"))

    def test_sigint_during_native_getaddrinfo_kills_and_reaps_worker(self):
        self.assertEqual(self.run_stalled_resolver(signal.SIGINT), (130, "cancelled"))


@unittest.skipUnless(shutil.which("openssl"), "OpenSSL is required for local TLS fixtures")
class LoopbackIngressTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-ingress-tls-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.cert, self.key = self.directory / "cert.pem", self.directory / "key.pem"
        generated = subprocess.run([
            "openssl", "req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
            "-nodes", "-x509", "-days", "1", "-subj", "/CN=localhost",
            "-addext", "subjectAltName=DNS:localhost",
            "-keyout", str(self.key), "-out", str(self.cert),
        ], capture_output=True, timeout=15, check=False)
        self.assertEqual(generated.returncode, 0, "Synthetic TLS certificate generation failed")
        self.key.chmod(0o600)
        self.requests = []
        requests = self.requests

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(inner):
                requests.append((inner.path, dict(inner.headers)))
                origin = f"https://localhost:{inner.server.server_port}"
                inner.send_response(302)
                inner.send_header("Location", challenge(origin))
                inner.send_header("Content-Length", "0")
                inner.end_headers()

            def log_message(self, *args):
                pass

        self.server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(self.cert, self.key)
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop)
        self.origin = f"https://localhost:{self.server.server_port}"

    def stop(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.assertFalse(self.thread.is_alive())

    def run_helper(self, origin, trusted):
        args = arguments(origin)
        if trusted:
            args += ["--ca-file", str(self.cert)]
        result = subprocess.run([sys.executable, "-B", str(SCRIPT), *args],
                                env={**os.environ, "HTTPS_PROXY": "http://127.0.0.1:1",
                                     "https_proxy": "http://127.0.0.1:1"},
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.stderr, "")
        self.assertNotIn(SENTINEL, result.stdout)
        self.assertNotIn("localhost", result.stdout)
        return result.returncode, json.loads(result.stdout)

    def test_actual_tls_challenge_and_no_redirect_or_ambient_proxy(self):
        code, result = self.run_helper(self.origin, True)
        self.assertEqual(code, 0)
        self.assertEqual(result["status"], "observed")
        self.assertEqual(len(self.requests), 1)
        path, headers = self.requests[0]
        self.assertEqual(path, "/ui/synthetic-agent/")
        for name in ("Authorization", "Cookie", "Proxy-Authorization"):
            self.assertNotIn(name, headers)

    def test_untrusted_certificate_is_not_accepted(self):
        code, result = self.run_helper(self.origin, False)
        self.assertEqual(code, 1)
        self.assertEqual(result["reason"], "tls-verification-failed")
        self.assertEqual(self.requests, [])

    def test_wrong_certificate_hostname_is_not_accepted(self):
        code, result = self.run_helper(self.origin.replace("localhost", "127.0.0.1"), True)
        self.assertEqual(code, 1)
        self.assertEqual(result["reason"], "tls-verification-failed")
        self.assertEqual(self.requests, [])


if __name__ == "__main__":
    unittest.main()
