// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useQuery } from "@tanstack/react-query";
import { getFeatures } from "../api/features";

export function useFeatures() {
  return useQuery({
    queryKey: ["features"],
    queryFn: getFeatures,
    staleTime: Infinity, // Feature flags don't change at runtime.
  });
}
