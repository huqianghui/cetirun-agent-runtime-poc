// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { chmod, cp, mkdtemp, mkdir, readFile, writeFile, rm, lstat, readdir, symlink, copyFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import { createMemoryBridge, digest, loadAssets, MEMORY_TOOL } from "../assets/adapters/common/bridge.mjs";
import { registerMemory, privateOrigin, evaluationRoute } from "../assets/adapters/openclaw/index.mjs";

const moduleDir = fileURLToPath(new URL("../", import.meta.url));
const stage = await mkdtemp(path.join(os.tmpdir(), "memory-native-tests-"));
const helper = path.join(stage, "cetirun-memory");
execFileSync("go", ["build", "-o", helper, "./cmd/cetirun-memory"], { cwd: moduleDir, env: { ...process.env, GOPROXY: "off", GOTOOLCHAIN: "local" } });
test.after(() => rm(stage, { recursive: true, force: true }));

test("installed CLI recovers process loss and keeps writes working after repair", () => {
  const output = execFileSync("python3", [path.join(moduleDir, "tests/cli_recovery.py"), "--helper", helper, "--work-dir", stage], { timeout: 120000, encoding: "utf8" });
  assert.ok(Object.values(JSON.parse(output)).every(result => result === "passed"));
});

async function fixture(t) {
  const home = await mkdtemp(path.join(stage, "home-"));
  const assets = path.join(home, "assets"), workspace = path.join(home, "workspace");
  await mkdir(assets); await mkdir(workspace);
  execFileSync(helper, ["install-assets", assets]);
  await mkdir(path.join(home, ".agents"));
  await cp(path.join(assets, "AGENTS.md"), path.join(home, ".agents/AGENTS.md"));
  await chmod(path.join(home, ".agents/AGENTS.md"), 0o600);
  await cp(path.join(assets, "skills"), path.join(home, ".agents/skills"), { recursive: true });
  const env = { ...process.env, HOME: home, CETIRUN_MEMORY_ASSETS: assets, CETIRUN_MEMORY_ROOT: path.join(home, ".cetirun/memory"),
    CETIRUN_MEMORY_BINARY: path.join(assets, "bin/cetirun-memory"), CETIRUN_MEMORY_CONTEXT: path.join(home, ".agents/AGENTS.md"),
    CETIRUN_MEMORY_NO_CAPTURE: "", CETIRUN_MEMORY_NO_MEMORY: "", CETIRUN_MEMORY_OWNER_ACCESS: "" };
  const bridge = await createMemoryBridge(env, { assets: await loadAssets(assets) });
  const binding = { entrypoint: "custom", nativeVersion: "fixture/1", sessionId: "native-session", opportunityId: "native-task", cwd: workspace,
    audience: "owner-private", destination: { kind: "owner-private" }, model: "fixture/model", modelRevision: digest("fixture-route"), modelEvaluation: true };
  return { home, assets, workspace, env, bridge, binding };
}
function note(f) {
  return { kind: "fact", title: "Private fixture marker", scope: { project: f.workspace }, body: "PRIVATE-MEMORY-FIXTURE-CONTENT",
    temporalClass: "historical", status: "active", revalidation: { mode: "on-change" }, sources: [{ kind: "fixture", ref: "native-fixture-source", origin: "external-claim", assurance: "reported", observedAt: "2026-10-08T00:00:00Z" }] };
}

test("native Memory tools describe valid query and capture payloads", () => {
  const payload = MEMORY_TOOL.parameters.properties.payload;
  assert.equal(payload.properties?.queryTerms?.type, "array", "queryTerms must be visible before the first tool call");
  assert.equal(payload.properties.queryTerms.maxItems, 8);
  assert.equal(payload.properties.limit.maximum, 20);
  assert.equal(payload.properties.sourceRef?.type, "string", "source checks must describe their workspace-file input");
  assert.match(MEMORY_TOOL.parameters.properties.command.description ?? "", /query.*stored note bodies/);
  const note = payload.properties.note;
  for (const field of ["kind", "title", "body", "scope", "temporalClass", "revalidation", "sources"]) {
    assert.ok(note.required.includes(field), `missing required note input ${field}`);
  }
  assert.ok(note.properties.sources.items.properties.origin.enum.includes("user-stated"));
  assert.match(payload.properties.procedure.description, /prepare/);
  assert.equal(payload.properties.procedure.properties.payload.properties.maxResults.maximum, 10);
  assert.equal(MEMORY_TOOL.parameters.additionalProperties, false);
  assert.equal(payload.properties.requestContext, undefined);
  assert.equal(payload.properties.root, undefined);
});

test("an unavailable source check does not hide a stored note", async t => {
  const f = await fixture(t);
  const session = f.bridge.session(f.binding);
  const captured = await session.tool({ command: "capture", payload: { note: note(f) } });
  assert.equal(captured.ok, true);
  const checked = await session.tool({ command: "source", payload: { ref: captured.data.refs[0] } });
  assert.equal(checked.ok, false);
  assert.equal(checked.reason, "SourceAssuranceUnavailable");
  assert.equal(checked.retryable, false);
  const queried = await session.tool({ command: "query", payload: { queryTerms: [note(f).title], limit: 1 } });
  assert.equal(queried.ok, true);
  assert.equal(queried.data.hits[0].note.body, note(f).body);
});

