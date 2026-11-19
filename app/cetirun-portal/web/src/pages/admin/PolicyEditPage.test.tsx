// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PolicyEditPage } from "./PolicyEditPage";
import { fromPolicyDTO, toggleAllowedSkill, toPolicyRequest } from "./PolicyEditPage";
import * as policyAPI from "../../api/adminPolicy";
import * as marketplaceAPI from "../../api/skillMarketplace";
import type { Features } from "../../types/features";
import * as gatewayAPI from "../../api/aiGateway";

vi.mock("../../api/aiGateway");

vi.mock("../../api/adminPolicy", async (importOriginal) => {
  const original = await importOriginal<typeof import("../../api/adminPolicy")>();
  return {
    ...original,
    createPolicy: vi.fn(),
    getPolicy: vi.fn(),
    updatePolicy: vi.fn(),
  };
});
vi.mock("../../api/skillMarketplace");
const features = vi.hoisted(() => ({ data: undefined as Features | undefined }));
vi.mock("../../hooks/useFeatures", () => ({ useFeatures: () => features }));

const cachedPolicy: policyAPI.AgentPolicyDTO = {
  name: "standard", uid: "policy-uid", resourceVersion: "42",
  harnesses: { allowed: ["openclaw"] }, skills: { allowed: ["original-skill"] },
};

const legacyPolicy: policyAPI.AgentPolicyDTO = {
  ...cachedPolicy,
  modelAccess: {
    copilot: { allowed: true },
    gatewayBudget: { defaultMonthlyUSD: "20", maxMonthlyUSD: "50" },
  },
  network: {
    blockedCIDRs: ["192.0.2.0/24", "192.0.2.0/24"],
    blockedPorts: [{ port: 443, protocol: "TCP" }, { port: 443, protocol: "TCP" }],
    allowedInternalCIDRs: [
      { cidr: "198.51.100.0/24", ports: [{ port: 443, protocol: "TCP" }, { port: 443, protocol: "TCP" }] },
      { cidr: "198.51.100.0/24", ports: [{ port: 53, protocol: "UDP" }] },
    ],
    ingress: { mode: "authenticated" },
  },
};

function renderCachedPolicy(policy = cachedPolicy) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 10_000 } } });
  queryClient.setQueryData(["admin", "policy", "standard"], policy);
  render(<QueryClientProvider client={queryClient}><MemoryRouter initialEntries={["/admin/policies/standard/edit"]}><Routes>
    <Route path="/admin/policies/:name/edit" element={<PolicyEditPage />} />
    <Route path="/admin/policies" element={<div>Policy list</div>} />
  </Routes></MemoryRouter></QueryClientProvider>);
  return queryClient;
}

