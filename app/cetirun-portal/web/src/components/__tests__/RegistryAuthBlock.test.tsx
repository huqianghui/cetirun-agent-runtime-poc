// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Instance } from "../../types/instance";
import { RegistryAuthBlock } from "../RegistryAuthBlock";
import { updateRegistryAuth } from "../../api/registry";

vi.mock("../../api/registry", () => ({ updateRegistryAuth: vi.fn() }));

const instance: Instance = {
  instanceName: "example", uid: "example-uid", resourceVersion: "1", agent: { type: "custom", image: "registry.example.com/agent:1", registrySecretRef: { name: "platform-source" } },
  registryAuthConfigured: true, alias: "example", displayName: "Example", team: "", phase: "Ready", createdAt: "2026-01-01", upgradeAvailable: false,
  conditions: [{ type: "RegistryCredentialsReady", status: "False", reason: "RegistrySyncPending", message: "Waiting for sync" }],
};

function setup(value = instance) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = (next: Instance) => <QueryClientProvider client={client}><RegistryAuthBlock instance={next} /></QueryClientProvider>;
  return { ...render(view(value)), view, client, user: userEvent.setup() };
}

describe("RegistryAuthBlock", () => {
  beforeEach(() => { vi.resetAllMocks(); vi.mocked(updateRegistryAuth).mockResolvedValue(); });

  it("shows private-image actions without synchronization copy or prefilling credentials", async () => {
    const { user } = setup();
    expect(screen.getByText("Configured (write-only)")).toBeInTheDocument();
    expect(screen.queryByText(/Credential sync/i)).toBeNull();
    expect(screen.queryByText(/RegistrySyncPending|Waiting for sync/)).toBeNull();
    expect(screen.getByRole("button", { name: "Remove registry credentials" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Replace registry credentials" }));
    expect(screen.getByLabelText(/Registry username/)).toHaveValue("");
    expect(screen.getByLabelText(/Registry password or token/)).toHaveValue("");
    await user.type(screen.getByLabelText(/Registry username/), "example");
    await user.type(screen.getByLabelText(/Registry password or token/), "synthetic-token");
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: "Replace registry credentials" }));
    expect(screen.getByLabelText(/Registry password or token/)).toHaveValue("");
  });

  it.each([false, true])("hides the whole section without a private-image reference even if configured is %s", (registryAuthConfigured) => {
    setup({ ...instance, registryAuthConfigured, agent: { ...instance.agent, registrySecretRef: undefined } });
    expect(screen.queryByRole("region", { name: "Registry credentials" })).toBeNull();
    expect(screen.queryByText("Not configured")).toBeNull();
    expect(screen.queryByText(instance.agent.image!)).toBeNull();
    expect(screen.queryByRole("button", { name: /registry credentials/i })).toBeNull();
    expect(screen.queryByText(/Credential sync/i)).toBeNull();
  });

  it("keeps replacement and removal available when private credentials still need saving", async () => {
    const { user } = setup({ ...instance, registryAuthConfigured: false });
    expect(screen.getByText("Not configured")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove registry credentials" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Replace registry credentials" }));
    expect(screen.getByLabelText(/Registry username/)).toHaveValue("");
    expect(screen.getByLabelText(/Registry password or token/)).toHaveValue("");
  });

  it("hides the whole section after private-image authentication is removed", async () => {
    const { user, rerender, view } = setup();
    await user.click(screen.getByRole("button", { name: "Remove registry credentials" }));
    await user.click(screen.getByRole("button", { name: "Confirm removal" }));
    await waitFor(() => expect(updateRegistryAuth).toHaveBeenCalledWith(instance, undefined));
    rerender(view({ ...instance, registryAuthConfigured: false, agent: { ...instance.agent, registrySecretRef: undefined } }));
    expect(screen.queryByRole("region", { name: "Registry credentials" })).toBeNull();
    expect(screen.queryByRole("button", { name: /registry credentials/i })).toBeNull();
    expect(screen.queryByRole("button", { name: "Confirm removal" })).toBeNull();
  });

  it("clears credentials after saving and does not retain them in mutation state", async () => {
    const { user, client } = setup();
    await user.click(screen.getByRole("button", { name: "Replace registry credentials" }));
    await user.type(screen.getByLabelText(/Registry username/), "example");
    await user.type(screen.getByLabelText(/Registry password or token/), "synthetic-token");
    await user.click(screen.getByRole("button", { name: "Save registry credentials" }));
    await waitFor(() => expect(updateRegistryAuth).toHaveBeenCalledWith(instance, { username: "example", password: "synthetic-token" }));
    expect(screen.queryByLabelText(/Registry password or token/)).toBeNull();
    expect(client.getMutationCache().getAll()).toHaveLength(0);
  });

  it("keeps the original target version when a background refresh changes the image", async () => {
    const { user, rerender, view } = setup();
    await user.click(screen.getByRole("button", { name: "Replace registry credentials" }));
    await user.type(screen.getByLabelText(/Registry username/), "example");
    await user.type(screen.getByLabelText(/Registry password or token/), "synthetic-token");
    rerender(view({ ...instance, resourceVersion: "2", agent: { ...instance.agent, image: "other.example.com/agent:2" } }));
    vi.mocked(updateRegistryAuth).mockRejectedValueOnce(new Error("Instance changed; refresh and try again"));
    await user.click(screen.getByRole("button", { name: "Save registry credentials" }));
    await waitFor(() => expect(screen.getByText(/Instance changed/)).toBeInTheDocument());
    expect(vi.mocked(updateRegistryAuth).mock.calls[0][0].resourceVersion).toBe("1");
    expect(screen.queryByLabelText(/Registry password or token/)).toBeNull();
    await user.click(screen.getByRole("button", { name: "Replace registry credentials" }));
    expect(screen.getByLabelText(/Registry username/)).toHaveValue("");
    expect(screen.getByLabelText(/Registry password or token/)).toHaveValue("");
  });

  it("requires confirmation to remove and does not offer writes during deletion", async () => {
    const { user, rerender, view } = setup();
    await user.click(screen.getByRole("button", { name: "Remove registry credentials" }));
    expect(updateRegistryAuth).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Confirm removal" }));
    await waitFor(() => expect(updateRegistryAuth).toHaveBeenCalledWith(instance, undefined));
    rerender(view({ ...instance, phase: "Deleting" }));
    expect(screen.queryByRole("button", { name: "Replace registry credentials" })).toBeNull();
  });
});
