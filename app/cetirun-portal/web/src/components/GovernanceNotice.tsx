// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { MessageBar, MessageBarBody, MessageBarTitle, Text } from "@fluentui/react-components";
import type { GovernanceStatus } from "../types/instance";

interface GovernanceObservation {
  governance?: GovernanceStatus;
  generation?: number;
  phase?: string;
  suspended?: boolean;
}

export function currentGovernance(instance: GovernanceObservation): GovernanceStatus | undefined {
  const { governance, generation, phase, suspended } = instance;
  if (!governance || suspended || phase === "Deleting" || phase === "Suspended") return undefined;
  if (generation !== undefined && governance.observedGeneration !== generation) return undefined;
  return governance;
}

function presentation(status: GovernanceStatus) {
  if (status.reason === "OwnerBindingMissing") {
    return {
      title: "Identity owner binding required",
      action: "Ask an administrator to verify the trusted owner binding for this Agent Identity.",
    };
  }
  if (status.reason === "UnauthorizedBinding" || status.reason === "UnapprovedGrant") {
    return {
      title: "Binding or identity approval required",
      action: "Ask an administrator to verify the Team binding and the Agent's identity or role approvals.",
    };
  }
  switch (status.category) {
    case "policy": return {
      title: "Blocked by policy",
      action: "Review the Agent configuration with an administrator against the Team's Agent Policy.",
    };
    case "identity": return {
      title: "Identity not ready",
      action: "Ask an administrator to check Agent Identity setup and the reported reason.",
    };
    case "platform": return {
      title: "Platform support required",
      action: "Ask an administrator to verify platform support for the requested Sandbox controls.",
    };
    case "dependency": return {
      title: "Governance dependency unavailable",
      action: "Ask an administrator to check the required policy, Sandbox, and platform dependencies.",
    };
    case "pending": return {
      title: "Waiting for governance",
      action: "The Operator is waiting for governance prerequisites. If this persists, ask an administrator to inspect the conditions.",
    };
  }
}

/** Lists show the cause inline; details also show the next step on every tab. */
export function GovernanceNotice({ instance, compact = false }: { instance: GovernanceObservation; compact?: boolean }) {
  const status = currentGovernance(instance);
  if (!status) return null;
  const { title, action } = presentation(status);
  if (compact) return (
    <div aria-label="Governance status" style={{ display: "flex", flexDirection: "column", gap: 4, marginTop: 8, overflowWrap: "anywhere" }}>
      <Text weight="semibold">{title}</Text>
      <Text size={200}>{status.reason}{status.message ? `: ${status.message}` : ""}</Text>
    </div>
  );
  return (
    <MessageBar intent={status.category === "pending" ? "info" : "warning"} style={{ marginBottom: 16, overflowWrap: "anywhere" }}>
      <MessageBarBody>
        <MessageBarTitle>{title}</MessageBarTitle>
        <Text block>{status.reason}{status.message ? `: ${status.message}` : ""}</Text>
        <Text block>{action}</Text>
      </MessageBarBody>
    </MessageBar>
  );
}
