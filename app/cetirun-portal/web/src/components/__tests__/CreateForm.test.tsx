// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CreateForm } from "../CreateForm";

const mockFeatures = vi.hoisted(() => ({
  data: { tailscaleEnabled: true, agentIdentityEnabled: false, allowedAgentTypes: ["openclaw", "hermes"] },
}));

vi.mock("../../hooks/useFeatures", () => ({
  useFeatures: () => ({ data: mockFeatures.data, isLoading: false }),
}));

vi.mock("../../hooks/useCreateOptions", () => ({
  useCreateOptions: (team: string) => ({
    data: { team, allowedAgentTypes: mockFeatures.data.allowedAgentTypes, aiGatewayEnabled: false,
      gateway: { known: false, sharedModels: [], githubCopilot: false, agentTypes: [] },
      agentIdentity: { allowed: mockFeatures.data.agentIdentityEnabled, required: false } },
    isFetching: false, refetch: vi.fn(),
  }),
}));

const mockMutate = vi.fn();
const mockCreateOptions = vi.hoisted(() => ({ onSettled: undefined as undefined | (() => void) }));
vi.mock("../../hooks/useInstance", () => ({
  useCreateInstance: (options: { onSettled?: () => void }) => {
    mockCreateOptions.onSettled = options.onSettled;
    return { mutate: mockMutate, isPending: false };
  },
}));

const mockTeams = vi.hoisted(() => ({
  data: [
    { name: "platform", role: "" },
    { name: "security", role: "" },
  ],
}));

