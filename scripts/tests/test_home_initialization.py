# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
COMMON = ROOT / "app/cetirun-chart/configs/init-home-common.sh"
IMAGE = "registry.example.com/agent@sha256:" + "a" * 64
STAGES = """
agent_check_requirements() { printf 'check\\n' >> "$TRACE"; }
agent_prepare_home() { printf 'prepare\\n' >> "$TRACE"; }
agent_apply_config() { printf 'apply\\n' >> "$TRACE"; }
agent_prepare_integrations() { printf 'integrations\\n' >> "$TRACE"; }
agent_verify() { printf 'verify\\n' >> "$TRACE"; }
"""


class HomeInitializationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "image-home"
        self.target = self.root / "pvc"
        self.source.mkdir()
        self.target.mkdir()
        self.trace = self.root / "trace"
        self.env = dict(
            os.environ,
            SOURCE_HOME=str(self.source),
            TARGET_HOME=str(self.target),
            CETIRUN_INIT_IMAGE=IMAGE,
            LEGACY_HOME_MARKER="",
            TRACE=str(self.trace),
            COMMON=str(COMMON),
        )

    def run_init(self, extra="", *, env=None):
        return subprocess.run(
            ["sh", "-c", 'set -e\n. "$COMMON"\n' + STAGES + extra + "\nrun_agent_init\n"],
            env=env or self.env, text=True, capture_output=True, timeout=10,
        )

    def seeded(self):
        return (self.target / ".cetirun-home-seeded").is_file()

    def context_fixture(self, shared=True):
        assets, view = self.root / "memory-assets", self.root / "context-view"
        (assets / "skills/cetirun-memory/references").mkdir(parents=True)
        view.mkdir()
        (assets / "manifest.json").write_text("{}")
        (assets / "AGENTS.md").write_text("Synthetic common Memory instructions.\n")
        (assets / "skills/cetirun-memory/SKILL.md").write_text("Synthetic Memory skill.\n")
        (assets / "skills/cetirun-memory/references/requests.md").write_text("Synthetic reference.\n")
        common = self.root / "common.sh"
        common.write_text(COMMON.read_text().replace("/opt/cetirun-memory", str(assets)))
        self.env.update(COMMON=str(common), CETIRUN_MEMORY_ASSETS=str(assets), CETIRUN_CONTEXT_VIEW=str(view))
        if shared:
            source = self.root / "shared"
            (source / "team-skill").mkdir(parents=True)
            (source / "team-skill/SKILL.md").write_text("Synthetic shared skill.\n")
            self.env["CETIRUN_SHARED_SKILLS_DIR"] = str(source)
        return assets, view

    def test_native_common_init_composes_memory_and_shared_skills(self):
        assets, view = self.context_fixture()
        (self.source / ".agents/skills/cetirun-memory").mkdir(parents=True)
        (self.source / ".agents/skills/cetirun-memory/SKILL.md").write_text("Preserve image content")
        result = self.run_init()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((view / "current/AGENTS.md").read_bytes(), (assets / "AGENTS.md").read_bytes())
        self.assertEqual((view / "current/skills/cetirun-memory/references/requests.md").read_text(), "Synthetic reference.\n")
        self.assertEqual((view / "current/skills/team-skill/SKILL.md").read_text(), "Synthetic shared skill.\n")
        self.assertEqual((self.target / ".agents/skills/cetirun-memory/SKILL.md").read_text(), "Preserve image content")
        self.assertFalse((self.target / ".cetirun/memory").exists())

    def test_native_memory_initializes_without_shared_storage(self):
        _, view = self.context_fixture(shared=False)
        result = self.run_init()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([p.name for p in (view / "current/skills").iterdir()], ["cetirun-memory"])

    def test_native_shared_collision_does_not_publish_partial_context(self):
        assets, view = self.context_fixture()
        source = Path(self.env["CETIRUN_SHARED_SKILLS_DIR"])
        (source / "cetirun-memory").mkdir()
        (source / "cetirun-memory/SKILL.md").write_text("Conflicting shared copy")
        result = self.run_init()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SharedSkillConflictsWithCanonicalContext", result.stderr)
        self.assertEqual(list(view.iterdir()), [])
        self.assertEqual((assets / "skills/cetirun-memory/SKILL.md").read_text(), "Synthetic Memory skill.\n")

    def test_lost_found_shape(self):
        for kind in ("directory", "file", "symlink"):
            with self.subTest(kind=kind):
                target = self.root / ("lost-" + kind)
                target.mkdir()
                entry = target / "lost+found"
                if kind == "directory":
                    entry.mkdir()
                    (entry / "orphan").write_text("retained")
                elif kind == "file":
                    entry.write_text("retained")
                else:
                    entry.symlink_to(self.source)
                result = self.run_init(env={**self.env, "TARGET_HOME": str(target)})
                self.assertEqual(result.returncode == 0, kind == "directory", result.stderr)
                self.assertEqual((target / ".cetirun-home-seeded").exists(), kind == "directory")

    def test_first_copy_preserves_hidden_files_executable_and_links(self):
        (self.source / ".config").mkdir()
        (self.source / ".config/settings").write_text("image")
        exe = self.source / "tool"
        exe.write_text("example")
        exe.chmod(0o755)
        (self.source / "link").symlink_to("/outside/target")
        result = self.run_init()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.seeded())
        self.assertEqual((self.target / ".config/settings").read_text(), "image")
        self.assertEqual((self.target / "tool").stat().st_mode & 0o777, 0o755)
        self.assertEqual(os.readlink(self.target / "link"), "/outside/target")
        self.assertEqual(self.trace.read_text().splitlines(), ["check", "prepare", "apply", "integrations", "verify"])

    def test_restart_and_image_upgrade_do_not_reseed_but_run_stages(self):
        (self.source / ".zshrc").write_text("image")
        self.assertEqual(self.run_init().returncode, 0)
        (self.target / ".zshrc").unlink()
        (self.source / "new-dependency").write_text("new image")
        self.env["CETIRUN_INIT_IMAGE"] = "registry.example.com/agent:new"
        result = self.run_init()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.target / ".zshrc").exists())
        self.assertFalse((self.target / "new-dependency").exists())
        self.assertEqual(self.trace.read_text().splitlines().count("apply"), 2)

    def test_native_failure_does_not_repeat_home_copy(self):
        (self.source / "dependency").write_text("v1")
        result = self.run_init("agent_apply_config() { return 23; }")
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.seeded())
        self.assertNotIn("verify", self.trace.read_text())
        (self.source / "dependency").write_text("v2")
        result = self.run_init()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.target / "dependency").read_text(), "v1")

    def test_copy_failure_has_no_completion_flag_and_same_digest_can_retry(self):
        (self.source / "dependency").write_text("content")
        result = self.run_init("cp() { return 27; }")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.seeded())
        self.assertTrue((self.target / ".cetirun-home-seeding").is_file())
        result = self.run_init()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.target / "dependency").read_text(), "content")

    def test_partial_publish_retry_preserves_completed_entries(self):
        (self.source / "one").write_text("one")
        (self.source / "two").write_text("two")
        result = self.run_init("""
mv() {
  case "$1" in */.cetirun-home-stage.*/two) return 28;; esac
  command mv "$@"
}
""")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.seeded())
        self.assertEqual((self.target / "one").read_text(), "one")
        result = self.run_init()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.target / "two").read_text(), "two")

    def test_changed_or_mutable_image_cannot_resume_failed_seed(self):
        (self.source / "dependency").write_text("image")
        self.assertNotEqual(self.run_init("cp() { return 1; }").returncode, 0)
        for image in ["registry.example.com/agent:1", "registry.example.com/agent@sha256:" + "b" * 64]:
            result = self.run_init(env=dict(self.env, CETIRUN_INIT_IMAGE=image))
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("HomeSeedImageUnconfirmed", result.stderr)
            self.assertFalse(self.seeded())

    def test_mutable_image_can_finish_first_seed(self):
        result = self.run_init(env=dict(self.env, CETIRUN_INIT_IMAGE="registry.example.com/agent:1"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.seeded())

    def test_unknown_nonempty_home_is_not_overwritten(self):
        (self.target / "user-data").write_text("keep")
        result = self.run_init()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("HomeSeedStateUnknown", result.stderr)
        self.assertEqual((self.target / "user-data").read_text(), "keep")

    def test_existing_native_marker_skips_copy(self):
        (self.target / ".native").mkdir()
        (self.target / ".native/.initialized").touch()
        (self.target / "user-data").write_text("keep")
        (self.source / "dependency").write_text("must not copy")
        result = self.run_init(env=dict(self.env, LEGACY_HOME_MARKER=".native/.initialized"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.seeded())
        self.assertFalse((self.target / "dependency").exists())

    def test_image_cannot_seed_platform_markers(self):
        (self.source / ".cetirun-home-seeded").write_text("invalid")
        (self.source / ".cetirun-home-seeding").write_text("invalid")
        result = self.run_init()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.target / ".cetirun-home-seeded").read_text().startswith("seeded-v1\n"))
        self.assertFalse((self.target / ".cetirun-home-seeding").exists())

    def test_marker_link_and_special_file_fail_without_following(self):
        outside = self.root / "outside"
        outside.write_text("keep")
        (self.target / ".cetirun-home-seeded").symlink_to(outside)
        result = self.run_init()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(outside.read_text(), "keep")
        (self.target / ".cetirun-home-seeded").unlink()
        os.mkfifo(self.source / "pipe")
        result = self.run_init()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("UnsupportedHomeFile", result.stderr)

    def test_staging_record_cannot_delete_another_directory(self):
        outside = self.root / "outside"
        outside.mkdir()
        (outside / "keep").write_text("keep")
        (self.target / ".cetirun-home-seeding").write_text(f"seeding-v1\n{IMAGE}\n../outside\n")
        result = self.run_init()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((outside / "keep").read_text(), "keep")

    def test_overlap_and_missing_stage_are_rejected(self):
        result = self.run_init(env=dict(self.env, SOURCE_HOME=str(self.target)))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("OverlappingHomePaths", result.stderr)
        result = self.run_init("unset -f agent_verify")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("InitStageMissing", result.stderr)
        self.assertFalse(self.seeded())

    def test_write_helper_does_not_follow_destination_link(self):
        outside = self.root / "outside"
        outside.write_text("keep")
        (self.source / "input").write_text("new")
        self.assertEqual(self.run_init().returncode, 0)
        (self.target / "config").symlink_to(outside)
        result = self.run_init('agent_apply_config() { home_atomic_copy "$SOURCE_HOME/input" config; }')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(outside.read_text(), "keep")

    def test_missing_tool_stops_before_seed(self):
        result = self.run_init("""
check_common_tools() {
  command -v cetirun-deliberately-missing >/dev/null 2>&1 || {
    init_error "InitToolsMissing"
    return 1
  }
}
""")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.seeded())
        self.assertFalse(self.trace.exists())


if __name__ == "__main__":
    unittest.main()
