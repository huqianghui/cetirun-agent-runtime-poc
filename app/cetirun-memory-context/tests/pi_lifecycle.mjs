// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { createServer } from "node:http";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { createPiServices, piTools } from "../../../examples/pi-agent/pi-runtime.mjs";
import { approvedResources } from "../../../examples/pi-agent/pi-resources.mjs";
import { verifyMemoryAssets } from "../../../examples/pi-agent/memory-runtime.mjs";
import { createMemoryBridge, loadAssets } from "../assets/adapters/common/bridge.mjs";

const { createAgentSession, SessionManager, SettingsManager } = await import(pathToFileURL(path.resolve("examples/pi-agent/node_modules/@earendil-works/pi-coding-agent/dist/index.js")).href);
const home = process.env.HOME, root = process.env.MEMORY_TEST_ASSETS, cwd = process.env.MEMORY_TEST_WORKSPACE;
const bridge = await createMemoryBridge(process.env, { assets: await loadAssets(root) });
const seed = bridge.session({ entrypoint: "pi-tui", nativeVersion: "0.87.1", cwd, sessionId: "seed", opportunityId: "seed",
  audience: "owner-private", destination: { kind: "owner-private" } });
const note = { kind: "fact", title: "PRIVATE-LIFECYCLE-FIXTURE", scope: { project: cwd }, body: "Synthetic private note",
  temporalClass: "historical", status: "active", revalidation: { mode: "on-change" },
  sources: [{ kind: "fixture", ref: "fixture-source", origin: "tool-observed", assurance: "reported" }] };
assert.equal((await seed.tool({ command: "capture", payload: { note } }, "seed-write")).ok, true);
let scenario;
const reply = (response, delta, reason) => {
  response.writeHead(200, { "Content-Type": "text/event-stream" });
  response.end(`data: ${JSON.stringify({ id: "fixture", model: "fixture", choices: [{ index: 0, delta, finish_reason: reason }] })}\n\ndata: [DONE]\n\n`);
};
const server = createServer(async (request, response) => {
  if (request.url === "/v1/models") { response.setHeader("Content-Type", "application/json"); response.end('{"data":[{"id":"fixture"}]}'); return; }
  let raw = "";
  for await (const chunk of request) raw += chunk;
  const body = JSON.parse(raw), index = scenario.requests.length;
  scenario.requests.push(body);
  if (index === 0) {
    scenario.started.resolve();
    await scenario.release.promise;
    reply(response, { role: "assistant", content: "Synthetic first response." }, "stop");
  } else if (index === 1) {
    reply(response, { role: "assistant", tool_calls: [{ index: 0, id: `write-${scenario.name}`, type: "function",
      function: { name: "cetirun_memory", arguments: JSON.stringify({ command: "capture", payload: { note: { ...note, title: `blocked-${scenario.name}` } } }) } }] }, "tool_calls");
  } else {
    reply(response, { role: "assistant", content: "Synthetic task finished." }, "stop");
  }
});
await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
const env = { ...process.env, OPENAI_BASE_URL: `http://127.0.0.1:${server.address().port}/v1`, OPENAI_API_KEY: "sk-dummy" };
const services = await createPiServices(home, cwd, { ...env, CETIRUN_MEMORY_ASSETS: undefined }, "pi-tui");
const model = (await services.modelAccess.models(AbortSignal.timeout(5000)))[0];
async function createSession() {
  const loader = await approvedResources(home, env, cwd, "pi-tui", () => verifyMemoryAssets(root));
  const { session } = await createAgentSession({ ...services, model, resourceLoader: loader, tools: piTools(loader), sessionManager: SessionManager.inMemory(cwd),
    settingsManager: SettingsManager.inMemory({ retry: { enabled: false }, compaction: { enabled: false } }) });
  return { session, loader };
}
try {
  for (const streamingBehavior of ["steer", "followUp"]) {
    for (const mode of ["off", "no-capture"]) {
      scenario = { name: `${streamingBehavior}-${mode}`, requests: [], started: Promise.withResolvers(), release: Promise.withResolvers() };
      const { session } = await createSession();
      try {
        const running = session.prompt("Synthetic owner task");
        await scenario.started.promise;
        assert.ok(JSON.stringify(scenario.requests[0]).includes(note.title));
        await session.prompt(`/memory ${mode}`, { streamingBehavior });
        const afterRestriction = (await seed.tool({ command: "status", payload: {} })).generation;
        scenario.release.resolve();
        await running;
        const results = session.messages.filter(message => message.role === "toolResult");
        assert.ok(results.length, "queued input did not reach a real tool call");
        const reason = mode === "off" ? "MemoryDisabled" : "CaptureProhibited";
        assert.ok(results.some(result => JSON.stringify(result).includes(reason)), `${scenario.name}: ${JSON.stringify(results)}`);
        if (mode === "off") {
          for (const request of scenario.requests.slice(1)) assert.ok(!JSON.stringify(request).includes(note.title), `${scenario.name} reused private context`);
        }
        const after = await seed.tool({ command: "status", payload: {} });
        assert.equal(after.generation, afterRestriction, `${scenario.name} wrote after restriction`);
      } finally { scenario.release.resolve(); session.dispose(); }
    }
  }

  scenario = { name: "cancel", requests: [] };
  const { session, loader } = await createSession();
  try {
    const entered = Promise.withResolvers(), release = Promise.withResolvers();
    const handlers = loader.getExtensions().extensions[0].handlers.get("context");
    const original = handlers[0];
    handlers[0] = async (event, ctx) => { entered.resolve(); await release.promise; return original(event, ctx); };
    const before = (await seed.tool({ command: "status", payload: {} })).generation;
    const running = session.prompt("Cancel before synthetic model dispatch");
    await entered.promise;
    const aborting = session.abort();
    release.resolve();
    await Promise.all([running, aborting]);
    assert.equal(scenario.requests.length, 0, "cancelled task reached model transport");
    assert.equal((await seed.tool({ command: "status", payload: {} })).generation, before, "cancelled context wrote an injection record");
  } finally { session.dispose(); }
  console.log("Pinned Pi lifecycle checks passed");
} finally {
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
}
