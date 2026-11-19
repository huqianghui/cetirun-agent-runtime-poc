// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import {
  Badge,
  Text,
  Spinner,
  tokens,
} from "@fluentui/react-components";
import type { StatusResponse } from "../types/instance";
import { thStyle, tdStyle } from "./tableStyles";

function conditionBadgeColor(status: string): "success" | "danger" | "informative" {
  if (status === "True") return "success";
  if (status === "False") return "danger";
  return "informative";
}

function phaseBadgeColor(phase: string): "success" | "warning" | "danger" | "informative" {
  if (phase === "Running") return "success";
  if (phase === "Pending") return "warning";
  if (phase === "Failed" || phase === "Unknown") return "danger";
  return "informative";
}

export function StatusTab({
  data,
  isLoading,
}: {
  data: StatusResponse | undefined;
  isLoading: boolean;
}) {
  if (isLoading) {
    return <Spinner size="small" label="Loading status..." />;
  }
  if (!data) {
    return <Text>No status data available.</Text>;
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 24, paddingTop: 12 }}>
      {/* Conditions */}
      <section>
        <Text weight="semibold" size={400} as="h3" style={{ marginBottom: 8, display: "block" }}>
          Conditions
        </Text>
        {data.conditions.length === 0 ? (
          <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>
            No conditions reported yet.
          </Text>
        ) : (
          <div style={{ overflowX: "auto" }}>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
              <thead>
                <tr style={{ borderBottom: `1px solid ${tokens.colorNeutralStroke1}` }}>
                  <th style={thStyle}>Type</th>
                  <th style={thStyle}>Status</th>
                  <th style={thStyle}>Reason</th>
                  <th style={thStyle}>Message</th>
                </tr>
              </thead>
              <tbody>
                {data.conditions.map((c) => (
                  <tr key={c.type} style={{ borderBottom: `1px solid ${tokens.colorNeutralStroke2}` }}>
                    <td style={tdStyle}>
                      <Text weight="semibold" size={200}>{c.type}</Text>
                    </td>
                    <td style={tdStyle}>
                      <Badge appearance="filled" color={conditionBadgeColor(c.status)}>
                        {c.status}
                      </Badge>
                    </td>
                    <td style={tdStyle}>
                      <Text size={200}>{c.reason}</Text>
                    </td>
                    <td style={tdStyle}>
                      <Text size={200}>{c.message}</Text>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {/* Pods */}
      <section>
        <Text weight="semibold" size={400} as="h3" style={{ marginBottom: 8, display: "block" }}>
          Pods
        </Text>
        {data.pods.length === 0 ? (
          <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>
            No pods found.
          </Text>
        ) : (
          <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
            {data.pods.map((pod) => (
              <div
                key={pod.name}
                style={{
                  border: `1px solid ${tokens.colorNeutralStroke1}`,
                  borderRadius: tokens.borderRadiusMedium,
                  padding: 12,
                  display: "flex",
                  alignItems: "center",
                  gap: 8,
                }}
              >
                <Text weight="semibold" size={300}>{pod.name}</Text>
                <Badge appearance="filled" color={phaseBadgeColor(pod.phase)}>
                  {pod.phase}
                </Badge>
                <Badge appearance="filled" color={pod.ready ? "success" : "danger"}>
                  {pod.ready ? "Ready" : "Not Ready"}
                </Badge>
                {pod.restartCount > 0 && (
                  <Text size={200} style={{ color: tokens.colorStatusWarningForeground1 }}>
                    Restarts: {pod.restartCount}
                  </Text>
                )}
              </div>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}
