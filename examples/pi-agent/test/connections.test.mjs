// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { connectionsConfiguration, connectionsTools } from "../connections-runtime.mjs";

async function fixture(t) {
  const directory = await mkdtemp(path.join(os.tmpdir(), "cetirun-connections-test-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const env = {
    CETIRUN_CONNECTIONS_MCP_URL: "https://connections.tools.svc.cluster.local/mcp",
    CETIRUN_CONNECTIONS_MCP_TOKEN_FILE: path.join(directory, "proof"),
    CETIRUN_CONNECTIONS_MCP_CA_FILE: path.join(directory, "ca.crt"),
  };
  await writeFile(env.CETIRUN_CONNECTIONS_MCP_TOKEN_FILE, "synthetic-proof");
  await writeFile(env.CETIRUN_CONNECTIONS_MCP_CA_FILE, "synthetic-public-ca");
  return env;
}

test("Connections is opt-in and rejects partial or external configuration", () => {
  assert.deepEqual(connectionsTools({}), []);
  const complete = {
    CETIRUN_CONNECTIONS_MCP_URL: "https://connections.tools.svc.cluster.local/mcp",
    CETIRUN_CONNECTIONS_MCP_TOKEN_FILE: "/synthetic/proof",
    CETIRUN_CONNECTIONS_MCP_CA_FILE: "/synthetic/ca.crt",
  };
  assert.equal(connectionsConfiguration(complete).url, complete.CETIRUN_CONNECTIONS_MCP_URL);
  for (const field of Object.keys(complete)) {
    assert.throws(() => connectionsConfiguration({ ...complete, [field]: undefined }));
  }
  for (const url of ["http://connections.tools.svc.cluster.local/mcp", "https://example.com/mcp",
    "https://connections.tools.svc.cluster.local:0/mcp",
    "https://connections.tools.svc.cluster.local/mcp?token=x", "https://user@connections.tools.svc.cluster.local/mcp"]) {
    assert.throws(() => connectionsConfiguration({ ...complete, CETIRUN_CONNECTIONS_MCP_URL: url }));
  }
});

test("the client initializes lazily, rereads rotating proof, and returns only the tool result", async t => {
  const env = await fixture(t);
  const calls = [];
  const tools = connectionsTools(env, async request => {
    const message = JSON.parse(request.body);
    calls.push({ message, proof: request.proof });
    if (message.method === "initialize") {
      await writeFile(env.CETIRUN_CONNECTIONS_MCP_TOKEN_FILE, "rotated-proof");
      return { status: 200, body: JSON.stringify({ jsonrpc: "2.0", id: message.id, result: { protocolVersion: "2025-11-25" } }) };
    }
    if (message.method === "notifications/initialized") return { status: 202, body: "" };
    return { status: 200, body: JSON.stringify({ jsonrpc: "2.0", id: message.id, result: { content: [{ type: "text", text: '{"status":200}' }] } }) };
  });
  assert.equal(calls.length, 0, "startup must not require a Ready workload");
  assert.equal(tools.length, 4);
  const result = await tools.find(tool => tool.name === "service_request").execute("one",
    { connection_id: "example", service: "github", method: "GET", path: "/repos/example/project" });
  assert.deepEqual(calls.map(call => call.message.method), ["initialize", "notifications/initialized", "tools/call"]);
  assert.deepEqual(calls.map(call => call.proof), ["synthetic-proof", "rotated-proof", "rotated-proof"]);
  assert.equal(result.content[0].text, '{"status":200}');
  assert.ok(!JSON.stringify(result).includes("proof"));
});

test("denials, token echoes and protocol errors fail without replay", async t => {
  const env = await fixture(t);
  for (const response of [
    { status: 403, body: "" }, { status: 302, body: "" }, { status: 200, body: "synthetic-proof" },
    { status: 200, body: '{"jsonrpc":"2.0","id":999,"result":{}}' },
  ]) {
    let calls = 0;
    const tool = connectionsTools(env, async () => { calls++; return response; })[0];
    await assert.rejects(tool.execute("one", {}));
    assert.equal(calls, 1);
  }
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(connectionsTools(env, async () => assert.fail("aborted request sent"))[0].execute("one", {}, controller.signal));
});
