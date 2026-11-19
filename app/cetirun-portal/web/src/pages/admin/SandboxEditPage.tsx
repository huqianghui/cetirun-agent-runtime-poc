// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useState, type FormEvent } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button, Divider, Field, Input, MessageBar, Select, Spinner, Text, Title2 } from "@fluentui/react-components";
import { createSandbox, getSandbox, getSandboxStorageOptions, updateSandbox, type SandboxHomeAccessMode, type SandboxSpec, type SandboxUpsertRequest } from "../../api/adminSandbox";
import { AdminConfigurationErrorMessage } from "../../components/AdminConfigurationErrorMessage";

type SandboxForm = SandboxUpsertRequest & SandboxSpec;

const emptyForm: SandboxForm = {
  name: "", type: "kata",
  resources: {
    agent: { requests: { cpu: "500m", memory: "1Gi", "ephemeral-storage": "1Gi" }, limits: { cpu: "2", memory: "4Gi", "ephemeral-storage": "8Gi" } },
    helpers: { requests: { cpu: "100m", memory: "128Mi", "ephemeral-storage": "128Mi" }, limits: { cpu: "500m", memory: "2Gi", "ephemeral-storage": "1Gi" } },
  },
  storage: { home: { storageClassName: "", size: "10Gi", accessMode: "ReadWriteOncePod" }, scratch: { sizeLimit: "1Gi" } },
};