describe("PolicyEditPage", () => {
  beforeEach(() => {
    vi.mocked(policyAPI.createPolicy).mockReset();
    vi.mocked(policyAPI.getPolicy).mockReset();
    vi.mocked(policyAPI.updatePolicy).mockReset();
    vi.mocked(marketplaceAPI.getSkillMarketplace).mockReset();
    features.data = undefined;
    vi.mocked(gatewayAPI.getAIGatewayCatalog).mockReset().mockResolvedValue({
      known: true, sharedModels: [], githubCopilot: true, agentTypes: ["openclaw"],
    });
  });

  it("adds and removes one catalog skill in the allowed skills text without touching other entries", () => {
    expect(toggleAllowedSkill("", "pdf-tools", true)).toBe("pdf-tools");
    expect(toggleAllowedSkill("npm:@scope/tool\n\n", "pdf-tools", true)).toBe("npm:@scope/tool\npdf-tools");
    expect(toggleAllowedSkill("pdf-tools", "pdf-tools", true)).toBe("pdf-tools");
    expect(toggleAllowedSkill("pdf-tools\n npm:@scope/tool\n pdf-tools ", "pdf-tools", false)).toBe(" npm:@scope/tool");
    expect(toggleAllowedSkill("pdf-tools-extra", "pdf-tools", false)).toBe("pdf-tools-extra");
  });

  it("toggles Skill Marketplace skills in the allowed skills and keeps other entries", async () => {
    features.data = { tailscaleEnabled: false, agentIdentityEnabled: false, agentToolsEnabled: false, maxInstancesPerUser: 1, skillMarketplaceEnabled: true };
    vi.mocked(marketplaceAPI.getSkillMarketplace).mockResolvedValue({ enabled: true, known: true, catalog: {
      version: "v1", repository: { url: "https://git.example.com/org/skills.git" },
      skills: [
        { name: "pdf-tools", description: "Reads and fills PDF forms.", version: "bbbbbbbbbbbb", path: "skills/pdf-tools", files: 5, bytes: 4096 },
        { name: "web-search", description: "Searches the web.", version: "cccccccccccc", path: "skills/web-search", files: 1, bytes: 512 },
      ],
    } });
    const policy = { ...cachedPolicy, skills: { allowed: ["original-skill", "npm:@scope/tool", "web-search"] } };
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(policy);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(policy);
    const user = userEvent.setup();
    renderCachedPolicy(policy);
    const skills = screen.getByRole("textbox", { name: "Allowed skills" });
    const list = within(await screen.findByRole("group", { name: "Allowed marketplace skills" }));
    expect(list.getByRole("checkbox", { name: "web-search" })).toBeChecked();
    expect(list.getByRole("checkbox", { name: "pdf-tools" })).not.toBeChecked();
    await user.click(list.getByRole("checkbox", { name: "pdf-tools" }));
    expect(skills).toHaveValue("original-skill\nnpm:@scope/tool\nweb-search\npdf-tools");
    await user.click(list.getByRole("checkbox", { name: "web-search" }));
    expect(skills).toHaveValue("original-skill\nnpm:@scope/tool\npdf-tools");
    // A name typed into the list checks its box too.
    fireEvent.change(skills, { target: { value: "original-skill\nnpm:@scope/tool\npdf-tools\nweb-search" } });
    expect(list.getByRole("checkbox", { name: "web-search" })).toBeChecked();
    await user.click(list.getByRole("checkbox", { name: "web-search" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].skills).toEqual({ allowed: ["original-skill", "npm:@scope/tool", "pdf-tools"] });
  });

  it("offers no catalog checklist without the Skill Marketplace", () => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(cachedPolicy);
    renderCachedPolicy();
    expect(screen.getByRole("textbox", { name: "Allowed skills" })).toHaveValue("original-skill");
    expect(screen.queryByRole("region", { name: "Skill Marketplace skills" })).not.toBeInTheDocument();
    expect(marketplaceAPI.getSkillMarketplace).not.toHaveBeenCalled();
  });

  it("preserves Custom policy permission and supports explicit removal", async () => {
    const policy: policyAPI.AgentPolicyDTO = { ...cachedPolicy, harnesses: { allowed: ["custom"] } };
    expect(toPolicyRequest(fromPolicyDTO(policy)).harnesses.allowed).toEqual(["custom"]);
    renderCachedPolicy(policy);
    const custom = screen.getByRole("switch", { name: "Custom" });
    expect(custom).toBeChecked();
    expect(screen.getByRole("switch", { name: "OpenClaw" })).not.toBeChecked();
    await userEvent.setup().click(custom);
    expect(custom).not.toBeChecked();
  });

  it("keeps skill examples and offers a model picker", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={["/admin/policies/create"]}>
          <Routes>
            <Route path="/admin/policies/create" element={<PolicyEditPage />} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByRole("textbox", { name: "Allowed skills" })).toHaveAttribute(
      "placeholder",
      "skill-a\nnpm:@scope/skill-b\nskill-c@1.2.0",
    );
    expect(screen.getByRole("combobox", { name: "Allowed shared models" })).toHaveAttribute(
      "placeholder",
      "Search models",
    );
    expect(screen.getAllByText(/commas are not separators/i)).toHaveLength(1);
    const readonly = within(screen.getByRole("region", { name: "Read-only policy capabilities" }));
    const kubernetes = within(readonly.getByRole("region", { name: "Kubernetes API access" }));
    expect(kubernetes.getByText("No Kubernetes API access is configured in this policy.")).toBeInTheDocument();
    expect(kubernetes.queryByRole("switch")).not.toBeInTheDocument();
    expect(kubernetes.queryByRole("textbox")).not.toBeInTheDocument();
    expect(kubernetes.queryByRole("button")).not.toBeInTheDocument();
  });

  it("clears the Gateway budget when the last model source is removed", async () => {
    const user = userEvent.setup();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={queryClient}><MemoryRouter initialEntries={["/admin/policies/create"]}><Routes>
      <Route path="/admin/policies/create" element={<PolicyEditPage />} />
    </Routes></MemoryRouter></QueryClientProvider>);
    const copilot = screen.getByRole("switch", { name: "Allow GitHub Copilot" });
    const budget = screen.getByRole("switch", { name: "Set monthly Gateway budget" });
    await user.click(copilot);
    await user.click(budget);
    expect(budget).toBeChecked();
    await user.click(copilot);
    expect(budget).not.toBeChecked();
  });

  it("edits a policy named new instead of entering create mode", async () => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue({
      name: "new",
      uid: "policy-uid",
      resourceVersion: "42",
      kubernetesAccess: {
        serviceAccountName: "agent-access",
        roleBindings: [{ namespace: "tools", clusterRoleName: "view" }],
        clusterRoleBindings: [{ clusterRoleName: "cluster-reader" }],
      },
      harnesses: { allowed: ["openclaw"] },
      skills: { allowed: [] },
      modelAccess: {
        copilot: { allowed: false },
        sharedModels: { allowed: [] },
      },
    });
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue({
      name: "new",
      uid: "policy-uid",
      resourceVersion: "43",
      harnesses: { allowed: ["openclaw"] },
    });
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={["/admin/policies/new/edit"]}>
          <Routes>
            <Route path="/admin/policies/:name/edit" element={<PolicyEditPage />} />
            <Route path="/admin/policies" element={<div>Policy list</div>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(await screen.findByText("Agent policy: new")).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Name" })).toBeDisabled();
    const readonly = within(screen.getByRole("region", { name: "Read-only policy capabilities" }));
    const kubernetes = within(readonly.getByRole("region", { name: "Kubernetes API access" }));
    expect(kubernetes.getByText("Service account: agent-access")).toBeInTheDocument();
    expect(kubernetes.getByText("ClusterRole view in namespace tools")).toBeInTheDocument();
    expect(kubernetes.getByText("ClusterRole cluster-reader")).toBeInTheDocument();
    expect(kubernetes.queryByRole("switch")).not.toBeInTheDocument();
    expect(kubernetes.queryByRole("textbox")).not.toBeInTheDocument();
    expect(kubernetes.queryByRole("button")).not.toBeInTheDocument();
    fireEvent.submit(screen.getByRole("button", { name: "Save" }).closest("form")!);

    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledWith("new", expect.objectContaining({
      name: "new",
      uid: "policy-uid",
      resourceVersion: "42",
    })));
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1]).not.toHaveProperty("kubernetesAccess");
    expect(policyAPI.createPolicy).not.toHaveBeenCalled();
  });

  it("refreshes a pristine cached form and saves the latest resource version", async () => {
    const latest = { ...cachedPolicy, resourceVersion: "43", skills: { allowed: ["remote-skill"] } };
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(latest);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(latest);
    renderCachedPolicy();

    await waitFor(() => expect(screen.getByRole("textbox", { name: "Allowed skills" })).toHaveValue("remote-skill"));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledWith("standard", expect.objectContaining({
      resourceVersion: "43", skills: { allowed: ["remote-skill"] },
    })));
  });

  it.each(["policy-uid", "replacement-uid"])("preserves dirty fields when a refetch returns UID %s", async (uid) => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(cachedPolicy);
    const queryClient = renderCachedPolicy();
    const skills = screen.getByRole("textbox", { name: "Allowed skills" });
    fireEvent.change(skills, { target: { value: "local-skill" } });
    const latest = { ...cachedPolicy, uid, resourceVersion: "43", skills: { allowed: ["remote-skill"] } };
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(latest);
    await act(async () => { await queryClient.refetchQueries({ queryKey: ["admin", "policy", "standard"] }); });

    expect(skills).toHaveValue("local-skill");
    await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeDisabled());
    expect(screen.getByText(/Your unsaved edits have been kept/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Discard edits and reload" }));
    await waitFor(() => expect(screen.getByRole("textbox", { name: "Allowed skills" })).toHaveValue("remote-skill"));
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(latest);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledWith("standard", expect.objectContaining({
      uid, resourceVersion: "43", skills: { allowed: ["remote-skill"] },
    })));
  });

  it("recovers from a save conflict with an explicit reload and retains edits if reload fails", async () => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(cachedPolicy);
    vi.mocked(policyAPI.updatePolicy).mockRejectedValue(new policyAPI.PolicyConflictError("Configuration changed."));
    renderCachedPolicy();
    fireEvent.change(screen.getByRole("textbox", { name: "Allowed skills" }), { target: { value: "local-skill" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Configuration changed.")).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Allowed skills" })).toHaveValue("local-skill");

    vi.mocked(policyAPI.getPolicy).mockRejectedValueOnce(new Error("Service unavailable"));
    fireEvent.click(screen.getByRole("button", { name: "Discard edits and reload" }));
    expect(await screen.findByText("Failed to reload policy: Service unavailable")).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Allowed skills" })).toHaveValue("local-skill");

    const latest = { ...cachedPolicy, resourceVersion: "44", skills: { allowed: ["remote-skill"] } };
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(latest);
    fireEvent.click(screen.getByRole("button", { name: "Discard edits and reload" }));
    await waitFor(() => expect(screen.getByRole("textbox", { name: "Allowed skills" })).toHaveValue("remote-skill"));
    expect(screen.queryByText(/Failed to reload policy/)).not.toBeInTheDocument();
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(latest);
    fireEvent.change(screen.getByRole("textbox", { name: "Allowed skills" }), { target: { value: "reapplied-skill" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenLastCalledWith("standard", expect.objectContaining({
      resourceVersion: "44", skills: { allowed: ["reapplied-skill"] },
    })));
    expect(await screen.findByText("Policy list")).toBeInTheDocument();
  });

  it("keeps a network rule draft through a refetch and refreshes after it is cancelled", async () => {
    const user = userEvent.setup();
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(cachedPolicy);
    const queryClient = renderCachedPolicy();
    await user.click(screen.getByRole("button", { name: "Add network rule" }));
    const dialog = within(await screen.findByRole("dialog", { hidden: true }));
    fireEvent.change(dialog.getByRole("textbox", { name: "Destination (CIDR)", hidden: true }), { target: { value: "10.20.0.0/24" } });
    const latest = { ...cachedPolicy, resourceVersion: "43", skills: { allowed: ["remote-skill"] } };
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(latest);
    await act(async () => { await queryClient.refetchQueries({ queryKey: ["admin", "policy", "standard"] }); });
    expect(dialog.getByRole("textbox", { name: "Destination (CIDR)", hidden: true })).toHaveValue("10.20.0.0/24");
    expect(screen.getByRole("textbox", { name: "Allowed skills", hidden: true })).toHaveValue("original-skill");
    await user.click(dialog.getByRole("button", { name: "Cancel", hidden: true }));
    await waitFor(() => expect(screen.queryByRole("dialog", { hidden: true })).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole("textbox", { name: "Allowed skills" })).toHaveValue("remote-skill"));
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
  });

  it("maps editable policy controls and excludes read-only capability fields", () => {
    const request = toPolicyRequest({
      ...fromPolicyDTO({
        name: "standard", uid: "policy-uid", resourceVersion: "42", harnesses: { allowed: ["openclaw"] },
        agentIdentity: { allowed: true, required: true, credentialServices: ["azure-resource-manager"] },
        entraID: { enabled: true },
        kubernetesAccess: { serviceAccountName: "portal.agent", roleBindings: [{ namespace: "tools", clusterRoleName: "reader" }] },
        network: { blockedFQDNs: ["example.com"], ingress: { requireMTLS: true } },
      }),
      allowMultiple: true,
      readOnlyRootFilesystem: false,
      blockedCIDRs: "10.0.0.0/8\n2001:db8::/32",
      blockedPorts: [{ port: 443, protocol: "TCP" }],
      allowedInternalCIDRs: [{ cidr: "10.1.0.0/16", ports: "53/UDP" }],
      ingressMode: "disabled",
      budgetEnabled: true,
      defaultMonthlyUSD: "20",
      maxMonthlyUSD: "50",
    });
    expect(request).toMatchObject({
      sandboxSelection: { allowMultiple: true }, security: { readOnlyRootFilesystem: false },
      network: {
        blockedCIDRs: ["10.0.0.0/8", "2001:db8::/32"], blockedPorts: [{ port: 443, protocol: "TCP" }],
        allowedInternalCIDRs: [{ cidr: "10.1.0.0/16", ports: [{ port: 53, protocol: "UDP" }] }], ingress: { mode: "disabled" },
      },
      modelAccess: { gatewayBudget: { defaultMonthlyUSD: "20", maxMonthlyUSD: "50" } },
      agentIdentity: { allowed: true, required: true, credentialServices: ["azure-resource-manager"] },
      entraID: { enabled: true },
    });
    expect(request).not.toHaveProperty("kubernetesAccess");
    expect(JSON.stringify(request)).not.toContain("blockedFQDNs");
    expect(JSON.stringify(request)).not.toContain("requireMTLS");
  });

  it.each([
    ["disabled", false], ["disabled", true],
    ["optional", false], ["optional", true],
    ["required", false], ["required", true],
  ] as const)("creates %s Agent Identity with Entra projection %s independently", async (mode, entraEnabled) => {
    vi.mocked(policyAPI.createPolicy).mockResolvedValue(cachedPolicy);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={queryClient}><MemoryRouter initialEntries={["/admin/policies/create"]}><Routes>
      <Route path="/admin/policies/create" element={<PolicyEditPage />} />
      <Route path="/admin/policies" element={<div>Policy list</div>} />
    </Routes></MemoryRouter></QueryClientProvider>);

    const identity = screen.getByRole("combobox", { name: "Agent Identity", exact: true });
    const entra = screen.getByRole("switch", { name: "Entra workload identity" });
    expect(identity).toHaveValue("disabled");
    expect(entra).not.toBeChecked();
    fireEvent.change(screen.getByRole("textbox", { name: "Name" }), { target: { value: "standard" } });
    fireEvent.change(identity, { target: { value: mode } });
    expect(entra).not.toBeChecked();
    if (entraEnabled) fireEvent.click(entra);
    expect(identity).toHaveValue(mode);
    fireEvent.click(screen.getByRole("button", { name: "Create", exact: true }));

    await waitFor(() => expect(policyAPI.createPolicy).toHaveBeenCalledWith(expect.objectContaining({
      agentIdentity: {
        allowed: mode !== "disabled", required: mode === "required",
        credentialServices: mode === "disabled" ? [] : ["azure-resource-manager"],
      },
      entraID: { enabled: entraEnabled },
    }), expect.anything()));
    expect(await screen.findByText("Policy list")).toBeInTheDocument();
  });

  it.each([
    ["disabled", false], ["disabled", true],
    ["optional", false], ["optional", true],
    ["required", false], ["required", true],
  ] as const)("loads and saves %s Agent Identity with Entra projection %s", async (mode, entraEnabled) => {
    const policy = {
      ...cachedPolicy,
      agentIdentity: {
        allowed: mode !== "disabled", required: mode === "required",
        credentialServices: mode === "disabled" ? [] : ["azure-resource-manager"],
      },
      entraID: { enabled: entraEnabled },
    };
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(policy);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(policy);
    renderCachedPolicy(policy);
    expect(screen.getByRole("combobox", { name: "Agent Identity", exact: true })).toHaveValue(mode);
    expect((screen.getByRole("switch", { name: "Entra workload identity" }) as HTMLInputElement).checked).toBe(entraEnabled);
    fireEvent.change(screen.getByRole("textbox", { name: "Allowed skills" }), { target: { value: "updated-skill" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledWith("standard", expect.objectContaining({
      agentIdentity: policy.agentIdentity, entraID: policy.entraID, skills: { allowed: ["updated-skill"] },
    })));
  });

  it("disables both identities and clears credential services", async () => {
    const policy = {
      ...cachedPolicy,
      agentIdentity: { allowed: true, required: true, credentialServices: ["azure-resource-manager"] },
      entraID: { enabled: true },
    };
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(policy);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(policy);
    renderCachedPolicy(policy);
    fireEvent.change(screen.getByRole("combobox", { name: "Agent Identity", exact: true }), { target: { value: "disabled" } });
    const entra = screen.getByRole("switch", { name: "Entra workload identity" });
    expect(entra).toBeChecked();
    fireEvent.click(entra);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledWith("standard", expect.objectContaining({
      agentIdentity: { allowed: false, required: false, credentialServices: [] }, entraID: { enabled: false },
    })));
  });

  it("keeps identity drafts on a policy conflict and reloads the current identity settings", async () => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(cachedPolicy);
    const queryClient = renderCachedPolicy();
    const identity = screen.getByRole("combobox", { name: "Agent Identity", exact: true });
    const entra = screen.getByRole("switch", { name: "Entra workload identity" });
    fireEvent.change(identity, { target: { value: "required" } });
    fireEvent.click(entra);
    const latest = {
      ...cachedPolicy, resourceVersion: "43",
      agentIdentity: { allowed: true, required: false, credentialServices: ["azure-resource-manager"] },
      entraID: { enabled: false },
    };
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(latest);
    await act(async () => { await queryClient.refetchQueries({ queryKey: ["admin", "policy", "standard"] }); });
    expect(identity).toHaveValue("required");
    expect(entra).toBeChecked();
    await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeDisabled());
    fireEvent.click(screen.getByRole("button", { name: "Discard edits and reload" }));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Agent Identity", exact: true })).toHaveValue("optional"));
    expect(screen.getByRole("switch", { name: "Entra workload identity" })).not.toBeChecked();
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(latest);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledWith("standard", expect.objectContaining({
      resourceVersion: "43", agentIdentity: latest.agentIdentity, entraID: latest.entraID,
    })));
  });

  it.each(["skills", "budget"])("saves %s edits without resubmitting legacy network duplicates", async (field) => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(legacyPolicy);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(legacyPolicy);
    renderCachedPolicy(legacyPolicy);
    fireEvent.change(screen.getByRole("textbox", { name: field === "skills" ? "Allowed skills" : "Maximum monthly USD" }), {
      target: { value: field === "skills" ? "updated-skill" : "60" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    const request = vi.mocked(policyAPI.updatePolicy).mock.calls[0][1];
    expect(request).toMatchObject({ uid: legacyPolicy.uid, resourceVersion: legacyPolicy.resourceVersion });
    expect(request).not.toHaveProperty("network");
    if (field === "skills") expect(request.skills?.allowed).toEqual(["updated-skill"]);
    else expect(request.modelAccess?.gatewayBudget?.maxMonthlyUSD).toBe("60");
    expect(await screen.findByText("Policy list")).toBeInTheDocument();
  });

  it("sends only the changed ingress mode while retaining legacy network lists", async () => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(legacyPolicy);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(legacyPolicy);
    renderCachedPolicy(legacyPolicy);
    fireEvent.change(screen.getByRole("combobox", { name: "Ingress mode" }), { target: { value: "disabled" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].network).toEqual({ ingress: { mode: "disabled" } });
  });

  it.each(["blockedCIDRs", "blockedPorts", "allowedInternalCIDRs"])("explicitly clears %s without resubmitting other network fields", async (field) => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(legacyPolicy);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(legacyPolicy);
    renderCachedPolicy(legacyPolicy);
    if (field === "blockedCIDRs") {
      fireEvent.change(screen.getByRole("textbox", { name: "Blocked CIDRs" }), { target: { value: "" } });
    } else {
      const label = field === "blockedPorts" ? "Remove" : "Delete rule";
      const count = screen.getAllByRole("button", { name: label, exact: true }).length;
      for (let index = 0; index < count; index++) fireEvent.click(screen.getAllByRole("button", { name: label, exact: true })[0]);
    }
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].network).toEqual({ [field]: [] });
  });

  it("still validates changed ports and submits a repaired list without touching other legacy fields", async () => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(legacyPolicy);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(legacyPolicy);
    renderCachedPolicy(legacyPolicy);
    const firstPort = screen.getByRole("spinbutton", { name: "Blocked port 1" });
    fireEvent.change(firstPort, { target: { value: "0" } });
    fireEvent.submit(screen.getByRole("button", { name: "Save" }).closest("form")!);
    expect(await screen.findByText("Ports must be 1–65535 and use TCP, UDP, or SCTP.")).toBeInTheDocument();
    expect(policyAPI.updatePolicy).not.toHaveBeenCalled();
    fireEvent.change(firstPort, { target: { value: "8443" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalledOnce());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].network).toEqual({
      blockedPorts: [{ port: 8443, protocol: "TCP" }, { port: 443, protocol: "TCP" }],
    });
  });

  it("preserves every port and protocol when an existing policy is saved", () => {
    const rules: policyAPI.AgentPolicyCIDRRule[] = [{
      cidr: "10.20.0.0/24",
      ports: [{ port: 443, protocol: "TCP" }, { port: 443, protocol: "UDP" }, { port: 5000, protocol: "SCTP" }],
    }];
    const form = fromPolicyDTO({
      name: "standard", uid: "policy-uid", resourceVersion: "42", harnesses: { allowed: ["openclaw"] },
      network: { allowedInternalCIDRs: rules },
    });
    expect(form.allowedInternalCIDRs[0].ports).toBe("443, 443/UDP, 5000/SCTP");
    expect(toPolicyRequest(form).network?.allowedInternalCIDRs).toEqual(rules);
  });

  it("edits and deletes whole rule groups, and discards cancelled edits", async () => {
    const user = userEvent.setup();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={queryClient}><MemoryRouter initialEntries={["/admin/policies/create"]}><Routes>
      <Route path="/admin/policies/create" element={<PolicyEditPage />} />
      <Route path="/admin/policies" element={<div>Policy list</div>} />
    </Routes></MemoryRouter></QueryClientProvider>);
    await user.type(screen.getByRole("textbox", { name: "Name" }), "standard");
    const section = within(screen.getByRole("region", { name: "Internal network access" }));
    const setDraft = async (cidr: string, ports: string) => {
      // Tabster temporarily applies aria-hidden while its portal modalizer settles in jsdom.
      // Assert against the mounted dialog and wait for actual unmount before opening the next one.
      const dialog = within(await screen.findByRole("dialog", { hidden: true }));
      fireEvent.change(dialog.getByRole("textbox", { name: "Destination (CIDR)", hidden: true }), { target: { value: cidr } });
      fireEvent.change(dialog.getByRole("textbox", { name: "Allowed ports", hidden: true }), { target: { value: ports } });
      return dialog;
    };
    const closed = async () => {
      await waitFor(() => expect(screen.queryByRole("dialog", { hidden: true })).not.toBeInTheDocument());
      await section.findByRole("button", { name: "Add network rule" });
    };
    for (const [cidr, ports] of [["10.20.0.0/24", "443, 5432, 53/udp"], ["10.30.0.0/24", "80, 8080"]]) {
      await user.click(section.getByRole("button", { name: "Add network rule" }));
      const dialog = await setDraft(cidr, ports);
      await user.click(dialog.getByRole("button", { name: "Add rule", exact: true, hidden: true }));
      await closed();
    }
    expect(section.queryByRole("textbox")).not.toBeInTheDocument();
    expect(section.queryByRole("combobox")).not.toBeInTheDocument();
    expect(section.queryByRole("button", { name: /add port|remove/i })).not.toBeInTheDocument();
    const first = within(section.getByRole("group", { name: "Internal network rule 1" }));
    expect(first.getByText("10.20.0.0/24")).toBeInTheDocument();
    expect(first.getByText("443, 5432, 53/UDP")).toBeInTheDocument();
    await user.click(first.getByRole("button", { name: "Edit rule" }));
    let dialog = await setDraft("10.40.0.0/24", "8443");
    await user.click(dialog.getByRole("button", { name: "Cancel", exact: true, hidden: true }));
    await closed();
    expect(first.getByText("10.20.0.0/24")).toBeInTheDocument();
    expect(first.getByText("443, 5432, 53/UDP")).toBeInTheDocument();
    await user.click(first.getByRole("button", { name: "Edit rule" }));
    dialog = await setDraft("10.30.0.0/24", "8443");
    await user.click(dialog.getByRole("button", { name: "Update rule", hidden: true }));
    expect(dialog.getByText(/This destination already has a rule/)).toBeInTheDocument();
    dialog = await setDraft("2001:db8::192.0.2.1/128", "8443, 5353/udp");
    await user.click(dialog.getByRole("textbox", { name: "Allowed ports", hidden: true }));
    await user.keyboard("{Enter}");
    await closed();
    expect(first.getByText("2001:db8::192.0.2.1/128")).toBeInTheDocument();
    expect(first.getByText("8443, 5353/UDP")).toBeInTheDocument();
    expect(policyAPI.createPolicy).not.toHaveBeenCalled();
    const second = within(section.getByRole("group", { name: "Internal network rule 2" }));
    await user.click(second.getByRole("button", { name: "Delete rule" }));
    expect(section.getAllByRole("group")).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: "Create", exact: true }));
    await waitFor(() => expect(policyAPI.createPolicy).toHaveBeenCalledWith(expect.objectContaining({
      network: expect.objectContaining({ allowedInternalCIDRs: [{
        cidr: "2001:db8::192.0.2.1/128", ports: [{ port: 8443, protocol: "TCP" }, { port: 5353, protocol: "UDP" }],
      }] }),
    }), expect.anything()));
  });

  it("keeps invalid drafts out of the rule list and discards them on cancel", async () => {
    const user = userEvent.setup();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={queryClient}><MemoryRouter initialEntries={["/admin/policies/create"]}><Routes>
      <Route path="/admin/policies/create" element={<PolicyEditPage />} />
      <Route path="/admin/policies" element={<div>Policy list</div>} />
    </Routes></MemoryRouter></QueryClientProvider>);
    await user.type(screen.getByRole("textbox", { name: "Name" }), "standard");
    const section = within(screen.getByRole("region", { name: "Internal network access" }));
    await user.click(section.getByRole("button", { name: "Add network rule" }));
    const dialog = within(await screen.findByRole("dialog", { hidden: true }));
    const submitRule = () => fireEvent.submit(dialog.getByRole("button", { name: "Add rule", exact: true, hidden: true }).closest("form")!);
    fireEvent.change(dialog.getByRole("textbox", { name: "Destination (CIDR)", hidden: true }), { target: { value: "invalid-cidr" } });
    submitRule();
    expect(dialog.getByText(/Enter a valid destination CIDR/)).toBeInTheDocument();
    fireEvent.change(dialog.getByRole("textbox", { name: "Destination (CIDR)", hidden: true }), { target: { value: "10.20.0.0/24" } });
    const ports = dialog.getByRole("textbox", { name: "Allowed ports", hidden: true });
    for (const value of ["", "443,", "0", "65536", "443/HTTP", "443, 443/TCP", "80-90", "4e2"]) {
      fireEvent.change(ports, { target: { value } });
      submitRule();
      expect(dialog.getByText(/ports from|appear only once/)).toBeInTheDocument();
      expect(ports).toHaveValue(value);
      expect(policyAPI.createPolicy).not.toHaveBeenCalled();
    }
    await user.click(dialog.getByRole("button", { name: "Cancel", exact: true, hidden: true }));
    await waitFor(() => expect(screen.queryByRole("dialog", { hidden: true })).not.toBeInTheDocument());
    expect(section.getByText("No additional internal access configured.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create", exact: true }));
    await waitFor(() => expect(policyAPI.createPolicy).toHaveBeenCalledWith(expect.objectContaining({
      network: expect.objectContaining({ allowedInternalCIDRs: [] }),
    }), expect.anything()));
  });

  function renderEdit(name: string) {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={[`/admin/policies/${name}/edit`]}>
          <Routes>
            <Route path="/admin/policies/:name/edit" element={<PolicyEditPage />} />
            <Route path="/admin/policies" element={<div>Policy list</div>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  const limitedPolicy: policyAPI.AgentPolicyDTO = {
    name: "limited",
    uid: "policy-uid",
    resourceVersion: "7",
    harnesses: { allowed: ["openclaw"] },
    modelAccess: {
      sharedModels: { allowed: ["example-model"] },
      gatewayBudget: { defaultMonthlyUSD: "10", maxMonthlyUSD: "20" },
      rateLimits: { maxParallelRequests: 4, tpm: 200000, perModel: { "example-model": { rpm: 10 } } },
    },
  };

  it("keeps the loaded rate limits when saving another change", async () => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(limitedPolicy);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(limitedPolicy);
    renderEdit("limited");
    expect(await screen.findByRole("textbox", { name: "Max parallel requests" })).toHaveValue("4");
    expect(screen.getByRole("textbox", { name: "Requests per minute" })).toHaveValue("");
    expect(screen.getByRole("textbox", { name: "Model name 1" })).toHaveValue("example-model");
    expect(screen.getByText(/counts the tokens of each finished request/i)).toBeInTheDocument();
    expect(screen.getByText(/limits alone.*without restarting running Agents/i)).toBeInTheDocument();
    expect(screen.queryByText(/restarts the Agents running under it once/i)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("switch", { name: "Hermes" }));
    fireEvent.submit(screen.getByRole("button", { name: "Save" }).closest("form")!);
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalled());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].modelAccess?.rateLimits).toEqual(limitedPolicy.modelAccess!.rateLimits);
  });

  it("sends new limits, removes cleared ones and never sends zero", async () => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(limitedPolicy);
    vi.mocked(policyAPI.updatePolicy).mockResolvedValue(limitedPolicy);
    renderEdit("limited");
    const parallel = await screen.findByRole("textbox", { name: "Max parallel requests" });
    fireEvent.change(parallel, { target: { value: "" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Requests per minute" }), { target: { value: "60" } });
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    fireEvent.click(screen.getByRole("button", { name: "Add per-model limit" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Model name 1" }), { target: { value: "auto" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Tokens per minute for model 1" }), { target: { value: "8192" } });
    fireEvent.submit(screen.getByRole("button", { name: "Save" }).closest("form")!);
    await waitFor(() => expect(policyAPI.updatePolicy).toHaveBeenCalled());
    expect(vi.mocked(policyAPI.updatePolicy).mock.calls[0][1].modelAccess?.rateLimits).toEqual({
      rpm: 60, tpm: 200000, perModel: { auto: { tpm: 8192 } },
    });
  });

  it("rejects a limit below the minimum before saving", async () => {
    vi.mocked(policyAPI.getPolicy).mockResolvedValue(limitedPolicy);
    renderEdit("limited");
    fireEvent.change(await screen.findByRole("textbox", { name: "Tokens per minute" }), { target: { value: "1000" } });
    fireEvent.submit(screen.getByRole("button", { name: "Save" }).closest("form")!);
    expect(await screen.findByText(/Tokens per minute must be at least 4,096/)).toBeInTheDocument();
    expect(policyAPI.updatePolicy).not.toHaveBeenCalled();
  });
});
