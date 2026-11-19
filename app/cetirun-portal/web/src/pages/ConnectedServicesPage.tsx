// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import {
  Badge,
  Button,
  Card,
  Dialog,
  DialogActions,
  DialogBody,
  DialogContent,
  DialogSurface,
  DialogTitle,
  DialogTrigger,
  MessageBar,
  Spinner,
  Text,
  Title1,
} from "@fluentui/react-components";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import {
  deleteConnection,
  getConnectionUsage,
  reconnectConnection,
  type Connection,
  type ConnectionProvider,
} from "../api/toolAuthorization";
import { HowAccessWorks, TechnicalDetails } from "../components/ServiceAccessDetails";
import {
  accessMethodLabel,
  connectionStatusColor,
  connectionStatusLabel,
  connectionTitle,
  isHistoricalDuplicate,
  providerDisabledReasons,
  providerLabels,
  providerOrder,
} from "../components/serviceAccess";
import { ForbiddenPage } from "./ForbiddenPage";
import { useConnectionSignIn } from "../hooks/useConnectionSignIn";
import { useFeatures } from "../hooks/useFeatures";
import { useToolConnections } from "../hooks/useToolAuthorization";

function agentLink(name: string, uid: string) {
  if (!name) return <Text key={uid}>{uid}</Text>;
  return <Link key={uid} to={`/instances/${encodeURIComponent(name)}/overview#service-access`}>{name}</Link>;
}

function ConnectionCard({ connection, historicalDuplicate = false }: { connection: Connection; historicalDuplicate?: boolean }) {
  const client = useQueryClient();
  const navigate = useNavigate();
  const [confirmingRemoval, setConfirmingRemoval] = useState(false);
  const usage = useQuery({
    queryKey: ["connection-usage", connection.id],
    queryFn: ({ signal }) => getConnectionUsage(connection.id, signal),
  });
  const signin = useConnectionSignIn(connection.id);
  const remove = useMutation({
    mutationFn: () => deleteConnection(connection.id),
    onSuccess: () => {
      setConfirmingRemoval(false);
      void client.invalidateQueries({ queryKey: ["tool-connections"] });
    },
    onError: () => {
      setConfirmingRemoval(false);
      void usage.refetch();
    },
  });
  const disabled = Boolean(providerDisabledReasons[connection.provider]);
  const setupIncomplete = ["connecting", "pending", "installation_required"].includes(connection.status);
  const canRemove = usage.data?.canRemove === true && !usage.isError && !usage.isFetching;
  // Revoked GitHub tokens and partial Google consent can still be marked connected.
  const canReauthorize = connection.status === "interaction_required"
    || ((connection.provider === "github" || connection.provider === "google") && connection.mode === "delegated" && connection.status === "connected");
  const usageSummary = usage.isLoading
    ? "Checking Agent usage..."
    : usage.isError
      ? "Agent usage unavailable"
      : usage.data
        ? `Used by ${usage.data.agents.length} agent${usage.data.agents.length === 1 ? "" : "s"}`
        : "Checking Agent usage...";
  const removalNotice = connection.mode === "installation"
    ? "Removing this connection does not remove the GitHub App installation or its permissions."
    : "Removing this connection does not remove the provider grant or its permissions.";

  return (
    <Card style={{ padding: 16, minWidth: 0, overflowWrap: "anywhere" }}>
      <div style={{ display: "flex", alignItems: "flex-start", justifyContent: "space-between", gap: 12, flexWrap: "wrap" }}>
        <div>
          <Text weight="semibold" size={400} block>{connectionTitle(connection)}</Text>
          <Text size={200} block>{accessMethodLabel(connection)}</Text>
        </div>
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
          {historicalDuplicate && <Badge appearance="tint">Historical duplicate</Badge>}
          <Badge appearance="filled" color={connectionStatusColor(connection)}>{connectionStatusLabel(connection)}</Badge>
        </div>
      </div>

      {connection.mode === "installation" ? <>
        <Text size={200} block style={{ marginTop: 12 }}>Signed in as {connection.github?.userLogin || "Not reported"}</Text>
        <Text size={200} block>GitHub actions run as {connection.accountDisplay || "the GitHub App bot"}</Text>
      </> : <Text size={200} block style={{ marginTop: 12 }}>Signed in as {connection.accountDisplay || "Not reported"}</Text>}

      {historicalDuplicate && <Text size={200} block style={{ marginTop: 12 }}>This older {accessMethodLabel(connection).toLowerCase()} connection is retained for existing Agent access and cleanup. New Agent access uses the current connection.</Text>}

      {disabled && <MessageBar intent="warning" style={{ marginTop: 12 }}>{providerLabels[connection.provider]}: {providerDisabledReasons[connection.provider]}</MessageBar>}
      {signin.message && <Text role="status" block style={{ marginTop: 12 }}>{signin.message}</Text>}
      {remove.error && <Text role="alert" block style={{ marginTop: 12 }}>{remove.error.message}</Text>}
      {usage.error && <Text role="alert" block style={{ marginTop: 12 }}>Could not check which Agents use this connection.</Text>}

      <details style={{ marginTop: 12 }}>
        <summary>{usageSummary}</summary>
        {usage.isLoading && <Spinner size="tiny" label="Checking agent usage..." style={{ marginTop: 8 }} />}
        {usage.error && <div style={{ marginTop: 8 }}><Button size="small" onClick={() => void usage.refetch()}>Retry</Button></div>}
        {usage.data && (usage.data.agents.length ? <div style={{ display: "flex", flexDirection: "column", gap: 4, marginTop: 8 }}>
          {usage.data.agents.map((agent) => agentLink(agent.instanceName, agent.instanceUid))}
        </div> : <Text size={200} block style={{ marginTop: 8 }}>No Agents use this connection.</Text>)}
      </details>
      {usage.data && usage.data.agents.length > 0 && <Text size={200} block style={{ marginTop: 8 }}>Remove access from these agents before removing this connection.</Text>}

      <HowAccessWorks connection={connection} />
      <TechnicalDetails connection={connection} />

      <div style={{ display: "flex", gap: 8, flexWrap: "wrap", marginTop: 16 }}>
        {canReauthorize && <Button disabled={disabled || signin.pending || remove.isPending} onClick={() => signin.begin(() => reconnectConnection(connection.id), () => {}, connection.mode)}>Sign in again</Button>}
        {setupIncomplete && <Button onClick={() => navigate("/")}>Finish setup from an Agent</Button>}
        <Dialog modalType="alert" open={confirmingRemoval} onOpenChange={(_, data) => { if (!remove.isPending) setConfirmingRemoval(data.open); }}>
          <DialogTrigger disableButtonEnhancement><Button disabled={!canRemove || signin.pending || remove.isPending}>Remove connection</Button></DialogTrigger>
          <DialogSurface><DialogBody>
            <DialogTitle>Remove {connectionTitle(connection)} from CetiRun?</DialogTitle>
            <DialogContent><Text block>{removalNotice}</Text></DialogContent>
            <DialogActions>
              <DialogTrigger disableButtonEnhancement><Button disabled={remove.isPending}>Cancel</Button></DialogTrigger>
              <Button appearance="primary" disabled={remove.isPending || !canRemove} onClick={() => remove.mutate()}>Remove connection permanently</Button>
            </DialogActions>
          </DialogBody></DialogSurface>
        </Dialog>
      </div>
    </Card>
  );
}

