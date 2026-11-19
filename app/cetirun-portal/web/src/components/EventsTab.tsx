// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import {
  Badge,
  Text,
  Spinner,
  tokens,
} from "@fluentui/react-components";
import type { EventItem, StatusResponse } from "../types/instance";
import { thStyle, tdStyle } from "./tableStyles";

function EventList({ events, title }: { events: EventItem[]; title: string }) {
  return (
    <section>
      <Text weight="semibold" size={400} as="h3" style={{ marginBottom: 8, display: "block" }}>
        {title}
      </Text>
      {events.length === 0 ? (
        <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>
          No events.
        </Text>
      ) : (
        <div style={{ overflowX: "auto" }}>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
            <thead>
              <tr style={{ borderBottom: `1px solid ${tokens.colorNeutralStroke1}` }}>
                <th style={thStyle}>Type</th>
                <th style={thStyle}>Reason</th>
                <th style={thStyle}>Message</th>
                <th style={thStyle}>Source</th>
                <th style={thStyle}>Count</th>
                <th style={thStyle}>Last Seen</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e, i) => (
                <tr
                  key={`${e.reason}-${e.lastSeen}-${i}`}
                  style={{
                    borderBottom: `1px solid ${tokens.colorNeutralStroke2}`,
                    backgroundColor:
                      e.type === "Warning"
                        ? tokens.colorStatusWarningBackground1
                        : undefined,
                  }}
                >
                  <td style={tdStyle}>
                    <Badge
                      appearance="filled"
                      color={e.type === "Warning" ? "warning" : "informative"}
                      size="small"
                    >
                      {e.type}
                    </Badge>
                  </td>
                  <td style={tdStyle}>
                    <Text weight="semibold" size={200}>{e.reason}</Text>
                  </td>
                  <td style={tdStyle}>
                    <Text size={200}>{e.message}</Text>
                  </td>
                  <td style={tdStyle}>
                    <Text size={200} style={{ fontFamily: "monospace" }}>{e.source}</Text>
                  </td>
                  <td style={tdStyle}>
                    <Text size={200}>{e.count}</Text>
                  </td>
                  <td style={tdStyle}>
                    <Text size={200} style={{ whiteSpace: "nowrap" }}>
                      {e.lastSeen ? new Date(e.lastSeen).toLocaleString() : "-"}
                    </Text>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export function EventsTab({
  data,
  isLoading,
}: {
  data: StatusResponse | undefined;
  isLoading: boolean;
}) {
  if (isLoading) {
    return <Spinner size="small" label="Loading events..." />;
  }
  if (!data) {
    return <Text>No event data available.</Text>;
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 24, paddingTop: 12 }}>
      <EventList events={data.events} title="Instance Events" />
      <EventList events={data.nsEvents} title="Namespace Events" />
    </div>
  );
}
