// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { createMemoryBridge, digest, MEMORY_TOOL, restrictiveMode, restoredRestrictions } from "../common/bridge.mjs";

// Owner-statement provenance is narrower than permission to use Memory.
const OWNER_SURFACES = new Set(["webchat", "cli"]);

function personInput(provenance) {
  return !provenance || provenance.kind === "external_user" && !provenance.originSessionId &&
    !provenance.sourceSessionKey && !provenance.sourceRole && !provenance.jobId;
}

function delegatedSession(sessionKey) {
  const rest = /^agent:[^:]*:(.*)$/i.exec(sessionKey ?? "")?.[1]?.toLowerCase() ?? "";
  return rest.startsWith("cron:") || rest.startsWith("acp:") || /(^|:)subagent:/.test(rest);
}

export function privateOrigin(ctx) {
  if (ctx.trigger !== "user" || ctx.jobId || !personInput(ctx.inputProvenance) || delegatedSession(ctx.sessionKey)) return false;
  const chat = ctx.channelContext?.chat;
  if (["group", "channel", "room"].includes(chat?.type)) return false;
  if (OWNER_SURFACES.has(ctx.messageProvider) && (!ctx.channel || ctx.channel === ctx.messageProvider) && !ctx.senderId) return true;
  return chat?.type === "direct" && ctx.channelContext?.sender?.isOwner === true;
}

function accessConfiguration(api, env) {
  const policies = [
    Object.hasOwn(api.pluginConfig ?? {}, "owner_access") ? api.pluginConfig.owner_access : {},
    JSON.parse(env.CETIRUN_MEMORY_OWNER_ACCESS || "{}"),
  ];
  const isObject = value => value !== null && typeof value === "object" && !Array.isArray(value);
  const isIdentifier = (value, limit) => typeof value === "string" && value.length > 0 && value.length <= limit &&
    value.trim() === value && !value.includes("*") && !/[\x00-\x1f]/.test(value);
  const isIdentifierList = (values, limit) => Array.isArray(values) && values.length <= 32 && values.every(value => isIdentifier(value, limit));
  for (const policy of policies) {
    if (!isObject(policy)) throw new Error("MemoryOwnerAccessInvalid");
    if (Object.keys(policy).some(key => !["platforms", "senders", "chats", "chat_types", "subagents", "cron", "acp"].includes(key))) {
      throw new Error("MemoryOwnerAccessInvalid");
    }
    if (["subagents", "cron", "acp"].some(key => Object.hasOwn(policy, key) && typeof policy[key] !== "boolean")) {
      throw new Error("MemoryOwnerAccessInvalid");
    }
    if (Object.hasOwn(policy, "platforms") && !isIdentifierList(policy.platforms, 64)) {
      throw new Error("MemoryOwnerAccessInvalid");
    }
    for (const key of ["senders", "chats", "chat_types"]) {
      const limits = Object.hasOwn(policy, key) ? policy[key] : {};
      if (!isObject(limits) || Object.keys(limits).length > 32) throw new Error("MemoryOwnerAccessInvalid");
      if (Object.entries(limits).some(([platform, values]) => !isIdentifier(platform, 64) || !isIdentifierList(values, 256))) {
        throw new Error("MemoryOwnerAccessInvalid");
      }
    }
  }
  const readRestriction = name => {
    const value = (env[name] ?? "").toLowerCase();
    if (!["", "false", "true", "0", "1"].includes(value)) throw new Error("MemoryRestrictionsInvalid");
    return value === "true" || value === "1";
  };
  return {
    policies,
    noCapture: readRestriction("CETIRUN_MEMORY_NO_CAPTURE"),
    noMemory: readRestriction("CETIRUN_MEMORY_NO_MEMORY"),
  };
}

function sessionSource(ctx) {
  let platform;
  if (ctx.jobId || ctx.trigger === "cron" || /:cron:/.test(ctx.sessionKey ?? "")) {
    platform = "cron";
  } else if (/:acp:/.test(ctx.sessionKey ?? "")) {
    platform = "acp";
  } else if (["heartbeat", "memory"].includes(ctx.trigger)) {
    platform = ctx.trigger;
  } else {
    platform = ctx.channel || ctx.messageProvider || ctx.trigger || "";
  }
  return {
    platform,
    sender: ctx.senderId ?? ctx.channelContext?.sender?.id ?? "",
    chat: ctx.chatId ?? ctx.channelContext?.chat?.id ?? ctx.channelId ?? "",
    chatType: ctx.channelContext?.chat?.type ?? "",
    account: ctx.accountId ?? "",
  };
}

