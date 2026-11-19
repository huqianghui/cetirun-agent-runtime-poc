// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { Features } from "../types/features";

const FAIL_CLOSED: Features = { tailscaleEnabled: false, agentIdentityEnabled: false, agentToolsEnabled: false, maxInstancesPerUser: 1, allowedAgentTypes: [] };

export async function getFeatures(): Promise<Features> {
  try {
    const response = await fetch("/api/features");
    if (!response.ok) {
      // Fail closed: if we can't fetch features, disable everything optional.
      return FAIL_CLOSED;
    }
    return response.json();
  } catch {
    // Fail closed: network error or invalid JSON.
    return FAIL_CLOSED;
  }
}