// The before_prompt_build context OpenClaw 2026.9.6 builds for an owner's
// Control UI or TUI turn: embedded-agent-runner/run/attempt-prompt-build.ts with
// buildAgentHookContextChannelFields. chat.send leaves the provenance unset, the
// channel falls back to the "webchat" provider, the internal route uses the
// session key as the target, and operator UI clients carry no sender ID.
function controlUiContext(overrides = {}) {
  return { sessionId: "web-session", sessionKey: "agent:main:main", runId: "run-1", agentId: "main", trigger: "user",
    messageProvider: "webchat", channel: "webchat", channelId: "agent:main:main", chatId: "agent:main:main", ...overrides };
}
function channelContext(provider, chatType, overrides = {}) {
  return controlUiContext({ messageProvider: provider, channel: provider, channelId: "fixture-conversation", chatId: "fixture-conversation",
    senderId: "fixture-sender", channelContext: { sender: { id: "fixture-sender" }, chat: { id: "fixture-conversation", ...(chatType ? { type: chatType } : {}) } },
    ...overrides });
}

test("OpenClaw keeps owner-statement provenance narrower than Memory access", () => {
  const cases = [
    ["Control UI or TUI turn", controlUiContext(), true],
    ["dashboard session", controlUiContext({ sessionKey: "agent:main:dashboard:fixture", channelId: "agent:main:dashboard:fixture" }), true],
    ["operator input marked as a person's input", controlUiContext({ inputProvenance: { kind: "external_user", sourceChannel: "fixture" } }), true],
    ["local CLI turn", controlUiContext({ messageProvider: "cli", channel: "cli" }), true],
    ["channel direct message a channel plugin marks as the owner",
      channelContext("slack", "direct", { channelContext: { sender: { id: "fixture-sender", isOwner: true }, chat: { id: "fixture-conversation", type: "direct" } } }), true],
    ["another Gateway client, such as the public webchat client", controlUiContext({ senderId: "webchat-ui" }), false],
    ["heartbeat", controlUiContext({ trigger: "heartbeat" }), false],
    ["cron job", controlUiContext({ trigger: "cron", jobId: "fixture-job", sessionKey: "agent:main:cron:fixture-job:run:fixture-run" }), false],
    ["memory flush", controlUiContext({ trigger: "memory" }), false],
    ["turn without a trigger", controlUiContext({ trigger: undefined }), false],
    ["inter-session message", controlUiContext({ inputProvenance: { kind: "inter_session", sourceTool: "sessions_send" } }), false],
    ["internal system input", controlUiContext({ inputProvenance: { kind: "internal_system", sourceTool: "fixture" } }), false],
    ["person's input relayed from another session", controlUiContext({ inputProvenance: { kind: "external_user", sourceSessionKey: "agent:main:other" } }), false],
    ["sub-agent session", controlUiContext({ sessionKey: "agent:main:subagent:fixture" }), false],
    ["cron session", controlUiContext({ sessionKey: "agent:main:cron:fixture-job" }), false],
    ["ACP session", controlUiContext({ sessionKey: "agent:main:acp:fixture" }), false],
    ["webchat turn routed to another channel", controlUiContext({ channel: "slack" }), false],
    ["voice transcript from a paired node", controlUiContext({ messageProvider: "node", channel: "node",
      inputProvenance: { kind: "external_user", sourceChannel: "voice", sourceTool: "gateway.voice.transcript" } }), false],
    ["channel direct message", channelContext("slack", "direct"), false],
    ["channel message without a chat type", channelContext("telegram"), false],
    ["group chat", channelContext("slack", "group"), false],
  ];
  for (const [name, ctx, expected] of cases) assert.equal(privateOrigin(ctx), expected, name);
});

test("generic Custom bridge runs the real helper, records usage, and obeys restrictions", async t => {
  const f = await fixture(t);
  const session = f.bridge.session(f.binding);
  assert.equal((await session.tool({ command: "status", payload: {} })).data.initialized, false);
  for (const command of ["prepare", "lint", "query"]) await session.tool({ command, payload: command === "query" ? { queryTerms: ["unknown"] } : {} });
  await assert.rejects(lstat(f.env.CETIRUN_MEMORY_ROOT), { code: "ENOENT" });
  const captured = await session.tool({ command: "capture", payload: { note: note(f) } }, "capture-fixture");
  assert.equal(captured.ok, true, JSON.stringify(captured));
  const queried = await session.tool({ command: "query", payload: { queryTerms: ["Private fixture marker"] } }, "lookup-fixture");
  assert.equal(queried.data.hits[0].note.body, "PRIVATE-MEMORY-FIXTURE-CONTENT");
  const trace = await session.tool({ command: "trace", payload: { ref: captured.data.refs[0] } }, "trace-fixture");
  assert.ok(trace.data.entries.some(entry => entry.relationship === "injected"));
  const restricted = f.bridge.session({ ...f.binding, noCapture: true });
  const denied = await restricted.tool({ command: "capture", payload: { note: note(f) } });
  assert.equal(denied.reason, "CaptureProhibited");
  for (const boundary of [{ audience: "shared" }, { noMemory: true }, { parentSessionId: "parent" }]) {
    const content = await f.bridge.session({ ...f.binding, ...boundary }).context();
    assert.ok(!content.includes("Private fixture marker"));
  }
  let calls = 0;
  const result = await session.tool({ command: "evaluate", payload: { ref: captured.data.refs[0] } }, "not-authorized", async () => { calls++; });
  assert.equal(result.ok, false); assert.equal(calls, 0);
  assert.equal((await loadAssets(f.assets)).manifest.adapterVersion, "memory-adapter/v1");
  assert.ok((await readFile(path.join(f.assets, "adapters/hermes/__init__.py"), "utf8")).includes("def register("));
});

