// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, mkdir, readFile, realpath, rm, writeFile, symlink, unlink } from "node:fs/promises";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import vm from "node:vm";
import { fileURLToPath } from "node:url";
import { scopeBrowserStorage } from "../browser-storage.mjs";
import { adaptHTML, createProxy, scopedCookie, validateConfig } from "../proxy.mjs";
import { prepareHome, configureMemoryTools } from "../start.mjs";
import "./gateway-cases.mjs";

const basePath = "/ui/example/";

test("Memory MCP bootstrap preserves trust, user servers and conflicting files", async t => {
  const workspace = await mkdtemp(path.join(os.tmpdir(), "vscode-memory-fixture-"));
  t.after(() => rm(workspace, { recursive: true, force: true }));
  const env = { CETIRUN_MEMORY_ASSETS: "/opt/cetirun-memory" };
  await configureMemoryTools(workspace, env);
  const file = path.join(workspace, ".vscode/mcp.json");
  const config = JSON.parse(await readFile(file, "utf8"));
  assert.deepEqual(config.servers["cetirun-memory"].args, ["mcp", "--stdio", "--owner-private"]);
  assert.equal(config.servers["cetirun-memory"].cwd, "${workspaceFolder}");
  assert.ok(!JSON.stringify(config).includes("autoApprove"));
  config.servers.fixture = { type: "stdio", command: "fixture" };
  await writeFile(file, JSON.stringify(config));
  await configureMemoryTools(workspace, env);
  assert.deepEqual(JSON.parse(await readFile(file, "utf8")), config);
  config.servers["cetirun-memory"].command = "owner-tool";
  await writeFile(file, JSON.stringify(config));
  await assert.rejects(configureMemoryTools(workspace, env), /preserved/);
  assert.deepEqual(JSON.parse(await readFile(file, "utf8")), config);
  await unlink(file);
  const other = path.join(workspace, "owner.json");
  await writeFile(other, "preserve owner file");
  await symlink(other, file);
  await assert.rejects(configureMemoryTools(workspace, env), /conflicts/);
  assert.equal(await readFile(other, "utf8"), "preserve owner file");
});
async function listen(server) {
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return server.address().port;
}

test("entrypoint reads the shared PORT before any VS Code startup", () => {
  const env = { PORT: "8080", CETIRUN_WEB_BASE_PATH: basePath, CETIRUN_WEB_INSTANCE: "uid-a" };
  const entrypoint = fileURLToPath(new URL("../start.mjs", import.meta.url));
  const result = spawnSync(process.execPath, [entrypoint], { env, encoding: "utf8", timeout: 5000 });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /VS Code Server license acceptance is required/);
  delete env.PORT;
  const missing = spawnSync(process.execPath, [entrypoint], { env, encoding: "utf8", timeout: 5000 });
  assert.equal(missing.status, 1);
  assert.doesNotMatch(missing.stderr, /license acceptance/);
});

test("browser storage namespaces secrets, callbacks, databases and channels by instance UID", () => {
  const data = new Map([["portal-setting", "keep"]]);
  const storage = { get length() { return data.size; }, key: (index) => [...data.keys()][index] ?? null,
    getItem: (key) => data.get(key) ?? null, setItem: (key, value) => data.set(key, value),
    removeItem: (key) => data.delete(key), clear: () => data.clear() };
  function browser(uid) {
    const context = vm.createContext({ localStorage: storage, sessionStorage: storage,
      indexedDB: { open: (name) => name, deleteDatabase: (name) => name },
      BroadcastChannel: class { constructor(name) { this.name = name; } } });
    vm.runInContext(`(${scopeBrowserStorage.toString()})(${JSON.stringify(uid)})`, context);
    return context;
  }
  const a = browser("uid-a"), b = browser("uid-b");
  a.localStorage.setItem("secrets.provider", "encrypted-a");
  b.localStorage.setItem("secrets.provider", "encrypted-b");
  a.localStorage["vscode-web.url-callbacks[1]"] = "callback-a";
  assert.equal(a.localStorage.getItem("secrets.provider"), "encrypted-a");
  assert.equal(b.localStorage.getItem("secrets.provider"), "encrypted-b");
  assert.equal(b.localStorage["vscode-web.url-callbacks[1]"], undefined);
  assert.equal(a.localStorage.length, 2);
  assert.deepEqual(Object.keys(a.localStorage), ["secrets.provider", "vscode-web.url-callbacks[1]"]);
  assert.equal(browser("uid-a").localStorage.getItem("secrets.provider"), "encrypted-a");
  assert.equal(a.indexedDB.open("workbench"), "cetirun-vscode:uid-a:workbench");
  assert.notEqual(new a.BroadcastChannel("editor").name, new b.BroadcastChannel("editor").name);
  a.localStorage.clear();
  assert.equal(data.get("portal-setting"), "keep");
  assert.equal(b.localStorage.getItem("secrets.provider"), "encrypted-b");
});

