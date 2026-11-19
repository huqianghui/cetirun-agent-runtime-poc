# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline Docker storage regressions; never run Docker, sudo, or systemctl."""

from contextlib import redirect_stderr
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import prepare_docker_storage as storage


class DockerStorageTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="cetirun-docker-test-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name).resolve()
        self.mount = self.root / "storage"
        self.mount.mkdir()
        self.config = self.root / "etc/docker/daemon.json"
        self.config.parent.mkdir(parents=True)
        self.original = b'{"log-driver": "journald", "features": {"buildkit": true}}\n'
        self.config.write_bytes(self.original)
        self.config.chmod(0o640)
        self.target = self.mount / "cetirun-docker"
        self.commands = []
        self.info_count = 0
        self.initial_root = "/var/lib/docker"
        self.initial_driver = "overlayfs"
        self.final_root = str(self.target)
        self.final_driver = "overlay2"
        self.containers = ""
        self.fail_start = False
        self.fail_validate = False
        self.fail_rollback = False
        self.free = 100 * storage.GIB
        for name, value in (
            ("same_filesystem", False),
            ("disk_usage", SimpleNamespace(free=self.free)),
        ):
            owner = storage.shutil if name == "disk_usage" else storage
            mock = patch.object(owner, name, return_value=value)
            setattr(self, name, mock.start())
            self.addCleanup(mock.stop)
        mock = patch.object(storage, "run", side_effect=self.fake_run)
        mock.start()
        self.addCleanup(mock.stop)

    def fake_run(self, args, *, capture=False):
        args = list(map(str, args))
        self.commands.append(args)
        if args[:len(storage.DOCKER)] == storage.DOCKER:
            if "info" in args:
                self.info_count += 1
                return json.dumps({
                    "DockerRootDir": self.initial_root if self.info_count == 1 else self.final_root,
                    "Driver": self.initial_driver if self.info_count == 1 else self.final_driver,
                })
            if "ps" in args:
                return self.containers
        if args[0] == "dockerd" and self.fail_validate:
            raise subprocess.CalledProcessError(1, args)
        if args[:2] == ["systemctl", "start"] and self.fail_start:
            raise subprocess.CalledProcessError(1, args)
        if args[:2] == ["systemctl", "restart"] and self.fail_rollback:
            raise subprocess.CalledProcessError(1, args)
        return "" if capture else None

    def prepare(self, **kwargs):
        return storage.prepare(self.mount, config_path=self.config, **kwargs)

    def systemctl(self):
        return [args for args in self.commands if args[0] == "systemctl"]

    def test_config_preserves_settings_and_moves_layers_cache_and_scratch(self):
        self.assertEqual(self.prepare(), self.target)
        config = json.loads(self.config.read_text())
        self.assertEqual(config["log-driver"], "journald")
        self.assertEqual(config["features"], {"buildkit": True, "containerd-snapshotter": False})
        self.assertEqual(config["data-root"], str(self.target))
        self.assertEqual(config["storage-driver"], "overlay2")
        self.assertEqual(self.config.stat().st_mode & 0o777, 0o640)
        self.assertEqual((self.mount / "cetirun-build-tmp").stat().st_mode & 0o7777, 0o1777)
        self.assertEqual(self.systemctl(), [
            ["systemctl", "stop", *storage.UNITS],
            ["systemctl", "start", *storage.UNITS],
        ])
        validate = next(i for i, args in enumerate(self.commands) if args[0] == "dockerd")
        stop = next(i for i, args in enumerate(self.commands) if args[:2] == ["systemctl", "stop"])
        self.assertLess(validate, stop)
        self.assertEqual(self.info_count, 2)
        self.assertEqual(list(self.config.parent.glob("cetirun-docker-*")), [])

    def test_missing_daemon_configuration_is_supported(self):
        self.config.unlink()
        self.prepare()
        self.assertEqual(json.loads(self.config.read_text())["data-root"], str(self.target))
        self.assertEqual(self.config.stat().st_mode & 0o777, 0o644)

    def test_os_filesystem_is_rejected_before_docker_or_writes(self):
        self.same_filesystem.return_value = True
        with self.assertRaisesRegex(ValueError, "OS filesystem"):
            self.prepare()
        self.assertEqual(self.info_count, 0)
        self.assertFalse(self.target.exists())
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertEqual(self.systemctl(), [])

    def test_insufficient_space_is_rejected_before_docker_or_writes(self):
        self.disk_usage.return_value = SimpleNamespace(free=63 * storage.GIB)
        with self.assertRaisesRegex(ValueError, "at least 64 GiB"):
            self.prepare()
        self.assertEqual(self.info_count, 0)
        self.assertFalse(self.target.exists())
        self.assertEqual(self.systemctl(), [])

    def test_exact_threshold_and_explicit_lower_threshold_are_supported(self):
        self.disk_usage.return_value = SimpleNamespace(free=64 * storage.GIB)
        self.prepare()
        self.disk_usage.return_value = SimpleNamespace(free=32 * storage.GIB)
        self.initial_root = str(self.target)
        self.initial_driver = "overlay2"
        self.info_count = 0
        self.prepare(minimum_free_gib=32)

    def assert_low_space_child_is_rejected(self, *, already_configured):
        scratch = self.mount / "cetirun-build-tmp"
        for destination in (self.target, scratch):
            with self.subTest(destination=destination, already_configured=already_configured):
                self.target.mkdir(exist_ok=True)
                scratch.mkdir(exist_ok=True)
                scratch.chmod(0o700)
                self.config.write_bytes(self.original)
                self.info_count = 0
                self.commands.clear()
                self.initial_root = str(self.target) if already_configured else "/var/lib/docker"
                self.initial_driver = "overlay2" if already_configured else "overlayfs"
                # The parent has ample space, but either non-OS child mount can
                # independently be below the required capacity.
                self.disk_usage.side_effect = lambda path: SimpleNamespace(
                    free=(63 if Path(path) == destination else 100) * storage.GIB
                )
                with self.assertRaisesRegex(ValueError, "at least 64 GiB") as error:
                    self.prepare()
                self.assertIn(str(destination), str(error.exception))
                self.disk_usage.assert_any_call(destination)
                self.assertEqual(self.config.read_bytes(), self.original)
                self.assertEqual(self.systemctl(), [])
                self.assertEqual(scratch.stat().st_mode & 0o7777, 0o700)

    def test_existing_child_mounts_require_their_own_free_space(self):
        self.assert_low_space_child_is_rejected(already_configured=False)

    def test_already_configured_storage_still_checks_child_capacity(self):
        self.assert_low_space_child_is_rejected(already_configured=True)

    def test_new_write_destinations_are_checked_before_daemon_changes(self):
        scratch = self.mount / "cetirun-build-tmp"
        for destination in (self.target, scratch):
            with self.subTest(destination=destination):
                for directory in (self.target, scratch):
                    if directory.exists():
                        directory.rmdir()
                self.info_count = 0
                self.commands.clear()
                self.disk_usage.side_effect = lambda path: SimpleNamespace(
                    free=(63 if Path(path) == destination else 100) * storage.GIB
                )
                with self.assertRaisesRegex(ValueError, "at least 64 GiB") as error:
                    self.prepare()
                self.assertIn(str(destination), str(error.exception))
                self.disk_usage.assert_any_call(destination)
                self.assertEqual(self.config.read_bytes(), self.original)
                self.assertEqual(self.systemctl(), [])

    def test_child_mounts_at_the_capacity_threshold_are_accepted(self):
        scratch = self.mount / "cetirun-build-tmp"
        self.target.mkdir()
        scratch.mkdir()
        self.disk_usage.side_effect = lambda path: SimpleNamespace(
            free=(100 if Path(path) == self.mount else 64) * storage.GIB
        )
        self.prepare()
        for destination in (self.target, scratch):
            self.disk_usage.assert_any_call(destination)

    def test_non_positive_threshold_and_missing_mount_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "positive"):
            self.prepare(minimum_free_gib=0)
        with self.assertRaisesRegex(ValueError, "existing absolute directory"):
            storage.prepare(self.mount / "missing", config_path=self.config)
        with self.assertRaisesRegex(ValueError, "existing absolute directory"):
            storage.prepare(Path("relative"), config_path=self.config)
        self.assertEqual(self.commands, [])

    def test_running_containers_are_not_interrupted(self):
        self.containers = "synthetic-container\n"
        with self.assertRaisesRegex(ValueError, "containers are running"):
            self.prepare()
        self.assertEqual(self.systemctl(), [])
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertFalse(self.target.exists())

    def test_invalid_daemon_config_is_not_replaced(self):
        for content in (b"not-json", b"[]", b'{"features": false}'):
            with self.subTest(content=content):
                self.config.write_bytes(content)
                with self.assertRaises(ValueError):
                    self.prepare()
                self.assertEqual(self.config.read_bytes(), content)
                self.assertEqual(self.systemctl(), [])

    def test_symlinks_and_file_targets_are_rejected(self):
        for name in ("cetirun-docker", "cetirun-build-tmp"):
            with self.subTest(name=name):
                path = self.mount / name
                path.symlink_to(self.root, target_is_directory=True)
                with self.assertRaisesRegex(ValueError, "symlink"):
                    self.prepare()
                path.unlink()
                path.write_text("fixture")
                with self.assertRaisesRegex(ValueError, "non-directory"):
                    self.prepare()
                path.unlink()
        self.assertEqual(self.info_count, 0)
        self.assertEqual(self.systemctl(), [])

    def test_nested_root_filesystem_is_rejected(self):
        for root_directory in (self.target, self.mount / "cetirun-build-tmp"):
            with self.subTest(root_directory=root_directory):
                self.info_count = 0
                self.same_filesystem.side_effect = lambda left, right: left == root_directory
                with self.assertRaisesRegex(ValueError, "data root or build scratch is on the OS"):
                    self.prepare()
                self.assertEqual(self.config.read_bytes(), self.original)
                self.assertEqual(self.systemctl(), [])

    def test_symlinked_daemon_configuration_is_not_replaced(self):
        self.config.unlink()
        original_path = self.root / "original-daemon.json"
        original_path.write_bytes(self.original)
        self.config.symlink_to(original_path)
        with self.assertRaisesRegex(ValueError, "regular file, not a symlink"):
            self.prepare()
        self.assertTrue(self.config.is_symlink())
        self.assertEqual(original_path.read_bytes(), self.original)
        self.assertEqual(self.systemctl(), [])

    def test_already_configured_daemon_is_not_restarted(self):
        self.target.mkdir()
        self.initial_root = str(self.target)
        self.initial_driver = "overlay2"
        self.prepare()
        self.assertEqual(self.systemctl(), [])
        self.assertEqual(self.config.read_bytes(), self.original)

    def test_daemon_validation_failure_does_not_stop_docker(self):
        self.fail_validate = True
        with self.assertRaises(subprocess.CalledProcessError):
            self.prepare()
        self.assertEqual(self.systemctl(), [])
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertEqual(list(self.config.parent.glob("cetirun-docker-*")), [])

    def test_start_failure_restores_original_configuration(self):
        self.fail_start = True
        with self.assertRaisesRegex(RuntimeError, "restored the original"):
            self.prepare()
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertEqual(self.systemctl()[-1], ["systemctl", "restart", *storage.UNITS])

    def test_start_failure_removes_new_config_if_none_existed(self):
        self.config.unlink()
        self.fail_start = True
        with self.assertRaisesRegex(RuntimeError, "restored the original"):
            self.prepare()
        self.assertFalse(self.config.exists())

    def test_actual_daemon_storage_is_verified_and_failure_rolls_back(self):
        for root, driver in (("/var/lib/docker", "overlay2"), (str(self.target), "overlayfs")):
            with self.subTest(root=root, driver=driver):
                self.info_count = 0
                self.final_root, self.final_driver = root, driver
                with self.assertRaisesRegex(RuntimeError, "did not activate"):
                    self.prepare()
                self.assertEqual(self.config.read_bytes(), self.original)

    def test_rollback_failure_is_reported_without_hiding_original_failure(self):
        self.fail_start = self.fail_rollback = True
        with self.assertRaisesRegex(RuntimeError, "rollback also failed"):
            self.prepare()
        self.assertEqual(self.config.read_bytes(), self.original)

    def test_cli_requires_a_privileged_linux_agent(self):
        for platform, uid in (("darwin", 0), ("linux", 1000)):
            with self.subTest(platform=platform, uid=uid):
                with patch.object(storage.sys, "platform", platform), patch.object(storage.os, "geteuid", return_value=uid):
                    with redirect_stderr(io.StringIO()) as errors:
                        self.assertEqual(storage.main([]), 1)
                self.assertIn("disposable Linux CI agent", errors.getvalue())
        self.assertEqual(self.commands, [])

    def test_privileged_cli_passes_explicit_mount_and_space_threshold(self):
        with patch.object(storage.sys, "platform", "linux"), patch.object(storage.os, "geteuid", return_value=0):
            with patch.object(storage, "prepare", return_value=self.target) as prepare:
                self.assertEqual(storage.main([
                    "--storage-mount", str(self.mount), "--minimum-free-gib", "32",
                ]), 0)
        prepare.assert_called_once_with(self.mount, 32)
        self.assertEqual(self.commands, [])


class ReleaseStorageContractTests(unittest.TestCase):
    def test_storage_is_prepared_before_builds_and_scratch_is_exported(self):
        root = Path(__file__).resolve().parents[3]
        pipeline = (root / ".pipelines/release-mcr.yml").read_text()
        prepare = pipeline.index("sudo -n python3 -B scripts/ci/prepare_docker_storage.py")
        build = pipeline.index("scripts/cetirun-release.py publish-images")
        self.assertLess(prepare, build)
        self.assertIn("dockerStorageMount: /mnt", pipeline)
        self.assertIn("DOCKER_STORAGE_MOUNT: $(dockerStorageMount)", pipeline)
        self.assertIn("##vso[task.setvariable variable=TMPDIR]${DOCKER_STORAGE_MOUNT}/cetirun-build-tmp", pipeline)
        self.assertIn("timeoutInMinutes: 180", pipeline)
        self.assertNotIn("\nconcurrency:", pipeline)


if __name__ == "__main__":
    unittest.main()
