# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline contracts for native runtime workload authentication."""

import base64
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
HERMES_AUTH = ROOT / "app/cetirun-chart/agents/hermes/compat/workload_mcp.py"
HERMES_INSTALLER = ROOT / "app/cetirun-chart/agents/hermes/compat/apply_workload_mcp.py"
OPENCLAW_AUTH = ROOT / "app/cetirun-chart/agents/openclaw/compat/workload-mcp.mjs"
OPENCLAW_INSTALLER = ROOT / "app/cetirun-chart/agents/openclaw/compat/apply-workload-mcp.mjs"
TARGET = "https://cetirun-connections-mcp.cetirun-tools.svc.cluster.local/mcp"


def synthetic_token(expiration):
    payload = base64.urlsafe_b64encode(
        json.dumps({"exp": expiration}, separators=(",", ":")).encode("ascii")
    ).decode("ascii").rstrip("=")
    return f"synthetic.{payload}.signature"


class FakeHTTPX:
    class Auth:
        pass


class FakeRequest:
    def __init__(self, url):
        self.url = url
        self.headers = {}


class NativeMCPRuntimeTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location("workload_mcp", HERMES_AUTH)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        self.addCleanup(sys.modules.pop, spec.name, None)
        spec.loader.exec_module(module)
        self.hermes = module

    def test_hermes_reads_the_rotated_token_for_every_request(self):
        with tempfile.TemporaryDirectory() as directory:
            token_path = Path(directory) / "token"
            with (
                mock.patch.dict(
                    os.environ, {"CETIRUN_CONNECTIONS_MCP_URL": TARGET}, clear=True
                ),
                mock.patch.object(self.hermes, "TOKEN_PATH", token_path),
            ):
                auth = self.hermes.workload_auth(FakeHTTPX, TARGET)
                token_path.write_text(synthetic_token(time.time() + 60), encoding="ascii")
                first = FakeRequest(TARGET)
                next(auth.auth_flow(first))
                token_path.write_text(synthetic_token(time.time() + 120), encoding="ascii")
                second = FakeRequest(TARGET)
                next(auth.auth_flow(second))
        self.assertNotEqual(first.headers["Authorization"], second.headers["Authorization"])

    def test_hermes_fails_closed_for_missing_expired_and_wrong_endpoint_tokens(self):
        with tempfile.TemporaryDirectory() as directory:
            token_path = Path(directory) / "token"
            with (
                mock.patch.dict(
                    os.environ, {"CETIRUN_CONNECTIONS_MCP_URL": TARGET}, clear=True
                ),
                mock.patch.object(self.hermes, "TOKEN_PATH", token_path),
            ):
                auth = self.hermes.workload_auth(FakeHTTPX, TARGET)
                with self.assertRaisesRegex(ValueError, "Workload identity is unavailable"):
                    next(auth.auth_flow(FakeRequest(TARGET)))
                token_path.write_text(synthetic_token(time.time() - 1), encoding="ascii")
                with self.assertRaisesRegex(ValueError, "Workload identity is unavailable"):
                    next(auth.auth_flow(FakeRequest(TARGET)))
                token_path.write_text(synthetic_token(time.time() + 60), encoding="ascii")
                with self.assertRaisesRegex(ValueError, "cannot leave the platform MCP endpoint"):
                    next(auth.auth_flow(FakeRequest(TARGET + "/unexpected")))

    def test_openclaw_reads_rotated_tokens_and_rejects_redirects(self):
        with tempfile.TemporaryDirectory() as directory:
            temporary = Path(directory)
            token_path = temporary / "token"
            module_path = temporary / "workload-mcp.mjs"
            module_path.write_text(
                OPENCLAW_AUTH.read_text(encoding="utf-8").replace(
                    "/var/run/cetirun/connections/token", str(token_path)
                ),
                encoding="utf-8",
            )
            runner = temporary / "runtime-test.mjs"
            runner.write_text(
                """
import { workloadFetch } from './workload-mcp.mjs';
import { existsSync, writeFileSync } from 'node:fs';
const target = process.env.CETIRUN_CONNECTIONS_MCP_URL;
const seen = [];
const fetchFn = async (_input, init) => { seen.push({ authorization: init.headers.get('authorization'), redirect: init.redirect }); return { status: 200 }; };
const guarded = workloadFetch(fetchFn, target);
await guarded(target);
writeFileSync(process.env.CETIRUN_TEST_READY, 'ready');
while (!existsSync(process.env.CETIRUN_TEST_CONTINUE)) await new Promise((resolve) => setTimeout(resolve, 5));
await guarded(target);
if (seen.length !== 2 || seen[0].authorization === seen[1].authorization || seen.some((request) => request.redirect !== 'error')) throw new Error('rotation or redirect policy failed');
await rejects(() => guarded(target + '/unexpected'), 'Workload identity cannot leave the platform MCP endpoint');
await rejects(() => workloadFetch(async () => ({ status: 302 }), target)(target), 'Platform MCP redirects are forbidden');
async function rejects(action, expected) { try { await action(); } catch (error) { if (error.message === expected) return; throw error; } throw new Error(`expected rejection: ${expected}`); }
""".strip(),
                encoding="utf-8",
            )
            token_path.write_text(synthetic_token(time.time() + 60), encoding="ascii")
            ready_path = temporary / "ready"
            continue_path = temporary / "continue"
            process = subprocess.Popen(
                ["node", str(runner)], cwd=temporary,
                env={**os.environ, "CETIRUN_CONNECTIONS_MCP_URL": TARGET, "CETIRUN_TEST_READY": str(ready_path), "CETIRUN_TEST_CONTINUE": str(continue_path)},
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            )
            for _ in range(100):
                if ready_path.exists():
                    break
                time.sleep(0.01)
            self.assertTrue(
                ready_path.exists(), "OpenClaw test did not make its first request"
            )
            token_path.write_text(synthetic_token(time.time() + 120), encoding="ascii")
            continue_path.write_text("continue", encoding="ascii")
            stdout, stderr = process.communicate(timeout=10)
            self.assertEqual(process.returncode, 0, f"{stdout}\n{stderr}")

    def test_installers_pin_known_sources_and_guard_native_transports(self):
        hermes = HERMES_INSTALLER.read_text(encoding="utf-8")
        openclaw = OPENCLAW_INSTALLER.read_text(encoding="utf-8")
        self.assertIn("EXPECTED_SHA256", hermes)
        self.assertIn(
            '"follow_redirects": not getattr(oauth_auth, "is_cetirun_workload", False)',
            hermes,
        )
        self.assertIn('config.get("ssl_verify", True) is not True', hermes)
        self.assertIn('config.get("transport") == "sse"', hermes)
        self.assertIn(
            'throw new Error("Workload identity is unavailable")',
            OPENCLAW_AUTH.read_text(encoding="utf-8"),
        )
        self.assertIn('redirect: "error"', OPENCLAW_AUTH.read_text(encoding="utf-8"))
        self.assertIn('createHash("sha256")', openclaw)
        self.assertIn("workloadFetch(httpFetch, resolved.url)", openclaw)
        self.assertIn('request.init?.redirect === "error" ? 0', openclaw)
        self.assertIn('resolved.sslVerify === false', openclaw)
        self.assertIn('resolved.transportType !== "streamable-http"', openclaw)
        self.assertIn("--check", openclaw)

    def test_hermes_platform_ca_extends_default_trust_only_for_platform_endpoint(self):
        context = mock.Mock()
        with mock.patch.dict(os.environ, {"CETIRUN_CONNECTIONS_MCP_URL": TARGET}, clear=True), mock.patch.object(self.hermes.ssl, "create_default_context", return_value=context) as factory:
            self.assertIs(self.hermes.workload_tls(TARGET), context)
            factory.assert_called_once_with()
            context.load_verify_locations.assert_called_once_with(cafile="/var/run/cetirun/connections-ca/ca.crt")
            self.assertIs(self.hermes.workload_tls("https://other.example.invalid/mcp", True), True)
            with self.assertRaisesRegex(ValueError, "requires verified TLS"):
                self.hermes.workload_tls(TARGET, False)
            context.load_verify_locations.side_effect = OSError("synthetic")
            with self.assertRaisesRegex(ValueError, "CA trust is unavailable"):
                self.hermes.workload_tls(TARGET)


if __name__ == "__main__":
    unittest.main()
