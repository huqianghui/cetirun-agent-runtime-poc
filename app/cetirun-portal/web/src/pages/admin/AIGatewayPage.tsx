// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Link, Text, tokens } from "@fluentui/react-components";

// The ingress routes /ai-gateway/ to the AI Gateway's LiteLLM admin console,
// which signs administrators in with its own account.
const CONSOLE_URL = "/ai-gateway/ui/";

export function AIGatewayPage() {
  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100%", gap: "8px" }}>
      <div style={{ display: "flex", alignItems: "baseline", justifyContent: "space-between" }}>
        <Text size={500} weight="semibold">
          AI Gateway
        </Text>
        <Link href={CONSOLE_URL} target="_blank" rel="noopener noreferrer">
          Open in a new tab
        </Link>
      </div>
      <iframe
        title="AI Gateway console"
        src={CONSOLE_URL}
        style={{
          flex: 1,
          minHeight: "600px",
          width: "100%",
          border: `1px solid ${tokens.colorNeutralStroke1}`,
          borderRadius: "4px",
          background: tokens.colorNeutralBackground1,
        }}
      />
    </div>
  );
}
