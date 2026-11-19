# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Webapp startup and readiness contracts, using synthetic HOME and listener fixtures."""

import importlib.util
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import types
import unittest
from unittest.mock import patch

import yaml

CHART = Path(__file__).resolve().parents[1]
BOOTSTRAP = CHART / "agents/hermes/bootstrap"
spec = importlib.util.spec_from_file_location("hermes_runtime", BOOTSTRAP / "hermes-runtime.py")
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class WebappRuntimeTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name)
        self.home, self.config, self.image = root / "home/.hermes", root / "config", root / "image"
        self.managed = self.config / "hermes-managed"
        self.home.mkdir(parents=True)
        self.managed.mkdir(parents=True)
        self.image.mkdir()
        self.env = {
            "HERMES_HOME": str(self.home), "HERMES_MANAGED_DIR": str(self.managed),
            "HERMES_PYTHON": "/usr/local/bin/hermes-python", "API_SERVER_ENABLED": "true",
            "API_SERVER_HOST": "0.0.0.0", "API_SERVER_PORT": "8642", "CETIRUN_AI_GATEWAY": "true",
            "HERMES_WEBAPP_ENABLED": "true", "HERMES_WEBAPP_HOST": "0.0.0.0", "HERMES_WEBAPP_PORT": "9120",
            "HERMES_WEBAPP_PUBLIC_URL": "https://agents.example.com/ui/synthetic-agent",
            "HERMES_WEBAPP_DIST": str(self.image / "apps/desktop/dist-webapp"),
            "HERMES_DASHBOARD_BASIC_AUTH_USERNAME": "cetirun", "API_SERVER_KEY": "synthetic-api-key-" * 3,
            "CETIRUN_AUTH_KEY": "synthetic-signing-key-" * 3, "CETIRUN_GATEWAY_KEY_ENV": "CETIRUN_GATEWAY_API_KEY",
            "CETIRUN_GATEWAY_API_KEY": "sk-synthetic-model",
        }
        self.env.update(HERMES_DASHBOARD_BASIC_AUTH_PASSWORD=self.env["API_SERVER_KEY"],
                        HERMES_DASHBOARD_BASIC_AUTH_SECRET=self.env["CETIRUN_AUTH_KEY"])
        self.expected = {"enabled": True, "host": "0.0.0.0", "port": 9120,
                         "public_url": self.env["HERMES_WEBAPP_PUBLIC_URL"]}
        self.write_managed()
        self.home.joinpath(".env").write_text("")
        self.home.joinpath("config.yaml").write_text("preferences: {theme: dark}\n")
        self.gateway = {
            "model": {"default": "synthetic-model", "provider": "cetirun-gateway"},
            "providers": {"cetirun-gateway": {
                "api": "https://gateway.example.com/v1", "key_env": "CETIRUN_GATEWAY_API_KEY",
            }},
        }
        (self.config / "hermes-config.yaml").write_text(yaml.safe_dump(self.gateway))
        shutil.copy(BOOTSTRAP / "hermes-apply-config.py", self.config)
        for name, value in {"HOME": self.home, "CONFIG": self.config, "MANAGED": self.managed, "ROOT": self.image}.items():
            mock = patch.object(runtime, name, value)
            mock.start()
            self.addCleanup(mock.stop)
        env = patch.dict(os.environ, self.env, clear=True)
        env.start()
        self.addCleanup(env.stop)

    def write_managed(self, value=None):
        path = self.managed / "config.yaml"
        if path.exists():
            path.chmod(0o644)
        managed = {"webapp": self.expected if value is None else value}
        if self.expected["host"] == "0.0.0.0":
            managed["dashboard"] = {"trusted_proxies": ["127.0.0.6"]}
        path.write_text(yaml.safe_dump(managed))
        path.chmod(0o444)

    def test_contract_requires_exact_managed_types_and_readonly_file(self):
        self.assertEqual(runtime.contract(), self.expected)
        self.write_managed(self.expected | {"enabled": 1})
        with self.assertRaisesRegex(ValueError, "ManagedWebappTypeInvalid"):
            runtime.contract()
        self.write_managed()
        (self.managed / "config.yaml").chmod(0o644)
        with self.assertRaisesRegex(ValueError, "ManagedConfigWritable"):
            runtime.contract()

    def test_mtls_cannot_use_a_wildcard_api_or_webapp(self):
        os.environ["CETIRUN_INGRESS_MTLS"] = "true"
        with self.assertRaisesRegex(ValueError, "ManagedWebappInvalid"):
            runtime.contract()
        self.expected["host"] = "127.0.0.1"
        self.write_managed()
        with self.assertRaisesRegex(ValueError, "PlatformEnvironmentInvalid"):
            runtime.contract()
        os.environ.update(API_SERVER_HOST="127.0.0.1", HERMES_WEBAPP_HOST="127.0.0.1")
        self.assertEqual(runtime.contract(), self.expected)

    def test_missing_or_shared_auth_keys_never_fall_back(self):
        for name in ("API_SERVER_KEY", "CETIRUN_AUTH_KEY", "HERMES_DASHBOARD_BASIC_AUTH_PASSWORD",
                     "HERMES_DASHBOARD_BASIC_AUTH_SECRET"):
            with self.subTest(name=name), patch.dict(os.environ, {name: ""}):
                with self.assertRaises(ValueError):
                    runtime.contract()
        with patch.dict(os.environ, {"CETIRUN_AUTH_KEY": self.env["API_SERVER_KEY"]}):
            with self.assertRaisesRegex(ValueError, "SigningKeyInvalid"):
                runtime.contract()

    def test_external_secret_bindings_cannot_override_platform_or_execute_bulk_helpers(self):
        original = {"secrets": {"onepassword": {"enabled": True, "env": {"UNRELATED_SERVICE_TOKEN": "op://example/item/token"}}}}
        runtime.validate_secret_sources(original)
        self.assertEqual(original["secrets"]["onepassword"]["env"], {"UNRELATED_SERVICE_TOKEN": "op://example/item/token"})
        for variable in ("API_SERVER_HOST", "HERMES_CONFIG_PATH", "HERMES_DASHBOARD_BASIC_AUTH_PASSWORD",
                         "CETIRUN_GATEWAY_API_KEY", "CETIRUN_AUTH_KEY"):
            with self.subTest(variable=variable):
                settings = {"enabled": True, "override_existing": True, "env": {variable: "op://example/item/token"}}
                with self.assertRaisesRegex(ValueError, "SecretSourcePlatformOverride"):
                    runtime.validate_secret_sources({"secrets": {"onepassword": settings}})
        for source in ("bitwarden", "command", "unqualified_plugin"):
            with self.subTest(source=source):
                with self.assertRaisesRegex(ValueError, "SecretSourceOutputsUnbounded"):
                    runtime.validate_secret_sources({"secrets": {source: {"enabled": True}}})
        with self.assertRaisesRegex(ValueError, "SecretSourcePlatformCredentialInvalid"):
            runtime.validate_secret_sources({"secrets": {"onepassword": {
                "enabled": True, "service_account_token_env": "API_SERVER_KEY", "env": {},
            }}})

    def test_process_binding_wins_over_retained_home(self):
        api = {"enabled": False, "host": "::", "port": 18000, "key": "synthetic-override",
               "extra": {"host": "::", "port": 18001, "key": "synthetic-override", "model_name": "retained"}}
        source = {"api_server": api, "platforms": {"api_server": api},
                  "gateway": {"api_server": api, "platforms": {"api_server": api}}, "preferences": {"theme": "dark"}}
        env = '# retained comment\nexport HERMES_MANAGED_DIR="/tmp/bypass"\nHERMES_DASHBOARD_BASIC_AUTH_PASSWORD="line1\nline2"\nAPI_SERVER_PORT=8642\nCETIRUN_GATEWAY_API_KEY=sk-override\nUSER_SETTING="keep me"\n'
        for port in ("8080", "8642"):
            with self.subTest(port=port), patch.dict(os.environ, {"API_SERVER_PORT": port}):
                (self.home / "config.yaml").write_text(yaml.safe_dump(source))
                (self.home / "gateway.json").write_text(json.dumps(source))
                (self.home / ".env").write_text(env)
                runtime.remove_home_overrides()
                self.assertEqual(runtime.contract(), self.expected)
                self.assertEqual(runtime.api_port(), int(port))
                self.assertEqual(os.environ["API_SERVER_PORT"], port)
                self.assertEqual((self.home / ".env").read_text(), '# retained comment\nUSER_SETTING="keep me"\n')
                for relative in ("config.yaml", "gateway.json"):
                    actual = yaml.safe_load((self.home / relative).read_text())
                    self.assertEqual(actual["preferences"], {"theme": "dark"})
                    for row in (actual["api_server"], actual["platforms"]["api_server"], actual["gateway"]["api_server"],
                                actual["gateway"]["platforms"]["api_server"]):
                        self.assertEqual(row, {"extra": {"model_name": "retained"}})

    def test_restart_restores_platform_model_and_provider(self):
        path = self.home / "config.yaml"
        current = yaml.safe_load(path.read_text())
        current.update(model={"default": "user-model", "provider": "user-provider"},
                       providers={"user-provider": {"api": "https://models.example.com/v1"}})
        path.write_text(yaml.safe_dump(current))
        runtime.remove_home_overrides()
        actual = yaml.safe_load(path.read_text())
        self.assertEqual(actual["model"], self.gateway["model"])
        self.assertEqual(actual["providers"], self.gateway["providers"])
        self.assertEqual(actual["preferences"], {"theme": "dark"})

    def test_readiness_requires_live_socket_provider_and_private_persistent_token(self):
        os.environ["API_SERVER_PORT"] = "8080"
        dist = self.image / "apps/desktop/dist-webapp"
        dist.mkdir(parents=True)
        for name in ("index.html", "hermes-build.json", "renderer-manifest.json"):
            (dist / name).write_text("{}")
        folder = self.home / "webapp"
        folder.mkdir(mode=0o700)
        token = folder / "session-token"
        token.write_text("a" * 43 + "\n")
        token.chmod(0o600)
        original = token.read_bytes()
        status = {"ok": True, "result": {"state": "serving", "url": "http://127.0.0.1:9120"}}
        fake = types.SimpleNamespace(private_control_query=lambda *_, **_kwargs: json.dumps(status).encode())
        with patch.dict("sys.modules", {"hermes_cli.gateway_client": fake}), patch.object(runtime, "get_json") as get:
            get.side_effect = [{"status": "ok"}, {"providers": [{"name": "basic", "supports_password": True}]}]
            runtime.readiness(1)
            self.assertEqual(get.call_args_list[0].args[0:2], (8080, "/health"))
            self.assertEqual(token.read_bytes(), original)
            status["result"]["state"] = "unavailable"
            with self.assertRaisesRegex(ValueError, "WebappNotServing"):
                runtime.readiness(1)
            status["result"]["state"] = "serving"
            get.side_effect = [{"status": "ok"}, {"providers": []}]
            with self.assertRaisesRegex(ValueError, "LivePasswordProviderMissing"):
                runtime.readiness(1)
            token.chmod(0o644)
            with self.assertRaisesRegex(ValueError, "BrowserCredentialPermissionsInvalid"):
                runtime.readiness(1)

        with patch.dict(os.environ, {"API_SERVER_PORT": "8080"}, clear=True), \
                patch.object(runtime.sys, "argv", ["hermes-runtime.py", "liveness"]), patch.object(runtime, "get_json") as get:
            self.assertEqual(runtime.main(), 0)
            self.assertEqual(get.call_args.args[0:2], (8080, "/health"))

    def test_mtls_and_egress_helpers_gate_health_with_bounded_checks(self):
        runtime.helper_health(1)
        for flag, filename, code in (
            ("CETIRUN_INGRESS_MTLS", "ingress-health.cjs", "IngressTLSNotReady"),
            ("CETIRUN_EGRESS_SNI", "proxy-health.cjs", "EgressProxyNotReady"),
        ):
            with self.subTest(flag=flag), patch.dict(os.environ, {flag: "true"}), patch.object(runtime.subprocess, "run") as run:
                run.return_value = subprocess.CompletedProcess([], 0)
                runtime.helper_health(0.5)
                self.assertEqual(run.call_args.args[0][-1], str(self.config / filename))
                self.assertEqual(run.call_args.kwargs["timeout"], 0.5)
                run.return_value = subprocess.CompletedProcess([], 1)
                with self.assertRaisesRegex(ValueError, code):
                    runtime.helper_health(0.5)
                run.side_effect = subprocess.TimeoutExpired("node", 0.5)
                with self.assertRaisesRegex(ValueError, code):
                    runtime.helper_health(0.5)


