// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, expect, it, vi } from "vitest";
import { TeamEditPage } from "./TeamEditPage";
import * as teams from "../../api/adminTeam";
import * as policies from "../../api/adminPolicy";
import * as sandboxes from "../../api/adminSandbox";
import * as marketplaceAPI from "../../api/skillMarketplace";
import type { Features } from "../../types/features";

vi.mock("../../api/adminTeam");
vi.mock("../../api/adminPolicy");
vi.mock("../../api/adminSandbox");
vi.mock("../../api/skillMarketplace");

const withoutMarketplace: Features = { tailscaleEnabled: false, agentIdentityEnabled: false, agentToolsEnabled: false, maxInstancesPerUser: 1 };
const features = vi.hoisted(() => ({ data: undefined as Features | undefined }));
vi.mock("../../hooks/useFeatures", () => ({ useFeatures: () => features }));

const team: teams.TeamDTO = { name: "example", uid: "team-uid", generation: 1, resourceVersion: "7", agentPolicyRef: "standard", sandboxRefs: ["small", "large"], defaultSandboxRef: "large", role: "Engineer" };
beforeEach(() => {
  vi.resetAllMocks();
  features.data = withoutMarketplace;
  vi.mocked(teams.getTeam).mockResolvedValue(team);
  vi.mocked(teams.createTeam).mockResolvedValue(team);
  vi.mocked(teams.updateTeam).mockResolvedValue(team);
  vi.mocked(policies.listPolicies).mockResolvedValue([{ name: "standard", uid: "policy-uid", resourceVersion: "1", harnesses: { allowed: ["hermes"] }, sandboxSelection: { allowMultiple: true } }]);
  vi.mocked(sandboxes.listSandboxes).mockResolvedValue(["small", "large"].map(name => ({ name, deleting: false }) as sandboxes.SandboxDTO));
});

function renderEditor(path = "/admin/teams/new") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[path]}><Routes>
    <Route path="/admin/teams/new" element={<TeamEditPage />} />
    <Route path="/admin/teams/:name/edit" element={<TeamEditPage />} />
    <Route path="/admin/teams" element={<div>Team list</div>} />
  </Routes></MemoryRouter></QueryClientProvider>);
}

it("creates a Team with its policy, allowed Sandboxes and selected default", async () => {
  const user = userEvent.setup(); renderEditor();
  await user.type(screen.getByRole("textbox", { name: "Name" }), "example");
  await user.selectOptions(await screen.findByRole("combobox", { name: "Agent Policy" }), "standard");
  await user.click(await screen.findByRole("checkbox", { name: "small" }));
  await user.click(screen.getByRole("checkbox", { name: "large" }));
  await user.selectOptions(screen.getByRole("combobox", { name: "Default Sandbox" }), "large");
  await user.click(screen.getByRole("button", { name: "Create" }));
  await waitFor(() => expect(teams.createTeam).toHaveBeenCalledWith(expect.objectContaining({ name: "example", agentPolicyRef: "standard", sandboxRefs: ["small", "large"], defaultSandboxRef: "large" })));
});

