// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PolicyEditPage } from "./PolicyEditPage";
import * as policyAPI from "../../api/adminPolicy";
import * as teamAPI from "../../api/adminTeam";
import * as gatewayAPI from "../../api/aiGateway";

vi.mock("../../api/adminPolicy", async (importOriginal) => ({
  ...await importOriginal<typeof import("../../api/adminPolicy")>(),
  createPolicy: vi.fn(), getPolicy: vi.fn(), updatePolicy: vi.fn(),
}));
vi.mock("../../api/aiGateway");
vi.mock("../../api/adminTeam");

const catalog: gatewayAPI.AIGatewayCatalog = {
  known: true, githubCopilot: true, agentTypes: ["openclaw"], decisionModels: ["decision-model"],
  sharedModels: [
    { name: "gpt-mini" }, { name: "gpt-main" }, { name: "gpt-mini" },
    { name: "auto", routerModels: ["another-model"] },
    { name: "auto-custom", routerUnverifiable: true },
  ],
};
const policy: policyAPI.AgentPolicyDTO = {
  name: "standard", uid: "policy-uid", resourceVersion: "42", harnesses: { allowed: ["openclaw"] },
  modelAccess: { sharedModels: { allowed: ["gpt-main"] }, gatewayBudget: { defaultMonthlyUSD: "20", maxMonthlyUSD: "50" } },
};

function renderEditor(existing?: policyAPI.AgentPolicyDTO) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (existing) {
    client.setQueryData(["admin", "policy", existing.name], existing);
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(existing);
  }
  render(<QueryClientProvider client={client}>
    <MemoryRouter initialEntries={[existing ? `/admin/policies/${existing.name}/edit` : "/admin/policies/create"]}>
      <Routes>
        <Route path="/admin/policies/create" element={<PolicyEditPage />} />
        <Route path="/admin/policies/:name/edit" element={<PolicyEditPage />} />
        <Route path="/admin/policies" element={<div>Policy list</div>} />
      </Routes>
    </MemoryRouter>
  </QueryClientProvider>);
  return client;
}

const picker = () => screen.getByRole("combobox", { name: "Allowed shared models" });
const selected = () => within(screen.getByRole("list", { name: "Selected shared models" }));