class WebappChartTests(unittest.TestCase):
    def render(self, *extra):
        return subprocess.run([
            "helm", "template", "synthetic-agent", str(CHART), "--set", "aiGateway.enabled=true",
            "--set", "aiGateway.keySecretName=synthetic-model-key", "--set", "secrets.model=synthetic-model",
            "--set", "secrets.apiBase=http://gateway.example.com/v1",
            "--set", "hermes.env.HERMES_WEBAPP_PUBLIC_URL=https://agents.example.com/ui/synthetic-agent", *extra,
        ], capture_output=True, text=True)

    def test_chart_renders_managed_mount_gateway_launcher_and_full_readiness(self):
        result = self.render()
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = list(yaml.safe_load_all(result.stdout))
        config = next(d for d in docs if d and d["kind"] == "ConfigMap" and "hermes-managed.yaml" in d["data"])
        managed = yaml.safe_load(config["data"]["hermes-managed.yaml"])
        self.assertEqual(managed["dashboard"], {"trusted_proxies": ["127.0.0.6"]})
        pod = next(d for d in docs if d and d["kind"] == "StatefulSet")["spec"]["template"]["spec"]
        main = pod["containers"][0]
        self.assertIn({"name": "hermes-managed", "mountPath": "/etc/cetirun/hermes-managed", "readOnly": True}, main["volumeMounts"])
        for probe in ("startupProbe", "livenessProbe"):
            self.assertEqual(main[probe]["exec"]["command"],
                             ["node", "/etc/cetirun/hermes-health-probe.cjs", "--timeout",
                              str(main[probe]["timeoutSeconds"])])
        self.assertEqual(main["readinessProbe"]["exec"]["command"],
                         ["/usr/local/bin/hermes-python", "/etc/cetirun/hermes-runtime.py", "readiness", "--timeout",
                          str(main["readinessProbe"]["timeoutSeconds"])])
        self.assertEqual(config["data"]["hermes-health-probe.cjs"], (BOOTSTRAP / "hermes-health-probe.cjs").read_text())
        self.assertIn({"name": "webapp", "containerPort": 9120, "protocol": "TCP"}, main["ports"])
        self.assertNotIn(9119, [port["containerPort"] for port in main["ports"]])

    def test_old_and_unmanaged_binding_inputs_are_rejected(self):
        for override in ("hermes.env.HERMES_DASHBOARD_PUBLIC_URL=https://example.com", "hermes.env.API_SERVER_PORT=9120",
                         "extraEnv[0].name=HERMES_MANAGED_DIR,extraEnv[0].value=/tmp/bypass", "hermes.config.webapp.port=9121"):
            with self.subTest(override=override):
                self.assertNotEqual(self.render("--set", override).returncode, 0)

    def test_external_sources_cannot_hydrate_reserved_environment(self):
        for override in (
            "hermes.config.secrets.onepassword.env.API_SERVER_KEY=op://example/item/key",
            "hermes.config.secrets.onepassword.env.HERMES_PROFILE_NAME=op://example/item/profile",
            "hermes.config.secrets.onepassword.service_account_token_env=CETIRUN_AUTH_KEY",
            "hermes.config.secrets.command.enabled=true",
            "hermes.config.secrets.bitwarden.enabled=true",
            "aiGateway.hermes.keyEnv=EXAMPLE_MODEL_API_KEY,hermes.config.secrets.onepassword.env.EXAMPLE_MODEL_API_KEY=op://example/item/key",
        ):
            with self.subTest(override=override):
                self.assertNotEqual(self.render("--set", override).returncode, 0)
        permitted = self.render("--set", "hermes.config.secrets.onepassword.enabled=true,hermes.config.secrets.onepassword.env.EXAMPLE_SERVICE_TOKEN=op://example/item/token")
        self.assertEqual(permitted.returncode, 0, permitted.stderr)


