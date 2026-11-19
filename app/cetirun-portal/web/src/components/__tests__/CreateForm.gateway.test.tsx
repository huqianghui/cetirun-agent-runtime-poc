// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CreateForm } from "../CreateForm";

const mockFeatures = vi.hoisted(() => ({
  data: { tailscaleEnabled: false, aiGatewayEnabled: true, allowedAgentTypes: ["openclaw", "hermes"] },
}));
vi.mock("../../hooks/useFeatures", () => ({
  useFeatures: () => ({ data: mockFeatures.data, isLoading: false }),
}));

const mockMutate = vi.fn();
vi.mock("../../hooks/useInstance", () => ({
  useCreateInstance: () => ({ mutate: mockMutate, isPending: false }),
}));

vi.mock("../../hooks/useTeams", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../hooks/useTeams")>();
  return {
    ...actual,
    useTeams: () => ({ data: [{ name: "platform", role: "" }], isLoading: false }),
  };
});

const mockCatalog = vi.hoisted(() => ({
  data: {
    known: true,
    sharedModels: [{ name: "gpt-6" }, { name: "gpt-6-mini" }],
    githubCopilot: true,
    agentTypes: ["openclaw", "hermes", "custom"],
  } as {
    known: boolean;
    sharedModels: { name: string }[];
    githubCopilot: boolean;
    agentTypes: string[] | null;
  },
}));
vi.mock("../../hooks/useCreateOptions", () => ({
  useCreateOptions: (team: string, enabled: boolean) => ({
    data: enabled ? { team, allowedAgentTypes: mockFeatures.data.allowedAgentTypes, aiGatewayEnabled: true,
      gateway: mockCatalog.data, agentIdentity: { allowed: false, required: false } } : undefined,
    isFetching: false, refetch: vi.fn(),
  }),
}));

function renderForm() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <CreateForm />
    </QueryClientProvider>,
  );
}

async function chooseOpenClawAndTeam(user: ReturnType<typeof userEvent.setup>) {
  await user.selectOptions(screen.getByRole("combobox", { name: /Team/ }), "platform");
  await user.selectOptions(screen.getByRole("combobox", { name: /Agent Type/ }), "openclaw");
}

