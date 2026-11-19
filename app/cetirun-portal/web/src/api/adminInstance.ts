// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { apiFetch, PortalError } from "./fetch";
import type { AgentConfig, ConditionStatus, GovernanceStatus, InstanceVersion, StatusResponse } from "../types/instance";

/** The administrator API deliberately omits registry secret references. */
export type AdminAgentConfig = Omit<AgentConfig, "registrySecretRef">;

export interface AdminInstanceSummary {
  name: string;
  alias: string;
  displayName?: string;
  email?: string;
  team?: string;
  agent: AdminAgentConfig;
  uid: string;
  phase?: string;
  ready: boolean;
  governance?: GovernanceStatus;
  createdAt: string;
}

export interface AdminInstanceDetail extends AdminInstanceSummary {
  resourceVersion: string;
  generation: number;
  observedGeneration: number;
  namespace?: string;
  lastReconciled?: string;
  deletionTimestamp?: string;
  suspended: boolean;
  sandboxRef?: string;
  chartVersion?: string;
  model?: string;
  agentIdentity?: { enabled: boolean; phase?: string; reason?: string };
  tailscaleEnabled: boolean;
  registryAuthConfigured: boolean;
  conditions: ConditionStatus[];
}

export interface AdminInstanceStatus extends StatusResponse {
  uid: string;
  errors?: Array<{ section: "events" | "nsEvents" | "pods"; message: string }>;
}

export interface AdminUpdateInstanceRequest extends InstanceVersion {
  displayName?: string;
  model?: string;
  agent?: {
    image?: string | null;
    port?: number | null;
    webUI?: boolean | null;
  };
}

async function portalError(response: Response, fallback: string): Promise<PortalError> {
  try {
    const body: unknown = await response.json();
    if (body && typeof body === "object" && "error" in body) {
      const error = body.error;
      if (error && typeof error === "object") {
        const message = "message" in error && typeof error.message === "string" ? error.message : fallback;
        const code = "code" in error && typeof error.code === "string" ? error.code : undefined;
        return new PortalError(message, response.status, code);
      }
    }
  } catch {
    // The API is expected to return the Portal error envelope, but retain a useful fallback.
  }
  return new PortalError(fallback, response.status);
}

export async function listAllInstances(): Promise<AdminInstanceSummary[]> {
  const response = await apiFetch("/api/v1/admin/instances");
  if (!response.ok) throw await portalError(response, "Failed to list agents");
  const data = await response.json();
  return data.instances || [];
}

/** Returns null only when the agent has been deleted or does not exist. */
export async function getAdminInstance(name: string): Promise<AdminInstanceDetail | null> {
  const response = await apiFetch(`/api/v1/admin/instances/${encodeURIComponent(name)}`);
  if (response.status === 404) return null;
  if (!response.ok) throw await portalError(response, "Failed to load agent");
  return response.json();
}

export async function getAdminInstanceStatus(name: string): Promise<AdminInstanceStatus> {
  const response = await apiFetch(`/api/v1/admin/instances/${encodeURIComponent(name)}/status`);
  if (!response.ok) throw await portalError(response, "Failed to load agent diagnostics");
  return response.json();
}

export async function updateAdminInstance(name: string, request: AdminUpdateInstanceRequest): Promise<AdminInstanceDetail> {
  const response = await apiFetch(`/api/v1/admin/instances/${encodeURIComponent(name)}`, {
    method: "PATCH",
    body: JSON.stringify(request),
  });
  if (!response.ok) throw await portalError(response, "Failed to update agent");
  return response.json();
}

export async function deleteAdminInstance(name: string, version: InstanceVersion): Promise<void> {
  const response = await apiFetch(`/api/v1/admin/instances/${encodeURIComponent(name)}`, {
    method: "DELETE",
    body: JSON.stringify(version),
  });
  // A concurrent deletion has already reached the desired state.
  if (!response.ok && response.status !== 404) throw await portalError(response, "Failed to delete agent");
}
