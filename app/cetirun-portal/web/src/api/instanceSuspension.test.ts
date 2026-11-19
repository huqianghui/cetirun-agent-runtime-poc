// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { afterEach, describe, expect, it, vi } from "vitest";
import { isSuspensionRequestRejected, setInstanceSuspendedByName } from "./instance";
import { PortalError } from "./fetch";

afterEach(() => vi.unstubAllGlobals());

describe("owner suspension API", () => {
  it.each([true, false])("retries suspended=%s using exactly the original action and version", async (suspended) => {
    const request = { uid: "agent-uid", resourceVersion: "7", generation: 2, suspended };
    const response = { ...request, resourceVersion: "8", generation: 3 };
    const fetch = vi.fn().mockImplementation(() => Promise.resolve(Response.json(response, { status: 202 })));
    vi.stubGlobal("fetch", fetch);
    await expect(setInstanceSuspendedByName("alice/synthetic", request)).resolves.toEqual(response);
    await setInstanceSuspendedByName("alice/synthetic", request);
    expect(fetch).toHaveBeenCalledTimes(2);
    for (const call of fetch.mock.calls) {
      expect(call).toEqual([`/api/v1/instances/alice%2Fsynthetic/${suspended ? "suspend" : "resume"}`, {
        method: "POST", headers: { "X-CetiRun-Agent-Generation": "2", "Content-Type": "application/json" },
        body: JSON.stringify({ uid: request.uid, resourceVersion: request.resourceVersion }),
      }]);
    }
  });

  it.each([
    [409, "instance_changed", true], [409, "instance_deleting", true],
    [409, "suspension_unavailable", true], [403, "forbidden", true],
    [409, "suspension_home_required", true], [503, "suspension_home_unverified", true],
    [403, "admission_rejected", true], [428, "version_required", true],
    [500, "internal_error", false], [503, "instance_changed", false], [409, undefined, false],
  ])("preserves HTTP %s / %s without inferring acceptance from text", async (status, code, rejected) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json({ error: { code, message: "Synthetic error" } }, { status })));
    const error = await setInstanceSuspendedByName("alice", { uid: "agent-uid", resourceVersion: "7", suspended: true })
      .catch((error: unknown) => error);
    expect(error).toBeInstanceOf(PortalError);
    expect(isSuspensionRequestRejected(error)).toBe(rejected);
  });

  it.each(["network", "unreadable error", "unreadable success", "wrong Agent", "wrong intent", "missing version", "invalid version"])(
    "keeps %s outcomes uncertain", async (outcome) => {
      const response = { uid: "agent-uid", resourceVersion: "8", generation: 3, suspended: true };
      const fetch = vi.fn();
      if (outcome === "network") fetch.mockRejectedValue(new TypeError("Connection interrupted"));
      else if (outcome.startsWith("unreadable")) fetch.mockResolvedValue(new Response("{", {
        status: outcome === "unreadable error" ? 409 : 202, headers: { "Content-Type": "application/json" },
      }));
      else fetch.mockResolvedValue(Response.json({
        ...response,
        ...(outcome === "wrong Agent" ? { uid: "replacement-uid" } : {}),
        ...(outcome === "wrong intent" ? { suspended: false } : {}),
        ...(outcome === "missing version" ? { resourceVersion: "" } : {}),
        ...(outcome === "invalid version" ? { resourceVersion: 8 } : {}),
      }, { status: 202 }));
      vi.stubGlobal("fetch", fetch);
      const error = await setInstanceSuspendedByName("alice", { uid: "agent-uid", resourceVersion: "7", suspended: true })
        .catch((error: unknown) => error);
      expect(error).toBeInstanceOf(Error);
      expect(isSuspensionRequestRejected(error)).toBe(false);
      expect(fetch).toHaveBeenCalledTimes(1);
    },
  );
});
