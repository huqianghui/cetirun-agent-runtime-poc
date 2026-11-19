// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

/** Prefix of the Team and Policy skill entries that name npm packages rather than Skill Marketplace skills. */
export const NPM_SKILL_PREFIX = "npm:";

/** The form of a Skill Marketplace skill name: lowercase words of letters and digits joined by single hyphens. */
export function validSkillMarketplaceName(name: string): boolean {
  return name.length <= 64 && /^[a-z0-9]+(-[a-z0-9]+)*$/.test(name);
}

/** A skill version as lists show it: the first 7 characters of its tree hash. */
export function shortSkillVersion(version?: string): string {
  return (version || "").slice(0, 7);
}

/** A catalog time in the browser's locale; "—" when absent, the raw value when it does not parse. */
export function formatSkillTime(value?: string): string {
  if (!value) return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

/** A skill's size in bytes, KiB or MiB. */
export function formatSkillSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  if (bytes < 1024) return `${bytes} B`;
  const kib = bytes / 1024;
  if (kib < 1024) return `${kib < 10 ? kib.toFixed(1) : Math.round(kib)} KiB`;
  const mib = kib / 1024;
  return `${mib < 10 ? mib.toFixed(1) : Math.round(mib)} MiB`;
}
