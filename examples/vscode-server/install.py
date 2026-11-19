#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Download and verify the pinned official VS Code CLI and web server."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import tarfile
import tempfile
import urllib.request


def install(arch, output, lock):
    manifest = json.loads(lock.read_text())
    if not re.fullmatch(r"[0-9a-f]{40}", manifest["commit"]):
        raise ValueError("Invalid VS Code commit")
    artifacts = manifest["artifacts"][arch]
    for kind in ("cli", "server"):
        artifact = artifacts[kind]
        if not re.fullmatch(r"[a-zA-Z0-9_.-]+\.tar\.gz", artifact["file"]) or not re.fullmatch(r"[0-9a-f]{64}", artifact["sha256"]):
            raise ValueError("Invalid artifact pin")
    if output.exists() and any(output.iterdir()):
        raise ValueError("Use an empty output directory; existing files are not replaced")
    output.mkdir(parents=True, exist_ok=True)
    for kind in ("cli", "server"):
        artifact = artifacts[kind]
        url = f'https://vscode.download.prss.microsoft.com/dbazure/download/stable/{manifest["commit"]}/{artifact["file"]}'
        with tempfile.TemporaryDirectory(prefix="cetirun-vscode-download-") as temporary:
            archive = Path(temporary) / "download.tar.gz"
            digest = hashlib.sha256()
            with urllib.request.urlopen(url, timeout=60) as response, archive.open("wb") as target:
                while chunk := response.read(1024 * 1024):
                    target.write(chunk)
                    digest.update(chunk)
            if digest.hexdigest() != artifact["sha256"]:
                raise ValueError(f"Checksum mismatch for {kind}")
            unpacked = Path(temporary) / "unpacked"
            with tarfile.open(archive) as bundle:
                bundle.extractall(unpacked, filter="data")
            entries = list(unpacked.iterdir())
            if kind == "server":
                if len(entries) != 1 or not (entries[0] / "bin/code-server").is_file():
                    raise ValueError("Unexpected official server archive layout")
                entries[0].rename(output / "server")
            else:
                if len(entries) != 1 or entries[0].name != "code" or not entries[0].is_file():
                    raise ValueError("Unexpected official CLI archive layout")
                entries[0].rename(output / "code")
    (output / "commit").write_text(manifest["commit"] + "\n")
    remote_extensions = {}
    for package in (output / "server/extensions").glob("*/package.json"):
        metadata = json.loads(package.read_text())
        browser = metadata.get("browser")
        if browser and metadata.get("main"):
            entry = package.parent / browser
            if not entry.is_file() and not Path(str(entry) + ".js").is_file():
                remote_extensions[f'{metadata["publisher"]}.{metadata["name"]}'] = ["workspace", "-web"]
    (output / "browser-defaults.json").write_text(json.dumps({"remote.extensionKind": remote_extensions}, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--lock", type=Path, default=Path(__file__).with_name("vscode-lock.json"))
    args = parser.parse_args()
    install(args.arch, args.output, args.lock)


if __name__ == "__main__":
    main()
