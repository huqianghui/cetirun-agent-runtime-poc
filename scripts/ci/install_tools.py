#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Download checksum-pinned Linux CI tools to an explicit, task-local directory.

This is a networked provisioning command, never invoked by the offline Make targets.
Tool versions and release provenance live in tools.json; no shared installs change.
Bundles use a "members" map of output basenames to exact archive member names.
All pins and outputs are checked before download, all payloads before writing.
"""

import argparse
from contextlib import ExitStack
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import re
import sys
import tarfile
import time
from urllib.error import HTTPError, URLError
from urllib.request import urlopen


MANIFEST = Path(__file__).with_name("tools.json")
# The pinned envtest + Helm binaries total about 233 MiB; leave bounded headroom.
MAX_DOWNLOAD_BYTES = 128 * 1024 * 1024
MAX_MEMBER_BYTES = 128 * 1024 * 1024
MAX_TOTAL_BYTES = 384 * 1024 * 1024
DOWNLOAD_RETRY_DELAYS = (5, 15, 30)


def check_size(size, limit, description):
    if size < 0:
        raise ValueError(f"Invalid {description} size: {size}")
    if size > limit:
        raise ValueError(f"{description} exceeds {limit}-byte size limit")


def download_bytes(response):
    length = response.headers.get("Content-Length")
    if length is not None:
        try:
            length = int(length)
        except ValueError as error:
            raise ValueError("Invalid tool download Content-Length") from error
        check_size(length, MAX_DOWNLOAD_BYTES, "Tool download")
    # Never trust a missing or understated Content-Length to bound this read.
    data = response.read(MAX_DOWNLOAD_BYTES + 1)
    check_size(len(data), MAX_DOWNLOAD_BYTES, "Tool download")
    return data


def download(url):
    for attempt in range(len(DOWNLOAD_RETRY_DELAYS) + 1):
        try:
            with urlopen(url, timeout=45) as response:
                return download_bytes(response)
        except (URLError, TimeoutError, ConnectionError) as error:
            if isinstance(error, HTTPError):
                error.close()
                retryable = error.code in (408, 429, 500, 502, 503, 504)
                description = f"HTTP {error.code}"
            else:
                reason = error.reason if isinstance(error, URLError) else error
                retryable = isinstance(reason, (TimeoutError, ConnectionError))
                description = type(reason).__name__
            if not retryable or attempt == len(DOWNLOAD_RETRY_DELAYS):
                raise
            delay = DOWNLOAD_RETRY_DELAYS[attempt]
            print(
                f"Download failed ({description}); retrying in {delay}s "
                f"({attempt + 1}/{len(DOWNLOAD_RETRY_DELAYS)})",
                file=sys.stderr, flush=True,
            )
            time.sleep(delay)


def archive_member(member):
    if (
        not isinstance(member, str)
        or "\\" in member
        or any(ord(char) < 32 for char in member)
        or any(part in ("", ".", "..") for part in member.split("/"))
    ):
        raise ValueError(f"Invalid tool archive member: {member!r}")
    return member


def binaries_bytes(data, checksum, members, *, max_total_bytes=None):
    check_size(len(data), MAX_DOWNLOAD_BYTES, "Tool download")
    if hashlib.sha256(data).hexdigest() != checksum:
        raise ValueError("Tool download SHA256 mismatch")
    if max_total_bytes is None:
        max_total_bytes = MAX_TOTAL_BYTES
    max_total_bytes = min(max_total_bytes, MAX_TOTAL_BYTES)
    binaries = {}
    total_bytes = 0
    requested = {archive_member(member) for member in members if member is not None}
    if None in members:
        check_size(len(data), MAX_MEMBER_BYTES, "Raw tool")
        total_bytes = len(data)
        check_size(total_bytes, max_total_bytes, "Selected tool payload total")
        binaries[None] = data
    if not requested:
        return binaries
    # Read only explicitly named regular files, never extract archive paths.
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
        entries = {}
        for entry in archive:
            if entry.name not in requested:
                continue
            if not entry.isfile():
                raise ValueError(
                    f"Tool archive member is not a regular file: {entry.name}"
                )
            if entry.name in entries:
                raise ValueError(f"Duplicate tool archive member: {entry.name}")
            check_size(entry.size, MAX_MEMBER_BYTES, f"Tool archive member {entry.name}")
            total_bytes += entry.size
            check_size(total_bytes, max_total_bytes, "Selected tool payload total")
            entries[entry.name] = entry
        for member in requested:
            if member not in entries:
                raise ValueError(f"Missing tool archive member: {member}")
        # Validate every selected size before allocating any member payload.
        for member, entry in entries.items():
            with archive.extractfile(entry) as source:
                payload = source.read(entry.size)
            if len(payload) != entry.size:
                raise ValueError(f"Incomplete tool archive member: {member}")
            binaries[member] = payload
    return binaries


def binary_bytes(data, checksum, member=None):
    return binaries_bytes(data, checksum, [member])[member]


def check_destination(destination):
    for directory in (destination, *destination.parents):
        if directory.is_symlink():
            raise ValueError("--bin-dir must not contain symlinks")
        if directory.exists() and not directory.is_dir():
            raise ValueError("--bin-dir must be a directory")


def output_name(name):
    if not isinstance(name, str) or not re.fullmatch("[A-Za-z0-9][A-Za-z0-9._-]*", name):
        raise ValueError(f"Invalid tool output name: {name!r}")
    return name


def check_output(output):
    if output.exists() or output.is_symlink():
        raise ValueError(f"Refusing to overwrite existing tool: {output}")


def plan(manifest, names, destination, system, machine, *, arch=None):
    if arch is None:
        arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(machine)
    if system != "Linux" or arch not in ("amd64", "arm64"):
        raise ValueError("Tool provisioning supports Linux amd64/arm64 only")
    check_destination(destination)
    if len(names) != len(set(names)):
        raise ValueError("Request each tool only once")
    entries = []
    outputs = set()
    for name in names:
        output_name(name)
        if name not in manifest:
            raise ValueError(f"Unknown tool: {name}")
        spec = manifest[name]
        if "members" in spec:
            members = spec["members"]
            if "member" in spec or not isinstance(members, dict) or not members:
                raise ValueError(f"Invalid tool bundle members: {name}")
        else:
            members = {name: spec.get("member")}
        planned = []
        for filename, member in members.items():
            output = destination / output_name(filename)
            check_output(output)
            if output in outputs:
                raise ValueError(f"Duplicate tool output: {output}")
            outputs.add(output)
            if "members" in spec and member is None:
                raise ValueError(f"Invalid tool archive member: {member!r}")
            planned.append((output, member))
        asset_arch = spec.get("architectures", {}).get(arch, arch)
        url = spec["url"].format(arch=asset_arch)
        checksum = spec["sha256"][arch]
        if (
            not url.startswith("https://")
            or not isinstance(checksum, str)
            or not re.fullmatch("[0-9a-f]{64}", checksum)
        ):
            raise ValueError(f"Invalid download pin: {name}")
        for output, member in planned:
            if member is not None:
                member = archive_member(member).format(arch=asset_arch)
                archive_member(member)
            entries.append((output, url, checksum, member))
    return entries


def write_binaries(destination, binaries):
    check_destination(destination)
    for output in binaries:
        check_output(output)
    destination.mkdir(parents=True, exist_ok=True)
    created = []
    try:
        with ExitStack() as stack:
            targets = []
            # Reserve every output exclusively before writing the first binary.
            for output, data in binaries.items():
                target = stack.enter_context(output.open("xb"))
                created.append((output, os.fstat(target.fileno())))
                targets.append((target, data))
            for target, data in targets:
                target.write(data)
                target.flush()
                os.fchmod(target.fileno(), 0o755)
    except BaseException:
        for output, owned in created:
            try:
                current = output.lstat()
                if (current.st_dev, current.st_ino) == (owned.st_dev, owned.st_ino):
                    output.unlink()
            except FileNotFoundError:
                pass
        raise


def main(argv=None):
    manifest = json.loads(MANIFEST.read_text())
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--bin-dir",
        required=True,
        type=Path,
        help="new/empty task-local binary directory (never a global bin directory)",
    )
    parser.add_argument(
        "--arch",
        choices=("amd64", "arm64"),
        help="target Linux architecture (defaults to the host architecture)",
    )
    parser.add_argument("tools", nargs="+", choices=manifest)
    args = parser.parse_args(argv)
    try:
        destination = args.bin_dir.absolute()
        entries = plan(
            manifest, args.tools, destination, platform.system(), platform.machine(),
            arch=args.arch,
        )
        downloads = {}
        for output, url, checksum, member in entries:
            downloads.setdefault((url, checksum), []).append((output, member))
        binaries = {}
        remaining_bytes = MAX_TOTAL_BYTES
        for (url, checksum), outputs in downloads.items():
            names = ", ".join(output.name for output, _ in outputs)
            print(f"Installing {names} from {url}", flush=True)
            data = binaries_bytes(
                download(url), checksum,
                [member for _, member in outputs],
                max_total_bytes=remaining_bytes,
            )
            remaining_bytes -= sum(len(payload) for payload in data.values())
            for output, member in outputs:
                binaries[output] = data[member]
        write_binaries(destination, binaries)
    except (ValueError, OSError, KeyError, tarfile.TarError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
