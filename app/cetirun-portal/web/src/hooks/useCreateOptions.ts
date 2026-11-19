// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "../api/fetch";
import type { AIGatewayCatalog } from "../api/aiGateway";
import type { AgentType } from "../types/instance";
import type { AgentPolicyRateLimits } from "../types/rateLimits";

export interface CreateOptions {
  team: string;
  policyName?: string;
  allowedAgentTypes: AgentType[];
  aiGatewayEnabled: boolean;
  gateway: AIGatewayCatalog;
  agentIdentity: { allowed: boolean; required: boolean; customAllowed?: boolean };
  /** The Team's smart-routing models the Team's Policy allows, by name. */
  modelRouters?: string[];
  /** AI Gateway limits the Team's policy sets on each Agent's key. */
  rateLimits?: AgentPolicyRateLimits;
  blocked?: { code: string; message: string };
}

export function useCreateOptions(team: string, enabled: boolean) {
  return useQuery<CreateOptions, Error>({
    queryKey: ["instance-create-options", team],
    queryFn: async ({ signal }) => {
      const response = await apiFetch(`/api/v1/instance/create-options?team=${encodeURIComponent(team)}`, { signal });
      if (!response.ok) {
        const body = await response.json();
        throw new Error(body?.error?.message || "Failed to load Agent creation options");
      }
      return response.json();
    },
    enabled,
    // A previous Team's permissions must never be used while its current
    // options are loading, including when returning to an earlier selection.
    staleTime: 0,
    gcTime: 0,
    retry: false,
  });
}
