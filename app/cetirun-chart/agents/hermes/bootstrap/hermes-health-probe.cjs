// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

"use strict";

const http = require("node:http");
const args = process.argv.slice(2);
const rawPort = process.env.API_SERVER_PORT || "";
const port = Number(rawPort);
const timeoutSeconds = Number(args[1]);
if (
  args.length !== 2 || args[0] !== "--timeout" || !/^[0-9]+$/.test(args[1]) ||
  !Number.isInteger(timeoutSeconds) || timeoutSeconds < 1 || timeoutSeconds > 2147483 ||
  !/^[0-9]+$/.test(rawPort) || !Number.isInteger(port) || port < 1 || port > 65535 || port === 9120
) {
  console.error("[hermes-health] InvalidProbeArguments");
  process.exit(1);
}

let finished = false;
let request;
// Leave time for the process to exit before kubelet's deadline.
const deadline = setTimeout(() => finish("ProbeTimeout"), timeoutSeconds * 1000 - 250);

function finish(error) {
  if (finished) return;
  finished = true;
  clearTimeout(deadline);
  if (request) request.destroy();
  if (error) console.error(`[hermes-health] ${error}`);
  process.exit(error ? 1 : 0);
}

const helpers = [
  ["CETIRUN_EGRESS_SNI", "proxy-health.cjs", "EgressProxyNotReady"],
  ["CETIRUN_INGRESS_MTLS", "ingress-health.cjs", "IngressTLSNotReady"],
];

function checkHelpers(index = 0) {
  if (index === helpers.length) return finish();
  const [flag, filename, error] = helpers[index];
  if (process.env[flag] !== "true") return checkHelpers(index + 1);
  try {
    require(`/etc/cetirun/${filename}`)((ready) => ready ? checkHelpers(index + 1) : finish(error));
  } catch {
    finish(error);
  }
}

request = http.get({ hostname: "127.0.0.1", port, path: "/health", agent: false }, (response) => {
  if (response.statusCode !== 200) return finish("HTTPProbeFailed");
  let body = "";
  response.setEncoding("utf8");
  response.on("data", (chunk) => { body += chunk; });
  response.on("error", () => finish("HTTPProbeFailed"));
  response.on("end", () => {
    try {
      JSON.parse(body);
    } catch {
      return finish("HTTPProbeFailed");
    }
    checkHelpers();
  });
});
request.setTimeout(Math.max(100, timeoutSeconds * 1000 / 3), () => finish("ProbeTimeout"));
request.on("error", () => finish("HTTPProbeFailed"));
