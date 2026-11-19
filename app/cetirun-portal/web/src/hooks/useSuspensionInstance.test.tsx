// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { PropsWithChildren } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fetchInstanceByName, setInstanceSuspendedByName } from "../api/instance";
import { PortalError } from "../api/fetch";
import type { Instance } from "../types/instance";
import { useInstanceByNameQuery, useSetInstanceSuspendedByName } from "./useInstance";

vi.mock("../api/instance", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api/instance")>(),
  fetchInstanceByName: vi.fn(),
  setInstanceSuspendedByName: vi.fn(),
}));
vi.mock("../context/MeContext", () => ({
  useMe: () => ({ me: { alias: "alice", email: "alice@example.com" }, error: null }),
}));

const instance: Instance = {
  instanceName: "alice", alias: "alice", displayName: "Alice", team: "example-team",
  uid: "agent-uid", resourceVersion: "7", generation: 2, observedGeneration: 2,
  agent: { type: "openclaw" }, phase: "Ready", createdAt: "2026-01-01T00:00:00Z",
  upgradeAvailable: false, suspended: false, suspendAvailable: true, restartAvailable: true,
};

describe("useSetInstanceSuspendedByName", () => {
  beforeEach(() => vi.resetAllMocks());

  it.each([true, false])("preserves newer status when a suspended=%s reply arrives late", async (suspended) => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: PropsWithChildren) =>
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    const key = ["instance", instance.instanceName, "alice:alice@example.com"];
    const request = { uid: instance.uid, resourceVersion: "requested-version", suspended };
    const accepted: Instance = { ...instance, resourceVersion: "intent-version", generation: 3, suspended,
      phase: suspended ? "Ready" : "Suspended", restartAvailable: false,
      suspendAvailable: !suspended, resumeAvailable: suspended, restartRequestId: "restart-00000001" };
    const observed: Instance = { ...accepted, resourceVersion: "status-version", observedGeneration: 3,
      phase: suspended ? "Suspended" : "Failed",
      conditions: [{ type: "Ready", status: "False", reason: suspended ? "Suspended" : "RecoveryFailed", message: "Synthetic status" }] };
    queryClient.setQueryData(key, { ...instance, resourceVersion: request.resourceVersion, suspended: !suspended });
    let resolveRequest!: (value: Instance) => void;
    vi.mocked(setInstanceSuspendedByName).mockReturnValue(new Promise((resolve) => { resolveRequest = resolve; }));
    const hook = renderHook(() => useSetInstanceSuspendedByName(instance.instanceName), { wrapper });
    try {
      act(() => hook.result.current.mutate(request));
      await waitFor(() => expect(setInstanceSuspendedByName).toHaveBeenCalledExactlyOnceWith(instance.instanceName, request));
      act(() => queryClient.setQueryData(key, observed));
      await act(async () => resolveRequest(accepted));
      await waitFor(() => expect(hook.result.current.isSuccess).toBe(true));
      expect(queryClient.getQueryData(key)).toEqual(observed);
      expect(queryClient.getQueryState(key)?.isInvalidated).toBe(true);
      expect(setInstanceSuspendedByName).toHaveBeenCalledTimes(1);
    } finally {
      hook.unmount();
      queryClient.clear();
    }
  });

  it.each(["accepted", "uncertain", "rejected", "replacement", "newer intent"])(
    "refreshes after %s without replaying or overwriting newer identity/intent", async (outcome) => {
      const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: 3 } } });
      const wrapper = ({ children }: PropsWithChildren) =>
        <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
      const request = { uid: instance.uid, resourceVersion: "7", suspended: true };
      const accepted: Instance = { ...instance, resourceVersion: "8", generation: 3, suspended: true,
        suspendAvailable: false, resumeAvailable: true, restartAvailable: false };
      const current = outcome === "replacement" ? { ...instance, uid: "new-uid", resourceVersion: "10" }
        : outcome === "newer intent" ? { ...instance, generation: 4, resourceVersion: "10" }
        : outcome === "rejected" ? { ...instance, resourceVersion: "8" } : accepted;
      vi.mocked(fetchInstanceByName).mockResolvedValueOnce(instance).mockResolvedValue(current);
      const failure = outcome === "rejected" ? new PortalError("Agent changed", 409, "instance_changed") : new Error("Connection interrupted");
      const failed = outcome === "uncertain" || outcome === "rejected";
      if (failed) vi.mocked(setInstanceSuspendedByName).mockRejectedValue(failure);
      else vi.mocked(setInstanceSuspendedByName).mockResolvedValue(accepted);
      const hook = renderHook(() => ({
        instance: useInstanceByNameQuery(instance.instanceName),
        change: useSetInstanceSuspendedByName(instance.instanceName),
      }), { wrapper });
      try {
        await waitFor(() => expect(hook.result.current.instance.data?.resourceVersion).toBe("7"));
        if (outcome === "replacement" || outcome === "newer intent") {
          queryClient.setQueryData(["instance", instance.instanceName, "alice:alice@example.com"], current);
        }
        await act(async () => {
          const result = hook.result.current.change.mutateAsync(request);
          if (failed) await expect(result).rejects.toBe(failure);
          else await expect(result).resolves.toEqual(accepted);
        });
        await waitFor(() => expect(hook.result.current.instance.data).toEqual(current));
        expect(setInstanceSuspendedByName).toHaveBeenCalledExactlyOnceWith(instance.instanceName, request);
        expect(hook.result.current.change.variables).toEqual(request);
      } finally {
        hook.unmount();
        queryClient.clear();
      }
    },
  );
});
