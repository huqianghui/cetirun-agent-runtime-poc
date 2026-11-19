// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PolicyListPage } from "./PolicyListPage";
import * as policyAPI from "../../api/adminPolicy";
import * as teamAPI from "../../api/adminTeam";

vi.mock("../../api/adminPolicy");
vi.mock("../../api/adminTeam");

describe("PolicyListPage", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    vi.mocked(teamAPI.listTeams).mockResolvedValue([]);
    // jsdom has no layout. Tabster otherwise treats every dialog control as hidden,
    // which prevents focus activation and can mark a reopened dialog aria-hidden.
    vi.spyOn(document.body, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 1024, 768));
    vi.spyOn(HTMLElement.prototype, "offsetParent", "get").mockImplementation(function (this: HTMLElement) {
      return this.parentElement;
    });
  });

  function currentCompatibility() {
    const checkedAt = new Date(Date.now() - 1_000);
    return {
      state: "Compatible" as const, team: { name: "research", uid: "team-uid", generation: 4 }, policy: { name: "standard", uid: "policy-uid", generation: 3 },
      sandboxes: [{ sandbox: { name: "kata", uid: "sandbox-uid", generation: 1 }, state: "Compatible" as const }], checkedAt: checkedAt.toISOString(), validUntil: new Date(checkedAt.getTime() + 30_000).toISOString(),
    };
  }
  afterEach(() => vi.restoreAllMocks());

  it("disables actions while a policy is deleting", async () => {
    vi.mocked(policyAPI.listPolicies).mockResolvedValue([{
      name: "standard", uid: "policy-uid", resourceVersion: "1", harnesses: { allowed: ["openclaw"] },
      generation: 3, observedGeneration: 2, deleting: true,
    }]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter><PolicyListPage /></MemoryRouter></QueryClientProvider>);
    expect(await screen.findByText("standard")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit standard" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Delete standard" })).toBeDisabled();
  });

  it("shows referencing Teams without claiming policy enforcement", async () => {
    const user = userEvent.setup();
    vi.mocked(policyAPI.listPolicies).mockResolvedValue([{
      name: "standard", uid: "policy-uid", resourceVersion: "1", harnesses: { allowed: ["openclaw"] }, generation: 3,
      observedGeneration: 3, conditions: [{ type: "Accepted", status: "True", observedGeneration: 3 }],
    }]);
    vi.mocked(teamAPI.listTeams).mockResolvedValue([{
      name: "research", uid: "team-uid", generation: 4, resourceVersion: "7", agentPolicyRef: "standard", sandboxRefs: ["kata"], defaultSandboxRef: "kata",
      compatibility: currentCompatibility(),
    }]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter><PolicyListPage /></MemoryRouter></QueryClientProvider>);
    expect(await screen.findByText("Accepted")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Show Teams using standard" }));
    const report = await screen.findByLabelText("Compatibility report for research");
    expect(report).toHaveTextContent("Compatibility: Compatible");
    expect(report).toHaveTextContent("Sandbox: kata");
    expect(screen.getAllByRole("link", { name: "standard" })).toHaveLength(1);
    expect(screen.queryByText("Applied")).not.toBeInTheDocument();
  });

  it("shows the current policy rejection reason with its referencing Teams", async () => {
    const user = userEvent.setup();
    vi.mocked(policyAPI.listPolicies).mockResolvedValue([{
      name: "standard", uid: "policy-uid", resourceVersion: "1", harnesses: { allowed: ["openclaw"] }, generation: 3,
      observedGeneration: 3, conditions: [{ type: "Accepted", status: "False", observedGeneration: 3, reason: "InvalidConfiguration", message: "A required control is missing." }],
    }]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter><PolicyListPage /></MemoryRouter></QueryClientProvider>);
    expect(await screen.findByText("Rejected")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Show Teams using standard" }));
    expect(await screen.findByText("Configuration: InvalidConfiguration: A required control is missing.")).toBeInTheDocument();
  });

  it("does not show a stale Team report as compatible after the Policy is recreated", async () => {
    const user = userEvent.setup();
    vi.mocked(policyAPI.listPolicies).mockResolvedValue([{
      name: "standard", uid: "replacement-policy-uid", resourceVersion: "2", harnesses: { allowed: ["openclaw"] }, generation: 4,
      observedGeneration: 4, conditions: [{ type: "Accepted", status: "True", observedGeneration: 4 }],
    }]);
    vi.mocked(teamAPI.listTeams).mockResolvedValue([{
      name: "research", uid: "team-uid", generation: 4, resourceVersion: "7", agentPolicyRef: "standard", sandboxRefs: ["kata"], defaultSandboxRef: "kata",
      compatibility: currentCompatibility(),
    }]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter><PolicyListPage /></MemoryRouter></QueryClientProvider>);
    await user.click(await screen.findByRole("button", { name: "Show Teams using standard" }));
    expect(await screen.findByText("The saved compatibility report no longer matches this configuration.")).toBeInTheDocument();
  });

  it.each([
    { change: "updated", uid: "policy-uid" },
    { change: "recreated", uid: "replacement-uid" },
  ])("keeps the confirmed version when a policy is $change during a refresh", async ({ uid }) => {
    const user = userEvent.setup();
    const selected: policyAPI.AgentPolicyDTO = {
      name: "standard", uid: "policy-uid", resourceVersion: "1", harnesses: { allowed: ["openclaw"] },
    };
    const refreshed: policyAPI.AgentPolicyDTO = {
      ...selected, uid, resourceVersion: "2", harnesses: { allowed: ["hermes"] },
    };
    vi.mocked(policyAPI.listPolicies).mockResolvedValueOnce([selected]).mockResolvedValue([refreshed]);
    const rejection = new Error("Policy changed; reload and try again.");
    vi.mocked(policyAPI.deletePolicy)
      .mockRejectedValueOnce(rejection)
      .mockRejectedValueOnce(rejection)
      .mockResolvedValue(undefined);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter><PolicyListPage /></MemoryRouter></QueryClientProvider>);

    await user.click(await screen.findByRole("button", { name: "Delete standard" }));
    expect(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" })).toHaveFocus();
    await act(async () => { await client.refetchQueries({ queryKey: ["admin", "policies"] }); });
    expect(await screen.findByText("hermes")).toBeInTheDocument();
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete", exact: true }));
    expect(await screen.findByText("Policy changed; reload and try again.")).toBeInTheDocument();
    expect(policyAPI.deletePolicy).toHaveBeenLastCalledWith(selected);

    // Retrying the same confirmation must not silently adopt the newer version.
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete", exact: true }));
    await waitFor(() => {
      expect(policyAPI.deletePolicy).toHaveBeenCalledTimes(2);
      expect(client.isMutating()).toBe(0);
      expect(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete", exact: true })).toBeEnabled();
    });
    expect(policyAPI.deletePolicy).toHaveBeenLastCalledWith(selected);

    // A new selection and confirmation can delete the version now shown in the list.
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
    // Wait for the exiting modal to unmount and release its focus trap before reopening.
    await waitFor(() => expect(screen.queryByRole("dialog", { hidden: true })).not.toBeInTheDocument());
    await user.click(await screen.findByRole("button", { name: "Delete standard" }));
    expect(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" })).toHaveFocus();
    expect(screen.queryByText("Policy changed; reload and try again.")).not.toBeInTheDocument();
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete", exact: true }));
    await waitFor(() => expect(policyAPI.deletePolicy).toHaveBeenCalledTimes(3));
    expect(policyAPI.deletePolicy).toHaveBeenLastCalledWith(refreshed);
    await waitFor(() => expect(screen.queryByRole("dialog", { hidden: true })).not.toBeInTheDocument());
    await waitFor(() => expect(client.isMutating()).toBe(0));
    client.clear();
  });
});
