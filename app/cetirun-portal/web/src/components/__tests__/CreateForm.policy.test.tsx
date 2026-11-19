// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CreateForm } from "../CreateForm";
import type { CreateOptions } from "../../hooks/useCreateOptions";

const mutation = vi.hoisted(() => ({ mutate: vi.fn(), isPending: false, error: null as Error | null }));
vi.mock("../../hooks/useInstance", () => ({ useCreateInstance: () => mutation }));
vi.mock("../../hooks/useFeatures", () => ({
  useFeatures: () => ({ data: { aiGatewayEnabled: true, agentIdentityEnabled: true, allowedAgentTypes: ["hermes", "openclaw", "custom"] }, isLoading: false }),
}));
vi.mock("../../hooks/useTeams", () => ({
  useTeams: () => ({ data: [{ name: "restricted" }, { name: "research" }], isLoading: false }),
}));

function options(team = "restricted", overrides: Partial<CreateOptions> = {}): CreateOptions {
  return { team, policyName: `${team}-policy`, allowedAgentTypes: ["hermes"], aiGatewayEnabled: true,
    gateway: { known: true, agentTypes: ["hermes", "openclaw"], sharedModels: [{ name: "allowed-model" }], githubCopilot: false },
    agentIdentity: { allowed: false, required: false }, ...overrides };
}
function respond(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}
function renderForm() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(<QueryClientProvider client={client}><CreateForm /></QueryClientProvider>);
  return { ...result, client };
}
async function selectTeam(user: ReturnType<typeof userEvent.setup>, team = "restricted") {
  await user.selectOptions(screen.getByRole("combobox", { name: /Team/ }), team);
  await waitFor(() => expect(screen.queryByText("Loading allowed Agent options...")).not.toBeInTheDocument());
}

beforeEach(() => { mutation.mutate.mockClear(); mutation.error = null; });
afterEach(() => vi.unstubAllGlobals());

