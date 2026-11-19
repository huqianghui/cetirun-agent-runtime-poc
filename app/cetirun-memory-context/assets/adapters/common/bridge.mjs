// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { spawn } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { lstat, readFile } from "node:fs/promises";
import path from "node:path";
import payloadSchema from "./payload-schema.json" with { type: "json" };

export const ADAPTER_VERSION = "memory-adapter/v1";
export const MEMORY_TOOL = {
  name: "cetirun_memory",
  description: "Read or update this Agent's private memory with the current request restrictions. Look up what earlier conversations established before answering; save facts, preferences, decisions and your own recommendations worth keeping, and correct notes that turn out wrong. At a safe boundary prepare observations and propose at most one scoped method. Source assurance, current authorization, and shared skills cannot be changed by this tool. Use status for revisions and capabilities; writes can conflict. Read the cetirun-memory skill for payload formats.",
  parameters: {
    type: "object", additionalProperties: false,
    properties: {
      command: { type: "string", description: "Use query with queryTerms to read stored note bodies. source only checks a recorded workspace file against its digest; it requires ref and sourceRef and does not read a note. Do not repeat a source check that returns SourceAssuranceUnavailable.", enum: ["status", "query", "capture", "prepare", "commit", "propose", "check", "advance", "select", "retire", "revise", "record-source-check", "record-use", "trace", "lint", "source", "evaluate"] },
      payload: payloadSchema,
    }, required: ["command", "payload"],
  },
};

const unavailable = (reason = "MemoryUnavailable") => ({ ok: false, reason, data: null });
export function digest(value) { return `sha256:${createHash("sha256").update(value).digest("hex")}`; }

export async function loadAssets(root) {
  const manifestFile = path.join(root, "manifest.json");
  const info = await lstat(manifestFile);
  if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1 || info.size > 256 * 1024) throw new Error("MemoryManifestInvalid");
  const manifest = JSON.parse(await readFile(manifestFile, "utf8"));
  if (manifest.contractVersion !== "memory-context/v1" || manifest.helperABI !== 1 || !Array.isArray(manifest.files) || manifest.files.length > 512) throw new Error("MemoryContractUnsupported");
  const seen = new Set();
  let total = 0;
  for (const file of manifest.files) {
    if (typeof file.path !== "string" || file.path.startsWith("/") || file.path.includes("\\") || file.path.split("/").some(part => !part || part === "." || part === "..") || seen.has(file.path)) throw new Error("MemoryManifestInvalid");
    seen.add(file.path);
    let name = root;
    for (const part of file.path.split("/")) {
      name = path.join(name, part);
      const stat = await lstat(name);
      if (stat.isSymbolicLink()) throw new Error("MemoryAssetUnsafe");
    }
    const stat = await lstat(name);
    total += stat.size;
    if (!stat.isFile() || stat.nlink !== 1 || total > 32 * 1024 * 1024 || ![0o444, 0o555].includes(file.mode)) throw new Error("MemoryAssetUnsafe");
    if (digest(await readFile(name)) !== file.digest) throw new Error("MemoryAssetMismatch");
  }
  for (const name of ["AGENTS.md", "skills/cetirun-memory/SKILL.md", "bin/cetirun-memory"]) if (!seen.has(name)) throw new Error("MemoryAssetMissing");
  const canonical = await readFile(path.join(root, "AGENTS.md"), "utf8");
  if (Buffer.byteLength(canonical) > 2048) throw new Error("MemoryContractTooLarge");
  return { root, manifest, canonical };
}

export async function loadAgentContext(home) {
  const directory = path.join(home, ".agents");
  if (!(await lstat(directory)).isDirectory()) throw new Error("AgentContextUnavailable");
  const file = path.join(directory, "AGENTS.md");
  const info = await lstat(file);
  if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1 || info.size > 32768) throw new Error("AgentContextUnavailable");
  return readFile(file, "utf8");
}

export function invoke(helper, event, env, signal) {
  if (signal?.aborted) return Promise.resolve(unavailable("Cancelled"));
  const input = JSON.stringify(event);
  if (Buffer.byteLength(input) > 128 * 1024) return Promise.resolve(unavailable("LimitReached"));
  return new Promise(resolve => {
    const child = spawn(helper, ["native-event"], { env, stdio: ["pipe", "pipe", "pipe"] });
    let bytes = 0, output = "", expired = false;
    const abort = () => { expired = true; child.kill("SIGTERM"); };
    const timer = setTimeout(() => { expired = true; child.kill("SIGKILL"); }, 5000);
    const finish = result => { clearTimeout(timer); signal?.removeEventListener("abort", abort); resolve(result); };
    signal?.addEventListener("abort", abort, { once: true });
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", chunk => {
      bytes += Buffer.byteLength(chunk);
      if (bytes > 256 * 1024) { expired = true; child.kill("SIGKILL"); }
      else output += chunk;
    });
    child.stderr.resume();
    child.stdin.on("error", () => {});
    child.on("error", () => finish(unavailable()));
    child.on("close", code => {
      try {
        const result = JSON.parse(output);
        if (expired || typeof result.ok !== "boolean" || (code === 0) !== result.ok) throw new Error();
        finish(result);
      } catch { finish(unavailable(expired ? "CancelledOrTimedOut" : "MemoryUnavailable")); }
    });
    if (signal?.aborted) abort();
    child.stdin.end(input);
  });
}

