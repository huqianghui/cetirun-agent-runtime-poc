// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { readFileSync } from "node:fs";

export function workloadFetch(fetchFn, endpoint) {
  const target = process.env.CETIRUN_CONNECTIONS_MCP_URL;
  if (!target || endpoint !== target) return fetchFn;
  const expected = new URL(target);
  if (expected.protocol !== "https:" || expected.pathname !== "/mcp" || expected.username || expected.password || expected.search || expected.hash) {
    throw new Error("Invalid platform MCP endpoint");
  }
  return async (input, init = {}) => {
    if (String(input instanceof Request ? input.url : input) !== target) {
      throw new Error("Workload identity cannot leave the platform MCP endpoint");
    }
    let token;
    try {
      const raw = readFileSync("/var/run/cetirun/connections/token");
      if (raw.length > 65536) throw new Error();
      token = raw.toString("ascii").trim();
      const parts = token.split(".");
      if (parts.length !== 3 || /\s/.test(token)) throw new Error();
      const claims = JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
      if (typeof claims.exp !== "number" || claims.exp <= Date.now() / 1000) throw new Error();
    } catch {
      throw new Error("Workload identity is unavailable");
    }
    const headers = new Headers(init.headers ?? (input instanceof Request ? input.headers : undefined));
    headers.set("authorization", `Bearer ${token}`);
    const response = await fetchFn(input, { ...init, headers, redirect: "error" });
    if (response.status >= 300 && response.status < 400) throw new Error("Platform MCP redirects are forbidden");
    return response;
  };
}
