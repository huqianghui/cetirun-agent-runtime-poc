// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Badge,
  Button,
  Checkbox,
  Dialog,
  DialogActions,
  DialogBody,
  DialogContent,
  DialogSurface,
  DialogTitle,
  Field,
  Input,
  MessageBar,
  Spinner,
  Tab,
  TabList,
  Text,
  tokens,
  type SelectTabData,
} from "@fluentui/react-components";
import { ArrowLeft20Regular, Delete20Regular, Edit20Regular } from "@fluentui/react-icons";
import {
  deleteAdminInstance,
  getAdminInstance,
  getAdminInstanceStatus,
  updateAdminInstance,
  type AdminInstanceDetail,
  type AdminUpdateInstanceRequest,
} from "../../api/adminInstance";
import { PortalError } from "../../api/fetch";
import { EventsTab } from "../../components/EventsTab";
import { StatusTab } from "../../components/StatusTab";
import { GovernanceNotice } from "../../components/GovernanceNotice";
import { useFeatures } from "../../hooks/useFeatures";
import { agentTypeLabel } from "../../utils/instance";

interface Draft {
  displayName: string;
  image: string;
  port: string;
  webUI: boolean;
  model: string;
}

function fromDetail(detail: AdminInstanceDetail): Draft {
  return {
    displayName: detail.displayName || "",
    image: detail.agent.image || "",
    port: detail.agent.port?.toString() || "",
    webUI: detail.agent.webUI || false,
    model: detail.model || "",
  };
}

function phaseColor(phase?: string): "success" | "warning" | "danger" | "informative" {
  if (phase === "Ready") return "success";
  if (phase === "Failed") return "danger";
  if (phase === "Deleting" || phase === "Suspended") return "warning";
  return "informative";
}

