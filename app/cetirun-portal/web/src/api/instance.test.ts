// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { getInstance, createInstance, deleteInstanceByName, updateInstance, upgradeInstance, restartInstanceByName, isRestartRequestRejected } from "./instance";
import { PortalError } from "./fetch";

describe("restartInstanceByName", () => {
  it("sends the same idempotency key and version on retries", async () => {
    const request = { uid: "agent-uid", resourceVersion: "7", requestId: "request-00000001" };
    const response = { restartRequestId: request.requestId, restartAvailable: false };
    global.fetch = vi.fn().mockImplementation(() => Promise.resolve(new Response(JSON.stringify(response), {
      status: 202, headers: { "Content-Type": "application/json" },
    })));
    await expect(restartInstanceByName("alice", request)).resolves.toEqual(response);
    await restartInstanceByName("alice", request);
    expect(fetch).toHaveBeenCalledTimes(2);
    for (const call of vi.mocked(fetch).mock.calls) {
      expect(call).toEqual(["/api/v1/instances/alice/restart", expect.objectContaining({
        method: "POST", headers: expect.objectContaining({ "Idempotency-Key": request.requestId }),
        body: JSON.stringify({ uid: request.uid, resourceVersion: request.resourceVersion }),
      })]);
    }
  });

  it("surfaces a rejected stale or suspended restart", async () => {
    global.fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      error: { code: "restart_unavailable", message: "a suspended Agent cannot be restarted" },
    }), { status: 409, headers: { "Content-Type": "application/json" } }));
    await expect(restartInstanceByName("alice", { uid: "agent-uid", resourceVersion: "7", requestId: "request-00000001" }))
      .rejects.toThrow("a suspended Agent cannot be restarted");
  });

  it("preserves the response status and error code for a definite rejection", async () => {
    global.fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      error: { code: "instance_changed", message: "Agent changed; reload before saving" },
    }), { status: 409, headers: { "Content-Type": "application/json" } }));
    await expect(restartInstanceByName("alice", { uid: "agent-uid", resourceVersion: "7", requestId: "request-00000001" }))
      .rejects.toMatchObject({ status: 409, code: "instance_changed", message: "Agent changed; reload before saving" });
  });

  it("preserves a server error as uncertain instead of classifying it by message", async () => {
    global.fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      error: { code: "internal_error", message: "Failed after the request may have been accepted" },
    }), { status: 500, headers: { "Content-Type": "application/json" } }));
    await expect(restartInstanceByName("alice", { uid: "agent-uid", resourceVersion: "7", requestId: "request-00000001" }))
      .rejects.toMatchObject({ status: 500, code: "internal_error" });
  });

  it("does not turn a malformed error response into a definite rejection", async () => {
    global.fetch = vi.fn().mockResolvedValue(new Response("{", {
      status: 409, headers: { "Content-Type": "application/json" },
    }));
    await expect(restartInstanceByName("alice", { uid: "agent-uid", resourceVersion: "7", requestId: "request-00000001" }))
      .rejects.toBeInstanceOf(SyntaxError);
  });

  it("surfaces a network failure without inventing a server rejection code", async () => {
    global.fetch = vi.fn().mockRejectedValue(new TypeError("Connection lost"));
    await expect(restartInstanceByName("alice", { uid: "agent-uid", resourceVersion: "7", requestId: "request-00000001" }))
      .rejects.toThrow("Session expired");
  });

  it.each([
    [409, "instance_changed", true],
    [409, "restart_unavailable", false],
    [400, "invalid_restart_request", false],
    [403, "admission_rejected", false],
    [500, "internal_error", false],
    [503, "instance_changed", false],
    [409, "unknown_error", false],
    [409, undefined, false],
  ])("classifies HTTP %s and code %s without guessing from text", (status, code, rejected) => {
    expect(isRestartRequestRejected(new PortalError("Any message", status, code))).toBe(rejected);
  });

  it("treats an invalid error payload as uncertain", async () => {
    global.fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      error: { code: 409, message: ["instance_changed"] },
    }), { status: 409, headers: { "Content-Type": "application/json" } }));
    try {
      await restartInstanceByName("alice", { uid: "agent-uid", resourceVersion: "7", requestId: "request-00000001" });
      expect.fail("Invalid response must still report an error");
    } catch (error) {
      expect(error).toBeInstanceOf(PortalError);
      expect(error).toMatchObject({ status: 409, message: "Failed to request Agent restart" });
      expect(isRestartRequestRejected(error)).toBe(false);
    }
  });
});
/** Build a minimal mock Response with headers and redirected. */
function mockResponse(init: {
  ok: boolean;
  status: number;
  json?: () => Promise<unknown>;
  redirected?: boolean;
  contentType?: string | null;
}): Response {
  const headers = new Headers();
  if (init.contentType !== null && init.contentType !== undefined) {
    headers.set("content-type", init.contentType);
  } else if (init.contentType === undefined) {
    headers.set("content-type", "application/json");
  }
  // contentType === null means no Content-Type header at all
  return {
    ok: init.ok,
    status: init.status,
    json: init.json ?? (() => Promise.resolve(undefined)),
    redirected: init.redirected ?? false,
    headers,
  } as unknown as Response;
}
describe("getInstance", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("returns instance data on success", async () => {
    const instanceData = {
      alias: "alice",
      displayName: "Alice",
      team: "example-team",
      phase: "Ready",
      createdAt: "2024-01-15T10:00:00Z",
    };

    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: true,
        status: 200,
        json: () => Promise.resolve(instanceData),
      }),
    );

    const result = await getInstance();

    expect(result).toEqual(instanceData);
    expect(fetch).toHaveBeenCalledWith("/api/v1/instance", expect.objectContaining({
      headers: expect.objectContaining({
        "Content-Type": "application/json",
      }),
    }));
  });

  it("returns null on 404", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({ ok: false, status: 404 }),
    );

    const result = await getInstance();
    expect(result).toBeNull();
  });

  it("throws on non-404 error", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({ ok: false, status: 500 }),
    );

    await expect(getInstance()).rejects.toThrow("Failed to get instance");
  });
});
describe("createInstance", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("creates instance successfully", async () => {
    const createdInstance = {
      alias: "alice",
      displayName: "Alice",
      team: "example-team",
      phase: "Pending",
      createdAt: "2024-01-15T10:00:00Z",
    };

    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: true,
        status: 201,
        json: () => Promise.resolve(createdInstance),
      }),
    );

    const result = await createInstance({ agent: {}, team: "example-team" });

    expect(result).toEqual(createdInstance);
    expect(fetch).toHaveBeenCalledWith("/api/v1/instances", expect.objectContaining({
      method: "POST",
      body: JSON.stringify({ agent: {}, team: "example-team" }),
    }));
  });

  it("throws with server error message", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: false,
        status: 409,
        json: () => Promise.resolve({
          error: { code: "instance_exists", message: "Instance already exists" },
        }),
      }),
    );

    await expect(createInstance({ agent: {}, team: "example-team" })).rejects.toThrow(
      "Instance already exists"
    );
  });
});

