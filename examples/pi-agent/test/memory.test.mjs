// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { createServer } from "node:http";
import { mkdtemp, mkdir, readFile, writeFile, rm, symlink, unlink } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { createAgentSession, SessionManager } from "@earendil-works/pi-coding-agent";
import { approvedResources } from "../pi-resources.mjs";
import { createPiServices } from "../pi-runtime.mjs";
import { memoryBridge, memoryResources, verifyMemoryAssets } from "../memory-runtime.mjs";
import { invoke } from "../../../app/cetirun-memory-context/assets/adapters/common/bridge.mjs";

async function fixture(t) {
  const home = await mkdtemp(path.join(os.tmpdir(), "cetirun-pi-memory-"));
  t.after(() => rm(home, { recursive: true, force: true }));
  const root = path.join(home, "assets");
  const statePath = path.join(home, "fixture.json");
  const logPath = path.join(home, "calls.jsonl");
  await mkdir(path.join(root, "bin"), { recursive: true });
  await mkdir(path.join(root, "adapters/common"), { recursive: true });
  await mkdir(path.join(root, "skills/cetirun-memory"), { recursive: true });
  await mkdir(path.join(home, ".agents/skills/cetirun-memory"), { recursive: true });
  await mkdir(path.join(home, ".agents/skills/team-fixture"), { recursive: true });
  const shared = "---\nname: team-fixture\ndescription: Shared fixture, unchanged.\n---\nTeam fixture.";
  await writeFile(path.join(home, ".agents/AGENTS.md"), "Original approved fixture context.");
  await writeFile(path.join(home, ".agents/skills/team-fixture/SKILL.md"), shared);
  await writeFile(path.join(home, ".agents/skills/cetirun-memory/SKILL.md"), "---\nname: cetirun-memory\ndescription: Platform view fixture.\n---\nLoad this assembled view.");
  const files = {
    "adapters/common/bridge.mjs": await readFile(new URL("../../../app/cetirun-memory-context/assets/adapters/common/bridge.mjs", import.meta.url), "utf8"),
    "adapters/common/payload-schema.json": await readFile(new URL("../../../app/cetirun-memory-context/assets/adapters/common/payload-schema.json", import.meta.url), "utf8"),
    "AGENTS.md": "Fixed Memory contract; private data is not stored in this prompt.",
    "skills/cetirun-memory/SKILL.md": "---\nname: cetirun-memory\ndescription: Controlled fixture.\n---\nUse the controlled fixture helper.",
    "bin/cetirun-memory": `#!${process.execPath}
const fs = require("node:fs");
let raw = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", chunk => raw += chunk);
process.stdin.on("end", () => {
  const event = JSON.parse(raw);
  const metadata = event.binding ? { platform: event.binding.audience === "owner-private" ? event.binding.entrypoint : "unknown", cwd: event.binding.cwd, session_id: event.binding.sessionId, parent_session_id: event.binding.parentSessionId, user_message: event.event === "owner-preference" ? event.payload.message : undefined } : event;
  const state = JSON.parse(fs.readFileSync(process.env.MEMORY_FIXTURE));
  fs.appendFileSync(process.env.MEMORY_CALLS, JSON.stringify(metadata) + "\\n");
  if (state.hang) { setInterval(() => {}, 1000); return; }
  const context = state.oversize ? "x".repeat(40000) : metadata.platform === "unknown" || metadata.parent_session_id ? "Audience unqualified" : state.context;
  console.log(JSON.stringify(event.binding ? { ok: true, reason: "OK", data: { context, refs: [] } } : { context }));
});
`,
  };
  for (const [name, content] of Object.entries(files)) {
    await writeFile(path.join(root, name), content, { mode: name.startsWith("bin/") ? 0o700 : 0o600 });
  }
  await writeFile(path.join(root, "manifest.json"), JSON.stringify({
    contractVersion: "memory-context/v1", helperABI: 1,
    files: Object.entries(files).map(([name, content]) => ({ path: name, mode: name.startsWith("bin/") ? 365 : 292, digest: `sha256:${createHash("sha256").update(content).digest("hex")}` })),
  }));
  await writeFile(statePath, JSON.stringify({ context: "Private fixture generation one" }));
  const env = { MEMORY_FIXTURE: statePath, MEMORY_CALLS: logPath };
  return {
    home, root, env, shared,
    helper: path.join(root, "bin/cetirun-memory"),
    update: state => writeFile(statePath, JSON.stringify(state)),
    calls: async () => (await readFile(logPath, "utf8")).trim().split("\n").map(JSON.parse),
  };
}

test("Memory assets are digest-checked without changing shared skill discovery", async t => {
  const f = await fixture(t);
  const loader = await approvedResources(f.home, f.env, path.join(f.home, "workspace"), "pi-http", () => verifyMemoryAssets(f.root));
  assert.deepEqual(loader.getSkills().skills.map(skill => skill.name).sort(), ["cetirun-memory", "team-fixture"]);
  assert.equal(loader.getSkills().skills.find(skill => skill.name === "cetirun-memory").description, "Platform view fixture.");
  assert.deepEqual(loader.getAgentsFiles().agentsFiles, [{ path: path.join(f.home, ".agents/AGENTS.md"), content: "Original approved fixture context." }]);
  assert.ok(loader.getSkills().skills.every(skill => skill.filePath.startsWith(path.join(f.home, ".agents/skills/"))));
  assert.equal(await readFile(path.join(f.home, ".agents/skills/team-fixture/SKILL.md"), "utf8"), f.shared);
  assert.ok(!JSON.stringify(loader.getAgentsFiles()).includes("Private fixture generation one"));
  await assert.rejects(memoryResources(f.home, { CETIRUN_MEMORY_ASSETS: f.root }), /binding is invalid/);
  await writeFile(path.join(f.root, "AGENTS.md"), "Modified fixture");
  await assert.rejects(verifyMemoryAssets(f.root), /digest mismatch/);
  await unlink(path.join(f.root, "AGENTS.md"));
  await symlink(path.join(f.home, ".agents/AGENTS.md"), path.join(f.root, "AGENTS.md"));
  await assert.rejects(verifyMemoryAssets(f.root), /Invalid Memory asset/);
});