function timestamp(value?: string): string {
  if (!value) return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

function DetailList({ detail }: { detail: AdminInstanceDetail }) {
  const entries: Array<[string, string | undefined]> = [
    ["Name", detail.name],
    ["Display name", detail.displayName || undefined],
    ["Owner", detail.email ? `${detail.alias} (${detail.email})` : detail.alias],
    ["Team", detail.team],
    ["Agent type", agentTypeLabel(detail.agent.type)],
    ["Agent username", detail.agent.username],
    ["Namespace", detail.namespace],
    ["Sandbox", detail.sandboxRef],
    ["Chart version", detail.chartVersion],
    ["Created", timestamp(detail.createdAt)],
    ["Last reconciled", timestamp(detail.lastReconciled)],
    ["Generation", String(detail.generation)],
    ["Observed generation", String(detail.observedGeneration)],
  ];
  return <dl style={{ display: "grid", gridTemplateColumns: "minmax(130px, auto) minmax(0, 1fr)", gap: "8px 16px", margin: 0 }}>
    {entries.map(([label, value]) => <div key={label} style={{ display: "contents" }}><dt style={{ color: tokens.colorNeutralForeground3 }}>{label}</dt><dd style={{ margin: 0, overflowWrap: "anywhere" }}>{value || "—"}</dd></div>)}
  </dl>;
}

function ConfigSummary({ detail }: { detail: AdminInstanceDetail }) {
  const entries: Array<[string, string]> = [
    ["Image", detail.agent.image || "Platform default"],
    ["Port", detail.agent.port?.toString() || "Platform default"],
    ["Web UI", detail.agent.webUI ? "Enabled" : "Disabled"],
    ["Model override", detail.model || "Platform default"],
    ["Tailscale", detail.tailscaleEnabled ? "Enabled" : "Disabled"],
    ["Agent identity", detail.agentIdentity?.enabled ? detail.agentIdentity.phase || "Enabled" : "Disabled"],
    ["Agent identity reason", detail.agentIdentity?.reason || "—"],
    ["Registry authentication", detail.registryAuthConfigured ? "Configured" : "Not configured"],
  ];
  return <dl style={{ display: "grid", gridTemplateColumns: "minmax(170px, auto) minmax(0, 1fr)", gap: "8px 16px", margin: 0 }}>
    {entries.map(([label, value]) => <div key={label} style={{ display: "contents" }}><dt style={{ color: tokens.colorNeutralForeground3 }}>{label}</dt><dd style={{ margin: 0, overflowWrap: "anywhere" }}>{value}</dd></div>)}
  </dl>;
}

export function InstanceDetailPage() {
  const { name } = useParams<{ name: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const activeName = useRef(name);
  const { data: features } = useFeatures();
  const [selectedTab, setSelectedTab] = useState("overview");
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [baseline, setBaseline] = useState<AdminInstanceDetail | null>(null);
  const [validationError, setValidationError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const [reviewNotice, setReviewNotice] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Pick<AdminInstanceDetail, "name" | "alias" | "team" | "uid" | "resourceVersion"> | null>(null);
  const [deleteName, setDeleteName] = useState("");
  const [deleting, setDeleting] = useState(false);
  const [deletionObserved, setDeletionObserved] = useState(false);
  const [deletionUID, setDeletionUID] = useState<string | null>(null);

  const detailQuery = useQuery({
    queryKey: ["admin", "instance", name],
    queryFn: () => getAdminInstance(name!),
    enabled: Boolean(name),
    retry: false,
    refetchInterval: (query) => {
      const current = query.state.data;
      return deleting || (current && (Boolean(current.deletionTimestamp) || current.generation > current.observedGeneration)) ? 5_000 : 30_000;
    },
  });
  const detail = detailQuery.data;
  const statusQuery = useQuery({
    queryKey: ["admin", "instance", name, "status", detail?.uid],
    queryFn: () => getAdminInstanceStatus(name!),
    enabled: Boolean(name && detail && selectedTab !== "overview"),
    refetchInterval: 10_000,
    retry: 1,
  });

  useEffect(() => {
    activeName.current = name;
    setEditing(false);
    setDraft(null);
    setBaseline(null);
    setValidationError(null);
    setConflict(false);
    setReviewNotice(null);
    setDeleteTarget(null);
    setDeleteName("");
    setDeleting(false);
    setDeletionObserved(false);
    setDeletionUID(null);
  }, [name]);
  useEffect(() => {
    if (detail?.deletionTimestamp) {
      setDeletionObserved(true);
      setDeletionUID((current) => current || detail.uid);
    }
  }, [detail?.deletionTimestamp, detail?.uid]);
  useEffect(() => {
    if (detailQuery.isLoading) return;
    if (detail === null) {
      navigate("/admin/instances", { replace: true, state: deletionObserved ? { deletedAgent: name } : { missingAgent: name } });
      return;
    }
    if (deletionObserved && deletionUID && detail && detail.uid !== deletionUID) {
      navigate("/admin/instances", { replace: true, state: { deletedAgent: name } });
    }
  }, [deletionObserved, deletionUID, detail, detailQuery.isLoading, name, navigate]);

  const editableType = Boolean(features?.allowedAgentTypes?.includes(detail?.agent.type || "custom"));
  const custom = detail?.agent.type === "custom";
  const pendingReconcile = Boolean(detail && detail.generation > detail.observedGeneration);
  const deletionInProgress = Boolean(detail?.deletionTimestamp) || (deleting && deletionUID === detail?.uid);

  const updateMutation = useMutation({
    mutationFn: ({ targetName, request }: { targetName: string; request: AdminUpdateInstanceRequest }) => updateAdminInstance(targetName, request),
    retry: false,
    onSuccess: (updated, variables) => {
      queryClient.setQueryData(["admin", "instance", updated.name], updated);
      queryClient.invalidateQueries({ queryKey: ["admin", "instances"] });
      if (variables.targetName !== activeName.current) return;
      setEditing(false);
      setDraft(null);
      setBaseline(null);
      setConflict(false);
    },
    onError: (error: Error) => {
      setConflict(error instanceof PortalError && error.status === 409 && error.code === "instance_changed");
      if (error instanceof PortalError && error.code === "instance_deleting") {
        setReviewNotice("Deletion has started. Refresh this page to review the latest lifecycle state.");
      }
    },
  });
  const deleteMutation = useMutation({
    mutationFn: ({ targetName, target }: { targetName: string; target: NonNullable<typeof deleteTarget> }) => deleteAdminInstance(targetName, { uid: target.uid, resourceVersion: target.resourceVersion }),
    retry: false,
    onSuccess: (_, variables) => {
      queryClient.invalidateQueries({ queryKey: ["admin", "instance", variables.targetName] });
      queryClient.invalidateQueries({ queryKey: ["admin", "instances"] });
      if (variables.targetName !== activeName.current) return;
      setDeleteTarget(null);
      setDeleting(true);
      setDeletionObserved(true);
      setDeletionUID(variables.target.uid);
    },
  });
  const deleteConflict = deleteMutation.error instanceof PortalError && deleteMutation.error.status === 409;
  const editBusy = updateMutation.isPending || (conflict && detailQuery.isFetching);
  const deleteBusy = deleteMutation.isPending || (deleteConflict && detailQuery.isFetching);
  const reloadDeletionConflict = async () => {
    const targetName = name;
    const refreshed = await detailQuery.refetch();
    if (targetName !== activeName.current || refreshed.isError || !refreshed.data) return;
    const current = refreshed.data;
    setDeleteTarget({ name: current.name, alias: current.alias, team: current.team, uid: current.uid, resourceVersion: current.resourceVersion });
    setDeleteName("");
    deleteMutation.reset();
  };

  const dirty = useMemo(() => {
    if (!draft || !baseline) return false;
    return JSON.stringify(draft) !== JSON.stringify(fromDetail(baseline));
  }, [draft, baseline]);

  const startEditing = () => {
    if (!detail || updateMutation.isPending || deletionInProgress) return;
    setSelectedTab("overview");
    updateMutation.reset();
    setBaseline(detail);
    setDraft(fromDetail(detail));
    setValidationError(null);
    setConflict(false);
    setReviewNotice(null);
    setEditing(true);
  };
  const cancelEditing = () => {
    setEditing(false);
    setDraft(null);
    setBaseline(null);
    setValidationError(null);
    setConflict(false);
    setReviewNotice(null);
  };
  const reloadAfterConflict = async () => {
    const refreshed = await detailQuery.refetch();
    if (refreshed.isError || !refreshed.data || !baseline || !draft) return;
    if (refreshed.data.uid !== baseline.uid) {
      cancelEditing();
      setReviewNotice("This Agent was replaced with a new object. The previous draft was discarded; review the new Agent before editing it.");
      return;
    }
    const previous = fromDetail(baseline);
    const latest = fromDetail(refreshed.data);
    const merged: Draft = {
      displayName: draft.displayName !== previous.displayName ? draft.displayName : latest.displayName,
      image: draft.image !== previous.image ? draft.image : latest.image,
      port: draft.port !== previous.port ? draft.port : latest.port,
      webUI: draft.webUI !== previous.webUI ? draft.webUI : latest.webUI,
      model: draft.model !== previous.model ? draft.model : latest.model,
    };
    setDraft(merged);
    setBaseline(refreshed.data);
    setConflict(false);
    setReviewNotice("The latest configuration is shown below. Your changed fields were retained; review them before saving.");
    updateMutation.reset();
  };
  const save = () => {
    if (!draft || !baseline || !detail || deletionInProgress || conflict) return;
    const port = draft.port.trim() === "" ? null : Number(draft.port);
    if (custom && port !== null && (!Number.isInteger(port) || port < 1 || port > 65535)) {
      setValidationError("HTTP port must be an integer between 1 and 65535, or left blank.");
      return;
    }
    if (custom && draft.webUI && port === null) {
      setValidationError("A port is required when Web UI is enabled.");
      return;
    }
    if (!draft.image.trim() && baseline.agent.image) {
      setValidationError("A pinned Agent image cannot be cleared.");
      return;
    }
    const request: AdminUpdateInstanceRequest = { uid: baseline.uid, resourceVersion: baseline.resourceVersion };
    if (draft.displayName !== (baseline.displayName || "")) request.displayName = draft.displayName.trim();
    if (draft.model !== (baseline.model || "")) request.model = draft.model.trim();
    const agent: NonNullable<AdminUpdateInstanceRequest["agent"]> = {};
    if (draft.image !== (baseline.agent.image || "")) agent.image = draft.image.trim() || null;
    if (custom && port !== (baseline.agent.port ?? null)) agent.port = port;
    if (custom && draft.webUI !== Boolean(baseline.agent.webUI)) agent.webUI = draft.webUI;
    if (Object.keys(agent).length) request.agent = agent;
    if (Object.keys(request).length === 2) { cancelEditing(); return; }
    setValidationError(null);
    updateMutation.mutate({ targetName: name!, request });
  };

  if (detailQuery.isLoading) return <Spinner label="Loading agent..." />;
  if (detailQuery.error && !detail) return <MessageBar intent="error">Failed to load: {detailQuery.error.message}</MessageBar>;
  if (!detail) return null;

  return <div style={{ maxWidth: 980 }}>
    {detailQuery.error && !editing && !deleteTarget && <MessageBar intent="error">Failed to refresh Agent: {detailQuery.error.message}. The last loaded configuration is shown.</MessageBar>}
    <Button appearance="subtle" icon={<ArrowLeft20Regular />} onClick={() => navigate("/admin/instances")} style={{ marginBottom: 12 }}>All agents</Button>
    <div style={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: 12, marginBottom: 16 }}>
      <Text as="h1" size={600} weight="semibold" style={{ margin: 0, overflowWrap: "anywhere" }}>{detail.displayName || detail.name}</Text>
      <Badge appearance="filled" color={phaseColor(detail.phase)}>{deletionInProgress ? "Deleting" : detail.phase || "Unknown"}</Badge>
      <Text size={200}>{detail.alias}{detail.team ? ` · ${detail.team}` : ""}</Text>
      <div style={{ marginLeft: "auto", display: "flex", gap: 8 }}>
        <Button appearance="secondary" icon={<Edit20Regular />} onClick={startEditing} disabled={deletionInProgress || editing || updateMutation.isPending}>Edit</Button>
        <Button appearance="primary" icon={<Delete20Regular />} onClick={() => { setDeleteName(""); deleteMutation.reset(); setDeleteTarget({ name: detail.name, alias: detail.alias, team: detail.team, uid: detail.uid, resourceVersion: detail.resourceVersion }); }} disabled={deletionInProgress} style={{ backgroundColor: tokens.colorStatusDangerBackground3 }}>Delete</Button>
      </div>
    </div>
    {!editableType && <MessageBar intent="info">Runtime configuration is unavailable because this Agent type is not enabled by the current platform policy. The display name can still be changed.</MessageBar>}
    {deletionInProgress && <MessageBar intent="warning">Deletion has been requested. This page will close once the Agent is removed.</MessageBar>}
    {pendingReconcile && !deleting && <MessageBar intent="info">Configuration saved; waiting for the Operator to apply generation {detail.generation}.</MessageBar>}
    {reviewNotice && !editing && <MessageBar intent="info">{reviewNotice}</MessageBar>}
    {!deletionInProgress && <GovernanceNotice instance={detail} />}

    <TabList selectedValue={selectedTab} onTabSelect={(_, data: SelectTabData) => setSelectedTab(data.value as string)} style={{ marginTop: 16, marginBottom: 16 }}>
      <Tab value="overview">Overview</Tab><Tab value="status">Status</Tab><Tab value="events">Events</Tab>
    </TabList>
    {selectedTab === "overview" && <div style={{ display: "flex", flexDirection: "column", gap: 24 }}>
      <section><Text as="h2" weight="semibold" size={400} block style={{ marginBottom: 12 }}>Agent details</Text><DetailList detail={detail} /></section>
      <section><Text as="h2" weight="semibold" size={400} block style={{ marginBottom: 12 }}>Configuration</Text><ConfigSummary detail={detail} /></section>
      {detail.conditions.length > 0 && <section><Text as="h2" weight="semibold" size={400} block style={{ marginBottom: 12 }}>Current conditions</Text>{detail.conditions.map((condition) => <div key={condition.type} style={{ marginBottom: 8 }}><Text weight="semibold">{condition.type}: </Text><Text>{condition.status}{condition.reason ? ` — ${condition.reason}` : ""}{condition.message ? `: ${condition.message}` : ""}</Text></div>)}</section>}
    </div>}
    {(selectedTab === "status" || selectedTab === "events") && <>
      {statusQuery.error && <MessageBar intent="error">Failed to load diagnostics: {statusQuery.error.message}</MessageBar>}
      {statusQuery.data?.uid === detail.uid && statusQuery.data.errors?.map((error) => <MessageBar key={error.section} intent="warning">{error.section}: {error.message}</MessageBar>)}
      {selectedTab === "status" ? <StatusTab data={statusQuery.data?.uid === detail.uid ? statusQuery.data : undefined} isLoading={statusQuery.isLoading} /> : <EventsTab data={statusQuery.data?.uid === detail.uid ? statusQuery.data : undefined} isLoading={statusQuery.isLoading} />}
    </>}

    <Dialog open={editing && draft !== null} onOpenChange={(_, data) => { if (!data.open && !editBusy) cancelEditing(); }}>
      <DialogSurface><DialogBody>
        <DialogTitle>Edit configuration</DialogTitle>
        <DialogContent style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <Text size={200}>{detail.name} · {detail.alias}{detail.team ? ` · ${detail.team}` : ""}</Text>
          {detailQuery.error && <MessageBar intent="error">Failed to refresh Agent: {detailQuery.error.message}. Your draft is retained.</MessageBar>}
          {reviewNotice && <MessageBar intent="info">{reviewNotice}</MessageBar>}
          {!editableType && <MessageBar intent="info">Runtime configuration is unavailable for this Agent type. The display name can still be changed.</MessageBar>}
          {draft && <>
            <Field label="Display name" hint="Leave blank to display the Agent name."><Input value={draft.displayName} disabled={deletionInProgress || editBusy} onChange={(_, data) => setDraft({ ...draft, displayName: data.value })} /></Field>
            <Field label="Agent image" hint="A pinned image cannot be cleared."><Input value={draft.image} disabled={!editableType || deletionInProgress || editBusy} onChange={(_, data) => setDraft({ ...draft, image: data.value })} maxLength={2048} /></Field>
            {custom ? <Field label={draft.webUI ? "Web UI port" : "HTTP port"} required={draft.webUI}><Input type="number" min={1} max={65535} value={draft.port} disabled={!editableType || deletionInProgress || editBusy} onChange={(_, data) => setDraft({ ...draft, port: data.value })} /></Field> : <Text size={200}>HTTP port: {draft.port || "Platform default"} (managed by the platform runtime).</Text>}
            {custom && <Checkbox label="Serve Web UI" checked={draft.webUI} disabled={!editableType || deletionInProgress || editBusy} onChange={(_, data) => setDraft({ ...draft, webUI: data.checked === true })} />}
            <Field label="Model override" hint="Leave blank to use the platform default."><Input value={draft.model} disabled={!editableType || deletionInProgress || editBusy} onChange={(_, data) => setDraft({ ...draft, model: data.value })} /></Field>
            <Text size={200}>Agent type, username, owner, Team, Sandbox and registry credentials are read-only.</Text>
            {validationError && <Text style={{ color: tokens.colorStatusDangerForeground1 }}>{validationError}</Text>}
            {conflict && <MessageBar intent="warning">This Agent changed since you opened it. Reload and review the current configuration; your draft is retained.</MessageBar>}
            {updateMutation.isError && !conflict && <MessageBar intent="error">{updateMutation.error.message}</MessageBar>}
            {deletionInProgress && <MessageBar intent="warning">This Agent is deleting, so edits cannot be saved.</MessageBar>}
          </>}
        </DialogContent>
        <DialogActions>
          <Button appearance="secondary" onClick={cancelEditing} disabled={editBusy}>Cancel</Button>
          {conflict ? <Button appearance="primary" onClick={() => void reloadAfterConflict()} disabled={detailQuery.isFetching}>Reload and review</Button> : <Button appearance="primary" onClick={save} disabled={!dirty || updateMutation.isPending || deletionInProgress}>{updateMutation.isPending ? "Saving..." : "Save"}</Button>}
        </DialogActions>
      </DialogBody></DialogSurface>
    </Dialog>

    <Dialog open={deleteTarget !== null} onOpenChange={(_, data) => { if (!data.open && !deleteBusy) setDeleteTarget(null); }}>
      <DialogSurface><DialogBody><DialogTitle>Delete Agent</DialogTitle><DialogContent>
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          <Text>Delete <strong>{deleteTarget?.name}</strong> owned by {deleteTarget?.alias}{deleteTarget?.team ? ` in Team ${deleteTarget.team}` : ""}? This begins resource cleanup and may delete persistent data.</Text>
          <Field label={`Type ${deleteTarget?.name || "the Agent name"} to confirm`} required><Input value={deleteName} disabled={deleteBusy} onChange={(_, data) => setDeleteName(data.value)} /></Field>
          {detailQuery.error && <MessageBar intent="error">Failed to refresh Agent: {detailQuery.error.message}. Reload before confirming deletion again.</MessageBar>}
          {deleteMutation.isError && <MessageBar intent="error">{deleteMutation.error.message}{deleteConflict ? " Reload and review the current owner and Team, then enter the Agent name again." : ""}</MessageBar>}
        </div>
      </DialogContent><DialogActions>
        <Button appearance="secondary" onClick={() => setDeleteTarget(null)} disabled={deleteBusy}>Cancel</Button>
        {deleteConflict ? <Button appearance="primary" onClick={() => void reloadDeletionConflict()} disabled={detailQuery.isFetching}>Reload and review</Button> : <Button appearance="primary" onClick={() => { if (deleteTarget) deleteMutation.mutate({ targetName: name!, target: deleteTarget }); }} disabled={deleteName !== deleteTarget?.name || deleteMutation.isPending || deletionInProgress} style={{ backgroundColor: tokens.colorStatusDangerBackground3 }}>{deleteMutation.isPending ? "Deleting..." : "Delete"}</Button>}
      </DialogActions></DialogBody></DialogSurface>
    </Dialog>
  </div>;
}