export function SandboxEditPage() {
  const { name } = useParams<{ name: string }>();
  const isCreate = !name;
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [form, setForm] = useState<SandboxForm>(emptyForm);
  const [submitError, setSubmitError] = useState<Error | null>(null);
  const { data: existing, isLoading, error } = useQuery({ queryKey: ["admin", "sandbox", name], queryFn: () => getSandbox(name!), enabled: !isCreate });
  const storageOptions = useQuery({ queryKey: ["admin", "sandbox-storage-options"], queryFn: getSandboxStorageOptions, retry: false });
  const classes = storageOptions.data?.storageClasses ?? [];
  const homeAccessModes = storageOptions.data?.homeAccessModes[form.type] ?? [];
  const selectedStorageClassIsApproved = classes.some((item) => item.name === form.storage.home.storageClassName);
  const selectedAccessModeIsSupported = homeAccessModes.includes(form.storage.home.accessMode);
  useEffect(() => {
    if (existing) setForm({ name: existing.name, uid: existing.uid, resourceVersion: existing.resourceVersion, type: existing.type, resources: existing.resources, storage: existing.storage });
  }, [existing]);
  useEffect(() => {
    if (!isCreate || !storageOptions.data || form.storage.home.storageClassName) return;
    const configuredDefault = storageOptions.data.defaultStorageClassName;
    const configuredClassExists = storageOptions.data.storageClasses.some((item) => item.name === configuredDefault);
    const selected = configuredClassExists ? configuredDefault : storageOptions.data.storageClasses.length === 1 ? storageOptions.data.storageClasses[0].name : "";
    if (selected) setForm((current) => ({ ...current, storage: { ...current.storage, home: { ...current.storage.home, storageClassName: selected } } }));
  }, [isCreate, storageOptions.data, form.storage.home.storageClassName]);
  useEffect(() => {
    if (!isCreate || !storageOptions.data) return;
    const supportedModes = storageOptions.data.homeAccessModes[form.type] ?? [];
    if (supportedModes.includes(form.storage.home.accessMode) || supportedModes.length === 0) return;
    setForm((current) => ({ ...current, storage: { ...current.storage, home: { ...current.storage.home, accessMode: supportedModes[0] } } }));
  }, [isCreate, storageOptions.data, form.storage.home.accessMode, form.type]);
  const success = () => {
    queryClient.invalidateQueries({ queryKey: ["admin", "sandboxes"] });
    if (name) queryClient.invalidateQueries({ queryKey: ["admin", "sandbox", name] });
    navigate("/admin/sandboxes");
  };
  const mutation = useMutation({ mutationFn: (request: SandboxUpsertRequest) => isCreate ? createSandbox(request) : updateSandbox(name!, request), onSuccess: success, onError: (error: Error) => setSubmitError(error) });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    setSubmitError(null);
    if (mutation.isPending || existing?.deleting) return;
    if (isCreate && !/^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/.test(form.name?.trim() || "")) {
      setSubmitError(new Error("Name must be a DNS-1123 label: lowercase, start with a letter, max 63 chars.")); return;
    }
    if (storageOptions.isFetching) { setSubmitError(new Error("Approved StorageClasses are still loading.")); return; }
    if (storageOptions.isError) { setSubmitError(new Error("Approved StorageClasses could not be loaded. Retry before saving.")); return; }
    if (!selectedStorageClassIsApproved) { setSubmitError(new Error("Choose an approved StorageClass used for Agent HOME.")); return; }
    if (!selectedAccessModeIsSupported) { setSubmitError(new Error(`Choose a HOME access mode supported by the ${form.type} runtime.`)); return; }
    mutation.mutate({ ...form, name: form.name?.trim(), storage: { ...form.storage, home: { ...form.storage.home, storageClassName: form.storage.home.storageClassName.trim() } } });
  };
  if (!isCreate && isLoading) return <Spinner label="Loading Sandbox..." />;
  if (!isCreate && error) return <MessageBar intent="error">Failed to load: {error.message}</MessageBar>;
  return (
    <form onSubmit={submit} style={{ maxWidth: 720, display: "flex", flexDirection: "column", gap: 16 }}>
      <Title2>{isCreate ? "New Sandbox" : `Sandbox: ${name}`}</Title2>
      <Text>A Sandbox defines reusable runtime, resource and storage settings. The selected runtime requires an explicitly registered RuntimeClass and verified handler; the StorageClass must also be approved.</Text>
      <Field label="Name" required><Input value={form.name || ""} disabled={!isCreate} onChange={(_, data) => setForm(current => ({ ...current, name: data.value }))} placeholder="e.g. standard" required /></Field>
      <Field label="Runtime" required><Select value={form.type} onChange={event => {
        const type = event.target.value as SandboxSpec["type"];
        setForm(current => ({ ...current, type }));
      }}>
        <option value="kata">Kata</option><option value="gvisor">gVisor</option><option value="container">Container (runc, shared node kernel)</option>
      </Select></Field>
      {form.type === "container" && <MessageBar intent="warning">Ordinary containers share the node kernel and do not provide Kata VM isolation. AgentPolicy, HOME ownership and lifecycle controls still apply. Resource limits are per container; no VM overhead is added automatically.</MessageBar>}
      {!isCreate && <Text size={300}>Changing this configuration does not convert a running Agent to another runtime. Use a separate Sandbox and a new Agent for a different backend.</Text>}
      {(["agent", "helpers"] as const).map(container => <div key={container} style={{ display: "flex", flexDirection: "column", gap: 12 }}>
        <Divider />
        <Text weight="semibold">{container === "agent" ? "Agent resources" : "Resources per helper container"}</Text>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
          {(["cpu", "memory", "ephemeral-storage"] as const).flatMap(resource => (["requests", "limits"] as const).map(bound => <Field key={`${resource}-${bound}`}
            label={`${container === "agent" ? "Agent" : "Helper"} ${resource} ${bound === "requests" ? "request" : "limit"}`} required>
            <Input value={form.resources[container][bound][resource]} required onChange={(_, data) => setForm(current => ({ ...current, resources: { ...current.resources, [container]: { ...current.resources[container], [bound]: { ...current.resources[container][bound], [resource]: data.value } } } }))} />
          </Field>))}
        </div>
        <Text size={300}>Use Kubernetes quantities such as 500m for CPU and 1Gi for memory or storage. Requests must not exceed limits.</Text>
        {container === "helpers" && <Text size={300}>OpenClaw initialization can exceed 512Mi; 2Gi is the recommended starting memory limit per helper container.</Text>}
      </div>)}
      <Divider />
      <Text weight="semibold">Agent HOME</Text>
      <Field label="HOME StorageClass" required hint="Choose an approved StorageClass for Agent HOME.">
        <Select value={form.storage.home.storageClassName} required disabled={storageOptions.isFetching || storageOptions.isError || classes.length === 0}
          onChange={event => {
            const storageClassName = event.target.value;
            setForm(current => ({ ...current, storage: { ...current.storage, home: { ...current.storage.home, storageClassName } } }));
          }}>
          <option value="" disabled>Choose an approved StorageClass</option>
          {!selectedStorageClassIsApproved && form.storage.home.storageClassName && <option value={form.storage.home.storageClassName}>{form.storage.home.storageClassName}{storageOptions.data && !storageOptions.isError ? " (unavailable)" : ""}</option>}
          {classes.map((storageClass) => <option key={storageClass.name} value={storageClass.name}>{storageClass.name} ({storageClass.provisioner}{storageClass.allowVolumeExpansion ? ", expansion supported" : ""})</option>)}
        </Select>
      </Field>
      {storageOptions.isPending && <Spinner size="tiny" label="Loading approved StorageClasses..." />}
      {storageOptions.isError && <MessageBar intent="warning">Failed to load approved StorageClasses: {storageOptions.error.message}</MessageBar>}
      {!storageOptions.isPending && !storageOptions.isError && classes.length === 0 && <MessageBar intent="warning">No approved StorageClasses are available. Configure one before creating a Sandbox.</MessageBar>}
      {!storageOptions.isPending && !storageOptions.isError && form.storage.home.storageClassName && !selectedStorageClassIsApproved && <MessageBar intent="warning">The selected StorageClass is no longer available or approved. Choose another StorageClass before saving.</MessageBar>}
      <Button type="button" appearance="subtle" style={{ alignSelf: "flex-start" }} disabled={storageOptions.isFetching} onClick={() => { void storageOptions.refetch(); }}>{storageOptions.isError ? "Retry StorageClasses" : "Refresh StorageClasses"}</Button>
      <Field label="HOME size" required><Input value={form.storage.home.size} required onChange={(_, data) => setForm(current => ({ ...current, storage: { ...current.storage, home: { ...current.storage.home, size: data.value } } }))} /></Field>
      <Field label="HOME access mode" required hint="The selected runtime determines the supported access modes."><Select value={form.storage.home.accessMode} disabled={storageOptions.isFetching || storageOptions.isError || homeAccessModes.length === 0} onChange={event => {
        const accessMode = event.target.value as SandboxHomeAccessMode;
        setForm(current => ({ ...current, storage: { ...current.storage, home: { ...current.storage.home, accessMode } } }));
      }}>
        {!selectedAccessModeIsSupported && <option value={form.storage.home.accessMode}>{form.storage.home.accessMode}{storageOptions.data && !storageOptions.isError ? " (not supported by this runtime)" : ""}</option>}
        {homeAccessModes.map((accessMode) => <option key={accessMode} value={accessMode}>{accessMode}</option>)}
      </Select></Field>
      {!storageOptions.isPending && !storageOptions.isError && !selectedAccessModeIsSupported && <MessageBar intent="warning">The selected HOME access mode is not supported by the {form.type} runtime. Choose a supported mode before saving.</MessageBar>}
      <Field label="Scratch size limit" required><Input value={form.storage.scratch.sizeLimit} required onChange={(_, data) => setForm(current => ({ ...current, storage: { ...current.storage, scratch: { sizeLimit: data.value } } }))} /></Field>
      {submitError && <AdminConfigurationErrorMessage error={submitError} />}
      <div style={{ display: "flex", gap: 8 }}>
        <Button appearance="primary" type="submit" disabled={mutation.isPending || !!existing?.deleting || storageOptions.isFetching || storageOptions.isError || !selectedStorageClassIsApproved || !selectedAccessModeIsSupported}>{mutation.isPending ? "Saving..." : isCreate ? "Create" : "Save"}</Button>
        <Button type="button" onClick={() => navigate("/admin/sandboxes")}>Cancel</Button>
      </div>
    </form>
  );
}
