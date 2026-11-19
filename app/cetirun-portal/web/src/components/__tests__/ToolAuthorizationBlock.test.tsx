// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Connection, ConnectionBinding } from "../../api/toolAuthorization";
import type { Instance } from "../../types/instance";
import { ToolAuthorizationBlock } from "../ToolAuthorizationBlock";

const api = vi.hoisted(() => ({ listConnections: vi.fn(), listConnectionBindings: vi.fn(), startConnection: vi.fn(), reconnectConnection: vi.fn(), deleteConnection: vi.fn(), getConnectionUsage: vi.fn(), createToolIdentity: vi.fn(), createConnectionBinding: vi.fn(), updateConnectionBinding: vi.fn(), deleteConnectionBinding: vi.fn(), getGitHubAppConfig: vi.fn(), listGitHubInstallations: vi.fn(), listGitHubRepositories: vi.fn(), selectGitHubInstallation: vi.fn() }));
vi.mock("../../api/toolAuthorization", async (original) => ({ ...(await original<typeof import("../../api/toolAuthorization")>()), ...api }));
const instance: Instance = { instanceName: "agent-a", uid: "agent-uid", resourceVersion: "1", agent: { type: "hermes" }, alias: "owner", displayName: "Agent A", team: "platform", phase: "Ready", createdAt: "2026-10-10T00:00:00Z", upgradeAvailable: false };
const personal: Connection = { id: "personal-github", provider: "github", accountDisplay: "octocat", services: ["github"], scopes: ["repo"], mode: "delegated", revision: 3, status: "connected", createdAt: "2026-10-10T00:00:00Z" };
const app: Connection = { ...personal, id: "app-github", mode: "installation", accountDisplay: "example[bot]", github: { installationId: 7, accountLogin: "example-org", appSlug: "example-app", userLogin: "octocat" } };
const azure: Connection = { ...personal, id: "azure", mode: "agent", provider: "azure", accountDisplay: "Agent A", services: ["azure_arm"], scopes: [], identity: { instanceUid: instance.uid, tenantId: "tenant", principalId: "principal", clientId: "client" } };
const google: Connection = { ...personal, id: "google", provider: "google", accountDisplay: "user@example.com", services: ["google_gmail", "google_calendar"] };
const binding: ConnectionBinding = { id: "binding-1", instanceUid: instance.uid, connectionId: personal.id, services: ["github"], enabled: true, revision: 7 };
const appBinding: ConnectionBinding = { ...binding, connectionId: app.id, github: { repositoryIds: [1], permissions: { contents: "read" } } };
const empty = "No identities configured yet. Add an identity to choose how this agent accesses external services.";
type User = ReturnType<typeof userEvent.setup>;
function renderUI() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const view = render(<QueryClientProvider client={client}><ToolAuthorizationBlock instance={instance} /></QueryClientProvider>);
  return { ...view, client };
}
async function open(user: User, provider = "GitHub", method?: "personal") {
  await user.click(await screen.findByRole("button", { name: "Add identity" }));
  await user.click(screen.getByRole("button", { name: provider, exact: true }));
  if (method) {
    await user.click(screen.getByText("Other access methods"));
    await user.click(screen.getByRole("button", { name: "Personal account", exact: true }));
  }
}
async function review(user: User) {
  await waitFor(() => expect(screen.getByRole("button", { name: "Review access" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Review access" }));
}
async function reviewApp(user: User) {
  await open(user);
  await user.click(screen.getByRole("button", { name: "Continue" }));
  await user.click(await screen.findByRole("checkbox", { name: "example-org/allowed", exact: true }));
  await review(user);
}
async function remove(user: User) {
  await user.click(await screen.findByRole("button", { name: /More actions for/ }));
  await user.click(await screen.findByRole("menuitem", { name: "Remove access" }));
}

describe("Agent identities", () => {
  beforeEach(() => {
    vi.spyOn(document.body, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 1024, 768));
    vi.spyOn(HTMLElement.prototype, "offsetParent", "get").mockImplementation(function (this: HTMLElement) { return this.parentElement; });
    Object.values(api).forEach((mock) => mock.mockReset());
    api.listConnections.mockResolvedValue([personal, app]);
    api.listConnectionBindings.mockResolvedValue({ bindings: [], readOnly: false });
    api.getGitHubAppConfig.mockResolvedValue({ enabled: true, installUrl: "https://github.com/apps/example-app/installations/new" });
    api.listGitHubInstallations.mockResolvedValue({ installations: [], installUrl: "https://github.com/apps/example-app/installations/new" });
    api.listGitHubRepositories.mockResolvedValue({ repositories: [{ id: 1, fullName: "example-org/allowed", admin: true }, { id: 2, fullName: "example-org/restricted", admin: false }], hasNextPage: false });
    api.createConnectionBinding.mockResolvedValue({}); api.updateConnectionBinding.mockResolvedValue({}); api.deleteConnectionBinding.mockResolvedValue(undefined);
  });
  afterEach(() => vi.restoreAllMocks());

  it("starts with services, with one GitHub option and no account or method decisions", async () => {
    const user = userEvent.setup(); renderUI(); await screen.findByText(empty);
    expect(screen.queryByText("octocat")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Add identity" }));
    expect(screen.getAllByRole("button", { name: "GitHub", exact: true })).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Microsoft 365" })).toBeDisabled();
    expect(screen.queryByText("Personal account")).toBeNull();
    expect(api.startConnection).not.toHaveBeenCalled();
  });

  it("scrolls and focuses Identities when arriving from Connections", async () => {
    const previousUrl = window.location.href;
    const scroll = vi.spyOn(Element.prototype, "scrollIntoView");
    window.history.replaceState(null, "", "#service-access");
    try {
      renderUI();
      await screen.findByText(empty);
      const region = screen.getByRole("region", { name: "Identities", exact: true });
      await waitFor(() => expect(region).toHaveFocus());
      expect(scroll).toHaveBeenCalledWith({ block: "start" });
    } finally {
      window.history.replaceState(null, "", previousUrl);
    }
  });

  it("defaults to the App and requires explicit review before granting selected repositories", async () => {
    const user = userEvent.setup(); renderUI(); await open(user);
    expect(screen.getByRole("radio", { name: /example-org.*octocat/ })).toBeChecked();
    expect(screen.queryByRole("radio", { name: "octocat" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByRole("checkbox", { name: "example-org/restricted (repository admin required)" })).toBeDisabled();
    expect(screen.getByLabelText("Code permission")).toHaveValue("read");
    expect(screen.getByLabelText("Issues permission")).toHaveValue("");
    expect(screen.getByRole("button", { name: "Review access" })).toBeDisabled();
    await user.click(screen.getByRole("checkbox", { name: "example-org/allowed", exact: true }));
    await user.selectOptions(screen.getByLabelText("Pull requests permission"), "write");
    await review(user);
    expect(screen.getByText("example-org/allowed")).toBeInTheDocument();
    expect(screen.getByText(/Pull requests: Read and write/)).toBeInTheDocument();
    expect(api.createConnectionBinding).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Allow access" }));
    await waitFor(() => expect(api.createConnectionBinding).toHaveBeenCalledWith("agent-a", { connectionId: app.id, services: ["github"], enabled: true, github: { repositoryIds: [1], permissions: { contents: "read", pull_requests: "write" } } }));
  });

  it("keeps a rejected access grant visible beside the dialog actions", async () => {
    api.createConnectionBinding.mockRejectedValueOnce(new Error("GitHub denied the selected access"));
    const user = userEvent.setup(); renderUI(); await reviewApp(user);
    await user.click(screen.getByRole("button", { name: "Allow access" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("GitHub denied the selected access");
    expect(screen.getByRole("group", { name: "Access action feedback" })).toContainElement(alert);
    expect(alert.closest(".fui-DialogContent")).toBeNull();
    expect(screen.getByRole("button", { name: "Allow access" })).toBeEnabled();
  });

  it("shows saving feedback and closes after an access grant succeeds", async () => {
    let finish!: (value: ConnectionBinding) => void;
    api.createConnectionBinding.mockImplementationOnce(() => new Promise<ConnectionBinding>((resolve) => { finish = resolve; }));
    const user = userEvent.setup(); renderUI(); await reviewApp(user);
    await user.click(screen.getByRole("button", { name: "Allow access" }));
    const status = await screen.findByRole("status");
    expect(status).toHaveTextContent("Saving access...");
    expect(screen.getByRole("group", { name: "Access action feedback" })).toContainElement(status);
    expect(status.closest(".fui-DialogContent")).toBeNull();
    expect(screen.getByRole("button", { name: "Allow access" })).toBeDisabled();
    finish(appBinding);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("keeps personal access secondary and describes its actual scope at review", async () => {
    const user = userEvent.setup(); renderUI(); await open(user, "GitHub", "personal");
    await user.click(screen.getByRole("button", { name: "Continue" }));
    expect(screen.getByRole("checkbox", { name: "GitHub" })).toBeChecked();
    expect(screen.queryByLabelText("Code permission")).toBeNull();
    await review(user);
    expect(screen.getByText("No per-repository restrictions apply to this personal account connection.")).toBeInTheDocument();
    expect(api.createConnectionBinding).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Allow access" }));
    await waitFor(() => expect(api.createConnectionBinding).toHaveBeenCalledWith("agent-a", { connectionId: personal.id, services: ["github"], enabled: true }));
  });

  it("does not silently fall back to personal access when the App is disabled", async () => {
    api.getGitHubAppConfig.mockResolvedValue({ enabled: false });
    const user = userEvent.setup(); renderUI(); await open(user);
    await screen.findByText(/Selected repository access is not configured/);
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
    expect(screen.queryByRole("radio", { name: "octocat" })).toBeNull();
    expect(api.startConnection).not.toHaveBeenCalled();
    await user.click(screen.getByText("Other access methods")); await user.click(screen.getByRole("button", { name: "Personal account", exact: true }));
    expect(screen.getByRole("radio", { name: "octocat" })).toBeChecked();
  });

  it("distinguishes an App configuration lookup failure from disabled configuration", async () => {
    api.getGitHubAppConfig.mockRejectedValueOnce(new Error("offline"));
    const user = userEvent.setup(); renderUI(); await open(user);
    await screen.findByText("Could not check GitHub App availability.");
    expect(screen.queryByText(/not configured/)).toBeNull();
    await user.click(screen.getByRole("button", { name: "Retry availability" }));
    await screen.findByRole("radio", { name: /example-org/ });
  });

  it("retains repository selections across pages and displays their names at review", async () => {
    api.listGitHubRepositories.mockImplementation((_id, _installation, page) => Promise.resolve({ repositories: [{ id: page, fullName: `example-org/repo-${page}`, admin: true }], hasNextPage: page === 1 }));
    const user = userEvent.setup(); renderUI(); await open(user); await user.click(screen.getByRole("button", { name: "Continue" }));
    await user.click(await screen.findByRole("checkbox", { name: "example-org/repo-1" }));
    await user.click(screen.getByRole("button", { name: "More repositories" }));
    await user.click(await screen.findByRole("checkbox", { name: "example-org/repo-2" }));
    await user.click(screen.getByRole("button", { name: "Previous repositories" }));
    expect(await screen.findByRole("checkbox", { name: "example-org/repo-1" })).toBeChecked();
    await review(user);
    expect(screen.getByText("example-org/repo-1")).toBeInTheDocument(); expect(screen.getByText("example-org/repo-2")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Back" }));
    expect(await screen.findByRole("checkbox", { name: "example-org/repo-1" })).toBeChecked();
  });

  it("requires a nonempty permission and preserves the draft when returning from review", async () => {
    const user = userEvent.setup(); renderUI(); await open(user); await user.click(screen.getByRole("button", { name: "Continue" }));
    await user.click(await screen.findByRole("checkbox", { name: "example-org/allowed", exact: true }));
    await user.selectOptions(screen.getByLabelText("Code permission"), "");
    expect(screen.getByRole("button", { name: "Review access" })).toBeDisabled();
    await user.selectOptions(screen.getByLabelText("Issues permission"), "write"); await review(user);
    await user.click(screen.getByRole("button", { name: "Back" }));
    expect(screen.getByLabelText("Code permission")).toHaveValue(""); expect(screen.getByLabelText("Issues permission")).toHaveValue("write");
  });

  it.each(["Organization", ""])("completes unfinished setup or a pending selection (%s) before granting access", async (accountType) => {
    const unfinished = { ...app, status: "installation_required", github: { userLogin: "octocat", appSlug: "example-app" } } as Connection;
    const canonical = { ...app, id: "canonical-app" };
    api.listConnections.mockResolvedValueOnce([unfinished]).mockResolvedValue([canonical]);
    api.listGitHubInstallations.mockResolvedValue({ installations: [{ id: 7, accountLogin: "example-org", accountType, permissions: { contents: "write" } }], installUrl: "https://github.com/apps/example-app/installations/new" });
    api.selectGitHubInstallation.mockResolvedValue(canonical);
    const user = userEvent.setup(); const view = renderUI(); await open(user); await user.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByRole("radio", { name: `example-org (${accountType ? "Organization" : "GitHub account"})` })).toBeChecked();
    expect(api.selectGitHubInstallation).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Continue" }));
    await screen.findByRole("checkbox", { name: "example-org/allowed", exact: true });
    expect(api.selectGitHubInstallation).toHaveBeenCalledWith(app.id, 7);
    await waitFor(() => expect(view.client.getQueryData<Connection[]>(["tool-connections"])).toEqual([canonical]));
    expect(api.createConnectionBinding).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(api.createConnectionBinding).not.toHaveBeenCalled();
  });

  it("offers installation setup and manual refresh without inventing approval status", async () => {
    api.listConnections.mockResolvedValue([{ ...app, status: "installation_required", github: { userLogin: "octocat" } }]);
    const user = userEvent.setup(); renderUI(); await open(user); await user.click(screen.getByRole("button", { name: "Continue" }));
    await screen.findByText(/Your organization may require administrator approval/);
    expect(screen.getByRole("link", { name: "Set up GitHub access" })).toHaveAttribute("target", "_blank");
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Refresh access" }));
    await waitFor(() => expect(api.listGitHubInstallations).toHaveBeenCalledTimes(2));
  });

  it("shows both GitHub grants within one service group and explains independent personal access", async () => {
    api.listConnectionBindings.mockResolvedValue({ bindings: [binding, { ...appBinding, id: "app-binding" }], readOnly: false });
    renderUI(); await screen.findByText("Personal GitHub access is also enabled. Its permissions remain available independently of the selected repository limits.");
    expect(screen.getAllByRole("region", { name: "GitHub", exact: true })).toHaveLength(1);
    expect(screen.getByRole("article", { name: "octocat — Personal account" })).toBeInTheDocument();
    expect(screen.getByRole("article", { name: "example-org — Selected repositories" })).toBeInTheDocument();
  });

  it("removes only current Agent access using its revision", async () => {
    api.listConnectionBindings.mockResolvedValue({ bindings: [binding], readOnly: false });
    const user = userEvent.setup(); renderUI(); await remove(user);
    await waitFor(() => expect(api.deleteConnectionBinding).toHaveBeenCalledWith("agent-a", binding.id, 7));
    expect(api.deleteConnection).not.toHaveBeenCalled();
  });

  it("allows removal of unavailable access on read-only runtimes", async () => {
    api.listConnections.mockResolvedValue([]); api.listConnectionBindings.mockResolvedValue({ bindings: [binding], readOnly: true });
    const user = userEvent.setup(); renderUI(); await remove(user);
    expect(api.deleteConnectionBinding).toHaveBeenCalledWith("agent-a", binding.id, 7);
    expect(screen.getByRole("button", { name: "Add identity" })).toBeDisabled();
  });

  it("reuses this Agent's Azure identity and never offers another Agent's identity", async () => {
    api.listConnections.mockResolvedValue([azure, { ...azure, id: "other", accountDisplay: "Other Agent", identity: { ...azure.identity!, instanceUid: "other-uid" } }]);
    const user = userEvent.setup(); renderUI(); await open(user, "Azure");
    expect(screen.queryByText("Other Agent")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Continue" })); await review(user);
    await user.click(screen.getByRole("button", { name: "Allow access" }));
    expect(api.createToolIdentity).not.toHaveBeenCalled();
    await waitFor(() => expect(api.createConnectionBinding).toHaveBeenCalledWith("agent-a", { connectionId: azure.id, services: ["azure_arm"], enabled: true }));
  });

  it("recovers an identity creation whose response failed without creating it twice", async () => {
    api.listConnections.mockResolvedValue([]);
    const user = userEvent.setup(); renderUI(); await open(user, "Azure");
    api.listConnections.mockResolvedValue([azure]); api.createToolIdentity.mockRejectedValueOnce(new Error("Response unavailable"));
    await user.click(screen.getByRole("button", { name: "Continue" })); await screen.findByText("Response unavailable");
    await waitFor(() => expect(api.listConnections).toHaveBeenCalledTimes(2));
    await user.click(screen.getByRole("button", { name: "Continue" })); await review(user);
    expect(api.createToolIdentity).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole("button", { name: "Allow access" }));
    await waitFor(() => expect(api.createConnectionBinding).toHaveBeenCalledWith("agent-a", expect.objectContaining({ connectionId: azure.id })));
  });

  it("offers only canonical personal accounts for new access, preserving existing bindings", async () => {
    api.listConnections.mockResolvedValue([personal, { ...personal, id: "alias", canonicalConnectionId: personal.id }]);
    const user = userEvent.setup(); renderUI(); await open(user, "GitHub", "personal");
    expect(screen.getAllByRole("radio", { name: "octocat" })).toHaveLength(1);
  });

  it("does not offer a canonical App connection already used through a historical alias", async () => {
    const alias = { ...app, id: "legacy-app", canonicalConnectionId: app.id };
    api.listConnections.mockResolvedValue([app, alias]);
    api.listConnectionBindings.mockResolvedValue({ bindings: [{ ...appBinding, connectionId: alias.id }], readOnly: false });
    const user = userEvent.setup(); renderUI(); await open(user);
    expect(screen.queryByRole("radio", { name: /example-org.*octocat/ })).toBeNull();
  });

  it("fails closed on load failure and recovers through retry", async () => {
    api.listConnectionBindings.mockRejectedValueOnce(new Error("Unavailable"));
    const user = userEvent.setup(); renderUI(); await screen.findByRole("alert");
    expect(screen.getByRole("button", { name: "Add identity" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Retry" })); await screen.findByText(empty);
    expect(screen.getByRole("button", { name: "Add identity" })).toBeEnabled();
  });

  it("does not initiate OAuth when the popup is blocked", async () => {
    api.listConnections.mockResolvedValue([]); vi.spyOn(window, "open").mockReturnValue(null);
    const user = userEvent.setup(); renderUI(); await open(user);
    await user.click(await screen.findByRole("button", { name: "Sign in to GitHub" }));
    await screen.findByText(/Allow popups/); expect(api.startConnection).not.toHaveBeenCalled();
  });

  it("cancel never restores a disabled binding or creates Agent access", async () => {
    api.listConnectionBindings.mockResolvedValue({ bindings: [{ ...appBinding, enabled: false }], readOnly: false });
    const user = userEvent.setup(); renderUI();
    const addIdentity = await screen.findByRole("button", { name: "Add identity" });
    await open(user); await user.click(screen.getByRole("button", { name: "Continue" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(api.createConnectionBinding).not.toHaveBeenCalled(); expect(api.updateConnectionBinding).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(addIdentity).toHaveFocus());
  });

  it("isolates drafts and caches when the same Agent name gets a new UID", async () => {
    api.listConnectionBindings.mockResolvedValueOnce({ bindings: [binding], readOnly: false });
    const user = userEvent.setup(); const view = renderUI(); await screen.findByRole("article"); await open(user);
    const callsBeforeReplacement = api.listConnectionBindings.mock.calls.length;
    let finish!: (value: { bindings: ConnectionBinding[]; readOnly: boolean }) => void;
    api.listConnectionBindings.mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
    view.rerender(<QueryClientProvider client={view.client}><ToolAuthorizationBlock instance={{ ...instance, uid: "new-uid" }} /></QueryClientProvider>);
    expect(screen.queryByRole("dialog")).toBeNull(); expect(screen.queryByRole("article")).toBeNull();
    expect(screen.getByRole("button", { name: "Add identity" })).toBeDisabled();
    await waitFor(() => expect(api.listConnectionBindings).toHaveBeenCalledTimes(callsBeforeReplacement + 1)); finish({ bindings: [], readOnly: false }); await screen.findByText(empty);
  });

  it("refetches conflicted removal and retries with the latest revision", async () => {
    api.listConnectionBindings.mockResolvedValueOnce({ bindings: [binding], readOnly: false }).mockResolvedValue({ bindings: [{ ...binding, revision: 8 }], readOnly: false });
    api.deleteConnectionBinding.mockRejectedValueOnce(new Error("Connection changed"));
    const user = userEvent.setup(); renderUI(); await remove(user); await screen.findByText("Connection changed");
    await waitFor(() => expect(api.listConnectionBindings).toHaveBeenCalledTimes(2)); await remove(user);
    await waitFor(() => expect(api.deleteConnectionBinding).toHaveBeenLastCalledWith("agent-a", binding.id, 8));
  });

  it("keeps an edit draft paired with its original revision across refetch and retries", async () => {
    const current = { ...binding, connectionId: google.id, services: google.services };
    const refreshed = { ...current, services: ["google_gmail"], revision: 9 };
    api.listConnections.mockResolvedValue([google]); api.listConnectionBindings.mockResolvedValue({ bindings: [current], readOnly: false });
    api.updateConnectionBinding.mockRejectedValue(new Error("Changed; cancel and reopen to reload."));
    const user = userEvent.setup(); const view = renderUI(); await user.click(await screen.findByRole("button", { name: "Manage access" }));
    await user.click(screen.getByRole("checkbox", { name: "Gmail" }));
    api.listConnectionBindings.mockResolvedValue({ bindings: [refreshed], readOnly: false });
    await act(async () => { await view.client.refetchQueries({ queryKey: ["connection-bindings", instance.instanceName, instance.uid] }); });
    await review(user); await user.click(screen.getByRole("button", { name: "Save access" })); await screen.findByText("Changed; cancel and reopen to reload.");
    const draft = { connectionId: google.id, services: ["google_calendar"], enabled: true, revision: 7 };
    expect(api.updateConnectionBinding).toHaveBeenLastCalledWith("agent-a", binding.id, draft);
    await user.click(screen.getByRole("button", { name: "Save access" })); await waitFor(() => expect(api.updateConnectionBinding).toHaveBeenCalledTimes(2));
    expect(api.updateConnectionBinding).toHaveBeenLastCalledWith("agent-a", binding.id, draft);
    await waitFor(() => expect(screen.getByRole("button", { name: "Cancel" })).toBeEnabled()); await user.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { hidden: true })).toBeNull());
    await user.click(await screen.findByRole("button", { name: "Manage access" })); expect(screen.getByRole("checkbox", { name: "Gmail" })).toBeChecked(); expect(screen.getByRole("checkbox", { name: "Calendar" })).not.toBeChecked();
    api.updateConnectionBinding.mockResolvedValue({}); await review(user); await user.click(screen.getByRole("button", { name: "Save access" }));
    await waitFor(() => expect(api.updateConnectionBinding).toHaveBeenLastCalledWith("agent-a", binding.id, { connectionId: google.id, services: ["google_gmail"], enabled: true, revision: 9 }));
  });

  it("preserves exact GitHub permissions when editing and allows removing unavailable selections", async () => {
    api.listConnections.mockResolvedValue([app]);
    api.listConnectionBindings.mockResolvedValue({ bindings: [{ ...appBinding, github: { repositoryIds: [1, 99], permissions: { issues: "write" } } }], readOnly: false });
    const user = userEvent.setup(); renderUI(); await user.click(await screen.findByRole("button", { name: "Manage access" }));
    expect(screen.getByLabelText("Code permission")).toHaveValue(""); expect(screen.getByLabelText("Issues permission")).toHaveValue("write");
    await user.click(screen.getByText("Selected repositories (2)"));
    await user.click(await screen.findByRole("button", { name: "Remove unavailable repository 99" }));
    await review(user); await user.click(screen.getByRole("button", { name: "Save access" }));
    await waitFor(() => expect(api.updateConnectionBinding).toHaveBeenCalledWith("agent-a", appBinding.id, { connectionId: app.id, services: ["github"], enabled: true, github: { repositoryIds: [1], permissions: { issues: "write" } }, revision: 7 }));
  });
});
