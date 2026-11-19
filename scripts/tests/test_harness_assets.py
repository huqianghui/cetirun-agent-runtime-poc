# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "app/cetirun-chart"


class HarnessAssetsTests(unittest.TestCase):
    def test_base_cli_skill_links_follow_the_common_view(self):
        source = (CHART / "Dockerfile.base").read_text()
        for target in (".config/opencode/skills", ".codex/skills", ".aks-skills/plugins/aks-shared-skills/skills"):
            self.assertIn(f"ln -s /home/vibe/.agents/skills /home/vibe/{target}", source)
        self.assertNotIn("/home/vibe/.aks/skills", source)

    def test_image_sources_exist_in_unchanged_build_context(self):
        for kind in ("openclaw", "hermes"):
            dockerfile = CHART / f"agents/{kind}/Dockerfile"
            content = dockerfile.read_text()
            for line in content.splitlines():
                if not line.startswith("COPY ") or "--from=" in line:
                    continue
                source = [part for part in line.split()[1:] if not part.startswith("--")][0]
                self.assertTrue((CHART / source).exists(), f"{kind}: {source}")
            native_version = re.search(
                rf"^ARG {kind.upper()}_VERSION=([^\n]+)", content, re.MULTILINE
            )[1]
            profile = (CHART / f"agents/{kind}/profile.yaml").read_text()
            self.assertIn(f'nativeVersion: "{native_version}"', profile)

    def test_hermes_uses_supported_runtime_interfaces(self):
        entrypoint = (
            CHART / "agents/hermes/bootstrap/hermes-entrypoint.sh"
        ).read_text()
        runtime = (
            CHART / "templates/agents/hermes/_runtime.tpl"
        ).read_text()

        self.assertIn(
            "hermes gateway run --replace --accept-hooks --external-supervisor",
            (CHART / "agents/hermes/bootstrap/hermes-gateway.sh").read_text(),
        )
        self.assertNotIn("| tee -a", entrypoint)
        self.assertNotIn("--insecure", entrypoint)
        self.assertNotIn("[program:hermes-dashboard]", entrypoint)
        self.assertIn("/usr/local/bin/hermes-python", entrypoint)

        for name, key in (
            ("HERMES_DASHBOARD_BASIC_AUTH_PASSWORD", "API_SERVER_KEY"),
            ("HERMES_DASHBOARD_BASIC_AUTH_SECRET", "CETIRUN_AUTH_KEY"),
        ):
            self.assertRegex(
                runtime,
                rf"- name: {name}\n"
                r"  valueFrom:\n"
                r"    secretKeyRef:\n"
                r"      name: .*?\n"
                rf"      key: {key}",
            )
        self.assertIn('name: HERMES_DASHBOARD_BASIC_AUTH_USERNAME', runtime)
        self.assertIn('value: "cetirun"', runtime)

    def test_hermes_image_has_no_direct_model_auth_helpers(self):
        dockerfile = (CHART / "agents/hermes/Dockerfile").read_text()
        for helper in ("hermes-auth.sh", "hermes-copilot-auth.py", "cetirun-auth-service.py"):
            self.assertNotIn(helper, dockerfile)
        entrypoint = (CHART / "agents/hermes/bootstrap/hermes-entrypoint.sh").read_text()
        self.assertNotIn("[program:cetirun-auth]", entrypoint)
        self.assertIn("AI Gateway is required", entrypoint)

    def test_hermes_config_uses_current_memory_keys(self):
        values = (CHART / "values.yaml").read_text()
        self.assertIn("memory_char_limit: 2200", values)
        self.assertNotRegex(values, r"(?m)^\s+char_limit:")


if __name__ == "__main__":
    unittest.main()