test("OpenClaw hooks use native origin, refresh context, and preserve unrelated messages", async t => {
  const f = await fixture(t), hooks = new Map();
  let factory, modelCalls = 0;
  registerMemory({ runtime: { version: "2026.9.6", llm: { complete: async () => { modelCalls++; throw new Error("no model authorized"); } } },
    on: (name, fn) => hooks.set(name, fn), registerTool: registration => { assert.equal(registration.contextVersion, 2); factory = registration.create; } }, { bridge: f.bridge });
  const ctx = controlUiContext({ workspaceDir: f.workspace, modelProviderId: "fixture", modelId: "model", assertInvocationCurrent() {} });
  const messages = [{ role: "user", content: "Unrelated native history" }];
  const event = { prompt: "fixture", currentUserMessage: "fixture", currentUserMessageId: "request-1", messages };
  assert.equal(privateOrigin(ctx), true);
  await hooks.get("before_prompt_build")(event, ctx);
  const tool = factory(ctx);
  const result = await tool.execute("native-write", { command: "capture", payload: { note: note(f) } });
  assert.equal(result.details.ok, true, JSON.stringify(result.details));
  const context = await hooks.get("before_prompt_build")({ ...event, currentUserMessageId: "request-2" }, { ...ctx, runId: "run-2" });
  assert.ok(context.prependContext.includes("Private fixture marker"));
  assert.equal(`${context.prependSystemContext}\n${context.prependContext}`.split("## CetiRun private memory").length - 1, 1);
  assert.deepEqual(messages, [{ role: "user", content: "Unrelated native history" }]);
  await hooks.get("llm_input")({}, { ...ctx, runId: "run-2" });
  assert.ok(hooks.get("before_tool_call")({ toolName: "sessions_send" }, { ...ctx, runId: "run-2" }).requireApproval);
  assert.equal((await tool.execute("stale-tool", { command: "status", payload: {} })).isError, true);
  await hooks.get("before_prompt_build")({ ...event, currentUserMessage: "/memory no-capture", currentUserMessageId: "request-3" }, { ...ctx, runId: "run-3" });
  const denied = await factory({ ...ctx, runId: "run-3" }).execute("native-write-2", { command: "capture", payload: { note: note(f) } });
  assert.equal(denied.details.reason, "CaptureProhibited");
  const shared = await hooks.get("before_prompt_build")({ ...event, currentUserMessageId: "shared-request" }, { ...channelContext("slack", "group"), runId: "run-4", workspaceDir: f.workspace, sessionId: "web-session" });
  assert.ok(shared.prependContext.includes("Private fixture marker"));
  await hooks.get("agent_end")({ success: true }, { ...ctx, runId: "run-4" });
  assert.equal(modelCalls, 0);
  hooks.get("session_end")({}, ctx);
  const restored = await hooks.get("before_prompt_build")({ ...event, messages: [{ role: "user", content: "/memory off" }], currentUserMessageId: "resumed" }, { ...ctx, runId: "resumed" });
  assert.ok(!restored.prependContext.includes("Private fixture marker"));
  const api = { runtime: { version: "2026.9.6", llm: { complete() {} } }, config: { agents: { defaults: { model: "fixture/model" } }, models: { providers: { fixture: { api: "openai-completions", models: [{ id: "model" }] } } } } };
  assert.equal(evaluationRoute(api, ctx), true);
  assert.equal(evaluationRoute(api, { ...ctx, modelId: "unqualified" }), false);
  api.config.models.providers.fixture.api = "openai-responses";
  assert.equal(evaluationRoute(api, ctx), false);
});

test("the native bridge loads the common Agent context, not a second asset copy", async t => {
  const f = await fixture(t);
  const context = "Synthetic platform preamble.\n" + await readFile(path.join(f.home, ".agents/AGENTS.md"), "utf8");
  await writeFile(path.join(f.home, ".agents/AGENTS.md"), context);
  const bridge = await createMemoryBridge(f.env, { assets: await loadAssets(f.assets) });
  assert.equal(bridge.assets.canonical, context);
  const composed = await bridge.session(f.binding).context();
  assert.ok(composed.startsWith("Synthetic platform preamble."));
  assert.equal(composed.split("## CetiRun private memory").length - 1, 1);
  await rm(path.join(f.home, ".agents/AGENTS.md"));
  await assert.rejects(createMemoryBridge(f.env, { assets: await loadAssets(f.assets) }));
});

test("OpenClaw asset failures disable Memory without an unhandled rejection", async () => {
  const hooks = new Map();
  registerMemory({ on: (name, fn) => hooks.set(name, fn), registerTool() {} }, { bridge: Promise.reject(new Error("synthetic asset mismatch")) });
  assert.match((await hooks.get("before_prompt_build")({}, {})).prependSystemContext, /Memory is unavailable/);
});

async function openclawFixture(t, bridgeOverride, options = {}) {
  const f = await fixture(t), hooks = new Map();
  await f.bridge.session(f.binding).tool({ command: "capture", payload: { note: note(f) } }, "seed");
  Object.assign(f.env, options.env);
  const entries = new Map();
  let time = 1000;
  let factory;
  registerMemory({ runtime: { version: "2026.9.6", agent: { session: {
    getSessionEntry: options.getSessionEntry ?? (({ sessionKey }) => entries.get(sessionKey)),
  } } }, pluginConfig: options.config, on: (name, fn) => hooks.set(name, fn),
    registerTool: registration => { factory = registration.create; } }, { bridge: bridgeOverride?.(f.bridge) ?? f.bridge, env: f.env, now: () => time });
  const ctx = controlUiContext({ sessionId: "sdk-shape", runId: "owner-1", workspaceDir: f.workspace, assertInvocationCurrent() {} });
  entries.set(ctx.sessionKey, { sessionId: ctx.sessionId });
  return { ...f, hooks, ctx, entries, advance: () => ++time, tool: context => factory(context) };
}

