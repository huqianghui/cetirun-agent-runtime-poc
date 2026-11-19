// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Requires separately installed OpenClaw 2026.9.6. All state and requests are
// synthetic; the native completion route talks only to this loopback fixture.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile, mkdir } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const host = process.env.MEMORY_OPENCLAW_SOURCE;
assert.equal(JSON.parse(await readFile(path.join(host, "package.json"), "utf8")).version, "2026.9.6");
const native = name => import(pathToFileURL(path.join(host, "dist", name)).href);
const requests = [];
let status = 200, hold = false;
const server = createServer(async (request, response) => {
  assert.equal(request.url, "/v1/chat/completions");
  let raw = "";
  for await (const part of request) raw += part;
  const body = JSON.parse(raw);
  requests.push(body);
  assert.ok(!body.tools?.length);
  assert.ok(!raw.includes("PRIVATE-MEMORY-FIXTURE-CONTENT"));
  if (hold) return;
  response.writeHead(status, { "Content-Type": status === 200 ? "text/event-stream" : "application/json" });
  if (status !== 200) { response.end('{"error":{"message":"synthetic error"}}'); return; }
  response.end(`data: ${JSON.stringify({ id: "fixture", model: "model", choices: [{ index: 0, delta: { role: "assistant", content: '{"total":9}' }, finish_reason: "stop" }] })}\n\ndata: [DONE]\n\n`);
});
await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
await mkdir(process.env.OPENCLAW_STATE_DIR, { recursive: true });
const logger = { info() {}, warn() {}, error() {}, debug() {} };
const config = {
  agents: { defaults: { model: { primary: "fixture/model" }, workspace: process.env.MEMORY_TEST_WORKSPACE } },
  models: { providers: { fixture: { baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: "openai-completions", apiKey: "sk-dummy", models: [
    { id: "model", name: "Synthetic fixture", input: ["text"], reasoning: false, contextWindow: 8192, maxTokens: 512,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } },
  ] } } },
  plugins: { allow: ["cetirun-memory-context"], load: { paths: [path.join(process.env.MEMORY_TEST_ASSETS, "adapters/openclaw")] }, entries: { "cetirun-memory-context": {
    enabled: true, hooks: { allowConversationAccess: true }, config: { owner_access: { platforms: ["webchat", "slack", "cron"] } },
  } } },
};
try {
  // Loading the real package catches manifest, import and host registration
  // incompatibilities. The helper bridge is separately exercised with real data.
  delete process.env.CETIRUN_MEMORY_ASSETS;
  const { r: loadPlugins } = await native("loader-runtime-load-CMBCNIWa.mjs");
  const registry = loadPlugins({ config, workspaceDir: process.env.MEMORY_TEST_WORKSPACE, logger, cache: false });
  const plugin = registry.plugins.find(item => item.id === "cetirun-memory-context");
  assert.ok(plugin, "Memory plugin was not discovered");
  assert.equal(plugin.status, "loaded", JSON.stringify(plugin));
  assert.ok(registry.tools.some(item => item.pluginId === "cetirun-memory-context"), JSON.stringify({ plugin, tools: registry.tools.map(item => ({ pluginId: item.pluginId, names: item.names })), diagnostics: registry.diagnostics }));
  assert.ok(registry.typedHooks.some(item => item.pluginId === "cetirun-memory-context" && item.hookName === "before_prompt_build"));
  // Exercise the real prompt builder used by the embedded harness, whose
  // normal caller supplies prompt/messages but no currentUserMessage.
  const { registerMemory } = await import(pathToFileURL(path.join(process.env.MEMORY_TEST_ASSETS, "adapters/openclaw/index.mjs")).href);
  const { createMemoryBridge, loadAssets } = await import(pathToFileURL(path.join(process.env.MEMORY_TEST_ASSETS, "adapters/common/bridge.mjs")).href);
  const bridge = await createMemoryBridge({ ...process.env, CETIRUN_MEMORY_ASSETS: process.env.MEMORY_TEST_ASSETS },
    { assets: await loadAssets(process.env.MEMORY_TEST_ASSETS) });
  const seed = bridge.session({ entrypoint: "openclaw", nativeVersion: "2026.9.6", sessionId: "sdk-seed", opportunityId: "sdk-seed",
    cwd: process.env.MEMORY_TEST_WORKSPACE, audience: "owner-private", destination: { kind: "owner-private" } });
  assert.equal((await seed.ownerPreference("Remember my preference: PRIVATE-MEMORY-FIXTURE-CONTENT")).ok, true);
  const typedHooks = [];
  let factory;
  registerMemory({ runtime: { version: "2026.9.6" }, on: (hookName, handler) => typedHooks.push({ pluginId: plugin.id, hookName, handler }),
    registerTool: registration => { factory = registration.create; } }, { bridge });
  const globals = await native("hook-runner-global-C81Znoo2.mjs");
  globals.i({ ...registry, typedHooks });
  const { _: buildPrompt } = await native("agent-harness-runtime-Cq-eu9so.mjs");
  // The context OpenClaw's embedded runner builds for an owner's Control UI turn
  // (attempt-prompt-build.ts), from OpenClaw's own builders: chat.send uses the
  // internal "webchat" route to the session key, with no input provenance and,
  // for an operator UI client, no sender ID.
  const { t: channelFields, n: identityFields } = await native("hook-agent-context-C33rGaR3.mjs");
  const context = { sessionId: "sdk-owner", sessionKey: "agent:main:main", runId: "sdk-run", workspaceDir: process.env.MEMORY_TEST_WORKSPACE,
    agentId: "main", trigger: "user",
    ...channelFields({ sessionKey: "agent:main:main", messageProvider: "webchat", currentChannelId: "agent:main:main", messageTo: "agent:main:main" }),
    ...identityFields({ trigger: "user" }) };
  assert.equal(context.channel, "webchat");
  const enabled = await buildPrompt({ prompt: "Synthetic ordinary task", developerInstructions: "Synthetic fixture contract.", messages: [], ctx: context });
  assert.ok(enabled.prompt.includes("PRIVATE-MEMORY-FIXTURE-CONTENT"), "native builder did not run the Memory plugin");
  for (const [name, fields] of [
    ["group", { trigger: "user", messageProvider: "slack", channel: "slack", senderId: "fixture-sender", channelContext: { chat: { type: "group" } } }],
    ["cron", { trigger: "cron", jobId: "fixture-job" }],
  ]) {
    const channel = { ...context, ...fields, sessionId: `sdk-${name}`, sessionKey: `agent:main:${name}:fixture`, runId: `sdk-${name}` };
    const result = await buildPrompt({ prompt: "Synthetic channel task", developerInstructions: "Synthetic fixture contract.", messages: [], ctx: channel });
    assert.ok(result.prompt.includes("PRIVATE-MEMORY-FIXTURE-CONTENT"), `${name} did not receive Memory by default`);
    const query = await factory({ ...channel, assertInvocationCurrent() {} }).execute(`${name}-query`, { command: "query", payload: { queryTerms: ["preference"] } });
    assert.equal(query.details.ok, true, JSON.stringify(query.details));
  }
  const disabled = await buildPrompt({ prompt: "/memory off", developerInstructions: "Synthetic fixture contract.", messages: [], ctx: { ...context, runId: "sdk-off" } });
  assert.ok(!disabled.prompt.includes("PRIVATE-MEMORY-FIXTURE-CONTENT"), "native prompt-only input ignored off");
  const denied = await factory({ ...context, runId: "sdk-off", assertInvocationCurrent() {} }).execute("off-query", { command: "query", payload: { queryTerms: ["preference"] } });
  assert.equal(denied.details.reason, "MemoryDisabled");
  globals.a();
  const { createRuntimeLlm } = await native("runtime-llm.runtime-CRhsHHlN.mjs");
  const llm = createRuntimeLlm({ getConfig: () => config, logger,
    authority: { caller: { kind: "plugin", id: "cetirun-memory-context" }, agentId: "main" } });
  const request = { systemPrompt: "Return only a JSON object from synthetic input.", messages: [{ role: "user", content: "Return the synthetic total 9." }],
    maxTokens: 512, temperature: 0, purpose: "cetirun-memory/artifact-contract-v1", signal: AbortSignal.timeout(10000) };
  const result = await llm.complete(request);
  assert.equal(result.stopReason, "stop");
  assert.equal(result.text, '{"total":9}');
  assert.equal(requests.length, 1);
  status = 500;
  const failure = await llm.complete({ ...request, signal: AbortSignal.timeout(5000) }).catch(() => null);
  assert.notEqual(failure?.stopReason, "stop");
  assert.equal(requests.length, 2, "native transport retried a failed request");
  status = 200; hold = true;
  const cancelled = await llm.complete({ ...request, signal: AbortSignal.timeout(200) }).catch(() => null);
  assert.notEqual(cancelled?.stopReason, "stop");
  assert.equal(requests.length, 3);
  console.log("Pinned OpenClaw host checks passed");
} finally {
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
}
