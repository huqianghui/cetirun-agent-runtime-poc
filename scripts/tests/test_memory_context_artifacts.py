# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline checks for Memory asset build and publication wiring."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
BUILD = ROOT / "scripts/memory-context-docker-build.sh"


class MemoryArtifactTests(unittest.TestCase):
    def test_codeql_builds_every_application_go_module(self):
        workflow = (ROOT / ".github/workflows/codeql.yml").read_text()
        for manifest in (ROOT / "app").glob("*/go.mod"):
            module = manifest.parent.relative_to(ROOT).as_posix()
            self.assertIn(f"(cd {module} && go build -a ./...)", workflow)

    @unittest.skipUnless(shutil.which("helm"), "requires Helm")
    def test_operator_defaults_and_image_override(self):
        chart = ROOT / "app/cetirun-operator/chart"
        for values, expected in (
            ({}, ["--memory-context-enabled=true", "--memory-context-image="]),
            ({"memoryContext": {"enabled": False}}, ["--memory-context-enabled=false", "--memory-context-image="]),
            ({"memoryContext": {"image": "registry.example.com/memory:1.2.3"}}, ["--memory-context-enabled=true", "--memory-context-image=registry.example.com/memory:1.2.3"]),
            ({"global": {"memoryContext": {"image": "registry.example.com/memory:2.3.4"}}}, ["--memory-context-enabled=true", "--memory-context-image=registry.example.com/memory:2.3.4"]),
            ({"global": {"memoryContext": {"image": "registry.example.com/memory:2.3.4"}}, "memoryContext": {"image": "registry.example.com/memory:3.4.5"}}, ["--memory-context-enabled=true", "--memory-context-image=registry.example.com/memory:3.4.5"]),
        ):
            with self.subTest(values=values):
                result = subprocess.run(
                    ["helm", "template", "fixture", str(chart), "--values", "-", "--show-only", "templates/deployment.yaml"],
                    input=json.dumps({**values, "cetiadmin": {"enabled": False}}), capture_output=True, text=True, timeout=30,
                    env={**os.environ, "KUBECONFIG": os.devnull, "HELM_REGISTRY_CONFIG": os.devnull},
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                args = yaml.safe_load(result.stdout)["spec"]["template"]["spec"]["containers"][0]["args"]
                self.assertEqual([arg for arg in args if arg.startswith("--memory-context-")], expected)

    def test_help_needs_no_tools_or_registry(self):
        for script in (BUILD,):
            result = subprocess.run(
                ["/bin/bash", str(script), "--help"],
                env={"PATH": "/not-installed"}, capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Memory Context", result.stdout)

    def test_invalid_inputs_do_not_call_docker_or_helm(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-memory-invalid-") as temp:
            for name in ("docker", "helm"):
                executable = Path(temp) / name
                executable.write_text("#!/bin/sh\nexit 99\n")
                executable.chmod(0o755)
            for script, key, value in (
                (BUILD, "REGISTRY", "https://registry.example.com"),
                (BUILD, "PUSH", "maybe"),
                (BUILD, "VERSION", "bad tag"),
                (BUILD, "PLATFORMS", "linux/unknown"),
                (BUILD, "PLATFORMS", "linux/amd64,linux/arm64"),


            ):
                env = {
                    **os.environ, "PATH": f"{temp}:{os.environ['PATH']}",
                    "REGISTRY": "registry.example.com", "PUSH": "false",
                    "VERSION": "0.1.1", "GIT_SHA": "fixture", key: value,
                }
                result = subprocess.run(["bash", str(script)], env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 2, f"{script.name} {key}={value}: {result.stderr}")

    def test_local_build_does_not_publish(self):
        with tempfile.TemporaryDirectory(prefix="cetirun-memory-build-") as temp:
            log = Path(temp) / "arguments.json"
            executable = Path(temp) / "docker"
            executable.write_text(
                "#!/usr/bin/env python3\nimport json, os, sys\n"
                "with open(os.environ['MEMORY_TEST_LOG'], 'w') as output:\n"
                "    json.dump(sys.argv[1:], output)\n"
            )
            executable.chmod(0o755)
            env = {
                **os.environ, "PATH": f"{temp}:{os.environ['PATH']}",
                "REGISTRY": "registry.example.com", "VERSION": "0.1.1",
                "GIT_SHA": "fixture", "PUSH": "false", "PLATFORMS": "linux/arm64",
                "MEMORY_TEST_LOG": str(log),
            }
            result = subprocess.run(["bash", str(BUILD)], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            command = json.loads(log.read_text())
            self.assertEqual(command[:2], ["buildx", "build"])
            self.assertIn(str(ROOT / "app/cetirun-memory-context/Dockerfile"), command)
            self.assertIn("registry.example.com/cetirun-memory-context:0.1.1", command)
            self.assertIn("--load", command)
            self.assertNotIn("--push", command)

    def test_memory_image_has_no_standalone_policy_chart(self):
        pipeline = (ROOT / ".pipelines/release-mcr.yml").read_text()
        inventory = json.loads((ROOT / "scripts/release-artifacts.json").read_text())
        memory = next(entry for entry in inventory["images"] if entry["name"] == "cetirun-memory-context")
        self.assertEqual(memory["script"], "scripts/memory-context-docker-build.sh")
        self.assertNotIn("cetirun-memory-context", [entry["name"] for entry in inventory["charts"]])
        self.assertIn("scripts/cetirun-release.py publish-images", pipeline)
        self.assertIn("scripts/cetirun-release.py list images", pipeline)
        self.assertNotIn("app/cetirun-memory-context/chart", pipeline)
        makefile = (ROOT / "Makefile").read_text()
        self.assertIn("scripts/cetirun-release.py $@", makefile)
        self.assertIn("build-images publish-images publish-charts publish:", makefile)
        self.assertNotIn("memory-context-helm-release", makefile)
        self.assertFalse((ROOT / "app/cetirun-memory-context/chart/Chart.yaml").exists())


if __name__ == "__main__":
    unittest.main()
