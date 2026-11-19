#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Move Docker storage off the OS disk on a disposable Linux CI agent only."""

import argparse
from contextlib import contextmanager
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


GIB = 1024 ** 3
DOCKER = ["docker", "--host", "unix:///var/run/docker.sock"]
UNITS = ["docker.socket", "docker.service"]


def run(args, *, capture=False):
    print("+", " ".join(map(str, args)), flush=True)
    return subprocess.run(
        list(map(str, args)), check=True, text=True,
        stdout=subprocess.PIPE if capture else None,
    ).stdout


def same_filesystem(left, right):
    return left.stat().st_dev == right.stat().st_dev


def require_free_space(directory, minimum_free_gib):
    free = shutil.disk_usage(directory).free
    if free < minimum_free_gib * GIB:
        raise ValueError(
            f"Storage destination {directory} has {free / GIB:.1f} GiB free; "
            f"at least {minimum_free_gib} GiB is required"
        )


def validate_write_destination(directory, minimum_free_gib):
    if same_filesystem(directory, Path("/")):
        raise ValueError("Docker data root or build scratch is on the OS filesystem")
    require_free_space(directory, minimum_free_gib)


def docker_info():
    info = json.loads(run([*DOCKER, "info", "--format", "{{json .}}"], capture=True))
    if not isinstance(info, dict) or not isinstance(info.get("DockerRootDir"), str):
        raise ValueError("Docker did not report its local data root")
    return info


@contextmanager
def staged_config(path, content, mode):
    descriptor, name = tempfile.mkstemp(prefix="cetirun-docker-", suffix=".json", dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(content)
        temporary.chmod(mode)
        yield temporary
    finally:
        temporary.unlink(missing_ok=True)


def prepare(storage_mount, minimum_free_gib=64, *, config_path=Path("/etc/docker/daemon.json")):
    if minimum_free_gib <= 0:
        raise ValueError("minimum-free-gib must be positive")
    if not storage_mount.is_absolute() or not storage_mount.is_dir():
        raise ValueError("storage-mount must be an existing absolute directory")
    storage_mount = storage_mount.resolve()
    run(["df", "-hT", "/", storage_mount])
    run(["df", "-ih", "/", storage_mount])
    if same_filesystem(storage_mount, Path("/")):
        raise ValueError("Storage mount is on the OS filesystem; configure a separate temporary or data disk")
    require_free_space(storage_mount, minimum_free_gib)

    data_root = storage_mount / "cetirun-docker"
    scratch = storage_mount / "cetirun-build-tmp"
    for directory in (data_root, scratch):
        if directory.is_symlink() or (directory.exists() and not directory.is_dir()):
            raise ValueError(f"Refusing to use non-directory or symlink: {directory}")
        if directory.exists():
            validate_write_destination(directory, minimum_free_gib)

    if config_path.is_symlink() or (config_path.exists() and not config_path.is_file()):
        raise ValueError("Docker daemon configuration must be a regular file, not a symlink")
    original = config_path.read_bytes() if config_path.exists() else None
    mode = config_path.stat().st_mode & 0o777 if original is not None else 0o644
    config = json.loads(original) if original is not None else {}
    if not isinstance(config, dict) or not isinstance(config.get("features", {}), dict):
        raise ValueError("Docker daemon configuration and its features must be JSON objects")

    info = docker_info()
    if Path(info["DockerRootDir"]).resolve() == data_root and info.get("Driver") == "overlay2":
        scratch.mkdir(exist_ok=True)
        for directory in (data_root, scratch):
            validate_write_destination(directory, minimum_free_gib)
        scratch.chmod(0o1777)
        print(f"Docker already uses {data_root}; no daemon restart needed.")
        return data_root
    if run([*DOCKER, "ps", "--quiet"], capture=True).strip():
        raise ValueError("Refusing to reconfigure Docker while containers are running")

    config["data-root"] = str(data_root)
    config["storage-driver"] = "overlay2"
    # Docker's containerd image store does not follow Docker's data-root. Use the
    # classic image store so layers AND BuildKit cache stay on the selected disk.
    config.setdefault("features", {})["containerd-snapshotter"] = False

    data_root.mkdir(exist_ok=True)
    scratch.mkdir(exist_ok=True)
    for directory in (data_root, scratch):
        validate_write_destination(directory, minimum_free_gib)
    scratch.chmod(0o1777)
    config_path.parent.mkdir(parents=True, exist_ok=True)
    content = (json.dumps(config, indent=2) + "\n").encode()
    with staged_config(config_path, content, mode) as staged:
        run(["dockerd", "--validate", "--config-file", staged])
        try:
            # Stop socket activation too: it must not revive the old daemon while
            # its configuration is being replaced. Existing cache is not deleted.
            run(["systemctl", "stop", *UNITS])
            staged.replace(config_path)
            run(["systemctl", "start", *UNITS])
            info = docker_info()
            if Path(info["DockerRootDir"]).resolve() != data_root or info.get("Driver") != "overlay2":
                raise ValueError("Docker did not activate the requested data root and overlay2 image store")
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            try:
                if original is None:
                    config_path.unlink(missing_ok=True)
                else:
                    with staged_config(config_path, original, mode) as restored:
                        restored.replace(config_path)
                run(["systemctl", "restart", *UNITS])
            except (OSError, subprocess.SubprocessError) as rollback_error:
                raise RuntimeError(f"Docker storage setup failed ({error}); rollback also failed ({rollback_error})") from error
            raise RuntimeError(f"Docker storage setup failed ({error}); restored the original daemon configuration") from error

    run(["df", "-hT", "/", data_root, scratch])
    print(f"Docker layers and BuildKit cache now use {data_root}.")
    print(f"Use {scratch} for TMPDIR in subsequent build steps.")
    return data_root


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--storage-mount", type=Path, default=Path("/mnt"))
    parser.add_argument("--minimum-free-gib", type=int, default=64)
    args = parser.parse_args(argv)
    try:
        if sys.platform != "linux" or os.geteuid() != 0:
            raise ValueError("This helper requires root on a disposable Linux CI agent; use sudo -n")
        prepare(args.storage_mount, args.minimum_free_gib)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
