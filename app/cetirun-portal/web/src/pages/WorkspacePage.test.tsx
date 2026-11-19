// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { render, screen } from "@testing-library/react";
import { afterEach, describe, it, expect, vi } from "vitest";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { WorkspacePage } from "./WorkspacePage";
import type { Instance } from "../types/instance";

const mockInstance: Instance = {
  uid: "test-agent-uid",
  resourceVersion: "1",
  instanceName: "testuser",
  agent: { type: "openclaw" },
  displayName: "Test User",
  alias: "testuser",
  team: "aks",
  phase: "Ready",
  accessUrl: "https://example.com/ui/testuser/",
  gatewayToken: "tok-abc",
  upgradeAvailable: false,
  createdAt: "2026-01-01T00:00:00Z",
};

let mockInstanceError: Error | null = null;
let mockViewerError: Error | null = null;
let mockHasInstance = true;
let mockChatError: string | undefined;
vi.mock("../context/MeContext", () => ({
  useMe: () => ({ me: { alias: "testuser", email: "testuser@example.com" }, isLoading: false, error: mockViewerError }),
}));
vi.mock("../chat/ChatAccount", () => ({
  useChatAccount: () => ({ error: mockChatError }),
}));
vi.mock("../hooks/useInstance", () => ({
  useInstanceByNameQuery: () => ({ data: mockHasInstance ? mockInstance : undefined, isLoading: false, isError: !!mockInstanceError, error: mockInstanceError }),
  useDeleteInstanceByName: () => ({
    mutate: vi.fn(),
    isPending: false,
    isError: false,
  }),
  useUpdateInstanceByName: () => ({
    mutate: vi.fn(),
    isPending: false,
    isError: false,
    reset: vi.fn(),
  }),
  useUpgradeInstanceByName: () => ({
    mutate: vi.fn(),
    isPending: false,
    isError: false,
  }),
  useInstanceStatusByNameQuery: () => ({ data: undefined, isLoading: false }),
}));

const mockFeatures = {
  data: { tailscaleEnabled: false, customChatEnabled: false },
  isLoading: false,
};
vi.mock("../hooks/useFeatures", () => ({ useFeatures: () => mockFeatures }));

