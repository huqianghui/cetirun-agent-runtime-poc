// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { apiFetch } from "./fetch";

export type ConnectionProvider = "azure" | "microsoft" | "github" | "google";
export type ConnectionMode = "agent" | "delegated" | "installation";
export type ConnectionStatus = "connecting" | "connected" | "interaction_required" | "installation_required" | "disconnected" | "pending" | "ready";

export interface AgentIdentity {
  instanceUid: string;
  tenantId: string;
  principalId: string;
  clientId: string;
}

export interface Connection {
  canonicalConnectionId?: string;
  id: string;
  provider: ConnectionProvider;
  accountDisplay: string;
  services: string[];
  scopes: string[];
  revision: number;
  status: ConnectionStatus;
  createdAt: string;
  /** Executor identity mode. */
  mode: ConnectionMode;
  /** Present only for Broker-derived independent Agent identities. */
  identity?: AgentIdentity;
  /** Present for shared GitHub App connections. */
  github?: GitHubConnection;
}

export interface GitHubConnection {
  installationId?: number;
  accountLogin?: string;
  appSlug?: string;
  userLogin: string;
}

export interface ConnectionStart {
  authorizationId: string;
  authorizationUrl: string;
  expiresAt: string;
  connectionId?: string;
}

export interface ConnectionBinding {
  id: string;
  instanceUid: string;
  connectionId: string;
  services: string[];
  enabled: boolean;
  revision: number;
  github?: GitHubBinding;
}
export interface ConnectionBindingList { bindings: ConnectionBinding[]; readOnly: boolean }
export interface GitHubPermissions {
  contents?: "read" | "write";
  pull_requests?: "read" | "write";
  issues?: "read" | "write";
}
export interface GitHubBinding { repositoryIds: number[]; permissions: GitHubPermissions }
export interface ConnectionBindingInput { connectionId: string; services: string[]; enabled: boolean; revision?: number; github?: GitHubBinding }

export interface GitHubAppConfig { enabled: boolean; installUrl?: string }
export interface GitHubInstallation {
  id: number;
  accountLogin: string;
  accountType: string;
  permissions: Record<string, string>;
}
export interface GitHubInstallations { installations: GitHubInstallation[]; installUrl: string }
export interface GitHubRepository { id: number; fullName: string; admin: boolean }
export interface GitHubRepositories { repositories: GitHubRepository[]; hasNextPage: boolean }

export class ToolAuthorizationError extends Error {
  constructor(message: string, readonly code?: string) {
    super(message);
  }
}

async function responseError(response: Response, fallback: string): Promise<ToolAuthorizationError> {
  const body = await response.json().catch(() => undefined);
  const error = body?.error;
  const message = typeof error?.message === "string" ? error.message : fallback;
  const code = typeof error?.code === "string" ? error.code : undefined;
  return new ToolAuthorizationError(message, code);
}

async function parse<T>(response: Response): Promise<T> {
  if (!response.ok) {
    throw await responseError(response, "Tool authorization request failed");
  }
  return response.json() as Promise<T>;
}

export async function listConnections(signal?: AbortSignal): Promise<Connection[]> {
  return parse(await apiFetch("/api/v1/connections", { signal }));
}

export async function startConnection(provider: ConnectionProvider, mode?: "installation"): Promise<ConnectionStart> {
  return parse(await apiFetch("/api/v1/connections/start", { method: "POST", body: JSON.stringify(mode ? { provider, mode } : { provider }) }));
}

export async function getGitHubAppConfig(signal?: AbortSignal): Promise<GitHubAppConfig> {
  return parse(await apiFetch("/api/v1/connections/github-app/config", { signal }));
}

export async function listGitHubInstallations(id: string, signal?: AbortSignal): Promise<GitHubInstallations> {
  return parse(await apiFetch(`/api/v1/connections/${encodeURIComponent(id)}/github/installations`, { signal }));
}

export async function selectGitHubInstallation(id: string, installationId: number): Promise<Connection> {
  return parse(await apiFetch(`/api/v1/connections/${encodeURIComponent(id)}/github/installation`, { method: "POST", body: JSON.stringify({ installationId }) }));
}

export async function listGitHubRepositories(id: string, installationId: number, page: number, signal?: AbortSignal): Promise<GitHubRepositories> {
  const query = new URLSearchParams({ installationId: String(installationId), page: String(page) });
  return parse(await apiFetch(`/api/v1/connections/${encodeURIComponent(id)}/github/repositories?${query}`, { signal }));
}

export async function reconnectConnection(id: string): Promise<ConnectionStart> {
  return parse(await apiFetch(`/api/v1/connections/${encodeURIComponent(id)}/reconnect`, { method: "POST" }));
}

export async function deleteConnection(id: string): Promise<void> {
  const response = await apiFetch(`/api/v1/connections/${encodeURIComponent(id)}`, { method: "DELETE" });
  if (!response.ok) {
    throw await responseError(response, "Could not disconnect this account");
  }
}

export async function listConnectionBindings(name: string, signal?: AbortSignal): Promise<ConnectionBindingList> {
  const response = await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}/connection-bindings`, { signal });
  const bindings = await parse<ConnectionBinding[]>(response);
  return { bindings, readOnly: response.headers.get("X-CetiRun-Connection-Bindings-Read-Only") === "true" };
}

export type ToolIdentityInput = { provider: "azure" };

export async function createToolIdentity(name: string, input: ToolIdentityInput = { provider: "azure" }): Promise<Connection> {
  return parse(await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}/tool-identities`, { method: "POST", body: JSON.stringify(input) }));
}

export async function createConnectionBinding(name: string, input: ConnectionBindingInput): Promise<ConnectionBinding> {
  return parse(await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}/connection-bindings`, { method: "POST", body: JSON.stringify(input) }));
}

export async function updateConnectionBinding(name: string, id: string, input: ConnectionBindingInput & { revision: number }): Promise<ConnectionBinding> {
  return parse(await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}/connection-bindings/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(input) }));
}

export async function deleteConnectionBinding(name: string, id: string, revision: number): Promise<void> {
  const response = await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}/connection-bindings/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision) } });
  if (!response.ok) {
    throw await responseError(response, "Could not remove this connection binding");
  }
}

export interface ConnectionAuthorization {
  authorizationId: string;
  status: "pending" | "complete" | "failed";
  connectionId?: string;
}
export interface ConnectionUsage {
  connectionId: string;
  agents: { instanceUid: string; instanceName: string; instanceNamespace: string }[];
  canRemove: boolean;
}
export async function getConnectionAuthorization(id: string, signal?: AbortSignal): Promise<ConnectionAuthorization> {
  return parse(await apiFetch(`/api/v1/connection-authorizations/${encodeURIComponent(id)}`, { signal }));
}
export async function getConnectionUsage(id: string, signal?: AbortSignal): Promise<ConnectionUsage> {
  return parse(await apiFetch(`/api/v1/connections/${encodeURIComponent(id)}/usage`, { signal }));
}