describe("deleteInstanceByName", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("deletes instance successfully (202, no Content-Type)", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({ ok: true, status: 202, contentType: null }),
    );

    await expect(deleteInstanceByName("abc", { uid: "agent-uid", resourceVersion: "7" })).resolves.toBeUndefined();
    expect(fetch).toHaveBeenCalledWith(
      "/api/v1/instances/abc",
      expect.objectContaining({ method: "DELETE" }),
    );
  });

  it("ignores 404 on delete", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({ ok: false, status: 404 }),
    );

    await expect(deleteInstanceByName("abc", { uid: "agent-uid", resourceVersion: "7" })).resolves.toBeUndefined();
  });

  it("throws with JSON error message on failure", async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 500,
      redirected: false,
      headers: { get: () => "application/json" },
      text: () => Promise.resolve(JSON.stringify({ error: { message: "internal server error" } })),
    });

    await expect(deleteInstanceByName("abc", { uid: "agent-uid", resourceVersion: "7" })).rejects.toThrow("internal server error");
  });

  it("falls back to plain-text error message when body is not JSON", async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 403,
      redirected: false,
      headers: { get: () => "text/plain" },
      text: () => Promise.resolve("Forbidden"),
    });

    await expect(deleteInstanceByName("abc", { uid: "agent-uid", resourceVersion: "7" })).rejects.toThrow("Forbidden");
  });
});