function renderAt(path: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route
            path="/instances/:instanceName/:section"
            element={<WorkspacePage />}
          />
          <Route path="/instances/:instanceName" element={<WorkspacePage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("WorkspacePage", () => {
  afterEach(() => {
    mockInstanceError = null;
    mockViewerError = null;
    mockHasInstance = true;
    mockChatError = undefined;
  });
  it("shows a session expiry from a background Agent refetch", () => {
    mockInstanceError = new Error("Session expired, please refresh the page");
    renderAt("/instances/testuser/overview");
    expect(screen.getByRole("alert")).toHaveTextContent(mockInstanceError.message);
    expect(screen.queryByText("Agent not found.")).toBeNull();
    expect(screen.getByText("Agent UI").closest("a")).not.toHaveAttribute("href");
  });
  it("shows the viewer error instead of claiming the Agent is missing", () => {
    mockHasInstance = false;
    mockViewerError = new Error("Session expired, please refresh the page");
    renderAt("/instances/testuser/overview");
    expect(screen.getByRole("alert")).toHaveTextContent(mockViewerError.message);
    expect(screen.queryByText("Agent not found.")).toBeNull();
  });
  it("keeps the expiry message when Chat identity verification clears the cache", () => {
    mockHasInstance = false;
    mockChatError = "Session expired, please refresh the page";
    renderAt("/instances/testuser/overview");
    expect(screen.getByRole("alert")).toHaveTextContent(mockChatError);
    expect(screen.queryByText("Agent not found.")).toBeNull();
  });
  it("preserves non-authentication errors", () => {
    mockInstanceError = new Error("Agent service is unavailable");
    renderAt("/instances/testuser/overview");
    expect(screen.getByRole("alert")).toHaveTextContent(mockInstanceError.message);
  });
  it("offers Portal Chat only for an enabled Custom HTTP Agent without Web UI", () => {
    const previous = mockInstance.agent;
    mockInstance.agent = { type: "custom", port: 8080, webUI: false };
    mockFeatures.data.customChatEnabled = true;
    try {
      renderAt("/instances/testuser/overview");
      expect(
        screen.getByRole("button", { name: "Chat", exact: true }),
      ).toBeInTheDocument();
    } finally {
      mockInstance.agent = previous;
      mockFeatures.data.customChatEnabled = false;
    }
  });
  it.each([
    { type: "hermes" as const },
    { type: "openclaw" as const },
    { type: "custom" as const },
    { type: "custom" as const, port: 8080, webUI: true },
  ])("does not replace the existing UI for $type / $webUI / $port", (agent) => {
    const previous = mockInstance.agent;
    mockInstance.agent = agent;
    mockFeatures.data.customChatEnabled = true;
    try {
      renderAt("/instances/testuser/overview");
      expect(
        screen.queryByRole("button", { name: "Chat", exact: true }),
      ).toBeNull();
    } finally {
      mockInstance.agent = previous;
      mockFeatures.data.customChatEnabled = false;
    }
  });
  it("waits for feature discovery before redirecting a direct Chat link", () => {
    const previous = mockInstance.agent;
    mockInstance.agent = { type: "custom", port: 8080 };
    mockFeatures.isLoading = true;
    try {
      renderAt("/instances/testuser/chat");
      expect(
        screen.getByText("Checking Chat availability…"),
      ).toBeInTheDocument();
      expect(screen.queryByRole("heading", { name: "Test User" })).toBeNull();
    } finally {
      mockInstance.agent = previous;
      mockFeatures.isLoading = false;
    }
  });
  it("shows the display name in the sidebar and heading", () => {
    renderAt("/instances/testuser/overview");
    expect(screen.getAllByText("Test User")).toHaveLength(2);
  });

  it("shows all nav items", () => {
    renderAt("/instances/testuser/overview");
    expect(
      screen.getByRole("navigation", { name: "Agent workspace" }),
    ).toBeInTheDocument();
    expect(screen.getAllByText("Overview").length).toBeGreaterThan(0);
    expect(screen.getByText("Agent UI")).toBeInTheDocument();
    expect(screen.getByText("Terminal")).toBeInTheDocument();
    expect(screen.getByText("Desktop")).toBeInTheDocument();
  });

  it("renders overview section by default", () => {
    renderAt("/instances/testuser/overview");
    expect(
      screen.getByRole("heading", { name: "Test User" }),
    ).toBeInTheDocument();
  });

  it("omits the Registry credentials section for a public image", () => {
    renderAt("/instances/testuser/overview");
    expect(
      screen.queryByRole("region", { name: "Registry credentials" }),
    ).toBeNull();
    expect(screen.queryByText("Not configured")).toBeNull();
  });

  it("keeps Registry credential recovery controls for an enabled private image", () => {
    const previous = mockInstance.agent;
    mockInstance.agent = {
      ...previous,
      image: "registry.example.com/agent:1",
      registrySecretRef: { name: "registry-source-example" },
    };
    try {
      renderAt("/instances/testuser/overview");
      expect(
        screen.getByRole("region", { name: "Registry credentials" }),
      ).toBeInTheDocument();
      expect(
        screen.getByRole("button", { name: "Replace registry credentials" }),
      ).toBeInTheDocument();
      expect(
        screen.getByRole("button", { name: "Remove registry credentials" }),
      ).toBeInTheDocument();
      expect(screen.queryByText(/Credential sync/i)).toBeNull();
    } finally {
      mockInstance.agent = previous;
    }
  });

  it("redirects /instances/:instanceName to /instances/:instanceName/overview", () => {
    renderAt("/instances/testuser");
    expect(
      screen.getByRole("heading", { name: "Test User" }),
    ).toBeInTheDocument();
  });

  it("redirects unknown section to overview", () => {
    renderAt("/instances/testuser/unknown-section");
    expect(
      screen.getByRole("heading", { name: "Test User" }),
    ).toBeInTheDocument();
  });

  it("links to standalone instance services when the instance is ready", () => {
    renderAt("/instances/testuser/overview");
    expect(screen.getByRole("link", { name: "Agent UI" })).toHaveAttribute(
      "href",
      mockInstance.accessUrl,
    );
    expect(screen.getByRole("link", { name: "Terminal" })).toHaveAttribute(
      "href",
      "https://example.com/testuser/term",
    );
    expect(screen.getByRole("link", { name: "Desktop" })).toHaveAttribute(
      "href",
      "https://example.com/testuser/desktop",
    );
  });

  it("links Hermes instances to their native Webapp", () => {
    mockInstance.agent.type = "hermes";
    renderAt("/instances/testuser/overview");
    expect(screen.getByRole("link", { name: "Agent UI" })).toHaveAttribute(
      "href",
      "https://example.com/ui/testuser/",
    );
    mockInstance.agent.type = "openclaw";
  });

  it("keeps Custom desktop access without inventing a native UI or terminal", () => {
    const previousConditions = mockInstance.conditions;
    mockInstance.agent.type = "custom";
    mockInstance.accessUrl = "https://gateway.example.com/testuser/desktop/";
    mockInstance.conditions = [
      {
        type: "IngressReady",
        status: "True",
        reason: "PlatformDesktopConfigured",
        message: "",
      },
    ];
    try {
      renderAt("/instances/testuser/overview");
      expect(screen.getByRole("link", { name: "Desktop" })).toHaveAttribute(
        "href",
        "https://gateway.example.com/testuser/desktop",
      );
      expect(screen.getByText("Agent UI").closest("a")).not.toHaveAttribute(
        "href",
      );
      expect(screen.getByText("Terminal").closest("a")).not.toHaveAttribute(
        "href",
      );
    } finally {
      mockInstance.agent.type = "openclaw";
      mockInstance.accessUrl = "https://example.com/ui/testuser/";
      mockInstance.conditions = previousConditions;
    }
  });

  it("disables standalone instance services until the instance is ready", () => {
    mockInstance.phase = "Provisioning";
    renderAt("/instances/testuser/overview");
    expect(screen.getByText("Agent UI")).toHaveAttribute(
      "aria-disabled",
      "true",
    );
    expect(screen.getByText("Terminal")).toHaveAttribute(
      "aria-disabled",
      "true",
    );
    expect(screen.getByText("Desktop")).toHaveAttribute(
      "aria-disabled",
      "true",
    );
    mockInstance.phase = "Ready";
  });

  it("enables the owned Custom terminal without enabling native UI or desktop", () => {
    const previous = { ...mockInstance, agent: { ...mockInstance.agent } };
    mockInstance.agent.type = "custom";
    mockInstance.accessUrl = "https://gateway.example.com/testuser/term/";
    mockInstance.conditions = [
      { type: "IngressReady", status: "True", reason: "", message: "" },
      { type: "TerminalConfigured", status: "True", reason: "", message: "" },
      { type: "DesktopConfigured", status: "False", reason: "", message: "" },
    ];
    try {
      renderAt("/instances/testuser/overview");
      expect(screen.getByRole("link", { name: "Terminal" })).toHaveAttribute(
        "href",
        "https://gateway.example.com/testuser/term",
      );
      expect(screen.getByText("Agent UI").closest("a")).not.toHaveAttribute(
        "href",
      );
      expect(screen.getByText("Desktop").closest("a")).not.toHaveAttribute(
        "href",
      );
    } finally {
      Object.assign(mockInstance, previous);
      mockInstance.conditions = previous.conditions;
    }
  });

  it("opens a Custom Web UI in a new tab without inventing a desktop", () => {
    const previous = { ...mockInstance };
    Object.assign(mockInstance, {
      agent: { type: "custom", port: 8080, webUI: true },
      accessUrl: undefined,
      webUIUrl: "https://example.com/ui/testuser/",
      conditions: [
        {
          type: "IngressReady",
          status: "True",
          reason: "PlatformWebUIConfigured",
          message: "",
        },
      ],
    });
    try {
      renderAt("/instances/testuser/overview");
      const link = screen.getByRole("link", { name: "Web UI" });
      expect(link).toHaveAttribute("href", "https://example.com/ui/testuser/");
      expect(link).toHaveAttribute("target", "_blank");
      expect(link).toHaveAttribute("rel", "noopener noreferrer");
      expect(screen.getByText("Desktop").closest("a")).not.toHaveAttribute(
        "href",
      );
      expect(screen.getByText("Terminal").closest("a")).not.toHaveAttribute(
        "href",
      );
    } finally {
      delete mockInstance.webUIUrl;
      Object.assign(mockInstance, previous);
    }
  });
});