describe("Policy model selection", () => {
  beforeEach(() => {
    vi.mocked(teamAPI.listTeams).mockReset().mockResolvedValue([]);
    vi.mocked(gatewayAPI.getAIGatewayCatalog).mockReset().mockResolvedValue(catalog);
    vi.mocked(policyAPI.getPolicy).mockReset();
    vi.mocked(policyAPI.createPolicy).mockReset().mockResolvedValue(policy);
    vi.mocked(policyAPI.updatePolicy).mockReset().mockResolvedValue(policy);
  });

  it("creates a policy with searchable, sorted shared models, including all routers", async () => {
    const user = userEvent.setup();
    renderEditor();
    await waitFor(() => expect(picker()).toBeEnabled());
    expect(gatewayAPI.getAIGatewayCatalog).toHaveBeenCalledOnce();
    await user.click(picker());
    expect(within(screen.getByRole("menu")).getAllByRole("menuitemcheckbox").map((option) => option.textContent))
      .toEqual(["auto", "auto-custom", "gpt-main", "gpt-mini"]);
    await user.click(screen.getByRole("menuitemcheckbox", { name: "auto", exact: true }));
    await user.click(screen.getByRole("menuitemcheckbox", { name: "auto-custom", exact: true }));
    await user.type(picker(), "GPT-MINI");
    expect(screen.queryByRole("menuitemcheckbox", { name: "gpt-main" })).not.toBeInTheDocument();
    expect(screen.getByRole("menuitemcheckbox", { name: "gpt-mini" })).toBeInTheDocument();
    await user.keyboard("{ArrowDown}{Enter}");
    expect(selected().getByText("gpt-mini")).toBeInTheDocument();
    expect(policyAPI.createPolicy).not.toHaveBeenCalled();
    await user.keyboard("{Escape}");
    await user.type(screen.getByRole("textbox", { name: "Name" }), "standard");
    await user.click(screen.getByRole("switch", { name: "Set monthly Gateway budget" }));
    await user.type(screen.getByRole("textbox", { name: "Default monthly USD" }), "20");
    await user.type(screen.getByRole("textbox", { name: "Maximum monthly USD" }), "50");
    await user.click(screen.getByRole("button", { name: "Create", exact: true }));
    await waitFor(() => expect(policyAPI.createPolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.createPolicy).mock.calls[0][0].modelAccess?.sharedModels?.allowed)
      .toEqual(["auto", "auto-custom", "gpt-mini"]);
  });

  it("edits selected models without adding arbitrary search text", async () => {
    const user = userEvent.setup();
    renderEditor(policy);
    await waitFor(() => expect(picker()).toBeEnabled());
    await user.click(picker());
    expect(screen.getByRole("menuitemcheckbox", { name: "gpt-main" })).toHaveAttribute("aria-checked", "true");
    await user.click(screen.getByRole("menuitemcheckbox", { name: "gpt-mini" }));
    await user.click(screen.getByRole("menuitemcheckbox", { name: "gpt-main" }));
    await user.type(picker(), "unknown-model");
    expect(screen.getByRole("menuitemcheckbox", { name: "No models match your search" })).toHaveAttribute("aria-disabled", "true");
    await user.keyboard("{Enter}{Escape}");
    expect(policyAPI.updatePolicy).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Save", exact: true }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].modelAccess?.sharedModels?.allowed).toEqual(["gpt-mini"]);
  });

  it("preserves missing models when adding a model and saving", async () => {
    const user = userEvent.setup();
    renderEditor({ ...policy, modelAccess: { ...policy.modelAccess, sharedModels: { allowed: ["old-model", "gpt-main"] } } });
    expect(await screen.findByText("Not listed in the current catalog")).toBeInTheDocument();
    await user.click(picker());
    await user.click(screen.getByRole("menuitemcheckbox", { name: "gpt-mini" }));
    await user.keyboard("{Escape}");
    expect(selected().getByText("old-model")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save", exact: true }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].modelAccess?.sharedModels?.allowed)
      .toEqual(["old-model", "gpt-main", "gpt-mini"]);
  });

  it("removes a missing model only on request and clears the budget when the last source is removed", async () => {
    const user = userEvent.setup();
    renderEditor({ ...policy, modelAccess: { ...policy.modelAccess, sharedModels: { allowed: ["old-model"] } } });
    expect(await screen.findByText("Not listed in the current catalog")).toBeInTheDocument();
    await user.click(selected().getByRole("button", { name: "Remove old-model" }));
    expect(screen.getByRole("switch", { name: "Set monthly Gateway budget" })).not.toBeChecked();
    await user.click(screen.getByRole("button", { name: "Save", exact: true }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    const access = vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].modelAccess;
    expect(access?.sharedModels?.allowed).toEqual([]);
    expect(access?.gatewayBudget).toBeUndefined();
  });

  it("keeps the budget when Copilot remains after removing the last shared model", async () => {
    const user = userEvent.setup();
    renderEditor({ ...policy, modelAccess: { ...policy.modelAccess, copilot: { allowed: true } } });
    await user.click(selected().getByRole("button", { name: "Remove gpt-main" }));
    expect(screen.getByRole("switch", { name: "Set monthly Gateway budget" })).toBeChecked();
  });

  it.each([
    ["unknown", { ...catalog, known: false }, "The Gateway model catalog is not available yet."],
    ["empty", { ...catalog, sharedModels: [] }, "No shared models are listed in the Gateway catalog."],
    ["failed", null, "Failed to load the model catalog. Your selections have been kept. Try refreshing."],
  ] as const)("keeps existing models and allows other edits with an %s catalog", async (_, response, message) => {
    if (response) vi.mocked(gatewayAPI.getAIGatewayCatalog).mockResolvedValue(response);
    else vi.mocked(gatewayAPI.getAIGatewayCatalog).mockRejectedValue(new Error("Unavailable"));
    renderEditor(policy);
    expect(await screen.findByText(message)).toBeInTheDocument();
    expect(picker()).toBeDisabled();
    expect(selected().getByText("gpt-main")).toBeInTheDocument();
    fireEvent.change(screen.getByRole("textbox", { name: "Allowed skills" }), { target: { value: "new-skill" } });
    fireEvent.click(screen.getByRole("button", { name: "Save", exact: true }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1]).toMatchObject({
      skills: { allowed: ["new-skill"] }, modelAccess: { sharedModels: { allowed: ["gpt-main"] } },
    });
  });

  it("keeps selections while loading and recovers a failed catalog through refresh", async () => {
    let reject!: (reason: Error) => void;
    vi.mocked(gatewayAPI.getAIGatewayCatalog).mockReturnValueOnce(new Promise((_, no) => { reject = no; }));
    const user = userEvent.setup();
    renderEditor(policy);
    expect(screen.getByText("Loading models...")).toBeInTheDocument();
    expect(picker()).toBeDisabled();
    expect(selected().getByText("gpt-main")).toBeInTheDocument();
    await act(async () => reject(new Error("Unavailable")));
    expect(await screen.findByText(/Failed to load the model catalog/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Refresh models" }));
    await waitFor(() => expect(picker()).toBeEnabled());
    expect(screen.queryByText(/Failed to load the model catalog/)).not.toBeInTheDocument();
    expect(selected().getByText("gpt-main")).toBeInTheDocument();
  });

  it("preserves unsaved selections when models disappear on refresh or a later refresh fails", async () => {
    const user = userEvent.setup();
    renderEditor(policy);
    await waitFor(() => expect(picker()).toBeEnabled());
    await user.click(picker());
    await user.click(screen.getByRole("menuitemcheckbox", { name: "gpt-mini" }));
    await user.keyboard("{Escape}");
    vi.mocked(gatewayAPI.getAIGatewayCatalog).mockResolvedValueOnce({ ...catalog, sharedModels: [{ name: "gpt-main" }] });
    await user.click(screen.getByRole("button", { name: "Refresh models" }));
    expect(await selected().findByText("Not listed in the current catalog")).toBeInTheDocument();
    expect(selected().getByText("gpt-mini")).toBeInTheDocument();
    vi.mocked(gatewayAPI.getAIGatewayCatalog).mockRejectedValueOnce(new Error("Unavailable"));
    await user.click(screen.getByRole("button", { name: "Refresh models" }));
    expect(await screen.findByText(/Failed to load the model catalog/)).toBeInTheDocument();
    expect(picker()).toBeEnabled();
    await user.click(screen.getByRole("button", { name: "Save", exact: true }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].modelAccess?.sharedModels?.allowed).toEqual(["gpt-main", "gpt-mini"]);
  });
  it("selects and persists only linked Team router aliases without granting their tiers", async () => {
    const team = (name: string, agentPolicyRef: string, routerName = "team-auto", ready = true): teamAPI.TeamDTO => ({
      name, uid: `${name}-uid`, resourceVersion: "1", generation: 1, agentPolicyRef,
      sandboxRefs: [], defaultSandboxRef: "", modelRouters: [{ name: routerName, simple: "gpt-mini", medium: "gpt-main", complex: "gpt-main" }],
      modelRouterStatus: [{ name: routerName, ready }],
    });
    vi.mocked(teamAPI.listTeams).mockResolvedValue([
      team("linked", policy.name), team("second", policy.name), team("other", "other-policy", "foreign-auto"),
      team("pending", policy.name, "pending-auto", false), { ...team("deleted", policy.name, "deleted-auto"), deleting: true },
    ]);
    const user = userEvent.setup();
    renderEditor(policy);
    await waitFor(() => expect(teamAPI.listTeams).toHaveBeenCalledOnce());
    await user.click(picker());
    expect(screen.queryByRole("menuitemcheckbox", { name: /foreign-auto|deleted-auto/ })).not.toBeInTheDocument();
    expect(screen.getAllByRole("menuitemcheckbox", { name: /team-auto/ })).toHaveLength(1);
    expect(screen.getByText("Team linked: registered; Team second: registered")).toBeInTheDocument();
    expect(screen.getByText("Team pending: registration pending")).toBeInTheDocument();
    await user.click(screen.getByRole("menuitemcheckbox", { name: /team-auto/ }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Save", exact: true }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].modelAccess?.sharedModels?.allowed)
      .toEqual(["gpt-main", "team-auto"]);
  });

  it("keeps a selected Team alias through discovery failure and recovers on refresh", async () => {
    vi.mocked(teamAPI.listTeams).mockRejectedValueOnce(new Error("Unavailable"));
    const user = userEvent.setup();
    renderEditor({ ...policy, modelAccess: { ...policy.modelAccess, sharedModels: { allowed: ["team-auto"] } } });
    expect(await screen.findByText(/Failed to load Team routers/)).toBeInTheDocument();
    expect(selected().getByText("team-auto")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Refresh models" }));
    await waitFor(() => expect(screen.queryByText(/Failed to load Team routers/)).not.toBeInTheDocument());
    expect(selected().getByText("team-auto")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save", exact: true }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].modelAccess?.sharedModels?.allowed).toEqual(["team-auto"]);
  });

});