describe("Policy-controlled Agent creation", () => {
  it("shows only allowed Harness and models and sends explicit permitted sources", async () => {
    vi.stubGlobal("fetch", vi.fn(() => respond(options())));
    const user = userEvent.setup(); renderForm();
    expect(screen.getByRole("button", { name: "Create Agent" })).toBeDisabled();
    expect(screen.queryByRole("option", { name: "OpenClaw" })).not.toBeInTheDocument();
    await selectTeam(user);
    const harness = screen.getByRole("combobox", { name: "Agent Type" });
    expect(within(harness).getAllByRole("option").map(option => option.textContent)).toEqual(["Hermes"]);
    expect(harness).toBeDisabled();
    expect(screen.queryByRole("switch", { name: "Use GitHub Copilot" })).not.toBeInTheDocument();
    expect(screen.queryByRole("switch", { name: /Entra Agent Identity/ })).not.toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Use shared models" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Use shared models" })).toBeDisabled();
    const models = screen.getByRole("combobox", { name: /Default model/ });
    expect(within(models).getAllByRole("option").map(option => option.textContent)).toEqual(["(automatic)", "allowed-model"]);
    await user.selectOptions(models, "allowed-model");
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mutation.mutate).toHaveBeenCalledWith(expect.objectContaining({ team: "restricted", agent: { type: "hermes" }, model: "allowed-model", modelAccess: { sharedModels: true, githubCopilot: false } }));
  });

  it("clears invalid Harness and model choices when switching Teams", async () => {
    vi.stubGlobal("fetch", vi.fn((url: string) => Promise.resolve(respond(url.endsWith("research") ? options("research", {
      allowedAgentTypes: ["openclaw", "hermes"], gateway: { known: true, agentTypes: ["openclaw", "hermes"], sharedModels: [{ name: "research-model" }], githubCopilot: true },
    }) : options()))));
    const user = userEvent.setup(); renderForm(); await selectTeam(user, "research");
    await user.selectOptions(screen.getByRole("combobox", { name: "Agent Type" }), "openclaw");
    await user.selectOptions(screen.getByRole("combobox", { name: /Default model/ }), "research-model");
    await user.type(screen.getByLabelText("Display Name (optional)"), "My Agent");
    await selectTeam(user);
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Agent Type" })).toHaveValue("hermes"));
    expect(screen.getByRole("combobox", { name: /Default model/ })).toHaveValue("");
    expect(screen.getByLabelText("Display Name (optional)")).toHaveValue("My Agent");
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mutation.mutate).toHaveBeenCalledWith(expect.objectContaining({ agent: { type: "hermes" }, model: undefined, modelAccess: { sharedModels: true, githubCopilot: false } }));
  });

  it("preserves still-authorized choices after a Team's options reload", async () => {
    vi.stubGlobal("fetch", vi.fn().mockImplementation(() => Promise.resolve(respond(options()))));
    const user = userEvent.setup(); const { client } = renderForm(); await selectTeam(user);
    await user.selectOptions(screen.getByRole("combobox", { name: /Default model/ }), "allowed-model");
    await act(async () => { await client.invalidateQueries({ queryKey: ["instance-create-options"] }); });
    expect(screen.getByRole("combobox", { name: /Default model/ })).toHaveValue("allowed-model");
  });

  it("ignores an earlier Team response and blocks submission while loading", async () => {
    let completeOld!: (response: Response) => void;
    vi.stubGlobal("fetch", vi.fn((url: string) => url.endsWith("research") ? new Promise<Response>(resolve => { completeOld = resolve; }) : Promise.resolve(respond(options()))));
    const user = userEvent.setup(); renderForm();
    await user.selectOptions(screen.getByRole("combobox", { name: /Team/ }), "research");
    expect(screen.getByRole("button", { name: "Create Agent" })).toBeDisabled();
    fireEvent.submit(screen.getByRole("button", { name: "Create Agent" }).closest("form")!);
    expect(mutation.mutate).not.toHaveBeenCalled();
    await selectTeam(user);
    await act(async () => completeOld(respond(options("research", { allowedAgentTypes: ["openclaw"] }))));
    expect(screen.getByRole("combobox", { name: "Agent Type" })).toHaveValue("hermes");
    expect(screen.queryByRole("option", { name: "OpenClaw" })).not.toBeInTheDocument();
  });

  it("fails closed on lookup errors and retries without creating an Agent", async () => {
    let restrictedRequests = 0;
    const fetchMock = vi.fn((url: string) => {
      const team = new URL(url, "http://test").searchParams.get("team");
      return Promise.resolve(team === "restricted" && restrictedRequests++ === 0
        ? respond({ error: { message: "Policy is unavailable" } }, 503)
        : respond(options(team ?? "")));
    });
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup(); renderForm(); await selectTeam(user);
    expect(await screen.findByText("Policy is unavailable")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create Agent" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Retry options" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Create Agent" })).toBeEnabled());
    expect(mutation.mutate).not.toHaveBeenCalled();
  });

  it.each([
    ["empty Harness intersection", { allowedAgentTypes: [], blocked: { code: "agent_type_not_allowed", message: "No Agent type is allowed" } }],
    ["missing identity prerequisite", { agentIdentity: { allowed: false, required: true }, blocked: { code: "policy_identity_unavailable", message: "Required identity is unavailable" } }],
    ["no models", { gateway: { known: true, agentTypes: ["hermes"], sharedModels: [], githubCopilot: false } }],
  ] as [string, Partial<CreateOptions>][])("blocks %s", async (_, override) => {
    vi.stubGlobal("fetch", vi.fn(() => respond(options("restricted", override))));
    const user = userEvent.setup(); renderForm(); await selectTeam(user);
    expect(screen.getByRole("button", { name: "Create Agent" })).toBeDisabled();
    fireEvent.submit(screen.getByRole("button", { name: "Create Agent" }).closest("form")!);
    expect(mutation.mutate).not.toHaveBeenCalled();
  });

  it.each([false, true])("lets the user choose an optional Identity: %s", async (enabled) => {
    vi.stubGlobal("fetch", vi.fn(() => respond(options("restricted", { agentIdentity: { allowed: true, required: false } }))));
    const user = userEvent.setup(); renderForm(); await selectTeam(user);
    const identity = screen.getByRole("switch", { name: /Entra Agent Identity/ });
    expect(identity).not.toBeChecked();
    expect(identity).toBeEnabled();
    if (enabled) await user.click(identity);
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mutation.mutate).toHaveBeenCalledWith(expect.objectContaining({ agentIdentityEnabled: enabled || undefined }));
  });

  it("enables and locks a required Identity", async () => {
    vi.stubGlobal("fetch", vi.fn(() => respond(options("restricted", { agentIdentity: { allowed: true, required: true } }))));
    const user = userEvent.setup(); renderForm(); await selectTeam(user);
    const identity = screen.getByRole("switch", { name: /Entra Agent Identity/ });
    expect(identity).toBeChecked(); expect(identity).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mutation.mutate).toHaveBeenCalledWith(expect.objectContaining({ agentIdentityEnabled: true }));
  });

  it("does not offer Identity to Custom when it is otherwise allowed", async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(respond(options("restricted", {
      policyName: undefined,
      allowedAgentTypes: ["custom"],
      gateway: { known: true, agentTypes: ["custom"], sharedModels: [{ name: "allowed-model" }], githubCopilot: false },
      agentIdentity: { allowed: true, required: false },
    }))));
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup(); renderForm(); await selectTeam(user);
    await user.type(screen.getByLabelText(/Agent Image/), "registry.example.com/assistant:1");
    expect(screen.queryByRole("switch", { name: /Entra Agent Identity/ })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mutation.mutate).toHaveBeenCalledWith(expect.objectContaining({ agentIdentityEnabled: undefined, agent: expect.objectContaining({ type: "custom" }) }));
  });

  it("offers required Identity to explicitly qualified governed Custom", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(respond(options("restricted", {
      allowedAgentTypes: ["custom"],
      gateway: { known: true, agentTypes: ["custom"], sharedModels: [{ name: "allowed-model" }], githubCopilot: false },
      agentIdentity: { allowed: true, required: true, customAllowed: true },
    })))));
    const user = userEvent.setup(); renderForm(); await selectTeam(user);
    expect(screen.getByRole("combobox", { name: "Agent Type" })).toHaveValue("custom");
    await user.type(screen.getByLabelText(/Agent Image/), "registry.example.com/assistant:1");
    const identity = screen.getByRole("switch", { name: /Entra Agent Identity/ });
    expect(identity).toBeChecked();
    expect(identity).toBeDisabled();
    const create = screen.getByRole("button", { name: "Create Agent" });
    expect(create).toBeEnabled();
    await user.click(create);
    expect(mutation.mutate).toHaveBeenCalledTimes(1);
    expect(mutation.mutate).toHaveBeenCalledWith(expect.objectContaining({
      agentIdentityEnabled: true, agent: expect.objectContaining({ type: "custom" }),
      modelAccess: { sharedModels: true, githubCopilot: false },
    }));
  });

  it("shows the Team policy's per-Agent Gateway limits read-only", async () => {
    vi.stubGlobal("fetch", vi.fn(() => respond(options("restricted", { rateLimits: { rpm: 30, tpm: 100000 } }))));
    const user = userEvent.setup(); renderForm(); await selectTeam(user);
    const limits = screen.getByLabelText("AI Gateway limits");
    expect(within(limits).getByText("30 requests per minute")).toBeInTheDocument();
    expect(within(limits).getByText("100,000 tokens per minute")).toBeInTheDocument();
  });

  it("refreshes permissions after a create rejection without automatically retrying", async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(respond(options())));
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup(); const { client, rerender } = renderForm(); await selectTeam(user);
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    mutation.error = new Error("Policy changed");
    fetchMock.mockImplementation(() => Promise.resolve(respond(options("restricted", { allowedAgentTypes: ["openclaw"] }))));
    rerender(<QueryClientProvider client={client}><CreateForm /></QueryClientProvider>);
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Agent Type" })).toHaveValue("openclaw"));
    expect(mutation.mutate).toHaveBeenCalledTimes(1);
    expect(screen.getByText("Policy changed")).toBeInTheDocument();
  });
});