test("OpenClaw embedded prompt-only owner input immediately disables Memory", async t => {
  const f = await openclawFixture(t);
  const result = await f.hooks.get("before_prompt_build")({ prompt: "/memory off", messages: [] }, f.ctx);
  assert.ok(!result.prependContext.includes("Private fixture marker"), "off leaked private navigation");
  const capture = await f.tool(f.ctx).execute("denied-write", { command: "capture", payload: { note: note(f) } });
  assert.equal(capture.details.reason, "MemoryDisabled");
});

test("OpenClaw internal restrictions do not become owner preferences", async t => {
  const f = await openclawFixture(t);
  const event = { prompt: "Synthetic owner task", messages: [] };
  await f.hooks.get("before_prompt_build")(event, f.ctx);
  const internal = await f.hooks.get("before_prompt_build")(event, { ...f.ctx, runId: "internal", inputProvenance: { kind: "internal_system", sourceTool: "fixture" } });
  assert.ok(internal.prependContext.includes("Private fixture marker"));
  const resumed = await f.hooks.get("before_prompt_build")(event, { ...f.ctx, runId: "owner-2" });
  assert.ok(resumed.prependContext.includes("Private fixture marker"), "internal turn permanently disabled owner Memory");
});

test("OpenClaw outbound guard is armed before asynchronous injection accounting", async t => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const f = await openclawFixture(t, bridge => ({ ...bridge, session(binding, signal) {
    const session = bridge.session(binding, signal);
    return { ...session, async injected() { await pending; return session.injected(); } };
  } }));
  await f.hooks.get("before_prompt_build")({ prompt: "Synthetic task", messages: [] }, f.ctx);
  const accounting = f.hooks.get("llm_input")({}, f.ctx);
  try {
    for (const toolName of ["message", "sessions_send"]) {
      assert.ok(f.hooks.get("before_tool_call")({ toolName }, f.ctx)?.requireApproval, `${toolName} raced the Memory guard`);
    }
    assert.equal(f.hooks.get("before_tool_call")({ toolName: "sessions_spawn" }, f.ctx), undefined);
  } finally { release(); await accounting; }
});

test("OpenClaw turns that cannot use Memory start no helper for context or the boundary", async t => {
  const calls = [];
  const f = await openclawFixture(t, bridge => ({ ...bridge, session(binding, signal) {
    const session = bridge.session(binding, signal);
    return { ...session,
      async ownerPreference(text) { calls.push("ownerPreference"); return session.ownerPreference(text); },
      async context(options) { calls.push("context"); return session.context(options); },
      async boundary() { calls.push("boundary"); return session.boundary(); } };
  } }), { config: { owner_access: { platforms: ["webchat"] } } });
  const group = { ...channelContext("slack", "group"), sessionId: "group-session", runId: "group-1", workspaceDir: f.workspace };
  const shared = await f.hooks.get("before_prompt_build")({ prompt: "Synthetic group question", messages: [] }, group);
  assert.match(shared.prependContext, /not available in this conversation/);
  await f.hooks.get("agent_end")({ success: true }, group);
  const off = { ...f.ctx, runId: "owner-off" };
  assert.match((await f.hooks.get("before_prompt_build")({ prompt: "/memory off", messages: [] }, off)).prependContext, /off for this conversation/);
  await f.hooks.get("agent_end")({ success: true }, off);
  assert.deepEqual(calls, []);
  const on = { ...f.ctx, runId: "owner-on" };
  assert.ok((await f.hooks.get("before_prompt_build")({ prompt: "/memory on", messages: [] }, on)).prependContext.includes("Private fixture marker"));
  await f.hooks.get("agent_end")({ success: true }, on);
  assert.deepEqual(calls, ["ownerPreference", "context", "boundary"]);
});

test("OpenClaw admits channels and automated turns to Memory by default", async t => {
  const contexts = [
    channelContext("slack", "group"), channelContext("discord", "channel"), channelContext("telegram", "direct"),
    channelContext("new-channel"), controlUiContext({ senderId: "webchat-ui" }),
    controlUiContext({ trigger: "cron", jobId: "scheduled-job", sessionKey: "agent:main:cron:scheduled-job" }),
    controlUiContext({ trigger: "heartbeat" }), controlUiContext({ trigger: "memory" }),
    controlUiContext({ sessionKey: "agent:main:acp:fixture" }),
    controlUiContext({ inputProvenance: { kind: "internal_system", sourceTool: "fixture" } }),
    controlUiContext({ inputProvenance: { kind: "inter_session", sourceTool: "sessions_send" } }),
  ];
  for (const [i, source] of contexts.entries()) {
    // Isolate sources so prior captures cannot evict the seed from bounded navigation.
    const f = await openclawFixture(t);
    const ctx = { ...source, sessionId: `admitted-${i}`, runId: `admitted-${i}`, workspaceDir: f.workspace, assertInvocationCurrent() {} };
    const result = await f.hooks.get("before_prompt_build")({ prompt: "Remember my preference: not an attested owner statement", messages: [] }, ctx);
    assert.ok(result.prependContext.includes("Private fixture marker"), JSON.stringify(source));
    const capture = await f.tool(ctx).execute(`capture-${i}`, { command: "capture", payload: { note: { ...note(f), title: `Channel fact ${i}` } } });
    assert.equal(capture.details.ok, true, JSON.stringify(capture.details));
    const query = await f.tool(ctx).execute(`query-${i}`, { command: "query", payload: { queryTerms: [`Channel fact ${i}`] } });
    assert.equal(query.details.ok, true);
    const core = await f.bridge.session(f.binding).tool({ command: "query", payload: { view: "core", queryTerms: [] } });
    assert.equal(core.data.hits.length, 0, "automated/channel input asserted an owner preference");
  }
});

