// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { readFile } from "node:fs/promises";
import path from "node:path";
import { createHash, randomUUID } from "node:crypto";
import { createExtensionRuntime, createSyntheticSourceInfo, loadSkillsFromDir } from "@earendil-works/pi-coding-agent";
import { memoryResources, memoryBridge } from "./memory-runtime.mjs";
import { connectionsTools } from "./connections-runtime.mjs";

export function memoryModelRevision(model) {
  const identity = { provider: model?.provider, model: model?.id, api: model?.api, baseUrl: model?.baseUrl,
    contextWindow: model?.contextWindow, maxTokens: model?.maxTokens, reasoning: model?.reasoning };
  return `sha256:${createHash("sha256").update(JSON.stringify(identity)).digest("hex")}`;
}

/** Load only the platform's read-only context, without global/project plugins. */
export async function approvedResources(home, env = process.env, cwd = path.join(home, "workspace"), platform = "pi-http", loadMemory = memoryResources, options = {}) {
  const root = path.join(home, ".agents");
  const canonical = path.join(root, "AGENTS.md");
  const content = await readFile(canonical, "utf8");
  const skills = loadSkillsFromDir({ dir: path.join(root, "skills"), source: "cetirun" });
  if (!content.trim() || !skills.skills.some((skill) => skill.name === "cetirun-memory")) {
    throw new Error("Canonical context and cetirun-memory are required");
  }
  const memory = await loadMemory(home, env);
  const extensions = { extensions: [], errors: [], runtime: createExtensionRuntime() };
  const connections = connectionsTools(env);
  if (connections.length) {
    const extensionPath = "/etc/cetirun/connections";
    const sourceInfo = createSyntheticSourceInfo(extensionPath, { source: "cetirun-connections", scope: "temporary" });
    extensions.extensions.push({
      path: extensionPath, resolvedPath: extensionPath, sourceInfo,
      handlers: new Map(), tools: new Map(connections.map(definition => [definition.name, { sourceInfo, definition }])),
      messageRenderers: new Map(), commands: new Map(), flags: new Map(), shortcuts: new Map(),
    });
  }
  let modelRuntime = options.modelRuntime;
  if (memory) {
    const extensionPath = path.join(memory.root, "pi-context");
    const { bridge, tool, restrictiveMode, restoredRestrictions } = await memoryBridge(memory, home, env);
    let ownerInput = false;
    let preference;
    let ownerText;
    let controls = { noCapture: !!options.noCapture, noMemory: !!options.noMemory };
    let current, runController;
    const start = (ctx, activeBinding) => {
      if (!current) {
        const history = ctx.sessionManager.getBranch?.().map(entry => entry.message).filter(Boolean) ?? [];
        controls = restoredRestrictions(history, controls);
        if (ownerInput) controls = restrictiveMode(ownerText, controls);
      }
      runController?.abort(); runController = new AbortController();
      const controller = runController;
      current = bridge.session({ entrypoint: platform, nativeVersion: "0.87.1", cwd: ctx.cwd,
        sessionId: ctx.sessionManager.getSessionId(), opportunityId: randomUUID(),
        parentSessionId: ctx.sessionManager.getHeader()?.parentSession ?? "",
        audience: ownerInput ? "owner-private" : "unknown", destination: { kind: ownerInput ? "owner-private" : "unknown" },
        model: ctx.model ? `${ctx.model.provider}/${ctx.model.id}` : "", modelEvaluation: !!modelRuntime && ctx.model?.api === "openai-completions",
        modelRevision: memoryModelRevision(ctx.model),
        ...activeBinding,
        ...controls, noCapture: controls.noCapture || !!options.noCapture, noMemory: controls.noMemory || !!options.noMemory,
      }, () => AbortSignal.any([controller.signal, ctx.signal, options.signal].filter(Boolean)));
    };
    const sourceInfo = createSyntheticSourceInfo(extensionPath, { source: "cetirun-memory", scope: "temporary" });
    extensions.extensions.push({
      path: extensionPath, resolvedPath: extensionPath,
      sourceInfo,
      handlers: new Map([
        ["input", [(event, ctx) => {
          ownerInput = ["interactive", "rpc"].includes(event.source);
          ownerText = ownerInput ? event.text : undefined;
          preference = ownerInput && /^(remember my preference:|记住我的偏好：)/i.test(event.text) ? event.text : undefined;
          if (ownerInput) controls = restrictiveMode(event.text, controls);
          if (event.streamingBehavior && current) {
            // Queued input does not run before_agent_start. Revoke in-flight
            // helper calls now, without upgrading the active audience.
            start(ctx, { ...current.binding, ...(ownerInput ? {} : { audience: "unknown", destination: { kind: "unknown" } }) });
            preference = undefined;
          }
        }]],
        ["before_agent_start", [async (event, ctx) => {
          const explicit = preference;
          preference = undefined;
          start(ctx);
          if (explicit && explicit === event.prompt) {
            await current.ownerPreference(explicit);
          }
        }]],
        ["context", [async (event, ctx) => {
          if (!current) start(ctx);
          let active = current;
          let content = await active.context({ includeCanonical: false });
          if (active !== current) {
            active = current;
            content = await active.context({ includeCanonical: false });
          }
          const messages = event.messages.filter(message => message.role !== "custom" || message.customType !== "cetirun-memory-context");
          if (active !== current || ctx.signal?.aborted || options.signal?.aborted || runController.signal.aborted) return { messages };
          await active.injected();
          if (active !== current || ctx.signal?.aborted || options.signal?.aborted || runController.signal.aborted) return { messages };
          return {
          // Dynamic private context is rebuilt for the request, not appended
          // to native history or frozen in the session's system prompt.
          messages: [
            ...messages,
            {
              role: "custom", customType: "cetirun-memory-context", display: false, timestamp: Date.now(),
              content,
            },
          ],
          };
        }]],
        ["tool_execution_end", [async event => {
          if (event.isError && current) await current.feedback(event.toolName, "failure", event.toolCallId);
        }]],
        ["agent_end", [async event => {
          const last = event.messages?.findLast(message => message.role === "assistant");
          if (current && !["error", "aborted"].includes(last?.stopReason)) await current.boundary();
          runController?.abort();
        }]],
        ["session_shutdown", [() => { runController?.abort(); current = undefined; }]],
      ]),
      tools: new Map([[tool.name, { sourceInfo, definition: { ...tool, label: "Private memory",
        async execute(id, params, signal, _update, ctx) {
          if (!current) return { content: [{ type: "text", text: "Memory context unavailable" }], details: { ok: false } };
          const cancel = () => runController?.abort();
          signal?.addEventListener("abort", cancel, { once: true });
          try {
            if (signal?.aborted) cancel();
            const result = await current.tool(params, id, modelRuntime && (async (ticket, abortSignal) => {
            const result = await modelRuntime.completeSimple(ctx.model, { systemPrompt: ticket.system,
              messages: [{ role: "user", content: ticket.prompt, timestamp: Date.now() }], tools: [] },
            { signal: AbortSignal.any([abortSignal, signal, AbortSignal.timeout(ticket.timeoutSeconds * 1000)].filter(Boolean)), maxTokens: ticket.maxTokens, maxRetries: 0, temperature: 0,
              onPayload: payload => ({ ...payload, metadata: { ...payload.metadata, tags: ["cetirun-memory-evaluation", "artifact-contract-v1"] } }) });
            return { output: result.content.filter(part => part.type === "text").map(part => part.text).join(""),
              outcome: result.stopReason === "stop" ? "completed" : "unknown", model: `${result.provider}/${result.model}`, requests: 1, tokens: result.usage?.totalTokens ?? null };
          }));
            return { content: [{ type: "text", text: JSON.stringify(result) }], details: result };
          } finally { signal?.removeEventListener("abort", cancel); }
        },
      } }]]), messageRenderers: new Map(), commands: new Map(), flags: new Map(), shortcuts: new Map(),
    });
  }
  return {
    memoryEnabled: !!memory,
    connectionTools: connections.map(tool => tool.name),
    setMemoryModelRuntime: value => { modelRuntime = value; },
    getExtensions: () => extensions,
    getSkills: () => skills,
    getPrompts: () => ({ prompts: [], diagnostics: [] }),
    getThemes: () => ({ themes: [], diagnostics: [] }),
    getAgentsFiles: () => ({ agentsFiles: [{ path: canonical, content }] }),
    getSystemPrompt: () => undefined,
    getSystemPromptSource: () => undefined,
    getAppendSystemPrompt: () => [],
    getAppendSystemPromptSources: () => [],
    extendResources: () => {},
    reload: async () => {},
  };
}
