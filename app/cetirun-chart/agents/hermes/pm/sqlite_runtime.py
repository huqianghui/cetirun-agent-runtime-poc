# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Load the image's SQLite extension ahead of the PM Python's built-in copy."""

import importlib.util
from pathlib import Path
import sys
import sysconfig


def install():
    if "sqlite3" in sys.modules:
        if sys.modules["sqlite3"].sqlite_version_info != (3, 53, 4):
            raise RuntimeError("SQLite was imported before the fixed Hermes runtime")
        return
    root = Path(__file__).resolve().parents[1]
    extension = root / "sqlite" / ("_sqlite3" + sysconfig.get_config_var("EXT_SUFFIX"))
    if not extension.is_file():
        raise RuntimeError("the fixed Hermes SQLite extension is missing")
    spec = importlib.util.spec_from_file_location("_sqlite3", extension)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    sys.modules["_sqlite3"] = module
    import sqlite3

    if sqlite3.sqlite_version_info != (3, 53, 4):
        raise RuntimeError("Hermes did not load SQLite 3.53.4")


def bootstrap():
    try:
        install()
    except Exception as exc:
        # site.py logs ordinary .pth exceptions and continues on the built-in
        # SQLite. A damaged fixed runtime must stop before application imports.
        raise SystemExit(f"Hermes SQLite runtime: {exc}") from exc