export async function createMemoryBridge(env = process.env, options = {}) {
  const root = env.CETIRUN_MEMORY_ASSETS;
  if (!root) return null;
  if (!options.assets && root !== "/opt/cetirun-memory") throw new Error("MemoryBindingInvalid");
  if (!env.HOME || env.CETIRUN_MEMORY_ROOT !== path.join(env.HOME, ".cetirun/memory")) throw new Error("MemoryBindingInvalid");
  const verified = options.assets ?? await loadAssets(root);
  const assets = { ...verified, canonical: await loadAgentContext(env.HOME) };
  const call = options.invoke ?? invoke;
  return {
    assets,
    session(binding, signal) {
      let checks = [], injected = [], counter = 0;
      const currentSignal = () => typeof signal === "function" ? signal() : signal;
      const bound = { adapter: ADAPTER_VERSION, noCapture: false, noMemory: false, evaluation: false, modelEvaluation: false, destination: { kind: "unknown" }, ...binding };
      bound.modelEvaluation = bound.modelEvaluation && /^sha256:[a-f0-9]{64}$/.test(bound.modelRevision ?? "");
      async function dispatch(event, payload = {}, command = "", eventID) {
        return call(path.join(assets.root, "bin/cetirun-memory"), { binding: { ...bound, currentSourceChecks: checks }, eventId: eventID ?? `${bound.opportunityId}-${++counter}`, event, command, payload }, env, currentSignal());
      }
      return {
        binding: bound,
        async ownerPreference(text) {
          if (/^(remember my preference:|记住我的偏好：)/i.test(text ?? "")) return dispatch("owner-preference", { message: text });
        },
        async context({ includeCanonical = true } = {}) {
          const result = await dispatch("context");
          injected = result.ok ? result.data?.refs ?? [] : [];
          const text = result.ok ? result.data.context : `CetiRun Memory: ${result.reason}. Do not access or claim to have saved private memory.`;
          const dynamic = text.startsWith(verified.canonical) ? text.slice(verified.canonical.length).trimStart() : text;
          return includeCanonical ? `${assets.canonical}\n${dynamic}` : dynamic;
        },
        async injected() {
          if (injected.length && !bound.noCapture && !bound.noMemory) {
            const refs = injected; injected = [];
            return dispatch("injected", { refs }, "", `${bound.opportunityId}-injected-${digest(JSON.stringify(refs))}`);
          }
        },
        async tool(params, id = randomUUID(), executor) {
          if (!params || Object.keys(params).some(key => !["command", "payload"].includes(key)) || !MEMORY_TOOL.parameters.properties.command.enum.includes(params.command) || !params.payload || typeof params.payload !== "object" || Array.isArray(params.payload)) return unavailable("InvalidInput");
          if (params.command === "evaluate") return this.evaluate(params.payload, executor, id);
          const payload = structuredClone(params.payload);
          if (params.command === "propose" && payload.procedure?.kind === "playbook") {
            payload.procedure.compatibility = { helperABI: "1", adapter: ADAPTER_VERSION, nativeVersion: bound.nativeVersion, model: bound.model, ...(bound.modelRevision ? { modelRevision: bound.modelRevision } : {}) };
          }
          const result = await dispatch("tool", payload, params.command, id);
          if (params.command === "source" && result.ok) checks = [...checks.filter(item => item.check.sourceRef !== result.data.check.sourceRef || item.check.targetRef.id !== result.data.check.targetRef.id), result.data].slice(-8);
          if (params.command === "query" && result.ok && !bound.noCapture) {
            const refs = (result.data?.hits ?? []).map(hit => hit.ref);
            if (refs.length) await dispatch("injected", { refs }, "", `${id}-result`);
          }
          return result;
        },
        async feedback(tool, category, id) {
          if (tool === MEMORY_TOOL.name || bound.noCapture || bound.noMemory || !["failure", "cancelled", "permission-denied", "unknown"].includes(category)) return;
          return dispatch("feedback", { tool, category }, "", id);
        },
        async boundary() { return dispatch("boundary"); },
        async evaluate(payload, executor, id = randomUUID()) {
          const activeSignal = currentSignal();
          if (!executor || !bound.modelEvaluation || activeSignal?.aborted) return unavailable("EvaluationUnavailable");
          const next = await dispatch("evaluation-next", payload, "", id);
          if (!next.ok) return next;
          const ticket = next.data;
          if (!ticket?.call) return next;
          let result;
          try {
            activeSignal?.throwIfAborted();
            result = await executor(ticket, activeSignal);
          } catch {
            result = { output: "", outcome: activeSignal?.aborted ? "cancelled" : "unknown", model: bound.model, requests: 1 };
          }
          // Cancellation cannot authorize a late write. The reservation already
          // accounts for the unknown outcome when recording is prohibited.
          if (activeSignal?.aborted) return unavailable("Cancelled");
          return dispatch("evaluation-result", { id: ticket.call.id, ...result }, "", `${id}-result`);
        },
      };
    },
  };
}

export function restrictiveMode(text, prior = {}) {
  const command = typeof text === "string" ? /^\/memory (off|no-capture|on)\s*$/.exec(text.trim()) : null;
  if (!command) return { ...prior };
  return { noMemory: command[1] === "off", noCapture: command[1] !== "on" };
}

// Native history may only restore restrictions. Only the current owner input
// can opt back in; no Memory write is needed to remember a no-capture session.
export function restoredRestrictions(messages = [], prior = {}) {
  const result = { ...prior };
  for (const message of messages) {
    if (message?.role !== "user") continue;
    const text = typeof message.content === "string" ? message.content :
      Array.isArray(message.content) ? message.content.filter(part => part.type === "text").map(part => part.text).join("") : "";
    const restored = restrictiveMode(text);
    if (restored.noMemory) result.noMemory = true;
    if (restored.noCapture) result.noCapture = true;
  }
  return result;
}