describe("updateInstance", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("updates instance successfully", async () => {
    const updatedInstance = {
      alias: "alice",
      displayName: "Alice Updated",
      team: "other-team",
      phase: "Ready",
      createdAt: "2024-01-15T10:00:00Z",
    };

    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: true,
        status: 200,
        json: () => Promise.resolve(updatedInstance),
      }),
    );

    const result = await updateInstance({ uid: "agent-uid", resourceVersion: "7", displayName: "Alice Updated", team: "other-team" });

    expect(result).toEqual(updatedInstance);
    expect(fetch).toHaveBeenCalledWith("/api/v1/instance", expect.objectContaining({
      method: "PATCH",
      body: JSON.stringify({ uid: "agent-uid", resourceVersion: "7", displayName: "Alice Updated", team: "other-team" }),
    }));
  });

  it("throws with server error message", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: false,
        status: 400,
        json: () => Promise.resolve({
          error: { code: "invalid_team", message: "team must be non-empty when provided" },
        }),
      }),
    );

    await expect(updateInstance({ uid: "agent-uid", resourceVersion: "7", team: "" })).rejects.toThrow(
      "team must be non-empty when provided"
    );
  });

  it("throws with default message when no server message", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: false,
        status: 500,
        json: () => Promise.resolve({}),
      }),
    );

    await expect(updateInstance({ uid: "agent-uid", resourceVersion: "7", displayName: "X" })).rejects.toThrow(
      "Failed to update instance"
    );
  });
});

describe("upgradeInstance", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("upgrades instance successfully", async () => {
    const upgradedInstance = {
      alias: "alice",
      displayName: "Alice",
      team: "example-team",
      phase: "Ready",
      createdAt: "2024-01-15T10:00:00Z",
      upgradeAvailable: false,
    };

    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: true,
        status: 200,
        json: () => Promise.resolve(upgradedInstance),
      }),
    );

    const result = await upgradeInstance({ uid: "agent-uid", resourceVersion: "7" });

    expect(result).toEqual(upgradedInstance);
    expect(fetch).toHaveBeenCalledWith("/api/v1/instance/upgrade", expect.objectContaining({
      method: "POST",
    }));
  });

  it("throws with server error message", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: false,
        status: 500,
        json: () => Promise.resolve({
          error: { code: "internal_error", message: "Failed to upgrade instance" },
        }),
      }),
    );

    await expect(upgradeInstance({ uid: "agent-uid", resourceVersion: "7" })).rejects.toThrow("Failed to upgrade instance");
  });

  it("throws with default message when no server message", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: false,
        status: 500,
        json: () => Promise.resolve({}),
      }),
    );

    await expect(upgradeInstance({ uid: "agent-uid", resourceVersion: "7" })).rejects.toThrow("Failed to upgrade instance");
  });
});

describe("apiFetch session expiry detection", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("throws session expired on network error (CORS-blocked redirect)", async () => {
    global.fetch = vi.fn().mockRejectedValue(new TypeError("Failed to fetch"));

    await expect(getInstance()).rejects.toThrow("Session expired, please refresh the page");
  });

  it("throws session expired when response was redirected", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: true,
        status: 200,
        redirected: true,
        contentType: "text/html",
        json: () => Promise.resolve({}),
      }),
    );

    await expect(getInstance()).rejects.toThrow("Session expired, please refresh the page");
  });

  it("throws session expired when response is HTML (OAuth2 Proxy sign-in page)", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({
        ok: true,
        status: 200,
        contentType: "text/html",
        json: () => Promise.resolve({}),
      }),
    );

    await expect(getInstance()).rejects.toThrow("Session expired, please refresh the page");
  });

  it("does not trigger session expiry for normal JSON error responses", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({ ok: false, status: 500 }),
    );

    await expect(getInstance()).rejects.toThrow("Failed to get instance");
  });

  it("does not trigger session expiry for 202 with no Content-Type", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({ ok: true, status: 202, contentType: null }),
    );

    await expect(deleteInstanceByName("abc", { uid: "agent-uid", resourceVersion: "7" })).resolves.toBeUndefined();
  });

  it("throws session expired on 401 (OAuth2 Proxy API route)", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      mockResponse({ ok: false, status: 401, json: () => Promise.resolve({}) }),
    );

    await expect(getInstance()).rejects.toThrow("Session expired, please refresh the page");
  });
});
