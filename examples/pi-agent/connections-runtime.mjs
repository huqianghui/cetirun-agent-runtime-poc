// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { readFile } from "node:fs/promises";
import { request } from "node:https";

const PROTOCOL = "2025-11-25";
const LIMIT = 1024 * 1024;
const EMPTY = { type: "object", properties: {}, additionalProperties: false };
const REQUEST = {
  type: "object", additionalProperties: false, required: ["connection_id", "service", "method", "path"],
  properties: {
    connection_id: { type: "string", minLength: 1, maxLength: 128 },
    service: { type: "string" },
    method: { type: "string", enum: ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"] },
    path: { type: "string", minLength: 1, maxLength: 8192 },
    query: { type: "object", additionalProperties: { oneOf: [{ type: "string" }, { type: "array", items: { type: "string" } }] } },
    headers: { type: "object", additionalProperties: { type: "string" }, description: "Only If-Match, If-None-Match, Prefer, ConsistencyLevel and X-GitHub-Api-Version." },
    body: { description: "Optional JSON body; no more than 256 KiB. Not supported for GET or HEAD." },
  },
};

export function connectionsConfiguration(env) {
  const url = env.CETIRUN_CONNECTIONS_MCP_URL;
  const tokenFile = env.CETIRUN_CONNECTIONS_MCP_TOKEN_FILE;
  const caFile = env.CETIRUN_CONNECTIONS_MCP_CA_FILE;
  if (!url && !tokenFile && !caFile) return undefined;
  const endpoint = new URL(url);
  if (!tokenFile || !caFile || endpoint.protocol !== "https:" ||
    !/^[a-z0-9-]+\.[a-z0-9-]+\.svc\.cluster\.local$/.test(endpoint.hostname) ||
    endpoint.pathname !== "/mcp" || endpoint.username || endpoint.password || endpoint.search || endpoint.hash ||
    endpoint.port === "0") {
    throw new Error("Incomplete or invalid governed Connections configuration");
  }
  return { url: endpoint.href, tokenFile, caFile };
}

export function connectionsTools(env = process.env, transport = postJSON) {
  const config = connectionsConfiguration(env);
  if (!config) return [];
  let nextID = 0;
  const rpc = async (method, params, signal, notification = false) => {
    signal = AbortSignal.any([signal, AbortSignal.timeout(30000)].filter(Boolean));
    signal?.throwIfAborted();
    // Read rotating proof and trust for every request, never capture them in
    // an extension definition, a tool result, or a retained Agent session.
    const proof = (await readFile(config.tokenFile, "utf8")).trim();
    const ca = await readFile(config.caFile);
    if (!/^[A-Za-z0-9._~-]{1,16384}$/.test(proof) || ca.length === 0 || ca.length > 65536 || ca.includes("PRIVATE KEY")) {
      throw new Error("Connections workload proof or public trust is invalid");
    }
    const id = notification ? undefined : ++nextID;
    const body = JSON.stringify({ jsonrpc: "2.0", id, method, params });
    if (Buffer.byteLength(body) > 320 * 1024) throw new Error("Connections request exceeds its bound");
    const response = await transport({ ...config, proof, ca, body, signal });
    if (notification) {
      if (response.status !== 202 || response.body) throw new Error("Connections initialization was rejected");
      return;
    }
    if (response.status !== 200 || Buffer.byteLength(response.body) > LIMIT || response.body.includes(proof)) {
      throw new Error("Connections response unavailable; an attempted operation may have an unknown outcome");
    }
    const value = JSON.parse(response.body);
    if (value.jsonrpc !== "2.0" || value.id !== id || value.error || !value.result) {
      throw new Error("Connections protocol request failed; do not automatically replay operations");
    }
    return value.result;
  };
  return [
    ["connections_status", "List this Agent's explicitly bound connections and status; never credentials.", EMPTY],
    ["connections_start_link", "Get the Portal link for owner connection management.", EMPTY],
    ["services_list", "Discover enabled services and their fixed provider base paths.", EMPTY],
    ["service_request", "Execute one JSON REST request through a bound Connection. Provider permissions apply. Never use an absolute URL or replay an execution_unknown result; inspect provider state first.", REQUEST],
  ].map(([name, description, parameters]) => ({
    name, label: name, description, parameters,
    async execute(_id, args, signal) {
      const initialized = await rpc("initialize", {
        protocolVersion: PROTOCOL, capabilities: {}, clientInfo: { name: "cetirun-pi", version: "1" },
      }, signal);
      if (initialized.protocolVersion !== PROTOCOL) throw new Error("Unsupported Connections protocol");
      await rpc("notifications/initialized", undefined, signal, true);
      const result = await rpc("tools/call", { name, arguments: args }, signal);
      if (result.isError || !Array.isArray(result.content) ||
        !result.content.every(item => item.type === "text" && typeof item.text === "string")) {
        throw new Error("Connections execution failed; check authorization and operation state before retrying");
      }
      return { content: result.content, details: result.structuredContent ?? {} };
    },
  }));
}

function postJSON({ url, proof, ca, body, signal }) {
  return new Promise((resolve, reject) => {
    const fail = () => reject(new Error("Connections transport failed; the operation outcome may be unknown"));
    const req = request(url, {
      method: "POST", agent: false, ca, minVersion: "TLSv1.2", rejectUnauthorized: true, signal,
      maxHeaderSize: 16384,
      headers: { Authorization: `Bearer ${proof}`, "Content-Type": "application/json",
        Accept: "application/json, text/event-stream", "MCP-Protocol-Version": PROTOCOL },
    }, response => {
      const chunks = [];
      let bytes = 0;
      response.on("data", chunk => {
        bytes += chunk.length;
        if (bytes > LIMIT) req.destroy(new Error("Connections response exceeded its bound"));
        else chunks.push(chunk);
      });
      response.on("error", fail);
      response.on("end", () => resolve({ status: response.statusCode, body: Buffer.concat(chunks).toString("utf8") }));
    });
    req.setTimeout(30000, () => req.destroy(new Error("Connections request timed out")));
    req.on("error", fail);
    req.end(body);
  });
}
