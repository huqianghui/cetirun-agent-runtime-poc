// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, expect, it, vi } from "vitest";
import { SandboxEditPage } from "./SandboxEditPage";
import { SandboxListPage } from "./SandboxListPage";
import * as api from "../../api/adminSandbox";
import * as teams from "../../api/adminTeam";
import { AdminConfigurationError } from "../../api/adminError";

vi.mock("../../api/adminSandbox");
vi.mock("../../api/adminTeam");
const quantities = { cpu: "1", memory: "1Gi", "ephemeral-storage": "1Gi" };
const box: api.SandboxDTO = { name: "example", uid: "sandbox-uid", resourceVersion: "7", generation: 1, type: "kata", ready: "Unknown", deleting: false,
  resources: { agent: { requests: quantities, limits: quantities }, helpers: { requests: quantities, limits: quantities } },
  storage: { home: { storageClassName: "example-csi", size: "10Gi", accessMode: "ReadWriteOncePod" }, scratch: { sizeLimit: "1Gi" } },
};
const storageOptions: api.SandboxStorageOptions = {
  storageClasses: [{ name: "example-csi", provisioner: "csi.example.com", allowVolumeExpansion: true }],
  defaultStorageClassName: "example-csi",
  homeAccessModes: { kata: ["ReadWriteOncePod", "ReadWriteOnce"], gvisor: ["ReadWriteOncePod", "ReadWriteOnce"], container: ["ReadWriteOncePod", "ReadWriteOnce"] },
};
beforeEach(() => {
  vi.resetAllMocks(); vi.mocked(api.getSandbox).mockResolvedValue(box); vi.mocked(api.listSandboxes).mockResolvedValue([box]);
  vi.mocked(teams.listTeams).mockResolvedValue([]);
  vi.mocked(api.createSandbox).mockResolvedValue(box); vi.mocked(api.updateSandbox).mockResolvedValue(box);
  vi.mocked(api.getSandboxStorageOptions).mockResolvedValue(storageOptions);
});
function renderPage(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[path]}><Routes>
    <Route path="/admin/sandboxes/new" element={<SandboxEditPage />} />
    <Route path="/admin/sandboxes/:name/edit" element={<SandboxEditPage />} />
    <Route path="/admin/sandboxes" element={<SandboxListPage />} />
  </Routes></MemoryRouter></QueryClientProvider>);
  return client;
}

it.each([
  { name: "default", helperMemoryLimit: "2Gi" },
  { name: "custom", helperMemoryLimit: "3Gi" },
])("creates complete Sandbox resources with $name helper memory without claiming readiness", async ({ helperMemoryLimit }) => {
  const user = userEvent.setup(); renderPage("/admin/sandboxes/new");
  const helperMemory = screen.getByRole("textbox", { name: "Helper memory limit" });
  expect(helperMemory).toHaveValue("2Gi");
  if (helperMemoryLimit !== "2Gi") {
    await user.clear(helperMemory);
    await user.type(helperMemory, helperMemoryLimit);
  }
  await user.type(screen.getByRole("textbox", { name: "Name" }), "example");
  await user.selectOptions(screen.getByRole("combobox", { name: "Runtime" }), "gvisor");
  await waitFor(() => expect(screen.getByRole("combobox", { name: "HOME StorageClass" })).toHaveValue("example-csi"));
  await user.click(screen.getByRole("button", { name: "Create" }));
  await waitFor(() => expect(api.createSandbox).toHaveBeenCalled());
  const request = vi.mocked(api.createSandbox).mock.calls[0][0];
  expect(request).toMatchObject({ name: "example", type: "gvisor", storage: box.storage });
  expect(request.resources.agent.requests).toHaveProperty("ephemeral-storage");
  expect(request.resources.helpers).toEqual({
    requests: { cpu: "100m", memory: "128Mi", "ephemeral-storage": "128Mi" },
    limits: { cpu: "500m", memory: helperMemoryLimit, "ephemeral-storage": "1Gi" },
  });
  expect(request).not.toHaveProperty("ready");
});

