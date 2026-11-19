// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Text, tokens } from "@fluentui/react-components";
import type { AgentPolicyRateLimits } from "../types/rateLimits";
import { describeRateLimits } from "../utils/rateLimits";

/**
 * The AI Gateway limits a policy sets on each Agent's own key, read-only.
 * Renders nothing when the policy sets no limit.
 */
export function RateLimitsSummary({ limits, intro }: { limits?: AgentPolicyRateLimits; intro: string }) {
  const lines = describeRateLimits(limits);
  if (lines.length === 0) return null;
  return (
    <div role="group" style={{ display: "flex", flexDirection: "column", gap: 2 }} aria-label="AI Gateway limits">
      <Text size={200} weight="semibold">Limits</Text>
      <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>{intro}</Text>
      <ul style={{ margin: 0, paddingLeft: 20 }}>
        {lines.map((line) => (
          <li key={line}><Text size={200}>{line}</Text></li>
        ))}
      </ul>
    </div>
  );
}
