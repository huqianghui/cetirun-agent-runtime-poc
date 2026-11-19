// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import http from "node:http";
import { pipeline } from "node:stream";
import { scopeBrowserStorage } from "./browser-storage.mjs";

const MAX_HTML = 4 * 1024 * 1024;
const VS_CODE_COOKIES = new Set(["vscode-cli-secret-half", "vscode-secret-key-path", "vscode-tkn", "vscode.nls.locale"]);

export function validateConfig({ basePath, instance, port }) {
  if (!/^\/ui\/[a-z0-9][a-z0-9-]*\/$/.test(basePath) || !/^[a-zA-Z0-9-]{1,80}$/.test(instance) ||
      !Number.isInteger(port) || port < 1 || port > 65535) throw new Error("Invalid platform Web UI binding");
}

export function scopedCookie(cookie, basePath) {
  const parts = cookie.split(";").map((part) => part.trim());
  if (!VS_CODE_COOKIES.has(parts[0].split("=")[0])) return undefined;
  return [...parts.filter((part, i) => i === 0 || !/^(path|domain)=/i.test(part)), `Path=${basePath}`].join("; ");
}

export function adaptHTML(body, storagePath, defaults) {
  body = body.replace(/(<meta\s+id="vscode-workbench-web-configuration"\s+data-settings=")([^"]*)"/i, (_, before, attribute) => {
    const decode = { "&quot;": '"', "&#39;": "'", "&lt;": "<", "&gt;": ">", "&amp;": "&" };
    const configuration = JSON.parse(attribute.replace(/&quot;|&#39;|&lt;|&gt;|&amp;/g, (entity) => decode[entity]));
    configuration.configurationDefaults = { ...defaults, ...configuration.configurationDefaults };
    const encoded = JSON.stringify(configuration).replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
    return before + encoded + '"';
  });
  return body.replace(/<head(?:\s[^>]*)?>/i, (head) => `${head}<script src="${storagePath}"></script>`);
}

export function createProxy({ basePath, instance, upstreamPort, defaults = {}, healthy = () => true }) {
  const storagePath = `${basePath}_cetirun/storage.js`;
  const bootstrap = `(${scopeBrowserStorage.toString()})(${JSON.stringify(instance)});`;
  const sockets = new Set();
  function targetHeaders(request) {
    const headers = { ...request.headers, "accept-encoding": "identity" };
    for (const key of ["authorization", "x-cetirun-auth-key", "x-cetirun-agent-api-key", "x-cetirun-portal-id-token",
      "x-original-host", "x-forwarded-host", "x-forwarded-port", "x-forwarded-prefix"]) delete headers[key];
    headers.cookie = (headers.cookie ?? "").split(";").map((part) => part.trim())
      .filter((part) => VS_CODE_COOKIES.has(part.split("=")[0])).join("; ");
    return headers;
  }
  function allowed(request) {
    const pathname = request.url?.split("?")[0] ?? "";
    return pathname.startsWith(basePath) && !/%2f|%5c|%2e|\\|\/\.\.?(?:\/|$)/i.test(pathname);
  }
  const server = http.createServer((request, response) => {
    if (request.url === "/healthz" && request.method === "GET") {
      const probe = http.get({ host: "127.0.0.1", port: upstreamPort, path: `${basePath}version`, timeout: 2000 }, (result) => {
        result.resume();
        const ready = healthy() && result.statusCode === 200;
        response.writeHead(ready ? 200 : 503, { "Content-Type": "application/json" });
        response.end(JSON.stringify({ status: ready ? "ok" : "starting" }));
      });
      probe.on("timeout", () => probe.destroy());
      probe.on("error", () => { response.writeHead(503); response.end(); });
      return;
    }
    if (!allowed(request)) { response.writeHead(404); response.end(); return; }
    if (request.url === storagePath && request.method === "GET") {
      response.writeHead(200, { "Content-Type": "application/javascript", "Cache-Control": "no-store" });
      response.end(bootstrap);
      return;
    }
    const upstream = http.request({ host: "127.0.0.1", port: upstreamPort, method: request.method,
      path: request.url, headers: targetHeaders(request) }, async (result) => {
      const headers = { ...result.headers };
      if (headers["set-cookie"]) headers["set-cookie"] = headers["set-cookie"].map((cookie) => scopedCookie(cookie, basePath)).filter(Boolean);
      const html = /text\/html/i.test(headers["content-type"] ?? "");
      const worker = ["worker", "sharedworker", "serviceworker"].includes(request.headers["sec-fetch-dest"]);
      if (!html && !worker) {
        response.writeHead(result.statusCode, headers);
        pipeline(result, response, () => {});
        return;
      }
      try {
        if (headers["content-encoding"] && headers["content-encoding"] !== "identity") throw new Error("Unexpected compressed page");
        const chunks = [];
        let size = 0;
        for await (const chunk of result) {
          size += chunk.length;
          if (size > MAX_HTML) throw new Error("Page exceeds adapter limit");
          chunks.push(chunk);
        }
        let body = Buffer.concat(chunks).toString("utf8");
        if (html) {
          body = adaptHTML(body, storagePath, defaults);
        } else {
          body = bootstrap + "\n" + body;
        }
        delete headers["content-encoding"];
        delete headers["transfer-encoding"];
        delete headers.etag;
        headers["content-length"] = Buffer.byteLength(body);
        headers["cache-control"] = "no-store";
        if (worker) headers["service-worker-allowed"] = basePath;
        response.writeHead(result.statusCode, headers);
        response.end(body);
      } catch {
        if (!response.headersSent) response.writeHead(502);
        response.end("VS Code page adaptation failed");
      }
    });
    upstream.on("error", () => { if (!response.headersSent) response.writeHead(502); response.end(); });
    request.on("aborted", () => upstream.destroy());
    response.on("close", () => upstream.destroy());
    request.pipe(upstream);
  });
  server.on("upgrade", (request, socket, head) => {
    if (!allowed(request)) { socket.end("HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n"); return; }
    const upstream = http.request({ host: "127.0.0.1", port: upstreamPort, path: request.url,
      headers: targetHeaders(request) });
    upstream.on("upgrade", (result, peer, upstreamHead) => {
      sockets.add(peer);
      peer.on("close", () => sockets.delete(peer));
      peer.on("error", () => socket.destroy());
      socket.on("error", () => peer.destroy());
      socket.on("close", () => peer.destroy());
      peer.on("close", () => socket.destroy());
      const headers = Object.entries(result.headers).filter(([name]) => name !== "set-cookie")
        .flatMap(([name, value]) => (Array.isArray(value) ? value : [value]).map((entry) => `${name}: ${entry}`));
      socket.write(`HTTP/1.1 101 Switching Protocols\r\n${headers.join("\r\n")}\r\n\r\n`);
      if (upstreamHead.length) socket.write(upstreamHead);
      if (head.length) peer.write(head);
      peer.pipe(socket).pipe(peer);
    });
    upstream.on("response", (result) => { result.resume(); socket.end(`HTTP/1.1 ${result.statusCode} Rejected\r\nConnection: close\r\n\r\n`); });
    upstream.on("error", () => socket.destroy());
    socket.on("close", () => upstream.destroy());
    upstream.end();
  });
  server.on("connection", (socket) => {
    sockets.add(socket);
    socket.on("close", () => sockets.delete(socket));
  });
  server.stop = () => { for (const socket of sockets) socket.destroy(); server.close(); };
  return server;
}
