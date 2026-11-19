// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { createHash } from "node:crypto";
import { lstat, readFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

export async function memoryResources(home, env = process.env) {
  const root = env.CETIRUN_MEMORY_ASSETS;
  if (!root) return null;
  if (root !== "/opt/cetirun-memory" || env.CETIRUN_MEMORY_ROOT !== path.join(home, ".cetirun/memory")) {
    throw new Error("Memory asset binding is invalid");
  }
  return verifyMemoryAssets(root);
}

export async function verifyMemoryAssets(root) {
  const manifest = JSON.parse(await readFile(path.join(root, "manifest.json"), "utf8"));
  if (manifest.contractVersion !== "memory-context/v1" || manifest.helperABI !== 1 || !Array.isArray(manifest.files)) throw new Error("Unsupported Memory asset contract");
  for (const name of ["AGENTS.md", "skills/cetirun-memory/SKILL.md", "bin/cetirun-memory", "adapters/common/bridge.mjs"]) {
    const entry = manifest.files.find(file => file.path === name);
    const info = await lstat(path.join(root, name));
    if (!entry || !info.isFile() || info.isSymbolicLink() || info.nlink !== 1 || info.size > 32 * 1024 * 1024) throw new Error("Invalid Memory asset");
    const data = await readFile(path.join(root, name));
    if (`sha256:${createHash("sha256").update(data).digest("hex")}` !== entry.digest) throw new Error("Memory asset digest mismatch");
  }
  return { root };
}

export async function memoryBridge(assets, home, env) {
  const adapter = await import(pathToFileURL(path.join(assets.root, "adapters/common/bridge.mjs")).href);
  const verified = await adapter.loadAssets(assets.root);
  const bridge = await adapter.createMemoryBridge({ ...env, HOME: home, CETIRUN_MEMORY_ASSETS: assets.root,
    CETIRUN_MEMORY_ROOT: path.join(home, ".cetirun/memory") }, { assets: verified });
  return { bridge, tool: adapter.MEMORY_TOOL, restrictiveMode: adapter.restrictiveMode, restoredRestrictions: adapter.restoredRestrictions };
}
