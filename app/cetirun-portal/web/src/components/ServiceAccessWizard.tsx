// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Badge, Button, Checkbox, Dialog, DialogActions, DialogBody, DialogContent, DialogSurface, DialogTitle, Radio, RadioGroup, Spinner, Text, tokens } from "@fluentui/react-components";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { getGitHubAppConfig, listGitHubInstallations, reconnectConnection, selectGitHubInstallation, startConnection, type Connection, type ConnectionBinding, type ConnectionMode, type ConnectionProvider, type GitHubBinding } from "../api/toolAuthorization";
import type { Instance } from "../types/instance";
import { useConnectionBindings, useToolIdentity } from "../hooks/useToolAuthorization";
import { useConnectionSignIn } from "../hooks/useConnectionSignIn";
import { GitHubAccessFields, GitHubPermissionSummary, useSelectedRepositories, validGitHubAccess } from "./GitHubAccessFields";
import { AzureAccessInstructions, HowAccessWorks } from "./ServiceAccessDetails";
import { accessMethodLabel, connectionTitle, isHistoricalDuplicate, providerDisabledReasons, providerLabels, providerOrder, serviceLabels } from "./serviceAccess";

type Step = "service" | "account" | "access" | "review";
const defaultGitHubAccess = (): GitHubBinding => ({ repositoryIds: [], permissions: { contents: "read" } });

