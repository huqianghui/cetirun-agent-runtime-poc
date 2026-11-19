#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Pin the reserved Memory skill to its installed assets in native skill loaders."""

import argparse
import hashlib
from pathlib import Path


HERMES_FILTER = '''def cetirun_memory_skill_allowed(skill_file: Path) -> bool:
    assets = os.environ.get("CETIRUN_MEMORY_ASSETS")
    if not assets:
        return True
    reserved = Path(assets) / "skills/cetirun-memory/SKILL.md"
    if skill_file.resolve() == reserved.resolve():
        return True
    if skill_file.parent.name == "cetirun-memory" or skill_file.stem == "cetirun-memory":
        return False
    try:
        frontmatter, _ = parse_frontmatter(skill_file.read_text(encoding="utf-8"))
    except (OSError, UnicodeError):
        return True
    return frontmatter.get("name") != "cetirun-memory"


'''

PATCHES = {
    "hermes": (
        ("agent/skill_utils.py", "9b643866caf9b0db3b568045e2b49bf2903f89197497c815a492afec89b33722", (
            ("def iter_skill_index_files(skills_dir: Path, filename: str):\n",
             HERMES_FILTER + "def iter_skill_index_files(skills_dir: Path, filename: str):\n"),
            ("    yield from map(Path, sorted(matches))\n",
             "    yield from (path for path in map(Path, sorted(matches))\n"
             '                if filename != "SKILL.md" or cetirun_memory_skill_allowed(path))\n'),
        )),
        ("tools/skills_tool.py", "0b6fbd304a6fff74c3e1afee3e959985b2205dc1926c7474ca5cc7a8bd8ea107", (
            ("    def _record(sd: Optional[Path], smd: Path) -> None:\n",
             "    def _record(sd: Optional[Path], smd: Path) -> None:\n"
             "        from agent.skill_utils import cetirun_memory_skill_allowed\n"
             "        if not cetirun_memory_skill_allowed(smd):\n"
             "            return\n"),
        )),
        ("agent/prompt_builder.py", "92794560ec7e3e0396e9f33bc505c7d2e13aed1275f199b39ed934f3c54a9fcb", (
            ("    snapshot = _load_skills_snapshot(skills_dir, manifest)\n",
             '    snapshot = None if os.environ.get("CETIRUN_MEMORY_ASSETS") else _load_skills_snapshot(skills_dir, manifest)\n'),
            ("        if cached is not None and not app_gated:\n",
             '        if cached is not None and not app_gated and not os.environ.get("CETIRUN_MEMORY_ASSETS"):\n'),
            ("    if snapshot is None:  # persist for fast cold-start reuse (best-effort)\n",
             '    if snapshot is None and not os.environ.get("CETIRUN_MEMORY_ASSETS"):\n'),
        )),
    ),
    "openclaw": (
        ("dist/workspace-skill-loader-CoQ1pd8t.mjs", "7db6941683a467cd406ada13c8a211c0dcbd85f646c72887801e58e9e66cffb9", (
            ("\tfor (const record of records) {\n",
             "\tfor (const record of records) {\n"
             "\t\tconst assets = process.env.CETIRUN_MEMORY_ASSETS;\n"
             '\t\tif (assets && record.skill.name === "cetirun-memory" &&\n'
             '\t\t\tcanonicalizePath(record.skill.filePath) !== canonicalizePath(path.join(assets, "skills/cetirun-memory/SKILL.md"))) continue;\n'),
        )),
    ),
}


def prepare_patches(root, runtime):
    updates = []
    for relative, expected, replacements in PATCHES[runtime]:
        path = (root / relative).resolve()
        if not path.is_relative_to(root):
            raise ValueError(f"Source path escapes native checkout: {relative}")
        source = path.read_text(encoding="utf-8")
        original = source
        for before, after in reversed(replacements):
            original = original.replace(after, before, 1)
        if hashlib.sha256(original.encode()).hexdigest() != expected:
            raise ValueError(f"Unsupported {runtime} skill source: {relative}; review the pinned integration")
        updated = original
        for before, after in replacements:
            if updated.count(before) != 1:
                raise ValueError(f"Expected one skill integration point: {relative}")
            updated = updated.replace(before, after, 1)
        if updated != source:
            updates.append((path, updated))
    return updates


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True, help="Pinned native source directory")
    parser.add_argument("--runtime", choices=PATCHES, required=True)
    parser.add_argument("--check", action="store_true", help="Validate without modifying the source")
    args = parser.parse_args()
    try:
        updates = prepare_patches(args.root.resolve(), args.runtime)
        if not args.check:
            for path, content in updates:
                path.write_text(content, encoding="utf-8")
    except (OSError, ValueError) as error:
        parser.exit(1, f"{error}\n")
    print(f"Memory skill source: {len(updates)} file(s) {'validated' if args.check else 'updated'}")


if __name__ == "__main__":
    main()
