// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { Instance } from "../types/instance";

export function agentTypeLabel(type?: string): string {
  if (!type) return "—";
  switch (type) {
    case "openclaw": return "OpenClaw";
    case "hermes": return "Hermes";
    case "custom": return "Custom";
    default: return type;
  }
}

/** Badge color for an instance phase. */
export const phaseBadgeColor: Record<Instance["phase"], "success" | "warning" | "danger" | "informative"> = {
  Ready: "success",
  Provisioning: "warning",
  Pending: "informative",
  Failed: "danger",
  Deleting: "warning",
  Suspended: "warning",
};

/** Derive the base URL (origin + pathname, no query params) from an instance accessUrl. */
export function parseBaseUrl(accessUrl: string | undefined): string | undefined {
  if (!accessUrl) return undefined;
  try {
    const u = new URL(accessUrl);
    if (u.protocol !== "https:" && u.protocol !== "http:") return undefined;
    return u.origin + u.pathname.replace(/\/$/, "");
  } catch {
    return undefined;
  }
}

/**
 * Return the browser-native UI entrypoint for an instance.
 *
 * Each supported agent serves its browser-native UI below /ui/<instanceName>.
 */
export function agentUIUrl(instance: Pick<Instance, "agent" | "accessUrl" | "webUIUrl">): string | undefined {
  if (instance.agent.type === "custom" && !instance.agent.webUI) return undefined;
  const baseUrl = parseBaseUrl(instance.agent.type === "custom" ? instance.webUIUrl : instance.accessUrl);
  if (!baseUrl) return undefined;
  return `${baseUrl}/`;
}

type InstanceAccess = Pick<Instance, "agent" | "instanceName" | "accessUrl"> & Partial<Pick<Instance, "conditions">>;

export function terminalUrl(instance: InstanceAccess): string | undefined {
  if (instance.agent.type === "custom" &&
    !instance.conditions?.some((condition) => condition.type === "TerminalConfigured" && condition.status === "True")) {
    return undefined;
  }
  const baseUrl = instanceGatewayUrl(instance);
  return baseUrl ? `${baseUrl}/term` : undefined;
}

export function desktopUrl(instance: InstanceAccess): string | undefined {
  if (instance.agent?.type === "custom") {
    const desktop = instance.conditions?.find((condition) => condition.type === "DesktopConfigured");
    if (desktop && desktop.status !== "True") return undefined;
  }
  const baseUrl = instanceGatewayUrl(instance);
  return baseUrl ? `${baseUrl}/desktop` : undefined;
}

/**
 * Return the gateway base URL for instance services such as the embedded
 * terminal, desktop, and CetiRun authentication helper. These services are
 * intentionally separate from the agent's browser-native /ui route.
 */
export function instanceGatewayUrl(
  instance: Pick<Instance, "instanceName" | "accessUrl"> & Partial<Pick<Instance, "agent" | "conditions">>,
  platformOrigin?: string,
): string | undefined {
  let source = instance.accessUrl;
  if (instance.agent?.type === "custom") {
    if (!instance.conditions?.some((condition) => condition.type === "IngressReady" && condition.status === "True")) return undefined;
    source = instance.accessUrl || platformOrigin;
  }
  if (!source) return undefined;
  try {
    const u = new URL(source);
    if (u.protocol !== "https:" && u.protocol !== "http:") return undefined;
    return `${u.origin}/${encodeURIComponent(instance.instanceName)}`;
  } catch {
    return undefined;
  }
}
