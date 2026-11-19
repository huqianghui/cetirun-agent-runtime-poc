// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AIGatewayBlock } from "../AIGatewayBlock";
import type { Instance } from "../../types/instance";

const api = vi.hoisted(() => ({
  startCopilotLogin: vi.fn(),
  pollCopilotLogin: vi.fn(),
  cancelCopilotLogin: vi.fn(),
  getAIGatewayCatalog: vi.fn(),
}));
vi.mock("../../api/aiGateway", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/aiGateway")>()),
  ...api,
}));
vi.mock("../../hooks/useInstance", () => ({
  useUpdateInstanceByName: () => ({ mutate: vi.fn(), isPending: false, error: null }),
}));

function instance(aiGateway: NonNullable<Instance["aiGateway"]>): Instance {
  return {
    instanceName: "alice-gw", uid: "agent-uid", resourceVersion: "42", agent: { type: "openclaw" }, alias: "alice",
    displayName: "Alice", team: "platform", phase: "Ready", createdAt: "2026-10-01T00:00:00Z", upgradeAvailable: false, aiGateway,
  };
}

describe("AIGatewayBlock smart-routing models", () => {
  beforeEach(() => {
    Object.values(api).forEach((fn) => fn.mockReset());
    api.pollCopilotLogin.mockResolvedValue({ state: "none" });
  });

  it("lists a granted router by its name and explains one that is not available", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <AIGatewayBlock instance={instance({
          routed: true, phase: "Ready", sharedModels: true, githubCopilot: false,
          models: ["gpt-6", "auto"], defaultModel: "auto",
          modelRouters: [
            { name: "auto", granted: true },
            { name: "auto-copilot", granted: false, reason: "tier simple uses the GitHub Copilot model fast-model, and GitHub Copilot is not available to this Agent" },
          ],
        })} />
      </QueryClientProvider>,
    );
    expect(screen.getByLabelText("Agent models")).toHaveTextContent("auto (default)");
    expect(screen.getByLabelText("Unavailable smart-routing models")).toHaveTextContent(
      "Smart-routing model auto-copilot is not available to this agent: tier simple uses the GitHub Copilot model fast-model",
    );
    expect(screen.queryByText(/cetirun-router-/)).not.toBeInTheDocument();
  });
});
