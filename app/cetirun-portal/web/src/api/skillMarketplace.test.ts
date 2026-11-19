// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { getSkillMarketplace } from "./skillMarketplace";
import { PortalError } from "./fetch";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

describe("skillMarketplace API", () => {
  const fetchMock = vi.fn();
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
  });

  it("reads the administrators' catalog", async () => {
    const body = { enabled: true, known: false, catalog: null };
    fetchMock.mockResolvedValue(jsonResponse(200, body));
    await expect(getSkillMarketplace()).resolves.toEqual(body);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/admin/skill-marketplace");
  });

  it("surfaces the Portal's error with its status", async () => {
    const message = "The Skill Marketplace catalog could not be read. Check the Portal logs.";
    fetchMock.mockResolvedValue(jsonResponse(503, { error: { code: "skill_marketplace_unavailable", message } }));
    const error = await getSkillMarketplace().catch((err: unknown) => err);
    expect(error).toBeInstanceOf(PortalError);
    expect(error).toMatchObject({ status: 503, message });
  });
});
