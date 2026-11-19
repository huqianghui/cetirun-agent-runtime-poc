# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Scope the pinned Webapp's browser state before its stores are imported."""

import argparse
import hashlib
from pathlib import Path

EXPECTED_SHA256 = "86008d5876e10da86cb1e1f206cd144f18387707c76536f08f97c9c71695b40d"
HANDOFF_SHA256 = "d3837976e33f199e5335955d22a7fdf3039dd463ca0c46b5ace0138f53d7dc79"
HANDOFF_BEFORE = "  const channel = new BroadcastChannel(`hermes.webapp.window:${id}`);\n"
HANDOFF_AFTER = r"""  const namespace = /^\/ui\/[a-z0-9][a-z0-9-]*$/.test(basePath)
    ? `cetirun.webapp:${encodeURIComponent(basePath)}:` : '';
  const channel = new BroadcastChannel(namespace + `hermes.webapp.window:${id}`);
"""
BEFORE = "import { showBootFailure } from './lib/boot-failure'\nimport { bootRenderer } from './lib/ui-preferences/boot'\n\nvoid bootRenderer(() => import('./main')).catch(showBootFailure)\n"
AFTER = """import { scopeWebappStorage } from './lib/cetirun-webapp-storage'

scopeWebappStorage()
void import('./lib/ui-preferences/boot')
  .then(({ bootRenderer }) => bootRenderer(() => import('./main')))
  .catch(async error => (await import('./lib/boot-failure')).showBootFailure(error))
"""


def prepare_patch(source: str) -> str:
    original = source.replace(AFTER, BEFORE, 1) if AFTER in source else source
    if hashlib.sha256(original.encode()).hexdigest() != EXPECTED_SHA256 or original.count(BEFORE) != 1:
        raise ValueError("Unsupported Hermes renderer entry; review Webapp browser state scope")
    return original.replace(BEFORE, AFTER, 1)


def prepare_handoff_patch(source: str) -> str:
    original = source.replace(HANDOFF_AFTER, HANDOFF_BEFORE, 1) if HANDOFF_AFTER in source else source
    if hashlib.sha256(original.encode()).hexdigest() != HANDOFF_SHA256 or original.count(HANDOFF_BEFORE) != 1:
        raise ValueError("Unsupported Hermes Webapp handoff source; review browser state scope")
    return original.replace(HANDOFF_BEFORE, HANDOFF_AFTER, 1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    entry = args.root / "apps/desktop/src/entry.ts"
    target = entry.parent / "lib/cetirun-webapp-storage.ts"
    handoff = args.root / "hermes_cli/web_routers/webapp.py"
    module = Path(__file__).with_name("webapp_storage.ts").read_text()
    try:
        patched = prepare_patch(entry.read_text())
        patched_handoff = prepare_handoff_patch(handoff.read_text())
        if target.exists() and target.read_text() != module:
            raise ValueError("Hermes Webapp browser state module differs from the managed source")
        if not args.check:
            entry.write_text(patched)
            target.write_text(module)
            handoff.write_text(patched_handoff)
    except (OSError, ValueError) as exc:
        parser.exit(1, str(exc) + "\n")


if __name__ == "__main__":
    main()
