// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const { stripTypeScriptTypes } = require("node:module");
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || "playwright");

async function main() {
  assert.ok(process.env.CHROMIUM_EXECUTABLE, "set CHROMIUM_EXECUTABLE to a prepared Chromium binary");
  const module = stripTypeScriptTypes(fs.readFileSync(path.resolve(__dirname,
    "../../app/cetirun-chart/agents/hermes/compat/webapp_storage.ts"), "utf8"));
  const server = http.createServer((request, response) => {
    if (request.url === "/scope.js") {
      response.writeHead(200, { "Content-Type": "text/javascript" });
      return response.end(module);
    }
    const url = new URL(request.url, "http://127.0.0.1");
    const prefix = url.pathname === "/native" ? undefined : url.pathname.split("/").slice(0, 3).join("/");
    const seed = url.searchParams.has("seed") ? `
      sessionStorage.setItem('hermes.webapp.session.v1:' + JSON.stringify([location.origin, '/ui/a']), 'fresh-launch-grant');
      sessionStorage.setItem('hermes.webapp.session.v1:' + JSON.stringify([location.origin, '/ui/b']), 'other-launch-grant');` : "";
    response.writeHead(200, { "Content-Type": "text/html" });
    response.end(`<script>window.__HERMES_BASE_PATH__ = ${JSON.stringify(prefix) || "undefined"};
      window.originalLocal = localStorage; window.originalSession = sessionStorage;
      window.originalChannel = BroadcastChannel; ${seed}</script>
      <script type="module">import { scopeWebappStorage } from '/scope.js';
      scopeWebappStorage(); scopeWebappStorage(); window.scopeReady = true;</script>`);
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  const origin = "http://127.0.0.1:" + server.address().port;
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_EXECUTABLE, headless: true });
  try {
    const context = await browser.newContext();
    const page = async prefix => {
      const current = await context.newPage();
      await current.goto(origin + prefix);
      await current.waitForFunction(() => window.scopeReady);
      return current;
    };
    const native = await page("/native");
    assert.equal(await native.evaluate(() => localStorage === window.originalLocal &&
      sessionStorage === window.originalSession && BroadcastChannel === window.originalChannel), true);
    await native.evaluate(() => localStorage.setItem("hermes.desktop.pluginDecisions.v2", '{"legacy":true}'));
    const [a, anotherA, b] = await Promise.all([page("/ui/a"), page("/ui/a"), page("/ui/b")]);
    for (const current of [a, anotherA, b]) {
      assert.equal(await current.evaluate(() => localStorage.getItem("hermes.desktop.pluginDecisions.v2")), null);
      await current.evaluate(() => {
        window.events = [];
        addEventListener("storage", event => window.events.push({ key: event.key, value: event.newValue,
          local: event.storageArea === localStorage }));
        window.messages = [];
        window.channel = new BroadcastChannel("hermes:sessions");
        window.channel.onmessage = event => window.messages.push(event.data);
      });
    }
    await a.evaluate(() => {
      localStorage.setItem("draft", "instance-a");
      localStorage.property = 42;
      sessionStorage.setItem("session", "instance-a-session");
    });
    await anotherA.waitForFunction(() => window.events.some(event => event.key === "draft"));
    assert.deepEqual(await anotherA.evaluate(() => window.events.find(event => event.key === "draft")),
      { key: "draft", value: "instance-a", local: true });
    await b.evaluate(() => localStorage.setItem("draft", "instance-b"));
    assert.deepEqual(await a.evaluate(() => ({ value: localStorage.getItem("draft"), property: localStorage.property,
      keys: Object.keys(localStorage).sort(), length: localStorage.length, missing: localStorage.key(50) })),
    { value: "instance-a", property: "42", keys: ["draft", "property"], length: 2, missing: null });
    assert.equal(await b.evaluate(() => localStorage.getItem("draft")), "instance-b");
    await a.evaluate(() => { delete localStorage.property; localStorage.clear(); });
    await anotherA.waitForFunction(() => window.events.some(event => event.key === "draft" && event.value === null));
    assert.equal(await a.evaluate(() => localStorage.length), 0);
    assert.equal(await b.evaluate(() => localStorage.getItem("draft")), "instance-b");
    assert.equal(await native.evaluate(() => localStorage.getItem("hermes.desktop.pluginDecisions.v2")), '{"legacy":true}');

    await a.evaluate(() => window.channel.postMessage("from-a"));
    await anotherA.waitForFunction(() => window.messages.includes("from-a"));
    await b.evaluate(() => window.channel.postMessage("from-b"));
    await b.waitForTimeout(100);
    assert.deepEqual(await b.evaluate(() => window.events), []);
    assert.deepEqual(await b.evaluate(() => window.messages), []);
    assert.deepEqual(await a.evaluate(() => window.messages), []);
    assert.deepEqual(await anotherA.evaluate(() => window.messages), ["from-a"]);
    assert.equal(await a.evaluate(() => window.channel.name), "hermes:sessions");
    await a.evaluate(() => dispatchEvent(new StorageEvent("storage", { key: "hydrated", newValue: "yes" })));
    assert.equal(await a.evaluate(() => window.events.some(event => event.key === "hydrated")), true);

    await a.goto(origin + "/ui/b");
    await a.waitForFunction(() => window.scopeReady);
    assert.equal(await a.evaluate(() => sessionStorage.getItem("session")), null);
    await a.evaluate(() => { sessionStorage.setItem("other", "b"); sessionStorage.clear(); });
    await a.goto(origin + "/ui/a");
    await a.waitForFunction(() => window.scopeReady);
    assert.equal(await a.evaluate(() => sessionStorage.getItem("session")), "instance-a-session");
    await a.evaluate(() => sessionStorage.setItem("hermes.webapp.session.v1:" +
      JSON.stringify([location.origin, "/ui/a"]), "old-scoped-grant"));
    await a.goto(origin + "/ui/a?seed=1");
    await a.waitForFunction(() => window.scopeReady);
    assert.deepEqual(await a.evaluate(() => {
      const current = "hermes.webapp.session.v1:" + JSON.stringify([location.origin, "/ui/a"]);
      const other = "hermes.webapp.session.v1:" + JSON.stringify([location.origin, "/ui/b"]);
      return { fresh: sessionStorage.getItem(current) === "fresh-launch-grant",
        rawConsumed: window.originalSession.getItem(current) === null,
        otherPreserved: window.originalSession.getItem(other) === "other-launch-grant",
        otherHidden: sessionStorage.getItem(other) === null };
    }), { fresh: true, rawConsumed: true, otherPreserved: true, otherHidden: true });
    await context.close();
    console.log(JSON.stringify({ passed: true, localAndSessionStorageIsolated: true, scopedEnumerationAndClear: true,
      sameInstanceEventsAndChannels: true, crossInstanceEventsAndChannelsBlocked: true, nativeDesktopUnchanged: true,
      currentInstanceLaunchGrantPreserved: true }));
  } finally {
    await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
}

main().catch(error => { console.error(error); process.exitCode = 1; });
