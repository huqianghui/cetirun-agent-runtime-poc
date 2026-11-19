# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Run the inventory's actual builders against a local Docker command double."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import tarfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/cetirun-release.py"
IMAGES = {"cetirun-base", "openclaw-gateway", "hermes-gateway", "cetirun-desktop", "cetirun-operator",
          "cetirun-ai-gateway", "cetirun-agent-broker", "cetirun-connections-mcp", "cetirun-terminal",
          "cetirun-portal", "cetirun-vscode", "cetirun-pi-agent", "cetirun-memory-context",
          "cetirun-observability"}
CHARTS = {"cetirun", "cetirun-istio", "cetirun-operator", "cetirun-portal", "cetirun-platform",
          "cetirun-ai-gateway", "cetirun-agent-broker", "cetirun-connections-mcp", "cetirun-observability"}
DOCKER = r'''
import json, os, pathlib, sys
args = sys.argv[1:]
log = pathlib.Path(os.environ["ARTIFACT_COMMANDS"])
previous = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
with log.open("a") as output:
    output.write(json.dumps(args) + "\n")
if args[:2] == ["buildx", "inspect"]:
    print("Driver: " + os.environ.get("ARTIFACT_BUILDX_DRIVER", "docker"))
if args[:3] == ["buildx", "imagetools", "inspect"]:
    reference = args[3]
    if os.environ.get("ARTIFACT_REGISTRY_ERROR"):
        print(os.environ["ARTIFACT_REGISTRY_ERROR"], file=sys.stderr)
        sys.exit(1)
    if reference == os.environ.get("ARTIFACT_EXISTS"):
        sys.exit(0)
    if not any(item[:2] == ["buildx", "build"] and "--push" in item and reference in item for item in previous):
        print("manifest unknown", file=sys.stderr)
        sys.exit(1)
if args[:2] == ["buildx", "build"] and os.environ.get("ARTIFACT_FAIL_BUILD") in args:
    sys.exit(7)
if args[:2] == ["buildx", "build"] and os.environ.get("ARTIFACT_SOURCE_VERSION"):
    root = pathlib.Path(args[-1])
    while not (root / "scripts/cetirun-version.py").is_file():
        root = root.parent
    version = os.environ["ARTIFACT_SOURCE_VERSION"]
    assert f'"chart-version", "{version}"' in (root / "app/cetirun-operator/cmd/main.go").read_text()
    assert f'"version": "{version}"' in (root / "examples/pi-agent/package.json").read_text()
    assert (root / "scripts/cetirun-release.py").stat().st_mode & 0o111
    source = pathlib.Path(os.environ["ARTIFACT_ORIGINAL_SOURCE"])
    assert root != source
    assert not (root / ".git").exists()
    assert not (root / "app/cetirun-portal/web/node_modules").exists()
    assert not (root / "site/node_modules").exists()
    for relative in (".", "app", "app/cetirun-chart", "scripts"):
        assert (root / relative).stat().st_mode == (source / relative).stat().st_mode
'''

HELM = r'''
import json, os, pathlib, shutil, subprocess, sys
args = sys.argv[1:]
store = pathlib.Path(os.environ["ARTIFACT_CHART_STORE"])
with pathlib.Path(os.environ["ARTIFACT_COMMANDS"]).open("a") as output:
    output.write(json.dumps(["helm", *args]) + "\n")
if args[:2] == ["show", "chart"]:
    reference = args[2].removeprefix("oci://") + ":" + args[args.index("--version") + 1]
    if os.environ.get("ARTIFACT_HELM_ERROR"):
        print(os.environ["ARTIFACT_HELM_ERROR"], file=sys.stderr)
        sys.exit(1)
    if reference == os.environ.get("ARTIFACT_EXISTS"):
        sys.exit(0)
    print(reference + ": not found", file=sys.stderr)
    sys.exit(1)
elif args[0] == "push":
    shutil.copy2(args[1], store / pathlib.Path(args[1]).name)
elif args[0] == "pull":
    name = args[1].rsplit("/", 1)[1] + "-" + args[args.index("--version") + 1] + ".tgz"
    shutil.copy2(store / name, pathlib.Path(args[args.index("--destination") + 1]) / name)
elif args[0] in ("lint", "template", "package", "dependency"):
    sys.exit(subprocess.run([os.environ["ARTIFACT_REAL_HELM"], *args], check=False).returncode)
else:
    sys.exit("Unexpected Helm operation: " + repr(args))
'''


class ReleaseInventoryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cetirun-artifacts-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.log = self.root / "commands.jsonl"
        docker = self.root / "docker"
        docker.write_text(f"#!{sys.executable}\n" + DOCKER)
        docker.chmod(0o755)
        version = subprocess.run([sys.executable, "-B", str(ROOT / "scripts/cetirun-version.py"), "get"], capture_output=True, text=True, check=True).stdout.strip()
        self.env = {**os.environ, "PATH": f"{self.root}:{os.environ['PATH']}", "REGISTRY": "registry.example.com/test",
                    "VERSION": version, "GIT_SHA": "a" * 40, "ACCEPT_VSCODE_SERVER_LICENSE_TERMS": "true",
                    "ARTIFACT_COMMANDS": str(self.log), "KUBECONFIG": os.devnull, "HELM_REGISTRY_CONFIG": os.devnull}

    def run_release(self, *args, success=True):
        result = subprocess.run([sys.executable, "-B", str(SCRIPT), *args], env=self.env, cwd=ROOT, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def commands(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def image(self, name):
        return f"{self.env['REGISTRY']}/{name}:{self.env['VERSION']}"

    def fake_registry(self):
        real = shutil.which("helm")
        if not real:
            self.skipTest("requires existing Helm")
        store = self.root / "registry"
        store.mkdir()
        helm = self.root / "helm"
        helm.write_text(f"#!{sys.executable}\n" + HELM)
        helm.chmod(0o755)
        self.env.update(ARTIFACT_REAL_HELM=real, ARTIFACT_CHART_STORE=str(store),
                        HELM_REPOSITORY_CONFIG=os.devnull)
        return store

    def assert_package(self, archive, registry, version, app_version=None):
        with tarfile.open(archive) as package:
            chart_name = archive.name.removesuffix(f"-{version}.tgz")
            metadata = yaml.safe_load(package.extractfile(f"{chart_name}/Chart.yaml"))
            self.assertEqual(metadata["version"], version)
            self.assertEqual(metadata["appVersion"], app_version or version)
            values = yaml.safe_load(package.extractfile(f"{chart_name}/values.yaml"))
            if chart_name == "cetirun-platform":
                self.assertEqual({d["version"] for d in metadata["dependencies"]}, {version})
                lock = yaml.safe_load(package.extractfile(f"{chart_name}/Chart.lock"))
                self.assertEqual({d["version"] for d in lock["dependencies"]}, {version})
                for dependency in metadata["dependencies"]:
                    child = yaml.safe_load(package.extractfile(f"{chart_name}/charts/{dependency['name']}/Chart.yaml"))
                    self.assertEqual((child["version"], child["appVersion"]), (version, app_version or version))
            elif chart_name == "cetirun":
                for key, image in (("openclaw", "openclaw-gateway"), ("hermes", "hermes-gateway"),
                                   ("desktop", "cetirun-desktop"), ("ttyd", "cetirun-terminal")):
                    self.assertEqual(values[key]["image"]["repository"], f"{registry}/{image}")
            elif chart_name != "cetirun-istio":
                self.assertEqual(values["image"]["repository"], f"{registry}/{chart_name}")
            if chart_name == "cetirun-operator":
                self.assertEqual(values["cetirun"]["chart"], {"repoURL": f"oci://{registry}/helm/cetirun", "version": version})
            for entry in package.getmembers():
                if entry.isfile() and entry.name.endswith(("values.yaml", ".tpl")):
                    text = package.extractfile(entry).read().decode()
                    self.assertNotIn("cetirun.azurecr.io", text)
                    self.assertNotIn("ghcr.io/azure/cetirun", text)
                    if registry != "mcr.microsoft.com/aks/ai-runtime/cetirun":
                        self.assertNotIn("mcr.microsoft.com/aks/ai-runtime/cetirun", text)
        entry = next(item for item in json.loads((ROOT / "scripts/release-artifacts.json").read_text())["charts"]
                     if item["name"] == chart_name)
        options = [item for key, value in entry.get("lintValues", {}).items() for item in ("--set", f"{key}={value}")]
        if entry.get("namespace"):
            options += ["--namespace", entry["namespace"]]
        if entry.get("valuesFile"):
            options += ["--values", str(ROOT / entry["valuesFile"])]
        result = subprocess.run([self.env.get("ARTIFACT_REAL_HELM", shutil.which("helm")), "template", "release-test", str(archive), *options],
                                env=self.env, cwd=ROOT, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)
        documents = list(yaml.safe_load_all(result.stdout))
        images = []

        def inspect(value):
            if isinstance(value, dict):
                for key, child in value.items():
                    if key == "image" and isinstance(child, str):
                        name = child.rsplit("/", 1)[-1].split(":", 1)[0]
                        if name in IMAGES:
                            images.append(child)
                            self.assertEqual(child, f"{registry}/{name}:{app_version or version}")
                    inspect(child)
            elif isinstance(value, list):
                for child in value:
                    inspect(child)

        inspect(documents)
        if chart_name != "cetirun-istio":
            self.assertTrue(images, "no first-party image found in the packaged chart render")
        if chart_name in ("cetirun-operator", "cetirun-platform"):
            self.assertIn(f"--chart-repo-url=oci://{registry}/helm/cetirun", result.stdout)
            self.assertIn(f"--chart-version={version}", result.stdout)

    def test_image_builds_filter_source_with_and_without_version_override_without_helm(self):
        tracked = subprocess.check_output(["git", "ls-files", "-z"], cwd=ROOT).decode().split("\0")
        before = {name: ((ROOT / name).read_bytes(), (ROOT / name).stat().st_mode) for name in tracked if (ROOT / name).is_file()}
        self.env["ARTIFACT_ORIGINAL_SOURCE"] = str(ROOT)
        helm = self.root / "helm"
        helm.write_text("#!/bin/sh\necho 'Helm must not run for image-only builds' >&2\nexit 97\n")
        helm.chmod(0o755)
        for version in (self.env["VERSION"], "0.1.1-dev.20990101.gabcdef0"):
            with self.subTest(version=version):
                self.env["VERSION"] = self.env["ARTIFACT_SOURCE_VERSION"] = version
                start = len(self.commands())
                result = subprocess.run(["bash", "-c", "umask 077; exec make build-images"], env=self.env, cwd=ROOT, capture_output=True, text=True, timeout=60)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                builds = [c for c in self.commands()[start:] if c[:2] == ["buildx", "build"]]
                self.assertEqual({next(c[i + 1] for i, word in enumerate(c) if word in ("-t", "--tag")) for c in builds},
                                 {self.image(name) for name in IMAGES})
        self.assertEqual(before, {name: ((ROOT / name).read_bytes(), (ROOT / name).stat().st_mode) for name in before})

    def test_istio_publisher_uses_helm_auth_without_docker_auth(self):
        store = self.fake_registry()
        self.env["ARTIFACT_REGISTRY_ERROR"] = "401 Unauthorized: Docker is not logged in"
        result = subprocess.run(["bash", "scripts/istio-helm-release.sh"], env=self.env, cwd=ROOT,
                                capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((store / f"cetirun-istio-{self.env['VERSION']}.tgz").is_file())
        self.assertTrue(all(c[0] == "helm" for c in self.commands()))

    def test_chart_preflight_rejects_existing_tags_and_unknown_errors(self):
        self.fake_registry()
        reference = f"{self.env['REGISTRY']}/helm/cetirun-istio:{self.env['VERSION']}"
        cases = (({"ARTIFACT_EXISTS": reference}, "already exists"),
                 ({"ARTIFACT_HELM_ERROR": "401 Unauthorized"}, "Cannot determine"),
                 ({"ARTIFACT_HELM_ERROR": "connection refused"}, "Cannot determine"))
        for variables, error in cases:
            with self.subTest(variables=variables):
                self.env.pop("ARTIFACT_EXISTS", None)
                self.env.pop("ARTIFACT_HELM_ERROR", None)
                self.env.update(variables)
                start = len(self.commands())
                result = self.run_release("publish-charts", "--chart", "cetirun-istio", success=False)
                self.assertIn(error, result.stderr)
                self.assertEqual(self.commands()[start:], [["helm", "show", "chart", "oci://" + reference.rsplit(":", 1)[0],
                                                          "--version", self.env["VERSION"]]])

    def test_nonstandard_chart_namespace_is_rejected_before_publication(self):
        for flags, env in ((["--chart-namespace", "custom/charts"], {}), ([], {"CHART_NAMESPACE": "charts"})):
            with self.subTest(flags=flags, env=env):
                self.env.update(env)
                result = self.run_release("publish-charts", *flags, success=False)
                self.assertIn("CHART_NAMESPACE must be helm", result.stderr)
                self.assertEqual(self.commands(), [])

    def test_unified_publish_overrides_both_coordinates_with_local_registry_double(self):
        store = self.fake_registry()
        self.env.update(VERSION="1.2.3-dev.1", REGISTRY="registry.example.com:5000/team/test")
        self.run_release("publish")
        for name in CHARTS:
            self.assert_package(store / f"{name}-{self.env['VERSION']}.tgz", self.env["REGISTRY"], self.env["VERSION"])
        builds = [c for c in self.commands() if c[:2] == ["buildx", "build"]]
        self.assertEqual(len(builds), len(IMAGES))
        self.assertTrue(all("--push" in c for c in builds))

    def test_each_chart_publisher_uses_same_packaging_and_defaults(self):
        store = self.fake_registry()
        scripts = {"cetirun": "cetirun", "cetirun-istio": "istio", "cetirun-operator": "operator",
                   "cetirun-portal": "portal", "cetirun-platform": "platform", "cetirun-ai-gateway": "ai-gateway",
                   "cetirun-agent-broker": "agent-broker", "cetirun-connections-mcp": "cetirun-connections-mcp",
                   "cetirun-observability": "cetirun-observability"}
        for registry, version in ((None, None), ("registry.example.com", None),
                                  (None, "1.2.3-dev.1"), ("registry.example.com:5000/team/test", "1.2.3-dev.2")):
            env = dict(self.env)
            for key, value in (("REGISTRY", registry), ("VERSION", version)):
                if value is None:
                    env.pop(key, None)
                else:
                    env[key] = value
            expected_registry = registry or "mcr.microsoft.com/aks/ai-runtime/cetirun"
            expected_version = version or self.env["VERSION"]
            for chart, script in scripts.items():
                with self.subTest(chart=chart, registry=registry, version=version):
                    before = self.commands()
                    result = subprocess.run(["bash", f"scripts/{script}-helm-release.sh"], env=env, cwd=ROOT,
                                            capture_output=True, text=True, timeout=60)
                    if registry is None:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn("MCR is a pull endpoint", result.stderr)
                        self.assertEqual(self.commands(), before, "default MCR publication contacted external tools")
                        result = subprocess.run([sys.executable, "-B", str(SCRIPT), "package-charts", "--chart", chart,
                                                 "--package-dir", str(store)], env=env, cwd=ROOT,
                                                capture_output=True, text=True, timeout=60)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assert_package(store / f"{chart}-{expected_version}.tgz", expected_registry, expected_version)

    def test_explicit_chart_and_app_versions_are_preserved(self):
        store = self.fake_registry()
        self.env.update(CHART_VERSION="1.2.3-dev.1", APP_VERSION="test-image-123")
        result = subprocess.run(["bash", "scripts/operator-helm-release.sh"], env=self.env, cwd=ROOT,
                                capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assert_package(store / "cetirun-operator-1.2.3-dev.1.tgz", self.env["REGISTRY"], "1.2.3-dev.1", "test-image-123")

    def test_each_image_builder_defaults_and_environment_overrides(self):
        entries = json.loads((ROOT / "scripts/release-artifacts.json").read_text())["images"]
        for registry, version in ((None, None), ("registry.example.com:5000/team/test", "1.2.3-dev.1")):
            for entry in entries:
                with self.subTest(image=entry["name"], registry=registry):
                    env = {**self.env, "PUSH": "false", "BUILD_BASE": "false", "BUILD_IMAGE": "false",
                           "BUILD_HERMES": "false", "BUILD_DESKTOP": "false", **entry.get("env", {})}
                    for key in ("IMAGE_TAG", "BASE_IMAGE", "REGISTRY", "VERSION"):
                        env.pop(key, None)
                    if registry:
                        env.update(REGISTRY=registry, VERSION=version)
                    start = len(self.commands())
                    result = subprocess.run(["bash", entry["script"]], env=env, cwd=ROOT, capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    builds = [c for c in self.commands()[start:] if c[:2] == ["buildx", "build"]]
                    self.assertEqual(len(builds), 1)
                    self.assertIn(f"{registry or 'mcr.microsoft.com/aks/ai-runtime/cetirun'}/{entry['name']}:{version or self.env['VERSION']}", builds[0])
                    self.assertNotIn("--push", builds[0])

    def test_component_make_image_coordinates(self):
        for component in ("operator", "portal"):
            for overrides in ({}, {"REGISTRY": "registry.example.com:5000/team/test", "VERSION": "1.2.3-dev.1"}):
                with self.subTest(component=component, overrides=overrides):
                    env = {key: value for key, value in self.env.items() if key not in ("REGISTRY", "VERSION")}
                    result = subprocess.run(["make", "-C", f"app/cetirun-{component}", "docker-build"],
                                            env={**env, **overrides}, cwd=ROOT, capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    command = self.commands()[-1]
                    version = overrides.get("VERSION", self.env["VERSION"])
                    registry = overrides.get("REGISTRY", "mcr.microsoft.com/aks/ai-runtime/cetirun")
                    self.assertIn(f"{registry}/cetirun-{component}:{version}", command)
                    self.assertIn(f"VERSION={version}", command)

    def test_nonbuild_make_targets_do_not_evaluate_release_metadata(self):
        python = self.root / "python3"
        python.write_text("#!/bin/sh\necho 'unexpected metadata lookup' >&2\nexit 97\n")
        python.chmod(0o755)
        env = {key: value for key, value in self.env.items() if key not in ("VERSION", "GIT_SHA", "REGISTRY")}
        for component, targets in (("operator", ["test", "vet"]), ("portal", ["test", "lint"])):
            with self.subTest(component=component):
                result = subprocess.run(["make", "-n", "-C", f"app/cetirun-{component}", *targets],
                                        env=env, cwd=ROOT, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn("unexpected metadata lookup", result.stderr)

    def test_unified_publish_rejects_mcr_before_preflight_even_with_force(self):
        for registry in (None, "mcr.microsoft.com/aks/ai-runtime/cetirun", "mcr.microsoft.com:443/example"):
            if registry is None:
                self.env.pop("REGISTRY", None)
            else:
                self.env["REGISTRY"] = registry
            for command in ("publish", "publish-images", "publish-charts"):
                for flags in ([], ["--force"]):
                    with self.subTest(registry=registry, command=command, flags=flags):
                        result = self.run_release(command, *flags, success=False)
                        self.assertIn("MCR is a pull endpoint", result.stderr)
                        self.assertEqual(self.commands(), [])
        self.env.pop("REGISTRY", None)
        for target in ("publish", "publish-images", "publish-charts"):
            result = subprocess.run(["make", target], env=self.env, cwd=ROOT, capture_output=True, text=True, timeout=30)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("MCR is a pull endpoint", result.stderr)
            self.assertEqual(self.commands(), [])

    def test_image_builders_and_component_push_targets_reject_mcr(self):
        entries = json.loads((ROOT / "scripts/release-artifacts.json").read_text())["images"]
        for entry in entries:
            with self.subTest(image=entry["name"]):
                env = {**self.env, "PUSH": "true", "BUILD_BASE": "false", "BUILD_IMAGE": "false",
                       "BUILD_HERMES": "false", "BUILD_DESKTOP": "false", **entry.get("env", {})}
                env.pop("REGISTRY", None)
                result = subprocess.run(["bash", entry["script"]], env=env, cwd=ROOT,
                                        capture_output=True, text=True, timeout=30)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("MCR is a pull endpoint", result.stderr)
        for component in ("operator", "portal"):
            env = {key: value for key, value in self.env.items() if key != "REGISTRY"}
            result = subprocess.run(["make", "-C", f"app/cetirun-{component}", "docker-push"], env=env, cwd=ROOT,
                                    capture_output=True, text=True, timeout=30)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("MCR is a pull endpoint", result.stderr)
        self.assertFalse(any(c[:2] == ["buildx", "build"] or c[0] == "push" for c in self.commands()))
        for component in ("operator", "portal"):
            result = subprocess.run(["make", "-C", f"app/cetirun-{component}", "docker-push"], env=self.env, cwd=ROOT,
                                    capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual(self.commands()[-1], ["push", self.image("cetirun-" + component)])

    def test_base_image_override_cannot_turn_a_pull_reference_into_a_push(self):
        env = {**self.env, "PUSH": "true", "BUILD_BASE": "true",
               "BASE_IMAGE": "mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-base:1.2.3"}
        result = subprocess.run(["bash", "scripts/cetirun-docker-build.sh"], env=env, cwd=ROOT,
                                capture_output=True, text=True, timeout=30)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("MCR is a pull endpoint", result.stderr)
        self.assertEqual(self.commands(), [])

    def test_all_make_release_targets_and_command_line_precedence(self):
        store = self.fake_registry()
        self.env.update(VERSION="1.2.3-dev.1", REGISTRY="registry.example.com/environment")
        for index, target in enumerate(("build-images", "publish-images", "publish-charts", "publish"), 1):
            with self.subTest(target=target):
                result = subprocess.run(["make", target, f"VERSION=2.3.4-dev.{index}", "REGISTRY=registry.example.com/command/line"],
                                        env=self.env, cwd=ROOT, capture_output=True, text=True, timeout=60)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assert_package(store / "cetirun-platform-2.3.4-dev.4.tgz", "registry.example.com/command/line", "2.3.4-dev.4")

    def test_standalone_deploy_scripts_use_both_coordinates_without_real_tools(self):
        # All kubectl/Helm calls, including writes, terminate at this local log.
        for name in ("helm", "kubectl"):
            tool = self.root / name
            tool.write_text(f"#!{sys.executable}\nimport json, os, sys\n"
                            "with open(os.environ['ARTIFACT_COMMANDS'], 'a') as out:\n"
                            "    out.write(json.dumps(sys.argv) + '\\n')\n")
            tool.chmod(0o755)
        for component in ("operator", "portal"):
            result = subprocess.run(["bash", f"app/cetirun-{component}/deploy/deploy.sh"], env={**self.env, "VERSION": "1.2.3-dev.1", "ISTIO_HOST": "portal.example.com"},
                                    cwd=ROOT, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            upgrade = next(c for c in reversed(self.commands()) if len(c) > 2 and c[1:3] == ["upgrade", "--install"])
            self.assertIn(f"oci://{self.env['REGISTRY']}/helm/cetirun-{component}", upgrade)
            self.assertEqual(upgrade[upgrade.index("--version") + 1], "1.2.3-dev.1")
            self.assertIn(f"image.repository={self.env['REGISTRY']}/cetirun-{component}", upgrade)
            self.assertIn("image.tag=1.2.3-dev.1", upgrade)
            if component == "operator":
                self.assertIn(f"cetirun.chart.repoURL=oci://{self.env['REGISTRY']}/helm/cetirun", upgrade)
                self.assertIn("cetirun.chart.version=1.2.3-dev.1", upgrade)

    def test_inventory_and_plan_do_not_contact_registry(self):
        self.assertEqual(set(self.run_release("list", "images").stdout.splitlines()), IMAGES)
        self.assertEqual(set(self.run_release("list", "charts").stdout.splitlines()), CHARTS)
        self.run_release("check")
        plan = json.loads(self.run_release("plan").stdout)
        self.assertEqual(plan["gitSha"], "a" * 40)
        self.assertEqual(self.commands(), [])

    def test_release_pipeline_shell_syntax(self):
        pipeline = yaml.safe_load((ROOT / ".pipelines/release-mcr.yml").read_text())
        for step in pipeline["jobs"][0]["steps"]:
            script = step.get("bash") or step.get("inputs", {}).get("inlineScript")
            if script:
                result = subprocess.run(["bash", "-n"], input=script, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, f"{step['displayName']}: {result.stderr}")

    def test_actual_builders_build_each_image_once_without_push(self):
        result = subprocess.run(["make", "build-images"], env=self.env, cwd=ROOT, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        builds = [command for command in self.commands() if command[:2] == ["buildx", "build"]]
        self.assertEqual(len(builds), 14)
        tags = {command[command.index("-t") + 1] if "-t" in command else command[command.index("--tag") + 1] for command in builds}
        self.assertEqual(tags, {self.image(name) for name in IMAGES})
        for command in builds:
            self.assertIn("--load", command)
            self.assertNotIn("--push", command)
            self.assertTrue(any("a" * 40 in argument for argument in command), "source SHA not passed to image build")
        self.assertIn(self.image("cetirun-base"), builds[0])
        vscode = next(command for command in builds if self.image("cetirun-vscode") in command)
        self.assertIn("ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true", vscode)

    def test_sbom_generator_override_reaches_all_attestation_builders(self):
        generator = "registry.example.com/tools/sbom-generator@sha256:" + "a" * 64
        self.env.update(ARTIFACT_BUILDX_DRIVER="docker-container", SBOM_GENERATOR=generator)
        self.run_release("publish-images")
        attested = {
            "cetirun-base", "openclaw-gateway", "hermes-gateway", "cetirun-desktop",
            "cetirun-operator", "cetirun-ai-gateway", "cetirun-agent-broker",
            "cetirun-portal", "cetirun-pi-agent",
        }
        builds = [command for command in self.commands() if command[:2] == ["buildx", "build"]]
        self.assertEqual(len(builds), 14)
        for image in attested:
            command = next(command for command in builds if self.image(image) in command)
            self.assertIn("--provenance=true", command)
            self.assertIn(f"--sbom=generator={generator}", command)
            self.assertNotIn("--sbom=true", command)

    def test_default_sbom_generator_is_retained(self):
        self.env["ARTIFACT_BUILDX_DRIVER"] = "docker-container"
        self.run_release("publish-images")
        builds = [command for command in self.commands() if command[:2] == ["buildx", "build"]]
        for image in ("cetirun-base", "openclaw-gateway", "hermes-gateway", "cetirun-desktop",
                      "cetirun-operator", "cetirun-ai-gateway", "cetirun-agent-broker",
                      "cetirun-portal", "cetirun-pi-agent"):
            command = next(command for command in builds if self.image(image) in command)
            self.assertIn("--provenance=true", command)
            self.assertIn("--sbom=true", command)

    def test_sbom_generator_override_rejects_a_non_attesting_driver(self):
        generator = "registry.example.com/tools/sbom-generator@sha256:" + "a" * 64
        self.env["SBOM_GENERATOR"] = generator
        result = self.run_release("publish-images", success=False)
        self.assertIn("requires a docker-container Buildx driver", result.stderr)
        self.assertFalse(any(command[:2] == ["buildx", "build"] for command in self.commands()))

    def test_sbom_generator_override_rejects_direct_local_builds(self):
        generator = "registry.example.com/tools/sbom-generator@sha256:" + "a" * 64
        for script in (
            "scripts/cetirun-docker-build.sh", "scripts/ai-gateway-docker-build.sh",
            "scripts/operator-docker-build.sh", "scripts/portal-docker-build.sh",
            "scripts/agent-broker-docker-build.sh", "scripts/pi-agent-docker-build.sh",
        ):
            with self.subTest(script=script):
                result = subprocess.run(
                    ["bash", script],
                    env={**self.env, "SBOM_GENERATOR": generator, "PUSH": "false",
                         "BUILD_BASE": "false", "BUILD_IMAGE": "false",
                         "BUILD_HERMES": "false", "BUILD_DESKTOP": "false"},
                    cwd=ROOT, capture_output=True, text=True, timeout=30,
                )
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertIn("SBOM_GENERATOR requires PUSH=true", result.stderr)
        self.assertEqual(self.commands(), [])

    def test_invalid_sbom_generator_stops_release_and_direct_builders_before_docker(self):
        invalid = "registry.example.com/tools/sbom-generator:mutable"
        self.env["SBOM_GENERATOR"] = invalid
        result = self.run_release("publish-images", success=False)
        self.assertIn("SBOM_GENERATOR must be", result.stderr)
        self.assertEqual(self.commands(), [])
        for script in (
            "scripts/cetirun-docker-build.sh", "scripts/ai-gateway-docker-build.sh",
            "scripts/operator-docker-build.sh", "scripts/portal-docker-build.sh",
            "scripts/agent-broker-docker-build.sh", "scripts/pi-agent-docker-build.sh",
        ):
            with self.subTest(script=script):
                result = subprocess.run(
                    ["bash", script], env={**self.env, "BUILD_BASE": "false", "BUILD_IMAGE": "false",
                                             "BUILD_HERMES": "false", "BUILD_DESKTOP": "false"},
                    cwd=ROOT, capture_output=True, text=True, timeout=30,
                )
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertIn("SBOM_GENERATOR must be", result.stderr)
        self.assertEqual(self.commands(), [])

    def test_license_and_coordinates_fail_before_docker(self):
        for key, value in (("ACCEPT_VSCODE_SERVER_LICENSE_TERMS", "false"), ("REGISTRY", ""),
                           ("VERSION", "latest"), ("GIT_SHA", "unknown")):
            with self.subTest(key=key):
                previous = self.env[key]
                self.env[key] = value
                self.run_release("publish", success=False)
                self.assertEqual(self.commands(), [])
                self.env[key] = previous

    def test_optional_vscode_exclusion_skips_license_gate_and_image(self):
        self.env.pop("ACCEPT_VSCODE_SERVER_LICENSE_TERMS")
        plan = json.loads(self.run_release("plan", "--exclude-image", "cetirun-vscode").stdout)
        self.assertNotIn("cetirun-vscode", {entry["name"] for entry in plan["images"]})
        self.run_release("build-images", "--exclude-image", "cetirun-vscode")
        builds = [command for command in self.commands() if command[:2] == ["buildx", "build"]]
        self.assertEqual(len(builds), 13)
        self.assertFalse(any(self.image("cetirun-vscode") in command for command in builds))

    def test_invalid_image_exclusions_fail_before_docker(self):
        cases = (("unknown", ["not-in-inventory"], "unknown image"),
                 ("required", ["cetirun-portal"], "only exclude optional"),
                 ("duplicate", ["cetirun-vscode", "cetirun-vscode"], "duplicate"))
        for name, excluded, message in cases:
            with self.subTest(name=name):
                result = self.run_release("publish-images", *(item for image in excluded for item in ("--exclude-image", image)), success=False)
                self.assertIn(message, result.stderr)
                self.assertEqual(self.commands(), [])

    def test_partial_runtime_chart_lock_fails_before_packaging_or_registry_access(self):
        output = self.root / "packages"
        self.env["OPENCLAW_IMAGE_DIGEST"] = "sha256:" + "a" * 64
        result = self.run_release("package-charts", "--package-dir", str(output), success=False)
        self.assertIn("must be supplied together", result.stderr)
        self.assertFalse(output.exists())
        self.assertEqual(self.commands(), [])

    def test_locked_runtime_chart_rejects_forced_publication_before_registry_access(self):
        self.env.update(OPENCLAW_IMAGE_DIGEST="sha256:" + "a" * 64,
                        HERMES_IMAGE_DIGEST="sha256:" + "b" * 64)
        result = self.run_release("publish-charts", "--force", success=False)
        self.assertIn("new immutable registry prefix", result.stderr)
        self.assertEqual(self.commands(), [])

    def test_existing_broker_or_vscode_or_chart_stops_before_any_build(self):
        for artifact in ("cetirun-agent-broker", "cetirun-vscode", "helm/cetirun-agent-broker"):
            with self.subTest(artifact=artifact):
                self.env["ARTIFACT_EXISTS"] = self.image(artifact)
                self.run_release("publish", success=False)
                self.assertFalse(any(command[:2] == ["buildx", "build"] for command in self.commands()))

    def test_registry_access_error_is_not_treated_as_an_absent_tag(self):
        for error in ("unauthorized: authentication required", "credential helper not found"):
            with self.subTest(error=error):
                self.env["ARTIFACT_REGISTRY_ERROR"] = error
                result = self.run_release("publish-images", success=False)
                self.assertIn("Cannot determine", result.stderr)
                self.assertFalse(any(command[:2] == ["buildx", "build"] for command in self.commands()))

    def test_publish_checks_each_image_and_failure_stops_the_sequence(self):
        self.env["ARTIFACT_FAIL_BUILD"] = self.image("cetirun-agent-broker")
        self.run_release("publish-images", success=False)
        commands = self.commands()
        builds = [command for command in commands if command[:2] == ["buildx", "build"]]
        self.assertIn(self.image("cetirun-agent-broker"), builds[-1])
        self.assertFalse(any(self.image("cetirun-vscode") in command for command in builds))
        for command in builds:
            self.assertIn("--push", command)
            self.assertNotIn("--load", command)

    def test_successful_publish_verifies_all_fourteen_images(self):
        self.run_release("publish-images")
        commands = self.commands()
        builds = [command for command in commands if command[:2] == ["buildx", "build"]]
        self.assertEqual(len(builds), 14)
        last_build = max(index for index, command in enumerate(commands) if command[:2] == ["buildx", "build"])
        self.assertEqual(commands[last_build + 1], ["buildx", "imagetools", "inspect", self.image("cetirun-pi-agent")])
        for name in IMAGES:
            inspect = ["buildx", "imagetools", "inspect", self.image(name)]
            self.assertEqual(commands.count(inspect), 2, "expected one absence check and one post-push inspection")

    @unittest.skipUnless(shutil.which("helm"), "requires existing Helm")
    def test_all_eight_charts_package_with_real_local_helm(self):
        output = self.root / "packages"
        self.run_release("package-charts", "--package-dir", str(output))
        self.assertEqual({path.name for path in output.glob("*.tgz")}, {f"{name}-{self.env['VERSION']}.tgz" for name in CHARTS})
        self.assertEqual(self.commands(), [])

    @unittest.skipUnless(shutil.which("helm"), "requires existing Helm")
    def test_unified_chart_package_locks_a_private_runtime_chart_copy(self):
        openclaw = "sha256:" + "a" * 64
        hermes = "sha256:" + "b" * 64
        self.env.update(OPENCLAW_IMAGE_DIGEST=openclaw, HERMES_IMAGE_DIGEST=hermes)
        output = self.root / "locked-packages"
        self.run_release("package-charts", "--package-dir", str(output))
        with tarfile.open(output / f"cetirun-{self.env['VERSION']}.tgz") as archive:
            values = archive.extractfile("cetirun/values.yaml").read().decode()
            openclaw_profile = archive.extractfile("cetirun/agents/openclaw/profile.yaml").read().decode()
            hermes_profile = archive.extractfile("cetirun/agents/hermes/profile.yaml").read().decode()
        self.assertIn(f"digest: {openclaw}", values)
        self.assertIn(f"digest: {hermes}", values)
        self.assertIn(f"- {self.env['REGISTRY']}/openclaw-gateway@{openclaw}", openclaw_profile)
        self.assertIn(f"- {self.env['REGISTRY']}/hermes-gateway@{hermes}", hermes_profile)
        self.assertEqual(self.commands(), [])


if __name__ == "__main__":
    unittest.main()
