# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import Mock, patch


HELPER = Path(__file__).resolve().parents[2] / "app/cetirun-chart/agents/hermes/pm/runtime.py"
spec = importlib.util.spec_from_file_location("cetirun_hermes_pm_runtime", HELPER)
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class PMSelectionTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.facts = self.root / "facts.json"
        self.environment = self.root / "generation/venv"
        self.environment.mkdir(parents=True)
        (self.environment / "pyvenv.cfg").write_text("version = 3.14.9\n")
        self.current = Mock(return_value=True)
        self.pm = types.ModuleType("pm")
        self.pm.venv_is_current = self.current
        self.pm.sync_venv = Mock(side_effect=AssertionError("startup cannot rebuild dependencies"))
        environments = types.ModuleType("pm.environments")
        environments.runtime_facts_path = lambda root: self.facts
        environments.selected_venv = lambda root: self.environment
        lock = types.ModuleType("pm.lock")
        facts_path = self.facts

        class Facts:
            def __init__(self, path, strict):
                assert path == facts_path and strict

            def get(self, key):
                return json.loads(facts_path.read_text())["packages"].get(key)

        lock.Facts = Facts
        modules = patch.dict(sys.modules, {"pm": self.pm, "pm.environments": environments, "pm.lock": lock})
        modules.start()
        self.addCleanup(modules.stop)

    def record(self, **changes):
        fact = {"environment": str(self.environment), "extras": list(runtime.REQUIRED_EXTRAS), "stamp": "current"}
        fact.update(changes)
        self.facts.write_text(json.dumps({"packages": {"venv": fact}}))

    def test_fresh_home_uses_verified_image_environment_without_creating_selection(self):
        self.assertEqual(runtime.validate_selection(self.root), self.environment)
        self.assertFalse(self.facts.exists())
        self.current.assert_called_once_with(extras=list(runtime.REQUIRED_EXTRAS), project_root=self.root)

    def test_prepared_generation_is_reused_without_changing_its_selection(self):
        self.record(extras=[*runtime.REQUIRED_EXTRAS, "documents"])
        before = self.facts.read_bytes()
        self.assertEqual(runtime.validate_selection(self.root), self.environment)
        self.assertEqual(self.facts.read_bytes(), before)
        self.pm.sync_venv.assert_not_called()

    def test_upgrade_mismatch_fails_without_mutation_or_fallback(self):
        self.record()
        self.current.return_value = False
        before = self.facts.read_bytes()
        with self.assertRaisesRegex(RuntimeError, "prepare them"):
            runtime.validate_selection(self.root)
        self.assertEqual(self.facts.read_bytes(), before)
        self.pm.sync_venv.assert_not_called()

    def test_missing_generation_and_platform_extras_are_rejected(self):
        for changes in ({"environment": None}, {"extras": ["all"]}, {"extras": None}):
            with self.subTest(changes=changes):
                self.record(**changes)
                before = self.facts.read_bytes()
                with self.assertRaises(RuntimeError):
                    runtime.validate_selection(self.root)
                self.assertEqual(self.facts.read_bytes(), before)
        self.current.assert_not_called()

    def test_missing_environment_files_do_not_trigger_provisioning(self):
        self.record()
        (self.environment / "pyvenv.cfg").unlink()
        with self.assertRaisesRegex(RuntimeError, "environment is missing"):
            runtime.validate_selection(self.root)
        self.pm.sync_venv.assert_not_called()

    def test_identity_checks_internal_uv_without_exposing_it_as_an_application(self):
        (self.root / "cetirun-image.json").write_text(json.dumps({"version": "0.22.7", "revision": "test-commit"}))
        (self.root / "install-stamp.json").write_text(json.dumps({"updateMechanism": "external", "distribution": "docker"}))
        version = types.ModuleType("hermes_cli.version_info")
        version.get_version_info = lambda: types.SimpleNamespace(base_version="0.22.7", commit="test-commit")
        self.pm.is_installed = Mock(return_value=True)

        def installed_package(name):
            if name == "uv":
                raise ValueError("uv is internal PM tooling, not an application package")
            return types.SimpleNamespace(binary=self.root / "tools" / name)

        self.pm.installed_package = Mock(side_effect=installed_package)
        with patch.dict(sys.modules, {"hermes_cli": types.ModuleType("hermes_cli"), "hermes_cli.version_info": version}), \
                patch.object(sys, "version_info", (3, 14, 9)):
            runtime.validate_identity(self.root)
        self.assertEqual([call.args[0] for call in self.pm.installed_package.call_args_list], ["python", "node", "npm"])


if __name__ == "__main__":
    unittest.main()
