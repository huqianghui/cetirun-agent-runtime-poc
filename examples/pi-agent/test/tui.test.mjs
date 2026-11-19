// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, rm, symlink } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

test("the installed pi-tui symlink runs the CLI entrypoint", async (t) => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "cetirun-pi-tui-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const executable = path.join(directory, "pi-tui");
  await symlink(fileURLToPath(new URL("../pi-tui.mjs", import.meta.url)), executable);
  const run = (...args) => spawnSync(process.execPath, [executable, ...args], {
    encoding: "utf8",
    timeout: 5000,
    env: { ...process.env, HOME: directory },
  });

  const help = run("--help");
  assert.equal(help.status, 0, help.stderr);
  assert.match(help.stdout, /^Usage: pi-tui/);
  assert.equal(help.stderr, "");

  for (const args of [["--invalid"], ["--new"]]) {
    const result = run(...args);
    assert.equal(result.status, 1, "invalid input and non-TTY startup must fail");
    assert.match(result.stderr, /Pi TUI startup failed/);
    assert.equal(result.stdout, "");
  }
});