function sourceAllowed(origin, policies) {
  if (!origin.platform) return false;
  return policies.every(policy => {
    if (policy.platforms && !policy.platforms.includes(origin.platform)) return false;
    if (policy[origin.platform] === false) return false;
    return [["senders", origin.sender], ["chats", origin.chat], ["chat_types", origin.chatType]].every(([key, value]) =>
      !Object.hasOwn(policy[key] ?? {}, origin.platform) || policy[key][origin.platform].includes(value));
  });
}

const NOT_ALLOWED = "CetiRun Memory is not available in this conversation. Do not read or save private memory, or claim to have done so.";
const TURNED_OFF = "CetiRun Memory is off for this conversation. Do not read or save private memory, or claim to have done so.";

// The pinned direct chat-completions transport disables SDK retries. Other
// transports can replay or switch transport, so they are not qualified here.
export function evaluationRoute(api, ctx) {
  if (api.runtime.version !== "2026.9.6" || typeof api.runtime.llm?.complete !== "function") return false;
  const config = api.config;
  const agent = config?.agents?.list?.find(item => item.id === ctx.agentId);
  const configured = agent?.model ?? config?.agents?.defaults?.model;
  const primary = typeof configured === "string" ? configured : configured?.primary;
  const provider = config?.models?.providers?.[ctx.modelProviderId];
  const model = provider?.models?.find(item => item.id === ctx.modelId);
  return primary === `${ctx.modelProviderId}/${ctx.modelId}` && !!model && (model.api ?? provider.api) === "openai-completions";
}

function modelRevision(api, ctx) {
  const provider = api.config?.models?.providers?.[ctx.modelProviderId];
  const model = provider?.models?.find(item => item.id === ctx.modelId);
  return digest(JSON.stringify({ provider: ctx.modelProviderId, model: ctx.modelId, api: model?.api ?? provider?.api,
    baseUrl: provider?.baseUrl, contextWindow: model?.contextWindow, maxTokens: model?.maxTokens, reasoning: model?.reasoning }));
}

