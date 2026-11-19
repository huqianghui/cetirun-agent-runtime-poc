// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { createServer } from "node:http";
import { createRequire } from "node:module";
import { gzipSync } from "node:zlib";
import test from "node:test";

// Mount this test in the built image and run it with --network none.
const imageOptions = {
  skip: process.env.CETIRUN_PI_IMAGE_TEST !== "1" && "Requires the built Pi image",
};
const gypRequire = createRequire("/usr/local/lib/node_modules/npm/node_modules/node-gyp/package.json");

function parserFixture() {
  const { ByteParser } = gypRequire("undici/lib/web/websocket/receiver.js");
  const symbols = gypRequire("undici/lib/web/websocket/symbols.js");
  const { states, sentCloseFrameState } = gypRequire("undici/lib/web/websocket/constants.js");
  const ws = new EventTarget();
  const result = { aborted: false, errors: [], messages: [] };
  ws[symbols.kReadyState] = states.OPEN;
  ws[symbols.kSentClose] = sentCloseFrameState.NOT_SENT;
  ws[symbols.kWebSocketURL] = new URL("ws://127.0.0.1/");
  ws[symbols.kController] = { abort() { result.aborted = true; } };
  ws[symbols.kResponse] = {
    socket: { destroyed: false, write() {}, destroy() { this.destroyed = true; } },
  };
  ws.addEventListener("error", (event) => result.errors.push(event.message));
  ws.addEventListener("message", (event) => result.messages.push(event.data));
  result.parser = new ByteParser(ws, null, { maxFragments: 2, maxPayloadSize: 8 });
  return result;
}

for (const payload of [Buffer.alloc(0), Buffer.from("x")]) {
  test(`npm undici rejects excess ${payload.length === 0 ? "empty" : "small"} fragments`, imageOptions, () => {
    const fixture = parserFixture();
    try {
      // Three tiny fragments are sufficient; never exercise memory exhaustion.
      for (const opcode of [0x01, 0x00, 0x00]) {
        fixture.parser.write(Buffer.concat([Buffer.from([opcode, payload.length]), payload]));
      }
      assert.equal(fixture.aborted, true);
      assert.deepEqual(fixture.errors, ["Too many message fragments"]);
      assert.deepEqual(fixture.messages, []);
    } finally {
      fixture.parser.destroy();
    }
  });
}

test("npm undici still accepts a valid fragmented message", imageOptions, () => {
  const fixture = parserFixture();
  try {
    fixture.parser.write(Buffer.from([0x01, 1, 0x61, 0x80, 1, 0x62]));
    assert.deepEqual(fixture.messages, ["ab"]);
    assert.deepEqual(fixture.errors, []);
    assert.equal(fixture.aborted, false);
  } finally {
    fixture.parser.destroy();
  }
});

test("node-gyp can still download through npm's undici", { ...imageOptions, timeout: 15000 }, async (t) => {
  const proxyNames = ["HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"];
  const previous = Object.fromEntries(proxyNames.map((name) => [name, process.env[name]]));
  for (const name of proxyNames) delete process.env[name];
  t.after(() => {
    for (const [name, value] of Object.entries(previous)) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  });
  let attempts = 0;
  const server = createServer((request, response) => {
    if (request.url === "/redirect") {
      response.writeHead(302, { Location: "/fixture" }).end();
    } else if (request.url === "/retry" && ++attempts === 1) {
      response.writeHead(503).end();
    } else if (request.url === "/gzip") {
      response.writeHead(200, { "Content-Encoding": "gzip" }).end(gzipSync("synthetic download\n"));
    } else {
      response.end("synthetic download\n");
    }
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const { download } = gypRequire("./lib/download.js");
    const baseURL = `http://127.0.0.1:${server.address().port}`;
    for (const route of ["/fixture", "/redirect", "/gzip", "/retry"]) {
      await t.test(route, async () => {
        const response = await download({ version: "test", opts: {} }, baseURL + route);
        assert.equal(response.status, 200);
        assert.equal(await response.text(), "synthetic download\n");
      });
    }
    assert.equal(attempts, 2);
    await t.test("no-proxy bypass", async () => {
      process.env.HTTP_PROXY = "http://127.0.0.1:1";
      const response = await download({ version: "test", opts: { noproxy: "127.0.0.1" } }, baseURL + "/fixture");
      assert.equal(response.status, 200);
      assert.equal(await response.text(), "synthetic download\n");
    });
  } finally {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
});
