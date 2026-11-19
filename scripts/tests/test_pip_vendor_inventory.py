# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import copy
import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

SCRIPT = (
    Path(__file__).resolve().parents[2]
    / "app/cetirun-chart/configs/sync-pip-vendor.py"
)
SPEC = importlib.util.spec_from_file_location("sync_pip_vendor", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)

VERSIONS = {"msgpack": "1.2.1", "setuptools": "80.10.2"}
OLD_VERSIONS = {"msgpack": "1.1.2", "setuptools": "70.3.0"}
OTHER = {
    "name": "requests",
    "version": "2.34.2",
    "type": "library",
    "bom-ref": "pkg:pypi/requests@2.34.2",
    "purl": "pkg:pypi/requests@2.34.2",
    "properties": [{"name": "keep", "value": "unchanged"}],
}


class PipVendorInventoryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.system = self.make_vendor(self.root / "system/pip/_vendor", bom=False)
        self.azure = self.make_vendor(self.root / "azure/pip/_vendor", bom=True)

    def make_vendor(self, root, *, bom):
        root.mkdir(parents=True)
        for name, version in VERSIONS.items():
            metadata = root / f"{name}-{version}.dist-info"
            metadata.mkdir()
            (metadata / "METADATA").write_text(
                f"Metadata-Version: 2.1\nName: {name}\nVersion: {version}\n"
            )
            for relative in MODULE.PAYLOADS[name]:
                payload = root / relative
                payload.parent.mkdir(parents=True, exist_ok=True)
                payload.write_text("# Test fixture\n")
        (root / "vendor.txt").write_text(
            "msgpack==1.1.2\nsetuptools==70.3.0\nrequests==2.34.2\n"
        )
        if bom:
            components = [
                {
                    "name": name,
                    "version": version,
                    "type": "library",
                    "purl": f"pkg:pypi/{name}@{version}",
                    "bom-ref": f"pkg:pypi/{name}@{version}",
                }
                for name, version in OLD_VERSIONS.items()
            ] + [copy.deepcopy(OTHER)]
            document = {
                "bomFormat": "CycloneDX",
                "specVersion": "1.4",
                "version": 1,
                "metadata": {"component": {"bom-ref": "pip", "name": "pip"}},
                "components": components,
                "dependencies": [
                    {"ref": "pip", "dependsOn": [item["bom-ref"] for item in components]}
                ] + [{"ref": item["bom-ref"]} for item in components],
            }
            (root / "bom.cdx.json").write_text(json.dumps(document))
        return root

    def test_both_roots_and_all_sbom_references_are_updated(self):
        for root in (self.system, self.azure):
            MODULE.synchronize(root, VERSIONS)
            MODULE.synchronize(root, VERSIONS, check=True)
            self.assertEqual(
                (root / "vendor.txt").read_text(),
                "msgpack==1.2.1\nsetuptools==80.10.2\nrequests==2.34.2\n",
            )
        self.assertFalse((self.system / "bom.cdx.json").exists())
        bom = json.loads((self.azure / "bom.cdx.json").read_text())
        self.assertEqual(bom["components"][-1], OTHER)
        self.assertEqual(bom["metadata"]["component"]["bom-ref"], "pip")
        self.assertEqual(bom["version"], 2)
        for name, version in VERSIONS.items():
            reference = f"pkg:pypi/{name}@{version}"
            self.assertIn(reference, bom["dependencies"][0]["dependsOn"])
            self.assertIn({"ref": reference}, bom["dependencies"])
            self.assertNotIn(f"pkg:pypi/{name}@{OLD_VERSIONS[name]}", json.dumps(bom))
            component = next(item for item in bom["components"] if item["name"] == name)
            self.assertEqual(component["version"], version)
            self.assertEqual(component["purl"], reference)
            self.assertEqual(component["bom-ref"], reference)

    def test_check_is_read_only_and_detects_stale_sbom(self):
        (self.azure / "vendor.txt").write_text(
            "msgpack==1.2.1\nsetuptools==80.10.2\nrequests==2.34.2\n"
        )
        before = self.snapshot()
        with self.assertRaises(ValueError):
            MODULE.synchronize(self.azure, VERSIONS, check=True)
        self.assertEqual(self.snapshot(), before)

    def test_update_is_idempotent(self):
        MODULE.synchronize(self.azure, VERSIONS)
        before = self.snapshot()
        MODULE.synchronize(self.azure, VERSIONS)
        self.assertEqual(self.snapshot(), before)

    def test_check_rejects_stale_legacy_inventory_without_writing(self):
        before = self.snapshot()
        with self.assertRaises(ValueError):
            MODULE.synchronize(self.system, VERSIONS, check=True)
        self.assertEqual(self.snapshot(), before)

    def test_malformed_bom_or_dangling_reference_rejects_update(self):
        for malformed in ("null", "dangling", "version"):
            with self.subTest(malformed=malformed):
                root = self.make_vendor(self.root / malformed, bom=True)
                path = root / "bom.cdx.json"
                if malformed == "null":
                    path.write_text("null")
                else:
                    bom = json.loads(path.read_text())
                    if malformed == "dangling":
                        bom["dependencies"][0]["dependsOn"].append("missing-component")
                    else:
                        bom["version"] = "invalid"
                    path.write_text(json.dumps(bom))
                before = self.snapshot()
                with self.assertRaises(ValueError):
                    MODULE.synchronize(root, VERSIONS)
                self.assertEqual(self.snapshot(), before)

    def test_missing_duplicate_or_wrong_distribution_rejects_update(self):
        for metadata_change in ("missing", "duplicate", "wrong"):
            with self.subTest(metadata_change=metadata_change):
                root = self.make_vendor(self.root / metadata_change, bom=True)
                metadata = root / "msgpack-1.2.1.dist-info/METADATA"
                if metadata_change == "missing":
                    metadata.unlink()
                elif metadata_change == "wrong":
                    metadata.write_text("Name: msgpack\nVersion: 1.1.2\n")
                else:
                    duplicate = root / "msgpack-1.1.2.dist-info"
                    duplicate.mkdir()
                    (duplicate / "METADATA").write_text("Name: msgpack\nVersion: 1.1.2\n")
                before = self.snapshot()
                with self.assertRaises(ValueError):
                    MODULE.synchronize(root, VERSIONS)
                self.assertEqual(self.snapshot(), before)

    def test_missing_payload_or_inventory_entry_rejects_update(self):
        for missing in ("payload", "vendor-entry", "bom-component"):
            with self.subTest(missing=missing):
                root = self.make_vendor(self.root / missing, bom=True)
                if missing == "payload":
                    (root / "pkg_resources/__init__.py").unlink()
                elif missing == "vendor-entry":
                    (root / "vendor.txt").write_text("setuptools==70.3.0\n")
                else:
                    path = root / "bom.cdx.json"
                    bom = json.loads(path.read_text())
                    bom["components"] = [OTHER]
                    path.write_text(json.dumps(bom))
                before = self.snapshot()
                with self.assertRaises(ValueError):
                    MODULE.synchronize(root, VERSIONS)
                self.assertEqual(self.snapshot(), before)

    def snapshot(self):
        return {
            str(path.relative_to(self.root)): path.read_bytes()
            for path in self.root.rglob("*")
            if path.is_file()
        }


if __name__ == "__main__":
    unittest.main()
