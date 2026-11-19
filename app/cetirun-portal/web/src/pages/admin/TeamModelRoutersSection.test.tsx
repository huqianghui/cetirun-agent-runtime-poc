// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, expect, it, vi } from "vitest";
import { TeamEditPage } from "./TeamEditPage";
import { modelRouterProblem, toModelRouterRequests } from "./TeamModelRoutersSection";
import * as teams from "../../api/adminTeam";
import * as policies from "../../api/adminPolicy";
import * as sandboxes from "../../api/adminSandbox";
import * as gateway from "../../api/aiGateway";

vi.mock("../../api/adminTeam");
vi.mock("../../api/adminPolicy");
vi.mock("../../api/adminSandbox");
vi.mock("../../api/aiGateway");

const team: teams.TeamDTO = {
  name: "research", uid: "team-uid", generation: 1, resourceVersion: "7", agentPolicyRef: "standard", sandboxRefs: ["small"], defaultSandboxRef: "small",
  modelRouters: [{ name: "auto", classifier: "heuristic", simple: "small-model", medium: "medium-model", complex: "large-model" }],
  modelRouterStatus: [{ name: "auto", ready: false, message: "router auto: tier medium names medium-model, which is not a shared model of the AI Gateway" }],
};

beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(teams.getTeam).mockResolvedValue(team);
  vi.mocked(teams.updateTeam).mockResolvedValue(team);
  vi.mocked(policies.listPolicies).mockResolvedValue([{ name: "standard", uid: "policy-uid", resourceVersion: "1", harnesses: { allowed: ["hermes"] } }]);
  vi.mocked(sandboxes.listSandboxes).mockResolvedValue([{ name: "small", deleting: false } as sandboxes.SandboxDTO]);
  vi.mocked(gateway.getAIGatewayCatalog).mockResolvedValue({ known: true, sharedModels: [{ name: "small-model" }, { name: "large-model" }],
    decisionModels: ["decision-1"], githubCopilot: true, agentTypes: ["hermes"] });
});

function renderEditor() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/admin/teams/research/edit"]}><Routes>
    <Route path="/admin/teams/:name/edit" element={<TeamEditPage />} />
    <Route path="/admin/teams" element={<div>Team list</div>} />
  </Routes></MemoryRouter></QueryClientProvider>);
}

it("shows a router's status and saves added routers with the Team", async () => {
  const user = userEvent.setup(); renderEditor();
  expect(await screen.findByText(/tier medium names medium-model/)).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Add smart-routing model" }));
  const added = screen.getByRole("group", { name: "Smart-routing model 2" });
  // The Gateway that make deploy installs already serves the shared router auto.
  expect(within(added).getByRole("textbox", { name: "Model name" })).toHaveAttribute("placeholder", "auto-team");
  await user.type(within(added).getByRole("textbox", { name: "Model name" }), "auto-copilot");
  await user.selectOptions(within(added).getByRole("combobox", { name: "Classifier" }), "llm");
  await user.selectOptions(within(added).getByRole("combobox", { name: "Classifier model" }), "small-model");
  await user.type(within(added).getByRole("combobox", { name: "Simple tier" }), "copilot/fast-model");
  await user.type(within(added).getByRole("combobox", { name: "Medium tier" }), "small-model");
  await user.type(within(added).getByRole("combobox", { name: "Complex tier" }), "large-model");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(teams.updateTeam).toHaveBeenCalledOnce());
  expect(vi.mocked(teams.updateTeam).mock.calls[0][1].modelRouters).toEqual([
    { name: "auto", classifier: "heuristic", simple: "small-model", medium: "medium-model", complex: "large-model" },
    { name: "auto-copilot", classifier: "llm", classifierModel: "small-model", simple: "copilot/fast-model", medium: "small-model", complex: "large-model" },
  ]);
});