test("storage events reach only the matching instance and retain logical keys", () => {
  const listeners = new Set();
  const native = { length: 0, getItem: () => null, key: () => null };
  const context = vm.createContext({ localStorage: native,
    addEventListener: (_, fn) => listeners.add(fn), removeEventListener: (_, fn) => listeners.delete(fn),
    StorageEvent: class { constructor(type, fields) { Object.assign(this, { type }, fields); } } });
  vm.runInContext(`(${scopeBrowserStorage.toString()})("uid-a")`, context);
  const seen = [];
  const listener = (event) => seen.push(event);
  context.addEventListener("storage", listener);
  const dispatch = [...listeners][0];
  dispatch({ key: "cetirun-vscode:uid-b:secrets.provider", storageArea: native });
  dispatch({ key: "cetirun-vscode:uid-a:secrets.provider", storageArea: native, url: "http://localhost/ui/example/" });
  assert.equal(seen.length, 1);
  assert.equal(seen[0].key, "secrets.provider");
  assert.equal(seen[0].storageArea, context.localStorage);
  context.removeEventListener("storage", listener);
  assert.equal(listeners.size, 0);
});

test("cookies retain upstream attributes but cannot escape the instance path", () => {
  assert.equal(scopedCookie("vscode-cli-secret-half=synthetic; Path=/; Domain=example.com; HttpOnly; SameSite=Strict", basePath),
    "vscode-cli-secret-half=synthetic; HttpOnly; SameSite=Strict; Path=/ui/example/");
  assert.equal(scopedCookie("_cetirun_oauth2=synthetic; Path=/", basePath), undefined);
  for (const invalid of ["/", "/ui/../", "/ui/example/?other", "//foreign.example/"]) {
    assert.throws(() => validateConfig({ basePath: invalid, instance: "uid-a", port: 8080 }));
  }
  validateConfig({ basePath, instance: "uid-a", port: 8080 });
});

test("workbench defaults preserve official connection, trust and product settings", () => {
  const config = { remoteAuthority: "localhost:8080", enableWorkspaceTrust: true, productConfiguration: { extensionsGallery: { serviceUrl: "https://example.com/gallery" } } };
  const html = `<html><head><meta id="vscode-workbench-web-configuration" data-settings="${JSON.stringify(config).replaceAll('"', '&quot;')}"></head></html>`;
  const result = adaptHTML(html, basePath + "_cetirun/storage.js", { "remote.extensionKind": { "vscode.example": ["workspace", "-web"] } });
  const parsed = JSON.parse(result.match(/data-settings="([^"]*)"/)[1].replaceAll('&quot;', '"'));
  assert.equal(parsed.remoteAuthority, config.remoteAuthority);
  assert.equal(parsed.enableWorkspaceTrust, true);
  assert.deepEqual(parsed.productConfiguration, config.productConfiguration);
  assert.deepEqual(parsed.configurationDefaults["remote.extensionKind"]["vscode.example"], ["workspace", "-web"]);
});

test("proxy preserves prefixes, adapts HTML and cookies, and strips unrelated credentials", async (t) => {
  const seen = [];
  const upstream = http.createServer((req, res) => {
    seen.push({ path: req.url, headers: req.headers });
    if (req.url.endsWith("/version")) { res.end("pinned-version"); return; }
    res.writeHead(200, { "Content-Type": "text/html", "Set-Cookie": ["vscode-cli-secret-half=synthetic; Path=/; HttpOnly"] });
    res.end("<!doctype html><html><head><script>original()</script></head><body>Editor</body></html>");
  });
  const upstreamPort = await listen(upstream);
  const proxy = createProxy({ basePath, instance: "uid-a", upstreamPort });
  const port = await listen(proxy);
  t.after(() => { proxy.stop(); upstream.closeAllConnections(); upstream.close(); });
  const url = `http://127.0.0.1:${port}`;
  const response = await fetch(url + basePath, { headers: { Authorization: "Bearer sk-dummy",
    Cookie: "_cetirun_oauth2=synthetic; vscode-cli-secret-half=synthetic", "x-original-host": "foreign.example" } });
  const body = await response.text();
  assert.ok(body.indexOf("_cetirun/storage.js") < body.indexOf("original()"));
  assert.match(response.headers.get("set-cookie"), /Path=\/ui\/example\//);
  assert.equal(seen[0].path, basePath);
  assert.equal(seen[0].headers.authorization, undefined);
  assert.equal(seen[0].headers["x-original-host"], undefined);
  assert.equal(seen[0].headers.cookie, "vscode-cli-secret-half=synthetic");
  assert.equal((await fetch(url + "/ui/other/")).status, 404);
  assert.equal((await fetch(url + "/healthz")).status, 200);
  assert.match(await (await fetch(url + basePath + "_cetirun/storage.js")).text(), /uid-a/);
});

test("proxy streams complete assets without changing their bytes or headers", async (t) => {
  const asset = Buffer.from([0, 1, 127, 128, 255]);
  const upstream = http.createServer((_, response) => {
    response.writeHead(200, { "Content-Type": "application/octet-stream", "Content-Length": asset.length,
      "Cache-Control": "public, max-age=3600", ETag: '"fixture"' });
    response.end(asset);
  });
  const proxy = createProxy({ basePath, instance: "uid-a", upstreamPort: await listen(upstream) });
  const port = await listen(proxy);
  t.after(() => { proxy.stop(); upstream.closeAllConnections(); upstream.close(); });
  const response = await fetch(`http://127.0.0.1:${port}${basePath}asset.bin`);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("content-length"), String(asset.length));
  assert.equal(response.headers.get("cache-control"), "public, max-age=3600");
  assert.equal(response.headers.get("etag"), '"fixture"');
  assert.deepEqual(Buffer.from(await response.arrayBuffer()), asset);
});

