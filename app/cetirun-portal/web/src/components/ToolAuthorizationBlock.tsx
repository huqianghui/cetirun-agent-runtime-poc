// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Badge, Button, Menu, MenuItem, MenuList, MenuPopover, MenuTrigger, Spinner, Text, tokens } from "@fluentui/react-components";
import { MoreHorizontal20Regular } from "@fluentui/react-icons";
import { useEffect, useRef, useState } from "react";
import type { Connection, ConnectionBinding, ConnectionProvider } from "../api/toolAuthorization";
import { reconnectConnection } from "../api/toolAuthorization";
import type { Instance } from "../types/instance";
import { useToolConnections, useConnectionBindings } from "../hooks/useToolAuthorization";
import { useConnectionSignIn } from "../hooks/useConnectionSignIn";
import { ServiceAccessWizard } from "./ServiceAccessWizard";
import { GitHubPermissionSummary } from "./GitHubAccessFields";
import { AzureAccessInstructions, HowAccessWorks, TechnicalDetails } from "./ServiceAccessDetails";
import { accessMethodLabel, connectionStatusColor, connectionStatusLabel, connectionTitle, providerDisabledReasons, providerLabels, providerOrder, serviceLabels } from "./serviceAccess";

export function ToolAuthorizationBlock({ instance }: { instance: Instance }) {
  return <AgentServiceAccess key={instance.uid} instance={instance} />;
}