vi.mock("../../hooks/useTeams", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../hooks/useTeams")>();
  return {
    ...actual,
    useTeams: () => ({ data: mockTeams.data, isLoading: false }),
  };
});

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe("CreateForm", () => {
  beforeEach(() => {
    mockMutate.mockClear();
    mockFeatures.data = {
      tailscaleEnabled: true,
      agentIdentityEnabled: false,
      allowedAgentTypes: ["openclaw", "hermes"],
    };
  });

  it("creates a generic headless Custom Agent without native settings", async () => {
    mockFeatures.data.allowedAgentTypes = ["hermes", "custom"];
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);
    await user.type(screen.getByLabelText("Gateway Token (optional)"), "sk-dummy");
    await user.selectOptions(screen.getByRole("combobox", { name: "Agent Type" }), "custom");
    expect(screen.getByText(/Image updates retain HOME and require a compatible image/)).toBeInTheDocument();
    expect(screen.queryByText(/image cannot change after creation/)).toBeNull();
    const image = "registry.example.com/team/assistant@sha256:" + "a".repeat(64);
    await user.type(screen.getByLabelText(/Agent Image/), image);
    expect(screen.getByLabelText(/Agent Username/)).toHaveValue("vibe");
    expect(screen.queryByLabelText("Gateway Token (optional)")).toBeNull();
    expect(screen.queryByLabelText("Tailscale Auth Key (optional)")).toBeNull();
    expect(screen.queryByLabelText("Model (optional)")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate).toHaveBeenCalledOnce();
    expect(JSON.parse(JSON.stringify(mockMutate.mock.calls[0][0]))).toEqual({ agent: { type: "custom", image, username: "vibe" } });
  });

  it("validates the Custom HTTP port before submitting", async () => {
    mockFeatures.data.allowedAgentTypes = ["custom"];
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);
    await user.type(await screen.findByLabelText(/Agent Image/), "registry.example.com/assistant:1");
    fireEvent.change(screen.getByLabelText("HTTP Port (optional)"), { target: { value: "65536" } });
    fireEvent.submit(screen.getByRole("button", { name: "Create Agent" }).closest("form")!);
    expect(mockMutate).not.toHaveBeenCalled();
    expect(screen.getByText(/HTTP port must be an integer/)).toBeInTheDocument();
  });

  it("does not offer Agent Identity for Custom", async () => {
    mockFeatures.data.allowedAgentTypes = ["custom"];
    mockFeatures.data.agentIdentityEnabled = true;
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);
    await user.type(await screen.findByLabelText(/Agent Image/), "registry.example.com/assistant:1");
    expect(screen.queryByLabelText("Enable Microsoft Entra Agent Identity")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    const input = JSON.parse(JSON.stringify(mockMutate.mock.calls[0][0]));
    expect(input.agent.type).toBe("custom");
    expect(input).not.toHaveProperty("agentIdentityEnabled");
    expect(input).not.toHaveProperty("gatewayToken");
  });

  it("creates a Web UI with the shared port and does not carry the flag to built-ins", async () => {
    mockFeatures.data.allowedAgentTypes = ["hermes", "custom"];
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);
    await user.selectOptions(screen.getByRole("combobox", { name: "Agent Type" }), "custom");
    await user.type(screen.getByLabelText(/Agent Image/), "registry.example.com/editor:1");
    await user.click(screen.getByRole("switch", { name: "Serve Web UI" }));
    fireEvent.submit(screen.getByRole("button", { name: "Create Agent" }).closest("form")!);
    expect(mockMutate).not.toHaveBeenCalled();
    expect(screen.getByText("A port is required when Web UI is enabled.")).toBeInTheDocument();
    await user.type(screen.getByRole("spinbutton", { name: "Web UI Port" }), "8080");
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate.mock.calls[0][0].agent).toEqual({ type: "custom", image: "registry.example.com/editor:1", username: "vibe", port: 8080, webUI: true });
    await user.selectOptions(screen.getByRole("combobox", { name: "Agent Type" }), "hermes");
    expect(screen.queryByRole("switch", { name: "Serve Web UI" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate.mock.calls[1][0].agent).not.toHaveProperty("webUI");
  });

  it.each(["hermes", "custom"])("creates a private %s image with a stable retry ID and clears credentials", async (type) => {
    mockFeatures.data.allowedAgentTypes = [type];
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);
    const image = "registry.example.com/team/assistant:1";
    await user.type(await screen.findByLabelText(/Agent Image/), image);
    await user.click(screen.getByLabelText("Use private registry credentials"));
    await user.type(screen.getByLabelText(/Registry username/), "synthetic-user");
    await user.type(screen.getByLabelText(/Registry password or token/), "synthetic-token");
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    const first = mockMutate.mock.calls[0][0];
    expect(first.agent).toMatchObject({ type, image });
    expect(first.registryAuth).toEqual({ username: "synthetic-user", password: "synthetic-token" });
    expect(first.requestId).toMatch(/^[a-zA-Z0-9_-]{16,128}$/);
    if (type === "custom") {
      expect(first.agent).toEqual({ type, image, username: "vibe" });
      expect(JSON.parse(JSON.stringify(first))).not.toHaveProperty("gatewayToken");
    }
    act(() => mockCreateOptions.onSettled?.());
    expect(screen.getByLabelText(/Registry username/)).toHaveValue("");
    expect(screen.getByLabelText(/Registry password or token/)).toHaveValue("");
    await user.type(screen.getByLabelText(/Registry username/), "synthetic-user");
    await user.type(screen.getByLabelText(/Registry password or token/), "synthetic-token");
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate.mock.calls[1][0].requestId).toBe(first.requestId);
    await user.click(screen.getByLabelText("Use private registry credentials"));
    await user.click(screen.getByLabelText("Use private registry credentials"));
    expect(screen.getByLabelText(/Registry password or token/)).toHaveValue("");
  });

  it("renders form fields", () => {
    renderWithQuery(<CreateForm />);

    expect(screen.getByText("Display Name (optional)")).toBeInTheDocument();
    expect(screen.getByText("Team (optional)")).toBeInTheDocument();
    expect(screen.getByText("Gateway Token (optional)")).toBeInTheDocument();
    expect(screen.getByText("Tailscale Auth Key (optional)")).toBeInTheDocument();
    expect(screen.getByText("Create Agent")).toBeInTheDocument();
  });

  it("defaults to Hermes even when OpenClaw is listed first", async () => {
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);

    await user.type(
      screen.getByPlaceholderText("e.g. my-project (leave blank for default)"),
      "my-instance",
    );
    await user.click(screen.getByText("Create Agent"));

    expect(mockMutate).toHaveBeenCalledWith({
      instanceName: "my-instance",
      agent: { type: "hermes" },
      displayName: undefined,
      team: undefined,
      gatewayToken: undefined,
      tailscaleAuthKey: undefined,
    });
  });

  it("submits with selected team", async () => {
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);

    await user.type(
      screen.getByPlaceholderText("e.g. my-project (leave blank for default)"),
      "my-instance",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /Team/ }),
      "platform",
    );
    await user.click(screen.getByText("Create Agent"));

    expect(mockMutate).toHaveBeenCalledWith({
      instanceName: "my-instance",
      agent: { type: "hermes" },
      displayName: undefined,
      team: "platform",
      gatewayToken: undefined,
      tailscaleAuthKey: undefined,
    });
  });

  it("submits with all optional fields filled in", async () => {
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);

    await user.type(
      screen.getByPlaceholderText("e.g. my-project (leave blank for default)"),
      "my-instance",
    );
    await user.type(
      screen.getByPlaceholderText("Defaults to your profile name"),
      "Custom Name",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /Team/ }),
      "security",
    );
    await user.type(
      screen.getByPlaceholderText("Leave empty to auto-generate"),
      "my-gw-token",
    );
    await user.type(
      screen.getByPlaceholderText("Leave empty to skip Tailscale"),
      "tskey-auth-xxx",
    );

    await user.click(screen.getByText("Create Agent"));

    expect(mockMutate).toHaveBeenCalledWith({
      instanceName: "my-instance",
      agent: { type: "hermes" },
      displayName: "Custom Name",
      team: "security",
      gatewayToken: "my-gw-token",
      tailscaleAuthKey: "tskey-auth-xxx",
    });
  });

  it("preserves an explicit OpenClaw selection", async () => {
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);

    await user.selectOptions(
      screen.getByRole("combobox", { name: /Agent Type/ }),
      "openclaw",
    );
    await user.click(screen.getByText("Create Agent"));

    expect(mockMutate).toHaveBeenCalledWith({
      instanceName: undefined,
      agent: { type: "openclaw" },
      displayName: undefined,
      team: undefined,
      gatewayToken: undefined,
      tailscaleAuthKey: undefined,
    });
  });

  it("uses the only allowed agent type", async () => {
    const user = userEvent.setup();
    mockFeatures.data = { tailscaleEnabled: true, allowedAgentTypes: ["openclaw"] };
    renderWithQuery(<CreateForm />);

    const agentTypeSelect = screen.getByRole("combobox", { name: /Agent Type/ });
    await waitFor(() => expect(agentTypeSelect).toHaveValue("openclaw"));
    expect(agentTypeSelect).toBeDisabled();
    await user.click(screen.getByText("Create Agent"));

    expect(mockMutate).toHaveBeenCalledWith({
      instanceName: undefined,
      agent: { type: "openclaw" },
      displayName: undefined,
      team: undefined,
      gatewayToken: undefined,
      tailscaleAuthKey: undefined,
    });
  });

  it("submit button is enabled even without team selected", () => {
    renderWithQuery(<CreateForm />);
    expect(screen.getByText("Create Agent")).not.toBeDisabled();
  });

  it("explains automatic Memory defaults without sending policy settings", async () => {
    const user = userEvent.setup();
    renderWithQuery(<CreateForm />);
    expect(screen.getByText(/memory and bounded learning are enabled automatically/)).toBeInTheDocument();
    expect(screen.queryByRole("switch", { name: /private memory/ })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create Agent" }));
    expect(mockMutate.mock.calls[0][0]).not.toHaveProperty("memory");
  });

  it("does not offer the retired direct Copilot model picker", () => {
    renderWithQuery(<CreateForm />);
    expect(screen.queryByText("GitHub Copilot model")).not.toBeInTheDocument();
  });
});