test("OpenClaw intersects configured and environment access limits using native metadata", async t => {
  const f = await openclawFixture(t, undefined, {
    config: { owner_access: { platforms: ["slack", "telegram"], senders: { slack: ["allowed"] }, chats: { slack: ["allowed-chat"] }, chat_types: { slack: ["direct"] } } },
    env: { CETIRUN_MEMORY_OWNER_ACCESS: JSON.stringify({ platforms: ["slack"] }) },
  });
  const ctx = { ...channelContext("slack", "direct"), sessionId: "allowed", senderId: "allowed", chatId: "allowed-chat", workspaceDir: f.workspace };
  const prompt = { prompt: "Synthetic request", messages: [] };
  assert.ok((await f.hooks.get("before_prompt_build")(prompt, ctx)).prependContext.includes("Private fixture marker"));
  for (const [i, changes] of [
    { senderId: "someone-else" }, { chatId: "another-chat" },
    { channelContext: { chat: { type: "group" } } }, { channelContext: {} },
    { senderId: undefined, channelContext: {} }, { channel: "telegram", messageProvider: "telegram" },
  ].entries()) {
    const blocked = { ...ctx, ...changes, sessionId: `blocked-${i}` };
    const result = await f.hooks.get("before_prompt_build")({ ...prompt, owner_access: {}, senderId: "allowed" }, blocked);
    assert.match(result.prependContext, /not available in this conversation/);
  }
  const off = { ...ctx, runId: "channel-off" };
  assert.match((await f.hooks.get("before_prompt_build")({ prompt: "/memory off", messages: [] }, off)).prependContext, /off for this conversation/);
  const on = { ...ctx, runId: "channel-on" };
  assert.ok((await f.hooks.get("before_prompt_build")({ prompt: "/memory on", messages: [] }, on)).prependContext.includes("Private fixture marker"));
});

test("OpenClaw rejects malformed access settings instead of opening Memory", () => {
  for (const owner_access of [null, [], { platforms: "slack" }, { platforms: ["*"] }, { cron: "false" }, { senders: null },
    { senders: { slack: "owner" } }, { chats: { slack: [""] } }, { unknown: true }]) {
    assert.throws(() => registerMemory({ pluginConfig: { owner_access } }, { env: {} }), /MemoryOwnerAccessInvalid/);
    assert.throws(() => registerMemory({}, { env: { CETIRUN_MEMORY_OWNER_ACCESS: JSON.stringify(owner_access) } }), /MemoryOwnerAccessInvalid/);
  }
  assert.throws(() => registerMemory({}, { env: { CETIRUN_MEMORY_OWNER_ACCESS: "{" } }));
  assert.throws(() => registerMemory({}, { env: { CETIRUN_MEMORY_NO_MEMORY: "maybe" } }), /MemoryRestrictionsInvalid/);
});

test("OpenClaw startup read/write restrictions cannot be relaxed by memory on", async t => {
  for (const name of ["CETIRUN_MEMORY_NO_CAPTURE", "CETIRUN_MEMORY_NO_MEMORY"]) {
    const f = await openclawFixture(t, undefined, { env: { [name]: "true" } });
    const result = await f.hooks.get("before_prompt_build")({ prompt: "/memory on", messages: [] }, f.ctx);
    assert.equal(result.prependContext.includes("Private fixture marker"), name === "CETIRUN_MEMORY_NO_CAPTURE");
    const capture = await f.tool(f.ctx).execute("restricted-write", { command: "capture", payload: { note: note(f) } });
    assert.equal(capture.details.reason, name === "CETIRUN_MEMORY_NO_CAPTURE" ? "CaptureProhibited" : "MemoryDisabled");
  }
});

function nativeChild(f, parent, name) {
  const ctx = { ...parent, sessionId: name, sessionKey: `agent:main:subagent:${name}`, runId: `${name}-run`,
    workspaceDir: path.join(f.workspace, name) };
  f.entries.set(ctx.sessionKey, { sessionId: ctx.sessionId, spawnedBy: parent.sessionKey, parentSessionId: parent.sessionId,
    createdVia: "spawn", createdAt: f.advance() });
  return ctx;
}

test("OpenClaw native children inherit scope and transitive restrictions without gaining owner provenance", async t => {
  const f = await openclawFixture(t);
  const start = (ctx, prompt = "Synthetic task") => f.hooks.get("before_prompt_build")({ prompt, messages: [] }, ctx);
  await start(f.ctx);
  const child = nativeChild(f, f.ctx, "child"), queued = nativeChild(f, f.ctx, "queued");
  assert.ok((await start(child, "Remember my preference: child cannot attest this")).prependContext.includes("Private fixture marker"));
  const capture = await f.tool(child).execute("child-write", { command: "capture", payload: { note: note(f) } });
  assert.equal(capture.details.ok, true, JSON.stringify(capture.details));
  const grandchild = nativeChild(f, child, "grandchild");
  assert.ok((await start(grandchild)).prependContext.includes("Private fixture marker"));
  const core = await f.bridge.session(f.binding).tool({ command: "query", payload: { view: "core", queryTerms: [] } });
  assert.equal(core.data.hits.length, 0);

  await start({ ...f.ctx, runId: "parent-no-capture" }, "/memory no-capture");
  for (const original of [child, grandchild]) {
    const ctx = { ...original, runId: `${original.runId}-restricted` };
    assert.ok((await start(ctx, "/memory on")).prependContext.includes("Private fixture marker"));
    const denied = await f.tool(ctx).execute("child-restricted-write", { command: "capture", payload: { note: note(f) } });
    assert.equal(denied.details.reason, "CaptureProhibited");
  }
  await start({ ...f.ctx, runId: "parent-off" }, "/memory off");
  assert.match((await start({ ...child, runId: "child-off" }, "/memory on")).prependContext, /off for this conversation/);
  f.advance();
  await start({ ...f.ctx, runId: "parent-on" }, "/memory on");
  assert.match((await start({ ...child, runId: "child-still-off" })).prependContext, /off for this conversation/);
  assert.match((await start(queued)).prependContext, /not available in this conversation/);
  assert.ok((await start(nativeChild(f, f.ctx, "fresh-child"))).prependContext.includes("Private fixture marker"));
});

