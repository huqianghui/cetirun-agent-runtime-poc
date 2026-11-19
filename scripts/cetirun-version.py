#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Read, update, and verify synchronized CetiRun release versions."""

import argparse
import json
from pathlib import Path
import re
import sys


ROOT = Path(__file__).resolve().parents[1]
# The version is also an image tag, so build metadata is not supported.
NUMBER = r"(?:0|[1-9][0-9]*)"
PRERELEASE_IDENTIFIER = rf"(?:{NUMBER}|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
VERSION_PATTERN = re.compile(
    rf"{NUMBER}\.{NUMBER}\.{NUMBER}"
    rf"(?:-{PRERELEASE_IDENTIFIER}(?:\.{PRERELEASE_IDENTIFIER})*)?"
)
PENDING_UPDATES = {}
DEFAULT_REGISTRY = "mcr.microsoft.com/aks/ai-runtime/cetirun"


def validate_push_registry(registry):
    host = registry.removeprefix("oci://").split("/", 1)[0].split(":", 1)[0]
    if host.rstrip(".").lower() == "mcr.microsoft.com":
        raise ValueError(
            "MCR is a pull endpoint, not a push destination. Set REGISTRY to a writable "
            "registry; for MCR publication use the ADO pipeline's backing ACR and automatic sync."
        )


def replace(path, pattern, replacement, expected=1, limit=0, updates=None):
    updates = PENDING_UPDATES if updates is None else updates
    source = updates.get(path, path.read_text())
    updated, count = re.subn(
        pattern, replacement, source, count=limit, flags=re.MULTILINE
    )
    if count != expected:
        raise ValueError(
            f"{path}: expected {expected} version field(s), found {count}"
        )
    updates[path] = updated


def top_level_yaml(path, field):
    match = re.search(
        rf"^{re.escape(field)}:\s*[\"']?([^\"'\s#]+)",
        path.read_text(),
        re.MULTILINE,
    )
    if not match:
        raise ValueError(f"{path.relative_to(ROOT)}: missing {field}")
    return match.group(1)


def validate_version(version):
    if len(version) > 128 or not VERSION_PATTERN.fullmatch(version):
        raise ValueError(
            f"{version!r} must be X.Y.Z or X.Y.Z-prerelease "
            "(at most 128 characters, without build metadata)"
        )


def chart_paths():
    return [entry["path"] for entry in json.loads(
        (ROOT / "scripts/release-artifacts.json").read_text()
    )["charts"]]


def chart_updates(root, paths, version, app_version=None):
    """Prepare first-party chart metadata without changing the source checkout."""
    updates = {}
    names = {top_level_yaml(root / relative / "Chart.yaml", "name") for relative in paths}
    for relative in paths:
        path = root / relative / "Chart.yaml"
        replace(path, r"^version:\s*.*$", f"version: {version}", updates=updates)
        replace(path, r"^appVersion:\s*.*$", f'appVersion: "{app_version or version}"', updates=updates)
        source = updates[path]
        for name in names:
            source = re.sub(
                rf"(^  - name: {re.escape(name)}\n    version: )[^\n]+(\n    repository: file://)",
                rf"\g<1>{version}\g<2>", source, flags=re.MULTILINE,
            )
        updates[path] = source
    if "app/cetirun-operator/chart" in paths:
        replace(root / "app/cetirun-operator/chart/values.yaml",
                r'(^  chart:\n(?:    [^\n]*\n)*?    version:\s*")[^"]*(")',
                rf'\g<1>{version}\g<2>', updates=updates)
    return updates


