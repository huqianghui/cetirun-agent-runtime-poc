// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { Instance, InstanceVersion, CreateInstanceRequest, UpdateInstanceRequest, RestartInstanceRequest, SetInstanceSuspendedRequest, StatusResponse } from "../types/instance";
import { apiFetch, PortalError } from "./fetch";

export type InstanceResponse = Instance;

export class CreateInstanceError extends Error {
  instanceName?: string;
  constructor(message: string, instanceName?: string) {
    super(message);
    this.instanceName = instanceName;
  }
}

export async function getInstance(): Promise<Instance | null> {
  const response = await apiFetch("/api/v1/instance");
  if (response.status === 404) return null;
  if (!response.ok) throw new Error("Failed to get instance");
  return response.json();
}

export async function createInstance(req: CreateInstanceRequest): Promise<Instance> {
  const { requestId, ...body } = req;
  const response = await apiFetch("/api/v1/instances", {
    method: "POST",
    headers: requestId ? { "Idempotency-Key": requestId } : undefined,
    body: JSON.stringify(body),
  });
  if (!response.ok) {
    const err = await response.json();
    throw new CreateInstanceError(err.error?.message || "Failed to create instance", err.instanceName);
  }
  return response.json();
}

export async function updateInstance(req: UpdateInstanceRequest): Promise<Instance> {
  const response = await apiFetch("/api/v1/instance", {
    method: "PATCH",
    body: JSON.stringify(req),
  });
  if (!response.ok) {
    const err = await response.json();
    throw new Error(err.error?.message || "Failed to update instance");
  }
  return response.json();
}

export async function upgradeInstance(version: InstanceVersion): Promise<Instance> {
  const response = await apiFetch("/api/v1/instance/upgrade", {
    method: "POST", body: JSON.stringify({ uid: version.uid, resourceVersion: version.resourceVersion }),
  });
  if (!response.ok) {
    const err = await response.json();
    throw new Error(err.error?.message || "Failed to upgrade instance");
  }
  return response.json();
}

export async function getInstanceStatus(): Promise<StatusResponse> {
  const response = await apiFetch("/api/v1/instance/status");
  if (!response.ok) {
    let err: any;
    try {
      err = await response.json();
    } catch {
      // Ignore JSON parsing errors and fall back to generic message below.
    }
    throw new Error(err?.error?.message || "Failed to get instance status");
  }
  return response.json();
}

export async function upgradeInstanceByName(name: string, version: InstanceVersion): Promise<Instance> {
  const response = await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}/upgrade`, {
    method: "POST", body: JSON.stringify({ uid: version.uid, resourceVersion: version.resourceVersion }),
  });
  if (!response.ok) {
    const err = await response.json();
    throw new Error(err.error?.message || "Failed to upgrade instance");
  }
  return response.json();
}

export async function restartInstanceByName(name: string, request: RestartInstanceRequest): Promise<Instance> {
  return requestInstanceAction(name, "restart", request, request.requestId);
}

export async function setInstanceSuspendedByName(name: string, request: SetInstanceSuspendedRequest): Promise<Instance> {
  const instance = await requestInstanceAction(name, request.suspended ? "suspend" : "resume", request, undefined, request.generation);
  if (!instance || instance.uid !== request.uid || typeof instance.resourceVersion !== "string" || !instance.resourceVersion ||
    instance.suspended !== request.suspended || !Number.isSafeInteger(instance.generation) || (instance.generation ?? 0) < 1) {
    throw new Error("The Agent response could not be confirmed. Check its current state or retry the same request.");
  }
  return instance;
}

async function requestInstanceAction(
  name: string, action: "restart" | "suspend" | "resume", version: InstanceVersion, requestId?: string, generation?: number,
): Promise<Instance> {
  const headers: Record<string, string> = {};
  if (requestId) headers["Idempotency-Key"] = requestId;
  if (Number.isSafeInteger(generation) && (generation ?? 0) > 0) headers["X-CetiRun-Agent-Generation"] = String(generation);
  const response = await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}/${action}`, {
    method: "POST",
    ...(Object.keys(headers).length ? { headers } : {}),
    body: JSON.stringify({ uid: version.uid, resourceVersion: version.resourceVersion }),
  });
  if (!response.ok) {
    const body: unknown = await response.json();
    let message = `Failed to request Agent ${action}`;
    let code: string | undefined;
    if (body !== null && typeof body === "object" && "error" in body &&
      body.error !== null && typeof body.error === "object") {
      if ("message" in body.error && typeof body.error.message === "string" && body.error.message) {
        message = body.error.message;
      }
      if ("code" in body.error && typeof body.error.code === "string") code = body.error.code;
    }
    throw new PortalError(message, response.status, code);
  }
  return response.json();
}

export function isRestartRequestRejected(error: unknown): boolean {
  // Exact retries return the stored request before checking resourceVersion.
  // Other errors, such as suspension, can follow an earlier accepted attempt.
  return error instanceof PortalError && error.status === 409 && error.code === "instance_changed";
}

export function isSuspensionRequestRejected(error: unknown): boolean {
  if (!(error instanceof PortalError)) return false;
  return (error.status === 409 && ["instance_changed", "instance_deleting", "suspension_unavailable", "suspension_home_required"].includes(error.code ?? "")) ||
    (error.status === 503 && error.code === "suspension_home_unverified") ||
    (error.status === 403 && ["forbidden", "invalid_origin", "admission_rejected"].includes(error.code ?? "")) ||
    (error.status === 400 && ["invalid_body", "invalid_agent", "invalid_name"].includes(error.code ?? "")) ||
    (error.status === 428 && error.code === "version_required") ||
    (error.status === 404 && error.code === "instance_not_found");
}

export async function getInstanceStatusByName(name: string): Promise<StatusResponse> {
  const response = await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}/status`);
  if (!response.ok) {
    let err: any;
    try {
      err = await response.json();
    } catch {
      // Ignore JSON parsing errors and fall back to generic message below.
    }
    throw new Error(err?.error?.message || "Failed to get instance status");
  }
  return response.json();
}

export async function fetchInstances(): Promise<InstanceResponse[]> {
  const response = await apiFetch("/api/v1/instances");
  if (!response.ok) throw new Error(`Failed to fetch instances: ${response.statusText}`);
  return response.json();
}

export async function fetchInstanceByName(name: string): Promise<InstanceResponse> {
  const response = await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}`);
  if (!response.ok) throw new Error(`Failed to fetch instance ${name}: ${response.statusText}`);
  return response.json();
}

export async function deleteInstanceByName(name: string, version: InstanceVersion): Promise<void> {
  const response = await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}`, {
    method: "DELETE",
    body: JSON.stringify({ uid: version.uid, resourceVersion: version.resourceVersion }),
  });
  if (!response.ok && response.status !== 404) {
    const text = await response.text().catch(() => "");
    let message = "Failed to delete instance";
    try {
      const err = JSON.parse(text);
      message = err.error?.message || message;
    } catch {
      if (text) message = text;
    }
    throw new Error(message);
  }
}

export async function updateInstanceByName(name: string, req: UpdateInstanceRequest): Promise<Instance> {
  const response = await apiFetch(`/api/v1/instances/${encodeURIComponent(name)}`, {
    method: "PATCH",
    body: JSON.stringify(req),
  });
  if (!response.ok) {
    const err = await response.json();
    throw new Error(err.error?.message || "Failed to update instance");
  }
  return response.json();
}