it("saves a router that classifies with a decision model", async () => {
  const user = userEvent.setup(); renderEditor();
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Add smart-routing model" }));
  const added = screen.getByRole("group", { name: "Smart-routing model 2" });
  await user.type(within(added).getByRole("textbox", { name: "Model name" }), "auto-decision");
  await user.selectOptions(within(added).getByRole("combobox", { name: "Classifier" }), "jev");
  await waitFor(() => expect(within(added).getByRole("option", { name: "decision-1" })).toBeInTheDocument());
  await user.selectOptions(within(added).getByRole("combobox", { name: "Classifier model" }), "decision-1");
  await user.type(within(added).getByRole("combobox", { name: "Simple tier" }), "small-model");
  await user.type(within(added).getByRole("combobox", { name: "Medium tier" }), "large-model");
  await user.type(within(added).getByRole("combobox", { name: "Complex tier" }), "large-model");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(teams.updateTeam).toHaveBeenCalledOnce());
  expect(vi.mocked(teams.updateTeam).mock.calls[0][1].modelRouters).toEqual([
    { name: "auto", classifier: "heuristic", simple: "small-model", medium: "medium-model", complex: "large-model" },
    { name: "auto-decision", classifier: "jev", classifierModel: "decision-1", simple: "small-model", medium: "large-model", complex: "large-model" },
  ]);
});

it("offers the decision classifier the decision models only", async () => {
  vi.mocked(gateway.getAIGatewayCatalog).mockResolvedValue({ known: true, githubCopilot: true, agentTypes: ["hermes"], decisionModels: ["decision-1"],
    sharedModels: [{ name: "small-model" }, { name: "gateway-auto", routerModels: ["small-model"] }] });
  const user = userEvent.setup(); renderEditor();
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Add smart-routing model" }));
  const added = screen.getByRole("group", { name: "Smart-routing model 2" });
  const offered = () => Array.from((within(added).getByRole("combobox", { name: "Classifier model" }) as HTMLSelectElement).options)
    .map((option) => option.textContent);
  await user.selectOptions(within(added).getByRole("combobox", { name: "Classifier" }), "llm");
  await waitFor(() => expect(offered()).toEqual(["Choose a shared model", "small-model"]));
  await user.selectOptions(within(added).getByRole("combobox", { name: "Classifier model" }), "small-model");
  // A shared model is no decision model: the switch drops it.
  await user.selectOptions(within(added).getByRole("combobox", { name: "Classifier" }), "jev");
  expect((within(added).getByRole("combobox", { name: "Classifier model" }) as HTMLSelectElement).value).toBe("");
  expect(offered()).toEqual(["Choose a decision model", "decision-1"]);
  expect(within(added).queryByText("The AI Gateway offers no decision model.")).not.toBeInTheDocument();
});

it("shows a decision model the Gateway does not offer", async () => {
  vi.mocked(gateway.getAIGatewayCatalog).mockResolvedValue({ known: true, sharedModels: [{ name: "small-model" }], githubCopilot: true, agentTypes: ["hermes"] });
  vi.mocked(teams.getTeam).mockResolvedValue({ ...team, modelRouterStatus: [],
    modelRouters: [{ name: "auto", classifier: "jev", classifierModel: "decision-1", simple: "small-model", medium: "medium-model", complex: "large-model" }] });
  renderEditor();
  const router = await screen.findByRole("group", { name: "Smart-routing model 1" });
  expect(await within(router).findByText("The AI Gateway offers no decision model.")).toBeInTheDocument();
  expect(within(router).getByRole("option", { name: "decision-1 (not a decision model)" })).toBeInTheDocument();
  expect((within(router).getByRole("combobox", { name: "Classifier model" }) as HTMLSelectElement).value).toBe("decision-1");
});

