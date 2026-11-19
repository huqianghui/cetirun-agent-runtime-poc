# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Regression tests for transient MCR manifest propagation."""

import importlib.util
import io
from pathlib import Path
import subprocess
import unittest
from contextlib import redirect_stderr
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "verify_mcr_image", ROOT / "scripts" / "verify-mcr-image.py"
)
assert SPEC is not None and SPEC.loader is not None
VERIFY_MCR_IMAGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VERIFY_MCR_IMAGE)

IMAGE = "mcr.microsoft.com/aks/ai-runtime/cetirun/openclaw-gateway:0.1.1"
DIGEST = "sha256:" + "a" * 64


class VerifyMCRImageTests(unittest.TestCase):
    def test_retries_when_platform_manifest_lags_tag_digest(self) -> None:
        """A synchronized index can reference a child manifest not yet in MCR."""
        pull_failure = subprocess.CalledProcessError(
            1,
            ["docker", "pull"],
            stderr="manifest unknown",
        )
        successful_pull = subprocess.CompletedProcess(["docker", "pull"], 0)
        inspect = subprocess.CompletedProcess(
            ["docker", "image", "inspect"],
            0,
            stdout="linux/amd64\n",
        )

        with (
            patch.object(VERIFY_MCR_IMAGE.shutil, "which", return_value="/usr/bin/docker"),
            patch.object(VERIFY_MCR_IMAGE, "manifest_matches", side_effect=[True, True]),
            patch.object(
                VERIFY_MCR_IMAGE.subprocess,
                "run",
                side_effect=[pull_failure, successful_pull, inspect],
            ) as run,
            patch.object(VERIFY_MCR_IMAGE.time, "sleep") as sleep,
        ):
            result = VERIFY_MCR_IMAGE.main(
                ["--image", IMAGE, "--digest", DIGEST, "--attempts", "2", "--interval", "0"]
            )

        self.assertEqual(result, 0)
        self.assertEqual(run.call_count, 3)
        sleep.assert_called_once_with(0)

    def test_reports_last_pull_error_after_propagation_window(self) -> None:
        pull_failure = subprocess.CalledProcessError(
            1,
            ["docker", "pull"],
            stderr="manifest unknown",
        )
        stderr = io.StringIO()

        with (
            patch.object(VERIFY_MCR_IMAGE.shutil, "which", return_value="/usr/bin/docker"),
            patch.object(VERIFY_MCR_IMAGE, "manifest_matches", return_value=True),
            patch.object(VERIFY_MCR_IMAGE.subprocess, "run", side_effect=pull_failure),
            redirect_stderr(stderr),
        ):
            result = VERIFY_MCR_IMAGE.main(
                ["--image", IMAGE, "--digest", DIGEST, "--attempts", "1", "--interval", "0"]
            )

        self.assertEqual(result, 1)
        self.assertIn("Could not pull the synchronized MCR manifest", stderr.getvalue())
        self.assertIn("manifest unknown", stderr.getvalue())
        self.assertNotIn("check onboarding", stderr.getvalue())

    def test_retains_timeout_diagnostics(self) -> None:
        timeout = subprocess.TimeoutExpired(
            ["docker", "pull"],
            600,
            output=b"pull progress",
            stderr=b"docker daemon unavailable",
        )

        with patch.object(VERIFY_MCR_IMAGE.subprocess, "run", side_effect=timeout):
            error = VERIFY_MCR_IMAGE.pull_image("example.invalid/image:1.0.0")

        self.assertIn("timed out after 600 seconds", error)
        self.assertIn("stdout: pull progress", error)
        self.assertIn("stderr: docker daemon unavailable", error)

    def test_retains_failure_diagnostics_from_both_streams(self) -> None:
        failure = subprocess.CalledProcessError(
            1,
            ["docker", "pull"],
            output="pull progress",
            stderr="manifest unknown",
        )

        with patch.object(VERIFY_MCR_IMAGE.subprocess, "run", side_effect=failure):
            error = VERIFY_MCR_IMAGE.pull_image("example.invalid/image:1.0.0")

        self.assertIn("stdout: pull progress", error)
        self.assertIn("stderr: manifest unknown", error)


if __name__ == "__main__":
    unittest.main()
