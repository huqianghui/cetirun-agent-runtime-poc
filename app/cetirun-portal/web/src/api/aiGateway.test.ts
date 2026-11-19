// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { cancelCopilotLogin, getAIGatewayCatalog, pollCopilotLogin, startCopilotLogin } from "./aiGateway";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

describe("aiGateway API", () => {
  const fetchMock = vi.fn();
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
  });

  it("reads the shared-model catalog", async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { known: true, sharedModels: [{ name: "gpt-6" }] }));
    await expect(getAIGatewayCatalog()).resolves.toEqual({ known: true, sharedModels: [{ name: "gpt-6" }] });
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/ai-gateway/catalog");
  });

  it("starts, polls and cancels a sign-in for the named agent", async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(200, { state: "pending", userCode: "ABCD-1234" }));
    await expect(startCopilotLogin("alice gw", "agent-uid")).resolves.toMatchObject({ state: "pending" });
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/instances/alice%20gw/copilot/login");
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ method: "POST", body: JSON.stringify({ uid: "agent-uid" }) });

    fetchMock.mockResolvedValueOnce(jsonResponse(200, { state: "succeeded" }));
    await expect(pollCopilotLogin("alice-gw")).resolves.toMatchObject({ state: "succeeded" });

    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));
    await expect(cancelCopilotLogin("alice-gw", "agent-uid")).resolves.toBeUndefined();
    expect(fetchMock.mock.calls[2][1]).toMatchObject({ method: "DELETE", body: JSON.stringify({ uid: "agent-uid" }) });
  });

  it("surfaces the Portal's error message", async () => {
    fetchMock.mockResolvedValue(jsonResponse(409, { error: { code: "copilot_not_ready", message: "Not set up yet." } }));
    await expect(startCopilotLogin("alice-gw", "agent-uid")).rejects.toThrow("Not set up yet.");
  });
});