it("edits a Sandbox named new using its exact version and displays rejection details", async () => {
  vi.mocked(api.getSandbox).mockResolvedValue({ ...box, name: "new" });
  vi.mocked(api.updateSandbox).mockRejectedValue(new Error("Agent cpu request must not exceed its limit"));
  const user = userEvent.setup(); renderPage("/admin/sandboxes/new/edit");
  expect(await screen.findByRole("textbox", { name: "Name" })).toBeDisabled();
  await waitFor(() => expect(screen.getByRole("combobox", { name: "HOME StorageClass" })).toHaveValue("example-csi"));
  expect(screen.getByRole("textbox", { name: "Helper memory limit" })).toHaveValue("1Gi");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText(/request must not exceed/)).toBeInTheDocument();
  expect(api.updateSandbox).toHaveBeenCalledExactlyOnceWith("new", expect.objectContaining({ uid: "sandbox-uid", resourceVersion: "7", resources: box.resources, storage: box.storage }));
  expect(vi.mocked(api.updateSandbox).mock.calls[0][1]).not.toHaveProperty("ready");
  expect(api.createSandbox).not.toHaveBeenCalled();
});

it("keeps the edited Sandbox form and shows safe compatibility diagnostics after a rejected save", async () => {
  vi.mocked(api.updateSandbox).mockRejectedValue(new AdminConfigurationError("The Sandbox conflicts with Team research.", "configuration_incompatible", [{
    code: "sandbox_incompatible", field: "spec.storage.home.accessMode", message: "The selected Sandbox does not support one or more requested Policy controls.",
    team: { name: "research", uid: "team-uid", generation: 1 }, policy: { name: "standard", uid: "policy-uid", generation: 2 }, sandbox: { name: "example", uid: "sandbox-uid", generation: 1 },
  }]));
  const user = userEvent.setup(); renderPage("/admin/sandboxes/example/edit");
  const homeSize = await screen.findByRole("textbox", { name: "HOME size" });
  await user.clear(homeSize); await user.type(homeSize, "20Gi");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByLabelText("Configuration save error")).toHaveTextContent("spec.storage.home.accessMode");
  expect(screen.getByLabelText("Configuration save error")).toHaveTextContent("Team research / Policy standard / Sandbox example");
  expect(homeSize).toHaveValue("20Gi");
});

it("shows the current Sandbox readiness failure with its referencing Teams", async () => {
  vi.mocked(api.listSandboxes).mockResolvedValue([{
    ...box, ready: "False", observedGeneration: 1,
    conditions: [{ type: "Ready", status: "False", observedGeneration: 1, reason: "PrerequisiteMissing", message: "The runtime class is unavailable." }],
  }]);
  const user = userEvent.setup(); renderPage("/admin/sandboxes");
  await user.click(await screen.findByRole("button", { name: "Show Teams using example" }));
  expect(await screen.findByText("Readiness: PrerequisiteMissing: The runtime class is unavailable.")).toBeInTheDocument();
});

it("does not show a stale Team report as compatible after the Sandbox is recreated", async () => {
  const checkedAt = new Date(Date.now() - 1_000);
  vi.mocked(api.listSandboxes).mockResolvedValue([{ ...box, uid: "replacement-sandbox-uid", generation: 2, ready: "True" }]);
  vi.mocked(teams.listTeams).mockResolvedValue([{
    name: "research", uid: "team-uid", generation: 1, resourceVersion: "1", agentPolicyRef: "standard", sandboxRefs: ["example"], defaultSandboxRef: "example",
    compatibility: {
      state: "Compatible", team: { name: "research", uid: "team-uid", generation: 1 }, policy: { name: "standard", uid: "policy-uid", generation: 1 },
      sandboxes: [{ sandbox: { name: "example", uid: "sandbox-uid", generation: 1 }, state: "Compatible" }], checkedAt: checkedAt.toISOString(), validUntil: new Date(checkedAt.getTime() + 30_000).toISOString(),
    },
  }]);
  const user = userEvent.setup(); renderPage("/admin/sandboxes");
  await user.click(await screen.findByRole("button", { name: "Show Teams using example" }));
  expect(await screen.findByText("The saved compatibility report no longer matches this configuration.")).toBeInTheDocument();
});

it.each(["ReadWriteOnce", "ReadWriteOncePod"] as const)("creates an explicit container Sandbox with %s and an isolation warning", async (accessMode) => {
  const user = userEvent.setup(); renderPage("/admin/sandboxes/new");
  expect(screen.getByRole("combobox", { name: "Runtime" })).toHaveValue("kata");
  expect(screen.queryByText(/Ordinary containers share/)).not.toBeInTheDocument();
  await user.selectOptions(screen.getByRole("combobox", { name: "Runtime" }), "container");
  expect(screen.getByText(/do not provide Kata VM isolation/)).toBeInTheDocument();
  expect(screen.getByText(/verified handler/)).toBeInTheDocument();
  await user.type(screen.getByRole("textbox", { name: "Name" }), "container-example");
  await waitFor(() => expect(screen.getByRole("combobox", { name: "HOME StorageClass" })).toHaveValue("example-csi"));
  await user.selectOptions(screen.getByRole("combobox", { name: "HOME access mode" }), accessMode);
  await user.click(screen.getByRole("button", { name: "Create" }));
  await waitFor(() => expect(api.createSandbox).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({
    name: "container-example", type: "container",
    storage: { ...box.storage, home: { ...box.storage.home, accessMode } },
  })));
  expect(vi.mocked(api.createSandbox).mock.calls[0][0].resources.helpers.limits.memory).toBe("2Gi");
});

