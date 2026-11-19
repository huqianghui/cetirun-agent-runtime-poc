# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Validate every PM candidate before its selection is committed."""

import argparse
import hashlib
from pathlib import Path


SOURCE_SHA256 = "3e43b75e2b3753c4202ed37355819985f2c835607342b7964c4e97425ce5616e"
PATCHED_SHA256 = "83b8445e736aee4c0695bc03012c5694be868b6e5c480f729b4b92979f480ec9"
BEFORE = '''            if repair:
                from pm.environments import venv_command
                from pm.recovery import validate_environment
                validate_environment(venv_command(project, candidate, ("-I",)),
                                     env=dict(environment.env), cwd=resolved_lock.parent)
'''
AFTER = "\n".join(line[4:] for line in BEFORE.splitlines()[1:]) + "\n"


def apply(root):
    path = root / "pm/packages.py"
    source = path.read_bytes()
    if hashlib.sha256(source).hexdigest() != SOURCE_SHA256:
        raise RuntimeError("Hermes PM candidate validation source has changed")
    text = source.decode()
    if text.count(BEFORE) != 1:
        raise RuntimeError("Hermes PM candidate validation boundary has changed")
    patched = text.replace(BEFORE, AFTER)
    if hashlib.sha256(patched.encode()).hexdigest() != PATCHED_SHA256:
        raise RuntimeError("Hermes PM candidate validation patch is inconsistent")
    path.write_text(patched)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    apply(parser.parse_args().root)
