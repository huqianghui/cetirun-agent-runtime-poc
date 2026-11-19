// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Button, Text } from "@fluentui/react-components";
import { useState } from "react";
import type { Connection } from "../api/toolAuthorization";

export function HowAccessWorks({ connection }: { connection: Connection }) {
  return <details style={{ marginTop: 16 }}><summary>How access works</summary>
    {connection.mode === "installation" ? <>
      <Text block>Signed in as {connection.github?.userLogin}. GitHub actions run as {connection.accountDisplay || "the GitHub App bot"}.</Text>
      <Text block>Each agent receives only the repositories and permissions selected in its Identities settings. Basic repository metadata is included.</Text>
      <Text block>The platform holds the App credentials and encrypted user authorization. Its operators can access the repositories granted to the App in GitHub. Install the App only on repositories you trust the platform to access.</Text>
    </> : connection.mode === "agent" ? <Text block>Resource owners grant this agent access through Azure RBAC. Connecting the identity does not verify access to Azure resources.</Text>
      : <Text block>Access depends on this personal account and the permissions authorized with the provider.{connection.provider === "github" && " CetiRun cannot restrict this method to selected repositories."}</Text>}
  </details>;
}

export function TechnicalDetails({ connection }: { connection: Connection }) {
  return <details style={{ marginTop: 12 }}><summary>Technical details</summary>
    <Text size={200} block>Connection: {connection.id}</Text>
    {connection.canonicalConnectionId && connection.canonicalConnectionId !== connection.id && <Text size={200} block>Canonical connection: {connection.canonicalConnectionId}</Text>}
    {connection.mode === "installation" ? <>
      <Text size={200} block>GitHub App: {connection.github?.appSlug || "Not reported"}</Text>
      <Text size={200} block>Installation: {connection.github?.installationId || "Not selected"}</Text>
    </> : connection.mode === "delegated" ? <Text size={200} block>OAuth scopes reported by the provider: {connection.scopes?.join(", ") || "Not reported"}</Text> : null}
  </details>;
}

export function AzureAccessInstructions({ connection }: { connection: Connection }) {
  const [message, setMessage] = useState("");
  if (!connection.identity) return null;
  const copy = async () => {
    try { await navigator.clipboard.writeText(connection.identity!.principalId); setMessage("Principal ID copied."); }
    catch { setMessage("Could not copy. Select the principal ID below to copy it."); }
  };
  return <details style={{ marginTop: 12 }}><summary>Assign Azure permissions</summary>
    <Text block>Ask the resource owner to assign Azure RBAC roles to this agent. Access has not been verified.</Text>
    <Text block>Principal ID: {connection.identity.principalId}</Text>
    <Button size="small" onClick={() => void copy()}>Copy principal ID</Button>
    <Text size={200} block>Client ID: {connection.identity.clientId}</Text>
    <Text size={200} block>Tenant ID: {connection.identity.tenantId}</Text>
    {message && <Text role="status" block>{message}</Text>}
  </details>;
}
