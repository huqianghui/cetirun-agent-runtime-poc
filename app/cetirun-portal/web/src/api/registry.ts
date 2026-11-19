// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { apiFetch } from "./fetch";
import type { Instance, RegistryAuth } from "../types/instance";

export function instanceETag(instance: Instance): string {
  return `"${instance.uid}/${instance.resourceVersion}"`;
}

export async function updateRegistryAuth(instance: Instance, credentials?: RegistryAuth): Promise<void> {
  const response = await apiFetch(`/api/v1/instances/${encodeURIComponent(instance.instanceName)}/registry-auth`, {
    method: credentials ? "PUT" : "DELETE",
    headers: { "If-Match": instanceETag(instance) },
    body: credentials ? JSON.stringify(credentials) : undefined,
  });
  if (!response.ok) {
    const body = await response.json().catch(() => null);
    throw new Error(body?.error?.message || "Unable to update registry credentials");
  }
}
