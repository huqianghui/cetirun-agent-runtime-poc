# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Tests for the AI Gateway image entrypoint's proxy modes."""

import importlib.machinery
import importlib.util
import os
import pathlib
import sys
import tempfile
import threading
import time
import types
import unittest
from unittest import mock

ENTRYPOINT = pathlib.Path(__file__).resolve().parents[2] / "app/cetirun-ai-gateway/entrypoint.py"


def load_entrypoint():
    loader = importlib.machinery.SourceFileLoader("gateway_entrypoint", str(ENTRYPOINT))
    spec = importlib.util.spec_from_loader("gateway_entrypoint", loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class CopilotProxyModeTest(unittest.TestCase):
    def setUp(self):
        self.entrypoint = load_entrypoint()

    def test_environment_disables_device_sign_in(self):
        env = self.entrypoint.copilot_proxy_environment("/run/token", {"PATH": "/bin"})
        self.assertEqual(env["GITHUB_COPILOT_ACCESS_TOKEN_FILE"], "/run/token")
        self.assertEqual(env["GITHUB_COPILOT_DEVICE_CODE_URL"], self.entrypoint.DISABLED_URL)
        self.assertEqual(env["GITHUB_COPILOT_ACCESS_TOKEN_URL"], self.entrypoint.DISABLED_URL)
        self.assertEqual(env["GITHUB_COPILOT_TOKEN_DIR"], "/var/cache/copilot")
        self.assertEqual(env["LITELLM_LOCAL_MODEL_COST_MAP"], "True")
        self.assertEqual(env["PATH"], "/bin")

    def test_environment_keeps_an_explicit_token_directory(self):
        env = self.entrypoint.copilot_proxy_environment("/run/token", {"GITHUB_COPILOT_TOKEN_DIR": "/cache"})
        self.assertEqual(env["GITHUB_COPILOT_TOKEN_DIR"], "/cache")

    def test_token_present_needs_a_non_empty_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "access-token"
            self.assertFalse(self.entrypoint.token_present(path))
            path.write_text("  \n", encoding="utf-8")
            self.assertFalse(self.entrypoint.token_present(path))
            path.write_text("gho_example\n", encoding="utf-8")
            self.assertTrue(self.entrypoint.token_present(path))

    def test_proxy_waits_for_the_token_then_starts_litellm(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "access-token"
            started = []

            def fake_exec(program, argv, env):
                started.append((program, argv, env["GITHUB_COPILOT_ACCESS_TOKEN_FILE"]))
                raise SystemExit(0)

            def sign_in():
                time.sleep(0.3)
                path.write_text("gho_example", encoding="utf-8")

            environ = {"COPILOT_TOKEN_FILE": str(path), "COPILOT_TOKEN_POLL_SECONDS": "0.05"}
            with mock.patch.dict(os.environ, environ), mock.patch.object(self.entrypoint.os, "execvpe", fake_exec), \
                    mock.patch.object(self.entrypoint.signal, "signal"):
                thread = threading.Thread(target=sign_in)
                thread.start()
                with self.assertRaises(SystemExit):
                    self.entrypoint.main_with(["copilot-proxy", "--config", "/etc/litellm/config.yaml", "--port", "4000"])
                thread.join()
            self.assertEqual(started, [("litellm", ["litellm", "--config", "/etc/litellm/config.yaml", "--port", "4000"], str(path))])


class RequestLogIndexTest(unittest.TestCase):
    """The request log indexes LiteLLM 1.105 builds after its migrations, not in them."""

    def setUp(self):
        self.entrypoint = load_entrypoint()

    def build(self, manager):
        package = types.ModuleType("litellm_proxy_extras")
        package.__path__ = []
        utils = types.ModuleType("litellm_proxy_extras.utils")
        if manager is not None:
            utils.ProxyExtrasDBManager = manager
        modules = {"litellm_proxy_extras": package, "litellm_proxy_extras.utils": utils}
        with mock.patch.dict(sys.modules, modules), mock.patch("builtins.print") as printed:
            self.entrypoint.build_request_log_indexes()
        return " ".join(str(call.args[0]) for call in printed.call_args_list)

    def test_the_indexes_are_built_after_the_migrations(self):
        calls = []
        manager = types.SimpleNamespace(build_request_log_indexes=lambda: calls.append("built") or True)
        self.assertIn("in place", self.build(manager))
        self.assertEqual(calls, ["built"])

    def test_an_unfinished_build_does_not_fail_the_migration(self):
        self.assertIn("next migration tries again",
                      self.build(types.SimpleNamespace(build_request_log_indexes=lambda: False)))

        def broken():
            raise RuntimeError("database went away")

        self.assertIn("database went away", self.build(types.SimpleNamespace(build_request_log_indexes=broken)))

    def test_a_litellm_without_the_step_has_nothing_to_build(self):
        self.assertEqual(self.build(types.SimpleNamespace()), "")
        self.assertEqual(self.build(None), "")


class GatewayModeTest(unittest.TestCase):
    """The Gateway proxy, always with the guard, with and without the request log.

    LiteLLM is not installed for these tests: stand-ins record what the
    entrypoint registers and in which order relative to starting the proxy.
    """

    ARGS = ["--config", "/etc/litellm/config.yaml", "--port", "4000"]

    def setUp(self):
        self.entrypoint = load_entrypoint()
        self.events = []
        self.handler = object()
        self.guard = object()
        test = self

        class RunServer:
            def main(self, args, prog_name):
                test.events.append(("run", list(args), prog_name, test.proxy_server.disable_spend_logs))
                raise SystemExit(0)

        litellm = types.ModuleType("litellm")
        litellm.logging_callback_manager = types.SimpleNamespace(
            add_litellm_callback=lambda callback: self.events.append(("callback", callback)))
        self.proxy_server = types.ModuleType("litellm.proxy.proxy_server")
        self.proxy_server.disable_spend_logs = False
        proxy_cli = types.ModuleType("litellm.proxy.proxy_cli")
        proxy_cli.run_server = RunServer()
        proxy = types.ModuleType("litellm.proxy")
        proxy.proxy_server = self.proxy_server
        proxy.proxy_cli = proxy_cli
        package = types.ModuleType("cetirun_ai_gateway")
        package.__path__ = []
        request_log = types.ModuleType("cetirun_ai_gateway.request_log")
        request_log.handler = self.handler
        guard = types.ModuleType("cetirun_ai_gateway.guard")
        guard.handler = self.guard
        self.modules = {
            "litellm": litellm,
            "litellm.proxy": proxy,
            "litellm.proxy.proxy_server": self.proxy_server,
            "litellm.proxy.proxy_cli": proxy_cli,
            "cetirun_ai_gateway": package,
            "cetirun_ai_gateway.request_log": request_log,
            "cetirun_ai_gateway.guard": guard,
        }

    def start(self, environ, args=None):
        def fake_exec(program, argv):
            self.events.append(("exec", program, argv))
            raise SystemExit(0)

        with mock.patch.dict(sys.modules, self.modules), mock.patch.object(self.entrypoint.os, "execvp", fake_exec):
            with self.assertRaises(SystemExit) as raised:
                self.entrypoint.gateway(self.ARGS if args is None else args, environ)
        return raised.exception.code

    def test_without_the_request_log_the_guard_is_still_registered(self):
        self.start({})
        self.assertEqual(self.events, [("callback", self.guard), ("run", self.ARGS, "litellm", False)])

    def test_the_spend_log_stays_on_without_the_request_log(self):
        self.start({"CETIRUN_SPEND_LOGS": "false"})
        self.assertEqual(self.events, [("callback", self.guard), ("run", self.ARGS, "litellm", False)])
        self.assertFalse(self.proxy_server.disable_spend_logs)

    def test_the_request_log_is_registered_before_the_proxy_starts(self):
        self.start({"CETIRUN_REQUEST_LOG": "true"})
        self.assertEqual(self.events, [
            ("callback", self.guard), ("callback", self.handler), ("run", self.ARGS, "litellm", False)])

    def test_the_spend_log_is_turned_off_together_with_the_request_log(self):
        self.start({"CETIRUN_REQUEST_LOG": "true", "CETIRUN_SPEND_LOGS": "false"})
        self.assertEqual(self.events, [
            ("callback", self.guard), ("callback", self.handler), ("run", self.ARGS, "litellm", True)])

    def test_workers_that_would_load_litellm_again_are_refused(self):
        cases = [
            ({}, ["--port", "4000", "--num_workers", "2"]),
            ({}, ["--port", "4000", "--num_workers=3"]),
            ({"NUM_WORKERS": "4"}, ["--port", "4000"]),
            ({}, ["--port", "4000", "--run_gunicorn"]),
            ({"CETIRUN_REQUEST_LOG": "true"}, ["--port", "4000", "--num_workers", "2"]),
        ]
        for environ, args in cases:
            with self.subTest(environ=environ, args=args):
                self.events.clear()
                code = self.start(environ, args)
                self.assertIn("one uvicorn process", str(code))
                self.assertEqual(self.events, [])

    def test_one_worker_is_accepted(self):
        self.start({"CETIRUN_REQUEST_LOG": "true", "NUM_WORKERS": "1"}, ["--port", "4000", "--num_workers", "1"])
        self.assertEqual(self.events[:2], [("callback", self.guard), ("callback", self.handler)])

    def test_a_litellm_without_the_spend_log_setting_does_not_start(self):
        del self.proxy_server.disable_spend_logs
        code = self.start({"CETIRUN_REQUEST_LOG": "true", "CETIRUN_SPEND_LOGS": "false"})
        self.assertIn("disable_spend_logs", str(code))
        self.assertEqual(self.events, [("callback", self.guard), ("callback", self.handler)])

    def test_other_arguments_start_the_gateway(self):
        with mock.patch.object(self.entrypoint, "gateway") as gateway:
            self.entrypoint.main_with(self.ARGS)
        gateway.assert_called_once_with(self.ARGS)


if __name__ == "__main__":
    unittest.main()
