// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  deleteAdminInstance,
  getAdminInstance,
  getAdminInstanceStatus,
  updateAdminInstance,
} from "./adminInstance";
import { PortalError } from "./fetch";

function response(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

describe("admin instance API", () => {
  const fetchMock = vi.fn();
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
  });

  it("uses encoded agent names and the reviewed version for writes", async () => {
    const request = { uid: "agent-uid", resourceVersion: "9", displayName: "Example", agent: { port: 8080 } };
    fetchMock.mockResolvedValueOnce(response(200, { name: "agent one" }));
    await updateAdminInstance("agent one", request);
    expect(fetchMock).toHaveBeenLastCalledWith("/api/v1/admin/instances/agent%20one", expect.objectContaining({ method: "PATCH", body: JSON.stringify(request) }));

    fetchMock.mockResolvedValueOnce(new Response(null, { status: 202 }));
    await deleteAdminInstance("agent one", { uid: "agent-uid", resourceVersion: "9" });
    expect(fetchMock).toHaveBeenLastCalledWith("/api/v1/admin/instances/agent%20one", expect.objectContaining({
      method: "DELETE", body: JSON.stringify({ uid: "agent-uid", resourceVersion: "9" }),
    }));
  });

  it("treats a missing detail as an absent Agent and retains diagnostics errors", async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
    await expect(getAdminInstance("gone")).resolves.toBeNull();

    const diagnostics = { uid: "agent-uid", conditions: [], pods: [], events: [], nsEvents: [], errors: [{ section: "pods", message: "unavailable" }] };
    fetchMock.mockResolvedValueOnce(response(200, diagnostics));
    await expect(getAdminInstanceStatus("agent")).resolves.toEqual(diagnostics);
  });

  it("surfaces Portal error messages and status for conflicts", async () => {
    fetchMock.mockResolvedValue(response(409, { error: { code: "instance_changed", message: "Reload and review the Agent." } }));
    await expect(updateAdminInstance("agent", { uid: "agent-uid", resourceVersion: "9" })).rejects.toEqual(
      new PortalError("Reload and review the Agent.", 409, "instance_changed"),
    );
  });
});
