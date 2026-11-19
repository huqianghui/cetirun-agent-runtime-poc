#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Check reserved skill sources before starting CetiAdmin's native scheduler.

Hermes owns discovery and lookup. This preflight uses its import-light index
and metadata parser to reject shadowing that its project precedence or
identical-copy ranking would otherwise permit. It never
edits native configuration or personal state. Only an exact, known local legacy
package may be renamed into HERMES_HOME/cetiadmin/skill-archive/<version>/,
retaining its relative location for recovery outside every active skill root.
"""

import argparse
from contextlib import contextmanager
import hashlib
import importlib
import json
import os
from pathlib import Path
import re
import runpy
import stat
import sys


HERMES_SHA = "b200f5bc60e31588ba2cb60e288029c4a54ba5fa"
# Private native APIs include the image's reserved Memory skill patch.
LOADER_HASHES = {
    "agent/safe_worker_policy.py": "f0759f0868f402d5aff6007303ad503e3d3d92999ed01693037bc8f07b4a2738",
    "agent/skill_utils.py": "ee78772f1773bfaf039caa2e02f246ab9f570ab1d85779d2c34a1e0790d80bf0",
    "tools/skills_tool.py": "cd530969317f1ff4c80b06fc24973cd68c9bba9b5f95bfdd67356048702f7ac3",
    "hermes_constants.py": "74d849629c176703d48f9aa43a245fd8819545ac908c805e347db7d691976510",
}
ASSETS = Path(__file__).resolve().parents[1]
RESERVED = ("cetiadmin", "adminclaw")


class SourceError(Exception):
    """A fixed public error code; never an input value or native exception."""


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise SourceError("invalid_arguments")


@contextmanager
def quiet_native_output():
    sys.stdout.flush()
    sys.stderr.flush()
    saved = os.dup(1), os.dup(2)
    try:
        with open(os.devnull, "w") as sink:
            os.dup2(sink.fileno(), 1)
            os.dup2(sink.fileno(), 2)
            try:
                yield
            finally:
                sys.stdout.flush()
                sys.stderr.flush()
    finally:
        for destination, descriptor in zip((1, 2), saved):
            os.dup2(descriptor, destination)
            os.close(descriptor)


def runtime_directory(name):
    raw = os.environ.get(name, "")
    path = Path(raw)
    if not raw or not path.is_absolute() or path == Path("/") or not path.is_dir():
        raise SourceError("invalid_runtime")
    return path.resolve()


def load_native(install):
    try:
        stamp = json.loads((install / "install-stamp.json").read_text(encoding="utf-8"))
        if (stamp.get("commit") != HERMES_SHA or stamp.get("baseVersion") != "0.22.7"
                or stamp.get("updateMechanism") != "external" or stamp.get("distribution") != "docker"):
            raise ValueError()
        for name, digest in LOADER_HASHES.items():
            if hashlib.sha256((install / name).read_bytes()).hexdigest() != digest:
                raise ValueError()
    except (OSError, UnicodeError, ValueError):
        raise SourceError("native_version_mismatch") from None
    sys.dont_write_bytecode = True
    sys.path.insert(0, str(install))
    identity = importlib.import_module("hermes_cli.version_info").get_version_info()
    if identity.base_version != "0.22.7" or identity.commit != HERMES_SHA or identity.source != "docker":
        raise SourceError("native_version_mismatch")
    utils = importlib.import_module("agent.skill_utils")
    if not Path(utils.__file__).resolve().is_relative_to(install):
        raise SourceError("native_version_mismatch")
    # tools.skills_tool and get_disabled_skill_names import provider/config
    # initialization that reads .env/SOUL. This preflight must not initialize HOME.
    return utils


def configured_strings(value):
    if value is None:
        return []
    if isinstance(value, str):
        value = [value]
    if not isinstance(value, list) or any(not isinstance(item, str) or not item.strip() for item in value):
        raise SourceError("invalid_config")
    return value


def source_roots(home, bundle, utils):
    try:
        config = utils.yaml_load((home / "config.yaml").read_text(encoding="utf-8"))
        skills = config["skills"]
        if not isinstance(skills, dict):
            raise ValueError()
    except Exception:
        raise SourceError("invalid_config") from None
    configured_strings(skills.get("external_dirs"))
    if bundle not in utils.get_external_skills_dirs():
        raise SourceError("builtin_not_registered")
    platforms = skills.get("platform_disabled") or {}
    if not isinstance(platforms, dict):
        raise SourceError("invalid_config")
    if any("cetiadmin" in utils._normalize_string_set(disabled)
           for disabled in [skills.get("disabled"), *platforms.values()]):
        raise SourceError("builtin_disabled")
    configured_strings(skills.get("trusted_project_dirs"))
    roots = utils.get_all_skills_dirs()
    if skills.get("project_discovery") is not False:
        # Gateway/cron sessions can select a trusted project other than the launch cwd.
        for project in sorted(utils._project_trusted_dirs_from_config()):
            if utils.find_project_root(project) == project:
                roots.extend(utils._candidate_project_skills_dirs(project))
    return list(dict.fromkeys(root.resolve() for root in roots if root.exists()))


def check_skill_file(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise SourceError("unsafe_skill_file")
    # The loader's forgiving frontmatter reader must not hide an unreadable alias.
    with path.open("rb"):
        pass


def candidates(roots, utils):
    """Check every reserved alias, before native project/copy precedence applies.

    Use the same native index/parser as _collect_skill_candidates. Legacy flat
    lookups also search outside that index (including excluded directories), but
    Markdown owned by a directory skill is a reference, not a second skill.
    """
    matches = {name: {} for name in RESERVED}

    def record(name, directory, document):
        check_skill_file(document)
        matches[name][document.resolve()] = (directory, document)

    for root in roots:
        for document in utils.iter_skill_index_files(root, "SKILL.md"):
            check_skill_file(document)
            frontmatter, _ = utils.parse_frontmatter(document.read_text(encoding="utf-8-sig", errors="replace"))
            for name in RESERVED:
                if document.parent.name == name or frontmatter.get("name") == name:
                    record(name, document.parent, document)
        for name in RESERVED:
            document = root / name / "SKILL.md"
            if (document.exists() or document.is_symlink()) and not utils.is_skill_support_path(document):
                record(name, document.parent, document)
            for document in root.rglob(name + ".md"):
                parents = document.relative_to(root).parents
                package_owned = any(parent != Path(".") and (root / parent / "SKILL.md").is_file()
                                    for parent in parents)
                if not utils.is_skill_support_path(document) and not package_owned:
                    record(name, None, document)
    return {name: list(found.values()) for name, found in matches.items()}


def legacy_packages():
    manifest = runpy.run_path(str(ASSETS / "scripts/legacy_skill_checksums.py"))["MANIFEST"]
    if manifest["schemaVersion"] != 1 or not isinstance(manifest["packages"], list):
        raise SourceError("invalid_legacy_manifest")
    for package in manifest["packages"]:
        if (not re.fullmatch(r"[A-Za-z0-9_-]{1,80}", package["version"])
                or not isinstance(package["files"], dict) or "SKILL.md" not in package["files"]):
            raise SourceError("invalid_legacy_manifest")
        for name, digest in package["files"].items():
            path = Path(name)
            if (path.is_absolute() or ".." in path.parts or str(path) != name
                    or not re.fullmatch(r"[0-9a-f]{64}", digest)):
                raise SourceError("invalid_legacy_manifest")
    return manifest["packages"]


def legacy_version(directory, local, packages):
    if (directory is None or directory == local or not directory.is_relative_to(local)
            or directory.resolve() != directory):
        return None
    entries = list(directory.rglob("*"))
    if any(p.is_symlink() or not (p.is_file() or p.is_dir()) for p in entries):
        return None
    files = {p.relative_to(directory).as_posix() for p in entries if p.is_file()}
    dirs = {p.relative_to(directory).as_posix() for p in entries if p.is_dir()}
    for package in packages:
        expected = package["files"]
        expected_dirs = {str(parent) for name in expected for parent in Path(name).parents if str(parent) != "."}
        if files != set(expected) or dirs != expected_dirs:
            continue
        if any((directory / name).stat().st_nlink != 1 for name in expected):
            continue
        if all(hashlib.sha256((directory / name).read_bytes()).hexdigest() == digest
               for name, digest in expected.items()):
            return package["version"]
    return None


def private_directory(path, home):
    for parent in [*reversed(path.relative_to(home).parents), path.relative_to(home)]:
        if parent == Path("."):
            continue
        directory = home / parent
        if directory.is_symlink():
            raise SourceError("unsafe_archive")
        directory.mkdir(mode=0o700, exist_ok=True)
        info = directory.stat()
        if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) & 0o077:
            raise SourceError("unsafe_archive")


def check_sources(home, install):
    utils = load_native(install)
    try:
        config = utils.yaml_load((home / "config.yaml").read_text(encoding="utf-8"))
        if not isinstance(config, dict):
            raise ValueError()
    except (OSError, UnicodeError, ValueError):
        raise SourceError("invalid_config") from None
    # This short-lived preflight uses the same skill resolver over a frozen,
    # credential-free config snapshot; config-backend bootstrap must not read .env.
    from agent.safe_worker_policy import _bind_worker_config
    _bind_worker_config(config)
    bundle = (ASSETS / "skills").resolve()
    builtin = bundle / "cetiadmin/SKILL.md"
    if not builtin.is_file():
        raise SourceError("missing_builtin")
    check_skill_file(builtin)
    frontmatter, _ = utils.parse_frontmatter(builtin.read_text(encoding="utf-8"))
    if (frontmatter.get("name") != "cetiadmin" or not utils.skill_matches_platform(frontmatter)
            or frontmatter.get("environments") or frontmatter.get("requires_apps")):
        raise SourceError("builtin_not_available")
    roots = source_roots(home, bundle, utils)
    archive = home / "cetiadmin/skill-archive"
    if any(archive.is_relative_to(root) for root in roots):
        raise SourceError("unsafe_archive")
    packages = legacy_packages()
    moves = {}
    found_builtin = False
    for name, matches in candidates(roots, utils).items():
        for directory, document in matches:
            if name == "cetiadmin" and document.resolve() == builtin:
                found_builtin = True
                continue
            version = legacy_version(directory, home / "skills", packages)
            if version is None:
                raise SourceError("skill_conflict")
            moves[directory] = archive / version / directory.relative_to(home / "skills")
    if not found_builtin:
        raise SourceError("missing_builtin")
    # Reject the whole conflicting set before retiring even one known package.
    for target in moves.values():
        if target.exists() or target.is_symlink():
            raise SourceError("archive_conflict")
    for directory, target in moves.items():
        private_directory(target.parent, home)
        directory.rename(target)
    found = candidates(roots, utils)
    if (len(found["cetiadmin"]) != 1 or found["cetiadmin"][0][1].resolve() != builtin
            or found["adminclaw"]):
        raise SourceError("native_readback_failed")
    return {"status": "ready", "skill": "cetiadmin", "archived": len(moves)}


def main():
    parser = Parser(prog="check_skill_sources.py", description=__doc__, epilog=(
        "Requires HERMES_HOME and HERMES_INSTALL_DIR. Run with the native Hermes Python; "
        "the read-only bundled skill is located relative to this script. "
        "Errors contain only fixed codes; resolve conflicts without overwriting user files."))
    try:
        parser.parse_args()
    except SourceError:
        print(json.dumps({"status": "error", "code": "invalid_arguments"}, sort_keys=True))
        return 1
    try:
        with quiet_native_output():
            home = runtime_directory("HERMES_HOME")
            install = runtime_directory("HERMES_INSTALL_DIR")
            result = check_sources(home, install)
    except SourceError as error:
        result = {"status": "error", "code": str(error)}
    except (Exception, SystemExit):
        result = {"status": "error", "code": "native_check_failed"}
    print(json.dumps(result, sort_keys=True))
    return 0 if result["status"] == "ready" else 1


if __name__ == "__main__":
    sys.exit(main())
