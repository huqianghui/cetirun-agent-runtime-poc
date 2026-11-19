// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { AgentPolicyModelRateLimit, AgentPolicyRateLimits } from "../types/rateLimits";

/** Minimums from the AgentPolicy CRD. The Gateway treats 0 as "reject every request". */
export const MIN_REQUEST_LIMIT = 1;
export const MIN_TOKEN_LIMIT = 4096;
export const MAX_MODEL_LIMITS = 64;
/** The Gateway stores max parallel requests as a 32-bit integer. */
export const MAX_PARALLEL_REQUESTS = 2147483647;

export const TPM_HINT =
  "The Gateway counts the tokens of each finished request, cached input tokens excluded, and refuses an Agent's " +
  "requests once the minute's count has reached the limit, until the next minute. A single long request can " +
  "take the count past the limit. Values below 4,096 are not accepted.";

export interface RateLimitRow {
  model: string;
  rpm: string;
  tpm: string;
}

/** Text inputs of the policy form; an empty input means no limit. */
export interface RateLimitForm {
  maxParallelRequests: string;
  rpm: string;
  tpm: string;
  perModel: RateLimitRow[];
}

export const emptyRateLimitForm: RateLimitForm = { maxParallelRequests: "", rpm: "", tpm: "", perModel: [] };

const text = (value?: number) => (value === undefined || value === null ? "" : String(value));

export function rateLimitFormFrom(limits?: AgentPolicyRateLimits): RateLimitForm {
  return {
    maxParallelRequests: text(limits?.maxParallelRequests),
    rpm: text(limits?.rpm),
    tpm: text(limits?.tpm),
    perModel: Object.keys(limits?.perModel || {})
      .sort()
      .map((model) => ({ model, rpm: text(limits!.perModel![model].rpm), tpm: text(limits!.perModel![model].tpm) })),
  };
}

const format = (value: number) => value.toLocaleString("en-US");

function parseLimit(input: string, minimum: number, label: string, maximum = Number.MAX_SAFE_INTEGER): { value?: number; error?: string } {
  const trimmed = input.trim();
  if (trimmed === "") return {};
  if (!/^[0-9]+$/.test(trimmed)) return { error: `${label} must be a whole number; leave it empty for no limit.` };
  const value = Number(trimmed);
  if (!Number.isSafeInteger(value)) return { error: `${label} is too large.` };
  if (value < minimum) return { error: `${label} must be at least ${format(minimum)}; leave it empty for no limit.` };
  if (value > maximum) return { error: `${label} must be at most ${format(maximum)}.` };
  return { value };
}

/**
 * Converts the form to the policy's rateLimits. Empty inputs are left out,
 * never sent as 0, and a form without any limit gives undefined.
 */
export function rateLimitsFromForm(form: RateLimitForm): { limits?: AgentPolicyRateLimits; error?: string } {
  const limits: AgentPolicyRateLimits = {};
  for (const [key, label, minimum, maximum] of [
    ["maxParallelRequests", "Max parallel requests", MIN_REQUEST_LIMIT, MAX_PARALLEL_REQUESTS],
    ["rpm", "Requests per minute", MIN_REQUEST_LIMIT, Number.MAX_SAFE_INTEGER],
    ["tpm", "Tokens per minute", MIN_TOKEN_LIMIT, Number.MAX_SAFE_INTEGER],
  ] as const) {
    const parsed = parseLimit(form[key], minimum, label, maximum);
    if (parsed.error) return { error: parsed.error };
    if (parsed.value !== undefined) limits[key] = parsed.value;
  }
  // Built from entries, not by assignment: assigning a model named __proto__
  // to a plain object would set its prototype and drop the entry.
  const entries: [string, AgentPolicyModelRateLimit][] = [];
  const seen = new Set<string>();
  for (const row of form.perModel) {
    const model = row.model.trim();
    if (model === "" && row.rpm.trim() === "" && row.tpm.trim() === "") continue;
    if (model === "" || /\s/.test(model) || [...model].length > 256) {
      return { error: "Per-model limits need an exact Gateway model name of up to 256 characters without spaces." };
    }
    if (seen.has(model)) return { error: `The model ${model} is listed more than once.` };
    seen.add(model);
    const rpm = parseLimit(row.rpm, MIN_REQUEST_LIMIT, `Requests per minute for ${model}`);
    const tpm = parseLimit(row.tpm, MIN_TOKEN_LIMIT, `Tokens per minute for ${model}`);
    if (rpm.error || tpm.error) return { error: rpm.error || tpm.error };
    if (rpm.value === undefined && tpm.value === undefined) return { error: `Set requests or tokens per minute for ${model}, or remove the row.` };
    entries.push([model, { ...(rpm.value !== undefined && { rpm: rpm.value }), ...(tpm.value !== undefined && { tpm: tpm.value }) }]);
  }
  const perModel: Record<string, AgentPolicyModelRateLimit> = Object.fromEntries(entries);
  if (Object.keys(perModel).length > MAX_MODEL_LIMITS) return { error: `At most ${MAX_MODEL_LIMITS} models can have their own limits.` };
  if (Object.keys(perModel).length > 0) limits.perModel = perModel;
  return Object.keys(limits).length === 0 ? {} : { limits };
}

function describeModel(limit: AgentPolicyModelRateLimit): string {
  return [
    limit.rpm !== undefined ? `${format(limit.rpm)} requests per minute` : "",
    limit.tpm !== undefined ? `${format(limit.tpm)} tokens per minute` : "",
  ].filter(Boolean).join(", ");
}

/** Human-readable lines for a read-only view; empty when nothing is limited. */
export function describeRateLimits(limits?: AgentPolicyRateLimits): string[] {
  if (!limits) return [];
  const lines: string[] = [];
  if (limits.maxParallelRequests !== undefined) lines.push(`Up to ${format(limits.maxParallelRequests)} requests at a time`);
  if (limits.rpm !== undefined) lines.push(`${format(limits.rpm)} requests per minute`);
  if (limits.tpm !== undefined) lines.push(`${format(limits.tpm)} tokens per minute`);
  for (const model of Object.keys(limits.perModel || {}).sort()) {
    const description = describeModel(limits.perModel![model]);
    if (description) lines.push(`${model}: ${description}`);
  }
  return lines;
}
