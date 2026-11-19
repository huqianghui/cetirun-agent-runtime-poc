// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

"use strict";

const fs = require("node:fs");
const http = require("node:http");
const https = require("node:https");
const net = require("node:net");
const crypto = require("node:crypto");
const prefix = "/ui/example";
const browserHeaders = { Host: "webapp.example.com", Origin: "https://webapp.example.com", "X-Forwarded-Proto": "https", "X-Forwarded-Prefix": prefix };
const password = "synthetic-api-key-retained-0123456789";
const { spawn, execFileSync } = require("node:child_process");
const python = "/usr/local/bin/hermes-python";
const home = "/home/vibe";
const configPath = home + "/.hermes/config.yaml";
const envPath = home + "/.hermes/.env";
const legacyPath = home + "/.hermes/gateway.json";
const children = [];
const results = [];
let stopping = false;
const deadline = setTimeout(() => finish(false, "native check deadline exceeded"), 150000);

function finish(ok, message) {
  if (stopping) return;
  stopping = true;
  clearTimeout(deadline);
  for (const child of children) child.process.kill("SIGTERM");
  const result = { ok, message, results };
  if (!ok) result.processes = children.map((child) => ({ command: child.command, exitCode: child.process.exitCode }));
  console.log(JSON.stringify(result));
  setTimeout(() => {
    for (const child of children) child.process.kill("SIGKILL");
    process.exit(ok ? 0 : 1);
  }, 750);
}
process.on("SIGTERM", () => finish(false, "native check stopped"));

function start(command, args) {
  const log = "run/child-" + children.length + ".log";
  const fd = fs.openSync(log, "a", 0o600);
  const process = spawn(command, args, { stdio: ["ignore", fd, fd] });
  fs.closeSync(fd);
  const child = { command, process, log, expectedExit: false };
  children.push(child);
  process.on("error", (error) => finish(false, "cannot start native process: " + error.code));
  process.on("exit", (code) => { if (!stopping && !child.expectedExit) finish(false, command + " exited " + code); });
  return child;
}

function request(port, path, tls, headers = {}, method = "GET", payload) {
  return new Promise((resolve, reject) => {
    const body = payload === undefined ? undefined : JSON.stringify(payload);
    const req = (tls ? https : http).request({ hostname: "127.0.0.1", port, path, method, agent: false,
      headers: { ...headers, ...(body ? { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) } : {}) }, ...(tls || {}) }, (res) => {
      let body = "";
      res.on("data", (chunk) => { body = (body + chunk).slice(0, 1048576); });
      res.on("end", () => resolve({ status: res.statusCode, headers: res.headers, body, html: /<html|<!doctype/i.test(body) }));
    });
    req.setTimeout(3000, () => req.destroy(new Error("request timed out")));
    req.on("error", reject);
    req.end(body);
  });
}

