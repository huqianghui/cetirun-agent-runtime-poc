// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { mkdir } from "node:fs/promises";
import path from "node:path";
import {
  createAgentSession, ModelRuntime, SessionManager, SettingsManager,
} from "@earendil-works/pi-coding-agent";
import { approvedResources } from "./pi-resources.mjs";
import { gatewayAccess, gatewayConfiguration, gatewayCredentials, GATEWAY_PROVIDER } from "./gateway-runtime.mjs";
import { RunError } from "./run-error.mjs";

export const PROVIDER = "github-copilot";
export const PI_TOOLS = ["read", "bash", "edit", "write", "grep", "find", "ls"];
export function piTools(resources) {
  return [...PI_TOOLS, ...(resources.memoryEnabled ? ["cetirun_memory"] : []), ...(resources.connectionTools ?? [])];
}
const HTTP_TOOL_BUDGET = 8;

/** Shared inputs for HTTP and TUI; neither loads arbitrary HOME extensions. */
export async function createPiServices(home, cwd = path.join(home, "workspace"), env = process.env, platform = "pi-http") {
  const agentDir = path.join(home, ".pi", "agent");
  await mkdir(agentDir, { recursive: true, mode: 0o700 });
  await mkdir(cwd, { recursive: true, mode: 0o700 });
  const resourceLoader = await approvedResources(home, env, cwd, platform);
  const gateway = gatewayConfiguration(env);
  const modelRuntime = await ModelRuntime.create({
    ...(gateway ? { credentials: gatewayCredentials() } : { authPath: path.join(agentDir, "auth.json") }),
    modelsPath: null,
    allowModelNetwork: false,
    refreshOnCreate: false,
  });
  const settingsManager = SettingsManager.inMemory({
    defaultProvider: gateway ? GATEWAY_PROVIDER : PROVIDER,
    retry: { enabled: false },
    enableInstallTelemetry: false,
  });
  const modelAccess = gateway ? gatewayAccess(modelRuntime, gateway) : undefined;
  resourceLoader.setMemoryModelRuntime?.(modelRuntime);
  return { cwd, agentDir, modelRuntime, resourceLoader, settingsManager, modelAccess, diagnostics: [] };
}

/** Adapt the real Pi SDK to agent-api.mjs and the optional local auth helper. */
export async function createPiRuntime(home, env = process.env) {
  const services = await createPiServices(home, path.join(home, "workspace"), env);
  const { cwd, agentDir, modelRuntime, resourceLoader } = services;

  async function availableModels(signal) {
    if (services.modelAccess) return services.modelAccess.models(signal);
    await modelRuntime.refresh({ allowNetwork: false, providers: [PROVIDER], signal });
    return modelRuntime.getAvailable(PROVIDER, { signal });
  }

  return {
    gateway: !!services.modelAccess,
    provider: services.modelAccess ? GATEWAY_PROVIDER : PROVIDER,
    skills: () => resourceLoader.getSkills().skills.map(({ name }) => name),
    async authenticated() {
      if (services.modelAccess) return false;
      return (await modelRuntime.listCredentials()).some((entry) => entry.providerId === PROVIDER);
    },
    async models(signal) {
      return (await availableModels(signal)).map(({ id }) => id);
    },
    async login(signal, notify) {
      if (services.modelAccess) throw new Error("Sign in through the Portal's AI Gateway settings");
      await modelRuntime.login(PROVIDER, "oauth", {
        signal,
        prompt: async () => "",
        notify: (event) => {
          if (event.type !== "device_code") return;
          const url = new URL(event.verificationUri);
          if (url.protocol !== "https:" || url.hostname !== "github.com") {
            throw new Error("Unexpected GitHub verification URL");
          }
          // Return only the user-facing device flow, never the stored credential.
          notify({ state: "awaiting-device", userCode: event.userCode, verificationUri: url.href });
        },
      });
    },
    async run({ model: id, messages, signal, onText, memory }) {
      const model = (await availableModels(signal)).find((candidate) => candidate.id === id);
      if (!model) throw new Error("Requested model is unavailable");

      const { sessionManager, prompt, systemPrompt } = replayHTTPHistory(cwd, model, messages);
      const resources = await approvedResources(home, env, cwd, "pi-http", undefined, { ...memory, signal, modelRuntime });
      const { session } = await createAgentSession({
        cwd, agentDir, model, modelRuntime, sessionManager,
        resourceLoader: { ...resources, getAppendSystemPrompt: () => systemPrompt },
        tools: piTools(resources),
        settingsManager: SettingsManager.inMemory({
          compaction: { enabled: false },
          retry: { enabled: false },
          enableInstallTelemetry: false,
        }),
      });
      try {
        return await runHTTPPrompt(session, prompt, signal, onText);
      } finally {
        session.dispose();
      }
    },
  };
}

// HTTP callers own their history. Do not append it to a native TUI session.
function replayHTTPHistory(cwd, model, messages) {
  const sessionManager = SessionManager.inMemory(cwd);
  const systemPrompt = messages.filter((message) => message.role === "system").map((message) => message.content);
  const conversation = messages.filter((message) => message.role !== "system");
  for (const message of conversation.slice(0, -1)) {
    if (message.role === "user") {
      sessionManager.appendMessage({ role: "user", content: message.content, timestamp: Date.now() });
    } else {
      sessionManager.appendMessage({
        role: "assistant",
        content: [{ type: "text", text: message.content }],
        api: model.api, provider: model.provider, model: model.id,
        stopReason: "stop", timestamp: Date.now(),
        usage: {
          input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0,
          cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
        },
      });
    }
  }
  return { sessionManager, systemPrompt, prompt: conversation.at(-1).content };
}

async function runHTTPPrompt(session, prompt, signal, onText) {
  let toolCalls = 0;
  let limitExceeded = false;
  const abort = () => { void session.abort().catch(() => {}); };
  signal.addEventListener("abort", abort);
  const unsubscribe = session.subscribe((event) => {
    if (event.type === "tool_execution_start" && ++toolCalls > HTTP_TOOL_BUDGET) {
      limitExceeded = true;
      abort();
    }
    if (event.type === "message_update" && event.assistantMessageEvent.type === "text_delta") {
      onText?.(event.assistantMessageEvent.delta);
    }
  });
  try {
    signal.throwIfAborted();
    await session.prompt(prompt);
    signal.throwIfAborted();
    if (limitExceeded) throw new RunError("tool_budget_exceeded");
    const last = session.messages.findLast((message) => message.role === "assistant");
    if (!last || !["stop", "length"].includes(last.stopReason)) throw new RunError("run_failed");
    return { text: session.getLastAssistantText() ?? "", toolCalls, finishReason: last.stopReason };
  } finally {
    signal.removeEventListener("abort", abort);
    unsubscribe();
  }
}