function AgentServiceAccess({ instance }: { instance: Instance }) {
  const accounts = useToolConnections(true);
  const api = useConnectionBindings(instance.instanceName, instance.uid, true);
  const signin = useConnectionSignIn(instance.uid);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<{ connection: Connection; binding: ConnectionBinding }>();
  const wizardTrigger = useRef<HTMLButtonElement | null>(null);
  const section = useRef<HTMLElement | null>(null);
  const followedAnchor = useRef(false);
  const connections = (accounts.connections.data ?? []).filter((item) => item.mode !== "agent" || item.identity?.instanceUid === instance.uid);
  const bindings = (api.bindings.data?.bindings ?? []).filter((item) => item.enabled && item.instanceUid === instance.uid);
  const loading = accounts.connections.isLoading || api.bindings.isLoading;
  const error = accounts.connections.error ?? api.bindings.error ?? api.remove.error;
  const canManage = !loading && !accounts.connections.isError && !api.bindings.isError && api.bindings.data?.readOnly === false;
  useEffect(() => {
    if (!loading && !followedAnchor.current && window.location.hash === "#service-access") {
      followedAnchor.current = true;
      section.current?.scrollIntoView({ block: "start" });
      section.current?.focus({ preventScroll: true });
    }
  }, [loading]);
  const groups: (ConnectionProvider | "unavailable")[] = [...providerOrder, "unavailable"];
  const retry = () => { api.remove.reset(); void accounts.connections.refetch(); void api.bindings.refetch(); };
  const closeWizard = () => {
    setAdding(false); setEditing(undefined);
    requestAnimationFrame(() => wizardTrigger.current?.focus());
  };
  return <section ref={section} id="service-access" tabIndex={-1} aria-label="Identities" style={{ marginTop: 16, padding: 20, borderRadius: tokens.borderRadiusLarge, border: `1px solid ${tokens.colorNeutralStroke1}`, overflowWrap: "anywhere" }}>
    <div style={{ display: "flex", flexWrap: "wrap", alignItems: "center", justifyContent: "space-between", gap: 12 }}>
      <Text as="h2" size={400} weight="semibold" style={{ margin: 0 }}>Identities</Text>
      <Button appearance="primary" disabled={!canManage} onClick={(event) => { wizardTrigger.current = event.currentTarget; setAdding(true); }}>Add identity</Button>
    </div>
    <Text size={200} block style={{ marginTop: 8 }}>Manage the identities and permissions this agent uses to access external services.</Text>
    {loading && <Spinner size="tiny" label="Loading identities..." />}
    {error && <div role="alert"><Text block>{error.message}</Text><Button onClick={retry}>Retry</Button></div>}
    {signin.message && <Text role="status" block>{signin.message}</Text>}
    {groups.map((provider) => {
      const rows = bindings.flatMap((binding) => {
        const connection = connections.find((item) => item.id === binding.connectionId);
        return (connection?.provider ?? "unavailable") === provider ? [{ binding, connection }] : [];
      });
      if (!rows.length) return null;
      const mixedGitHub = provider === "github" && rows.some((row) => row.connection?.mode === "installation") && rows.some((row) => row.connection?.mode === "delegated");
      return <section key={provider} aria-label={provider === "unavailable" ? "Unavailable service" : providerLabels[provider]} style={{ marginTop: 24 }}>
        <Text as="h3" weight="semibold" style={{ margin: "0 0 8px" }}>{provider === "unavailable" ? "Unavailable service" : providerLabels[provider]}</Text>
        {mixedGitHub && <Text block style={{ marginBottom: 12 }}>Personal GitHub access is also enabled. Its permissions remain available independently of the selected repository limits.</Text>}
        {rows.map(({ binding, connection }) => <article key={binding.id} aria-label={connection ? `${connectionTitle(connection)} — ${accessMethodLabel(connection)}` : "Unavailable connection"} style={{ padding: 16, marginTop: 8, borderRadius: tokens.borderRadiusMedium, background: tokens.colorNeutralBackground2 }}>
          <div style={{ display: "flex", flexWrap: "wrap", gap: 8, alignItems: "center" }}>
            <Text weight="semibold">{connection ? connectionTitle(connection) : "Unavailable connection"}</Text>
            {connection && <Badge appearance="outline">{accessMethodLabel(connection)}</Badge>}
            <Badge appearance="tint" color={connectionStatusColor(connection)}>{connection ? connectionStatusLabel(connection) : "Unavailable"}</Badge>
          </div>
          {connection?.mode === "installation" && <Text size={200} block>Acts as {connection.accountDisplay || "the GitHub App bot"}</Text>}
          {binding.github ? <GitHubPermissionSummary value={binding.github} /> : <Text size={200} block>{binding.services.map((id) => serviceLabels[id] ?? id).join(" · ")}</Text>}
          {connection?.provider === "github" && connection.mode === "delegated" && <Text size={200} block>Uses personal account permissions. No per-repository restrictions in CetiRun.</Text>}
          {connection && providerDisabledReasons[connection.provider] && <Text size={200} block>{providerDisabledReasons[connection.provider]}</Text>}
          {connection && <><HowAccessWorks connection={connection} />{connection.mode === "agent" && <AzureAccessInstructions connection={connection} />}<TechnicalDetails connection={connection} /></>}
          <div style={{ display: "flex", flexWrap: "wrap", gap: 8, marginTop: 12 }}>
            {connection && <Button disabled={!canManage || connection.status !== "connected" || !!providerDisabledReasons[connection.provider]} onClick={(event) => { wizardTrigger.current = event.currentTarget; setEditing({ connection, binding }); }}>Manage access</Button>}
            {connection?.status === "interaction_required" && connection.mode !== "agent" && <Button disabled={!canManage || !!providerDisabledReasons[connection.provider] || signin.pending} onClick={() => signin.begin(() => reconnectConnection(connection.id), () => {}, connection.mode)}>Sign in again</Button>}
            <Menu><MenuTrigger disableButtonEnhancement><Button appearance="subtle" icon={<MoreHorizontal20Regular />} aria-label={`More actions for ${connection ? connectionTitle(connection) : "unavailable connection"}`} /></MenuTrigger><MenuPopover><MenuList>
              <MenuItem disabled={api.bindings.isError || api.remove.isPending} onClick={() => { signin.cancel(); api.remove.reset(); api.remove.mutate({ id: binding.id, revision: binding.revision }); }}>Remove access</MenuItem>
            </MenuList></MenuPopover></Menu>
          </div>
        </article>)}
      </section>;
    })}
    {!bindings.length && !loading && !error && <Text block style={{ marginTop: 20 }}>No identities configured yet. Add an identity to choose how this agent accesses external services.</Text>}
    {(adding || (editing && bindings.some((item) => item.id === editing.binding.id))) && <ServiceAccessWizard instance={instance} connections={connections} editing={editing} canManage={canManage} onClose={closeWizard} />}
  </section>;
}