describe("CreateForm with the AI Gateway", () => {
  beforeEach(() => {
    mockMutate.mockClear();
    mockCatalog.data = {
      known: true,
      sharedModels: [{ name: "gpt-6" }, { name: "gpt-6-mini" }],
      githubCopilot: true,
      agentTypes: ["openclaw", "hermes", "custom"],
    };
  });

  it("allows an unbound agent when Gateway model sources are available", async () => {
    renderForm();
    expect(screen.getByText("Team (optional)")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create Agent" })).toBeEnabled();
  });

  it("sends both model sources on by default for OpenClaw", async () => {
    const user = userEvent.setup();
    renderForm();
    await chooseOpenClawAndTeam(user);
    expect(screen.getByRole("switch", { name: "Use shared models" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Use GitHub Copilot" })).toBeChecked();
    expect(screen.getByText(/gpt-6, gpt-6-mini/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate).toHaveBeenCalledWith(
      expect.objectContaining({
        agent: { type: "openclaw" },
        team: "platform",
        model: undefined,
        modelAccess: { sharedModels: true, githubCopilot: true },
      }),
    );
  });

  it("keeps GitHub Copilot on when shared models are turned off", async () => {
    const user = userEvent.setup();
    renderForm();
    await chooseOpenClawAndTeam(user);
    await user.click(screen.getByRole("switch", { name: "Use GitHub Copilot" }));
    expect(screen.getByRole("switch", { name: "Use GitHub Copilot" })).not.toBeChecked();
    await user.click(screen.getByRole("switch", { name: "Use shared models" }));
    const copilot = screen.getByRole("switch", { name: "Use GitHub Copilot" });
    expect(copilot).toBeChecked();
    expect(copilot).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate).toHaveBeenCalledWith(
      expect.objectContaining({ modelAccess: { sharedModels: false, githubCopilot: true } }),
    );
  });

  it("requires GitHub Copilot when no shared model is available", async () => {
    mockCatalog.data = { agentTypes: ["hermes", "openclaw"], known: true, sharedModels: [], githubCopilot: true };
    const user = userEvent.setup();
    renderForm();
    await chooseOpenClawAndTeam(user);
    const copilot = screen.getByRole("switch", { name: "Use GitHub Copilot" });
    expect(copilot).toBeChecked();
    expect(copilot).toBeDisabled();
    expect(screen.queryByRole("switch", { name: "Use shared models" })).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: /Default model/ })).not.toBeInTheDocument();
  });

  it("keeps shared models on when the deployment offers no GitHub Copilot", async () => {
    mockCatalog.data = { agentTypes: ["hermes", "openclaw"], known: true, sharedModels: [{ name: "gpt-6" }], githubCopilot: false };
    const user = userEvent.setup();
    renderForm();
    await chooseOpenClawAndTeam(user);
    expect(screen.queryByRole("switch", { name: "Use GitHub Copilot" })).not.toBeInTheDocument();
    const shared = screen.getByRole("switch", { name: "Use shared models" });
    expect(shared).toBeChecked();
    expect(shared).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate).toHaveBeenCalledWith(
      expect.objectContaining({ modelAccess: { sharedModels: true, githubCopilot: false } }),
    );
  });

  it("refuses an OpenClaw agent when no model source is available", async () => {
    mockCatalog.data = { agentTypes: ["hermes", "openclaw"], known: true, sharedModels: [], githubCopilot: false };
    const user = userEvent.setup();
    renderForm();
    await chooseOpenClawAndTeam(user);
    expect(screen.getByText(/No models are available for new agents/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create Agent" })).toBeDisabled();
  });

  it("waits for the published model sources before creating an OpenClaw agent", async () => {
    mockCatalog.data = { agentTypes: ["hermes", "openclaw"], known: false, sharedModels: [], githubCopilot: false };
    const user = userEvent.setup();
    renderForm();
    await chooseOpenClawAndTeam(user);
    expect(screen.getByText(/has not published its models yet/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create Agent" })).toBeDisabled();
  });

  it("sends Copilot off and a default shared model", async () => {
    const user = userEvent.setup();
    renderForm();
    await chooseOpenClawAndTeam(user);
    await user.click(screen.getByRole("switch", { name: "Use GitHub Copilot" }));
    await user.selectOptions(screen.getByRole("combobox", { name: /Default model/ }), "gpt-6-mini");
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate).toHaveBeenCalledWith(
      expect.objectContaining({ model: "gpt-6-mini", modelAccess: { sharedModels: true, githubCopilot: false } }),
    );
  });

  it("offers Hermes the same model sources as OpenClaw", async () => {
    const user = userEvent.setup();
    renderForm();
    await user.selectOptions(screen.getByRole("combobox", { name: /Team/ }), "platform");
    await user.selectOptions(screen.getByRole("combobox", { name: /Agent Type/ }), "hermes");
    expect(screen.getByRole("switch", { name: "Use shared models" })).toBeChecked();
    await user.click(screen.getByRole("switch", { name: "Use GitHub Copilot" }));
    await user.selectOptions(screen.getByRole("combobox", { name: /Default model/ }), "gpt-6-mini");
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate).toHaveBeenCalledWith(
      expect.objectContaining({
        agent: { type: "hermes" },
        model: "gpt-6-mini",
        modelAccess: { sharedModels: true, githubCopilot: false },
      }),
    );
  });

  it("refuses Hermes while the deployment's agent chart routes only OpenClaw", async () => {
    mockCatalog.data = { ...mockCatalog.data, agentTypes: ["openclaw"] };
    const user = userEvent.setup();
    renderForm();
    await user.selectOptions(screen.getByRole("combobox", { name: /Team/ }), "platform");
    await user.selectOptions(screen.getByRole("combobox", { name: /Agent Type/ }), "hermes");
    expect(screen.getByText(/does not route Hermes agents through the AI Gateway/)).toBeInTheDocument();
    expect(screen.getByText(/upgrade the agent chart/)).toBeInTheDocument();
    expect(screen.queryByRole("switch", { name: "Use shared models" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create Agent" })).toBeDisabled();
    await user.selectOptions(screen.getByRole("combobox", { name: /Agent Type/ }), "openclaw");
    expect(screen.queryByText(/does not route/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create Agent" })).toBeEnabled();
  });

  it("waits until the operator has read its agent chart", async () => {
    mockCatalog.data = { ...mockCatalog.data, agentTypes: null };
    const user = userEvent.setup();
    renderForm();
    await user.selectOptions(screen.getByRole("combobox", { name: /Team/ }), "platform");
    for (const type of ["hermes", "openclaw"]) {
      await user.selectOptions(screen.getByRole("combobox", { name: /Agent Type/ }), type);
      await user.selectOptions(screen.getByRole("combobox", { name: /Team/ }), "platform");
      expect(screen.getByText(/has not read its agent chart yet/)).toBeInTheDocument();
      expect(screen.queryByText(/does not route/)).not.toBeInTheDocument();
      expect(screen.queryByRole("switch", { name: "Use shared models" })).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Create Agent" })).toBeDisabled();
    }
  });

  it("keeps Gateway sources but omits the native default model when switching to Custom", async () => {
    mockFeatures.data = { ...mockFeatures.data, allowedAgentTypes: ["openclaw", "hermes", "custom"] };
    try {
      const user = userEvent.setup();
      renderForm();
      await user.selectOptions(screen.getByRole("combobox", { name: /Default model/ }), "gpt-6-mini");
      await user.selectOptions(screen.getByRole("combobox", { name: /Agent Type/ }), "custom");
      await user.type(screen.getByRole("textbox", { name: /Agent Image/ }), "registry.example.com/team/agent:1.0");
      expect(screen.queryByRole("combobox", { name: /Default model/ })).not.toBeInTheDocument();
      expect(screen.getByRole("switch", { name: "Use shared models" })).toBeChecked();
      await user.click(screen.getByRole("switch", { name: "Use GitHub Copilot" }));
      await user.click(screen.getByRole("button", { name: "Create Agent" }));
      expect(mockMutate).toHaveBeenCalledWith(
        expect.objectContaining({
          agent: { type: "custom", image: "registry.example.com/team/agent:1.0", username: "vibe" },
          team: undefined,
          model: undefined,
          modelAccess: { sharedModels: true, githubCopilot: false },
        }),
      );
    } finally {
      mockFeatures.data = { ...mockFeatures.data, allowedAgentTypes: ["openclaw", "hermes"] };
    }
  });
});
