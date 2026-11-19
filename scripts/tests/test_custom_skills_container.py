# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Optional mount-level qualification using an explicitly selected local image."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "app/cetirun-chart"
IMAGE = os.environ.get("CETIRUN_SKILLS_TEST_IMAGE")


@unittest.skipUnless(IMAGE, "set CETIRUN_SKILLS_TEST_IMAGE to an already-cached shell image")
@unittest.skipIf(os.geteuid() == 0, "requires a non-root caller for the mount qualification")
class CustomSkillsContainerTests(unittest.TestCase):
    def test_shared_materialization_and_readonly_agent_view(self):
        self.assertIsNotNone(shutil.which("docker"))
        image_id, platform = subprocess.check_output(
            ["docker", "image", "inspect", "--format", "{{.Id}} {{.Os}}/{{.Architecture}}", IMAGE],
            text=True, timeout=10,
        ).split()
        self.assertTrue(image_id.startswith("sha256:"))
        with tempfile.TemporaryDirectory(prefix="cetirun-skills-mounts-") as directory:
            root = Path(directory)
            source, home, context, shared, view = (root / name for name in ("source", "home", "context", "shared", "view"))
            for path in (source, home, context, shared, view):
                path.mkdir()
            (source / "seed").write_text("synthetic image HOME")
            (context / "AGENTS.md").write_text("synthetic canonical")
            (context / "skills/cetirun-memory").mkdir(parents=True)
            (context / "skills/cetirun-memory/SKILL.md").write_text("synthetic memory")
            (shared / "team").mkdir()
            (shared / "team/SKILL.md").write_text("synthetic shared skill")

            def mount(path, target, readonly=False):
                self.assertNotIn(",", str(path))
                return ["--mount", f"type=bind,src={path},dst={target}" + (",readonly" if readonly else "")]

            base = [
                "docker", "run", "--rm", "--pull=never", "--platform", platform, "--network=none", "--read-only",
                "--user", f"{os.getuid()}:{os.getgid()}", "--cap-drop=ALL",
                "--security-opt=no-new-privileges", "--entrypoint=/bin/sh",
            ]
            init = base + mount(source, "/image-home", True) + mount(home, "/home-data")
            init += mount(context, "/etc/cetirun-context", True) + mount(shared, "/etc/cetirun-shared-skills", True)
            init += mount(view, "/etc/cetirun-context-view")
            init += mount(CHART / "configs/init-home-common.sh", "/etc/cetirun/init-home-common.sh", True)
            init += mount(CHART / "agents/custom/bootstrap/init-home.sh", "/etc/cetirun/custom-init-home.sh", True)
            init += [
                "-e", "SOURCE_HOME=/image-home", "-e", "TARGET_HOME=/home-data",
                "-e", f"CETIRUN_INIT_IMAGE={image_id}",
                "-e", "CETIRUN_CONTEXT_DIR=/etc/cetirun-context",
                "-e", "CETIRUN_SHARED_SKILLS_DIR=/etc/cetirun-shared-skills",
                "-e", "CETIRUN_CONTEXT_VIEW=/etc/cetirun-context-view",
                image_id, "/etc/cetirun/custom-init-home.sh",
            ]
            result = subprocess.run(init, text=True, capture_output=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            check = base + mount(home, "/home/worker") + mount(view / "current", "/home/worker/.agents", True)
            check += mount(shared, "/shared-source", True)
            check += [image_id, "-ec", """
                test "$(id -u)" -ne 0
                test "$(cat /home/worker/.agents/AGENTS.md)" = "synthetic canonical"
                test "$(cat /home/worker/.agents/skills/cetirun-memory/SKILL.md)" = "synthetic memory"
                test "$(cat /home/worker/.agents/skills/team/SKILL.md)" = "synthetic shared skill"
                if printf changed > /home/worker/.agents/write-probe; then exit 1; fi
                if printf changed > /shared-source/team/SKILL.md; then exit 1; fi
                mkdir -p /home/worker/memory
                printf kept > /home/worker/memory/private
            """]
            result = subprocess.run(check, text=True, capture_output=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            result = subprocess.run(init, text=True, capture_output=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((home / "memory/private").read_text(), "kept")
            self.assertEqual((shared / "team/SKILL.md").read_text(), "synthetic shared skill")
            self.assertFalse((view / "current/write-probe").exists())


if __name__ == "__main__":
    unittest.main()
