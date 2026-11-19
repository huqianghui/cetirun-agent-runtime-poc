// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Text, tokens } from "@fluentui/react-components";

export function ForbiddenPage({ message }: { message?: string }) {
  return (
    <div
      style={{
        display: "flex",
        flexDirection: "column",
        alignItems: "center",
        justifyContent: "center",
        flex: 1,
        gap: "8px",
        padding: "48px",
      }}
    >
      <Text size={700} weight="semibold">
        403 Forbidden
      </Text>
      <Text size={400} style={{ color: tokens.colorNeutralForeground3 }}>
        {message || "You do not have access to this page."}
      </Text>
    </div>
  );
}
