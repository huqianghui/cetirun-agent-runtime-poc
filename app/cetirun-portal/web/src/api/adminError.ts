// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { CompatibilityIssue } from "./adminTeam";

export class AdminConfigurationError extends Error {
  constructor(
    message: string,
    readonly code?: string,
    readonly details: CompatibilityIssue[] = [],
  ) {
    super(message);
    this.name = "AdminConfigurationError";
  }
}

function isReference(value: unknown): boolean {
  if (!value || typeof value !== "object") return false;
  const reference = value as Record<string, unknown>;
  return typeof reference.name === "string" && (reference.uid === undefined || typeof reference.uid === "string") &&
    (reference.generation === undefined || typeof reference.generation === "number");
}

function isCompatibilityIssue(value: unknown): value is CompatibilityIssue {
  if (!value || typeof value !== "object") return false;
  const issue = value as Record<string, unknown>;
  return typeof issue.code === "string" && (issue.field === undefined || typeof issue.field === "string") && typeof issue.message === "string" &&
    (issue.team === undefined || isReference(issue.team)) && (issue.policy === undefined || isReference(issue.policy)) &&
    (issue.sandbox === undefined || isReference(issue.sandbox));
}

export async function adminResponseError(response: Response, fallback: string): Promise<AdminConfigurationError> {
  try {
    const body = await response.json();
    const error = body?.error;
    const details = Array.isArray(error?.details) ? error.details.filter(isCompatibilityIssue) : [];
    return new AdminConfigurationError(error?.message || fallback, error?.code, details);
  } catch {
    return new AdminConfigurationError(fallback);
  }
}
