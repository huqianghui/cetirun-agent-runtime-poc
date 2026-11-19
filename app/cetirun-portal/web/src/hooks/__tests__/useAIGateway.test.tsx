// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useCopilotGatewayLogin } from "../useAIGateway";

const api = vi.hoisted(() => ({
  startCopilotLogin: vi.fn(),
  pollCopilotLogin: vi.fn(),
  cancelCopilotLogin: vi.fn(),
  getAIGatewayCatalog: vi.fn(),
}));
vi.mock("../../api/aiGateway", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/aiGateway")>()),
  ...api,
}));

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

const PENDING = { state: "pending", userCode: "WXYZ-9876", verificationUri: "https://github.com/login/device", interval: 5 };

describe("useCopilotGatewayLogin", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    Object.values(api).forEach((fn) => fn.mockReset());
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("drops a poll answered after the sign-in was cancelled", async () => {
    const unanswered: Array<(value: unknown) => void> = [];
    api.startCopilotLogin.mockResolvedValue(PENDING);
    api.cancelCopilotLogin.mockResolvedValue(undefined);
    api.pollCopilotLogin.mockImplementation(() => new Promise((resolve) => unanswered.push(resolve)));
    const { result } = renderHook(() => useCopilotGatewayLogin("alice-gw", "agent-uid"), { wrapper });

    await act(async () => {
      await result.current.start();
    });
    expect(result.current.login?.state).toBe("pending");
    expect(api.startCopilotLogin).toHaveBeenCalledWith("alice-gw", "agent-uid");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });
    expect(unanswered).toHaveLength(1);

    await act(async () => {
      await result.current.cancel();
    });
    await act(async () => {
      unanswered[0](PENDING);
      await vi.advanceTimersByTimeAsync(20_000);
    });
    expect(result.current.login).toBeNull();
    expect(api.cancelCopilotLogin).toHaveBeenCalledWith("alice-gw", "agent-uid");
    expect(api.pollCopilotLogin).toHaveBeenCalledTimes(1);
  });

  it("does not resume unless asked, and resumes only a pending sign-in", async () => {
    api.pollCopilotLogin.mockResolvedValue({ state: "succeeded", message: "done" });
    const quiet = renderHook(() => useCopilotGatewayLogin("alice-gw", "agent-uid", true), { wrapper });
    await act(async () => {
      await Promise.resolve();
    });
    expect(quiet.result.current.login).toBeNull();

    api.pollCopilotLogin.mockReset();
    renderHook(() => useCopilotGatewayLogin("alice-gw", "agent-uid"), { wrapper });
    await act(async () => {
      await Promise.resolve();
    });
    expect(api.pollCopilotLogin).not.toHaveBeenCalled();
  });

  it("keeps polling a sign-in being completed past its code's expiry", async () => {
    // The Portal reports a sign-in whose token is being stored as pending,
    // with the expiry of its code, for up to two minutes after that.
    const expired = { ...PENDING, expiresAt: new Date(Date.now() - 60_000).toISOString() };
    api.pollCopilotLogin
      .mockResolvedValueOnce(expired)
      .mockResolvedValueOnce(expired)
      .mockResolvedValueOnce({ state: "succeeded", message: "Signed in" });
    const { result } = renderHook(() => useCopilotGatewayLogin("alice-gw", "agent-uid", true), { wrapper });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.login?.state).toBe("pending");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(api.pollCopilotLogin).toHaveBeenCalledTimes(3);
    expect(result.current.login?.state).toBe("succeeded");
  });

  it("keeps following a sign-in whose cancel the Portal refused", async () => {
    // The Portal refuses to cancel a sign-in whose token it is storing, and
    // reports it as pending until it has an outcome.
    api.startCopilotLogin.mockResolvedValue(PENDING);
    api.cancelCopilotLogin.mockRejectedValue(new Error("The GitHub Copilot sign-in is finishing. Try again in a moment."));
    api.pollCopilotLogin
      .mockResolvedValueOnce(PENDING)
      .mockResolvedValueOnce({ state: "succeeded", message: "Signed in to GitHub Copilot: 2 models." });
    const { result } = renderHook(() => useCopilotGatewayLogin("alice-gw", "agent-uid"), { wrapper });
    await act(async () => {
      await result.current.start();
    });
    await act(async () => {
      await result.current.cancel();
    });
    expect(result.current.error).toContain("finishing");
    expect(result.current.login?.state).toBe("pending");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });
    expect(api.pollCopilotLogin).toHaveBeenCalledTimes(2);
    expect(result.current.login?.state).toBe("succeeded");
    expect(result.current.error).toBeNull();
  });
});