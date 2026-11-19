// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CreateForm } from "../CreateForm";
import type { CreateOptions } from "../../hooks/useCreateOptions";

const mutation = vi.hoisted(() => ({ mutate: vi.fn(), isPending: false, error: null as Error | null }));
vi.mock("../../hooks/useInstance", () => ({ useCreateInstance: () => mutation }));
vi.mock("../../hooks/useFeatures", () => ({
  useFeatures: () => ({ data: { aiGatewayEnabled: true, agentIdentityEnabled: false, allowedAgentTypes: ["hermes", "openclaw"] }, isLoading: false }),
}));
vi.mock("../../hooks/useTeams", () => ({
  useTeams: () => ({ data: [{ name: "research" }], isLoading: false }),
}));

const options: CreateOptions = {
  team: "research", policyName: "research-policy", allowedAgentTypes: ["hermes"], aiGatewayEnabled: true,
  gateway: { known: true, agentTypes: ["hermes", "openclaw"], sharedModels: [{ name: "allowed-model" }], githubCopilot: false },
  agentIdentity: { allowed: false, required: false }, modelRouters: ["auto"],
};

beforeEach(() => { mutation.mutate.mockClear(); mutation.error = null; });
afterEach(() => vi.unstubAllGlobals());

it("offers the Team's smart-routing models as the default model", async () => {
  vi.stubGlobal("fetch", vi.fn(() => new Response(JSON.stringify(options), { status: 200, headers: { "Content-Type": "application/json" } })));
  const user = userEvent.setup();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><CreateForm /></QueryClientProvider>);
  await user.selectOptions(screen.getByRole("combobox", { name: /Team/ }), "research");
  await waitFor(() => expect(screen.queryByText("Loading allowed Agent options...")).not.toBeInTheDocument());
  const models = await screen.findByRole("combobox", { name: /Default model/ });
  expect(within(models).getAllByRole("option").map((option) => option.textContent)).toEqual(["(automatic)", "allowed-model", "auto (smart routing)"]);
  await user.selectOptions(models, "auto");
  expect(models).toHaveValue("auto");
  await user.click(screen.getByRole("button", { name: "Create Agent" }));
  expect(mutation.mutate).toHaveBeenCalledWith(expect.objectContaining({ team: "research", model: "auto" }));
});

it("mentions GitHub Copilot models only when the agent may use GitHub Copilot", async () => {
  for (const githubCopilot of [false, true]) {
    const offered = { ...options, gateway: { ...options.gateway, githubCopilot } };
    vi.stubGlobal("fetch", vi.fn(() => new Response(JSON.stringify(offered), { status: 200, headers: { "Content-Type": "application/json" } })));
    const user = userEvent.setup();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { unmount } = render(<QueryClientProvider client={client}><CreateForm /></QueryClientProvider>);
    await user.selectOptions(screen.getByRole("combobox", { name: /Team/ }), "research");
    await waitFor(() => expect(screen.queryByText("Loading allowed Agent options...")).not.toBeInTheDocument());
    await screen.findByRole("combobox", { name: /Default model/ });
    expect(screen.getByText(/Smart-routing models pick a model/)).toBeInTheDocument();
    expect(screen.queryAllByText(/GitHub Copilot models can be chosen after sign-in/).length > 0).toBe(githubCopilot);
    unmount();
  }
});
