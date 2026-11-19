#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Copy a Runtime Chart and lock its built-in runtime image digests.

The source Chart is never modified. The resulting private copy retains the
Chart identity and receives only paired OpenClaw/Hermes digest pins plus exact
profile allow-list entries for the supplied registry prefix.
"""

import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import sys


DIGEST = re.compile(r"sha256:[a-f0-9]{64}\Z")
TAG = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\Z")
REGISTRY_COMPONENT = re.compile(r"[a-z0-9]+(?:[._-][a-z0-9]+)*\Z")
IMAGE_COMPONENT = re.compile(r"[a-z0-9]+(?:[._-][a-z0-9]+)*\Z")


def fail(message: str) -> None:
    raise ValueError(message)


def valid_registry(value: str) -> bool:
    if not value or len(value) > 1024 or any(character.isspace() for character in value):
        return False
    if value.startswith(("/", ".")) or value.endswith("/") or any(token in value for token in ("://", "@")):
        return False
    parts = value.split("/")
    host = parts[0]
    hostname, separator, port = host.rpartition(":")
    if separator:
        if not hostname or not port.isdecimal() or not 1 <= int(port) <= 65535:
            return False
    else:
        hostname = host
    if not hostname or any(not REGISTRY_COMPONENT.fullmatch(component) for component in hostname.split(".")):
        return False
    return all(IMAGE_COMPONENT.fullmatch(component) for component in parts[1:])


def valid_image(value: str) -> bool:
    if not value or len(value) > 2048 or any(character.isspace() for character in value):
        return False
    if value.startswith("/") or "://" in value:
        return False
    image, marker, digest = value.partition("@")
    if marker and not DIGEST.fullmatch(digest):
        return False
    if image.count("@"):
        return False
    last_slash = image.rfind("/")
    last_colon = image.rfind(":")
    if last_colon > last_slash:
        repository, tag = image[:last_colon], image[last_colon + 1:]
        if not TAG.fullmatch(tag):
            return False
    else:
        repository = image
    parts = repository.split("/")
    return len(parts) >= 2 and all(IMAGE_COMPONENT.fullmatch(component) for component in parts[1:]) and valid_registry("/".join(parts[:-1]))


def parse_qualified_images(path: Path) -> tuple[list[str], list[str], int, int]:
    lines = path.read_text().splitlines(keepends=True)
    start = next((index for index, line in enumerate(lines)
                  if re.fullmatch(r"qualifiedImages:\s*(?:\[\])?\s*(?:#.*)?\n?", line)), None)
    if start is None:
        fail(f"{path}: missing top-level qualifiedImages")
    if "[]" in lines[start]:
        return lines, [], start, start + 1
    end = start + 1
    images: list[str] = []
    while end < len(lines):
        line = lines[end]
        if line.lstrip().startswith("-"):
            matched = re.fullmatch(r"\s*-\s*([^#\s][^#]*?)\s*(?:#.*)?\n?", line)
            if matched is None:
                fail(f"{path}: invalid qualifiedImages entry")
            value = matched.group(1).strip()
            if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
                value = value[1:-1]
            images.append(value)
        elif line.strip() and not line.lstrip().startswith("#"):
            break
        end += 1
    if len(images) != len(set(images)) or any(not valid_image(value) for value in images):
        fail(f"{path}: qualifiedImages must contain distinct valid image references")
    return lines, images, start, end


def write_profile(path: Path, image: str) -> None:
    lines, images, start, end = parse_qualified_images(path)
    if image not in images:
        images.append(image)
    replacement = ["qualifiedImages:\n", *(f"- {value}\n" for value in images)]
    path.write_text("".join([*lines[:start], *replacement, *lines[end:]]))


def block_end(lines: list[str], start: int, indentation: int) -> int:
    for index in range(start + 1, len(lines)):
        line = lines[index]
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if len(line) - len(line.lstrip(" ")) <= indentation:
            return index
    return len(lines)


def digest_location(path: Path, runtime: str) -> tuple[list[str], int, str]:
    lines = path.read_text().splitlines(keepends=True)
    runtime_start = next((index for index, line in enumerate(lines)
                          if re.fullmatch(rf"{runtime}:\s*(?:#.*)?\n?", line)), None)
    if runtime_start is None:
        fail(f"{path}: missing {runtime} values")
    runtime_end = block_end(lines, runtime_start, 0)
    image_start = next((index for index in range(runtime_start + 1, runtime_end)
                        if re.fullmatch(r"  image:\s*(?:#.*)?\n?", lines[index])), None)
    if image_start is None:
        fail(f"{path}: missing {runtime}.image values")
    image_end = block_end(lines, image_start, 2)
    digest_line = next((index for index in range(image_start + 1, image_end)
                        if re.fullmatch(r"    digest:\s*.*\n?", lines[index])), None)
    if digest_line is None:
        fail(f"{path}: missing {runtime}.image.digest")
    existing = lines[digest_line].split(":", 1)[1].strip()
    return lines, digest_line, existing


def lock_digest(path: Path, runtime: str, digest: str) -> None:
    lines, digest_line, existing = digest_location(path, runtime)
    if existing not in ('""', "''", digest):
        fail(f"{path}: refusing to replace existing {runtime}.image.digest")
    lines[digest_line] = f"    digest: {digest}\n"
    path.write_text("".join(lines))


def lock_chart(source: Path, output: Path, registry: str, version: str,
               openclaw_digest: str, hermes_digest: str) -> dict[str, str]:
    if not source.is_dir() or not (source / "Chart.yaml").is_file():
        fail(f"{source}: expected a Helm chart directory")
    if output.exists():
        fail(f"{output}: output chart path must not already exist")
    source_path, output_path = source.resolve(), output.resolve()
    if source_path == output_path or source_path in output_path.parents:
        fail("output chart path must be outside the source chart")
    if not valid_registry(registry):
        fail("registry must be a lowercase OCI registry hostname with an optional repository prefix")
    if not TAG.fullmatch(version) or version == "latest":
        fail("version must be a non-latest OCI image tag")
    for name, digest in (("openclaw", openclaw_digest), ("hermes", hermes_digest)):
        if not DIGEST.fullmatch(digest):
            fail(f"{name} image digest must be sha256 followed by 64 lowercase hexadecimal characters")

    # Validate all source assets before copying, so a failed invocation cannot
    # leave a partially changed chart presented as a completed artifact.
    source_values = source / "values.yaml"
    digests = {"openclaw": openclaw_digest, "hermes": hermes_digest}
    for runtime, digest in digests.items():
        _, _, existing = digest_location(source_values, runtime)
        if existing not in ('""', "''", digest):
            fail(f"{source_values}: refusing to replace existing {runtime}.image.digest")
        parse_qualified_images(source / "agents" / runtime / "profile.yaml")

    # Keep the same source boundary as ordinary release packaging. Stale
    # dependency charts and Python caches are not inputs to a runtime lock.
    shutil.copytree(source, output, ignore=shutil.ignore_patterns("charts", "__pycache__"))
    for runtime, digest in digests.items():
        lock_digest(output / "values.yaml", runtime, digest)
        write_profile(output / "agents" / runtime / "profile.yaml",
                      f"{registry}/{runtime}-gateway@{digest}")
    lock_inputs = "\n".join((registry, version, openclaw_digest, hermes_digest)).encode()
    return {
        "hermesImage": f"{registry}/hermes-gateway@{hermes_digest}",
        "lockHash": hashlib.sha256(lock_inputs).hexdigest(),
        "openclawImage": f"{registry}/openclaw-gateway@{openclaw_digest}",
        "version": version,
    }


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-chart", required=True, type=Path)
    parser.add_argument("--output-chart", required=True, type=Path)
    parser.add_argument("--registry", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--openclaw-image-digest", required=True)
    parser.add_argument("--hermes-image-digest", required=True)
    args = parser.parse_args(argv)
    try:
        result = lock_chart(args.source_chart, args.output_chart, args.registry, args.version,
                            args.openclaw_image_digest, args.hermes_image_digest)
    except ValueError as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 2
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
