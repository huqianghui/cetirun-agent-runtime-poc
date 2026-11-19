// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const GROUP = "CetiRun AI Gateway";
const STRING = '"(?:[^"\\\\]|\\\\[\\s\\S])*"';
const COMMENTS = '/\\*[\\s\\S]*?\\*/|//[^\\r\\n]*';

function gatewayConfiguration(env) {
  const base = env.OPENAI_BASE_URL;
  const key = env.OPENAI_API_KEY;
  if (!base && !key) return undefined;
  if (!base || !key || /[\x00-\x20\x7f]/.test(key)) throw new Error("Invalid Gateway binding");
  let url;
  try { url = new URL(base); } catch { throw new Error("Invalid Gateway binding"); }
  if (!["http:", "https:"].includes(url.protocol) || url.username || url.password ||
      url.search || url.hash || !/^\/v1\/?$/.test(url.pathname)) throw new Error("Invalid Gateway binding");
  return { baseURL: url.href.replace(/\/$/, ""), apiKey: key };
}

async function discoverModels(config, fetcher = fetch) {
  const response = await fetcher(config.baseURL + "/models", {
    headers: { Authorization: "Bearer " + config.apiKey, Accept: "application/json" },
    redirect: "error", signal: AbortSignal.timeout(10000),
  });
  if (!response.ok) {
    await response.body?.cancel();
    throw new Error("Gateway model discovery failed");
  }
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > 256 * 1024) throw new Error("Gateway catalog is too large");
      chunks.push(value);
    }
  } finally { await reader.cancel(); }
  const catalog = JSON.parse(Buffer.concat(chunks).toString("utf8"));
  if (!Array.isArray(catalog.data) || catalog.data.length > 256) throw new Error("Invalid Gateway catalog");
  const ids = catalog.data.map(model => model?.id);
  if (ids.some(id => typeof id !== "string" || !id.trim() || Buffer.byteLength(id) > 256 ||
      /[\x00-\x1f\x7f]/.test(id)) || new Set(ids).size !== ids.length) throw new Error("Invalid Gateway catalog");
  return ids.filter(id => id !== "cetirun-shared" &&
    !/^cetirun-copilot-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(id)).sort();
}

function providerGroup(config, ids) {
  return { name: GROUP, vendor: "customendpoint", apiKey: config.apiKey, apiType: "chat-completions",
    models: ids.map(id => ({ id, name: id, url: config.baseURL + "/chat/completions",
      toolCalling: true, vision: false, maxInputTokens: 114688, maxOutputTokens: 16384,
      modelOptions: { temperature: null, top_p: null } })) };
}

// Keep string literals intact while accepting VS Code's comments and trailing commas.
function parseGroups(text) {
  const withoutComments = text.replace(new RegExp(`${STRING}|${COMMENTS}`, "g"),
    token => token.startsWith('"') ? token : " ");
  const json = withoutComments.replace(new RegExp(`${STRING}|,\\s*(?=[}\\]])`, "g"),
    token => token.startsWith('"') ? token : "");
  const groups = JSON.parse(json);
  if (!Array.isArray(groups) || groups.some(group => !group || typeof group !== "object" ||
      Array.isArray(group) || typeof group.name !== "string" || typeof group.vendor !== "string")) {
    throw new Error("Invalid language model configuration");
  }
  return groups;
}

function mergeGatewayGroup(text, group) {
  const groups = parseGroups(text);
  const spans = [];
  let depth = 0, start, arrayStart;
  for (const token of text.matchAll(new RegExp(`${STRING}|${COMMENTS}|[{}\\[\\]]`, "g"))) {
    const value = token[0];
    if (value === "[" || value === "{") {
      if (depth === 0) arrayStart = token.index + 1;
      if (depth === 1) start = token.index;
      depth++;
    } else if (value === "]" || value === "}") {
      depth--;
      if (depth === 1) spans.push({ start, end: token.index + 1 });
    }
  }
  const index = groups.findIndex(item => item.name === GROUP && item.vendor === "customendpoint");
  const rendered = JSON.stringify(group, null, 2);
  if (index >= 0) return text.slice(0, spans[index].start) + rendered + text.slice(spans[index].end);
  const at = spans.at(-1)?.end ?? arrayStart;
  return text.slice(0, at) + (groups.length ? ",\n" : "\n") + rendered + "\n" + text.slice(at);
}

module.exports = { GROUP, gatewayConfiguration, discoverModels, providerGroup, parseGroups, mergeGatewayGroup };