test("OpenClaw retains background child grants after completion but revokes them on interruption and reset", async t => {
  for (const action of ["interrupt", "reset", "source", "scope"]) {
    const f = await openclawFixture(t);
    const prompt = { prompt: "Synthetic task", messages: [] };
    await f.hooks.get("before_prompt_build")(prompt, f.ctx);
    const child = nativeChild(f, f.ctx, "child");
    await f.hooks.get("before_prompt_build")(prompt, child);
    await f.hooks.get("agent_end")({ success: true }, f.ctx);
    assert.ok((await f.hooks.get("before_prompt_build")(prompt, { ...child, runId: "background" })).prependContext.includes("Private fixture marker"));
    const resumed = { ...f.ctx, runId: "parent-resumed" };
    await f.hooks.get("before_prompt_build")(prompt, resumed);
    if (action === "interrupt") await f.hooks.get("agent_end")({ success: false }, resumed);
    if (action === "reset") f.hooks.get("before_reset")({}, resumed);
    if (action === "source") await f.hooks.get("before_prompt_build")(prompt, { ...resumed, runId: "changed-source", senderId: "another-sender" });
    if (action === "scope") await f.hooks.get("before_prompt_build")(prompt, { ...resumed, runId: "changed-scope", workspaceDir: path.join(f.workspace, "other-project") });
    assert.match((await f.hooks.get("before_prompt_build")(prompt, { ...child, runId: "revoked" })).prependContext, /not available in this conversation/, action);
  }
});

test("OpenClaw child access requires current native lineage and a fresh parent grant", async t => {
  const f = await openclawFixture(t);
  const prompt = { prompt: "Synthetic task", messages: [] };
  await f.hooks.get("before_prompt_build")(prompt, f.ctx);
  for (const [i, changes] of [
    { sessionId: "another-incarnation" }, { parentSessionId: "another-parent" }, { createdVia: "client" },
    { createdAt: 999 }, { createdAt: undefined }, { spawnedBy: "agent:main:unknown" },
  ].entries()) {
    const child = nativeChild(f, f.ctx, `invalid-${i}`);
    Object.assign(f.entries.get(child.sessionKey), changes);
    assert.match((await f.hooks.get("before_prompt_build")({ ...prompt, parentSessionId: f.ctx.sessionId }, child)).prependContext, /not available in this conversation/);
  }
  const missing = nativeChild(f, f.ctx, "missing");
  f.entries.delete(missing.sessionKey);
  assert.match((await f.hooks.get("before_prompt_build")(prompt, missing)).prependContext, /not available in this conversation/);
  const replacedParent = nativeChild(f, f.ctx, "replaced-parent");
  f.entries.set(f.ctx.sessionKey, { sessionId: "replacement-parent" });
  assert.match((await f.hooks.get("before_prompt_build")(prompt, replacedParent)).prependContext, /not available in this conversation/);
});

test("OpenClaw can explicitly disable child, cron and ACP access", async t => {
  const f = await openclawFixture(t, undefined, { config: { owner_access: { subagents: false, cron: false, acp: false } } });
  const prompt = { prompt: "Synthetic task", messages: [] };
  await f.hooks.get("before_prompt_build")(prompt, f.ctx);
  assert.ok(f.hooks.get("before_tool_call")({ toolName: "sessions_spawn" }, f.ctx)?.requireApproval);
  for (const ctx of [nativeChild(f, f.ctx, "disabled-child"),
    { ...f.ctx, sessionId: "cron", sessionKey: "agent:main:cron:fixture", trigger: "cron" },
    { ...f.ctx, sessionId: "acp", sessionKey: "agent:main:acp:fixture" }]) {
    assert.match((await f.hooks.get("before_prompt_build")(prompt, ctx)).prependContext, /not available in this conversation/);
  }
});

test("OpenClaw metadata read failures invalidate an existing Memory session", async t => {
  let fail = false, f;
  f = await openclawFixture(t, undefined, { getSessionEntry: ({ sessionKey }) => {
    if (fail) throw new Error("Synthetic metadata failure");
    return f.entries.get(sessionKey);
  } });
  const prompt = { prompt: "Synthetic task", messages: [] };
  await f.hooks.get("before_prompt_build")(prompt, f.ctx);
  const child = nativeChild(f, f.ctx, "child");
  await f.hooks.get("before_prompt_build")(prompt, child);
  fail = true;
  await assert.rejects(f.hooks.get("before_prompt_build")(prompt, f.ctx), /Synthetic metadata failure/);
  assert.equal((await f.tool(child).execute("after-failure", { command: "query", payload: { queryTerms: ["Private fixture"] } })).isError, true);
});

test("OpenClaw parent restrictions cancel a child's pending private-context injection", async t => {
  let release, entered;
  const pending = new Promise(resolve => { release = resolve; });
  const started = new Promise(resolve => { entered = resolve; });
  const f = await openclawFixture(t, bridge => ({ ...bridge, session(binding, signal) {
    const session = bridge.session(binding, signal);
    return { ...session, async context(options) {
      const result = await session.context(options);
      if (binding.parentSessionId) { entered(); await pending; }
      return result;
    } };
  } }));
  const prompt = { prompt: "Synthetic task", messages: [] };
  await f.hooks.get("before_prompt_build")(prompt, f.ctx);
  const child = nativeChild(f, f.ctx, "pending-child");
  const injection = f.hooks.get("before_prompt_build")(prompt, child);
  await started;
  try {
    await f.hooks.get("before_prompt_build")({ prompt: "/memory off", messages: [] }, { ...f.ctx, runId: "parent-off" });
  } finally { release(); }
  assert.equal(await injection, undefined, "private context escaped after the parent disabled Memory");
});

