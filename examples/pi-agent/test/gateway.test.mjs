// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import test from "node:test";
import { createServer } from "node:http";
import { mkdtemp, mkdir, writeFile, readFile, readdir, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createPiRuntime } from "../pi-runtime.mjs";
import { gatewayConfiguration } from "../gateway-runtime.mjs";
import { createServers } from "../server.mjs";

async function fixture(t, options = {}) {
  const home = await mkdtemp(path.join(os.tmpdir(), "cetirun-pi-gateway-"));
  t.after(() => rm(home, { recursive: true, force: true }));
  await mkdir(path.join(home, ".agents/skills/cetirun-memory"), { recursive: true });
  await writeFile(path.join(home, ".agents/AGENTS.md"), "Use only the requested synthetic workspace task.");
  await writeFile(path.join(home, ".agents/skills/cetirun-memory/SKILL.md"), "---\nname: cetirun-memory\ndescription: Synthetic memory instructions.\n---\nUse private HOME.");
  let requests = 0, models = 0;
  const inputs = [];
  const server = createServer(async (request, response) => {
    assert.equal(request.headers.authorization, "Bearer sk-dummy-gateway-key");
    if (request.url === "/v1/models") {
      models++;
      if (options.catalogStatus) { response.writeHead(options.catalogStatus); response.end("private-provider-error"); return; }
      if (options.redirect) { response.writeHead(302, { Location: options.redirect }); response.end(); return; }
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({ data: (options.catalog ?? (options.empty ? [] : ["shared-example"])).map(id => ({ id, object: "model" })) }));
      return;
    }
    assert.equal(request.url, "/v1/chat/completions");
    requests++;
    let raw = "";
    for await (const chunk of request) raw += chunk;
    const body = JSON.parse(raw);inputs.push(body);
    assert.equal(body.model, "shared-example");
    if (options.modelStatus) { response.writeHead(options.modelStatus); response.end("private-provider-error"); return; }
    response.writeHead(200, { "Content-Type": "text/event-stream" });
    const event = (delta, finish_reason = null) => response.write(`data: ${JSON.stringify({ id: "test-completion", object: "chat.completion.chunk", created: 1, model: body.model, choices: [{ index: 0, delta, finish_reason }] })}\n\n`);
    if (requests <= (options.toolRounds ?? (options.tool ? 1 : 0))) {
      const file = options.toolRounds ? `gateway-proof-${requests}.txt` : "gateway-proof.txt";
      event({ role: "assistant", tool_calls: [{ index: 0, id: `call_write_${requests}`, type: "function", function: { name: "write", arguments: JSON.stringify({ path: file, content: "synthetic-tool-output" }) } }] });
      event({}, "tool_calls");
    } else {
      event({ role: "assistant", content: "Synthetic Gateway answer." });
      event({}, options.finishReason ?? "stop");
    }
    response.end("data: [DONE]\n\n");
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  const env = { OPENAI_BASE_URL: `http://127.0.0.1:${server.address().port}/v1`, OPENAI_API_KEY: "sk-dummy-gateway-key" };
  const runtime = await createPiRuntime(home, env);
  return { runtime, home, inputs, requests: () => requests, models: () => models };
}

test("Gateway mode is explicit and incomplete configuration never selects native auth", () => {
  assert.equal(gatewayConfiguration({}), undefined);
  for (const env of [
    { OPENAI_BASE_URL: "http://gateway.example/v1" }, { OPENAI_API_KEY: "sk-dummy" },
    { OPENAI_BASE_URL: "http://user:pass@gateway.example/v1", OPENAI_API_KEY: "sk-dummy" },
    { OPENAI_BASE_URL: "http://gateway.example/v1?target=other", OPENAI_API_KEY: "sk-dummy" },
  ]) assert.throws(() => gatewayConfiguration(env));
});

test("real Pi SDK discovers and calls the Gateway without storing its key in HOME", async t => {
  const f = await fixture(t);
  assert.equal(f.runtime.gateway, true);assert.equal(f.models(), 0, "startup must not call the Gateway");
  assert.deepEqual(await f.runtime.models(AbortSignal.timeout(5000)), ["shared-example"]);
  let streamed = "";
  const answer = await f.runtime.run({ model: "shared-example", messages: [{ role: "user", content: "Synthetic question" }], signal: AbortSignal.timeout(10000), onText: text => streamed += text });
  assert.equal(answer.text, "Synthetic Gateway answer.");assert.equal(streamed, answer.text);assert.equal(f.requests(), 1);
  assert.ok(f.inputs[0].tools.some(tool => tool.function.name === "write"));
  await assert.rejects(f.runtime.login(AbortSignal.timeout(1000), () => {}), /Portal/);
  assert.ok(!(await readdir(path.join(f.home, ".pi/agent"))).includes("auth.json"));
});

test("real Pi tools run behind the Gateway and return their result to that same Gateway", async t => {
  const f = await fixture(t, { tool: true });
  const answer = await f.runtime.run({ model: "shared-example", messages: [{ role: "user", content: "Write the synthetic proof file" }], signal: AbortSignal.timeout(10000) });
  assert.equal(answer.toolCalls, 1);assert.equal(f.requests(), 2);
  assert.equal(await readFile(path.join(f.home, "workspace/gateway-proof.txt"), "utf8"), "synthetic-tool-output");
  assert.ok(f.inputs[1].messages.some(message => message.role === "tool"));
});

test("the real Pi HTTP adapter permits eight tools but reports a ninth as a failed task", async t => {
  for (const toolRounds of [8, 9]) {
    await t.test(`${toolRounds} requested tools`, async t => {
      const f = await fixture(t, { toolRounds });
      const servers = createServers(f.runtime);
      await new Promise(resolve => servers.api.listen(0, "127.0.0.1", resolve));
      t.after(() => servers.close());
      const response = await fetch(`http://127.0.0.1:${servers.api.address().port}/v1/chat/completions`, {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ model: "shared-example", messages: [{ role: "user", content: "Write the synthetic numbered proof files." }], stream: true }),
        signal: AbortSignal.timeout(10000),
      });
      assert.equal(response.status, 200);
      const text = await response.text();
      assert.equal(f.requests(), 9, "completed tool calls are not replayed");
      const files = (await readdir(path.join(f.home, "workspace"))).sort();
      assert.deepEqual(files, Array.from({ length: 8 }, (_, index) => `gateway-proof-${index + 1}.txt`));
      if (toolRounds === 8) {
        assert.match(text, /"finish_reason":"stop"/);
        assert.equal(text.split("data: [DONE]").length, 2);
      } else {
        const failure = text.split("\n").filter(line => line.startsWith("data: "))
          .map(line => JSON.parse(line.slice(6))).find(event => event.error);
        assert.equal(failure?.error.code, "tool_budget_exceeded");
        assert.doesNotMatch(text, /"finish_reason":"stop"|\[DONE\]/);
      }
    });
  }
});

