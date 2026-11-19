#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Run go test with optional deterministic sharding of all top-level tests."""

import argparse
import hashlib
import json
import re
import subprocess
import sys


def discover_tests(output):
    names = set()
    for line in output.splitlines():
        event = json.loads(line)
        if not isinstance(event, dict):
            raise ValueError("Invalid go test discovery event")
        if event.get("Action") != "output":
            continue
        for name in event.get("Output", "").splitlines():
            if re.fullmatch(r"(?:Test|Example|Fuzz)\w*", name):
                names.add(name)
    if not names:
        raise ValueError("go test discovery returned no runnable tests")
    return sorted(names)


def select_tests(names, index, count):
    # Hash only the name so identical names in different packages stay together.
    return [
        name for name in names
        if int.from_bytes(hashlib.sha256(name.encode()).digest()[:8], "big") % count == index - 1
    ]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--shard-index", type=int, default=1, help="one-based shard index")
    parser.add_argument("--shard-count", type=int, default=1)
    parser.add_argument("go_args", nargs=argparse.REMAINDER, help="go test arguments after --")
    args = parser.parse_args(argv)
    if args.shard_count < 1 or not 1 <= args.shard_index <= args.shard_count:
        parser.error("require 1 <= shard-index <= shard-count")
    go_args = args.go_args
    if go_args[:1] == ["--"]:
        go_args = go_args[1:]
    if not go_args:
        parser.error("go test arguments are required after --")
    if any(
        arg.split("=", 1)[0].removeprefix("-test.") in (
            "-run", "-skip", "-list", "-bench", "-fuzz", "-args",
            "run", "skip", "list", "bench", "fuzz",
        )
        for arg in go_args
    ):
        parser.error("test selection flags and -args cannot be combined with this runner")

    command = ["go", "test", *go_args]
    try:
        if args.shard_count > 1:
            discovery = subprocess.run(
                [*command, "-json", "-list", "."],
                check=True, text=True, stdout=subprocess.PIPE,
            )
            names = discover_tests(discovery.stdout)
            selected = select_tests(names, args.shard_index, args.shard_count)
            if not selected:
                raise ValueError(
                    f"Shard {args.shard_index}/{args.shard_count} contains no runnable tests"
                )
            print(
                f"Go test shard {args.shard_index}/{args.shard_count}: "
                f"{len(selected)} of {len(names)} top-level test names",
                flush=True,
            )
            # An exact top-level match runs every subtest and fuzz seed underneath it.
            command.append("-run=^(" + "|".join(re.escape(name) for name in selected) + ")$")
        return subprocess.call(command)
    except subprocess.CalledProcessError as error:
        if error.stdout:
            print(error.stdout, end="")
        print(f"ERROR: go test discovery failed (exit {error.returncode})", file=sys.stderr)
        return error.returncode
    except (OSError, ValueError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
