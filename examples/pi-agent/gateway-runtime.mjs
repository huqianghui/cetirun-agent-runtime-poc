// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { InMemoryCredentialStore } from "@earendil-works/pi-ai";
import { streamSimple } from "@earendil-works/pi-ai/compat";

export const GATEWAY_PROVIDER = "cetirun-gateway";

export function gatewayConfiguration(env = process.env) {
  const base = env.OPENAI_BASE_URL;
  const key = env.OPENAI_API_KEY;
  if (!base && !key) return undefined;
  if (!base || !key || /[\r\n]/.test(key)) throw new Error("Both Gateway environment variables are required");
  const url = new URL(base);
  if (!["http:", "https:"].includes(url.protocol) || url.username || url.password || url.search || url.hash ||
    !/^\/v1\/?$/.test(url.pathname)) throw new Error("Invalid Gateway API base");
  return { baseURL: url.href.replace(/\/$/, ""), apiKey: key };
}

export function gatewayCredentials() {
  return new InMemoryCredentialStore();
}

// No arbitrary provider URLs, persistent credentials or model-response logging.
export function gatewayAccess(modelRuntime, config) {
  const allowed = new URL(config.baseURL);
  const boundedFetch = (input, init = {}) => {
    const url = new URL(input instanceof Request ? input.url : input);
    if (url.origin !== allowed.origin || !["/v1/models", "/v1/chat/completions"].includes(url.pathname) || url.search) {
      throw new Error("Gateway request left the approved API");
    }
    return fetch(input, { ...init, redirect: "error" });
  };
  const register = (ids) => modelRuntime.registerProvider(GATEWAY_PROVIDER, {
    name: "CetiRun AI Gateway", baseUrl: config.baseURL, api: "openai-completions",
    models: ids.map((id) => ({
      id, name: id, reasoning: false, input: ["text"],
      contextWindow: 32768, maxTokens: 2048,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      compat: { supportsStore: false },
    })),
    streamSimple: (model, context, options) => streamSimple(model, context, {
      ...options, apiKey: config.apiKey, maxRetries: 0, fetch: boundedFetch,
    }),
  });
  register([]);
  return {
    async models(signal) {
      const response = await boundedFetch(config.baseURL + "/models", {
        headers: { Authorization: "Bearer " + config.apiKey, Accept: "application/json" }, signal,
      });
      if (!response.ok) {
        await response.body?.cancel();
        throw new Error("Gateway model discovery is unavailable");
      }
      const reader = response.body.getReader();
      const chunks = [];
      let bytes = 0;
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          bytes += value.byteLength;
          if (bytes > 256 * 1024) throw new Error("Gateway catalog is too large");
          chunks.push(value);
        }
      } finally {
        await reader.cancel();
      }
      const catalog = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      if (!Array.isArray(catalog.data) || catalog.data.length > 256) throw new Error("Invalid Gateway catalog");
      let ids = catalog.data.map((model) => model?.id);
      if (ids.some((id) => typeof id !== "string" || !id.trim() || Buffer.byteLength(id) > 256 || /[\r\n]/.test(id)) || new Set(ids).size !== ids.length) {
        throw new Error("Invalid Gateway model identifier");
      }
      // LiteLLM expands populated access groups, but lists a grant literally
      // while its group has no deployments. Those grants are not model IDs.
      ids = ids.filter((id) => id !== "cetirun-shared" &&
        !/^cetirun-copilot-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(id));
      register(ids);
      await modelRuntime.setRuntimeApiKey(GATEWAY_PROVIDER, config.apiKey, { signal });
      return modelRuntime.getAvailable(GATEWAY_PROVIDER, { signal });
    },
  };
}
