# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Run Hermes startup with synthetic HOME and strict native-command doubles."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest


ENTRYPOINT = Path(__file__).resolve().parents[1] / "agents/hermes/bootstrap/hermes-entrypoint.sh"
SYNC = "/opt/cetirun/cetiadmin/scripts/cron_sync.py"
SKILLS = "/opt/cetirun/cetiadmin/scripts/check_skill_sources.py"
PRIVATE = "synthetic-token-never-print"


class CetiAdminStartupTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.home = self.root / "hermes"
        self.home.mkdir()
        (self.home / "config.yaml").write_text("model: {default: fixture-model}\n")
        (self.home / ".env").write_text(
            "API_SERVER_HOST=127.0.0.1\nAPI_SERVER_PORT=8642\n"
            "HERMES_TIMEZONE=invalid-retained-timezone\nUNRELATED=retained\n"
        )
        self.config = self.root / "cetiadmin-runtime.json"
        self.config.write_text(json.dumps({
            "instance": {"name": "cetiadmin", "namespace": "cetirun-cetiadmin", "uid": "fixture-uid"},
            "monitor": {"enabled": True, "schedule": "every 30m", "timezone": "Asia/Singapore", "deliver": "local"},
        }))
        self.events = self.root / "events.jsonl"
        self.scratch = self.root / "scratch"
        self.scratch.mkdir()
        self.assets_bin = self.root / "assets-bin"
        self.assets_bin.mkdir()
        self.startup = self.root / "entrypoint.sh"
        # Relocate scratch and the binary fixture; run the whole shipped startup script.
        self.startup.write_text(ENTRYPOINT.read_text().replace("/tmp/", str(self.scratch) + "/")
                                .replace("/opt/cetirun/cetiadmin/bin", str(self.assets_bin))
                                .replace("/usr/local/bin/hermes-prepare-dependencies",
                                         str(self.root / "bin/hermes-python") + " --prepare")
                                .replace("/usr/local/bin/hermes-python", str(self.root / "bin/hermes-python")))
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for directory, version in ((self.assets_bin, "exported-helm"), (self.bin, "unrelated-helm")):
            helm = directory / "helm"
            helm.write_text('#!/bin/sh\n[ "$*" = "version --short" ] || exit 9\nprintf "%s\\n" ' + version + '\n')
            helm.chmod(0o755)
        self.python = self.bin / "hermes-python"
        self.python.write_text(f"#!{sys.executable}\n" + textwrap.dedent(f"""\
            import json
            import os
            from pathlib import Path
            import sys

            args = sys.argv[1:]
            if args == ["--prepare"]:
                if os.environ.get("FIXTURE_PREPARE_FAIL"):
                    sys.exit(8)
                Path(os.environ["HERMES_HOME"], "dependencies-ready").touch()
            elif args == ["/etc/cetirun/hermes-runtime.py", "check"]:
                assert Path(os.environ["HERMES_HOME"], "dependencies-ready").is_file()
                with open(os.environ["FIXTURE_EVENTS"], "a") as out:
                    out.write(json.dumps({{"event": "runtime", "host": os.environ.get("API_SERVER_HOST"),
                        "port": os.environ.get("API_SERVER_PORT")}}) + "\\n")
                if os.environ.get("FIXTURE_RUNTIME_FAIL"):
                    sys.exit(9)
            elif args == [{SKILLS!r}]:
                assert not Path(os.environ["FIXTURE_SCRATCH"], "supervisord.conf").exists()
                with open(os.environ["FIXTURE_EVENTS"], "a") as out:
                    out.write(json.dumps({{"event": "skills", "args": args,
                        "host": os.environ.get("API_SERVER_HOST"), "port": os.environ.get("API_SERVER_PORT")}}) + "\\n")
                if os.environ.get("FIXTURE_SKILLS_FAIL"):
                    print({PRIVATE!r})
                    print({PRIVATE!r}, file=sys.stderr)
                    sys.exit(9)
                print('{{"status":"ready","skill":"cetiadmin","archived":0}}')
            elif args and args[0] == {SYNC!r}:
                assert args == [{SYNC!r}, "--config", os.environ["CETIRUN_CETIADMIN_CONFIG"]]
                assert not Path(os.environ["FIXTURE_SCRATCH"], "supervisord.conf").exists()
                with open(os.environ["FIXTURE_EVENTS"], "a") as out:
                    out.write(json.dumps({{"event": "sync", "args": args,
                        "host": os.environ.get("API_SERVER_HOST"), "port": os.environ.get("API_SERVER_PORT")}}) + "\\n")
                if os.environ.get("FIXTURE_SYNC_FAIL"):
                    print({PRIVATE!r})
                    print({PRIVATE!r}, file=sys.stderr)
                    sys.exit(9)
                print(os.environ["FIXTURE_SYNC_RESPONSE"])
            elif args and args[0] in ("-", "-c"):
                os.execv({sys.executable!r}, [{sys.executable!r}, *args])
            else:
                sys.exit("unexpected native command")
        """))
        self.python.chmod(0o755)
        supervisor = self.bin / "supervisord"
        supervisor.write_text(f"#!{sys.executable}\n" + textwrap.dedent("""\
            import json
            import os
            from pathlib import Path
            import subprocess
            import sys

            assert sys.argv[1:] == ["-n", "-c", str(Path(os.environ["FIXTURE_SCRATCH"], "supervisord.conf"))]
            helm = subprocess.check_output(["helm", "version", "--short"], text=True).strip()
            with open(os.environ["FIXTURE_EVENTS"], "a") as out:
                out.write(json.dumps({"event": "supervisor", "timezone": os.environ.get("HERMES_TIMEZONE"),
                    "host": os.environ.get("API_SERVER_HOST"), "port": os.environ.get("API_SERVER_PORT"),
                    "helm": helm}) + "\\n")
        """))
        supervisor.chmod(0o755)
        self.env = {
            "PATH": str(self.bin) + ":" + os.defpath,
            "HOME": str(self.root),
            "HERMES_HOME": str(self.home), "HERMES_INSTALL_DIR": str(self.root),
            "HERMES_PYTHON": str(self.python), "CETIRUN_AI_GATEWAY": "true",
            "TTYD_ENABLED": "false", "TAILSCALE_ENABLED": "false",
            "CETIRUN_CETIADMIN_ENABLED": "true", "CETIRUN_CETIADMIN_CONFIG": str(self.config),
            "HERMES_TIMEZONE": "Etc/GMT+7", "API_SERVER_HOST": "127.0.0.1", "API_SERVER_PORT": "19000",
            "FIXTURE_EVENTS": str(self.events), "FIXTURE_SCRATCH": str(self.scratch),
            "FIXTURE_SYNC_RESPONSE": json.dumps({"status": "created", "timezone": "Asia/Singapore"}),
        }

    def run_startup(self, **updates):
        result = subprocess.run(["/bin/bash", str(self.startup)], env=self.env | updates,
                                capture_output=True, text=True, timeout=10)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)
        return result

    def recorded(self):
        return [json.loads(line) for line in self.events.read_text().splitlines()] if self.events.exists() else []

    def test_skills_and_sync_follow_runtime_check_and_precede_supervisor(self):
        result = self.run_startup()
        self.assertEqual(result.returncode, 0, result.stderr)
        events = self.recorded()
        self.assertEqual([item["event"] for item in events], ["runtime", "skills", "sync", "supervisor"])
        self.assertEqual(events[1]["args"], [SKILLS])
        self.assertEqual(events[2]["args"], [SYNC, "--config", str(self.config)])
        for event in events:
            self.assertEqual(event["host"], "127.0.0.1")
            self.assertEqual(event["port"], "19000")
        self.assertEqual(events[3]["timezone"], "Asia/Singapore")
        self.assertEqual(events[3]["helm"], "exported-helm")
        self.assertIn("HERMES_TIMEZONE=invalid-retained-timezone", (self.home / ".env").read_text())

    def test_disabled_ordinary_startup_does_not_check_skills_or_sync(self):
        result = self.run_startup(CETIRUN_CETIADMIN_ENABLED="false", FIXTURE_SKILLS_FAIL="true")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([item["event"] for item in self.recorded()], ["runtime", "supervisor"])
        self.assertEqual(self.recorded()[1]["timezone"], "Etc/GMT+7")
        self.assertEqual(self.recorded()[1]["helm"], "unrelated-helm")

    def test_startup_restores_native_python_without_container_env_entry(self):
        self.env.pop("HERMES_PYTHON")
        result = self.run_startup()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([item["event"] for item in self.recorded()], ["runtime", "skills", "sync", "supervisor"])

    def test_native_sync_failure_stops_startup_without_exposing_output(self):
        result = self.run_startup(FIXTURE_SYNC_FAIL="true")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([item["event"] for item in self.recorded()], ["runtime", "skills", "sync"])
        self.assertFalse((self.scratch / "supervisord.conf").exists())

    def test_skill_conflict_stops_before_cron_without_exposing_output(self):
        result = self.run_startup(FIXTURE_SKILLS_FAIL="true")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([item["event"] for item in self.recorded()], ["runtime", "skills"])
        self.assertFalse((self.scratch / "supervisord.conf").exists())

    def test_invalid_or_mismatched_response_never_starts_scheduler(self):
        for response in (PRIVATE, json.dumps({"status": "error", "timezone": "Asia/Singapore", "detail": PRIVATE}),
                         json.dumps({"status": "created"}), json.dumps({"status": "created", "timezone": "invalid/zone"}),
                         json.dumps({"status": "created", "timezone": "UTC"}),
                         json.dumps({"status": "unexpected", "timezone": "Asia/Singapore"})):
            with self.subTest(response=response):
                result = self.run_startup(FIXTURE_SYNC_RESPONSE=response)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("supervisor", [item["event"] for item in self.recorded()])

    def test_native_python_and_generated_config_are_required(self):
        self.python.chmod(0o600)
        self.assertNotEqual(self.run_startup().returncode, 0)
        self.python.chmod(0o755)
        for changes in ({"CETIRUN_CETIADMIN_CONFIG": ""}, {"CETIRUN_CETIADMIN_CONFIG": "/missing/monitor.json"}):
            with self.subTest(changes=changes):
                result = self.run_startup(**changes)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("supervisor", [item["event"] for item in self.recorded()])

    def test_runtime_failure_precedes_cetiadmin_work(self):
        result = self.run_startup(FIXTURE_RUNTIME_FAIL="true")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([item["event"] for item in self.recorded()], ["runtime"])
        self.assertFalse((self.scratch / "supervisord.conf").exists())

    def test_dependency_preparation_failure_stops_before_runtime_check(self):
        for enabled in ("true", "false"):
            with self.subTest(cetiadmin=enabled):
                result = self.run_startup(FIXTURE_PREPARE_FAIL="true", CETIRUN_CETIADMIN_ENABLED=enabled)
                self.assertEqual(result.returncode, 8, result.stderr)
                self.assertEqual(self.recorded(), [])
                self.assertFalse((self.scratch / "supervisord.conf").exists())

    def test_disabled_monitor_still_propagates_validated_timezone(self):
        config = json.loads(self.config.read_text())
        config["monitor"]["enabled"] = False
        self.config.write_text(json.dumps(config))
        result = self.run_startup(FIXTURE_SYNC_RESPONSE=json.dumps({"status": "paused", "timezone": "Asia/Singapore"}))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.recorded()[-1]["timezone"], "Asia/Singapore")


if __name__ == "__main__":
    unittest.main()
