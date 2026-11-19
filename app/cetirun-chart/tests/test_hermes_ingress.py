# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Exercise listener authority against synthetic retained HOME files."""

import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest

import yaml

CHART = Path(__file__).resolve().parents[1]
APPLY = CHART / "agents/hermes/bootstrap/hermes-apply-config.py"


class HermesIngressTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.home = self.root / "home"
        (self.home / ".hermes").mkdir(parents=True)
        self.config = self.home / ".hermes/config.yaml"
        self.env_file = self.home / ".hermes/.env"
        self.legacy = self.home / ".hermes/gateway.json"
        self.generated = self.root / "generated.yaml"
        self.generated_env = self.root / "generated.env"
        self.generated.write_text(
            "model:\n"
            "  provider: cetirun-gateway\n"
            "  default: synthetic-model\n"
            "providers:\n"
            "  cetirun-gateway:\n"
            "    name: CetiRun AI Gateway\n"
            "    api: http://gateway.example.invalid/v1\n"
            "    key_env: CETIRUN_GATEWAY_API_KEY\n"
            "    transport: chat_completions\n"
            "    default_model: synthetic-model\n"
            "    discover_models: false\n"
            "    models:\n"
            "      synthetic-model: {}\n"
        )
        self.generated_env.write_text("API_SERVER_KEY=sk-dummy\nAPI_SERVER_HOST=0.0.0.0\nAPI_SERVER_PORT=18080\n")
        self.saved = {
            "model": {"provider": "openai", "default": "saved-model"},
            "user_preference": {"theme": "retained", "nested": [1, 2]},
            "platforms": {
                "api_server": {
                    "enabled": False, "host": "0.0.0.0", "port": 18080,
                    "extra": {"host": "::", "port": 18081, "model_name": "retained-name"},
                    "reply_to_mode": "all",
                },
                "fixture-platform": {"enabled": False, "extra": {"port": 18082}},
            },
            "dashboard": {"host": "0.0.0.0", "port": 18083, "theme": "retained"},
        }
        self.saved["gateway"] = {
            "platforms": {"api_server": copy.deepcopy(self.saved["platforms"]["api_server"])},
            "api_server": copy.deepcopy(self.saved["platforms"]["api_server"]),
            "unrelated": {"retained": True},
        }
        self.saved_legacy = {"platforms": {"api_server": copy.deepcopy(self.saved["platforms"]["api_server"])},
                             "legacy_preference": "retained"}
        self.saved_legacy["platforms"]["api_server"]["extra"]["_enabled_explicit"] = True
        self.legacy.write_text(json.dumps(self.saved_legacy))
        self.config.write_text(yaml.safe_dump(self.saved))
        self.env_file.write_text(
            "API_SERVER_HOST=0.0.0.0\nAPI_SERVER_PORT=18084\nAPI_SERVER_ENABLED=false\n"
            "USER_SETTING=retained=value\nHERMES_DASHBOARD_HOST=0.0.0.0\n"
            "HERMES_WEBAPP_PUBLIC_URL=https://other.example.com/wrong\n"
            "HERMES_DASHBOARD_PORT=18085\nTTYD_PORT=18086\nTTYD_BIND_ARGS=--interface 0.0.0.0\n"
        )
        self.process_env = dict(
            os.environ, CETIRUN_INGRESS_MTLS="true", API_SERVER_ENABLED="true",
            API_SERVER_HOST="127.0.0.1", API_SERVER_PORT="8642",
            HERMES_WEBAPP_PUBLIC_URL="https://example.com/ui/example",
        )

    def run_apply(self, *arguments, environment=None):
        return subprocess.run(
            [sys.executable, str(APPLY), "--home", str(self.home), *arguments],
            env=environment or self.process_env, text=True, capture_output=True, timeout=10,
        )

    def assert_reserved(self):
        config = yaml.safe_load(self.config.read_text())
        api = config["platforms"]["api_server"]
        self.assertTrue({"enabled", "host", "port"}.isdisjoint(api))
        self.assertTrue({"host", "port"}.isdisjoint(api["extra"]))
        self.assertEqual(api["extra"]["model_name"], "retained-name")
        self.assertEqual(api["reply_to_mode"], "all")
        self.assertEqual(config["platforms"]["fixture-platform"], self.saved["platforms"]["fixture-platform"])
        self.assertEqual(config["dashboard"], self.saved["dashboard"])
        self.assertEqual(config["user_preference"], self.saved["user_preference"])
        for section in (config["gateway"]["platforms"]["api_server"], config["gateway"]["api_server"],
                        json.loads(self.legacy.read_text())["platforms"]["api_server"]):
            self.assertTrue({"enabled", "host", "port"}.isdisjoint(section))
            self.assertTrue({"host", "port", "_enabled_explicit"}.isdisjoint(section["extra"]))
            self.assertEqual(section["extra"]["model_name"], "retained-name")
        self.assertEqual(config["gateway"]["unrelated"], {"retained": True})
        self.assertEqual(json.loads(self.legacy.read_text())["legacy_preference"], "retained")
        env = self.env_file.read_text()
        for name in ("API_SERVER_HOST=", "API_SERVER_PORT=", "API_SERVER_ENABLED=", "HERMES_WEBAPP_PUBLIC_URL="):
            self.assertNotIn(name, env)
        for line in ("USER_SETTING=retained=value", "HERMES_DASHBOARD_HOST=0.0.0.0",
                     "HERMES_DASHBOARD_PORT=18085", "TTYD_PORT=18086", "TTYD_BIND_ARGS=--interface 0.0.0.0"):
            self.assertIn(line, env)

    def test_mtls_initialization_removes_only_competing_listener_settings(self):
        result = self.run_apply("--config", str(self.generated), "--env", str(self.generated_env), "--ingress-mtls")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_reserved()
        self.assertEqual(self.run_apply("--check", "--ingress-mtls").returncode, 0)

    def test_false_mode_preserves_existing_listener_preferences(self):
        result = self.run_apply("--config", str(self.generated), "--env", str(self.generated_env),
                                environment=dict(self.process_env, CETIRUN_INGRESS_MTLS="false"))
        self.assertEqual(result.returncode, 0, result.stderr)
        current = yaml.safe_load(self.config.read_text())
        self.assertEqual(current["platforms"], self.saved["platforms"])
        self.assertEqual(current["gateway"], self.saved["gateway"])
        self.assertEqual(json.loads(self.legacy.read_text()), self.saved_legacy)
        self.assertIn("API_SERVER_HOST=0.0.0.0", self.env_file.read_text())
        self.assertIn("API_SERVER_PORT=18080", self.env_file.read_text())
        self.assertIn("API_SERVER_ENABLED=false", self.env_file.read_text())
        self.assertIn("HERMES_WEBAPP_PUBLIC_URL=https://other.example.com/wrong", self.env_file.read_text())

    def test_supervised_restart_reasserts_authority_without_resetting_model_or_user_data(self):
        self.env_file.write_text(self.env_file.read_text() + "# User comment\n\nKEEP_FORMAT=  retained value  \n")
        for _ in range(2):
            self.config.write_text(yaml.safe_dump(self.saved))
            self.env_file.write_text(self.env_file.read_text() + "API_SERVER_HOST=::\nAPI_SERVER_PORT=15443\n")
            result = self.run_apply("--ingress-mtls", "--listeners-only")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_reserved()
            self.assertEqual(yaml.safe_load(self.config.read_text())["model"], self.saved["model"])
            self.assertIn("# User comment\n\nKEEP_FORMAT=  retained value  \n", self.env_file.read_text())
        before = (self.config.read_bytes(), self.env_file.read_bytes())
        self.assertEqual(self.run_apply("--ingress-mtls", "--listeners-only").returncode, 0)
        self.assertEqual(before, (self.config.read_bytes(), self.env_file.read_bytes()))

    def test_unavailable_runtime_authority_fails_without_modifying_home(self):
        for changes in (
            {"CETIRUN_INGRESS_MTLS": "false"}, {"API_SERVER_HOST": "0.0.0.0"},
            {"API_SERVER_PORT": ""}, {"API_SERVER_PORT": "15443"}, {"API_SERVER_PORT": "65536"},
            {"API_SERVER_ENABLED": "false"},
        ):
            with self.subTest(changes=changes):
                before = (self.config.read_bytes(), self.env_file.read_bytes())
                result = self.run_apply("--ingress-mtls", "--listeners-only",
                                        environment=self.process_env | changes)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(before, (self.config.read_bytes(), self.env_file.read_bytes()))

    def test_malformed_platform_parent_fails_closed_and_preserves_files(self):
        bad = copy.deepcopy(self.saved)
        bad["platforms"]["api_server"]["extra"] = ["malformed-synthetic-setting"]
        self.config.write_text(yaml.safe_dump(bad))
        before = (self.config.read_bytes(), self.env_file.read_bytes())
        result = self.run_apply("--ingress-mtls", "--listeners-only")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("malformed-synthetic-setting", result.stdout + result.stderr)
        self.assertEqual(before, (self.config.read_bytes(), self.env_file.read_bytes()))

    def test_check_rejects_stale_bindings_without_rewriting(self):
        before = (self.config.read_bytes(), self.env_file.read_bytes())
        self.assertNotEqual(self.run_apply("--check", "--ingress-mtls").returncode, 0)
        self.assertEqual(before, (self.config.read_bytes(), self.env_file.read_bytes()))

    def test_malformed_legacy_configuration_is_not_silently_ignored(self):
        self.legacy.write_text('{"malformed-synthetic-legacy":')
        before = (self.config.read_bytes(), self.env_file.read_bytes(), self.legacy.read_bytes())
        result = self.run_apply("--ingress-mtls", "--listeners-only")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("malformed-synthetic-legacy", result.stderr + result.stdout)
        self.assertEqual(before, (self.config.read_bytes(), self.env_file.read_bytes(), self.legacy.read_bytes()))

    def test_mode_does_not_leave_persisted_loopback_defaults_on_rollback(self):
        self.assertEqual(self.run_apply("--ingress-mtls", "--listeners-only").returncode, 0)
        self.assertNotIn("127.0.0.1", self.config.read_text() + self.env_file.read_text())
        self.env_file.write_text(self.env_file.read_text() + "API_SERVER_HOST=0.0.0.0\nAPI_SERVER_PORT=19000\n")
        result = self.run_apply("--config", str(self.generated), "--env", str(self.generated_env))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("API_SERVER_HOST=0.0.0.0", self.env_file.read_text())

    def test_listener_only_mode_requires_explicit_mtls(self):
        self.assertNotEqual(self.run_apply("--listeners-only").returncode, 0)

    def assert_yaml_alias_preserved(self, shared_kind):
        endpoint = """\
enabled: false
host: 127.0.0.1
port: 8642
extra:
  host: "::"
  port: 18080
  _enabled_explicit: true
  model_name: retained
"""
        shared = {
            "extra": "host: 127.0.0.1\nport: 8642\n_enabled_explicit: true\nmodel_name: retained\n",
            "api": endpoint,
            "parent": "api_server:\n" + textwrap.indent(endpoint, "  ") + "unrelated: {retained: true}\n",
        }[shared_kind]

        def at(config, path):
            for name in path:
                config = config[name]
            return config

        saved_env = self.env_file.read_text()
        for path in (("platforms", "api_server"), ("gateway", "platforms", "api_server"), ("gateway", "api_server")):
            alias_path = path + ("extra",) if shared_kind == "extra" else path if shared_kind == "api" else path[:-1]
            document = "user_preference:\n  endpoint: &endpoint\n" + textwrap.indent(shared, "    ")
            for depth, name in enumerate(alias_path):
                document += "  " * depth + name + (": *endpoint\n" if depth == len(alias_path) - 1 else ":\n")
            original = yaml.safe_load(document)
            self.assertIs(at(original, alias_path), original["user_preference"]["endpoint"])
            wanted_preference = copy.deepcopy(original["user_preference"])
            wanted_api = copy.deepcopy(at(original, path))
            for mode in ("initialization", "supervised-start", "ordinary"):
                with self.subTest(shared=shared_kind, path=path, mode=mode):
                    self.config.write_text(document)
                    self.env_file.write_text(saved_env)
                    if mode == "supervised-start":
                        result = self.run_apply("--ingress-mtls", "--listeners-only")
                    else:
                        arguments = ["--config", str(self.generated), "--env", str(self.generated_env)]
                        if mode == "initialization":
                            arguments.append("--ingress-mtls")
                        result = self.run_apply(*arguments, environment=self.process_env | {
                            "CETIRUN_INGRESS_MTLS": "false" if mode == "ordinary" else "true",
                        })
                    self.assertEqual(result.returncode, 0, result.stderr)
                    current = yaml.safe_load(self.config.read_text())
                    self.assertEqual(current["user_preference"], wanted_preference)
                    api = at(current, path)
                    if mode == "ordinary":
                        self.assertEqual(api, wanted_api)
                        self.assertIs(at(current, alias_path), current["user_preference"]["endpoint"])
                    else:
                        self.assertTrue({"enabled", "host", "port"}.isdisjoint(api))
                        self.assertTrue({"host", "port", "_enabled_explicit"}.isdisjoint(api["extra"]))
                        self.assertEqual(api["extra"]["model_name"], "retained")
                        self.assertIsNot(at(current, alias_path), current["user_preference"]["endpoint"])

    def test_yaml_alias_shared_extra_preserves_unrelated_preferences(self):
        self.assert_yaml_alias_preserved("extra")

    def test_yaml_alias_shared_api_preserves_unrelated_preferences(self):
        self.assert_yaml_alias_preserved("api")

    def test_yaml_alias_shared_platforms_parent_preserves_unrelated_preferences(self):
        self.assert_yaml_alias_preserved("parent")

    def test_supervisor_checks_each_gateway_start(self):
        initializer = (CHART / "agents/hermes/bootstrap/hermes-init-home.sh").read_text()
        entrypoint = (CHART / "agents/hermes/bootstrap/hermes-entrypoint.sh").read_text()
        wrapper = (CHART / "agents/hermes/bootstrap/hermes-gateway.sh").read_text()
        template = (CHART / "templates/agents/hermes/_runtime.tpl").read_text()
        self.assertIn('set -- "$@" --ingress-mtls', initializer)
        self.assertIn("CETIRUN_INGRESS_MTLS", template)
        self.assertIn("command=/bin/sh /etc/cetirun/hermes-gateway.sh", entrypoint)
        self.assertIn("hermes-runtime.py check", wrapper)
        self.assertIn("exec hermes gateway run --replace --accept-hooks --external-supervisor", wrapper)
        self.assertNotIn("[program:hermes-dashboard]", entrypoint)


if __name__ == "__main__":
    unittest.main()
