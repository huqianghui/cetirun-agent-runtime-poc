// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useQuery } from "@tanstack/react-query";
import type { TeamSummary } from "../types/instance";

async function fetchTeams(): Promise<TeamSummary[]> {
  const res = await fetch("/api/v1/teams");
  if (!res.ok) throw new Error(`Failed to fetch teams: ${res.status}`);
  return res.json();
}

export function useTeams() {
  return useQuery({
    queryKey: ["teams"],
    queryFn: fetchTeams,
    staleTime: 30_000,
  });
}