it("preserves an existing container runtime on edit without claiming runtime conversion", async () => {
  vi.mocked(api.getSandbox).mockResolvedValue({ ...box, type: "container" });
  const user = userEvent.setup(); renderPage("/admin/sandboxes/example/edit");
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Runtime" })).toHaveValue("container"));
  expect(screen.getByText(/does not convert a running Agent/)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(api.updateSandbox).toHaveBeenCalledExactlyOnceWith("example", expect.objectContaining({
    type: "container", uid: box.uid, resourceVersion: box.resourceVersion,
    resources: box.resources, storage: box.storage,
  })));
});

it("selects the configured approved StorageClass for a new Sandbox", async () => {
  renderPage("/admin/sandboxes/new");
  const storageClass = await screen.findByRole("combobox", { name: "HOME StorageClass" });
  await waitFor(() => expect(storageClass).toHaveValue("example-csi"));
  expect(screen.getByRole("option", { name: /csi\.example\.com, expansion supported/ })).toBeInTheDocument();
});

it("keeps the approved StorageClass when a form edit arrives with the catalog", async () => {
  let resolveStorage!: (value: api.SandboxStorageOptions) => void;
  vi.mocked(api.getSandboxStorageOptions).mockImplementationOnce(() => new Promise(resolve => { resolveStorage = resolve; }));
  function EditOnCatalogArrival() {
    const catalog = useQuery({ queryKey: ["admin", "sandbox-storage-options"], queryFn: api.getSandboxStorageOptions });
    useEffect(() => {
      // The child's catalog effect and this edit share a batch, before the
      // queued default StorageClass update has committed to the form.
      if (catalog.data) fireEvent.change(screen.getByRole("combobox", { name: "Runtime" }), { target: { value: "container" } });
    }, [catalog.data]);
    return <SandboxEditPage />;
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/admin/sandboxes/new"]}>
    <Routes><Route path="/admin/sandboxes/new" element={<EditOnCatalogArrival />} /></Routes>
  </MemoryRouter></QueryClientProvider>);
  await act(async () => { resolveStorage(storageOptions); });
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Runtime" })).toHaveValue("container"));
  expect(screen.getByRole("combobox", { name: "HOME StorageClass" })).toHaveValue("example-csi");
});

it("requires a choice when several approved StorageClasses have no configured default", async () => {
  vi.mocked(api.getSandboxStorageOptions).mockResolvedValue({
    ...storageOptions,
    storageClasses: [storageOptions.storageClasses[0], { name: "premium-csi", provisioner: "premium.example.com", allowVolumeExpansion: false }],
    defaultStorageClassName: "",
  });
  const user = userEvent.setup(); renderPage("/admin/sandboxes/new");
  const storageClass = await screen.findByRole("combobox", { name: "HOME StorageClass" });
  await waitFor(() => expect(screen.getByRole("option", { name: /premium\.example\.com/ })).toBeInTheDocument());
  expect(storageClass).toHaveValue("");
  expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
  await user.selectOptions(storageClass, "premium-csi");
  expect(storageClass).toHaveValue("premium-csi");
  expect(screen.getByRole("button", { name: "Create" })).toBeEnabled();
});

