// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Connection } from "../api/toolAuthorization";
import { ConnectedServicesPage } from "./ConnectedServicesPage";

const features = vi.hoisted(() => ({ data: { agentToolsEnabled: true } as { agentToolsEnabled: boolean } | undefined, isLoading: false }));
const api = vi.hoisted(() => ({ listConnections: vi.fn(), getConnectionUsage: vi.fn(), deleteConnection: vi.fn(), reconnectConnection: vi.fn(), getConnectionAuthorization: vi.fn() }));

vi.mock("../hooks/useFeatures", () => ({ useFeatures: () => features }));
vi.mock("../api/toolAuthorization", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/toolAuthorization")>()),
  ...api,
}));

const personalGitHub: Connection = {
  id: "github-personal",
  provider: "github",
  accountDisplay: "octocat",
  services: ["github"],
  scopes: ["repo"],
  revision: 1,
  status: "connected",
  createdAt: "2026-10-26T00:00:00Z",
  mode: "delegated",
};

const personalGoogle: Connection = {
  ...personalGitHub,
  id: "google-personal",
  provider: "google",
  accountDisplay: "person@example.com",
  services: ["google_calendar", "google_gmail"],
  scopes: ["openid", "email", "https://www.googleapis.com/auth/calendar"],
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><ConnectedServicesPage /></MemoryRouter></QueryClientProvider>);
}

