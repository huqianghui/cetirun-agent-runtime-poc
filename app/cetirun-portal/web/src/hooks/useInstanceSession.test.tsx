// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { PropsWithChildren } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { fetchInstanceByName } from "../api/instance";
import { SESSION_EXPIRED_MESSAGE } from "../api/fetch";
import type { Instance } from "../types/instance";
import { useInstanceByNameQuery } from "./useInstance";

let viewerError: Error | null = null;
vi.mock("../context/MeContext", () => ({
  useMe: () => ({ me: { alias: "alice", email: "alice@example.com" }, error: viewerError }),
}));
vi.mock("../api/instance", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api/instance")>(),
  fetchInstanceByName: vi.fn(),
}));

afterEach(() => { viewerError = null; vi.resetAllMocks(); });

it("keeps the same viewer cache after session expiry without hiding the refetch error", async () => {
  const instance: Instance = {
    instanceName: "alice", alias: "alice", displayName: "Alice", team: "example",
    uid: "synthetic-agent", resourceVersion: "1", agent: { type: "openclaw" },
    phase: "Ready", createdAt: "2026-01-01T00:00:00Z", upgradeAvailable: false,
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  const wrapper = ({ children }: PropsWithChildren) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  vi.mocked(fetchInstanceByName).mockResolvedValue(instance);
  const hook = renderHook(() => {
    const query = useInstanceByNameQuery("alice");
    return { data: query.data, error: query.error, refetch: query.refetch };
  }, { wrapper });
  try {
    await waitFor(() => expect(hook.result.current.data).toEqual(instance));
    viewerError = new Error(SESSION_EXPIRED_MESSAGE);
    hook.rerender();
    expect(hook.result.current.data).toEqual(instance);
    vi.mocked(fetchInstanceByName).mockRejectedValue(viewerError);
    await hook.result.current.refetch();
    await waitFor(() => expect(hook.result.current.error?.message).toBe(SESSION_EXPIRED_MESSAGE));
    expect(hook.result.current.data).toEqual(instance);
    expect(fetchInstanceByName).toHaveBeenCalledTimes(2);
  } finally {
    hook.unmount();
    client.clear();
  }
});
