// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

"use strict";

const fs = require("node:fs");
const http = require("node:http");
const https = require("node:https");
const net = require("node:net");
const { spawn } = require("node:child_process");

const children = [];
let stopping = false;
const deadline = setTimeout(() => fail("native endpoint readiness deadline"), 120000);
function cleanup() {
  stopping = true;
  clearTimeout(deadline);
  for (const child of children) child.kill("SIGKILL");
}
function fail(message) {
  cleanup();
  console.error(message);
  process.exit(1);
}
process.on("SIGTERM", () => fail("native probe stopped"));
function start(command, args) {
  const child = spawn(command, args, { stdio: ["ignore", "pipe", "pipe"] });
  let log = "";
  for (const stream of [child.stdout, child.stderr]) {
    stream.on("data", (chunk) => { log = (log + chunk).slice(-6000); });
  }
  child.on("exit", (code) => {
    if (!stopping) fail(`${command} exited (${code}): ${log}`);
  });
  child.on("error", () => fail("native process failed to start"));
  children.push(child);
}
function request(port, path, tls) {
  return new Promise((resolve, reject) => {
    const client = tls ? https : http;
    const req = client.get({ host: "127.0.0.1", port, path, agent: false, ...(tls || {}) }, (res) => {
      res.resume();
      res.on("end", () => resolve(res.statusCode));
    });
    req.setTimeout(1500, () => req.destroy(new Error("request timeout")));
    req.on("error", reject);
  });
}
async function refuses(address, port) {
  return new Promise((resolve, reject) => {
    const socket = net.connect({ host: address, port });
    socket.setTimeout(1000, () => { socket.destroy(); resolve(); });
    socket.on("error", () => resolve());
    socket.on("connect", () => { socket.destroy(); reject(new Error("native backend bound beyond loopback")); });
  });
}
async function run(input) {
  // Match the image's install roots without mounting any host HOME.
  fs.symlinkSync("/home/vibe", "home");
  fs.mkdirSync("home/.openclaw/state", { recursive: true });
  fs.mkdirSync("run", { recursive: true });
  fs.mkdirSync("cache", { recursive: true });
  fs.mkdirSync("state", { recursive: true });
  fs.mkdirSync("tls", { recursive: true, mode: 0o700 });
  for (const [file, contents] of Object.entries(input.material)) {
    fs.writeFileSync("tls/" + file, contents, { mode: 0o600 });
  }
  fs.writeFileSync("envoy.json", JSON.stringify(input.bootstrap));
  fs.writeFileSync("home/.openclaw/openclaw.json", JSON.stringify({
    gateway: { bind: "loopback", mode: "local", port: 18789,
      auth: { mode: "token", token: "synthetic-native-probe-token" },
      controlUi: { basePath: "/ui/example" }, trustedProxies: ["127.0.0.1"] },
    browser: { enabled: false }, update: { checkOnStart: false }
  }));
  start("openclaw", ["gateway", "--bind", "loopback", "--port", "18789", "--allow-unconfigured"]);
  start("ttyd", ["--interface", "127.0.0.1", "--port", "7681", "--base-path", "/term", "--writable", "--check-origin", "/bin/bash"]);
  start("/opt/envoy", ["--disable-hot-restart", "--concurrency", "1", "--log-level", "error", "-c", "/work/envoy.json"]);
  const tls = { ca: input.material["ca.crt"], cert: input.clientCert, key: input.clientKey, servername: input.serverName, minVersion: "TLSv1.3" };
  let ready = false;
  for (let attempt = 0; attempt < 110; attempt++) {
    try {
      if (await request(15443, "/readyz", tls) === 200 && await request(15445, "/term/", tls) === 200) {
        ready = true;
        break;
      }
    } catch {}
    await new Promise((resolve) => setTimeout(resolve, 750));
  }
  if (!ready) throw new Error("native API/terminal did not become ready behind mTLS");
  if (await request(15443, "/ui/example/", tls) !== 200) throw new Error("native Control UI did not traverse mTLS");
  for (const port of [18789, 7681]) await refuses("127.0.0.2", port);
  for (const port of [15443, 15445]) {
    let failed = false;
    try { await request(port, "/", { ca: tls.ca, servername: tls.servername }); } catch { failed = true; }
    if (!failed) throw new Error("native ingress accepted a missing client certificate");
    failed = false;
    try { await request(port, "/"); } catch { failed = true; }
    if (!failed) throw new Error("native ingress accepted plaintext");
  }
  cleanup();
  console.log("native OpenClaw API/UI and ttyd traversed mTLS; non-loopback backend access rejected");
}

let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => { input += chunk; });
process.stdin.on("end", () => run(JSON.parse(input)).catch((error) => fail(error.message)));
