#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Check first-party source headers without inspecting dependencies or secrets.

This is a source-header check, not an authorship or dependency-license audit.
Keep the explicit provenance exceptions aligned with NOTICE. Unknown notices
and generated files require review; the adder never replaces an existing notice.
"""

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path

COPYRIGHT = "Copyright (c) Microsoft Corporation."
LICENSE = "Licensed under the MIT License."
REQUIRED_DIRS = ("app", "infra", "scripts")
DEFAULT_ROOT = Path(__file__).resolve().parent.parent
UTF8_BOM = b"\xef\xbb\xbf"

# These are reviewed path exceptions, not patterns that trust arbitrary new code.
EXCLUSIONS = {
    "app/cetirun-chart/templates/_helpers.tpl":
        "Helm scaffold-derived helpers; see NOTICE",
    "app/cetirun-ai-gateway/chart/templates/_helpers.tpl":
        "Helm scaffold-derived helpers; see NOTICE",
    "app/cetirun-operator/chart/templates/_helpers.tpl":
        "Helm-style scaffold; exact provenance not recorded; see NOTICE",
    "app/cetirun-portal/chart/templates/_helpers.tpl":
        "Helm-style scaffold; exact provenance not recorded; see NOTICE",
    "infra/istio/templates/_helpers.tpl":
        "Helm-style scaffold; exact provenance not recorded; see NOTICE",
    "app/cetirun-operator/api/v1alpha1/groupversion_info.go":
        "Kubebuilder-style scaffold; exact provenance not recorded; see NOTICE",
    "app/cetirun-operator/api/v1alpha1/zz_generated.deepcopy.go":
        "controller-gen output; do not hand-edit generated code",
    "app/cetirun-portal/web/src/vite-env.d.ts":
        "Vite client type-reference scaffold",
}
ARTIFACT_DIRS = {
    "node_modules", "vendor", "third_party", "third-party", "dist", "ts-dist",
    "build", "bin", "coverage", "__pycache__", "venv",
}
SLASH = (f"// {COPYRIGHT}", f"// {LICENSE}")
HASH = (f"# {COPYRIGHT}", f"# {LICENSE}")
STYLES = {
    ".go": SLASH, ".js": SLASH, ".jsx": SLASH, ".mjs": SLASH, ".cjs": SLASH,
    ".ts": SLASH, ".tsx": SLASH, ".bicep": SLASH, ".kql": SLASH,
    ".py": HASH, ".sh": HASH,
    ".css": ("/*", f" * {COPYRIGHT}", f" * {LICENSE}", " */"),
    ".html": ("<!--", COPYRIGHT, LICENSE, "-->"),
    ".tpl": ("{{/*", COPYRIGHT, LICENSE, "*/}}"),
}
ENCODING = re.compile(r"^[ \t\f]*#.*?coding[:=][ \t]*[-\w.]+")
NOTICE = re.compile(
    r"(?im)^\s*(?://|#|/\*|\*|<!--|\{\{/\*)?\s*"
    r"(?:copyright\b|SPDX-(?:License-Identifier|FileCopyrightText)\s*:|"
    r"licensed under\b)"
)
GENERATED = re.compile(r"(?im)^\s*(?://|#|/\*|\*)\s*Code generated\b")


def exclusion_reason(relative: str) -> str | None:
    path = Path(relative)
    if any(
        part in ARTIFACT_DIRS or (part.startswith(".") and part != ".github")
        for part in path.parts
    ):
        return "hidden, dependency, or build-output path"
    if relative in EXCLUSIONS:
        return EXCLUSIONS[relative]
    return None


def header_for(relative: str) -> tuple[str, ...] | None:
    name = Path(relative).name
    if name in {"Dockerfile", "Makefile"} or name.startswith(("Dockerfile.", "Makefile.")):
        return HASH
    return STYLES.get(Path(name).suffix.lower())


def validate_root(root: Path) -> Path:
    root = root.resolve(strict=True)
    if not root.is_dir():
        raise ValueError("--root must be a directory")
    license_path = root / "LICENSE"
    if license_path.is_symlink() or not license_path.is_file():
        raise ValueError("--root must contain a regular LICENSE file")
    for name in REQUIRED_DIRS:
        directory = root / name
        if directory.is_symlink() or not directory.is_dir():
            raise ValueError(f"--root must contain a real {name}/ directory")
    return root


def git_files(root: Path) -> list[str]:
    result = subprocess.run(
        ["git", "-C", str(root), "ls-files", "--cached", "--others",
         "--exclude-standard", "-z"],
        check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    return sorted({os.fsdecode(name) for name in result.stdout.split(b"\0") if name})


def export_files(root: Path) -> list[str]:
    files = []

    def fail(error):
        raise error

    for directory, dirs, names in os.walk(root, onerror=fail):
        base = Path(directory)
        dirs[:] = sorted(
            name for name in dirs
            if not (base / name).is_symlink()
            and not exclusion_reason((base / name).relative_to(root).as_posix())
        )
        files.extend((base / name).relative_to(root).as_posix() for name in names)
    return sorted(files)


def source_files(root: Path) -> list[tuple[str, tuple[str, ...]]]:
    paths = git_files(root) if (root / ".git").exists() else export_files(root)
    sources = []
    for relative in paths:
        if exclusion_reason(relative):
            continue
        header = header_for(relative)
        if header is None:
            continue
        path = root / relative
        # Never follow a link to a credential store or another checkout.
        if any(part.is_symlink() for part in (path, *path.parents) if part != root):
            continue
        if not path.exists():  # A tracked file deleted in the working tree.
            continue
        if not path.is_file():
            raise ValueError(f"{relative}: expected a regular source file")
        sources.append((relative, header))
    if not sources:
        raise ValueError("no supported first-party source files found under --root")
    return sources


def insertion_index(relative: str, lines: list[str]) -> int:
    if Path(relative).suffix == ".html" and lines:
        # Hugo content frontmatter and template definitions must stay first.
        first = lines[0].strip()
        if first in {"---", "+++"}:
            for position, line in enumerate(lines[1:], start=1):
                if line.strip() == first:
                    return position + 1
            raise ValueError("unclosed HTML frontmatter")
        if first.startswith("{{") and "define " in first:
            return 1
    index = 1 if lines and lines[0].startswith("#!") else 0
    if Path(relative).suffix == ".py":
        # PEP 263 permits the coding cookie on either of the first two lines.
        for position, line in enumerate(lines[:2]):
            if ENCODING.match(line):
                index = position + 1
    if Path(relative).name.startswith("Dockerfile"):
        # Docker parser directives must precede ordinary comments.
        while index < len(lines) and re.match(
            r"(?i)^#\s*(syntax|escape|check)\s*=", lines[index]
        ):
            index += 1
    return index


def has_header(relative: str, text: str, header: tuple[str, ...]) -> bool:
    lines = text.splitlines()
    index = insertion_index(relative, lines)
    return tuple(lines[index:index + len(header)]) == header


def add_header(relative: str, text: str, header: tuple[str, ...]) -> str:
    if has_header(relative, text, header):
        return text
    if NOTICE.search(text) or GENERATED.search(text):
        raise ValueError("existing notice or generated source requires provenance review")
    newline = "\r\n" if "\r\n" in text else "\n"
    lines = text.splitlines(keepends=True)
    index = insertion_index(relative, lines)
    before, after = "".join(lines[:index]), "".join(lines[index:])
    if before and not before.endswith(("\n", "\r")):
        before += newline
    separator = newline * 2 if after else newline
    return before + newline.join(header) + separator + after


def read_source(path: Path) -> tuple[bytes, bytes, str]:
    original = path.read_bytes()
    bom = UTF8_BOM if original.startswith(UTF8_BOM) else b""
    text = original[len(bom):].decode("utf-8")
    if "\0" in text:
        raise ValueError("binary data in source file")
    return original, bom, text


def run(mode: str, argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=f"{mode.capitalize()} CetiRun first-party copyright/MIT headers.",
        epilog=(
            "Scope: supported source formats and Makefiles across the repository. "
            "YAML, JSON, Markdown, lockfiles, dependencies and generated/scaffold "
            "exceptions are not relicensed. Exit 0: success; 1: header/review "
            "failure; 2: invalid input or filesystem/Git error. No network calls."
        ),
    )
    parser.add_argument(
        "--root", type=Path, default=DEFAULT_ROOT,
        help="checkout or source-export root (default: this script's repository)",
    )
    parser.add_argument(
        "--list-exclusions", action="store_true",
        help="print documented scope exceptions and exit",
    )
    if mode == "add":
        parser.add_argument("--dry-run", action="store_true", help="list changes without writing")
        parser.add_argument(
            "--confirm-first-party", action="store_true",
            help="confirm authorship of all missing-header candidates before writing",
        )
    args = parser.parse_args(argv)
    if mode == "add" and not (
        args.dry_run or args.confirm_first_party or args.list_exclusions
    ):
        parser.error("review --dry-run first, then use --confirm-first-party to write")
    try:
        root = validate_root(args.root)
        if args.list_exclusions:
            for path, reason in sorted(EXCLUSIONS.items()):
                print(f"{path}: {reason}")
            print("Hidden paths except .github/, dependencies and build output: excluded")
            print("Unsupported formats (including YAML, JSON and Markdown): excluded")
            return 0
        sources = source_files(root)
        pending = []
        failures = []
        for relative, header in sources:
            try:
                original, bom, text = read_source(root / relative)
                if has_header(relative, text, header):
                    continue
                updated = add_header(relative, text, header)
                pending.append((relative, original, bom + updated.encode("utf-8")))
            except (UnicodeError, ValueError) as error:
                failures.append(f"{relative}: {error}")
        if failures:
            for failure in failures:
                print(failure, file=sys.stderr)
            print("No files changed. Resolve provenance/format issues first.", file=sys.stderr)
            return 1
        if mode == "check":
            for relative, _, _ in pending:
                print(f"{relative}: missing copyright/MIT header", file=sys.stderr)
            if pending:
                print("Review NOTICE and run add-license-headers.py --dry-run.", file=sys.stderr)
                return 1
            print(f"License headers OK: {len(sources)} source files checked.")
            return 0
        # Preflight every candidate before writing; preserve concurrent edits.
        for relative, original, _ in pending:
            if (root / relative).read_bytes() != original:
                raise ValueError(f"{relative}: changed during scan; retry after review")
        for relative, original, updated in pending:
            if not args.dry_run:
                path = root / relative
                if path.is_symlink() or path.read_bytes() != original:
                    raise ValueError(f"{relative}: changed during update; stopped")
                path.write_bytes(updated)
            print(f"{'Would add' if args.dry_run else 'Added'} header: {relative}")
        print(f"{len(pending)} header(s) {'planned' if args.dry_run else 'added'}.")
        return 0
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"license headers: {error}", file=sys.stderr)
        return 2


def check_headers(argv: list[str] | None = None) -> int:
    return run("check", argv)


def add_headers(argv: list[str] | None = None) -> int:
    return run("add", argv)


if __name__ == "__main__":
    raise SystemExit(check_headers())
