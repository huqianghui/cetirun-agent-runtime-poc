// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { pathToFileURL } from "node:url";
import { createAgentAPI } from "./agent-api.mjs";
import { createAuthAPI } from "./auth-api.mjs";
import { closeServer, listen } from "./http.mjs";

export function createServers(runtime) {
  const activity = { chat: false, login: false };
  const agent = createAgentAPI(runtime, activity);
  const auth = createAuthAPI(runtime, activity);
  return {
    api: agent.server,
    auth: auth.server,
    async close() {
      agent.abort();
      auth.abort();
      await Promise.all([closeServer(agent.server), closeServer(auth.server)]);
    },
  };
}

async function main() {
  if (process.argv.includes("--help")) {
    console.log("Pi HTTP adapter: HOME and PORT are required. API binds 0.0.0.0:PORT unless CETIRUN_BIND_HOST=127.0.0.1; local auth binds 127.0.0.1:9099. Start the native TUI with pi-tui.mjs.");
    return;
  }
  const port = Number(process.env.PORT);
  if (!process.env.HOME || !Number.isInteger(port) || port < 1 || port > 65535 || port === 9099) {
    throw new Error("HOME and a valid PORT other than 9099 are required");
  }
  const bindHost = process.env.CETIRUN_BIND_HOST || "0.0.0.0";
  if (!["0.0.0.0", "127.0.0.1"].includes(bindHost)) throw new Error("Invalid CETIRUN_BIND_HOST");
  process.umask(0o077);
  process.env.PI_TELEMETRY = "0";
  const { createPiRuntime } = await import("./pi-runtime.mjs");
  const servers = createServers(await createPiRuntime(process.env.HOME));
  try {
    await listen(servers.api, port, bindHost);
    if (!process.env.OPENAI_BASE_URL) await listen(servers.auth, 9099, "127.0.0.1");
  } catch (error) {
    await servers.close();
    throw error;
  }
  const stop = () => { void servers.close(); };
  process.once("SIGTERM", stop);
  process.once("SIGINT", stop);
  console.log("Pi HTTP adapter ready; no credential or prompt logging is enabled.");
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(() => {
    console.error("Pi example startup failed; check HOME, context and PORT.");
    process.exitCode = 1;
  });
}
