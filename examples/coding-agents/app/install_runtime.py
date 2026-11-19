# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Install only checksum-pinned official native binaries, never run installers."""

import hashlib
import json
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import time
import urllib.error
from urllib.parse import urlsplit
import urllib.request


def install(name, destination, cache=None):
    artifact = json.loads(Path(__file__).with_name("artifacts.json").read_text())[name]
    with tempfile.TemporaryDirectory() as directory:
        download = Path(directory) / "download"
        cached = Path(cache) / Path(urlsplit(artifact["url"]).path).name if cache else None
        attempts = 1 if cached and cached.is_file() else 4
        for attempt in range(attempts):
            digest = hashlib.sha256()
            try:
                if cached and cached.is_file():
                    source = cached.open("rb")
                else:
                    request = urllib.request.Request(
                        artifact["url"], headers={"User-Agent": "cetirun-runtime-build"}
                    )
                    source = urllib.request.urlopen(request, timeout=120)
                with source, download.open("wb") as output:
                    while chunk := source.read(1048576):
                        digest.update(chunk)
                        output.write(chunk)
                break
            except (urllib.error.URLError, TimeoutError):
                if attempt == attempts - 1:
                    raise
                time.sleep(5 * (attempt + 1))
        if digest.hexdigest() != artifact["sha256"]:
            raise RuntimeError("Official artifact checksum mismatch")
        destination = Path(destination)
        destination.mkdir(parents=True, exist_ok=True)
        if artifact["archive"]:
            with tarfile.open(download) as archive:
                members = [
                    member for member in archive.getmembers()
                    if Path(member.name).name == artifact["member"] and member.isfile()
                ]
                if len(members) != 1:
                    raise RuntimeError("Unexpected official archive layout")
                with archive.extractfile(members[0]) as source, (destination / name).open("wb") as output:
                    shutil.copyfileobj(source, output)
        else:
            shutil.copyfile(download, destination / name)
        (destination / name).chmod(0o755)
    print(f"Installed {name} {artifact['version']} with verified SHA-256")


if __name__ == "__main__":
    install(sys.argv[1], sys.argv[2], sys.argv[3] if len(sys.argv) > 3 else None)
