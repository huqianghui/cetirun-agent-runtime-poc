// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { MessageBar, Text } from "@fluentui/react-components";
import { AdminConfigurationError } from "../api/adminError";

export function AdminConfigurationErrorMessage({ error }: { error: Error }) {
  const details = error instanceof AdminConfigurationError ? error.details : [];
  return <MessageBar intent="error" aria-label="Configuration save error">
    <Text>{error.message}</Text>
    {details.length > 0 && <ul style={{ margin: "8px 0 0", paddingLeft: 24 }}>
      {details.map((detail, index) => {
        const context = [detail.team && `Team ${detail.team.name}`, detail.policy && `Policy ${detail.policy.name}`, detail.sandbox && `Sandbox ${detail.sandbox.name}`].filter(Boolean).join(" / ");
        return <li key={`${detail.code}-${detail.field || ""}-${index}`}>
          {context && `${context}: `}{detail.field && `${detail.field}: `}{detail.message}
        </li>;
      })}
    </ul>}
  </MessageBar>;
}
