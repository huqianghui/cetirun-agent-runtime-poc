// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { apiFetch, PortalError } from "./fetch";
import type { AgentType } from "../types/instance";

export { PortalError } from "./fetch";

export interface SharedModel {
  name: string;
  contextWindow?: number;
  maxOutputTokens?: number;
  /** For an auto router of the Gateway: the models it sends requests to answer. */
  routerModels?: string[];
  /** An auto router whose models the operator cannot read completely. */
  routerUnverifiable?: boolean;
}

export interface AIGatewayCatalog {
  /** False until the operator has published the shared models. */
  known: boolean;
  sharedModels: SharedModel[];
  /** The shared decision models a Team router's decision classifier may use; no agent is configured with them. */
  decisionModels?: string[];
  /** Whether the deployment offers GitHub Copilot to agents. */
  githubCopilot: boolean;
  /**
   * The agent types the deployment's agent chart routes through the Gateway;
   * null while the operator has not read that chart.
   */
  agentTypes: AgentType[] | null;
}

export type CopilotLoginState = "none" | "pending" | "succeeded" | "expired" | "denied" | "failed";

/** A GitHub Copilot sign-in run by the Portal; never carries a token. */
export interface CopilotLogin {
  state: CopilotLoginState;
  userCode?: string;
  verificationUri?: string;
  expiresAt?: string;
  /** Seconds between polls while pending. */
  interval?: number;
  message?: string;
}

async function errorMessage(response: Response, fallback: string): Promise<string> {
  try {
    const body = await response.json();
    return body?.error?.message || fallback;
  } catch {
    return fallback;
  }
}

export async function getAIGatewayCatalog(): Promise<AIGatewayCatalog> {
  const response = await apiFetch("/api/v1/ai-gateway/catalog");
  if (!response.ok) throw new Error(await errorMessage(response, "Failed to load the shared models"));
  return response.json();
}

function loginPath(instanceName: string): string {
  return `/api/v1/instances/${encodeURIComponent(instanceName)}/copilot/login`;
}

// Start and cancel name the Agent's UID, so a page left open while the name
// was deleted and created again cannot change the new Agent's sign-in.
export async function startCopilotLogin(instanceName: string, uid: string): Promise<CopilotLogin> {
  const response = await apiFetch(loginPath(instanceName), { method: "POST", body: JSON.stringify({ uid }) });
  if (!response.ok) {
    throw new PortalError(await errorMessage(response, "Failed to start the GitHub Copilot sign-in"), response.status);
  }
  return response.json();
}

export async function pollCopilotLogin(instanceName: string): Promise<CopilotLogin> {
  const response = await apiFetch(loginPath(instanceName));
  if (!response.ok) {
    throw new PortalError(await errorMessage(response, "Failed to check the GitHub Copilot sign-in"), response.status);
  }
  return response.json();
}

export async function cancelCopilotLogin(instanceName: string, uid: string): Promise<void> {
  const response = await apiFetch(loginPath(instanceName), { method: "DELETE", body: JSON.stringify({ uid }) });
  if (!response.ok && response.status !== 404) {
    throw new Error(await errorMessage(response, "Failed to cancel the GitHub Copilot sign-in"));
  }
}
