// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PortalError } from "../api/fetch";
import type { Instance, SetInstanceSuspendedRequest } from "../types/instance";
import { SuspendAgentButton } from "./SuspendAgentButton";

const mutation = vi.hoisted(() => ({
  mutate: vi.fn(), reset: vi.fn(), isPending: false, isError: false,
  error: new Error("Connection interrupted"), data: undefined as Instance | undefined,
  variables: undefined as SetInstanceSuspendedRequest | undefined,
}));
vi.mock("../hooks/useInstance", () => ({ useSetInstanceSuspendedByName: () => mutation }));

const instance: Instance = {
  instanceName: "alice", alias: "alice", displayName: "Alice", team: "example-team",
  uid: "agent-uid", resourceVersion: "7", generation: 2, observedGeneration: 2,
  agent: { type: "openclaw" }, phase: "Ready", createdAt: "2026-01-01T00:00:00Z",
  upgradeAvailable: false, suspended: false, suspendAvailable: true, resumeAvailable: false, restartAvailable: true,
};

describe("SuspendAgentButton", () => {
  beforeEach(() => {
    mutation.mutate.mockClear();
    mutation.reset.mockClear();
    mutation.isPending = false;
    mutation.isError = false;
    mutation.error = new Error("Connection interrupted");
    mutation.data = undefined;
    mutation.variables = undefined;
    mutation.mutate.mockImplementation((request: SetInstanceSuspendedRequest) => { mutation.variables = request; });
    mutation.reset.mockImplementation(() => {
      mutation.isError = false;
      mutation.data = undefined;
      mutation.variables = undefined;
    });
  });

  it("confirms only the originally displayed Agent UID and version", () => {
    const view = render(<SuspendAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Suspend Agent" }));
    expect(screen.getByText(/any existing HOME files or Gateway key are retained/)).toBeInTheDocument();
    view.rerender(<SuspendAgentButton instance={{ ...instance, uid: "replacement-uid", resourceVersion: "8" }} />);
    fireEvent.click(screen.getByRole("button", { name: "Suspend", exact: true }));
    expect(mutation.mutate).toHaveBeenCalledWith({
      uid: instance.uid, resourceVersion: "7", generation: 2, suspended: true,
    }, expect.anything());
  });

  it("keeps the original generation and version across a status-only refresh", () => {
    const view = render(<SuspendAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Suspend Agent" }));
    view.rerender(<SuspendAgentButton instance={{ ...instance, resourceVersion: "8", phase: "Pending" }} />);
    fireEvent.click(screen.getByRole("button", { name: "Suspend", exact: true }));
    expect(mutation.mutate).toHaveBeenCalledWith({
      uid: instance.uid, resourceVersion: "7", generation: 2, suspended: true,
    }, expect.anything());
  });

  it("explains why an ephemeral HOME cannot use retained suspension", () => {
    const reason = "Retained suspension requires persistent HOME storage";
    render(<SuspendAgentButton instance={{ ...instance, suspendAvailable: false, suspensionUnavailableReason: reason }} />);
    expect(screen.getByRole("button", { name: "Suspend Agent" })).toBeDisabled();
    expect(screen.getByRole("status")).toHaveTextContent(reason);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(mutation.mutate).not.toHaveBeenCalled();
  });

  it.each(["Cancel", "Escape"])("discards only an unsent draft on %s", (dismissal) => {
    const view = render(<SuspendAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Suspend Agent" }));
    if (dismissal === "Escape") fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });
    else fireEvent.click(screen.getByRole("button", { name: dismissal }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(mutation.mutate).not.toHaveBeenCalled();
    view.rerender(<SuspendAgentButton instance={{ ...instance, resourceVersion: "8" }} />);
    fireEvent.click(screen.getByRole("button", { name: "Suspend Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Suspend", exact: true }));
    expect(mutation.mutate).toHaveBeenCalledWith({
      uid: instance.uid, resourceVersion: "8", generation: 2, suspended: true,
    }, expect.anything());
  });

  it.each([true, false])("retains an uncertain suspended=%s request across closing and newer observations", (suspended) => {
    const initial = { ...instance, suspended: !suspended, suspendAvailable: suspended, resumeAvailable: !suspended };
    const action = suspended ? "Suspend" : "Resume";
    const view = render(<SuspendAgentButton instance={initial} />);
    fireEvent.click(screen.getByRole("button", { name: `${action} Agent` }));
    fireEvent.click(screen.getByRole("button", { name: action, exact: true }));
    const sent = mutation.mutate.mock.calls[0][0];
    mutation.isError = true;
    view.rerender(<SuspendAgentButton instance={{ ...initial, resourceVersion: "8" }} />);
    expect(screen.getByRole("alert")).toHaveTextContent("The outcome is uncertain");
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    view.rerender(<SuspendAgentButton instance={{ ...initial, resourceVersion: "9", suspendAvailable: false, resumeAvailable: false }} />);
    fireEvent.click(screen.getByRole("button", { name: `Review ${action.toLowerCase()} request` }));
    fireEvent.click(screen.getByRole("button", { name: "Retry request" }));
    expect(mutation.mutate.mock.calls[1][0]).toEqual(sent);
    expect(sent).toEqual({ uid: instance.uid, resourceVersion: "7", generation: 2, suspended });
  });

  it("observes accepted intent after a lost reply without claiming shutdown or submitting again", () => {
    const view = render(<SuspendAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Suspend Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Suspend", exact: true }));
    mutation.isError = true;
    view.rerender(<SuspendAgentButton instance={{ ...instance, resourceVersion: "8", generation: 3,
      suspended: true, suspendAvailable: false, resumeAvailable: true, restartAvailable: false }} />);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("does not confirm Pod termination");
    expect(screen.getByRole("button", { name: "Resume Agent" })).toBeEnabled();
    expect(mutation.mutate).toHaveBeenCalledTimes(1);
  });

  it("requires a new confirmation after a definite rejection", () => {
    const view = render(<SuspendAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Suspend Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Suspend", exact: true }));
    mutation.isError = true;
    mutation.error = new PortalError("Agent changed", 409, "instance_changed");
    view.rerender(<SuspendAgentButton instance={{ ...instance, resourceVersion: "8" }} />);
    expect(screen.getByRole("button", { name: "Request rejected" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    fireEvent.click(screen.getByRole("button", { name: "Suspend Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Suspend", exact: true }));
    expect(mutation.mutate.mock.calls[1][0].resourceVersion).toBe("8");
  });

  it("does not cancel an in-flight submission on close or Escape", () => {
    const view = render(<SuspendAgentButton instance={instance} />);
    fireEvent.click(screen.getByRole("button", { name: "Suspend Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Suspend", exact: true }));
    mutation.isPending = true;
    view.rerender(<SuspendAgentButton instance={instance} />);
    expect(screen.getByRole("button", { name: "Close" })).toBeDisabled();
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", code: "Escape" });
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(mutation.mutate).toHaveBeenCalledTimes(1);
  });

  it.each(["Pending", "Suspended", "Failed"] as const)("permits resuming the original pending operation from %s", (phase) => {
    render(<SuspendAgentButton instance={{ ...instance, phase, suspended: true, suspendAvailable: false,
      resumeAvailable: true, restartAvailable: false, observedGeneration: 0, restartRequestId: "request-00000001" }} />);
    fireEvent.click(screen.getByRole("button", { name: "Resume Agent" }));
    fireEvent.click(screen.getByRole("button", { name: "Resume", exact: true }));
    expect(mutation.mutate).toHaveBeenCalledWith({
      uid: instance.uid, resourceVersion: "7", generation: 2, suspended: false,
    }, expect.anything());
  });

  it.each(["Ready", "Deleting"] as const)("respects backend unavailability in %s, including legacy lifetimes", (phase) => {
    render(<SuspendAgentButton instance={{ ...instance, phase, suspendAvailable: false, resumeAvailable: false }} />);
    expect(screen.getByRole("button", { name: "Suspend Agent" })).toBeDisabled();
    expect(mutation.mutate).not.toHaveBeenCalled();
  });

  it("keeps a failed resume visible and allows suspension instead of claiming recovery", () => {
    const view = render(<SuspendAgentButton instance={{ ...instance, generation: 3, phase: "Failed", restartAvailable: false }} />);
    expect(screen.getByRole("alert")).toHaveTextContent("The workload has not recovered");
    expect(screen.getByRole("button", { name: "Suspend Agent" })).toBeEnabled();
    view.rerender(<SuspendAgentButton instance={instance} />);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByText(/Waiting for/)).not.toBeInTheDocument();
  });
});
