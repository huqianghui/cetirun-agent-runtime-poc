// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

/** A limit for one model, by the Gateway model name the Agent requests. */
export interface AgentPolicyModelRateLimit {
  rpm?: number;
  tpm?: number;
}

/**
 * AI Gateway limits an AgentPolicy sets on the key of every Agent that uses
 * it. Each Agent's key has its own counters. An absent field is unlimited.
 */
export interface AgentPolicyRateLimits {
  maxParallelRequests?: number;
  rpm?: number;
  tpm?: number;
  perModel?: Record<string, AgentPolicyModelRateLimit>;
}
