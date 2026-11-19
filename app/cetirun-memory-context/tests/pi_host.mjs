// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { createServer } from "node:http";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { createPiServices, piTools } from "../../../examples/pi-agent/pi-runtime.mjs";
import { approvedResources, memoryModelRevision } from "../../../examples/pi-agent/pi-resources.mjs";
import { verifyMemoryAssets } from "../../../examples/pi-agent/memory-runtime.mjs";
import { createMemoryBridge, loadAssets } from "../assets/adapters/common/bridge.mjs";

const { createAgentSession, SessionManager, SettingsManager } = await import(pathToFileURL(path.resolve("examples/pi-agent/node_modules/@earendil-works/pi-coding-agent/dist/index.js")).href);
const home = process.env.HOME, root = process.env.MEMORY_TEST_ASSETS, cwd = process.env.MEMORY_TEST_WORKSPACE;
let candidate, turn = 0, extra = 0;
const nativeRequests = [];
const server = createServer(async (request, response) => {
  if (request.url === "/v1/models") { response.setHeader("Content-Type", "application/json"); response.end('{"data":[{"id":"fixture"}]}'); return; }
  let raw = "";
  for await (const chunk of request) raw += chunk;
  const body = JSON.parse(raw);
  nativeRequests.push(body);
  const evaluation = body.messages.some(message => typeof message.content === "string" && message.content.startsWith("Transform only the supplied synthetic fixture."));
  let delta, reason;
  if (evaluation) {
    extra++;
    assert.deepEqual(body.metadata?.tags, ["cetirun-memory-evaluation", "artifact-contract-v1"]);
    assert.ok(!body.tools?.length);
    assert.ok(!raw.includes("Private synthetic task history"));
    delta = { role: "assistant", content: '{"total":9}' }; reason = "stop";
  } else if (turn++ % 2 === 0) {
    assert.ok(!body.metadata?.tags?.includes("cetirun-memory-evaluation"));
    assert.ok(body.tools.some(tool => tool.function.name === "cetirun_memory"));
    delta = { role: "assistant", tool_calls: [{ index: 0, id: `memory-call-${turn}`, type: "function", function: {
      name: "cetirun_memory", arguments: JSON.stringify({ command: "evaluate", payload: { ref: candidate } }),
    } }] }; reason = "tool_calls";
  } else { delta = { role: "assistant", content: "Synthetic native task done." }; reason = "stop"; }
  response.writeHead(200, { "Content-Type": "text/event-stream" });
  response.end(`data: ${JSON.stringify({ id: "fixture", model: "fixture", choices: [{ index: 0, delta, finish_reason: reason }] })}\n\ndata: [DONE]\n\n`);
});
await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
const env = { ...process.env, OPENAI_BASE_URL: `http://127.0.0.1:${server.address().port}/v1`, OPENAI_API_KEY: "sk-dummy" };
let session;
try {
  const services = await createPiServices(home, cwd, { ...env, CETIRUN_MEMORY_ASSETS: undefined }, "pi-tui");
  const model = (await services.modelAccess.models(AbortSignal.timeout(5000)))[0];
  const bridge = await createMemoryBridge(env, { assets: await loadAssets(root) });
  const seed = bridge.session({ entrypoint: "pi-tui", nativeVersion: "0.87.1", cwd, sessionId: "fixture-seed", opportunityId: "fixture-seed",
    audience: "owner-private", destination: { kind: "owner-private" }, model: `${model.provider}/${model.id}`, modelRevision: memoryModelRevision(model), modelEvaluation: true });
  const observed = await seed.feedback("fixture_tool", "failure", "synthetic-observation");
  const propose = async () => {
    const proposed = await seed.tool({ command: "propose", payload: { procedure: { kind: "playbook", scope: { project: cwd },
      applicability: ["Only synthetic artifact-contract-v1 records"], exclusions: ["All live tasks"], hypothesis: "Check the selected record fields.",
      sourceObservationIds: [observed.data.refs[0].id], payload: { steps: ["Read fields and apply the requested predicate."], expectedEvidence: ["Exact JSON"], stopConditions: ["Unknown schema"] } } } });
    assert.equal(proposed.ok, true, JSON.stringify(proposed));
    return proposed.data.refs[0];
  };
  candidate = await propose();
  const loader = await approvedResources(home, env, cwd, "pi-tui", () => verifyMemoryAssets(root), { modelRuntime: services.modelRuntime });
  ({ session } = await createAgentSession({ ...services, model, resourceLoader: loader, tools: piTools(loader), sessionManager: SessionManager.inMemory(cwd),
    settingsManager: SettingsManager.inMemory({ retry: { enabled: false }, compaction: { enabled: false } }) }));
  assert.ok(session.getActiveToolNames().includes("cetirun_memory"));
  await session.prompt("Private synthetic task history. Evaluate the fixture candidate once.");
  assert.equal(extra, 1);
  assert.ok(session.messages.some(message => message.role === "toolResult" && JSON.stringify(message).includes('recorded')));
  for (let i = 1; i < 4; i++) await session.prompt("Evaluate the fixture candidate within the fixed daily limit.");
  assert.equal(extra, 4);
  // A new candidate cannot reset the store's rolling model-call allowance.
  candidate = await propose();
  await session.prompt("Evaluate again only if the daily limit permits it.");
  assert.equal(extra, 4, "an extra model request bypassed the rolling limit");
  assert.ok(session.messages.some(message => message.role === "toolResult" && JSON.stringify(message).includes("BudgetExhausted")));
  assert.equal(nativeRequests.length, 14);
  console.log("Pinned Pi SDK and real helper checks passed");
} finally {
  session?.dispose();
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
}