function ProviderGroup({ provider, connections }: { provider: ConnectionProvider; connections: Connection[] }) {
  if (!connections.length) return null;
  const current = connections.filter((connection) => !isHistoricalDuplicate(connection));
  const historicalDuplicates = connections.filter(isHistoricalDuplicate);
  return <section aria-labelledby={`${provider}-connections`} style={{ marginTop: 28 }}>
    <Text id={`${provider}-connections`} as="h2" size={500} weight="semibold">{providerLabels[provider]}</Text>
    {!!current.length && <div style={{ display: "grid", gap: 12, gridTemplateColumns: "repeat(auto-fit, minmax(min(100%, 280px), 1fr))", marginTop: 12 }}>
      {current.map((connection) => <ConnectionCard key={connection.id} connection={connection} />)}
    </div>}
    {!!historicalDuplicates.length && <section aria-label="Historical duplicate connections" style={{ marginTop: current.length ? 20 : 12 }}>
      <Text as="h3" size={300} weight="semibold">Historical duplicate connections</Text>
      <Text size={200} block style={{ marginTop: 4 }}>These records are kept for existing Agent bindings. Review usage before removing an unused connection.</Text>
      <div style={{ display: "grid", gap: 12, gridTemplateColumns: "repeat(auto-fit, minmax(min(100%, 280px), 1fr))", marginTop: 12 }}>
        {historicalDuplicates.map((connection) => <ConnectionCard key={connection.id} connection={connection} historicalDuplicate />)}
      </div>
    </section>}
  </section>;
}

export function ConnectedServicesPage() {
  const features = useFeatures();
  const enabled = features.data?.agentToolsEnabled === true;
  const { connections } = useToolConnections(enabled);
  if (features.isLoading) return <div style={{ flex: 1, padding: 24 }}><Spinner label="Loading connections..." /></div>;
  if (!enabled) return <ForbiddenPage message="Connections are not enabled." />;

  const usableConnections = (connections.data ?? []).filter((connection) => connection.mode === "delegated" || connection.mode === "installation");
  return <main style={{ flex: 1, minHeight: 0, overflowY: "auto", padding: 24 }}>
    <Title1>Connections</Title1>
    <Text block style={{ marginTop: 8 }}>Manage external accounts and authorizations for your agents. To add a connection or give an agent access, open that agent and configure Identities.</Text>
    {connections.isLoading && <Spinner label="Loading connections..." style={{ marginTop: 24 }} />}
    {connections.error && <MessageBar intent="error" style={{ marginTop: 24 }}>Could not load connections: {connections.error.message}<Button appearance="transparent" onClick={() => void connections.refetch()}>Retry</Button></MessageBar>}
    {!connections.isLoading && !connections.error && !usableConnections.length && <MessageBar intent="info" style={{ marginTop: 24 }}>No connections yet. <Link to="/">View agents</Link> and select Add identity to connect an external account.</MessageBar>}
    {!connections.isLoading && !connections.error && providerOrder.map((provider) => <ProviderGroup key={provider} provider={provider} connections={usableConnections.filter((connection) => connection.provider === provider)} />)}
  </main>;
}
