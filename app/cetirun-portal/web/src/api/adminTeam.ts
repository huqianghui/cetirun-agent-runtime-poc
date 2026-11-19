// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { apiFetch } from "./fetch";
import { adminResponseError } from "./adminError";

export interface TeamSkillStorage {
  resourceGroup: string;
  storageAccount: string;
  containerName: string;
  storageClassName?: string;
  storageSize?: string;
  mountPath?: string;
}

/**
 * How a router classifies requests: locally (heuristic), with a shared model (llm), or with a
 * shared decision model such as Microsoft-Decision-1 (jev).
 */
export type TeamModelRouterClassifier = "heuristic" | "llm" | "jev";

/** One smart-routing model of a Team. Tiers name shared models, or copilot/<model> for GitHub Copilot models. */
export interface TeamModelRouter {
  name: string;
  classifier?: TeamModelRouterClassifier;
  /** The shared model of the llm classifier, or the decision model of the jev classifier. */
  classifierModel?: string;
  simple: string;
  medium: string;
  complex: string;
  reasoning?: string;
}

export interface TeamModelRouterStatus {
  name: string;
  ready: boolean;
  message?: string;
}

export interface CompatibilityReference {
  name: string;
  uid: string;
  generation: number;
}

export interface CompatibilityIssue {
  code: string;
  field?: string;
  message: string;
  team?: CompatibilityReference;
  policy?: CompatibilityReference;
  sandbox?: CompatibilityReference;
}

export interface SandboxCompatibilityStatus {
  sandbox: CompatibilityReference;
  state: "Compatible" | "Incompatible" | "Unknown";
  reason?: string;
  issues?: CompatibilityIssue[];
}

export interface TeamCompatibilityStatus {
  state: "Compatible" | "Incompatible" | "Unknown";
  reason?: string;
  message?: string;
  team: CompatibilityReference;
  policy: CompatibilityReference;
  sandboxes?: SandboxCompatibilityStatus[];
  issues?: CompatibilityIssue[];
  checkedAt: string;
  validUntil: string;
}

export interface TeamDTO {
  uid: string;
  resourceVersion: string;
  generation: number;
  deleting?: boolean;
  agentPolicyRef: string;
  sandboxRefs: string[];
  defaultSandboxRef: string;
  name: string;
  role?: string;
  description?: string[];
  skills?: string[];
  skillStorage?: TeamSkillStorage;
  createdAt?: string;
  modelRouters?: TeamModelRouter[];
  modelRouterStatus?: TeamModelRouterStatus[];
  compatibility?: TeamCompatibilityStatus;
}

export interface TeamUpsertRequest {
  uid?: string;
  resourceVersion?: string;
  agentPolicyRef: string;
  sandboxRefs: string[];
  defaultSandboxRef: string;
  name?: string;
  role?: string;
  description?: string[];
  skills?: string[];
  skillStorage?: TeamSkillStorage;
  modelRouters?: TeamModelRouter[];
}

export async function listTeams(): Promise<TeamDTO[]> {
  const response = await apiFetch("/api/v1/admin/teams");
  if (!response.ok) throw await adminResponseError(response, "Failed to list teams");
  const data = await response.json();
  return data.teams || [];
}

export async function getTeam(name: string): Promise<TeamDTO> {
  const response = await apiFetch(`/api/v1/admin/teams/${encodeURIComponent(name)}`);
  if (!response.ok) throw await adminResponseError(response, "Failed to get team");
  return response.json();
}

export async function createTeam(req: TeamUpsertRequest): Promise<TeamDTO> {
  const response = await apiFetch("/api/v1/admin/teams", {
    method: "POST",
    body: JSON.stringify(req),
  });
  if (!response.ok) throw await adminResponseError(response, "Failed to create team");
  return response.json();
}

export async function updateTeam(name: string, req: TeamUpsertRequest): Promise<TeamDTO> {
  const response = await apiFetch(`/api/v1/admin/teams/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(req),
  });
  if (!response.ok) throw await adminResponseError(response, "Failed to update team");
  return response.json();
}

export async function deleteTeam(team: Pick<TeamDTO, "name" | "uid" | "resourceVersion">): Promise<void> {
  const response = await apiFetch(`/api/v1/admin/teams/${encodeURIComponent(team.name)}`, {
    method: "DELETE",
    body: JSON.stringify({ uid: team.uid, resourceVersion: team.resourceVersion }),
  });
  if (!response.ok && response.status !== 404) {
    throw await adminResponseError(response, "Failed to delete team");
  }
}