test("OpenClaw Memory tool cancellation revokes descendants but a settled signal does not", async t => {
  const f = await openclawFixture(t);
  const prompt = { prompt: "Synthetic task", messages: [] };
  await f.hooks.get("before_prompt_build")(prompt, f.ctx);
  const child = nativeChild(f, f.ctx, "child");
  await f.hooks.get("before_prompt_build")(prompt, child);
  const settled = new AbortController();
  assert.equal((await f.tool(f.ctx).execute("settled", { command: "status", payload: {} }, settled.signal)).isError, false);
  settled.abort();
  assert.equal((await f.tool(child).execute("still-allowed", { command: "status", payload: {} })).isError, false);
  const cancelled = new AbortController();
  cancelled.abort();
  assert.equal((await f.tool(f.ctx).execute("cancelled", { command: "status", payload: {} }, cancelled.signal)).isError, true);
  assert.equal((await f.tool(child).execute("revoked", { command: "status", payload: {} })).isError, true);
});

test("pinned Hermes host APIs load the adapter and execute bounded native calls", { skip: !process.env.MEMORY_HERMES_SOURCE }, async t => {
  const f = await fixture(t);
  assert.ok(process.env.MEMORY_HERMES_PYTHON, "select the provisioned Hermes PM interpreter");
  const output = execFileSync(process.env.MEMORY_HERMES_PYTHON, ["-B", path.join(moduleDir, "tests/hermes_host.py")], {
    env: { ...f.env, MEMORY_TEST_ASSETS: f.assets, MEMORY_TEST_WORKSPACE: f.workspace, MEMORY_HERMES_SOURCE: process.env.MEMORY_HERMES_SOURCE,
      MEMORY_TEST_PLUGIN: path.join(moduleDir, "../cetirun-chart/agents/hermes/plugins/cetirun-memory-context/__init__.py"),
      HERMES_HOME: path.join(f.home, ".hermes"), HERMES_SKIP_AUTO_UPDATE: "1", PYTHONDONTWRITEBYTECODE: "1" }, encoding: "utf8", timeout: 30000,
  });
  assert.match(output, /Pinned Hermes host checks passed/);
});

test("pinned Hermes applies access restrictions through native hooks and session metadata", { skip: !process.env.MEMORY_HERMES_SOURCE }, async t => {
  const f = await fixture(t);
  assert.ok(process.env.MEMORY_HERMES_PYTHON, "select the provisioned Hermes PM interpreter");
  const home = path.join(f.home, ".hermes");
  await mkdir(home);
  await writeFile(path.join(home, "OWNER_ACCESS_TEST_FIXTURE"), "synthetic native access fixture\n");
  const owner_access = { platforms: ["discord", "telegram"],
    senders: { discord: ["fixture-owner"], telegram: ["fixture-telegram"] },
    chats: { discord: ["fixture-chat"] }, chat_types: { discord: ["dm"] } };
  await writeFile(path.join(home, "config.yaml"), JSON.stringify({
    plugins: { entries: { "cetirun-memory-context": { settings: { owner_access } } } },
  }));
  const output = execFileSync(process.env.MEMORY_HERMES_PYTHON, ["-B", path.join(moduleDir, "tests/hermes_owner_host.py")], {
    env: { ...f.env, MEMORY_TEST_ASSETS: f.assets, MEMORY_TEST_WORKSPACE: f.workspace,
      MEMORY_HERMES_SOURCE: process.env.MEMORY_HERMES_SOURCE, HERMES_HOME: home,
      HERMES_SKIP_AUTO_UPDATE: "1", PYTHONDONTWRITEBYTECODE: "1" }, encoding: "utf8", timeout: 30000,
  });
  const result = JSON.parse(output);
  assert.equal(result.real_hooks_registry, true);
  assert.equal(result.model_requests, 0);
});

async function skillHost(source, runtime, changed) {
  const target = await mkdtemp(path.join(stage, `${runtime}-skills-`));
  async function populate(relative = "") {
    const directory = path.join(target, relative);
    await mkdir(directory, { recursive: true });
    for (const name of await readdir(path.join(source, relative))) {
      const file = path.join(relative, name), original = path.join(source, file), destination = path.join(target, file);
      if (changed.includes(file)) await copyFile(original, destination);
      else if (changed.some(entry => entry.startsWith(`${file}/`))) await populate(file);
      else await symlink(original, destination);
    }
  }
  await populate();
  const installer = path.resolve(moduleDir, "../cetirun-chart/configs/apply-memory-skills.py");
  const args = [installer, "--runtime", runtime, "--root", target];
  execFileSync("python3", [...args, "--check"]);
  execFileSync("python3", args);
  assert.match(execFileSync("python3", args, { encoding: "utf8" }), /0 file\(s\) updated/);
  return target;
}

test("pinned Hermes protects Memory discovery through live shared-skill changes", { skip: !process.env.MEMORY_HERMES_SOURCE }, async () => {
  const host = await skillHost(process.env.MEMORY_HERMES_SOURCE, "hermes", ["agent/skill_utils.py", "tools/skills_tool.py", "agent/prompt_builder.py"]);
  const output = execFileSync(process.env.MEMORY_HERMES_PYTHON ?? "python3", ["-B", path.join(moduleDir, "tests/hermes_skills.py")], {
    env: { ...process.env, MEMORY_HERMES_SOURCE: host, PYTHONDONTWRITEBYTECODE: "1" }, encoding: "utf8", timeout: 30000,
  });
  assert.match(output, /Pinned Hermes live skill-source checks passed/);
});