it("keeps an unavailable existing StorageClass visible until an administrator replaces it", async () => {
  vi.mocked(api.getSandbox).mockResolvedValue({ ...box, storage: { ...box.storage, home: { ...box.storage.home, storageClassName: "retired-csi" } } });
  const user = userEvent.setup(); renderPage("/admin/sandboxes/example/edit");
  const storageClass = await screen.findByRole("combobox", { name: "HOME StorageClass" });
  await waitFor(() => expect(storageClass).toHaveValue("retired-csi"));
  expect(await screen.findByText(/selected StorageClass is no longer available or approved/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  await user.selectOptions(storageClass, "example-csi");
  expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
});

it("blocks saves until a failed StorageClass catalog is retried", async () => {
  vi.mocked(api.getSandboxStorageOptions).mockRejectedValueOnce(new Error("Sandbox storage options are not configured. Configure the Portal to read the Operator's approved platform configuration."));
  const user = userEvent.setup(); renderPage("/admin/sandboxes/new");
  expect(await screen.findByText(/Failed to load approved StorageClasses/)).toBeInTheDocument();
  expect(screen.getByText(/Sandbox storage options are not configured/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
  await user.click(screen.getByRole("button", { name: "Retry StorageClasses" }));
  await waitFor(() => expect(screen.getByRole("combobox", { name: "HOME StorageClass" })).toBeEnabled());
  expect(screen.getByRole("combobox", { name: "HOME StorageClass" })).toHaveValue("example-csi");
});

it("keeps StorageClass selection and saving disabled while approved choices load", async () => {
  vi.mocked(api.getSandboxStorageOptions).mockImplementationOnce(() => new Promise<api.SandboxStorageOptions>(() => {}));
  renderPage("/admin/sandboxes/new");
  expect(await screen.findByText("Loading approved StorageClasses...")).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "HOME StorageClass" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
});

it("explains when no approved StorageClasses are available", async () => {
  vi.mocked(api.getSandboxStorageOptions).mockResolvedValue({ ...storageOptions, storageClasses: [], defaultStorageClassName: "" });
  renderPage("/admin/sandboxes/new");
  expect(await screen.findByText(/No approved StorageClasses are available/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
});

it("does not replace a choice when it disappears after a refresh", async () => {
  const user = userEvent.setup(); renderPage("/admin/sandboxes/new");
  const storageClass = await screen.findByRole("combobox", { name: "HOME StorageClass" });
  await waitFor(() => expect(storageClass).toHaveValue("example-csi"));
  vi.mocked(api.getSandboxStorageOptions).mockResolvedValueOnce({
    ...storageOptions,
    storageClasses: [{ name: "replacement-csi", provisioner: "replacement.example.com", allowVolumeExpansion: false }],
    defaultStorageClassName: "replacement-csi",
  });
  await user.click(screen.getByRole("button", { name: "Refresh StorageClasses" }));
  expect(await screen.findByText(/selected StorageClass is no longer available or approved/)).toBeInTheDocument();
  expect(storageClass).toHaveValue("example-csi");
  expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
});

it("uses only access modes approved for the selected runtime", async () => {
  vi.mocked(api.getSandboxStorageOptions).mockResolvedValue({
    ...storageOptions,
    homeAccessModes: { kata: ["ReadWriteOncePod"], gvisor: ["ReadWriteOnce"], container: ["ReadWriteOncePod", "ReadWriteOnce"] },
  });
  const user = userEvent.setup(); renderPage("/admin/sandboxes/new");
  await waitFor(() => expect(screen.getByRole("combobox", { name: "HOME StorageClass" })).toHaveValue("example-csi"));
  await user.selectOptions(screen.getByRole("combobox", { name: "Runtime" }), "gvisor");
  const accessMode = screen.getByRole("combobox", { name: "HOME access mode" });
  await waitFor(() => expect(accessMode).toHaveValue("ReadWriteOnce"));
  expect(screen.queryByRole("option", { name: "ReadWriteOncePod" })).not.toBeInTheDocument();
});

it("shows an unsupported existing access mode without silently changing it", async () => {
  vi.mocked(api.getSandboxStorageOptions).mockResolvedValue({
    ...storageOptions,
    homeAccessModes: { kata: ["ReadWriteOnce"] },
  });
  const user = userEvent.setup(); renderPage("/admin/sandboxes/example/edit");
  const accessMode = await screen.findByRole("combobox", { name: "HOME access mode" });
  await waitFor(() => expect(accessMode).toHaveValue("ReadWriteOncePod"));
  expect(await screen.findByText(/not supported by the kata runtime/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  await user.selectOptions(accessMode, "ReadWriteOnce");
  expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
});

it("deletes the version selected for confirmation even if the list refreshes", async () => {
  const user = userEvent.setup(); const client = renderPage("/admin/sandboxes");
  await user.click(await screen.findByRole("button", { name: "Delete example" }));
  await act(async () => { client.setQueryData(["admin", "sandboxes"], [{ ...box, uid: "replacement-uid", resourceVersion: "20" }]); });
  await user.click(await screen.findByRole("button", { name: "Delete", exact: true }));
  await waitFor(() => expect(api.deleteSandbox).toHaveBeenCalledExactlyOnceWith(box));
});