test("proxy terminates the downstream response when an upstream asset is truncated", async (t) => {
  let upstreamResponse;
  const upstream = http.createServer((_, response) => {
    upstreamResponse = response;
    response.writeHead(200, { "Content-Type": "application/javascript", "Content-Length": 1024 });
    response.write("partial");
  });
  const proxy = createProxy({ basePath, instance: "uid-a", upstreamPort: await listen(upstream) });
  const port = await listen(proxy);
  t.after(() => { proxy.stop(); upstream.closeAllConnections(); upstream.close(); });
  const result = await new Promise((resolve, reject) => {
    const request = http.get(`http://127.0.0.1:${port}${basePath}asset.js`, (response) => {
      let bytes = 0;
      response.on("data", (chunk) => {
        bytes += chunk.length;
        upstreamResponse.destroy();
      });
      response.on("error", (error) => {
        resolve({ status: response.statusCode, bytes, complete: response.complete, code: error.code });
      });
      response.on("end", () => reject(new Error("Truncated asset was accepted as complete")));
    });
    request.setTimeout(3000, () => {
      reject(new Error("Downstream response stayed pending after the upstream aborted"));
      request.destroy();
    });
    request.on("error", reject);
    t.after(() => request.destroy());
  });
  assert.deepEqual(result, { status: 200, bytes: 7, complete: false, code: "ECONNRESET" });
});

test("proxy forwards WebSocket upgrades under the instance prefix", async (t) => {
  const upstream = http.createServer();
  const peers = new Set();
  upstream.on("upgrade", (request, socket) => {
    assert.equal(request.url, basePath + "socket");
    peers.add(socket);
    socket.on("close", () => peers.delete(socket));
    socket.write("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n");
    socket.on("data", (data) => socket.write(data));
  });
  const proxy = createProxy({ basePath, instance: "uid-a", upstreamPort: await listen(upstream) });
  const port = await listen(proxy);
  t.after(() => { proxy.stop(); for (const peer of peers) peer.destroy(); upstream.close(); });
  await new Promise((resolve, reject) => {
    const socket = net.connect(port, "127.0.0.1");
    t.after(() => socket.destroy());
    socket.setTimeout(3000, () => reject(new Error("WebSocket proxy timed out")));
    socket.on("error", reject);
    let upgraded = false;
    socket.on("data", (data) => {
      if (!upgraded) {
        assert.match(data.toString(), /101 Switching Protocols/);
        upgraded = true;
        socket.write("synthetic-frame");
      } else { assert.equal(data.toString(), "synthetic-frame"); socket.destroy(); resolve(); }
    });
    socket.on("connect", () => socket.write(`GET ${basePath}socket HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n`));
  });
});

test("bootstrap uses the pinned offline server and preserves user files on restart", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "cetirun-vscode-test-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const home = path.join(root, "home"), install = path.join(root, "install"), commit = "a".repeat(40);
  await mkdir(path.join(home, ".agents/skills/cetirun-memory"), { recursive: true });
  await mkdir(path.join(install, "server"), { recursive: true });
  await writeFile(path.join(home, ".agents/AGENTS.md"), "Synthetic platform instructions");
  await writeFile(path.join(home, ".agents/skills/cetirun-memory/SKILL.md"), "Synthetic skill");
  const dirs = await prepareHome(home, install, commit);
  assert.equal(await realpath(path.join(dirs.cliData, "serve-web", commit)), path.join(install, "server"));
  assert.equal(await readFile(path.join(home, ".copilot/copilot-instructions.md"), "utf8"), "Synthetic platform instructions");
  const settings = path.join(dirs.data, "data/Machine/settings.json");
  const initialSettings = JSON.parse(await readFile(settings, "utf8"));
  assert.equal(initialSettings["chat.instructionsFilesLocations"][path.join(home, ".cetirun-vscode/instructions")], true);
  assert.match(await readFile(path.join(home, ".cetirun-vscode/instructions/cetirun.instructions.md"), "utf8"), /applyTo: '\*\*'/);
  await writeFile(settings, '{"userSetting":"keep"}');
  await writeFile(path.join(dirs.workspace, "keep.txt"), "User work");
  await prepareHome(home, install, commit);
  assert.equal(await readFile(settings, "utf8"), '{"userSetting":"keep"}');
  assert.equal(await readFile(path.join(dirs.workspace, "keep.txt"), "utf8"), "User work");
});
