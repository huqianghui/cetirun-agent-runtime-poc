// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { deletePolicy, PolicyConflictError, updatePolicy, type AgentPolicyUpsertRequest } from "./adminPolicy";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

const request: AgentPolicyUpsertRequest = {
  uid: "policy-uid",
  resourceVersion: "42",
  harnesses: { allowed: ["openclaw"] },
  agentIdentity: { allowed: true, required: false, credentialServices: ["azure-resource-manager"] },
  entraID: { enabled: false },
};

describe("admin policy API", () => {
  const fetchMock = vi.fn();
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
  });

  it("sends the version read by the page on update and delete", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(200, { name: "standard" }));
    await updatePolicy("standard policy", request);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/admin/policies/standard%20policy");
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ method: "PUT", body: JSON.stringify(request) });

    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));
    await deletePolicy({ name: "standard policy", uid: "policy-uid", resourceVersion: "42" });
    expect(fetchMock.mock.calls[1][1]).toMatchObject({
      method: "DELETE",
      body: JSON.stringify({ uid: "policy-uid", resourceVersion: "42" }),
    });
  });

  it("returns the conflict message for a stale policy", async () => {
    fetchMock.mockResolvedValue(jsonResponse(409, { error: { code: "policy_changed", message: "Reload and try again." } }));
    await expect(updatePolicy("standard", request)).rejects.toEqual(new PolicyConflictError("Reload and try again."));
  });

  it("rejects an unavailable policy deletion", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(503, { error: { code: "policy_change_unavailable", message: "Recover the recorded operation before retrying." } }),
    );
    await expect(deletePolicy({ name: "standard", uid: "policy-uid", resourceVersion: "42" })).rejects.toThrow(
      "Recover the recorded operation before retrying.",
    );
  });
});
