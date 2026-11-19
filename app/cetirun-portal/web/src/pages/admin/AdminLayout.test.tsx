// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Features } from "../../types/features";
import { AdminLayout } from "./AdminLayout";

const features = vi.hoisted(() => ({ data: undefined as Features | undefined }));
vi.mock("../../hooks/useFeatures", () => ({ useFeatures: () => features }));

const base: Features = { tailscaleEnabled: false, agentIdentityEnabled: false, agentToolsEnabled: false, maxInstancesPerUser: 1 };

function renderLayout() {
  return render(<MemoryRouter initialEntries={["/admin/skills"]}><Routes>
    <Route path="/admin" element={<AdminLayout />}>
      <Route path="skills" element={<div>Skills page</div>} />
    </Route>
  </Routes></MemoryRouter>);
}

describe("AdminLayout", () => {
  beforeEach(() => { features.data = undefined; });

  it.each([undefined, false])("hides the Skills entry when skillMarketplaceEnabled is %s", (enabled) => {
    features.data = enabled === undefined ? undefined : { ...base, skillMarketplaceEnabled: enabled };
    renderLayout();
    expect(screen.getByRole("link", { name: "Teams" })).toHaveAttribute("href", "/admin/teams");
    expect(screen.queryByRole("link", { name: "Skills" })).not.toBeInTheDocument();
  });

  it("links to the Skills page when the Skill Marketplace is enabled", () => {
    features.data = { ...base, skillMarketplaceEnabled: true };
    renderLayout();
    expect(screen.getByRole("link", { name: "Skills" })).toHaveAttribute("href", "/admin/skills");
    expect(screen.getByText("Skills page")).toBeInTheDocument();
  });
});