@unittest.skipUnless(shutil.which("node"), "requires Node for the Hermes health probe")
class HermesHealthProbeTests(unittest.TestCase):
    def setUp(self):
        self.status, self.body, self.stall = 200, b'{"status":"ok"}', False
        self.requests = []
        self.release = threading.Event()
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                fixture.requests.append((self.path, self.headers))
                self.send_response(fixture.status)
                self.send_header("Content-Length", str(len(fixture.body)))
                self.end_headers()
                if fixture.stall:
                    fixture.release.wait(timeout=5)
                    return
                self.wfile.write(fixture.body)

            def log_message(self, *_args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop_server)

    def stop_server(self):
        self.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        self.assertFalse(self.thread.is_alive())

    def probe(self, *, args=None, env=None, helpers=None):
        command = [shutil.which("node")]
        if helpers is not None:
            # Only the existing Envoy helpers are synthetic; HTTP and the probe run unchanged.
            command += ["-e", """
const Module = require('node:module');
const original = Module._load;
const helpers = JSON.parse(process.env.TEST_HELPERS);
Module._load = function(name, ...args) {
  if (Object.hasOwn(helpers, name)) return (callback) => {
    console.error(name);
    if (helpers[name] === 'throw') throw new Error('synthetic helper failure');
    if (helpers[name] !== 'stall') setTimeout(() => callback(helpers[name] === 'ready'), 25);
  };
  return original.call(this, name, ...args);
};
require(process.argv[1]);
"""]
        command += [str(BOOTSTRAP / "hermes-health-probe.cjs"), *(args if args is not None else ["--timeout", "1"])]
        return subprocess.run(command, env={
            **os.environ, "PATH": "", "API_SERVER_PORT": str(self.server.server_port),
            "CETIRUN_EGRESS_SNI": "false", "CETIRUN_INGRESS_MTLS": "false",
            "TEST_HELPERS": json.dumps(helpers), **(env or {}),
        }, capture_output=True, text=True, timeout=30, check=False)

    def test_loopback_json_health_needs_no_python_or_credentials_and_ignores_proxy(self):
        result = self.probe(env={"HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1",
                                 "ALL_PROXY": "http://127.0.0.1:1", "NODE_USE_ENV_PROXY": "1",
                                 "API_SERVER_KEY": "sk-dummy", "HERMES_PYTHON": "/missing-python"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "")
        path, headers = self.requests[0]
        self.assertEqual(path, "/health")
        self.assertEqual(headers["Host"], f"127.0.0.1:{self.server.server_port}")
        self.assertIsNone(headers["Authorization"])
        self.assertIsNone(headers["Cookie"])

    def test_requires_http_200_and_complete_valid_json(self):
        for status, body in ((204, b""), (302, b"{}"), (503, b"{}"), (200, b""),
                             (200, b"<html>not health</html>"), (200, b'{"status":')):
            with self.subTest(status=status, body=body):
                self.status, self.body = status, body
                result = self.probe()
                self.assertEqual(result.returncode, 1)
                self.assertIn("HTTPProbeFailed", result.stderr)

    def test_stalled_response_is_bounded(self):
        self.stall = True
        result = self.probe()
        self.assertEqual(result.returncode, 1)
        self.assertIn("ProbeTimeout", result.stderr)

    def test_enabled_helpers_are_required_and_share_the_deadline(self):
        proxy, ingress = "/etc/cetirun/proxy-health.cjs", "/etc/cetirun/ingress-health.cjs"
        env = {"CETIRUN_EGRESS_SNI": "true", "CETIRUN_INGRESS_MTLS": "true"}
        for helpers, expected in (
            ({proxy: "ready", ingress: "ready"}, None),
            ({proxy: "failed", ingress: "ready"}, "EgressProxyNotReady"),
            ({proxy: "ready", ingress: "failed"}, "IngressTLSNotReady"),
            ({proxy: "throw", ingress: "ready"}, "EgressProxyNotReady"),
            ({proxy: "ready", ingress: "stall"}, "ProbeTimeout"),
        ):
            with self.subTest(helpers=helpers):
                result = self.probe(env=env, helpers=helpers)
                self.assertEqual(result.returncode, 1 if expected else 0, result.stderr)
                if expected:
                    self.assertIn(expected, result.stderr)
                else:
                    self.assertLess(result.stderr.index(proxy), result.stderr.index(ingress))

    def test_invalid_inputs_fail_without_http(self):
        for args, port in (([], "8642"), (["--timeout", "0"], "8642"),
                           (["--timeout", "2147484"], "8642"), (["--timeout", "1"], "9120"),
                           (["--timeout", "1"], "65536"), (["--timeout", "1"], "localhost")):
            with self.subTest(args=args, port=port):
                result = self.probe(args=args, env={"API_SERVER_PORT": port})
                self.assertEqual(result.returncode, 1)
                self.assertIn("InvalidProbeArguments", result.stderr)
        self.assertEqual(self.requests, [])


if __name__ == "__main__":
    unittest.main()