it("does not save an incomplete router", async () => {
  const user = userEvent.setup(); renderEditor();
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Add smart-routing model" }));
  await user.type(within(screen.getByRole("group", { name: "Smart-routing model 2" })).getByRole("textbox", { name: "Model name" }), "auto-2");
  // The browser stops a form with an empty required tier; submit past that to check the page's own check.
  fireEvent.submit(screen.getByRole("button", { name: "Save" }).closest("form")!);
  expect(await screen.findByText(/auto-2: the simple tier needs a model name/)).toBeInTheDocument();
  expect(teams.updateTeam).not.toHaveBeenCalled();
});

it("removes a router", async () => {
  const user = userEvent.setup(); renderEditor();
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(within(screen.getByRole("group", { name: "Smart-routing model 1" })).getByRole("button", { name: "Remove" }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(teams.updateTeam).toHaveBeenCalledOnce());
  expect(vi.mocked(teams.updateTeam).mock.calls[0][1].modelRouters).toBeUndefined();
});

it("checks routers the way the Team API does", () => {
  const router = { name: "auto", simple: "a", medium: "b", complex: "c" };
  expect(modelRouterProblem([router])).toBeNull();
  expect(modelRouterProblem([{ ...router, name: "CetiRun-x" }])).toMatch(/reserved/);
  expect(modelRouterProblem([{ ...router, name: "copilot/auto" }])).toMatch(/without spaces or slashes/);
  expect(modelRouterProblem([router, router])).toMatch(/defined twice/);
  expect(modelRouterProblem([{ ...router, classifier: "llm" }])).toMatch(/needs a shared model/);
  expect(modelRouterProblem([{ ...router, classifier: "llm", classifierModel: "copilot/x" }])).toMatch(/needs a shared model/);
  expect(modelRouterProblem([{ ...router, classifier: "jev" }])).toBe("Smart-routing model auto: the decision classifier needs a decision model.");
  expect(modelRouterProblem([{ ...router, classifier: "jev", classifierModel: "copilot/decision-1" }])).toMatch(/needs a decision model/);
  expect(modelRouterProblem([{ ...router, classifier: "jev", classifierModel: " decision-1 " }])).toBeNull();
  expect(modelRouterProblem(Array.from({ length: 11 }, (_, i) => ({ ...router, name: `auto-${i}` })))).toMatch(/at most 10/);
  expect(toModelRouterRequests([{ ...router, name: " auto ", classifierModel: "x", reasoning: " " }])).toEqual([
    { name: "auto", classifier: "heuristic", simple: "a", medium: "b", complex: "c" },
  ]);
  expect(toModelRouterRequests([{ ...router, classifier: "jev", classifierModel: " decision-1 " }])).toEqual([
    { name: "auto", classifier: "jev", classifierModel: "decision-1", simple: "a", medium: "b", complex: "c" },
  ]);
});

it("offers no router of the Gateway as a tier or classifier model", async () => {
  vi.mocked(gateway.getAIGatewayCatalog).mockResolvedValue({ known: true, githubCopilot: true, agentTypes: ["hermes"], sharedModels: [
    { name: "small-model" }, { name: "gateway-auto", routerModels: ["small-model"] }, { name: "gateway-plugin", routerUnverifiable: true },
  ], decisionModels: ["decision-1"] });
  const user = userEvent.setup(); renderEditor();
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Add smart-routing model" }));
  const added = screen.getByRole("group", { name: "Smart-routing model 2" });
  await user.selectOptions(within(added).getByRole("combobox", { name: "Classifier" }), "llm");
  await waitFor(() => expect(within(added).getByRole("option", { name: "small-model" })).toBeInTheDocument());
  const offered = within(added).getAllByRole("option").map((option) => option.textContent);
  expect(offered).not.toContain("gateway-auto");
  expect(offered).not.toContain("gateway-plugin");
  // A decision model only classifies: neither a tier nor the model classifier is offered one.
  expect(offered).not.toContain("decision-1");
  const suggested = Array.from(document.querySelectorAll("datalist option")).map((option) => option.getAttribute("value"));
  expect(suggested).toEqual(["small-model"]);
});
