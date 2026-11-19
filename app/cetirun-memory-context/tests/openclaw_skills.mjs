// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const root = await mkdtemp(path.join(os.tmpdir(), "memory-skills-openclaw-"));
const assets = process.env.MEMORY_SKILLS_ASSETS ?? path.join(root, "platform");
const platform = path.join(assets, "skills");
const shared = path.join(root, "shared"), personal = path.join(root, "personal");
process.env.HOME = root;
process.env.OPENCLAW_STATE_DIR = path.join(root, ".openclaw");
process.env.OPENCLAW_CONFIG_PATH = path.join(root, ".openclaw/config.json");
process.env.CETIRUN_MEMORY_ASSETS = assets;
globalThis.fetch = () => { throw new Error("This skill-loader check must not use the network"); };

async function skill(directory, name, description) {
  await mkdir(directory, { recursive: true });
  await writeFile(path.join(directory, "SKILL.md"), `---\nname: ${name}\ndescription: ${description}\n---\nSynthetic skill.\n`);
}

try {
  if (!process.env.MEMORY_SKILLS_ASSETS) await skill(path.join(platform, "cetirun-memory"), "cetirun-memory", "Platform fixture");
  await skill(path.join(shared, "team-skill"), "team-skill", "Version one");
  await skill(path.join(personal, "personal-skill"), "personal-skill", "Personal fixture");
  const { o: readSources } = await import(pathToFileURL(path.join(process.env.MEMORY_OPENCLAW_SOURCE, "dist/workspace-skill-loader-CoQ1pd8t.mjs")).href);
  function read(directories = [personal, platform, shared]) {
    return readSources({ additionalBins: [], sourcePlan: {
      roots: directories.map((dir, order) => ({ dir, order, source: "openclaw-extra", tier: "extra" })),
      pluginSkillRoots: [], managedSkillsDir: path.join(root, "managed"), workspaceDir: root, allowSymlinkTargets: [],
    } }).entries.map(entry => entry.skill);
  }
  function check(description) {
    for (const directories of [[personal, platform, shared], [shared, platform, personal]]) {
      const skills = read(directories);
      const reserved = skills.filter(entry => entry.name === "cetirun-memory");
      assert.equal(reserved.length, 1);
      assert.equal(reserved[0].filePath, path.join(platform, "cetirun-memory/SKILL.md"));
      assert.equal(skills.find(entry => entry.name === "team-skill").description, description);
      assert.ok(skills.some(entry => entry.name === "personal-skill"));
    }
  }
  check("Version one");
  await skill(path.join(shared, "cetirun-memory"), "cetirun-memory", "Shared collision");
  check("Version one");
  await skill(path.join(personal, "renamed"), "cetirun-memory", "Personal collision");
  await skill(path.join(shared, "category/alias"), "cetirun-memory", "Frontmatter collision");
  await skill(path.join(shared, "team-skill"), "team-skill", "Version two");
  await skill(path.join(shared, "added-later"), "added-later", "Live publication");
  check("Version two");
  assert.ok(read().some(entry => entry.name === "added-later"));
  delete process.env.CETIRUN_MEMORY_ASSETS;
  assert.notEqual(read().find(entry => entry.name === "cetirun-memory").filePath, path.join(platform, "cetirun-memory/SKILL.md"));
  console.log("Pinned OpenClaw live skill-source checks passed");
} finally {
  await rm(root, { recursive: true, force: true });
}
