// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, expect, it, vi } from "vitest";
import { instanceETag, updateRegistryAuth } from "./registry";
import { createInstance, updateInstanceByName } from "./instance";
import type { Instance } from "../types/instance";

describe("registry requests", () => {
  const instance = { instanceName: "example", uid: "example-uid", resourceVersion: "2" } as Instance;
  it("uses separate write-only PUT and DELETE with UID/resourceVersion preconditions", async () => {
    global.fetch = vi.fn().mockImplementation(() => Promise.resolve(new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } })));
    await updateRegistryAuth(instance, { username: "example", password: "synthetic-token" });
    expect(fetch).toHaveBeenLastCalledWith("/api/v1/instances/example/registry-auth", expect.objectContaining({ method: "PUT", headers: expect.objectContaining({ "If-Match": instanceETag(instance) }) }));
    await updateRegistryAuth(instance);
    expect(fetch).toHaveBeenLastCalledWith("/api/v1/instances/example/registry-auth", expect.objectContaining({ method: "DELETE", body: undefined }));
  });

  it("sends the stable creation request ID as a header, not a CR field", async () => {
    global.fetch = vi.fn().mockResolvedValue(new Response("{}", { status: 201, headers: { "Content-Type": "application/json" } }));
    await createInstance({ agent: { type: "hermes", image: "registry.example.com/agent:1" }, registryAuth: { username: "example", password: "synthetic-token" }, requestId: "synthetic-id-123456" });
    const [, options] = vi.mocked(fetch).mock.calls[0];
    expect(options?.headers).toMatchObject({ "Idempotency-Key": "synthetic-id-123456" });
    expect(JSON.parse(String(options?.body))).not.toHaveProperty("requestId");
  });

  it("keeps ordinary edits on the management body precondition contract", async () => {
    global.fetch = vi.fn().mockResolvedValue(new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }));
    await updateInstanceByName(instance.instanceName, { uid: instance.uid, resourceVersion: instance.resourceVersion, agent: { port: null } });
    const [, options] = vi.mocked(fetch).mock.calls[0];
    expect(JSON.parse(String(options?.body))).toEqual({ uid: instance.uid, resourceVersion: instance.resourceVersion, agent: { port: null } });
    expect(options?.headers).not.toHaveProperty("If-Match");
  });
});