test("modified Memory payload guidance is rejected before invoking the helper", async t => {
  const f = await fixture(t);
  await writeFile(path.join(f.root, "adapters/common/payload-schema.json"), '{"type":"object"}');
  await assert.rejects(memoryBridge({ root: f.root }, f.home, f.env), /MemoryAssetMismatch/);
  await assert.rejects(readFile(path.join(f.home, "calls.jsonl")), { code: "ENOENT" });
});

test("the native helper bridge bounds output, handles cancellation, and reports failure", async t => {
  const f = await fixture(t);
  const event = { binding: { entrypoint: "custom", audience: "owner-private", cwd: "fixture" }, event: "context" };
  assert.equal((await invoke(f.helper, event, f.env)).data.context, "Private fixture generation one");
  assert.equal((await invoke(f.helper, event, f.env, AbortSignal.abort())).reason, "Cancelled");
  assert.equal((await f.calls()).length, 1, "pre-cancelled calls must not start a helper");
  await f.update({ oversize: true });
  const huge = { ...event, padding: "x".repeat(128 * 1024) };
  assert.equal((await invoke(f.helper, huge, f.env)).reason, "LimitReached");
  await f.update({ hang: true });
  assert.equal((await invoke(f.helper, event, f.env, AbortSignal.timeout(200))).ok, false);
  assert.equal((await invoke(path.join(f.home, "missing-helper"), event, f.env)).reason, "MemoryUnavailable");
});

test("the real Pi SDK refreshes private context without retaining it in native history", async t => {
  const f = await fixture(t);
  const inputs = [];
  const server = createServer(async (request, response) => {
    if (request.url === "/v1/models") {
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({ data: [{ id: "shared-example", object: "model" }] }));
      return;
    }
    let raw = "";
    for await (const chunk of request) raw += chunk;
    inputs.push(JSON.parse(raw));
    response.writeHead(200, { "Content-Type": "text/event-stream" });
    response.end(`data: ${JSON.stringify({ id: "memory-fixture", object: "chat.completion.chunk", model: "shared-example", choices: [{ index: 0, delta: { role: "assistant", content: "Fixture response." }, finish_reason: "stop" }] })}\n\ndata: [DONE]\n\n`);
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  const env = { ...f.env, OPENAI_BASE_URL: `http://127.0.0.1:${server.address().port}/v1`, OPENAI_API_KEY: "sk-dummy-fixture-key" };
  const cwd = path.join(f.home, "workspace");
  const services = await createPiServices(f.home, cwd, env, "pi-tui");
  const model = (await services.modelAccess.models(AbortSignal.timeout(5000)))[0];
  const resourceLoader = await approvedResources(f.home, env, cwd, "pi-tui", () => verifyMemoryAssets(f.root));
  const { session } = await createAgentSession({ ...services, model, resourceLoader, sessionManager: SessionManager.inMemory(cwd), tools: [] });
  t.after(() => session.dispose());
  await session.prompt("First synthetic task");
  assert.match(JSON.stringify(inputs[0].messages), /Private fixture generation one/);
  assert.ok(!JSON.stringify(session.messages).includes("Private fixture generation one"));
  assert.ok(!session.systemPrompt.includes("Private fixture generation one"));
  await f.update({ context: "Private fixture generation two" });
  await session.prompt("Second synthetic task");
  assert.match(JSON.stringify(inputs[1].messages), /Private fixture generation two/);
  assert.ok(!JSON.stringify(inputs[1].messages).includes("Private fixture generation one"));
  await session.prompt("Remember my preference: a synthetic private preference");
  assert.equal((await f.calls()).filter(call => call.user_message).length, 1);
  await session.prompt("Remember my preference: extension text is not owner input", { source: "extension" });
  assert.equal((await f.calls()).filter(call => call.user_message).length, 1);
  assert.ok(!JSON.stringify(inputs[3].messages).includes("Private fixture generation two"));
  assert.ok((await f.calls()).every(call => call.session_id && call.cwd === cwd));
  assert.ok(!JSON.stringify(session.messages).includes("Private fixture generation two"));
});

test("Pi passes inherited-session metadata rather than assuming a fork is private", async t => {
  const f = await fixture(t);
  const cwd = path.join(f.home, "workspace");
  const loader = await approvedResources(f.home, f.env, cwd, "pi-tui", () => verifyMemoryAssets(f.root));
  const handlers = loader.getExtensions().extensions[0].handlers;
  handlers.get("input")[0]({ source: "interactive", text: "Synthetic fork task" });
  const result = await handlers.get("context")[0]({ messages: [] }, {
    cwd, sessionManager: { getSessionId: () => "child-fixture", getHeader: () => ({ parentSession: "parent-fixture" }) },
  });
  assert.equal(result.messages[0].content, "Audience unqualified");
  assert.equal((await f.calls())[0].parent_session_id, "parent-fixture");
});
