# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Compile CPython's matching SQLite module against the image's fixed library."""

import argparse
from pathlib import Path
import shutil
import subprocess
import sys
import sysconfig


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--python-source", type=Path, required=True)
parser.add_argument("--sqlite", type=Path, required=True)
parser.add_argument("--out", type=Path, required=True)
args = parser.parse_args()
version = ".".join(map(str, sys.version_info[:3]))
header = (args.python_source / "Include/patchlevel.h").read_text()
assert f'#define PY_VERSION              "{version}"' in header
args.out.mkdir(parents=True, exist_ok=True)
extension = args.out / ("_sqlite3" + sysconfig.get_config_var("EXT_SUFFIX"))
includes = (Path(sysconfig.get_path("include")), args.python_source / "Include",
            args.python_source / "Include/internal", args.sqlite / "include")
subprocess.run([
    "gcc", "-shared", "-fPIC", "-O2", "-DPy_BUILD_CORE_MODULE",
    *[f"-I{path}" for path in includes],
    *map(str, sorted((args.python_source / "Modules/_sqlite").glob("*.c"))),
    f"-L{args.sqlite / 'lib'}", "-Wl,-rpath,$ORIGIN", "-lsqlite3", "-o", str(extension),
], check=True)
shutil.copy2(args.python_source / "LICENSE", args.out / "LICENSE.python")