def set_version(version):
    PENDING_UPDATES.clear()
    PENDING_UPDATES.update(chart_updates(ROOT, chart_paths(), version))
    replace(
        ROOT / "app/cetirun-operator/cmd/main.go",
        r'(flag\.StringVar\(&chartVersion, "chart-version", ")[^"]+(")',
        rf"\g<1>{version}\g<2>",
    )
    for relative in (
        "app/cetirun-operator/deploy/deploy.sh",
        "app/cetirun-portal/deploy/deploy.sh",
    ):
        replace(
            ROOT / relative,
            r'^CHART_VERSION="\$\{CHART_VERSION:-\$\{VERSION:-[^}]+\}\}"$',
            f'CHART_VERSION="${{CHART_VERSION:-${{VERSION:-{version}}}}}"',
        )

    for relative in ("app/internal/go.mod", "app/cetirun-portal/go.mod"):
        replace(
            ROOT / relative,
            r"^(\s*github\.com/Azure/CetiRun/app/cetirun-operator )v\S+$",
            rf"\g<1>v{version}",
        )
    for directory in ("app/cetirun-portal/web", "examples/pi-agent"):
        for filename in ("package.json", "package-lock.json"):
            replace(
                ROOT / directory / filename,
                r'^  "version": "[^"]+",$',
                f'  "version": "{version}",',
            )
        replace(
            ROOT / directory / "package-lock.json",
            r'^      "version": "[^"]+",$',
            f'      "version": "{version}",',
            1,
            1,
        )
    replace(
        ROOT / "app/cetirun-memory-context/assets/adapters/openclaw/package.json",
        r'^  "version": "[^"]+",$',
        f'  "version": "{version}",',
    )
    for path, content in PENDING_UPDATES.items():
        path.write_text(content)


def check_version(version):
    errors = []

    for directory in chart_paths():
        relative = directory + "/Chart.yaml"
        path = ROOT / relative
        if top_level_yaml(path, "version") != version:
            errors.append(f"{relative}: version is not {version}")
        if top_level_yaml(path, "appVersion") != version:
            errors.append(f"{relative}: appVersion is not {version}")

    platform_chart = (ROOT / "app/cetirun-platform/Chart.yaml").read_text()
    platform_lock = (ROOT / "app/cetirun-platform/Chart.lock").read_text()
    dependency_versions = re.findall(
        r"^    version:\s*[\"']?([^\"'\s#]+)", platform_chart, re.MULTILINE
    )
    lock_versions = re.findall(
        r"^  version:\s*[\"']?([^\"'\s#]+)", platform_lock, re.MULTILINE
    )
    if not dependency_versions or any(value != version for value in dependency_versions):
        errors.append("app/cetirun-platform/Chart.yaml: dependency versions differ")
    if lock_versions != dependency_versions:
        errors.append("app/cetirun-platform/Chart.lock: dependency versions differ")

    expected_text = {
        "app/cetirun-memory-context/assets/adapters/openclaw/package.json": f'"version": "{version}"',
        "app/cetirun-operator/chart/values.yaml": f'version: "{version}"',
        "app/cetirun-operator/cmd/main.go": f'"chart-version", "{version}"',
        "app/cetirun-operator/deploy/deploy.sh": f"VERSION:-{version}",
        "app/cetirun-portal/deploy/deploy.sh": f"VERSION:-{version}",
        "app/cetirun-portal/go.mod": f"cetirun-operator v{version}",
        "app/internal/go.mod": f"cetirun-operator v{version}",
    }
    for relative, expected in expected_text.items():
        if expected not in (ROOT / relative).read_text():
            errors.append(f"{relative}: expected {expected!r}")

    for directory in ("app/cetirun-portal/web", "examples/pi-agent"):
        package = json.loads((ROOT / directory / "package.json").read_text())
        package_lock = json.loads((ROOT / directory / "package-lock.json").read_text())
        if package.get("version") != version:
            errors.append(f"{directory}/package.json: version differs")
        if package_lock.get("version") != version:
            errors.append(f"{directory}/package-lock.json: version differs")
        if package_lock.get("packages", {}).get("", {}).get("version") != version:
            errors.append(f"{directory}/package-lock.json: root package differs")

    if errors:
        raise ValueError("\n".join(errors))


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("get", help="print the Runtime Chart version")
    subparsers.add_parser("registry", help="print the default public artifact registry prefix")
    push_parser = subparsers.add_parser("check-push-registry", help="reject the public MCR pull endpoint as a push target")
    push_parser.add_argument("registry")
    for command in ("set", "check"):
        subparser = subparsers.add_parser(command)
        subparser.add_argument("version", help="X.Y.Z[-prerelease], e.g. 0.1.0-dev.1; no +build metadata")
    return parser.parse_args()


def main():
    args = parse_args()
    if args.command == "get":
        print(top_level_yaml(ROOT / "app/cetirun-chart/Chart.yaml", "version"))
        return 0
    if args.command == "registry":
        print(DEFAULT_REGISTRY)
        return 0
    if args.command == "check-push-registry":
        validate_push_registry(args.registry)
        return 0
    validate_version(args.version)
    if args.command == "set":
        set_version(args.version)
    else:
        check_version(args.version)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        sys.exit(1)
