// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { apiFetch, PortalError } from "./fetch";

/** The synchronized skills repository, without credentials. */
export interface SkillMarketplaceRepository {
  url: string;
  ref?: string;
  /** The directory of the skills, relative to the repository root. */
  path?: string;
}

/** One valid skill of the repository. */
export interface SkillMarketplaceSkill {
  name: string;
  description: string;
  /** The first 12 hexadecimal characters of the skill directory's Git tree hash. */
  version: string;
  /** The skill directory, relative to the repository root. */
  path: string;
  /** When the marketplace first saw this version. */
  updatedAt?: string;
  files: number;
  bytes: number;
}

/** A skill directory the marketplace rejected, and why. */
export interface SkillMarketplaceProblem {
  path: string;
  name?: string;
  reason: string;
}

/** One skill version in a Team's release. */
export interface SkillMarketplaceReleaseSkill {
  name: string;
  version: string;
}

/** The publication of one policy-bound Team's selected skills. */
export interface SkillMarketplaceTeamPublication {
  name: string;
  uid: string;
  /** Identifies the Team's current selection. */
  selectionDigest: string;
  /** The current release, empty before the first publication. */
  release?: string;
  /** The selection of the current release. The Team's Agents can start once it equals selectionDigest. */
  releaseSelectionDigest?: string;
  publishedAt?: string;
  /** The skill versions in the current release. */
  skills?: SkillMarketplaceReleaseSkill[];
  /** Selected skills no longer valid in the repository; the release keeps their last published version. */
  retained?: string[];
  /** Selected skills that were never published. */
  missing?: string[];
  error?: string;
}

/** The marketplace's published state. Times are RFC 3339 UTC strings. */
export interface SkillMarketplaceCatalog {
  version: string;
  repository: SkillMarketplaceRepository;
  /** The last successfully synchronized commit. */
  commit?: string;
  syncedAt?: string;
  checkedAt?: string;
  /** The last failed check of the repository. A failed publication is the error of its Team. */
  error?: string;
  /** The valid skills at commit, sorted by name. */
  skills?: SkillMarketplaceSkill[];
  invalid?: SkillMarketplaceProblem[];
  /** The publication of each policy-bound Team, sorted by name. */
  teams?: SkillMarketplaceTeamPublication[];
  /** Lists were shortened to fit the catalog. */
  truncated?: boolean;
}

/** GET /api/v1/admin/skill-marketplace. */
export interface SkillMarketplace {
  /** Whether the deployment uses the Skill Marketplace. */
  enabled: boolean;
  /** False until the marketplace has published its catalog. */
  known: boolean;
  catalog: SkillMarketplaceCatalog | null;
}

async function errorMessage(response: Response, fallback: string): Promise<string> {
  try {
    const body = await response.json();
    return body?.error?.message || fallback;
  } catch {
    return fallback;
  }
}

export async function getSkillMarketplace(): Promise<SkillMarketplace> {
  const response = await apiFetch("/api/v1/admin/skill-marketplace");
  if (!response.ok) {
    throw new PortalError(await errorMessage(response, "Failed to load the Skill Marketplace"), response.status);
  }
  return response.json();
}
