#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Synchronize pip inventories only after validating installed vendor packages."""

import argparse
import copy
import json
from importlib.metadata import distributions
from pathlib import Path

PAYLOADS = {
    "msgpack": ("msgpack/__init__.py",),
    "setuptools": ("setuptools/__init__.py", "pkg_resources/__init__.py"),
}


def replace_refs(value, replacements):
    if isinstance(value, dict):
        return {key: replace_refs(item, replacements) for key, item in value.items()}
    if isinstance(value, list):
        return [replace_refs(item, replacements) for item in value]
    return replacements.get(value, value) if isinstance(value, str) else value


def synchronize(vendor_dir, expected, *, check=False):
    vendor_dir = Path(vendor_dir)
    installed = list(distributions(path=[str(vendor_dir)]))
    for name, version in expected.items():
        matches = [
            dist for dist in installed if dist.metadata.get("Name", "").lower() == name
        ]
        if len(matches) != 1 or matches[0].version != version:
            raise ValueError(f"{vendor_dir}: expected one installed {name}=={version}")
        for payload in PAYLOADS[name]:
            if not (vendor_dir / payload).is_file():
                raise ValueError(f"{vendor_dir}: missing package payload {payload}")

    manifest_path = vendor_dir / "vendor.txt"
    manifest = manifest_path.read_text()
    lines = manifest.splitlines(keepends=True)
    for name, version in expected.items():
        matches = [
            index for index, line in enumerate(lines) if line.startswith(f"{name}==")
        ]
        if len(matches) != 1:
            raise ValueError(f"{manifest_path}: expected one {name} entry")
        lines[matches[0]] = f"{name}=={version}\n"
    updated_manifest = "".join(lines)

    bom_path = vendor_dir / "bom.cdx.json"
    has_bom = bom_path.exists()
    bom = json.loads(bom_path.read_text()) if has_bom else None
    updated_bom = copy.deepcopy(bom)
    if has_bom:
        if not isinstance(bom, dict) or bom.get("bomFormat") != "CycloneDX":
            raise ValueError(f"{bom_path}: expected CycloneDX format")
        if type(bom.get("version", 1)) is not int or bom.get("version", 1) < 1:
            raise ValueError(f"{bom_path}: expected a positive integer BOM version")
        replacements = {}
        for name, version in expected.items():
            matches = [
                item for item in updated_bom["components"] if item.get("name") == name
            ]
            if len(matches) != 1:
                raise ValueError(f"{bom_path}: expected one {name} component")
            component = matches[0]
            reference = f"pkg:pypi/{name}@{version}"
            replacements[component["bom-ref"]] = reference
            replacements[component["purl"]] = reference
            if component["version"] != version:
                component.pop("hashes", None)
                component.pop("externalReferences", None)
            component.update({"version": version, "purl": reference, "bom-ref": reference})
        updated_bom = replace_refs(updated_bom, replacements)
        references = {item.get("bom-ref") for item in updated_bom["components"]}
        references.add(
            updated_bom.get("metadata", {}).get("component", {}).get("bom-ref")
        )
        for dependency in updated_bom.get("dependencies", []):
            for reference in [dependency["ref"], *dependency.get("dependsOn", [])]:
                if reference not in references:
                    raise ValueError(
                        f"{bom_path}: unknown dependency reference {reference}"
                    )

    if check:
        if manifest != updated_manifest or bom != updated_bom:
            raise ValueError(f"{vendor_dir}: inventories do not match installed packages")
        return

    # Validate both inventories before writing either one.
    if manifest != updated_manifest:
        manifest_path.write_text(updated_manifest)
    if bom != updated_bom:
        updated_bom["version"] = bom.get("version", 1) + 1
        bom_path.write_text(json.dumps(updated_bom, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("vendor_dir", type=Path)
    parser.add_argument("--msgpack-version", required=True)
    parser.add_argument("--setuptools-version", required=True)
    parser.add_argument("--check", action="store_true", help="Check without changing files")
    args = parser.parse_args()
    try:
        synchronize(
            args.vendor_dir,
            {"msgpack": args.msgpack_version, "setuptools": args.setuptools_version},
            check=args.check,
        )
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"{error}\n")
    print(f"{args.vendor_dir}: vendor inventories match the installed packages")


if __name__ == "__main__":
    main()