it("offers creation links and blocks submission when prerequisites are absent", async () => {
  vi.mocked(policies.listPolicies).mockResolvedValue([]); vi.mocked(sandboxes.listSandboxes).mockResolvedValue([]);
  renderEditor();
  expect(await screen.findByRole("link", { name: "Agent Policy" })).toHaveAttribute("href", "/admin/policies/new");
  expect(screen.getByRole("link", { name: "Sandbox" })).toHaveAttribute("href", "/admin/sandboxes/new");
  // Each notice reads as one sentence: its words and link sit in one
  // MessageBarBody, not in separate items of the MessageBar's row.
  for (const name of ["Agent Policy", "Sandbox"]) {
    const body = screen.getByRole("link", { name }).closest(".fui-MessageBarBody");
    expect(body).not.toBeNull();
    expect(body).toHaveTextContent(new RegExp(`^Create an? ${name} before creating this Team\\.$`));
  }
  expect(screen.getByRole("group", { name: /Allowed Sandboxes/ })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
  fireEvent.submit(screen.getByRole("button", { name: "Create" }).closest("form")!);
  expect(teams.createTeam).not.toHaveBeenCalled();
});

it("blocks failed lookups and offers a retry", async () => {
  vi.mocked(sandboxes.listSandboxes).mockRejectedValueOnce(new Error("Sandboxes unavailable"));
  const user = userEvent.setup(); renderEditor();
  expect(await screen.findByText("Sandboxes unavailable")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
  await user.click(screen.getByRole("button", { name: "Retry configuration" }));
  await waitFor(() => expect(screen.queryByText("Sandboxes unavailable")).not.toBeInTheDocument());
  expect(teams.createTeam).not.toHaveBeenCalled();
});

it("round-trips bindings and the reviewed version without retrying a stale save", async () => {
  vi.mocked(teams.getTeam).mockResolvedValue({ ...team, name: "new" });
  vi.mocked(teams.updateTeam).mockRejectedValue(new Error("Configuration changed; reload it before saving or deleting"));
  const user = userEvent.setup(); renderEditor("/admin/teams/new/edit");
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  expect(screen.getByRole("textbox", { name: "Name" })).toHaveValue("new");
  expect(screen.getByRole("combobox", { name: "Default Sandbox" })).toHaveValue("large");
  await user.click(screen.getByRole("checkbox", { name: "large" }));
  expect(screen.getByRole("combobox", { name: "Default Sandbox" })).toHaveValue("small");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText(/Configuration changed/)).toBeInTheDocument();
  expect(teams.updateTeam).toHaveBeenCalledExactlyOnceWith("new", expect.objectContaining({ uid: "team-uid", resourceVersion: "7", agentPolicyRef: "standard", sandboxRefs: ["small"], defaultSandboxRef: "small" }));
  expect(teams.createTeam).not.toHaveBeenCalled();
});

it("preserves existing Sandbox choices but blocks a restrictive policy until they are fixed", async () => {
  vi.mocked(policies.listPolicies).mockResolvedValue([
    { name: "standard", uid: "policy-uid", resourceVersion: "1", harnesses: { allowed: ["hermes"] }, sandboxSelection: { allowMultiple: true } },
    { name: "single", uid: "single-uid", resourceVersion: "1", harnesses: { allowed: ["hermes"] }, sandboxSelection: { allowMultiple: false } },
  ]);
  const user = userEvent.setup();
  renderEditor("/admin/teams/example/edit");
  await user.selectOptions(await screen.findByRole("combobox", { name: "Agent Policy" }), "single");
  expect(screen.getByRole("checkbox", { name: "small" })).toBeChecked();
  expect(screen.getByRole("checkbox", { name: "large" })).toBeChecked();
  expect(screen.getByText(/keeps its existing Sandbox selections/i)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  await user.click(screen.getByRole("checkbox", { name: "large" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
});

it.each(["create", "update"])("shows validation details after %s and preserves the form for retry", async operation => {
  const message = "Team configuration was rejected by the cluster. spec.skills[0]: Invalid value. Check the Team fields.";
  const mutation = operation === "create" ? vi.mocked(teams.createTeam) : vi.mocked(teams.updateTeam);
  mutation.mockRejectedValueOnce(new Error(message));
  const user = userEvent.setup();
  renderEditor(operation === "create" ? "/admin/teams/new" : "/admin/teams/example/edit");
  if (operation === "create") {
    await user.type(screen.getByRole("textbox", { name: "Name" }), "example");
    await user.selectOptions(await screen.findByRole("combobox", { name: "Agent Policy" }), "standard");
    await user.click(await screen.findByRole("checkbox", { name: "small" }));
  }
  const role = await screen.findByRole("textbox", { name: "Role" });
  await user.clear(role);
  await user.type(role, "Platform Engineer");
  const button = screen.getByRole("button", { name: operation === "create" ? "Create" : "Save" });
  await waitFor(() => expect(button).toBeEnabled());
  await user.click(button);
  expect(await screen.findByText(message)).toBeInTheDocument();
  expect(screen.getByRole("textbox", { name: "Name" })).toHaveValue("example");
  expect(role).toHaveValue("Platform Engineer");
  expect(screen.queryByText("Team list")).not.toBeInTheDocument();
  await waitFor(() => expect(button).toBeEnabled());
  await user.click(button);
  expect(await screen.findByText("Team list")).toBeInTheDocument();
  expect(screen.queryByText(message)).not.toBeInTheDocument();
});

const marketplace: marketplaceAPI.SkillMarketplace = {
  enabled: true, known: true,
  catalog: {
    version: "v1", repository: { url: "https://git.example.com/org/skills.git" },
    skills: [
      { name: "pdf-tools", description: "Reads and fills PDF forms.", version: "bbbbbbbbbbbb", path: "skills/pdf-tools", files: 5, bytes: 4096 },
      { name: "web-search", description: "Searches the web.", version: "cccccccccccc", path: "skills/web-search", files: 1, bytes: 512 },
    ],
  },
};

function enableMarketplace(allowed: string[]) {
  features.data = { ...withoutMarketplace, skillMarketplaceEnabled: true };
  vi.mocked(policies.listPolicies).mockResolvedValue([{ name: "standard", uid: "policy-uid", resourceVersion: "1", harnesses: { allowed: ["hermes"] },
    sandboxSelection: { allowMultiple: true }, skills: { allowed } }]);
}

it("keeps the Skills text box without the Skill Marketplace", async () => {
  vi.mocked(teams.getTeam).mockResolvedValue({ ...team, skills: ["npm:@scope/tool", "legacy-skill"] });
  const user = userEvent.setup(); renderEditor("/admin/teams/example/edit");
  const skills = await screen.findByRole("textbox", { name: "Skills" });
  expect(skills).toHaveValue("npm:@scope/tool\nlegacy-skill");
  expect(screen.queryByRole("region", { name: "Team skills" })).not.toBeInTheDocument();
  expect(screen.queryByRole("textbox", { name: "npm skill packages, one per line" })).not.toBeInTheDocument();
  await user.type(skills, "{Enter}another-skill");
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(teams.updateTeam).toHaveBeenCalledOnce());
  // Unsorted, as entered.
  expect(vi.mocked(teams.updateTeam).mock.calls[0][1].skills).toEqual(["npm:@scope/tool", "legacy-skill", "another-skill"]);
  expect(marketplaceAPI.getSkillMarketplace).not.toHaveBeenCalled();
});

it("saves the skills chosen from the catalog together with the npm packages", async () => {
  enableMarketplace(["pdf-tools", "web-search", "retired-skill", "npm:@scope/tool", "npm:@scope/extra"]);
  vi.mocked(marketplaceAPI.getSkillMarketplace).mockResolvedValue(marketplace);
  vi.mocked(teams.getTeam).mockResolvedValue({ ...team, skills: ["retired-skill", "npm:@scope/tool", "pdf-tools"] });
  const user = userEvent.setup(); renderEditor("/admin/teams/example/edit");
  const section = within(await screen.findByRole("region", { name: "Team skills" }));
  expect(screen.queryByRole("textbox", { name: "Skills" })).not.toBeInTheDocument();
  expect(section.getByRole("checkbox", { name: "pdf-tools" })).toBeChecked();
  await user.click(section.getByRole("checkbox", { name: "web-search" }));
  await user.click(section.getByRole("checkbox", { name: "retired-skill (unavailable)" }));
  const npm = screen.getByRole("textbox", { name: "npm skill packages, one per line" });
  expect(npm).toHaveValue("npm:@scope/tool");
  await user.type(npm, "{Enter}npm:@scope/extra");
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(teams.updateTeam).toHaveBeenCalledOnce());
  expect(vi.mocked(teams.updateTeam).mock.calls[0][1].skills).toEqual(["npm:@scope/extra", "npm:@scope/tool", "pdf-tools", "web-search"]);
});

it("marks skills the selected Agent Policy does not allow", async () => {
  enableMarketplace(["pdf-tools"]);
  vi.mocked(marketplaceAPI.getSkillMarketplace).mockResolvedValue(marketplace);
  renderEditor("/admin/teams/example/edit");
  const section = within(await screen.findByRole("region", { name: "Team skills" }));
  await waitFor(() => expect(section.getByRole("checkbox", { name: "web-search" })).toBeDisabled());
  expect(section.getByRole("checkbox", { name: "web-search" })).toHaveAccessibleDescription(/Not allowed by the Agent Policy/);
  expect(section.getByRole("checkbox", { name: "pdf-tools" })).toBeEnabled();
});

it("refuses an npm skill package without the npm: prefix", async () => {
  enableMarketplace(["npm:@scope/tool"]);
  vi.mocked(marketplaceAPI.getSkillMarketplace).mockResolvedValue(marketplace);
  const user = userEvent.setup(); renderEditor("/admin/teams/example/edit");
  await screen.findByRole("region", { name: "Team skills" });
  await user.type(screen.getByRole("textbox", { name: "npm skill packages, one per line" }), "@scope/tool");
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText(/^Enter each npm skill package as npm:<package>.*Check: @scope\/tool\.$/)).toBeInTheDocument();
  expect(teams.updateTeam).not.toHaveBeenCalled();
});

it("keeps the Team's skills when the skill catalog cannot be read", async () => {
  enableMarketplace(["pdf-tools", "npm:@scope/tool"]);
  vi.mocked(marketplaceAPI.getSkillMarketplace).mockRejectedValue(new Error("The Skill Marketplace catalog could not be read. Check the Portal logs."));
  vi.mocked(teams.getTeam).mockResolvedValue({ ...team, skills: ["pdf-tools", "npm:@scope/tool"] });
  const user = userEvent.setup(); renderEditor("/admin/teams/example/edit");
  expect(await screen.findByText("Failed to load the skill catalog: The Skill Marketplace catalog could not be read. Check the Portal logs.")).toBeInTheDocument();
  expect(screen.getByText(/The skill catalog is not available/)).toBeInTheDocument();
  expect(screen.getByRole("checkbox", { name: "pdf-tools" })).toBeChecked();
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(teams.updateTeam).toHaveBeenCalledOnce());
  expect(vi.mocked(teams.updateTeam).mock.calls[0][1].skills).toEqual(["npm:@scope/tool", "pdf-tools"]);
});

it("shows why the Team API refused new skills", async () => {
  enableMarketplace(["pdf-tools", "web-search"]);
  vi.mocked(marketplaceAPI.getSkillMarketplace).mockResolvedValue(marketplace);
  const message = "spec.skills[1]: Skill web-search is not in the Skill Marketplace catalog. Choose a skill the marketplace has synchronized from its repository.";
  vi.mocked(teams.updateTeam).mockRejectedValueOnce(new Error(message));
  const user = userEvent.setup(); renderEditor("/admin/teams/example/edit");
  const section = within(await screen.findByRole("region", { name: "Team skills" }));
  await user.click(section.getByRole("checkbox", { name: "web-search" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText(message)).toBeInTheDocument();
  expect(section.getByRole("checkbox", { name: "web-search" })).toBeChecked();
});
