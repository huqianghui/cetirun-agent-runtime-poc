// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PortalError } from "../api/aiGateway";
import type { Instance, RestartInstanceRequest } from "../types/instance";
import { RestartAgentButton } from "./RestartAgentButton";

const mutation = vi.hoisted(() => ({
  mutate: vi.fn(), reset: vi.fn(), isPending: false, isError: false,
  error: new Error("Connection interrupted"), data: undefined as Instance | undefined,
  variables: undefined as RestartInstanceRequest | undefined,
}));
vi.mock("../hooks/useInstance", () => ({ useRestartInstanceByName: () => mutation }));

const instance: Instance = {
  instanceName: "alice", alias: "alice", displayName: "Alice", team: "example-team",
  uid: "agent-uid", resourceVersion: "7", generation: 2, observedGeneration: 2,
  agent: { type: "openclaw" }, phase: "Ready", createdAt: "2026-01-01T00:00:00Z",
  upgradeAvailable: false, restartAvailable: true,
};

describe("RestartAgentButton", () => {
  beforeEach(() => {
    mutation.mutate.mockClear();
    mutation.reset.mockClear();
    mutation.isPending = false;
    mutation.isError = false;
    mutation.error = new Error("Connection interrupted");
    mutation.data = undefined;
    mutation.variables = undefined;
    mutation.mutate.mockImplementation((request: RestartInstanceRequest) => { mutation.variables = request; });
    mutation.reset.mockImplementation(() => {
      mutation.isError = false;
      mutation.data = undefined;
      mutation.variables = undefined;
    });
  });
  afterEach(() => vi.restoreAllMocks());

  it("confirms a retained restart using the originally displayed Agent version", () => {
    const view = render(<RestartAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    expect(screen.getByText(/HOME files and the existing Agent are retained/)).toBeInTheDocument();
    view.rerender(<RestartAgentButton instance={{ ...instance, uid: "replacement-uid", resourceVersion: "8" }} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    expect(mutation.mutate).toHaveBeenCalledWith({
      uid: "agent-uid", resourceVersion: "7", requestId: expect.stringMatching(/^[a-f0-9-]{36}$/),
    }, expect.anything());
  });

  it("reuses the identical request ID and version after an uncertain response", () => {
    const view = render(<RestartAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    const first = mutation.mutate.mock.calls[0][0];
    mutation.isError = true;
    view.rerender(<RestartAgentButton instance={{ ...instance, resourceVersion: "8" }} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Connection interrupted");
    fireEvent.click(screen.getByRole("button", { name: "Retry request" }));
    expect(mutation.mutate.mock.calls[1][0]).toEqual(first);
  });

  it("does not turn a persisted pending request into another restart after page reload", () => {
    render(<RestartAgentButton instance={{ ...instance, restartRequestId: "request-00000001" }} />);
    expect(screen.getByRole("button", { name: "Restart Agent" })).toBeDisabled();
    expect(screen.getByRole("status")).toHaveTextContent("waiting for the replacement Pod");
  });

  it("retires an uncertain request when polling confirms completion before a new restart", () => {
    const view = render(<RestartAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    const first = mutation.mutate.mock.calls[0][0];
    mutation.isError = true;
    view.rerender(<RestartAgentButton instance={{ ...instance, resourceVersion: "9", generation: 3,
      restartRequestId: first.requestId, observedRestartRequestId: first.requestId }} />);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    const next = mutation.mutate.mock.calls[1][0];
    expect(next.resourceVersion).toBe("9");
    expect(next.requestId).not.toBe(first.requestId);
  });

  it("does not offer restart when the server reports it unavailable", () => {
    render(<RestartAgentButton instance={{ ...instance, phase: "Suspended", restartAvailable: false }} />);
    expect(screen.getByRole("button", { name: "Restart Agent" })).toBeDisabled();
  });

  it.each(["Cancel", "Escape"])("discards an unsent draft on %s and uses the newly displayed version", (dismissal) => {
    const uuid = vi.spyOn(crypto, "randomUUID")
      .mockReturnValueOnce("00000000-0000-4000-8000-000000000001")
      .mockReturnValueOnce("00000000-0000-4000-8000-000000000002");
    const view = render(<RestartAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    if (dismissal === "Cancel") fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    else fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(mutation.mutate).not.toHaveBeenCalled();
    view.rerender(<RestartAgentButton instance={{ ...instance, resourceVersion: "8" }} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    expect(mutation.mutate).toHaveBeenCalledWith({
      uid: instance.uid, resourceVersion: "8", requestId: "00000000-0000-4000-8000-000000000002",
    }, expect.anything());
    expect(uuid).toHaveBeenCalledTimes(2);
  });

  it.each(["Cancel", "Escape"])("retains a sent uncertain request across %s and reopening", (dismissal) => {
    const uuid = vi.spyOn(crypto, "randomUUID")
      .mockReturnValue("00000000-0000-4000-8000-000000000001");
    const view = render(<RestartAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    const sent = mutation.mutate.mock.calls[0][0];
    mutation.isError = true;
    view.rerender(<RestartAgentButton instance={{ ...instance, resourceVersion: "8" }} />);
    if (dismissal === "Cancel") fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    else fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Retry request" }));
    expect(mutation.mutate.mock.calls[1][0]).toEqual(sent);
    expect(uuid).toHaveBeenCalledTimes(1);
  });

  it("does not dismiss a request while submission is pending", () => {
    const view = render(<RestartAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    mutation.isPending = true;
    view.rerender(<RestartAgentButton instance={instance} />);
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(mutation.mutate).toHaveBeenCalledTimes(1);
  });

  it.each(["Cancel", "Escape"])("allows a fresh confirmation after a definite stale-version rejection and %s", (dismissal) => {
    const uuid = vi.spyOn(crypto, "randomUUID")
      .mockReturnValueOnce("00000000-0000-4000-8000-000000000001")
      .mockReturnValueOnce("00000000-0000-4000-8000-000000000002");
    const view = render(<RestartAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    view.rerender(<RestartAgentButton instance={{ ...instance, resourceVersion: "8" }} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    expect(mutation.mutate.mock.calls[0][0].resourceVersion).toBe("7");
    mutation.isError = true;
    mutation.error = new PortalError("Agent changed", 409, "instance_changed");
    view.rerender(<RestartAgentButton instance={{ ...instance, resourceVersion: "8" }} />);
    expect(screen.getByRole("alert")).toHaveTextContent("The request was not accepted");
    expect(screen.getByRole("button", { name: "Request rejected" })).toBeDisabled();
    if (dismissal === "Cancel") fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    else fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    expect(mutation.mutate.mock.calls[1][0]).toEqual({
      uid: instance.uid, resourceVersion: "8", requestId: "00000000-0000-4000-8000-000000000002",
    });
    expect(uuid).toHaveBeenCalledTimes(2);
  });

  it.each([
    new Error("Connection interrupted"),
    new SyntaxError("Invalid response"),
    new PortalError("Request result is unavailable", 500, "internal_error"),
    new PortalError("Agent is now suspended", 409, "restart_unavailable"),
    new PortalError("Unknown proxy result", 409, "unrecognized_error"),
    new PortalError("Server failed", 503, "instance_changed"),
  ])("retains the original sent request for uncertain outcome %s", (error) => {
    const view = render(<RestartAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    const sent = mutation.mutate.mock.calls[0][0];
    mutation.isError = true;
    mutation.error = error;
    view.rerender(<RestartAgentButton instance={{ ...instance, resourceVersion: "8" }} />);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Retry request" }));
    expect(mutation.mutate.mock.calls[1][0]).toEqual(sent);
  });

  it.each(["poll", "response"])("keeps an accepted request observed by %s when a later retry is rejected", (source) => {
    const view = render(<RestartAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Restart Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart", exact: true }));
    const sent = mutation.mutate.mock.calls[0][0];
    const accepted = { ...instance, resourceVersion: "8", generation: 3,
      restartRequestId: sent.requestId, restartAvailable: false };
    mutation.isError = true;
    mutation.error = new PortalError("Agent changed", 409, "instance_changed");
    if (source === "response") mutation.data = accepted;
    view.rerender(<RestartAgentButton instance={source === "poll" ? accepted : instance} />);
    expect(screen.getByRole("alert")).toHaveTextContent("closing this dialog does not cancel an accepted restart");
    expect(screen.queryByText("The request was not accepted", { exact: false })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Retry request" }));
    expect(mutation.mutate.mock.calls[1][0]).toEqual(sent);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Restart Agent" })).toBeDisabled();
  });
});
