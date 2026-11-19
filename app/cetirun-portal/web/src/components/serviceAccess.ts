// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { Connection, ConnectionProvider, GitHubPermissions } from "../api/toolAuthorization";

export const providerOrder: ConnectionProvider[] = ["github", "azure", "google", "microsoft"];
export const providerLabels: Record<ConnectionProvider, string> = { github: "GitHub", azure: "Azure", google: "Google", microsoft: "Microsoft 365" };
export const providerDisabledReasons: Partial<Record<ConnectionProvider, string>> = { microsoft: "Not available in this environment" };
export const serviceLabels: Record<string, string> = { azure_arm: "Azure Resource Manager", github: "GitHub", microsoft_graph: "Microsoft Graph", google_gmail: "Gmail", google_calendar: "Calendar" };
export const permissionLabels: Record<keyof GitHubPermissions, string> = { contents: "Code", pull_requests: "Pull requests", issues: "Issues" };
export const permissionKeys = Object.keys(permissionLabels) as (keyof GitHubPermissions)[];
export const permissionLabel = (value?: "read" | "write") => value === "write" ? "Read and write" : value === "read" ? "Read" : "No access";
export const accessMethodLabel = (connection: Connection) => connection.mode === "installation" ? "Selected repositories" : connection.mode === "agent" ? "Agent identity" : "Personal account";
export const connectionTitle = (connection: Connection) => connection.mode === "installation" ? connection.github?.accountLogin || connection.github?.userLogin || "Setup incomplete" : connection.accountDisplay || "Setup incomplete";
/** Historical duplicate records remain available for existing Agent bindings and safe cleanup. */
export const isHistoricalDuplicate = (connection: Connection) => Boolean(connection.canonicalConnectionId && connection.canonicalConnectionId !== connection.id);
export function connectionStatusColor(connection?: Connection): "success" | "warning" | "danger" {
  if (connection?.status === "connected") return "success";
  if (connection && ["interaction_required", "connecting", "pending", "installation_required"].includes(connection.status)) return "warning";
  return "danger";
}
export function connectionStatusLabel(connection: Connection): string {
  switch (connection.status) {
    case "connected": return "Connected";
    case "interaction_required": return "Sign-in required";
    case "installation_required":
    case "connecting":
    case "pending": return "Setup incomplete";
    default: return "Unavailable";
  }
}
