// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { createHash } from "node:crypto";
import { readFileSync, writeFileSync, copyFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = process.argv[2];
if (!root) throw new Error("Provide the pinned OpenClaw package root");
const path = join(root, "dist/mcp-transport-DoEl0EB8.mjs");
const prefix = 'import { workloadFetch } from "./cetirun-workload-mcp.mjs";\n';
const before = "fetch: httpFetch\n";
const after = "fetch: workloadFetch(httpFetch, resolved.url)\n";
const guardBefore = "\tconst baseFetch = buildMcpHttpFetch({";
const guardAfter = '\tif (resolved.url === process.env.CETIRUN_CONNECTIONS_MCP_URL && (resolved.sslVerify === false || resolved.transportType !== "streamable-http")) throw new Error("Platform MCP requires verified TLS and Streamable HTTP");\n' + guardBefore;
const source = readFileSync(path, "utf8");
const original = source.startsWith(prefix) ? source.slice(prefix.length).replace(after, before).replace(guardAfter, guardBefore) : source;
if (createHash("sha256").update(original).digest("hex") !== "a26c67e52216040d4deb81ede67d357b7a3b6e4313626d8a7b1dc285f9224c46") {
  throw new Error("Unsupported OpenClaw MCP source; review the pinned integration");
}
if (original.split(before).length !== 2 || original.split(guardBefore).length !== 2) throw new Error("OpenClaw MCP integration point changed");
const fetchPath = join(root, "dist/mcp-http-fetch-CXlFHx86.mjs");
const fetchBefore = "maxRedirects: MCP_HTTP_MAX_REDIRECTS,";
const fetchAfter = 'maxRedirects: request.init?.redirect === "error" ? 0 : MCP_HTTP_MAX_REDIRECTS,';
const fetchSource = readFileSync(fetchPath, "utf8");
const originalFetch = fetchSource.replace(fetchAfter, fetchBefore);
if (createHash("sha256").update(originalFetch).digest("hex") !== "a2655222d39a4bd5060d140d1451642bdfe5ccba1a141a34d5a6c58bba6e221c") {
  throw new Error("Unsupported OpenClaw guarded fetch source");
}
if (!process.argv.includes("--check")) {
  writeFileSync(fetchPath, originalFetch.replace(fetchBefore, fetchAfter));
  copyFileSync(join(dirname(fileURLToPath(import.meta.url)), "workload-mcp.mjs"), join(root, "dist/cetirun-workload-mcp.mjs"));
  writeFileSync(path, prefix + original.replace(before, after).replace(guardBefore, guardAfter));
}
