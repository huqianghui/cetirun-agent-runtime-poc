// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { createHTTPServer, readJSON, writeJSON } from "./http.mjs";

/**
 * Optional local operations API used by copilot-auth.mjs, not a CetiRun
 * Harness endpoint. server.mjs binds it only to Pod loopback.
 */
export function createAuthAPI(runtime, activity) {
  let state = { state: "idle" };
  let loginController;

  function startLogin() {
    const controller = new AbortController();
    loginController = controller;
    activity.login = true;
    state = { state: "starting" };
    const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(15 * 60_000)]);
    void runtime.login(signal, (event) => { state = event; })
      .then(() => { state = { state: "authenticated" }; })
      .catch(() => { state = { state: controller.signal.aborted ? "cancelled" : "failed" }; })
      .finally(() => {
        activity.login = false;
        loginController = undefined;
      });
  }

  const server = createHTTPServer(async (request, response) => {
    try {
      if (!isLocalClient(request)) {
        return writeJSON(response, 403, { error: "Local auth client required" });
      }
      switch (`${request.method} ${request.url}`) {
        case "GET /v1/status":
          return writeJSON(response, 200, {
            ...state,
            authenticated: await runtime.authenticated(),
            skills: runtime.skills(),
          });
        case "POST /v1/login/start": {
          const input = await readJSON(request);
          if (input.allowModelPolicyUpdates !== true) {
            return writeJSON(response, 400, { error: "Explicit model-policy consent required" });
          }
          if (activity.login) return writeJSON(response, 409, { error: "Login is already active" });
          if (activity.chat) return writeJSON(response, 409, { error: "Agent is busy" });
          startLogin();
          return writeJSON(response, 202, { state: "starting" });
        }
        case "POST /v1/login/cancel":
          loginController?.abort();
          return writeJSON(response, 200, { state: "cancelling" });
        default:
          return writeJSON(response, 404, { error: "Not found" });
      }
    } catch {
      writeJSON(response, 400, { error: "Invalid authentication request" });
    }
  });
  return { server, abort: () => loginController?.abort() };
}

function isLocalClient(request) {
  const host = new URL(`http://${request.headers.host}`).hostname;
  return ["localhost", "127.0.0.1", "[::1]"].includes(host) &&
    !request.headers.origin && request.headers["x-cetirun-pi-auth"] === "1";
}
