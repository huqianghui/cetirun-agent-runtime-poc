#!/usr/bin/env node
// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const command = process.argv[2];
if (!["login", "status", "cancel"].includes(command)) {
  console.log("Usage: node copilot-auth.mjs login --allow-model-policy-updates | status | cancel\nRun inside the Agent container through an approved operations channel, or use a working loopback port-forward to Pod port 9099. PI_AUTH_PORT changes only the client's local destination port.");
  process.exit(command === "--help" ? 0 : 2);
}
const port = Number(process.env.PI_AUTH_PORT || 9099);
if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error("Invalid PI_AUTH_PORT");
const base = `http://127.0.0.1:${port}`;
async function request(path, body) {
  const response = await fetch(base + path, {
    method: body ? "POST" : "GET",
    headers: { "X-CetiRun-Pi-Auth": "1", "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(10_000),
  });
  if (!response.ok) throw new Error(`Local auth service returned ${response.status}`);
  return response.json();
}
try {
  if (command === "status") console.log(JSON.stringify(await request("/v1/status")));
  if (command === "cancel") console.log(JSON.stringify(await request("/v1/login/cancel", {})));
  if (command === "login") {
    if (!process.argv.includes("--allow-model-policy-updates")) {
      throw new Error("Pi native login may enable unconfigured Copilot models. Pass --allow-model-policy-updates only after approving that behavior.");
    }
    await request("/v1/login/start", { allowModelPolicyUpdates: true });
    let shown;
    for (;;) {
      const status = await request("/v1/status");
      if (status.userCode && shown !== status.userCode) {
        shown = status.userCode;
        console.log(`Open ${status.verificationUri} and enter ${status.userCode}.`);
      }
      if (status.state === "authenticated") { console.log("Pi Copilot login completed."); break; }
      if (["failed", "cancelled"].includes(status.state)) throw new Error(`Login ${status.state}`);
      await new Promise((resolve) => setTimeout(resolve, 2000));
    }
  }
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