export function ServiceAccessWizard({ instance, connections, editing, canManage, onClose }: {
  instance: Instance;
  connections: Connection[];
  editing?: { connection: Connection; binding: ConnectionBinding };
  canManage: boolean;
  onClose: () => void;
}) {
  const client = useQueryClient();
  const api = useConnectionBindings(instance.instanceName, instance.uid, true);
  const identity = useToolIdentity(instance.instanceName, instance.uid);
  const signin = useConnectionSignIn(instance.uid);
  // Keep the draft and its original revision together, including after refetch/conflict.
  const [original] = useState(editing);
  const [step, setStep] = useState<Step>(original ? "access" : "service");
  const [provider, setProvider] = useState<ConnectionProvider | undefined>(original?.connection.provider);
  const [mode, setMode] = useState<ConnectionMode>(original?.connection.mode ?? "installation");
  const [connection, setConnection] = useState(original?.connection);
  const [choice, setChoice] = useState<string>();
  const [installationChoice, setInstallationChoice] = useState<number>();
  const [github, setGitHub] = useState<GitHubBinding>(original?.binding.github ?? defaultGitHubAccess);
  const [services, setServices] = useState(original?.binding.services ?? []);
  const [error, setError] = useState("");
  const body = useRef<HTMLDivElement>(null);
  const app = useQuery({ queryKey: ["github-app-config"], queryFn: ({ signal }) => getGitHubAppConfig(signal), enabled: provider === "github" && mode === "installation" });
  const needsInstallation = connection?.mode === "installation" && !connection.github?.installationId;
  const installations = useQuery({ queryKey: ["github-installations", connection?.id], queryFn: ({ signal }) => listGitHubInstallations(connection!.id, signal), enabled: !!needsInstallation && step === "account", refetchOnWindowFocus: "always" });
  const choose = (next: Connection) => {
    setError(""); setConnection(next); setServices(next.services); setGitHub(defaultGitHubAccess()); setInstallationChoice(undefined);
    setStep(next.mode === "installation" && !next.github?.installationId ? "account" : "access");
  };
  const selectedRepos = useSelectedRepositories(connection?.mode === "installation" ? connection : undefined, github);
  const active = new Set(api.bindings.data?.bindings.filter((binding) => binding.enabled).map((binding) => binding.connectionId));
  const connectionByID = new Map(connections.map((item) => [item.id, item]));
  const canonicalID = (id: string) => connectionByID.get(id)?.canonicalConnectionId || id;
  const activeCanonicalIDs = new Set([...active].map(canonicalID));
  const isActiveConnection = (id: string) => activeCanonicalIDs.has(canonicalID(id));
  const selectInstallation = useMutation({ mutationFn: (id: number) => selectGitHubInstallation(connection!.id, id), onSuccess: (updated) => {
    const sourceID = connection!.id;
    client.setQueryData<Connection[]>(["tool-connections"], (current) => {
      const withoutConsumedSource = (current ?? []).filter((item) => item.id !== sourceID);
      const existing = withoutConsumedSource.findIndex((item) => item.id === updated.id);
      if (existing < 0) return [...withoutConsumedSource, updated];
      return withoutConsumedSource.map((item, index) => index === existing ? updated : item);
    });
    void client.invalidateQueries({ queryKey: ["tool-connections"] });
    choose(updated);
    if (!original && isActiveConnection(updated.id)) setError("This agent already has access through this connection. Close this dialog and edit that access.");
  } });
  const busy = api.create.isPending || api.update.isPending || identity.isPending || selectInstallation.isPending;
  const disabled = busy || !canManage;
  const candidates = connections.filter((item) => item.provider === provider && item.mode === mode &&
    (item.status === "connected" || item.status === "interaction_required" || item.status === "installation_required") &&
    !isHistoricalDuplicate(item) && !isActiveConnection(item.id) &&
    (mode !== "agent" || item.identity?.instanceUid === instance.uid));
  const selectedId = choice ?? (candidates.length === 1 ? candidates[0].id : undefined);
  const selectedAccount = candidates.find((item) => item.id === selectedId);
  const installed = installations.data?.installations ?? [];
  const selectedInstallation = installationChoice ?? (installed.length === 1 ? installed[0].id : undefined);
  const appUnavailable = provider === "github" && mode === "installation" && (app.isLoading || app.isError || !app.data?.enabled);
  const liveConnection = connections.find((item) => item.id === connection?.id);
  const grantValid = !!connection && liveConnection?.status === "connected" && !providerDisabledReasons[connection.provider] &&
    (original || !isActiveConnection(connection.id)) && !appUnavailable &&
    (connection.mode === "installation" ? validGitHubAccess(github) && !!selectedRepos.data && !selectedRepos.isFetching && !selectedRepos.isError && selectedRepos.data.every((item) => item.repository?.admin) : services.length > 0);

  useEffect(() => { body.current?.querySelector<HTMLElement>("[data-step-heading]")?.focus(); }, [step, provider, mode, needsInstallation]);
  useEffect(() => {
    if (!needsInstallation || step !== "account") return;
    const refresh = () => { if (document.visibilityState === "visible") void installations.refetch(); };
    document.addEventListener("visibilitychange", refresh);
    return () => document.removeEventListener("visibilitychange", refresh);
  }, [needsInstallation, step, installations.refetch]);

  const close = () => { if (!busy) { signin.cancel(); onClose(); } };
  const changeMode = (next: ConnectionMode) => {
    signin.cancel(); setMode(next); setChoice(undefined); setConnection(undefined); setError("");
  };
  const changeProvider = (next: ConnectionProvider) => {
    setProvider(next); changeMode(next === "github" ? "installation" : next === "azure" ? "agent" : "delegated"); setStep("account");
  };
  const continueAccount = () => {
    if (disabled || signin.pending || appUnavailable) return;
    setError("");
    if (needsInstallation) {
      if (installed.some((item) => item.id === selectedInstallation) && !installations.isError && !installations.isFetching) selectInstallation.mutate(selectedInstallation!);
    } else if (selectedAccount) {
      if (selectedAccount.status === "interaction_required") signin.begin(() => reconnectConnection(selectedAccount.id), choose, selectedAccount.mode);
      else choose(selectedAccount);
    } else if (provider === "azure" && mode === "agent") {
      const existing = connections.find((item) => item.provider === "azure" && item.mode === "agent" && item.identity?.instanceUid === instance.uid);
      if (existing) { setError("This agent identity is already connected or unavailable. Review its existing access."); return; }
      identity.mutate({ provider: "azure" }, { onSuccess: choose, onError: (reason) => setError(reason.message) });
    }
  };
  const back = () => {
    signin.cancel(); setError(""); selectInstallation.reset();
    if (step === "review") setStep("access");
    else if (step === "access") { setConnection(undefined); setStep("account"); }
    else if (needsInstallation) setConnection(undefined);
    else { setProvider(undefined); setStep("service"); }
  };
  const save = () => {
    if (disabled || !grantValid || !connection) return;
    setError("");
    const input = { connectionId: connection.id, services: connection.mode === "installation" ? ["github"] : services, enabled: true, ...(connection.mode === "installation" ? { github } : {}) };
    const callbacks = { onSuccess: close, onError: (reason: Error) => setError(reason.message) };
    // Success closes directly: the mutation's pending state still includes onSuccess.
    callbacks.onSuccess = () => { signin.cancel(); onClose(); };
    if (original) api.update.mutate({ id: original.binding.id, input: { ...input, revision: original.binding.revision } }, callbacks);
    else api.create.mutate(input, callbacks);
  };
  const title = step === "service" ? "Add identity" : step === "account" ? `${providerLabels[provider!]} account` : step === "access" ? (mode === "installation" ? "Repositories & permissions" : "Access settings") : "Review access";
  const nextAccountDisabled = disabled || signin.pending || appUnavailable || (needsInstallation ? installations.isError || installations.isFetching || !installed.some((item) => item.id === selectedInstallation) : !selectedAccount && !(provider === "azure" && mode === "agent"));
  const actionError = error || selectInstallation.error?.message;
  const saving = api.create.isPending || api.update.isPending;

  return <Dialog open onOpenChange={(_, data) => { if (!data.open) close(); }}><DialogSurface style={{ width: "min(680px, calc(100vw - 32px))", maxWidth: 680 }}><DialogBody ref={body}>
    <DialogTitle tabIndex={-1} data-step-heading>{title}</DialogTitle>
    <DialogContent style={{ overflowWrap: "anywhere" }}>
      <Text size={200} block>For {instance.displayName || instance.instanceName}</Text>
      {step !== "service" && <ol aria-label="Access setup steps" style={{ display: "flex", flexWrap: "wrap", gap: 24, paddingLeft: 20, color: tokens.colorNeutralForeground2 }}>
        {["account", "access", "review"].map((item, index) => <li key={item} aria-current={step === item ? "step" : undefined} style={{ fontWeight: step === item ? 600 : 400 }}>{["Account", mode === "installation" ? "Repositories & permissions" : "Access", "Review"][index]}</li>)}
      </ol>}
      {step === "service" && <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(min(100%, 200px), 1fr))", gap: 12, marginTop: 20 }}>
        {providerOrder.map((item) => <div key={item}><Button appearance="secondary" style={{ width: "100%", minHeight: 52 }} disabled={disabled || !!providerDisabledReasons[item]} onClick={() => changeProvider(item)}>{providerLabels[item]}</Button>{providerDisabledReasons[item] && <Text size={200} block>{providerDisabledReasons[item]}</Text>}</div>)}
      </div>}
      {step === "account" && <>
        <Text weight="semibold" block>{mode === "installation" ? "Selected repositories" : mode === "agent" ? "Agent identity" : "Personal account"} {mode !== "delegated" && <Badge appearance="tint">Recommended</Badge>}</Text>
        <Text block size={200}>{mode === "installation" ? "Choose repositories and set separate permissions for this agent." : mode === "agent" ? "Give this agent its own Azure identity. Resource owners grant access through Azure RBAC." : "Access depends on your account and the permissions authorized with the provider."}</Text>
        {provider === "github" && mode === "delegated" && <Text block>CetiRun cannot restrict personal account access to selected repositories.</Text>}
        {appUnavailable && <div style={{ marginTop: 12 }}>
          {app.isLoading ? <Spinner size="tiny" label="Checking GitHub availability..." /> : app.isError ? <><Text role="alert" block>Could not check GitHub App availability.</Text><Button onClick={() => void app.refetch()}>Retry availability</Button></> : <Text block>Selected repository access is not configured in this environment. Contact your platform administrator.</Text>}
        </div>}
        {!appUnavailable && (needsInstallation ? <>
          <Text block style={{ marginTop: 16 }}>Signed in as {connection.github?.userLogin}. Choose a personal account or organization.</Text>
          {installations.isLoading && <Spinner size="tiny" label="Loading GitHub organizations..." />}
          {installations.error && <Text role="alert" block>Could not load GitHub access. Try refreshing, or sign in again.</Text>}
          <RadioGroup aria-label="GitHub account or organization" value={selectedInstallation?.toString() ?? ""} onChange={(_, data) => setInstallationChoice(Number(data.value))}>
            {installed.map((item) => <Radio key={item.id} value={String(item.id)} disabled={disabled} label={`${item.accountLogin} (${item.accountType === "Organization" ? "Organization" : item.accountType === "User" ? "Personal account" : "GitHub account"})`} />)}
          </RadioGroup>
          {!installations.isLoading && !installations.isError && !installed.length && <Text block>No GitHub access is available yet. Set up the App for your repositories. Your organization may require administrator approval.</Text>}
          <div style={{ display: "flex", flexWrap: "wrap", gap: 8, marginTop: 12 }}>
            {(installations.data?.installUrl || app.data?.installUrl) && <Button as="a" href={installations.data?.installUrl || app.data?.installUrl} target="_blank" rel="noreferrer">Set up GitHub access</Button>}
            <Button disabled={disabled || installations.isFetching} onClick={() => void installations.refetch()}>Refresh access</Button>
            {installations.isError && <Button disabled={disabled || signin.pending} onClick={() => signin.begin(() => reconnectConnection(connection.id), choose, "installation")}>Sign in again</Button>}
          </div>
        </> : <>
          {!!candidates.length && <RadioGroup aria-label="Available accounts" value={selectedId ?? ""} onChange={(_, data) => setChoice(data.value)}>
            {candidates.map((item) => <Radio key={item.id} value={item.id} disabled={disabled || signin.pending} label={`${connectionTitle(item)}${item.mode === "installation" ? ` · signed in as ${item.github?.userLogin}` : ""}${item.status === "interaction_required" ? " · Sign-in required" : item.status === "installation_required" ? " · Setup incomplete" : ""}`} />)}
          </RadioGroup>}
          {mode !== "agent" && <Button style={{ marginTop: 12 }} disabled={disabled || signin.pending} onClick={() => signin.begin(() => startConnection(provider!, mode === "installation" ? "installation" : undefined), choose, mode)}>{candidates.length ? "Sign in with another account" : `Sign in to ${providerLabels[provider!]}`}</Button>}
        </>)}
        {(provider === "github" || provider === "azure") && <details style={{ marginTop: 20 }}><summary>Other access methods</summary>
          {mode === "delegated" ? <Button disabled={disabled || signin.pending} onClick={() => changeMode(provider === "github" ? "installation" : "agent")}>{provider === "github" ? "Use selected repositories" : "Use agent identity"}</Button> : <><Text size={200} block>{provider === "github" ? "Personal account access cannot be limited to selected repositories in CetiRun." : "Use an existing personal Azure account and its provider permissions."}</Text><Button disabled={disabled || signin.pending} onClick={() => changeMode("delegated")}>Personal account</Button></>}
        </details>}
      </>}
      {connection && (step === "access" || step === "review") && <>
        <Text weight="semibold" block style={{ marginTop: 12 }}>{providerLabels[connection.provider]} — {connectionTitle(connection)}</Text>
        <Badge appearance="outline">{accessMethodLabel(connection)}</Badge>
        {step === "access" ? connection.mode === "installation" ? <GitHubAccessFields key={connection.id} connection={connection} value={github} onChange={setGitHub} disabled={disabled} /> : <>
          <Text block style={{ marginTop: 16 }}>Choose the services this agent can use.</Text>
          {connection.services.map((id) => <Checkbox key={id} label={serviceLabels[id] ?? id} disabled={disabled} checked={services.includes(id)} onChange={(_, data) => setServices(data.checked === true ? [...new Set([...services, id])] : services.filter((item) => item !== id))} />)}
          {connection.provider === "github" && <Text block>Access follows your personal account and OAuth authorization. No per-repository restrictions apply in CetiRun.</Text>}
        </> : <>
          <Text block style={{ marginTop: 16 }}>{original ? "Save these permissions for" : "Allow access for"} {instance.displayName || instance.instanceName}.</Text>
          {connection.mode === "installation" ? <>
            <GitHubPermissionSummary value={github} />
            {selectedRepos.isLoading && <Spinner size="tiny" label="Checking selected repositories..." />}
            {selectedRepos.data && <ul>{selectedRepos.data.map(({ id, repository }) => <li key={id}>{repository?.fullName ?? "Unavailable repository"}{repository && !repository.admin ? " (repository admin required)" : ""}</li>)}</ul>}
            {selectedRepos.data?.some((item) => !item.repository?.admin) && <Text role="alert" block>Go back and remove repositories that are unavailable or require administrator permission.</Text>}
            {selectedRepos.error && <><Text role="alert" block>Could not check selected repositories.</Text><Button onClick={() => void selectedRepos.refetch()}>Retry selected repositories</Button></>}
          </> : <><Text block>Services: {services.map((id) => serviceLabels[id] ?? id).join(", ")}</Text><Text block>{connection.mode === "agent" ? "Access is managed through Azure RBAC and has not been verified." : "Access follows the permissions authorized with the provider."}</Text>{connection.provider === "github" && <Text block>No per-repository restrictions apply to this personal account connection.</Text>}</>}
          {connection.provider === "github" && api.bindings.data?.bindings.some((item) => item.enabled && item.id !== original?.binding.id && connections.some((other) => other.id === item.connectionId && other.provider === "github" && other.mode === "delegated")) && <Text block>This agent also has personal GitHub access. Its existing permissions remain available until you remove that access.</Text>}
        </>}
        {connection.mode === "agent" && <AzureAccessInstructions connection={connection} />}
        <HowAccessWorks connection={connection} />
        {liveConnection?.status !== "connected" && <Text role="alert" block>This authorization is no longer connected. Close this dialog and sign in again.</Text>}
      </>}
      {signin.pending && <Spinner size="tiny" label="Waiting for sign-in..." />}
      {signin.message && <Text role="status" block>{signin.message}</Text>}
      {!canManage && <Text role="alert" block>Access cannot be changed right now. Close this dialog and refresh the agent.</Text>}
    </DialogContent>
    <DialogActions fluid style={{ flexDirection: "column", alignItems: "stretch" }}>
      {(actionError || saving) && <div role="group" aria-label="Access action feedback">
        {actionError && <Text role="alert" aria-live="assertive" block>{actionError}</Text>}
        {saving && <Text role="status" aria-live="polite" block>Saving access...</Text>}
      </div>}
      <div style={{ display: "flex", flexWrap: "wrap", justifyContent: "flex-end", gap: 8 }}>
        <Button disabled={busy} onClick={close}>Cancel</Button>
        {step !== "service" && !(original && step === "access") && <Button disabled={busy} onClick={back}>Back</Button>}
        {step === "account" && <Button appearance="primary" disabled={nextAccountDisabled} onClick={continueAccount}>Continue</Button>}
        {step === "access" && <Button appearance="primary" disabled={disabled || !grantValid} onClick={() => { setError(""); setStep("review"); }}>Review access</Button>}
        {step === "review" && <Button appearance="primary" disabled={disabled || !grantValid} onClick={save}>{original ? "Save access" : "Allow access"}</Button>}
      </div>
    </DialogActions>
  </DialogBody></DialogSurface></Dialog>;
}
