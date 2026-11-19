// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { PropsWithChildren } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fetchInstanceByName, restartInstanceByName } from "../api/instance";
import { PortalError } from "../api/fetch";
import type { Instance } from "../types/instance";
import { useInstanceByNameQuery, useRestartInstanceByName } from "./useInstance";

vi.mock("../api/instance", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api/instance")>(),
  fetchInstanceByName: vi.fn(),
  restartInstanceByName: vi.fn(),
}));
vi.mock("../context/MeContext", () => ({
  useMe: () => ({ me: { alias: "alice", email: "alice@example.com" }, error: null }),
}));

const instance: Instance = {
  instanceName: "alice", alias: "alice", displayName: "Alice", team: "example-team",
  uid: "agent-uid", resourceVersion: "7", generation: 2, observedGeneration: 2,
  agent: { type: "openclaw" }, phase: "Ready", createdAt: "2026-01-01T00:00:00Z",
  upgradeAvailable: false, restartAvailable: true,
};

describe("useRestartInstanceByName", () => {
  beforeEach(() => vi.resetAllMocks());

  it.each(["rejected", "uncertain", "accepted"])("refreshes the displayed Agent after a %s result without resubmitting", async (outcome) => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const wrapper = ({ children }: PropsWithChildren) =>
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    const request = { uid: instance.uid, resourceVersion: instance.resourceVersion, requestId: "request-00000001" };
    const current: Instance = { ...instance, resourceVersion: "8",
      ...(outcome === "rejected" ? {} : { restartRequestId: request.requestId, restartAvailable: false }) };
    vi.mocked(fetchInstanceByName).mockResolvedValueOnce(instance).mockResolvedValue(current);
    const failure = outcome === "rejected"
      ? new PortalError("Agent changed", 409, "instance_changed") : new Error("Connection interrupted");
    if (outcome === "accepted") vi.mocked(restartInstanceByName).mockResolvedValue(current);
    else vi.mocked(restartInstanceByName).mockRejectedValue(failure);
    const hook = renderHook(() => ({
      instance: useInstanceByNameQuery(instance.instanceName),
      restart: useRestartInstanceByName(instance.instanceName),
    }), { wrapper });
    try {
      await waitFor(() => expect(hook.result.current.instance.data?.resourceVersion).toBe("7"));
      await act(async () => {
        const result = hook.result.current.restart.mutateAsync(request);
        if (outcome === "accepted") await expect(result).resolves.toEqual(current);
        else await expect(result).rejects.toBe(failure);
      });
      await waitFor(() => expect(hook.result.current.instance.data?.resourceVersion).toBe("8"));
      expect(restartInstanceByName).toHaveBeenCalledExactlyOnceWith(instance.instanceName, request);
      expect(hook.result.current.restart.variables).toEqual(request);
      if (outcome !== "accepted") expect(hook.result.current.restart.error).toBe(failure);
    } finally {
      hook.unmount();
      queryClient.clear();
    }
  });
});
