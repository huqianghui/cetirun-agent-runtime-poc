# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""CLI tests with synthetic HOME; native cases require the pinned real source.

Set CETIADMIN_TEST_HERMES_INSTALL_DIR to an already restored Hermes install.
Use CETIADMIN_TEST_HERMES_PYTHON for an existing Python with native dependencies.
No loader double, dependency download, model call, or user HOME is used here.
Only the archival allowlist is synthetic in the archival-mechanics tests.
"""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "assets/scripts/check_skill_sources.py"
CHECKSUMS = ROOT / "assets/scripts/legacy_skill_checksums.py"
HERMES_SHA = "b200f5bc60e31588ba2cb60e288029c4a54ba5fa"
NATIVE_INSTALL = os.environ.get("CETIADMIN_TEST_HERMES_INSTALL_DIR", "")
NATIVE_PYTHON = os.environ.get("CETIADMIN_TEST_HERMES_PYTHON", sys.executable)
PRIVATE = "sk-dummy-private-content"

# This observes the real helper/imports, without substituting native modules.
AUDIT = r'''
import json
import os
from pathlib import Path
import sys
import traceback

forbidden = set(json.loads(os.environ.get("FIXTURE_FORBIDDEN", "[]")))
def audit(event, args):
    violation = event in {"socket.connect", "socket.getaddrinfo", "subprocess.Popen", "os.system"}
    if event == "open" and isinstance(args[0], (str, bytes)):
        violation |= str(Path(os.fsdecode(args[0])).resolve()) in forbidden
    if violation:
        with open(os.environ["FIXTURE_VIOLATIONS"], "a") as out:
            out.write(event + ": " + " -> ".join(
                Path(frame.filename).name + ":" + frame.name
                for frame in traceback.extract_stack(limit=8)) + "\n")
        raise RuntimeError("sk-dummy-private-content")
sys.addaudithook(audit)
'''


def tree_bytes(directory):
    return {str(p.relative_to(directory)): p.read_bytes()
            for p in directory.rglob("*") if p.is_file()}


class SkillSourceBoundaryTests(unittest.TestCase):
    def run_cli(self, *args, env=None):
        return subprocess.run([sys.executable, "-B", str(SCRIPT), *args],
                              env={"PATH": os.defpath, **(env or {})},
                              capture_output=True, text=True, timeout=10)

    def test_help_does_not_require_runtime_or_credentials(self):
        result = self.run_cli("--help")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("HERMES_HOME", result.stdout)
        self.assertIn("HERMES_INSTALL_DIR", result.stdout)

    def test_invalid_arguments_do_not_echo_values(self):
        result = self.run_cli("--unknown", PRIVATE)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)
        self.assertEqual(json.loads(result.stdout)["code"], "invalid_arguments")
        self.assertEqual(result.stderr, "")

    def test_missing_runtime_never_falls_back_to_user_home(self):
        result = self.run_cli()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout)["code"], "invalid_runtime")
        self.assertEqual(result.stderr, "")

    def test_wrong_native_pin_fails_before_import(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "install-stamp.json").write_text("wrong-" + PRIVATE)
            result = self.run_cli(env={"HOME": directory, "HERMES_HOME": directory,
                                       "HERMES_INSTALL_DIR": directory})
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)
        self.assertEqual(json.loads(result.stdout)["code"], "native_version_mismatch")

    def test_correct_marker_with_wrong_loader_bytes_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "install-stamp.json").write_text(json.dumps({"commit": HERMES_SHA, "baseVersion": "0.22.7", "updateMechanism": "external", "distribution": "docker"}))
            (root / "agent").mkdir()
            (root / "agent/skill_utils.py").write_text("raise RuntimeError('" + PRIVATE + "')")
            result = self.run_cli(env={"HOME": directory, "HERMES_HOME": directory,
                                       "HERMES_INSTALL_DIR": directory})
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)
        self.assertEqual(json.loads(result.stdout)["code"], "native_version_mismatch")


@unittest.skipUnless(NATIVE_INSTALL, "set CETIADMIN_TEST_HERMES_INSTALL_DIR for real pinned-loader tests")
class NativeSkillSourceTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="cetiadmin-skill-test-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.home = self.root / "home/.hermes"
        self.local = self.home / "skills"
        self.local.mkdir(parents=True)
        self.assets = self.root / "exported-assets"
        self.script = self.assets / "scripts/check_skill_sources.py"
        self.script.parent.mkdir(parents=True)
        shutil.copy2(SCRIPT, self.script)
        shutil.copy2(CHECKSUMS, self.script.with_name(CHECKSUMS.name))
        self.bundle = self.assets / "skills"
        shutil.copytree(ROOT / "assets/skills", self.bundle)
        self.config = {"skills": {"external_dirs": [str(self.bundle)]}}
        self.config_path = self.home / "config.yaml"
        self.violation = self.root / "audit-violations"
        self.archive = self.home / "cetiadmin/skill-archive"
        self.sentinels = {}
        for name in (".env", "auth.json", "SOUL.md", "USER.md", "state.db", "memories/notes.md",
                     "cron/jobs.json", "cron/notepad/kept.json", "cron/output/kept.md"):
            path = self.home / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(PRIVATE + ":" + name)
            self.sentinels[path] = (path.read_bytes(), path.stat().st_mtime_ns)
        self.env = {
            "PATH": os.defpath, "HOME": str(self.home.parent), "HERMES_HOME": str(self.home),
            "HERMES_INSTALL_DIR": NATIVE_INSTALL, "PYTHONDONTWRITEBYTECODE": "1",
            "FIXTURE_FORBIDDEN": json.dumps([str(path) for path in self.sentinels]),
            "FIXTURE_VIOLATIONS": str(self.violation), "FIXTURE_BUNDLE": str(self.bundle),
        }

    def run_python(self, program, *args, env=None):
        result = subprocess.run([NATIVE_PYTHON, "-B", "-c", AUDIT + program, *args],
                                env=self.env | (env or {}), cwd=self.root,
                                capture_output=True, text=True, timeout=20)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)
        self.assertFalse(self.violation.exists(), self.violation.read_text() if self.violation.exists() else "")
        for path, original in self.sentinels.items():
            self.assertEqual((path.read_bytes(), path.stat().st_mtime_ns), original)
        return result

    def save_config(self):
        self.config_path.write_text(json.dumps(self.config))  # JSON is valid YAML.

    def check(self, code=None, *, env=None):
        self.save_config()
        before = self.config_path.read_bytes()
        result = self.run_python('''
import runpy
sys.argv = sys.argv[1:]
runpy.run_path(sys.argv[0], run_name="__main__")
''', str(self.script), env=env)
        self.assertEqual(self.config_path.read_bytes(), before)
        self.assertEqual(result.stderr, "", result.stderr)
        self.assertNotIn(str(self.root), result.stdout)
        output = json.loads(result.stdout)
        if code is None:
            self.assertEqual(result.returncode, 0, output)
            self.assertEqual(output["status"], "ready")
            self.assertEqual(output["skill"], "cetiadmin")
        else:
            self.assertNotEqual(result.returncode, 0, output)
            self.assertEqual(output, {"status": "error", "code": code})
        return output

    def native(self, name="cetiadmin", *, env=None):
        self.save_config()
        result = self.run_python('''
sys.path.insert(0, os.environ["HERMES_INSTALL_DIR"])
from tools import skills_tool
roots, _ = skills_tool.skill_search_dirs()
error, directory, document = skills_tool.locate_skill(sys.argv[1], None, roots)
candidates = skills_tool._collect_skill_candidates(sys.argv[1], None, [d for _, d in roots])
view = json.loads(skills_tool.skill_view(sys.argv[1], preprocess=False))
readable_references = []
if view.get("success"):
    for reference in (view.get("linked_files") or {}).get("references", []):
        reference_view = json.loads(skills_tool.skill_view(sys.argv[1], file_path=reference, preprocess=False))
        if reference_view.get("success") and reference_view.get("content"):
            readable_references.append(reference)
print(json.dumps({
    "error": bool(error), "document": str(document.resolve()) if document else None,
    "candidateCount": len(candidates),
    "viewSuccess": view.get("success"), "readableReferences": sorted(readable_references),
    "names": sorted(item["name"] for item in skills_tool.find_all_skills()),
    "linked": skills_tool._skill_linked_files(directory) if directory else {},
}))
''', name, env={
            # The unmodified full native loader initializes its disposable HOME.
            # Permit its normal synthetic .env/SOUL reads; the helper has no such exemption.
            "FIXTURE_FORBIDDEN": json.dumps([str(path) for path in self.sentinels
                                             if path.name not in {".env", "SOUL.md"}]),
            **(env or {}),
        })
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def skill(self, root, name, *, declared=None):
        document = root / name / "SKILL.md"
        document.parent.mkdir(parents=True)
        document.write_text(f"---\nname: {declared or Path(name).name}\ndescription: Fixture guide.\n---\n\nA safe fixture.\n")
        return document

    def legacy(self, name="adminclaw", *, root=None):
        document = self.skill(root or self.local, name, declared="adminclaw")
        reference = document.parent / "references/guide.md"
        reference.parent.mkdir()
        reference.write_text("A synthetic former managed reference.\n")
        package = {"version": "synthetic-v1", "sourceRevision": "synthetic",
                   "files": {name: hashlib.sha256(data).hexdigest()
                             for name, data in tree_bytes(document.parent).items()}}
        self.script.with_name(CHECKSUMS.name).write_text(
            "MANIFEST = " + repr({"schemaVersion": 1, "packages": [package]}) + "\n")
        return document.parent

    def test_packaged_loader_drift_is_rejected_before_import(self):
        install = self.root / "changed-install"
        install.mkdir()
        shutil.copy2(Path(NATIVE_INSTALL) / "install-stamp.json", install / "install-stamp.json")
        loaders = ("agent/safe_worker_policy.py", "agent/skill_utils.py", "tools/skills_tool.py", "hermes_constants.py")
        for name in loaders:
            target = install / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(Path(NATIVE_INSTALL) / name, target)
        for name in loaders:
            with self.subTest(loader=name):
                target = install / name
                original = target.read_bytes()
                target.write_bytes(original + b"\n# unexpected source change\n")
                self.check("native_version_mismatch", env={"HERMES_INSTALL_DIR": str(install)})
                target.write_bytes(original)

    def test_builtin_is_unique_with_shared_skills_off_and_on(self):
        for shared in (False, True):
            with self.subTest(shared=shared):
                if shared:
                    store = self.root / "shared-store"
                    self.skill(store, "shared-guide")
                    composed = self.home.parent / ".agents/skills"
                    composed.mkdir(parents=True)
                    (composed / "shared-guide").symlink_to(store / "shared-guide", target_is_directory=True)
                    self.config["skills"]["external_dirs"].append(str(composed))
                self.assertEqual(self.check()["archived"], 0)
                found = self.native()
                self.assertFalse(found["error"])
                self.assertEqual(found["candidateCount"], 1)
                self.assertEqual(found["names"].count("cetiadmin"), 1)
                self.assertNotIn("adminclaw", found["names"])
                self.assertEqual("shared-guide" in found["names"], shared)
                self.assertEqual(found["document"], str(self.bundle / "cetiadmin/SKILL.md"))
                self.assertTrue(found["viewSuccess"])
                self.assertEqual(sorted(found["linked"]["references"]), [
                    "references/crd-reference.md", "references/instance-lifecycle.md",
                    "references/operational-constraints.md", "references/remediation-playbooks.md"])
                self.assertEqual(found["readableReferences"], sorted(found["linked"]["references"]))
        self.assertFalse(self.archive.exists())

    def test_local_named_and_frontmatter_and_flat_conflicts_are_preserved(self):
        for alias in ("cetiadmin", "adminclaw"):
            for shape in ("directory", "frontmatter", "flat"):
                with self.subTest(alias=alias, shape=shape):
                    if shape == "flat":
                        document = self.local / f"{alias}.md"
                        document.write_text("User guide " + PRIVATE)
                    else:
                        document = self.skill(self.local, alias if shape == "directory" else "category/renamed", declared=alias)
                    before = tree_bytes(self.local)
                    self.check("skill_conflict")
                    self.assertEqual(tree_bytes(self.local), before)
                    self.assertFalse(self.archive.exists())
                    if shape == "flat":
                        document.unlink()
                    else:
                        shutil.rmtree(document.parent)

    def test_external_and_create_directory_conflicts(self):
        for field in ("external_dirs", "create_dir"):
            with self.subTest(field=field):
                external = self.root / field
                self.skill(external, "renamed", declared="cetiadmin")
                self.config["skills"][field] = ([str(self.bundle), str(external)] if field == "external_dirs" else str(external))
                self.check("skill_conflict")
                self.assertTrue((external / "renamed/SKILL.md").exists())
                self.config["skills"]["external_dirs"] = [str(self.bundle)]

    def test_trusted_project_override_is_rejected_even_outside_startup_cwd(self):
        for subdir in (".hermes/skills", ".agents/skills"):
            with self.subTest(subdir=subdir):
                project = self.root / subdir.replace("/", "-")
                (project / ".git").mkdir(parents=True)
                document = self.skill(project / subdir, "cetiadmin")
                self.config["skills"]["trusted_project_dirs"] = [str(project)]
                native = self.native(env={"TERMINAL_CWD": str(project)})
                self.assertFalse(native["error"])
                self.assertEqual(native["document"], str(document))
                self.assertEqual(native["candidateCount"], 2)
                self.check("skill_conflict")
                self.assertTrue(document.exists())

    def test_untrusted_or_disabled_project_is_not_a_source(self):
        project = self.root / "project"
        (project / ".git").mkdir(parents=True)
        self.skill(project / ".agents/skills", "cetiadmin")
        self.check(env={"TERMINAL_CWD": str(project)})
        self.config["skills"].update(trusted_project_dirs=[str(project)], project_discovery=False)
        self.check(env={"TERMINAL_CWD": str(project)})

    def test_identical_same_root_copies_are_not_silently_ranked(self):
        document = self.skill(self.local, "adminclaw")
        shutil.copytree(document.parent, self.local / "category/adminclaw")
        native = self.native("adminclaw")
        self.assertFalse(native["error"])
        self.assertEqual(native["candidateCount"], 2)
        self.check("skill_conflict")

    def test_missing_optional_directory_preserves_builtin_registration(self):
        self.config["skills"]["external_dirs"].append(str(self.root / "missing-" / PRIVATE))
        self.check()
        self.assertTrue(self.native()["viewSuccess"])
        self.config["skills"]["external_dirs"] = []
        self.check("builtin_not_registered")

    def test_missing_optional_symlink_target_preserves_builtin_skill(self):
        shared = self.root / "shared"
        shared.symlink_to(self.root / "unmounted", target_is_directory=True)
        self.config["skills"]["external_dirs"].append(str(shared))
        self.check()
        self.assertTrue(self.native()["viewSuccess"])
        self.assertTrue(shared.is_symlink())

    def test_invalid_config_is_reported_without_echoing_its_body(self):
        self.config["skills"] = PRIVATE
        self.check("invalid_config")

    def test_flat_legacy_in_excluded_directory_still_conflicts_with_native_lookup(self):
        hidden = self.local / ".archive/adminclaw.md"
        hidden.parent.mkdir()
        hidden.write_text("A former flat skill.\n")
        self.assertEqual(self.native("adminclaw")["candidateCount"], 1)
        self.check("skill_conflict")
        self.assertTrue(hidden.exists())

    def test_support_documents_are_not_separate_skills(self):
        document = self.skill(self.local, "unrelated")
        reference = document.parent / "references/adminclaw.md"
        reference.parent.mkdir()
        reference.write_text("A reference, not a skill.\n")
        self.skill(document.parent / "references", "cetiadmin")
        self.check()
        self.assertEqual(self.native()["candidateCount"], 1)

    def test_native_relative_expansion_and_same_file_aliases(self):
        shared = self.home.parent / "shared"
        self.skill(shared, "shared-guide")
        self.config["skills"]["external_dirs"] += ["../shared", "~/shared", "${FIXTURE_SHARED}"]
        self.check(env={"FIXTURE_SHARED": str(shared)})
        (self.local / "cetiadmin").symlink_to(self.bundle / "cetiadmin", target_is_directory=True)
        self.check(env={"FIXTURE_SHARED": str(shared)})

    def test_global_or_platform_disabled_cetiadmin_fails(self):
        for fields in ({"disabled": ["cetiadmin"]}, {"disabled": '["cetiadmin"]'},
                       {"platform_disabled": {"slack": ["cetiadmin"]}}):
            with self.subTest(fields=fields):
                self.config["skills"] = {"external_dirs": [str(self.bundle)], **fields}
                self.check("builtin_disabled")

    def test_exact_known_local_legacy_archive_and_retry_preserve_state(self):
        old = self.legacy("category/adminclaw")
        before = tree_bytes(old)
        self.assertEqual(self.check()["archived"], 1)
        destination = self.archive / "synthetic-v1/category/adminclaw"
        self.assertFalse(old.exists())
        self.assertEqual(tree_bytes(destination), before)
        self.assertEqual(self.check()["archived"], 0)
        self.assertEqual(tree_bytes(destination), before)
        self.assertNotIn("adminclaw", self.native()["names"])
        self.assertEqual(self.archive.stat().st_mode & 0o777, 0o700)

    def test_bundle_upgrade_keeps_existing_home_and_shared_content(self):
        shared = self.home.parent / ".agents/skills"
        self.skill(shared, "shared-guide")
        self.config["skills"]["external_dirs"].append(str(shared))
        self.check()
        shared_before = tree_bytes(shared)
        document = self.bundle / "cetiadmin/SKILL.md"
        document.write_text(document.read_text() + "\nFixture asset upgrade.\n")
        self.assertEqual(self.check()["archived"], 0)
        self.assertEqual(tree_bytes(shared), shared_before)
        found = self.native()
        self.assertEqual(found["document"], str(document))
        self.assertIn("shared-guide", found["names"])

    def test_same_version_current_builtin_copy_is_not_old_managed_data(self):
        shutil.copytree(self.bundle / "cetiadmin", self.local / "cetiadmin")
        self.check("skill_conflict")
        self.assertTrue((self.local / "cetiadmin/SKILL.md").exists())

    def test_modified_legacy_and_added_files_or_directories_are_not_archived(self):
        old = self.legacy()
        for name in ("SKILL.md", "references/guide.md", "personal.md", "empty-directory"):
            with self.subTest(name=name):
                target = old / name
                original = target.read_bytes() if target.exists() else None
                if name == "empty-directory":
                    target.mkdir()
                else:
                    target.write_text(PRIVATE)
                before = tree_bytes(old)
                self.check("skill_conflict")
                self.assertEqual(tree_bytes(old), before)
                self.assertFalse(self.archive.exists())
                if original is not None:
                    target.write_bytes(original)
                elif target.is_dir():
                    target.rmdir()
                else:
                    target.unlink()

    def test_known_external_or_project_legacy_is_not_moved(self):
        for kind in ("external", "project"):
            with self.subTest(kind=kind):
                source = self.root / kind
                source.mkdir()
                old = self.legacy(root=source)
                if kind == "external":
                    self.config["skills"]["external_dirs"].append(str(source))
                else:
                    (source / ".git").mkdir()
                    target = source / ".agents/skills"
                    target.mkdir(parents=True)
                    old.rename(target / old.name)
                    old = target / old.name
                    self.config["skills"]["trusted_project_dirs"] = [str(source)]
                self.check("skill_conflict")
                self.assertTrue(old.exists())
                self.assertFalse(self.archive.exists())
                self.config["skills"]["external_dirs"] = [str(self.bundle)]

    def test_conflicts_are_preflighted_before_any_archival(self):
        old = self.legacy()
        self.skill(self.local, "cetiadmin")
        self.check("skill_conflict")
        self.assertTrue(old.exists())
        self.assertFalse(self.archive.exists())

    def test_existing_archive_is_not_overwritten(self):
        old = self.legacy()
        destination = self.archive / "synthetic-v1/adminclaw"
        destination.mkdir(parents=True, mode=0o700)
        (destination / "keep.md").write_text(PRIVATE)
        self.check("archive_conflict")
        self.assertTrue(old.exists())
        self.assertEqual((destination / "keep.md").read_text(), PRIVATE)

    def test_completed_rename_can_resume_after_interruption(self):
        old = self.legacy()
        remaining = self.legacy("category/adminclaw")
        destination = self.archive / "synthetic-v1/adminclaw"
        for path in (self.home / "cetiadmin", self.archive, destination.parent):
            path.mkdir(mode=0o700, exist_ok=True)
        old.rename(destination)
        self.assertEqual(self.check()["archived"], 1)
        self.assertFalse(remaining.exists())
        self.assertEqual(self.check()["archived"], 0)
        self.assertTrue(destination.exists())

    def test_symlinked_archive_does_not_move_or_overwrite_anything(self):
        old = self.legacy()
        outside = self.root / "unrelated"
        outside.mkdir()
        (outside / "keep.md").write_text(PRIVATE)
        self.archive.parent.mkdir(mode=0o700)
        self.archive.symlink_to(outside, target_is_directory=True)
        self.check("unsafe_archive")
        self.assertTrue(old.exists())
        self.assertEqual(tree_bytes(outside), {"keep.md": PRIVATE.encode()})

    def test_archive_cannot_live_inside_a_discovery_root(self):
        old = self.legacy()
        self.config["skills"]["external_dirs"].append(str(self.home))
        self.check("unsafe_archive")
        self.assertTrue(old.exists())

    def test_skill_file_symlink_is_rejected_without_reading_target(self):
        for name in ("renamed/SKILL.md", "cetiadmin.md"):
            with self.subTest(name=name):
                link = self.local / name
                link.parent.mkdir(parents=True, exist_ok=True)
                link.symlink_to(self.home / ".env")
                self.check("unsafe_skill_file")
                self.assertTrue(link.is_symlink())
                link.unlink()

    def test_legacy_symlink_or_extra_credential_file_is_not_followed(self):
        old = self.legacy()
        extra = old / ".env"
        extra.symlink_to(self.home / ".env")
        self.check("skill_conflict")
        self.assertTrue(extra.is_symlink())
        extra.unlink()
        external = self.root / "user-owned"
        old.rename(external)
        old.symlink_to(external, target_is_directory=True)
        self.check("skill_conflict")
        self.assertTrue(old.is_symlink())


if __name__ == "__main__":
    unittest.main()
