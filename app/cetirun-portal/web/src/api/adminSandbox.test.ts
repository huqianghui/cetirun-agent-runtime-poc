// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { createSandbox, getSandboxStorageOptions, updateSandbox, type SandboxUpsertRequest } from "./adminSandbox";

const request: SandboxUpsertRequest = {
  name: "example",
  resources: {
    agent: {
      requests: { cpu: "125m", memory: "128Mi", "ephemeral-storage": "128Mi" },
      limits: { cpu: "1", memory: "1Gi", "ephemeral-storage": "1Gi" },
    },
    helpers: {
      requests: { cpu: "100m", memory: "128Mi", "ephemeral-storage": "128Mi" },
      limits: { cpu: "500m", memory: "2Gi", "ephemeral-storage": "512Mi" },
    },
  },
  storage: {
    home: { storageClassName: "example-csi", size: "10Gi", accessMode: "ReadWriteOncePod" },
    scratch: { sizeLimit: "1Gi" },
  },
};

describe("admin Sandbox defaults", () => {
  const fetchMock = vi.fn();
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
  });

  it.each([undefined, null, "kata", "gvisor", "container"] as const)("preserves the request type %s and every resource field", async (type) => {
    const body: SandboxUpsertRequest = { ...request, type };
    const stored = { ...request, type: type ?? "kata", uid: "example-uid", resourceVersion: "42", ready: "Unknown", deleting: false };
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(stored), {
      status: 201, headers: { "content-type": "application/json" },
    }));
    expect(await createSandbox(body)).toEqual(stored);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/admin/sandboxes");
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ method: "POST", body: JSON.stringify(body) });

    const update: SandboxUpsertRequest = { ...body, uid: stored.uid, resourceVersion: stored.resourceVersion };
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(stored), {
      status: 200, headers: { "content-type": "application/json" },
    }));
    expect(await updateSandbox("example", update)).toEqual(stored);
    expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/admin/sandboxes/example");
    expect(fetchMock.mock.calls[1][1]).toMatchObject({ method: "PUT", body: JSON.stringify(update) });
  });

  it("keeps validation failures explicit instead of selecting a fallback runtime", async () => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({
      error: { code: "invalid_sandbox", message: "type must be kata, gvisor or container" },
    }), { status: 400, headers: { "content-type": "application/json" } }));
    await expect(createSandbox(request)).rejects.toThrow("type must be kata, gvisor or container");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("loads approved StorageClasses and runtime-specific HOME access modes", async () => {
    const options = {
      storageClasses: [{ name: "managed-csi", provisioner: "disk.csi.azure.com", allowVolumeExpansion: true }],
      defaultStorageClassName: "managed-csi",
      homeAccessModes: { kata: ["ReadWriteOncePod"] as const },
    };
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(options), {
      status: 200, headers: { "content-type": "application/json" },
    }));
    await expect(getSandboxStorageOptions()).resolves.toEqual(options);
    expect(fetchMock).toHaveBeenCalledWith("/api/v1/admin/sandbox-storage-options", expect.objectContaining({
      headers: { "Content-Type": "application/json" },
    }));
  });
});
