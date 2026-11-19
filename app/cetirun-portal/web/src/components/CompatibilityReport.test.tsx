// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { act, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter } from "react-router-dom";
import type { TeamDTO } from "../api/adminTeam";
import { CompatibilityBadge, CompatibilityReport, compatibilityPresentation } from "./CompatibilityReport";

function team(checkedAt: string): TeamDTO {
  return {
    name: "research", uid: "team-uid", generation: 1, resourceVersion: "1", agentPolicyRef: "standard", sandboxRefs: ["kata"], defaultSandboxRef: "kata",
    compatibility: { state: "Compatible", team: { name: "research", uid: "team-uid", generation: 1 }, policy: { name: "standard", uid: "policy-uid", generation: 1 },
      sandboxes: [{ sandbox: { name: "kata", uid: "sandbox-uid", generation: 1 }, state: "Compatible" }], checkedAt, validUntil: "2026-01-01T00:00:06Z" },
  };
}

function mixedCompatibilityTeam(): TeamDTO {
  const result = team("2026-01-01T00:00:00Z");
  result.sandboxRefs.push("restricted");
  result.compatibility!.state = "Incompatible";
  result.compatibility!.sandboxes!.push({
    sandbox: { name: "restricted", uid: "restricted-uid", generation: 1 }, state: "Incompatible",
    issues: [{ code: "ingress_unsupported", message: "The Sandbox does not support the Policy ingress configuration." }],
  });
  return result;
}

describe("CompatibilityBadge", () => {
  afterEach(() => vi.useRealTimers());

  it("uses the current time when a refreshed report arrives", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
    const view = render(<CompatibilityBadge team={team("2026-01-01T00:00:00Z")} />);
    expect(screen.getByText("Compatible")).toBeInTheDocument();
    act(() => vi.advanceTimersByTime(2_000));
    view.rerender(<CompatibilityBadge team={team("2026-01-01T00:00:01Z")} />);
    expect(screen.getByText("Compatible")).toBeInTheDocument();
  });

  it("fails closed when the Team is deleting", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
    expect(compatibilityPresentation({ ...team("2026-01-01T00:00:00Z"), deleting: true }).state).toBe("Unknown");
  });

  it("does not repeat Sandbox-scoped issues at the Team level", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
    const report = team("2026-01-01T00:00:00Z");
    report.compatibility!.issues = [{ code: "sandbox_incompatible", message: "Sandbox control conflict", sandbox: { name: "kata", uid: "sandbox-uid", generation: 1 } }, { code: "team_incompatible", message: "Team control conflict" }];
    report.compatibility!.sandboxes![0].issues = [{ code: "sandbox_incompatible", message: "Sandbox control conflict" }];
    render(<MemoryRouter><CompatibilityReport team={report} sandboxName="kata" /></MemoryRouter>);
    expect(screen.getAllByText("Sandbox control conflict")).toHaveLength(1);
    expect(screen.getByText("Team control conflict")).toBeInTheDocument();
  });

  it("accepts a persisted report without an optional Sandbox list", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
    const report = team("2026-01-01T00:00:00Z");
    report.sandboxRefs = [];
    report.defaultSandboxRef = "";
    delete report.compatibility!.sandboxes;
    render(<CompatibilityBadge team={report} />);
    expect(screen.getByText("Compatible")).toBeInTheDocument();
  });

  it("distinguishes the overall result from each Policy and Sandbox combination", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
    render(<MemoryRouter><CompatibilityReport team={mixedCompatibilityTeam()} /></MemoryRouter>);
    expect(screen.getByLabelText("Compatibility report for research")).toHaveTextContent("Compatibility: Incompatible");
    const table = screen.getByRole("table", { name: "Sandbox compatibility with Policy standard" });
    expect(within(table).getAllByRole("columnheader").map((cell) => cell.textContent)).toEqual(["Sandbox", "Compatibility", "Reason"]);
    expect(within(table).getByRole("row", { name: /kata/ })).toHaveTextContent("Compatible");
    expect(within(table).getByRole("row", { name: /restricted/ })).toHaveTextContent("Incompatible");
    expect(within(table).getByRole("row", { name: /restricted/ })).toHaveTextContent("The Sandbox does not support the Policy ingress configuration.");
  });

  it("keeps Team and selected Sandbox results distinct on the Sandbox page", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
    render(<MemoryRouter><CompatibilityReport team={mixedCompatibilityTeam()} sandboxName="kata" /></MemoryRouter>);
    expect(screen.getByLabelText("Compatibility report for research")).toHaveTextContent("Team compatibility: Incompatible");
    const table = screen.getByRole("table", { name: "Sandbox compatibility with Policy standard" });
    expect(within(table).getByRole("row", { name: /kata/ })).toHaveTextContent("Compatible");
    expect(within(table).queryByRole("row", { name: /restricted/ })).not.toBeInTheDocument();
  });
});
