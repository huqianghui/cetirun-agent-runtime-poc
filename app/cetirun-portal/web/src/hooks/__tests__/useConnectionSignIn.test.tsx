// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";
import { useConnectionSignIn } from "../useConnectionSignIn";
import { getConnectionAuthorization, listConnections } from "../../api/toolAuthorization";
vi.mock("../../api/toolAuthorization", () => ({ getConnectionAuthorization: vi.fn(), listConnections: vi.fn() }));
const authorization = { authorizationId: "tx-1", expiresAt: new Date(Date.now() + 600000).toISOString(), authorizationUrl: "https://provider.example.com/signin" };
const connection = { id: "canonical", mode: "delegated", status: "connected" };
describe("OAuth popup transaction", () => {
 let popup: Window;
 beforeEach(() => {
  popup = { closed: false, close: vi.fn(), location: { replace: vi.fn() } } as unknown as Window;
  vi.spyOn(window, "open").mockReturnValue(popup);
  vi.mocked(getConnectionAuthorization).mockReset().mockResolvedValue({ authorizationId: "tx-1", status: "complete", connectionId: "canonical" });
  vi.mocked(listConnections).mockReset().mockResolvedValue([connection as never]);
 });
 afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });
 function setup() { const client = new QueryClient(); return renderHook(({ scope }) => useConnectionSignIn(scope), { initialProps: { scope: "agent-a" }, wrapper: ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider> }); }
 function message(changes = {}) { window.dispatchEvent(new MessageEvent("message", { origin: window.location.origin, source: popup, data: { type: "cetirun-connection", authorizationId: "tx-1", status: "complete" }, ...changes })); }
 it("checks origin, source and transaction before querying the owner-scoped result", async () => {
  const hook = setup(); const complete = vi.fn();
  await act(async () => hook.result.current.begin(async () => authorization, complete));
  act(() => { message({ origin: "https://attacker.example.com" }); message({ source: window }); message({ data: { type: "cetirun-connection", authorizationId: "other" } }); });
  expect(getConnectionAuthorization).not.toHaveBeenCalled(); expect(complete).not.toHaveBeenCalled();
  await act(async () => message());
  await waitFor(() => expect(complete).toHaveBeenCalledWith(connection));
  expect(getConnectionAuthorization).toHaveBeenCalledWith("tx-1", expect.any(AbortSignal));
 });
 it("does not trust a successful message when the backend reports failure", async () => {
  vi.mocked(getConnectionAuthorization).mockResolvedValue({ authorizationId: "tx-1", status: "failed" });
  const hook = setup(); const complete = vi.fn();
  await act(async () => hook.result.current.begin(async () => authorization, complete)); await act(async () => message());
  expect(complete).not.toHaveBeenCalled(); expect(listConnections).not.toHaveBeenCalled();
 });
 it.each(["cancel", "switch", "unmount"])("ignores an in-flight verification after %s", async (action) => {
  let resolve!: (value: Awaited<ReturnType<typeof getConnectionAuthorization>>) => void;
  vi.mocked(getConnectionAuthorization).mockReturnValue(new Promise((r) => { resolve = r; }));
  const hook = setup(); const complete = vi.fn(); await act(async () => hook.result.current.begin(async () => authorization, complete)); act(() => message());
  act(() => { if (action === "cancel") hook.result.current.cancel(); else if (action === "switch") hook.rerender({ scope: "agent-b" }); else hook.unmount(); });
  await act(async () => resolve({ authorizationId: "tx-1", status: "complete", connectionId: "canonical" }));
  expect(complete).not.toHaveBeenCalled(); expect(listConnections).not.toHaveBeenCalled();
 });
 it("verifies a closed popup before reporting an unfinished transaction as cancelled", async () => {
  vi.mocked(getConnectionAuthorization).mockResolvedValue({ authorizationId: "tx-1", status: "pending" });
  const hook = setup(); const complete = vi.fn(); await act(async () => hook.result.current.begin(async () => authorization, complete));
  Object.defineProperty(popup, "closed", { value: true });
  await waitFor(() => expect(hook.result.current.pending).toBe(false));
  expect(complete).not.toHaveBeenCalled(); expect(listConnections).not.toHaveBeenCalled();
  expect(getConnectionAuthorization).toHaveBeenCalledWith("tx-1", expect.any(AbortSignal));
  expect(hook.result.current.message).toContain("cancelled");
 });
 it("completes a committed transaction when the popup closes before its queued message arrives", async () => {
  vi.useFakeTimers();
  let resolve!: (value: Awaited<ReturnType<typeof getConnectionAuthorization>>) => void;
  vi.mocked(getConnectionAuthorization).mockReturnValue(new Promise((r) => { resolve = r; }));
  const hook = setup(); const complete = vi.fn();
  await act(async () => hook.result.current.begin(async () => authorization, complete));
  Object.defineProperty(popup, "closed", { value: true });
  await act(async () => { await vi.advanceTimersByTimeAsync(300); });
  expect(hook.result.current.pending).toBe(true);
  expect(getConnectionAuthorization).toHaveBeenCalledTimes(1);
  act(() => message());
  expect(getConnectionAuthorization).toHaveBeenCalledTimes(1);
  await act(async () => resolve({ authorizationId: "tx-1", status: "complete", connectionId: "canonical" }));
  expect(complete).toHaveBeenCalledExactlyOnceWith(connection);
  expect(hook.result.current.message).toBe("");
  expect(hook.result.current.pending).toBe(false);
 });
 it("recovers a completed sign-in even if its popup message never arrives", async () => {
  const hook = setup(); const complete = vi.fn();
  await act(async () => hook.result.current.begin(async () => authorization, complete));
  Object.defineProperty(popup, "closed", { value: true });
  await waitFor(() => expect(complete).toHaveBeenCalledExactlyOnceWith(connection));
  expect(hook.result.current.message).toBe("");
 });
 it("accepts an installation-required GitHub App connection for installation selection", async () => {
  const installation = { id: "github-app", mode: "installation", status: "installation_required" };
  vi.mocked(getConnectionAuthorization).mockResolvedValue({ authorizationId: "tx-1", status: "complete", connectionId: "github-app" });
  vi.mocked(listConnections).mockResolvedValue([installation as never]);
  const hook = setup(); const complete = vi.fn();
  await act(async () => hook.result.current.begin(async () => authorization, complete, "installation"));
  await act(async () => message());
  await waitFor(() => expect(complete).toHaveBeenCalledWith(installation));
 });
 it("accepts a reauthorized GitHub App connection without changing its mode", async () => {
  const installation = { id: "github-app", mode: "installation", status: "connected", github: { installationId: 7 } };
  vi.mocked(getConnectionAuthorization).mockResolvedValue({ authorizationId: "tx-1", status: "complete", connectionId: "github-app" });
  vi.mocked(listConnections).mockResolvedValue([installation as never]);
  const hook = setup(); const complete = vi.fn();
  await act(async () => hook.result.current.begin(async () => authorization, complete, "installation"));
  await act(async () => message());
  await waitFor(() => expect(complete).toHaveBeenCalledWith(installation));
 });
});
