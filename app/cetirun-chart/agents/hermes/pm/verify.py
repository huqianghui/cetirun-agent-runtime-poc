# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Check the actual Hermes interpreter, locked tools and SQLite features."""

import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
from zoneinfo import ZoneInfo

from pm.runtime import runtime_command, runtime_environment


root = Path(os.environ["HERMES_INSTALL_DIR"])
pins = json.loads((root / "pm/lock.json").read_text())["packages"]
assert ".".join(map(str, sys.version_info[:3])) == pins["python"]["version"].split("+")[0]
for command in ("node", "npm"):
    actual = subprocess.check_output([str(root / "bin" / command), "--version"], text=True).strip().lstrip("v")
    assert actual == pins[command]["version"], (command, actual)
for zone in ("UTC", "Asia/Singapore", "America/New_York"):
    ZoneInfo(zone)
assert sqlite3.sqlite_version_info == (3, 53, 4), sqlite3.sqlite_version
with tempfile.TemporaryDirectory(prefix="hermes-sqlite-") as directory:
    database = Path(directory) / "state.db"
    with sqlite3.connect(database) as connection:
        assert connection.execute("PRAGMA journal_mode=WAL").fetchone()[0] == "wal"
        connection.execute("CREATE VIRTUAL TABLE messages USING fts5(body, tokenize='trigram')")
        connection.execute("INSERT INTO messages VALUES ('synthetic workspace message')")
    with sqlite3.connect(database) as connection:
        assert connection.execute("SELECT body FROM messages WHERE messages MATCH 'workspace'").fetchone()
        assert connection.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
for product, entry in (("apps/desktop/dist-webapp", "index.html"), ("tui-product/dist", "entry.js")):
    assert (root / product / entry).is_file()
    assert (root / product / "hermes-build.json").is_file()
assert (root / "apps/desktop/dist-webapp/renderer-manifest.json").is_file()
with tempfile.TemporaryDirectory(prefix="hermes-manager-") as directory:
    probe = Path(directory) / "probe.py"
    probe.write_text(
        f"import sys; sys.path.insert(0, {str(root)!r})\n"
        "import hashlib, pathlib, pm.packages\n"
        "from cetirun_pm.apply_generation import PATCHED_SHA256\n"
        f"expected = pathlib.Path({str(root / 'pm/packages.py')!r})\n"
        "assert pathlib.Path(pm.packages.__file__) == expected\n"
        "assert hashlib.sha256(expected.read_bytes()).hexdigest() == PATCHED_SHA256\n"
    )
    subprocess.run(runtime_command(probe, bootstrap=False), env=runtime_environment(), check=True)
print("Hermes Python, PM tools, Webapp/TUI and SQLite WAL/FTS5 checks passed")
