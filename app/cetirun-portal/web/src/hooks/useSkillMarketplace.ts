// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useQuery } from "@tanstack/react-query";
import { getSkillMarketplace } from "../api/skillMarketplace";

/** The Skill Marketplace catalog, for administrators. */
export function useSkillMarketplace(enabled = true) {
  return useQuery({
    queryKey: ["admin", "skill-marketplace"],
    queryFn: getSkillMarketplace,
    enabled,
    staleTime: 30_000,
  });
}
