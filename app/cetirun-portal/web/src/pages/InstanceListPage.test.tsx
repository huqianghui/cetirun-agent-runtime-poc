// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import { InstanceListPage } from "./InstanceListPage";

vi.mock("../hooks/useInstance", () => ({
  useInstancesQuery: () => ({ data: [
    { instanceName: "research", displayName: "Research", agent: { type: "openclaw" }, phase: "Pending", generation: 2,
      governance: { category: "policy", condition: "PolicyResolved", reason: "PolicyDenied", message: "policy requires Agent Identity", observedGeneration: 2 } },
    { instanceName: "support", displayName: "Support", agent: { type: "hermes" }, phase: "Provisioning" },
  ] }),
}));
vi.mock("../hooks/useFeatures", () => ({ useFeatures: () => ({ data: { maxInstancesPerUser: 3 } }) }));
vi.mock("../components/CreateForm", () => ({ CreateForm: () => null }));

describe("InstanceListPage", () => {
  it("shows the policy cause alongside Pending while preserving ordinary provisioning", () => {
    render(<MemoryRouter><InstanceListPage /></MemoryRouter>);
    const blocked = screen.getByText("research").closest("[role=group]")!;
    expect(within(blocked as HTMLElement).getByText("Pending")).toBeInTheDocument();
    expect(within(blocked as HTMLElement).getByText("Blocked by policy")).toBeInTheDocument();
    expect(screen.getByText("PolicyDenied: policy requires Agent Identity")).toBeInTheDocument();
    expect(screen.getByText("Provisioning")).toBeInTheDocument();
    expect(screen.getAllByLabelText("Governance status")).toHaveLength(1);
  });
});
