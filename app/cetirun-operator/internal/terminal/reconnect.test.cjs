// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

function fixture(responses, fetchResponse) {
  const timers = new Map();
  const notices = [];
  let timerId = 0;
  let reloads = 0;
  class FakeSocket {
    constructor() { this.handlers = {}; }
    addEventListener(name, callback) { (this.handlers[name] ??= []).push(callback); }
    emit(name, value = {}) { for (const callback of this.handlers[name] ?? []) callback(value); }
  }
  const window = {
    WebSocket: FakeSocket,
    location: { href: "https://gateway.example.com/alice/term/", reload: () => { reloads++; } },
    setTimeout: (callback, delay) => { timers.set(++timerId, { callback, delay }); return timerId; },
    clearTimeout: (id) => timers.delete(id),
  };
  const document = {
    currentScript: { dataset: { agentUid: "agent-uid" } },
    body: { appendChild: (node) => notices.push(node) },
    createElement: () => ({ style: {}, setAttribute() {}, remove() {} }),
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "reconnect.js"), "utf8"), {
    window, document, URL, AbortSignal,
    fetch: async (url) => {
      assert.equal(String(url), "https://gateway.example.com/alice/term/readyz");
      if (fetchResponse) return fetchResponse();
      const value = responses.shift() ?? { status: 503 };
      return { status: value.status, ok: value.status === 200, type: "basic", json: async () => value.body };
    },
  });
  return {
    socket: new window.WebSocket("wss://gateway.example.com/alice/term/ws"),
    newSocket: () => new window.WebSocket("wss://gateway.example.com/alice/term/ws"),
    timers, notices,
    reloads: () => reloads,
    async tick() {
      const [id, timer] = timers.entries().next().value;
      timers.delete(id);
      await timer.callback();
      return timer.delay;
    },
  };
}

test("a failed first connection retries until the same Agent is available", async () => {
  const f = fixture([{ status: 503 }, { status: 200, body: { ready: true, agentUID: "agent-uid" } }]);
  f.socket.emit("error");
  f.socket.emit("close", { code: 1006 });
  assert.equal(f.timers.size, 1);
  assert.equal(await f.tick(), 1000);
  assert.equal(await f.tick(), 2000);
  assert.equal(f.reloads(), 1);
});

test("an intentional exit does not reconnect", () => {
  const f = fixture([]);
  f.socket.emit("open");
  f.socket.emit("close", { code: 1000 });
  assert.equal(f.timers.size, 0);
});

test("normal closure cancels an already scheduled retry", () => {
  const f = fixture([]);
  f.socket.emit("error");
  f.socket.emit("close", { code: 1000 });
  assert.equal(f.timers.size, 0);
});

test("a WebSocket policy rejection does not reconnect", () => {
  const f = fixture([]);
  f.socket.emit("error");
  f.socket.emit("close", { code: 1008 });
  assert.equal(f.timers.size, 0);
});

test("authorization failure stops retries", async () => {
  const f = fixture([{ status: 403 }]);
  f.socket.emit("error");
  await f.tick();
  assert.equal(f.timers.size, 0);
  assert.equal(f.reloads(), 0);
  assert.match(f.notices.at(-1).textContent, /sign in again/);
});

test("a same-name replacement Agent requires reopening from the Portal", async () => {
  const f = fixture([{ status: 200, body: { ready: true, agentUID: "new-agent-uid" } }]);
  f.socket.emit("error");
  await f.tick();
  assert.equal(f.timers.size, 0);
  assert.equal(f.reloads(), 0);
  assert.match(f.notices.at(-1).textContent, /replaced/);
});

test("ttyd's own successful reconnect cancels the fallback timer", () => {
  const f = fixture([]);
  f.socket.emit("error");
  assert.equal(f.timers.size, 1);
  f.socket.emit("open");
  assert.equal(f.timers.size, 0);
});

test("a late close from an old socket does not interrupt a reconnected terminal", () => {
  const f = fixture([]);
  f.socket.emit("error");
  const next = f.newSocket();
  next.emit("open");
  f.socket.emit("close", { code: 1006 });
  assert.equal(f.timers.size, 0);
});

for (const event of ["open", "normal close", "replacement"]) {
  test(`a pending readiness response cannot reload after ${event}`, async () => {
    const pending = Promise.withResolvers();
    const f = fixture([], () => pending.promise);
    f.socket.emit("error");
    const check = f.tick();
    if (event === "open") f.socket.emit("open");
    if (event === "normal close") f.socket.emit("close", { code: 1000 });
    if (event === "replacement") f.newSocket();
    pending.resolve({ status: 200, ok: true, json: async () => ({ ready: true, agentUID: "agent-uid" }) });
    await check;
    assert.equal(f.reloads(), 0);
    assert.equal(f.timers.size, 0);
  });
}

test("a stale readiness body cannot reload a now-connected terminal", async () => {
  const pending = Promise.withResolvers();
  const reading = Promise.withResolvers();
  const f = fixture([], async () => ({
    status: 200, ok: true,
    json() { reading.resolve(); return pending.promise; },
  }));
  f.socket.emit("error");
  const check = f.tick();
  await reading.promise;
  f.socket.emit("open");
  pending.resolve({ ready: true, agentUID: "agent-uid" });
  await check;
  assert.equal(f.reloads(), 0);
  assert.equal(f.timers.size, 0);
});