export function registerMemory(api, options = {}) {
  const env = options.env ?? process.env;
  const access = accessConfiguration(api, env);
  const now = options.now ?? Date.now;
  const allowChildren = access.policies.every(policy => policy.subagents !== false);
  const bridgePromise = (options.bridge ? Promise.resolve(options.bridge) : createMemoryBridge(env)).catch(() => null);
  const sessions = new Map();
  const grants = new Map();
  const key = ctx => ctx.sessionId ?? ctx.sessionKey;
  const grantKey = ctx => ctx.sessionKey ?? ctx.sessionId;
  const entry = sessionKey => api.runtime?.agent?.session?.getSessionEntry?.({ sessionKey, readConsistency: "latest" });
  function refreshGrant(grant) {
    if (grant.parent) {
      refreshGrant(grant.parent);
      grant.allowed &&= grant.parent.allowed;
      grant.controls.noCapture ||= grant.parent.controls.noCapture;
      grant.controls.noMemory ||= grant.parent.controls.noMemory;
    }
    return grant;
  }
  function refreshSessions() {
    for (const session of sessions.values()) {
      const grant = refreshGrant(session.grant);
      const binding = session.memory.binding;
      if (!grant.allowed || (grant.controls.noMemory && !binding.noMemory) || (grant.controls.noCapture && !binding.noCapture)) {
        session.controller.abort();
      }
      Object.assign(binding, grant.controls, { audience: grant.allowed ? "owner-private" : "unknown" });
    }
  }
  function revoke(grant) {
    if (grant) grant.allowed = false;
    refreshSessions();
  }
  function prune() {
    while (sessions.size > 128) {
      const id = sessions.keys().next().value;
      const session = sessions.get(id);
      session.controller.abort();
      sessions.delete(id);
    }
    while (grants.size > 128) {
      const id = grants.keys().next().value;
      revoke(grants.get(id));
      grants.delete(id);
    }
  }
  // Turns that cannot use Memory get a fixed notice without starting the helper.
  function turnContext(session) {
    if (!session.grant.allowed) return NOT_ALLOWED;
    if (session.memory.binding.noMemory) return TURNED_OFF;
    return session.memory.context({ includeCanonical: false });
  }
  api.on("before_prompt_build", async (event, ctx) => {
    const bridge = await bridgePromise;
    if (!bridge) return { prependSystemContext: "CetiRun Memory is unavailable. Do not claim private memory was read or saved, or bypass the missing native binding." };
    const id = key(ctx);
    if (!id || !ctx.workspaceDir) return { prependSystemContext: bridge.assets.canonical };
    const previous = sessions.get(id);
    const identity = grantKey(ctx);
    const origin = sessionSource(ctx);
    const principal = JSON.stringify(origin);
    const readEntry = sessionKey => {
      try {
        return entry(sessionKey);
      } catch (error) {
        previous?.controller.abort();
        revoke(grants.get(identity));
        throw error;
      }
    };
    const native = ctx.sessionKey ? readEntry(ctx.sessionKey) : undefined;
    const isChild = Boolean(native?.spawnedBy) || /(^|:)subagent:/.test(ctx.sessionKey ?? "");
    const oldGrant = grants.get(identity);
    const priorControls = previous?.controls ?? restoredRestrictions(event.messages);
    const inputText = event.currentUserMessage ?? event.prompt ?? "";
    const opportunity = event.currentUserMessageId ?? ctx.runId ?? digest(inputText);
    const isDirectInput = ctx.trigger === "user" && personInput(ctx.inputProvenance) && !isChild && !ctx.jobId && origin.platform !== "cron";
    const controls = isDirectInput ? restrictiveMode(inputText, priorControls) : { ...priorControls };
    controls.noCapture = Boolean(controls.noCapture || access.noCapture);
    controls.noMemory = Boolean(controls.noMemory || access.noMemory);
    let grant;
    if (isChild) {
      const parent = grants.get(native?.spawnedBy);
      const parentEntry = parent ? readEntry(native.spawnedBy) : undefined;
      const lineageMatches = allowChildren && parent && refreshGrant(parent).allowed && native.sessionId === id &&
        native.createdVia === "spawn" && parentEntry?.sessionId === parent.sessionId &&
        (!native.parentSessionId || native.parentSessionId === parent.sessionId);
      if (oldGrant?.parent && oldGrant.sessionId === id) {
        grant = oldGrant;
        grant.allowed &&= Boolean(lineageMatches && grant.parent === parent);
      } else {
        if (oldGrant) revoke(oldGrant);
        // Native creation time fences orphaned or queued children from a later
        // parent grant after reload, cancellation, or an explicit opt-in.
        const canInherit = lineageMatches && Number.isFinite(native.createdAt) && native.createdAt >= parent.authorizedAt;
        grant = {
          sessionId: id,
          allowed: Boolean(canInherit),
          parent: canInherit ? parent : null,
          cwd: canInherit ? parent.cwd : ctx.workspaceDir,
          controls: { ...controls },
          authorizedAt: now(),
        };
      }
      grant.controls.noCapture ||= controls.noCapture;
      grant.controls.noMemory ||= controls.noMemory;
      refreshGrant(grant);
    } else {
      const allowed = sourceAllowed(origin, access.policies);
      if (oldGrant?.allowed && !oldGrant.parent && oldGrant.sessionId === id && oldGrant.principal === principal && oldGrant.cwd === ctx.workspaceDir) {
        grant = oldGrant;
        if ((grant.controls.noMemory && !controls.noMemory) || (grant.controls.noCapture && !controls.noCapture)) {
          grant.authorizedAt = now();
        }
        grant.allowed = allowed;
        grant.controls = { ...controls };
      } else {
        if (oldGrant) revoke(oldGrant);
        grant = { sessionId: id, principal, allowed, cwd: ctx.workspaceDir, controls: { ...controls }, authorizedAt: now() };
      }
    }
    grants.set(identity, grant);
    refreshSessions();
    if (previous?.runId === ctx.runId && previous?.opportunity === opportunity && previous.grant === grant && previous.inputText === inputText && !previous.controller.signal.aborted) {
      const context = await turnContext(previous);
      ctx.hookInvocation?.assertActive();
      if (previous.controller.signal.aborted || sessions.get(id) !== previous) return;
      previous.injected ||= grant.allowed && !previous.memory.binding.noMemory;
      return { prependSystemContext: bridge.assets.canonical, prependContext: context };
    }
    previous?.controller.abort();
    const controller = new AbortController();
    const memory = bridge.session({
      entrypoint: "openclaw", nativeVersion: api.runtime.version,
      sessionId: id, opportunityId: opportunity,
      cwd: grant.cwd, audience: grant.allowed ? "owner-private" : "unknown",
      destination: { kind: grant.allowed ? "owner-private" : "unknown" },
      ...grant.controls, ...(grant.parent ? { parentSessionId: grant.parent.sessionId, ownerAuthorizedChild: true } : {}),
      model: `${ctx.modelProviderId ?? ""}/${ctx.modelId ?? ""}`,
      modelRevision: modelRevision(api, ctx),
      modelEvaluation: evaluationRoute(api, ctx),
    }, controller.signal);
    const session = { memory, controls, grant, controller, runId: ctx.runId, opportunity, inputText,
      injected: previous?.runId === ctx.runId && previous.injected || false };
    sessions.set(id, session);
    prune();
    if (grant.allowed && privateOrigin(ctx) && !isChild && !memory.binding.noMemory && !memory.binding.noCapture) {
      await memory.ownerPreference(inputText);
    }
    const context = await turnContext(session);
    ctx.hookInvocation?.assertActive();
    if (controller.signal.aborted || sessions.get(id) !== session) return;
    session.injected ||= grant.allowed && !memory.binding.noMemory;
    return { prependSystemContext: bridge.assets.canonical, prependContext: context };
  });
  api.on("llm_input", async (_event, ctx) => {
    refreshSessions();
    const session = sessions.get(key(ctx));
    if (session && session.runId === ctx.runId && !session.controller.signal.aborted) await session.memory.injected();
  });
  api.registerTool({ contextVersion: 2, create: ctx => ({
    ...MEMORY_TOOL, label: "Private memory",
    async execute(id, params, signal) {
      ctx.assertInvocationCurrent();
      refreshSessions();
      const session = sessions.get(key(ctx));
      if (!session || session.controller.signal.aborted || ctx.runId && session.runId !== ctx.runId) return { content: [{ type: "text", text: "Memory session unavailable" }], isError: true };
      const cancel = () => {
        session.controller.abort();
        revoke(session.grant);
      };
      signal?.addEventListener("abort", cancel, { once: true });
      try {
        if (signal?.aborted) cancel();
        const result = await session.memory.tool(params, id, async (ticket, abortSignal) => {
          ctx.assertInvocationCurrent();
          const result = await api.runtime.llm.complete({
            systemPrompt: ticket.system, messages: [{ role: "user", content: ticket.prompt }],
            maxTokens: ticket.maxTokens, temperature: 0,
            signal: AbortSignal.any([abortSignal, AbortSignal.timeout(ticket.timeoutSeconds * 1000)]),
            purpose: "cetirun-memory/artifact-contract-v1",
          });
          const model = `${result.provider}/${result.model}`;
          return { output: result.text, outcome: result.stopReason === "stop" ? "completed" : "unknown", model, requests: 1, tokens: result.usage?.totalTokens ?? null };
        });
        return { content: [{ type: "text", text: JSON.stringify(result) }], details: result, isError: !result.ok };
      } finally { signal?.removeEventListener("abort", cancel); }
    },
  }) }, { name: MEMORY_TOOL.name });
  api.on("after_tool_call", async (event, ctx) => {
    refreshSessions();
    const session = sessions.get(key(ctx));
    if (event.error && session && session.runId === ctx.runId && !session.controller.signal.aborted) await session.memory.feedback(event.toolName, "failure", event.toolCallId ?? ctx.toolCallId);
  });
  api.on("before_tool_call", (event, ctx) => {
    refreshSessions();
    const session = sessions.get(key(ctx));
    if (!session?.injected || session.runId !== ctx.runId || !["message", "sessions_send", "sessions_spawn"].includes(event.toolName)) return;
    if (event.toolName === "sessions_spawn" && allowChildren && session.grant.allowed && !session.memory.binding.noMemory) return;
    return { requireApproval: { title: "Check private memory before sharing", description: "This turn received Agent-private context. Confirm the exact outgoing task or message and its recipient; share only the minimum authorized content. Memory access remains subject to source and inherited restrictions.", severity: "warning", timeoutMs: 60000 } };
  });
  api.on("agent_end", async (event, ctx) => {
    refreshSessions();
    const session = sessions.get(key(ctx));
    if (!session || session.runId !== ctx.runId) return;
    const { noCapture, noMemory } = session.memory.binding;
    if (!event.success) revoke(session.grant);
    if (event.success && session.grant.allowed && !session.controller.signal.aborted && !noCapture && !noMemory) await session.memory.boundary();
    session.controller.abort();
  });
  for (const hook of ["before_reset", "session_end"]) api.on(hook, (_event, ctx) => {
    const id = key(ctx);
    const identity = grantKey(ctx);
    sessions.get(id)?.controller.abort();
    sessions.delete(id);
    revoke(grants.get(identity));
    grants.delete(identity);
  });
}

export default { id: "cetirun-memory-context", name: "CetiRun private memory", register: registerMemory };
