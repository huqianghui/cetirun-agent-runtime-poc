// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { GovernanceStatus } from "../types/instance";
import { GovernanceNotice } from "./GovernanceNotice";

const denied: GovernanceStatus = {
  category: "policy", reason: "PolicyDenied", message: "policy requires Agent Identity",
  condition: "PolicyResolved", observedGeneration: 3,
};

describe("GovernanceNotice", () => {
  it.each([
    ["policy", "PolicyDenied", "Blocked by policy", /Agent Policy/],
    ["identity", "OwnerBindingMissing", "Identity owner binding required", /trusted owner binding/],
    ["identity", "CredentialUnavailable", "Identity not ready", /Agent Identity setup/],
    ["policy", "UnauthorizedBinding", "Binding or identity approval required", /identity or role approvals/],
    ["policy", "UnapprovedGrant", "Binding or identity approval required", /identity or role approvals/],
    ["platform", "UnsupportedControl", "Platform support required", /platform support/],
    ["dependency", "DependencyUnavailable", "Governance dependency unavailable", /platform dependencies/],
    ["pending", "EnforcementPending", "Waiting for governance", /governance prerequisites/],
  ] as const)("explains %s/%s and gives a next step", (category, reason, title, action) => {
    render(<GovernanceNotice instance={{ generation: 3, phase: "Pending", governance: { ...denied, category, reason } }} />);
    expect(screen.getByText(title)).toBeInTheDocument();
    expect(screen.getByText(`${reason}: ${denied.message}`)).toBeInTheDocument();
    expect(screen.getByText(action)).toBeInTheDocument();
  });

  it("keeps compact causes visible without tooltips and escapes condition text", () => {
    const message = "<img src=x onerror=alert(1)>";
    const { container } = render(<GovernanceNotice instance={{ governance: { ...denied, message } }} compact />);
    expect(screen.getByLabelText("Governance status")).toHaveTextContent("Blocked by policy");
    expect(screen.getByText(`PolicyDenied: ${message}`)).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
  });

  it("removes a denial after a configuration change and after recovery", () => {
    const { rerender } = render(<GovernanceNotice instance={{ generation: 3, governance: denied }} />);
    expect(screen.getByText("Blocked by policy")).toBeInTheDocument();
    rerender(<GovernanceNotice instance={{ generation: 4, governance: denied }} />);
    expect(screen.queryByText("Blocked by policy")).not.toBeInTheDocument();
    rerender(<GovernanceNotice instance={{ generation: 4, phase: "Ready" }} />);
    expect(screen.queryByText("Blocked by policy")).not.toBeInTheDocument();
  });

  it.each([
    { phase: "Deleting" }, { phase: "Suspended" }, { phase: "Provisioning", suspended: true },
  ])("gives lifecycle state precedence: %j", (lifecycle) => {
    const { container } = render(<GovernanceNotice instance={{ ...lifecycle, governance: denied }} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("does not invent a governance cause for ordinary provisioning", () => {
    const { container } = render(<GovernanceNotice instance={{ phase: "Provisioning" }} />);
    expect(container).toBeEmptyDOMElement();
  });
});