test("pinned OpenClaw protects Memory discovery through live shared-skill changes", { skip: !process.env.MEMORY_OPENCLAW_SOURCE }, async () => {
  const host = await skillHost(process.env.MEMORY_OPENCLAW_SOURCE, "openclaw", ["dist/workspace-skill-loader-CoQ1pd8t.mjs"]);
  const output = execFileSync(process.execPath, [path.join(moduleDir, "tests/openclaw_skills.mjs")], {
    env: { ...process.env, MEMORY_OPENCLAW_SOURCE: host }, encoding: "utf8", timeout: 30000,
  });
  assert.match(output, /Pinned OpenClaw live skill-source checks passed/);
});

test("pinned OpenClaw loads the packaged plugin and bounds direct native requests", { skip: !process.env.MEMORY_OPENCLAW_SOURCE }, async t => {
  const f = await fixture(t);
  const output = execFileSync(process.execPath, [path.join(moduleDir, "tests/openclaw_host.mjs")], {
    env: { ...f.env, MEMORY_TEST_ASSETS: f.assets, MEMORY_TEST_WORKSPACE: f.workspace, MEMORY_OPENCLAW_SOURCE: process.env.MEMORY_OPENCLAW_SOURCE,
      OPENCLAW_STATE_DIR: path.join(f.home, ".openclaw"), OPENCLAW_CONFIG_PATH: path.join(f.home, ".openclaw/config.json"), OPENCLAW_NO_RESPAWN: "1" },
    encoding: "utf8", timeout: 45000,
  });
  assert.match(output, /Pinned OpenClaw host checks passed/);
});

test("pinned Pi SDK executes the real memory tool and isolated evaluator", { skip: process.env.MEMORY_PI_NATIVE !== "true" }, async t => {
  const f = await fixture(t);
  const output = execFileSync(process.execPath, [path.join(moduleDir, "tests/pi_host.mjs")], {
    cwd: path.resolve(moduleDir, "../.."), env: { ...f.env, MEMORY_TEST_ASSETS: f.assets, MEMORY_TEST_WORKSPACE: f.workspace }, encoding: "utf8", timeout: 30000,
  });
  assert.match(output, /Pinned Pi SDK and real helper checks passed/);
});

test("pinned Pi SDK tightens steering and follow-up inputs and propagates cancellation", { skip: process.env.MEMORY_PI_NATIVE !== "true" }, async t => {
  const f = await fixture(t);
  const output = execFileSync(process.execPath, [path.join(moduleDir, "tests/pi_lifecycle.mjs")], {
    cwd: path.resolve(moduleDir, "../.."), env: { ...f.env, MEMORY_TEST_ASSETS: f.assets, MEMORY_TEST_WORKSPACE: f.workspace }, encoding: "utf8", timeout: 30000,
  });
  assert.match(output, /Pinned Pi lifecycle checks passed/);
});

test("Hermes adapter uses the same helper through request middleware and native tools", async t => {
  const f = await fixture(t);
  const output = execFileSync("python3", ["-B", path.join(moduleDir, "tests/hermes_adapter.py")], {
    env: { ...f.env, MEMORY_TEST_ASSETS: f.assets, MEMORY_TEST_WORKSPACE: f.workspace,
      MEMORY_TEST_PLUGIN: path.join(moduleDir, "../cetirun-chart/agents/hermes/plugins/cetirun-memory-context/__init__.py"),
      PYTHONDONTWRITEBYTECODE: "1" }, encoding: "utf8",
  });
  assert.match(output, /Hermes native adapter checks passed/);
});

test("Custom stdio exposes fixed tools and narrows per-request capture without sampling", async t => {
  const f = await fixture(t);
  const child = spawn(helper, ["mcp", "--stdio", "--owner-private"], { cwd: f.workspace, env: f.env, stdio: ["pipe", "pipe", "pipe"] });
  t.after(() => child.kill());
  let id = 0, errors = "";
  child.stderr.on("data", chunk => { errors += chunk; });
  const pending = new Map();
  createInterface({ input: child.stdout }).on("line", line => { const result = JSON.parse(line); pending.get(result.id)?.(result); pending.delete(result.id); });
  const call = (method, params) => new Promise(resolve => { const requestId = ++id; pending.set(requestId, resolve); child.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: requestId, method, params }) + "\n"); });
  const initialized = await call("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "synthetic-custom", version: "1" } });
  assert.equal(initialized.result.capabilities.sampling, undefined);
  assert.equal((await call("tools/list", {})).result.tools[0].name, "cetirun_memory");
  const params = { name: "cetirun_memory", arguments: { command: "capture", payload: { note: note(f) } } };
  let result = await call("tools/call", params);
  assert.equal(JSON.parse(result.result.content[0].text).ok, true);
  result = await call("tools/call", { ...params, _meta: { "cetirun/noCapture": true } });
  assert.equal(JSON.parse(result.result.content[0].text).reason, "CaptureProhibited");
  result = await call("tools/call", { name: "cetirun_memory", arguments: { command: "query", payload: { queryTerms: ["Private fixture marker"] } } });
  assert.equal(JSON.parse(result.result.content[0].text).data.hits[0].note.body, "PRIVATE-MEMORY-FIXTURE-CONTENT");
  assert.equal(errors, "");
  child.stdin.end();
  await new Promise(resolve => child.once("exit", resolve));
});