describe("ConnectedServicesPage", () => {
  beforeEach(() => {
    vi.spyOn(document.body, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 1024, 768));
    vi.spyOn(HTMLElement.prototype, "offsetParent", "get").mockImplementation(function (this: HTMLElement) { return this.parentElement; });
    features.data = { agentToolsEnabled: true };
    features.isLoading = false;
    api.listConnections.mockReset().mockResolvedValue([personalGitHub]);
    api.getConnectionUsage.mockReset().mockResolvedValue({ connectionId: personalGitHub.id, agents: [], canRemove: true });
    api.deleteConnection.mockReset().mockResolvedValue(undefined);
    api.reconnectConnection.mockReset();
    api.getConnectionAuthorization.mockReset();
  });
  afterEach(() => vi.restoreAllMocks());

  it("fails closed when agent identities are disabled", () => {
    features.data = { agentToolsEnabled: false };
    renderPage();
    expect(screen.getByText("403 Forbidden")).toBeInTheDocument();
    expect(api.listConnections).not.toHaveBeenCalled();
  });

  it("groups personal and GitHub App access while keeping legacy aliases visible", async () => {
    const appConnection: Connection = {
      ...personalGitHub,
      id: "github-app",
      mode: "installation",
      accountDisplay: "cetirun[bot]",
      github: { installationId: 7, accountLogin: "octo-org", appSlug: "cetirun", userLogin: "octocat" },
    };
    const legacyAlias: Connection = { ...personalGitHub, id: "legacy-github", canonicalConnectionId: personalGitHub.id, accountDisplay: "legacy-octocat" };
    api.listConnections.mockResolvedValue([personalGitHub, appConnection, legacyAlias]);
    api.getConnectionUsage.mockImplementation((id: string) => Promise.resolve({ connectionId: id, agents: id === appConnection.id ? [{ instanceUid: "agent-uid", instanceName: "agent-a", instanceNamespace: "default" }] : [], canRemove: id !== appConnection.id }));
    renderPage();

    expect(await screen.findByRole("heading", { name: "GitHub" })).toBeInTheDocument();
    expect(screen.getByText("Selected repositories")).toBeInTheDocument();
    expect(screen.getByText("GitHub actions run as cetirun[bot]")).toBeInTheDocument();
    expect(screen.getByText("legacy-octocat")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Historical duplicate connections" })).toBeInTheDocument();
    expect(screen.getByText("This older personal account connection is retained for existing Agent access and cleanup. New Agent access uses the current connection.")).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "agent-a" })).toHaveAttribute("href", "/instances/agent-a/overview#service-access");
  });

  it("removes only an unused connection after confirmation and preserves provider permissions", async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText("octocat");
    await user.click(screen.getByRole("button", { name: "Remove connection" }));
    expect(screen.getByText("Removing this connection does not remove the provider grant or its permissions.")).toBeInTheDocument();
    expect(api.deleteConnection).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Remove connection permanently" }));
    await waitFor(() => expect(api.deleteConnection).toHaveBeenCalledWith(personalGitHub.id));
  });

  it.each([
    ["connected personal GitHub", personalGitHub],
    ["historical duplicate GitHub", { ...personalGitHub, id: "legacy-github", canonicalConnectionId: personalGitHub.id }],
    ["sign-in required GitHub", { ...personalGitHub, status: "interaction_required" as const }],
    ["connected Google with partial consent", personalGoogle],
    ["historical duplicate Google", { ...personalGoogle, id: "legacy-google", canonicalConnectionId: personalGoogle.id }],
    ["sign-in required Google", { ...personalGoogle, status: "interaction_required" as const }],
  ])("reauthorizes a %s connection without removing Agent access", async (_, connection) => {
    const user = userEvent.setup();
    const authorizationUrl = connection.provider === "google"
      ? "https://accounts.google.com/o/oauth2/v2/auth"
      : "https://github.com/login/oauth/authorize";
    const popup = { closed: false, close: vi.fn(), location: { replace: vi.fn() } };
    vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    api.listConnections.mockResolvedValue([connection]);
    api.getConnectionUsage.mockResolvedValue({ connectionId: connection.id, agents: [{ instanceUid: "agent-uid", instanceName: "agent-a", instanceNamespace: "default" }], canRemove: false });
    api.reconnectConnection.mockResolvedValue({ authorizationId: "reauthorization", authorizationUrl, expiresAt: new Date(Date.now() + 60_000).toISOString() });
    api.getConnectionAuthorization.mockResolvedValue({ authorizationId: "reauthorization", status: "complete", connectionId: connection.id });
    renderPage();

    const signin = await screen.findByRole("button", { name: "Sign in again" });
    expect(await screen.findByRole("link", { name: "agent-a" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove connection" })).toBeDisabled();
    await user.click(signin);
    await waitFor(() => expect(popup.location.replace).toHaveBeenCalledWith(authorizationUrl));
    expect(api.reconnectConnection).toHaveBeenCalledExactlyOnceWith(connection.id);
    expect(signin).toBeDisabled();

    api.listConnections.mockResolvedValue([{ ...connection, status: "connected", revision: 2 }]);
    act(() => window.dispatchEvent(new MessageEvent("message", { origin: window.location.origin, source: popup as unknown as Window, data: { type: "cetirun-connection", authorizationId: "reauthorization" } })));
    await waitFor(() => expect(popup.close).toHaveBeenCalled());
    expect(api.getConnectionAuthorization).toHaveBeenCalledWith("reauthorization", expect.any(AbortSignal));
    expect(api.listConnections).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("link", { name: "agent-a" })).toBeInTheDocument();
    expect(api.deleteConnection).not.toHaveBeenCalled();
  });

  it("blocks removal when usage cannot be checked", async () => {
    api.getConnectionUsage.mockRejectedValue(new Error("unavailable"));
    renderPage();
    await screen.findByText("Could not check which Agents use this connection.");
    expect(screen.getByRole("button", { name: "Remove connection" })).toBeDisabled();
  });

  it("announces the removal dialog, focuses Cancel, and restores keyboard focus", async () => {
    const user = userEvent.setup();
    renderPage();
    const trigger = await screen.findByRole("button", { name: "Remove connection" });
    await waitFor(() => expect(trigger).toBeEnabled());
    trigger.focus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("alertdialog", { name: "Remove octocat from CetiRun?" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(trigger).toHaveFocus());
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(api.deleteConnection).not.toHaveBeenCalled();
  });
});
