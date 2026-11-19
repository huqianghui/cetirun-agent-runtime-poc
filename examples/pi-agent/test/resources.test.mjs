// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import test from "node:test";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { approvedResources } from "../pi-resources.mjs";
import { createPiRuntime, piTools } from "../pi-runtime.mjs";
import { createAgentSession, ModelRuntime, SessionManager, SettingsManager } from "@earendil-works/pi-coding-agent";

test("the real Pi SDK discovers approved context without model credentials", async (t) => {
  const home = await mkdtemp(path.join(os.tmpdir(), "cetirun-pi-sdk-"));
  t.after(() => rm(home, { recursive: true, force: true }));
  await mkdir(path.join(home, ".agents/skills/cetirun-memory"), { recursive: true });
  await writeFile(path.join(home, ".agents/AGENTS.md"), "Synthetic approved instructions");
  await writeFile(path.join(home, ".agents/skills/cetirun-memory/SKILL.md"),
    "---\nname: cetirun-memory\ndescription: Write approved private notes.\n---\nUse private HOME.");
  const loader = await approvedResources(home, {
    CETIRUN_CONNECTIONS_MCP_URL: "https://connections.tools.svc.cluster.local/mcp",
    CETIRUN_CONNECTIONS_MCP_TOKEN_FILE: path.join(home, "not-read-before-tool-call"),
    CETIRUN_CONNECTIONS_MCP_CA_FILE: path.join(home, "not-read-before-tool-call-ca"),
  });
  assert.deepEqual(loader.getSkills().skills.map(({ name }) => name), ["cetirun-memory"]);
  assert.equal(loader.getAgentsFiles().agentsFiles[0].content, "Synthetic approved instructions");
  const runtime = await createPiRuntime(home);
  assert.equal(await runtime.authenticated(), false);
  assert.deepEqual(runtime.skills(), ["cetirun-memory"]);
  const modelRuntime = await ModelRuntime.create({
    authPath: path.join(home, ".pi/agent/auth.json"), modelsPath: null, refreshOnCreate: false,
  });
  await modelRuntime.refresh({ allowNetwork: false, providers: ["github-copilot"] });
  const model = modelRuntime.models.getModels().find((candidate) => candidate.provider === "github-copilot");
  assert.ok(model, "pinned Pi release must contain a Copilot model");
  const sessionManager = SessionManager.inMemory(path.join(home, "workspace"));
  sessionManager.appendMessage({ role: "user", content: "Synthetic earlier turn", timestamp: 1 });
  const { session } = await createAgentSession({
    cwd: path.join(home, "workspace"), agentDir: path.join(home, ".pi/agent"),
    model, modelRuntime, resourceLoader: loader, sessionManager,
    tools: piTools(loader),
    settingsManager: SettingsManager.inMemory({ retry: { enabled: false }, compaction: { enabled: false } }),
  });
  try {
    assert.match(session.systemPrompt, /Synthetic approved instructions/);
    assert.match(session.systemPrompt, /cetirun-memory/);
    assert.ok(session.getActiveToolNames().includes("write"));
    assert.ok(session.getActiveToolNames().includes("bash"));
    for (const name of ["connections_status", "connections_start_link", "services_list", "service_request"]) {
      assert.ok(session.getActiveToolNames().includes(name), "the real Pi SDK must register " + name);
    }
  } finally {
    session.dispose();
  }
});