test("the real Pi HTTP adapter preserves an output-limit finish reason", async t => {
  const f = await fixture(t, { finishReason: "length" });
  const servers = createServers(f.runtime);
  await new Promise(resolve => servers.api.listen(0, "127.0.0.1", resolve));
  t.after(() => servers.close());
  for (const stream of [false, true]) {
    const response = await fetch(`http://127.0.0.1:${servers.api.address().port}/v1/chat/completions`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model: "shared-example", messages: [{ role: "user", content: "Synthetic output-limit task." }], stream }),
      signal: AbortSignal.timeout(10000),
    });
    assert.equal(response.status, 200);
    const text = await response.text();
    assert.match(text, /"finish_reason":"length"/);
    assert.doesNotMatch(text, /"finish_reason":"stop"/);
  }
  assert.equal(f.requests(), 2);
});

test("Gateway model failure is not retried by the Pi SDK or sent to a native provider", async t => {
  const f = await fixture(t, { modelStatus: 503 });
  await assert.rejects(f.runtime.run({ model: "shared-example", messages: [{ role: "user", content: "Do not replay" }], signal: AbortSignal.timeout(10000) }));
  assert.equal(f.requests(), 1);
});

test("empty or unauthorized Gateway catalogs do not invent an available model", async t => {
  const empty = await fixture(t, { empty: true });assert.deepEqual(await empty.runtime.models(AbortSignal.timeout(5000)), []);
  const denied = await fixture(t, { catalogStatus: 401 });await assert.rejects(denied.runtime.models(AbortSignal.timeout(5000)), /discovery is unavailable/);
  assert.equal(empty.requests(), 0);assert.equal(denied.requests(), 0);
});

test("Gateway model discovery refuses redirects", async t => {
  const f = await fixture(t, { redirect: "http://127.0.0.1:1/v1/models" });
  await assert.rejects(f.runtime.models(AbortSignal.timeout(5000)));assert.equal(f.models(), 1);
});

test("unregistered Gateway access groups never appear as callable models", async t => {
  const groups = ["cetirun-shared", "cetirun-copilot-11111111-1111-4111-8111-111111111111"];
  const empty = await fixture(t, { catalog: groups });
  assert.deepEqual(await empty.runtime.models(AbortSignal.timeout(5000)), []);
  const populated = await fixture(t, { catalog: [...groups, "copilot/example", "cetirun-example"] });
  assert.deepEqual(await populated.runtime.models(AbortSignal.timeout(5000)), ["copilot/example", "cetirun-example"]);
});