function socketPing(tls, ticket) {
  return new Promise((resolve, reject) => {
    const key = crypto.randomBytes(16).toString("base64");
    const req = https.request({ hostname: "127.0.0.1", port: 15444, path: "/api/ws?ticket=" + encodeURIComponent(ticket),
      ...tls, headers: { ...browserHeaders, Upgrade: "websocket", Connection: "Upgrade", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": key } });
    const timer = setTimeout(() => { req.destroy(); reject(new Error("Webapp socket ping timed out")); }, 5000);
    req.on("error", () => { clearTimeout(timer); reject(new Error("Webapp socket rejected")); });
    req.on("response", () => { clearTimeout(timer); reject(new Error("Webapp socket rejected")); });
    req.on("upgrade", (response, socket, head) => {
      const expected = crypto.createHash("sha1").update(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");
      if (response.headers["sec-websocket-accept"] !== expected) { socket.destroy(); clearTimeout(timer); reject(new Error("Webapp socket handshake invalid")); return; }
      const payload = Buffer.from(JSON.stringify({ jsonrpc: "2.0", id: "native-ping", method: "gateway.ping" }));
      const mask = crypto.randomBytes(4);
      socket.write(Buffer.concat([Buffer.from([0x81, 0x80 | payload.length]), mask, Buffer.from(payload.map((byte, i) => byte ^ mask[i % 4]))]));
      let received = head;
      const consume = (chunk) => {
        received = Buffer.concat([received, chunk]);
        while (received.length >= 2) {
          const opcode = received[0] & 15;
          let length = received[1] & 127;
          let offset = 2;
          if (length === 126) { if (received.length < 4) return; length = received.readUInt16BE(2); offset = 4; }
          if (length === 127) { socket.destroy(); clearTimeout(timer); reject(new Error("unexpected Webapp socket frame")); return; }
          if (received.length < offset + length) return;
          const content = received.subarray(offset, offset + length); received = received.subarray(offset + length);
          if (opcode === 8) { socket.destroy(); clearTimeout(timer); reject(new Error("Webapp socket closed before ping")); return; }
          if (opcode === 1) {
            const message = JSON.parse(content.toString());
            if (message.id === "native-ping") { socket.destroy(); clearTimeout(timer); message.result?.ok === true ? resolve() : reject(new Error("Webapp gateway ping failed")); return; }
          }
        }
      };
      socket.on("data", consume);
      socket.on("error", () => { clearTimeout(timer); reject(new Error("Webapp socket closed")); });
      consume(Buffer.alloc(0));
    });
    req.end();
  });
}

async function login(tls) {
  for (const [provider, value, status] of [["basic", "wrong-password", 401], ["missing", password, 404]]) {
    const failed = await request(15444, "/auth/password-login", tls, browserHeaders, "POST", { provider, username: "cetirun", password: value });
    if (failed.status !== status) throw new Error("Webapp invalid credentials or missing provider was accepted");
  }
  const response = await request(15444, "/auth/password-login", tls, browserHeaders, "POST", { provider: "basic", username: "cetirun", password });
  if (response.status !== 200 || !JSON.parse(response.body).ok) throw new Error("Webapp password login failed");
  const cookies = response.headers["set-cookie"] || [];
  if (cookies.length < 3 || cookies.some((cookie) => !cookie.includes("Path=" + prefix + "/") && !cookie.includes("Path=" + prefix + ";"))) throw new Error("Webapp cookie path escaped instance prefix");
  if (!cookies.some((cookie) => cookie.startsWith("__Secure-hermes_webapp_signin_"))) throw new Error("Webapp browser sign-in binding missing");
  return cookies.map((cookie) => cookie.split(";", 1)[0]).join("; ");
}

async function browserSession(tls, cookies, phase) {
  const headers = { ...browserHeaders, Cookie: cookies };
  const me = await request(15444, "/api/auth/me", tls, headers);
  if (me.status !== 200 || JSON.parse(me.body).provider !== "basic") throw new Error("Webapp cookie session failed");
  const page = await request(15444, "/projects/example", tls, headers);
  if (page.status !== 200 || !page.html || !page.body.includes("__HERMES_BASE_PATH__") || !page.body.includes(prefix)) throw new Error("Webapp deep-link prefix bootstrap failed");
  const assetPaths = Array.from(page.body.matchAll(/(?:src|href)=["']([^"']+\.(?:js|css))["']/g), (match) => match[1]);
  if (!assetPaths.length) throw new Error("Webapp bundle has no assets");
  for (const asset of assetPaths) {
    const path = new URL(asset, "https://webapp.example.com" + prefix + "/").pathname;
    if (!path.startsWith(prefix + "/")) throw new Error("Webapp asset escaped instance prefix");
    if ((await request(15444, path.slice(prefix.length), tls, headers)).status !== 200) throw new Error("Webapp asset unavailable");
  }
  const ticketResponse = await request(15444, "/api/auth/ws-ticket", tls, headers, "POST", {});
  if (ticketResponse.status !== 200) throw new Error("Webapp cookie did not authorize WS ticket");
  const ticket = JSON.parse(ticketResponse.body).ticket;
  await socketPing(tls, ticket);
  let reused = false;
  try { await socketPing(tls, ticket); reused = true; } catch {}
  if (reused) throw new Error("Webapp socket ticket was reusable");
  results.push({ phase, passwordProvider: true, cookieSession: true, prefixedAssets: assetPaths.length, websocketPing: true, oneUseTicket: true });
}

async function ready(tls, phase) {
  for (let attempt = 0; attempt < 80 && !stopping; attempt++) {
    try {
      const api = await request(15443, "/health", tls);
      const webapp = await request(15444, "/login", tls, browserHeaders);
      const terminal = await request(15445, "/term/", tls);
      if (api.status === 200 && webapp.status === 200 && webapp.html && terminal.status === 200 && terminal.html) {
        results.push({ phase, api: api.status, webapp: webapp.status, terminal: terminal.status });
        return;
      }
    } catch {}
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error("native API/Webapp/terminal did not become ready");
}

function refuses(port, host = "127.0.0.2") {
  return new Promise((resolve, reject) => {
    const socket = net.connect({ host, port });
    socket.setTimeout(1000, () => { socket.destroy(); resolve(); });
    socket.on("error", () => resolve());
    socket.on("connect", () => { socket.destroy(); reject(new Error("backend bound beyond loopback: " + port)); });
  });
}

function readConfig() {
  return JSON.parse(execFileSync(python, ["-c", "import json,sys,hermes_yaml as yaml; print(json.dumps(yaml.safe_load(open(sys.argv[1]))))", configPath], { encoding: "utf8", timeout: 5000 }));
}

function seedOverrides() {
  const config = fs.existsSync(configPath) ? readConfig() : {};
  config.user_preference = { retained: true, nested: ["one", "two"] };
  config.platforms = { ...(config.platforms || {}), api_server: {
    enabled: false, host: "0.0.0.0", port: 18080,
    extra: { host: "::", port: 18081, model_name: "retained-model-name" }, reply_to_mode: "all"
  } };
  config.dashboard = { host: "0.0.0.0", port: 18082, theme: "retained" };
  config.gateway = { ...(config.gateway || {}),
    platforms: { api_server: { enabled: false, host: "0.0.0.0", port: 18087, extra: { host: "::", port: 18088 } } },
    api_server: { enabled: false, host: "0.0.0.0", port: 18089, extra: { host: "::", port: 18090 }, max_concurrent_runs: 7 }
  };
  fs.writeFileSync(configPath, JSON.stringify(config), { mode: 0o600 });
  fs.writeFileSync(legacyPath, JSON.stringify({ platforms: { api_server: {
    enabled: false, host: "0.0.0.0", port: 18091, extra: { host: "::", port: 18092, _enabled_explicit: true }
  } }, legacy_preference: "retained" }), { mode: 0o600 });
  fs.writeFileSync(envPath, [
    "API_SERVER_KEY=synthetic-api-key-retained-0123456789", "API_SERVER_ENABLED=false", "API_SERVER_HOST=0.0.0.0",
    "API_SERVER_PORT=18083", "HERMES_DASHBOARD_HOST=0.0.0.0", "HERMES_DASHBOARD_PORT=18084",
    "TTYD_PORT=18085", "TTYD_BIND_ARGS=--interface 0.0.0.0", "USER_SETTING=retained", ""
  ].join("\n"), { mode: 0o600 });
}

function assertRetained() {
  const config = readConfig();
  const api = config.platforms.api_server;
  if (["enabled", "host", "port"].some((name) => name in api) || ["host", "port"].some((name) => name in api.extra)) {
    throw new Error("persisted API listener override survived authority enforcement");
  }
  if (!config.user_preference.retained || config.dashboard.theme !== "retained" ||
    api.reply_to_mode !== "all" || api.extra.model_name !== "retained-model-name") throw new Error("unrelated HOME configuration was lost");
  const legacy = JSON.parse(fs.readFileSync(legacyPath, "utf8"));
  for (const entry of [config.gateway.platforms.api_server, config.gateway.api_server, legacy.platforms.api_server]) {
    if (["enabled", "host", "port"].some((key) => key in entry) ||
      ["host", "port", "_enabled_explicit"].some((key) => key in entry.extra)) throw new Error("an API listener alias survived authority enforcement");
  }
  if (config.gateway.api_server.max_concurrent_runs !== 7 || legacy.legacy_preference !== "retained") throw new Error("unrelated gateway configuration was lost");
  const env = fs.readFileSync(envPath, "utf8");
  if (/^API_SERVER_(HOST|PORT|ENABLED)=/m.test(env) || !env.includes("USER_SETTING=retained") ||
    env.includes("HERMES_DASHBOARD_PORT=18084") || !env.includes("TTYD_PORT=18085")) throw new Error("HOME environment authority or retention failed");
}

async function bindings(phase) {
  for (const port of [8642, 9120, 7681]) {
    await refuses(port);
    results.push({ phase, loopbackOnly: port });
  }
}

async function run(input) {
  fs.mkdirSync(home + "/.hermes", { recursive: true });
  fs.mkdirSync(home + "/.aks/skills", { recursive: true });
  fs.mkdirSync(home + "/.cache", { recursive: true });
  fs.mkdirSync("run", { recursive: true });
  fs.mkdirSync("tls", { recursive: true, mode: 0o700 });
  if (input.retained) seedOverrides();
  fs.writeFileSync("generated.env", "API_SERVER_KEY=synthetic-api-key-retained-0123456789\nAPI_SERVER_CORS_ORIGINS=https://example.com\n", { mode: 0o600 });
  execFileSync(python, ["/etc/cetirun/hermes-apply-config.py", "--home", home, "--config", "/candidate/config.yaml",
    "--env", "/work/generated.env", "--gateway-key-env", "CETIRUN_GATEWAY_API_KEY", "--ingress-mtls"], { timeout: 10000 });
  for (const [file, contents] of Object.entries(input.material)) fs.writeFileSync("tls/" + file, contents, { mode: 0o600 });
  for (const file of ["tls.crt", "ca.crt"]) fs.writeFileSync("/var/run/cetirun/ingress-public/" + file, input.material[file], { mode: 0o600 });
  fs.writeFileSync("envoy.json", JSON.stringify(input.bootstrap));
  // Only scratch paths are relocated; source remains read-only.
  fs.writeFileSync("entrypoint.sh", fs.readFileSync("/candidate/entrypoint.sh", "utf8").replaceAll("/tmp/", "/work/run/"), { mode: 0o500 });
  start("/bin/bash", ["/work/entrypoint.sh"]);
  const envoy = start("/opt/envoy", ["--disable-hot-restart", "--concurrency", "1", "--log-level", "error", "-c", "/work/envoy.json"]);
  const tls = { ca: input.material["ca.crt"], cert: input.clientCert, key: input.clientKey, servername: input.serverName, minVersion: "TLSv1.3" };
  await ready(tls, input.retained ? "retained" : "fresh");
  await bindings("initial");
  if (input.retained) assertRetained();
  if ((await request(15443, "/v1/models", tls, { Authorization: "Bearer synthetic-api-key-retained-0123456789" })).status !== 200) throw new Error("native API models failed");
  const cookies = await login(tls);
  await browserSession(tls, cookies, "initial");
  const browserCredential = fs.readFileSync(home + "/.hermes/webapp/session-token");
  const tokenInfo = fs.statSync(home + "/.hermes/webapp/session-token");
  if ((fs.statSync(home + "/.hermes/webapp").mode & 0o777) !== 0o700) throw new Error("Webapp credential directory permissions invalid");
  if ((tokenInfo.mode & 0o777) !== 0o600 || tokenInfo.uid !== process.getuid()) throw new Error("Webapp browser credential permissions invalid");
  await refuses(9119, "127.0.0.1");
  if (/\[program:.*dashboard/.test(fs.readFileSync("/work/run/supervisord.conf", "utf8"))) throw new Error("obsolete Dashboard process configured");
  if ((await request(15444, "/api/auth/me", tls, browserHeaders)).status === 200) throw new Error("Webapp unauthenticated access succeeded");
  if (input.retained) {
    seedOverrides();
    execFileSync("supervisorctl", ["-c", "/work/run/supervisord.conf", "restart", "all"], { timeout: 30000 });
    await ready(tls, "supervised-restart");
    await bindings("supervised-restart");
    assertRetained();
    if (!fs.readFileSync(home + "/.hermes/webapp/session-token").equals(browserCredential)) throw new Error("normal restart rotated browser credential");
    await browserSession(tls, cookies, "supervised-restart");
  }
  for (const port of [15443, 15444, 15445]) {
    for (const kind of ["missing-client", "wrong-client-uri", "wrong-server-san", "plaintext"]) {
      let options = { ...tls };
      if (kind === "missing-client") { delete options.cert; delete options.key; }
      if (kind === "wrong-client-uri") { options.cert = input.wrongCert; options.key = input.wrongKey; }
      if (kind === "wrong-server-san") options.servername = "other.example.com";
      if (kind === "plaintext") options = null;
      let rejected = false;
      try { await request(port, "/", options); } catch { rejected = true; }
      if (!rejected) throw new Error(kind + " accepted on " + port);
      results.push({ rejection: kind, port });
    }
  }
  const certificateReady = await new Promise((resolve) => require("/candidate/ingress-health.cjs")(resolve, "/work/tls/", 15440, input.serverName));
  if (!certificateReady) throw new Error("candidate certificate readiness failed");
  const health = () => execFileSync(python, ["/etc/cetirun/hermes-runtime.py", "readiness", "--timeout", "5"], { timeout: 10000, stdio: "pipe" });
  const refusesReadiness = () => {
    let refused = false;
    try { health(); } catch { refused = true; }
    if (!refused) throw new Error("Hermes readiness ignored broken ingress TLS");
  };
  health();
  execFileSync(python, ["-c", [
    "import importlib.util, os, stat",
    "from pathlib import Path",
    "spec = importlib.util.spec_from_file_location('runtime', '/etc/cetirun/hermes-runtime.py')",
    "runtime = importlib.util.module_from_spec(spec)",
    "spec.loader.exec_module(runtime)",
    "runtime.HOME = Path('/candidate/foreign-home')",
    "os.environ['HERMES_HOME'] = str(runtime.HOME)",
    "folder = runtime.HOME / 'webapp'",
    "assert stat.S_IMODE(folder.stat().st_mode) == 0o700 and folder.stat().st_uid == os.geteuid()",
    "token = folder / 'session-token'",
    "assert stat.S_IMODE(token.stat().st_mode) == 0o600 and token.stat().st_uid != os.geteuid(), 'fixture requires host UID different from container UID 1024'",
    "try:",
    "    runtime.readiness(1)",
    "except ValueError as exc:",
    "    assert str(exc) == 'BrowserCredentialPermissionsInvalid'",
    "else:",
    "    raise AssertionError('foreign-owned browser credential accepted')",
  ].join("\n")], { timeout: 10000, stdio: "pipe" });
  results.push({ phase: "readiness", foreignOwnedCredentialRejected: true });
  fs.writeFileSync("/var/run/cetirun/ingress-public/tls.crt", input.expiredCert, { mode: 0o600 });
  refusesReadiness();
  fs.writeFileSync("/var/run/cetirun/ingress-public/tls.crt", input.material["tls.crt"], { mode: 0o600 });
  health();
  fs.unlinkSync("/var/run/cetirun/ingress-public/tls.crt");
  refusesReadiness();
  fs.writeFileSync("/var/run/cetirun/ingress-public/tls.crt", input.material["tls.crt"], { mode: 0o600 });
  health();
  envoy.expectedExit = true;
  await new Promise((resolve) => { envoy.process.once("exit", resolve); envoy.process.kill("SIGTERM"); });
  if ((await request(8642, "/health", null)).status !== 200) throw new Error("API stopped during helper-only readiness test");
  refusesReadiness();
  results.push({ phase: "readiness", healthyEndpoint: true, expiredCertificateRejected: true, missingCertificateRejected: true, stoppedEnvoyRejected: true });

  finish(true, "native Webapp password/cookie/WS, listener authority, HOME retention and TLS traversal verified");
}

let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => { input += chunk; });
process.stdin.on("end", () => run(JSON.parse(input)).catch((error) => finish(false, error.message)));
