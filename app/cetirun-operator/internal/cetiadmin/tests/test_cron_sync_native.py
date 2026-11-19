# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Qualify cron synchronization with a prebuilt, pinned native Hermes install."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


INSTALL = os.environ.get("CETIADMIN_TEST_HERMES_INSTALL_DIR", "")
PYTHON = os.environ.get("CETIADMIN_TEST_HERMES_PYTHON", "")
SCRIPT = Path(__file__).resolve().parents[1] / "assets/scripts/cron_sync.py"


@unittest.skipUnless(INSTALL and PYTHON, "set the pinned native Hermes install and Python")
class NativeCronSyncTests(unittest.TestCase):
    def test_create_restart_pause_and_resume_keep_one_native_job(self):
        with tempfile.TemporaryDirectory(prefix="cetiadmin-native-cron-") as directory:
            root = Path(directory)
            home = root / "home/.hermes"
            home.mkdir(parents=True, mode=0o700)
            config = {
                "instance": {"name": "example", "namespace": "cetirun-example", "uid": "synthetic-agent"},
                "monitor": {"enabled": True, "schedule": "every 30m", "timezone": "UTC", "deliver": "local"},
            }
            path = root / "monitor.json"
            env = {
                "PATH": os.defpath, "HOME": str(home.parent), "HERMES_HOME": str(home),
                "HERMES_INSTALL_DIR": INSTALL, "PYTHONPATH": INSTALL, "PYTHONDONTWRITEBYTECODE": "1",
            }
            job = None
            for enabled, expected in [(True, "created"), (True, "unchanged"), (False, "paused"), (True, "updated"), (True, "unchanged")]:
                config["monitor"]["enabled"] = enabled
                path.write_text(json.dumps(config))
                result = subprocess.run([PYTHON, "-B", str(SCRIPT), "--config", str(path)],
                                        env=env, cwd=root, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 0, result.stdout)
                self.assertEqual(result.stderr, "")
                value = json.loads(result.stdout)
                self.assertEqual(value["status"], expected)
                job = job or value["jobId"]
                self.assertEqual(value["jobId"], job)
            jobs = json.loads((home / "cron/jobs.json").read_text())["jobs"]
            self.assertEqual(len(jobs), 1)
            self.assertEqual(jobs[0]["id"], job)
            self.assertTrue(jobs[0]["enabled"])
            self.assertEqual(jobs[0]["skills"], ["cetiadmin"])


if __name__ == "__main__":
    unittest.main()
