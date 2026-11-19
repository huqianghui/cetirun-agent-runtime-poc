# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import hashlib
import importlib.util
import io
import json
import os
import re
import shlex
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "app" / "cetirun-chart"
OPENAT2_PROBE = CHART / "agents" / "openclaw" / "check-openat2.py"


def version_tuple(value):
    return tuple(int(part) for part in value.split("."))


class ImageDependencyTests(unittest.TestCase):
    def openat2_probe(self):
        spec = importlib.util.spec_from_file_location("check_openat2", OPENAT2_PROBE)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def _hermes_source_command(self):
        content = (CHART / "agents/hermes/Dockerfile").read_text()
        stage = content.split("FROM ${BASE_IMAGE} AS hermes-source\n", 1)[1].split("\nFROM ", 1)[0]
        return stage.split("\nRUN ", 1)[1].strip().replace("\\\n", "")

    def test_hermes_source_download_allows_slow_transfers_and_paces_retries(self):
        arguments = shlex.split(self._hermes_source_command().split("&&", 1)[0])
        self.assertEqual(arguments[0], "curl")
        for flag, expected in {
            "--connect-timeout": "15",
            "--max-time": "1800",
            "--retry": "3",
            "--retry-delay": "15",
            "--retry-max-time": "1800",
        }.items():
            with self.subTest(flag=flag):
                self.assertIn(flag, arguments)
                self.assertEqual(arguments[arguments.index(flag) + 1], expected)
        self.assertIn("--retry-all-errors", arguments)

    def test_hermes_source_download_preserves_integrity_gates(self):
        for failure in ("none", "http", "checksum"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                archive = root / "source.tar.gz"
                project = b'[project]\nversion = "0.0.0"\n'
                with tarfile.open(archive, "w:gz") as stream:
                    entry = tarfile.TarInfo("hermes-source/pyproject.toml")
                    entry.size = len(project)
                    stream.addfile(entry, io.BytesIO(project))
                binaries = root / "bin"
                binaries.mkdir()
                curl = binaries / "curl"
                curl.write_text(
                    "#!/bin/sh\n"
                    'if [ "$DOWNLOAD_RESULT" != "0" ]; then exit "$DOWNLOAD_RESULT"; fi\n'
                    'while [ "$#" -gt 0 ]; do\n'
                    '  if [ "$1" = "-o" ]; then cp "$SOURCE_ARCHIVE" "$2"; exit $?; fi\n'
                    "  shift\n"
                    "done\n"
                    "exit 1\n"
                )
                curl.chmod(0o755)
                checkout = root / "checkout"
                checkout.mkdir()
                download = root / "download.tar.gz"
                command = self._hermes_source_command().replace(
                    "/tmp/hermes-agent.tar.gz", str(download)
                ).replace("/usr/local/lib/hermes-agent", str(checkout))
                result = subprocess.run(
                    ["sh", "-c", command], cwd=checkout, text=True, capture_output=True, timeout=10,
                    env={
                        **os.environ,
                        "PATH": f"{binaries}{os.pathsep}{os.environ['PATH']}",
                        "SOURCE_ARCHIVE": str(archive),
                        "DOWNLOAD_RESULT": "22" if failure == "http" else "0",
                        "HERMES_GIT_SHA": "b200f5bc60e31588ba2cb60e288029c4a54ba5fa",
                        "HERMES_SOURCE_SHA256": "0" * 64 if failure == "checksum" else hashlib.sha256(archive.read_bytes()).hexdigest(),
                    },
                )
                if failure == "none":
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual((checkout / "pyproject.toml").read_bytes(), project)
                    self.assertFalse(download.exists())
                else:
                    self.assertNotEqual(result.returncode, 0)
                    if failure == "http":
                        self.assertEqual(result.returncode, 22)
                    self.assertFalse((checkout / "pyproject.toml").exists())

    def _docker_build_arguments(self, **overrides):
        with tempfile.TemporaryDirectory() as temp_dir:
            temp = Path(temp_dir)
            log_path = temp / "docker-arguments.log"
            docker = temp / "docker"
            docker.write_text(
                "#!/usr/bin/env sh\n"
                'printf "%s\\n" "$*" >> "$CETIRUN_DOCKER_ARGUMENTS"\n'
                'if [ "$1" = "buildx" ] && [ "$2" = "inspect" ]; then exit 1; fi\n'
            )
            docker.chmod(0o755)
            environment = os.environ.copy()
            environment.update(
                {
                    "BUILD_BASE": "false",
                    "BUILD_DESKTOP": "false",
                    "GIT_SHA": "test-sha",
                    "PUSH": "false",
                    "VERSION": "test-version",
                    "CETIRUN_DOCKER_ARGUMENTS": str(log_path),
                    "PATH": f"{temp}{os.pathsep}{environment['PATH']}",
                }
            )
            environment.update(overrides)
            result = subprocess.run(
                [ROOT / "scripts" / "cetirun-docker-build.sh"],
                capture_output=True,
                text=True,
                check=False,
                env=environment,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            return log_path.read_text()

    def test_base_image_provides_common_initializer_tools(self):
        content = (CHART / "Dockerfile.base").read_text()
        common = (CHART / "configs/init-home-common.sh").read_text()
        required = re.search(r"for tool in ([^;]+); do", common)
        checked = re.search(r"RUN for tool in ([^;]+); do", content)
        self.assertIsNotNone(required)
        self.assertIsNotNone(checked)
        self.assertEqual(set(checked[1].split()), {"sh", *required[1].split()})
        self.assertRegex(content, r"(?m)^  diffutils \\\s*$")
        self.assertIn('command -v "$tool" >/dev/null 2>&1 || exit 1;', content)

    def test_fixed_image_inputs(self):
        minimums = {
            "Dockerfile.base": {
                "CLAUDE_CODE_VERSION": "2.1.163",
                "OPENCODE_VERSION": "1.18.33",
                "GH_VERSION": "2.95.0",
                "KUBECTL_VERSION": "1.36.4",
                "TAILSCALE_VERSION": "1.100.0",
                "MSGPACK_VERSION": "1.2.1",
                "SETUPTOOLS_VERSION": "80.10.2",
            },
            "agents/openclaw/Dockerfile": {
                "OPENCLAW_VERSION": "2026.9.6",
                "CLAWHUB_VERSION": "0.23.3",
                "CLAWHUB_UNDICI_VERSION": "7.29.1",
            },
            "agents/hermes/Dockerfile": {
                "HERMES_VERSION": "0.22.7",
            },
        }
        for filename, arguments in minimums.items():
            content = (CHART / filename).read_text()
            for name, minimum in arguments.items():
                with self.subTest(filename=filename, argument=name):
                    match = re.search(rf"^ARG {name}=([\d.]+)$", content, re.MULTILINE)
                    self.assertIsNotNone(match)
                    self.assertGreaterEqual(
                        version_tuple(match[1]), version_tuple(minimum)
                    )

    def test_ai_gateway_oauthlib_security_floor(self):
        requirements = (
            ROOT / "app/cetirun-ai-gateway/requirements.txt"
        ).read_text()
        versions = re.findall(r"^oauthlib==([\d.]+)$", requirements, re.MULTILINE)
        self.assertEqual(len(versions), 1)
        self.assertGreaterEqual(version_tuple(versions[0]), (4, 0, 0))

    def test_openclaw_config_uses_current_schema_contracts(self):
        template = (CHART / "agents/openclaw/config/native.tpl").read_text()
        entrypoint = (
            CHART / "agents/openclaw/bootstrap/entrypoint.sh"
        ).read_text()
        values = (CHART / "values.yaml").read_text()
        schema = json.loads((CHART / "values.schema.json").read_text())

        self.assertIn('set $defaultsCurrent "modelPolicy"', template)
        self.assertIn('set $pluginEntries "github"', template)
        self.assertIn("rememberAcrossConversations: false", values)
        self.assertIn('trustedProxies:\n        - "127.0.0.6"', values)
        self.assertIn(
            'tailscale serve --bg "http://127.0.0.1:${TAILSCALE_SERVE_PORT}"',
            entrypoint,
        )
        self.assertNotIn('tailscale serve --bg "http://localhost:', entrypoint)
        self.assertNotIn(
            "memorySearch", schema["properties"]["openclaw"]["properties"]
        )

        retired_fields = (
            "reserveTokensFloor",
            "maxConcurrentSessions",
            "coalesceIdleMs",
            "maxChunkChars",
            "ttlMinutes",
            "remoteCdpTimeoutMs",
            "remoteCdpHandshakeTimeoutMs",
            "watchDebounceMs",
        )
        for field in retired_fields:
            with self.subTest(field=field):
                self.assertNotIn(field, template)
                self.assertNotIn(field, values)

    def test_openclaw_builder_checks_openat2_before_plugin_downloads(self):
        dockerfile = (CHART / "agents/openclaw/Dockerfile").read_text()
        preflight = OPENAT2_PROBE
        self.assertTrue(preflight.is_file())
        self.assertIn("COPY --chown=vibe:vibe agents/openclaw/check-openat2.py", dockerfile)
        self.assertLess(
            dockerfile.index("python3 /tmp/check-openat2.py"),
            dockerfile.index("--prefix /tmp/openclaw-plugin-prefetch"),
        )
        source = preflight.read_text()
        self.assertIn("libc.syscall", source)
        self.assertIn("errno.ENOSYS", source)
        self.assertIn("Linux Docker Buildx builder", source)
        self.assertIn("remote builders that block this syscall are unsupported", source)

    def test_openat2_probe_closes_the_successful_descriptor(self):
        probe = self.openat2_probe()
        libc = Mock()
        libc.syscall.return_value = 41
        with patch.object(probe.ctypes, "CDLL", return_value=libc) as load_libc, \
             patch.object(probe.os, "close") as close:
            probe.main()
        load_libc.assert_called_once_with(None, use_errno=True)
        self.assertEqual(libc.syscall.call_args.args[0].value, probe.SYS_OPENAT2)
        close.assert_called_once_with(41)

    def test_openat2_probe_reports_enosys(self):
        probe = self.openat2_probe()
        libc = Mock()
        libc.syscall.return_value = -1
        with patch.object(probe.ctypes, "CDLL", return_value=libc), \
             patch.object(probe.ctypes, "get_errno", return_value=probe.errno.ENOSYS), \
             self.assertRaisesRegex(SystemExit, r"openat2.*not implemented"):
            probe.main()

    def test_openat2_probe_rejects_other_syscall_failures(self):
        probe = self.openat2_probe()
        libc = Mock()
        libc.syscall.return_value = -1
        with patch.object(probe.ctypes, "CDLL", return_value=libc), \
             patch.object(probe.ctypes, "get_errno", return_value=probe.errno.EPERM), \
             self.assertRaisesRegex(SystemExit, r"rejected it.*Operation not permitted"):
            probe.main()

    def test_hermes_uses_official_locked_source_install(self):
        content = (CHART / "agents/hermes/Dockerfile").read_text()
        builder = (CHART / "agents/hermes/pm/build.py").read_text()
        expected = {
            "HERMES_RELEASE": "0.22.7",
            "HERMES_GIT_SHA": "b200f5bc60e31588ba2cb60e288029c4a54ba5fa",
            "HERMES_SOURCE_SHA256": "1f7d43fa1a515283929a739c42e10bf8d48910e523eccec74b2a2bece858d2b5",
            "HERMES_CPYTHON_SHA256": "e02d9ac21cebda6e46435369744a92c65cc7cd8669697694122cf97ffacd1e53",
            "HERMES_SQLITE_VERSION": "3530400",
            "HERMES_SQLITE_SHA256": "0e9483900e92cd5de8fd48d16bf9200145a61f7fd5be542a5ac81d8a9516eb9c",
        }
        for argument, value in expected.items():
            with self.subTest(argument=argument):
                self.assertIn(f"ARG {argument}={value}\n", content)
        self.assertIn("codeload.github.com/NousResearch/hermes-agent/tar.gz/${HERMES_GIT_SHA}", content)
        self.assertIn("sha256sum --check", content)
        self.assertIn('pm.ensure(name, explicit=True)', builder)
        self.assertIn('pm.stage_manager_runtime(', builder)
        self.assertIn('"--sealed", "--record-selection"', builder)
        self.assertIn('("all", "messaging", "anthropic", "azure-identity")', builder)
        self.assertIn('"--distribution", "docker", "--source", "docker", "--update-mechanism", "external"', builder)
        self.assertIn('node apps/desktop/scripts/build-webapp.mjs --deps-only', content)
        self.assertIn('node apps/desktop/scripts/build-webapp.mjs --no-install', content)
        self.assertIn('node scripts/build/tui.mjs', content)
        self.assertIn('ENV HERMES_PYTHON=/usr/local/bin/hermes-python', content)
        self.assertIn('ENV HERMES_DISABLE_LAZY_INSTALLS=1', content)
        self.assertIn('/usr/local/bin/hermes-python /usr/local/lib/hermes-agent/cetirun_pm/verify.py', content)
        for retired in ('HERMES_UV_IMAGE', 'HERMES_NODE_IMAGE', 'HERMES_WEB_DIST=', 'npm --prefix web run build', 'venv/bin/python -c', 'compat/apply.py', 'EXPOSE 8642 7681 9119'):
            with self.subTest(retired=retired):
                self.assertNotIn(retired, content)

    def test_hermes_sqlite_build_has_kernel_headers(self):
        content = (CHART / "agents/hermes/Dockerfile").read_text()
        sqlite_build = content.split("AS hermes-sqlite-build", 1)[1].split(
            "FROM ${BASE_IMAGE} AS hermes-toolchain", 1
        )[0]
        self.assertRegex(sqlite_build, r"tdnf install -y [^\n]*\bkernel-headers\b")
        self.assertIn("-DSQLITE_ENABLE_FTS5", sqlite_build)
        self.assertIn("-DSQLITE_THREADSAFE=1", sqlite_build)
        self.assertNotIn("cc-test", sqlite_build)

    def test_node_release_and_architecture_checksums(self):
        content = (CHART / "Dockerfile.base").read_text()
        expected = {
            "NODE_VERSION": "24.21.0",
            "NODE_SHA256_X64": "fd8e59d5a511510f6a298afb548f18c7d2b1be404d8b4a27d94fbe49f56cb2d6",
            "NODE_SHA256_ARM64": "6ad1325edbdb5649c379b75a237147a666c95d4f9ae8d340fef2d1575d289ad2",
        }
        for argument, value in expected.items():
            with self.subTest(argument=argument):
                self.assertIn(f"ARG {argument}={value}\n", content)
        self.assertIn('NODE_ARCH="x64"; NODE_SHA256="${NODE_SHA256_X64}"', content)
        self.assertIn('NODE_ARCH="arm64"; NODE_SHA256="${NODE_SHA256_ARM64}"', content)
        self.assertIn('echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum --check', content)

    def test_npm_overrides_are_empty_after_dependency_upgrades(self):
        overrides = json.loads((CHART / "configs/cve-overrides.json").read_text())[
            "overrides"
        ]
        self.assertEqual(overrides, {})

        base = (CHART / "Dockerfile.base").read_text()
        self.assertNotIn("tinyglobby/node_modules/picomatch", base)
        self.assertNotIn("UNDICI_DIR=", base)
        self.assertIn(
            'npm install --global --prefix /usr/local --ignore-scripts', base
        )
        self.assertIn('"npm@${NPM_VERSION}"', base)
        self.assertIn("npm install-scripts approve opencode-ai", base)
        self.assertIn("npm rebuild opencode-ai", base)
        self.assertIn('test "$(opencode --version)" = "${OPENCODE_VERSION}"', base)
        self.assertIn(
            "npm install-scripts approve @anthropic-ai/claude-code",
            base,
        )
        self.assertIn("npm rebuild @anthropic-ai/claude-code", base)
        self.assertIn('claude --version | grep -F "${CLAUDE_CODE_VERSION}"', base)

        openclaw = (CHART / "agents/openclaw/Dockerfile").read_text()
        self.assertIn('npm pkg set "overrides[undici@7]=${CLAWHUB_UNDICI_VERSION}"', openclaw)
        self.assertIn('npm ls "undici@${CLAWHUB_UNDICI_VERSION}" --all', openclaw)
        self.assertIn("! npm ls undici@7.29.0 --all", openclaw)

    def test_pi_example_preserves_fixed_base_dependencies(self):
        content = (ROOT / "examples/pi-agent/Dockerfile").read_text()
        anchor = "RUN inherited_undici="
        self.assertIn(anchor, content)
        install = "inherited_undici=" + content.split(anchor, 1)[1].split("\n\nCOPY ", 1)[0].replace("\\\n", "")
        for version in ("6.26.0", "6.27.0", "6.28.0", "6.25.0", "7.29.0"):
            with self.subTest(version=version):
                mocks = (
                    f"node() {{ printf '%s\\n' '{version}'; }}\n"
                    "npm() { return 0; }\n"
                    "mktemp() { printf '%s\\n' /unused-fixture; }\n"
                    "curl() { echo patch-download; return 99; }\n"
                )
                result = subprocess.run(
                    ["sh", "-c", mocks + install],
                    text=True, capture_output=True, check=False,
                )
                self.assertEqual(result.returncode == 0, version in ("6.27.0", "6.28.0"))
                self.assertEqual("patch-download" in result.stdout, version == "6.26.0")

    def test_azure_cli_top_level_packages_are_not_replaced(self):
        content = (CHART / "Dockerfile.base").read_text()
        install = content.split("# Install Azure CLI", 1)[1].split(
            "# Install ripgrep", 1
        )[0]
        self.assertNotIn('rm -rf "${AZ_SITE}/urllib3"', install)
        self.assertNotIn('rm -rf "${AZ_SITE}/msgpack"', install)
        self.assertNotIn('rm -rf "${AZ_SITE}/setuptools"', install)
        self.assertNotIn('--target "${AZ_SITE}"', install)

    def test_pip_vendor_payloads_and_manifest_are_updated_together(self):
        content = (CHART / "Dockerfile.base").read_text()
        install = content.split("# Install Azure CLI", 1)[1].split(
            "# Install ripgrep", 1
        )[0]
        self.assertIn("import pip._vendor", install)
        self.assertIn(
            'for PIP_VENDOR in "${SYSTEM_PIP_VENDOR}" "${AZ_SITE}/pip/_vendor"',
            install,
        )
        self.assertIn(
            '--upgrade --no-deps --only-binary=:all: --target "${PIP_VENDOR}"',
            install,
        )
        for package in ("msgpack", "setuptools"):
            with self.subTest(package=package):
                self.assertIn(f'"{package}==${{{package.upper()}_VERSION}}"', install)
        self.assertLess(
            install.index("tdnf install -y azure-cli"), install.index("for PIP_VENDOR")
        )
        self.assertLess(
            install.index('--target "${PIP_VENDOR}"'),
            install.index("python3 /usr/local/share/cetirun/sync-pip-vendor.py"),
        )
        self.assertIn('test -f "${PIP_VENDOR}/vendor.txt"', install)
        self.assertIn('PYTHONPATH="${PIP_VENDOR%/pip/_vendor}"', install)
        self.assertIn("--check", install)
        self.assertIn("|| exit 1;", install)

    def test_plugin_install_and_identity_cleanup_failure_handling(self):
        content = (CHART / "agents/openclaw/Dockerfile").read_text()
        self.assertNotIn("OPENCLAW_PLUGIN_STAGE_DIR", content)
        self.assertIn("npm pkg set 'allowScripts.openclaw=true' --json", content)
        self.assertIn("npm pkg delete allowScripts.openclaw", content)
        self.assertIn("npm install-scripts approve openclaw", content)
        self.assertIn("npm rebuild openclaw", content)
        self.assertIn(
            "test ! -e node_modules/openclaw/.openclaw-lifecycle-pending", content
        )
        self.assertIn("ARG OPENCLAW_TARBALL_INTEGRITY=sha512-", content)
        self.assertIn('view "openclaw@${OPENCLAW_VERSION}" dist.tarball dist.integrity', content)
        self.assertIn('crypto.createHash("sha512")', content)
        self.assertNotIn("npm --min-release-age=0 pack", content)
        self.assertIn(
            "source=/home/vibe/openclaw-pack/openclaw.tgz,target=/tmp/openclaw.tgz,ro",
            content,
        )
        self.assertNotIn("openclaw-2026.9.6.tgz", content)
        self.assertIn("--prefix /tmp/openclaw-plugin-prefetch", content)
        expected_plugins = (
            "acpx",
            "brave-plugin",
            "discord",
            "feishu",
            "perplexity-plugin",
            "slack",
        )
        for plugin in expected_plugins:
            with self.subTest(plugin=plugin):
                package = f"@openclaw/{plugin}@${{OPENCLAW_VERSION}}"
                self.assertIn(f'"{package}"', content)
                self.assertIn(f'openclaw plugins install "npm:{package}"', content)
        self.assertLess(
            content.index("--prefix /tmp/openclaw-plugin-prefetch"),
            content.index('openclaw plugins install "npm:@openclaw/acpx'),
        )
        self.assertNotIn("openclaw doctor --fix", content)
        match = re.search(
            r"^RUN (.*openclaw plugins install.*)$",
            content.replace("\\\n", ""),
            re.MULTILINE,
        )
        self.assertIsNotNone(match)
        install = match[1]
        self.assertIn("rm -rf /home/vibe/.openclaw/identity", install)
        self.assertIn("rm -f /home/vibe/.openclaw/openclaw.json", install)
        self.assertNotIn("\nRUN openclaw doctor", content)
        plugin_count = len(expected_plugins)
        cases = [(fail_at, 0) for fail_at in range(plugin_count + 1)] + [(0, 1), (plugin_count, 1)]
        for fail_at, cleanup_exit in cases:
            with self.subTest(fail_at=fail_at, cleanup_exit=cleanup_exit):
                mocks = (
                    "calls=0\n"
                    "openclaw() { calls=$((calls + 1)); "
                    f'[ "$calls" -ne {fail_at} ]; }}\n'
                    "npm() { return 0; }\n"
                    "find() { return 0; }\n"
                    f'rm() {{ printf "%s\\n" "$*"; return {cleanup_exit}; }}\n'
                )
                result = subprocess.run(
                    ["sh", "-c", mocks + install],
                    capture_output=True,
                    text=True,
                    check=False,
                )
                if fail_at or cleanup_exit:
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("-rf /home/vibe/.openclaw/identity", result.stdout)
                    self.assertIn("-f /home/vibe/.openclaw/openclaw.json", result.stdout)

    def test_runtime_build_receives_the_configured_python_index(self):
        index = "https://packages.example.invalid/simple/"
        arguments = self._docker_build_arguments(
            BUILD_IMAGE="true",
            BUILD_HERMES="false",
            PYPI_INDEX_URL=index,
        )
        self.assertIn(f"--build-arg PYPI_INDEX_URL={index}", arguments)

if __name__ == "__main__":
    unittest.main()
