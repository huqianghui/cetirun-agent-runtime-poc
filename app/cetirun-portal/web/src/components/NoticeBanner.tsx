// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useState } from "react";
import {
  MessageBar,
  MessageBarBody,
  MessageBarActions,
  Button,
} from "@fluentui/react-components";
import { DismissRegular } from "@fluentui/react-icons";

export function NoticeBanner() {
  const [message, setMessage] = useState<string | null>(null);
  const [dismissed, setDismissed] = useState(false);

  useEffect(() => {
    const controller = new AbortController();

    fetch("/api/v1/config", { signal: controller.signal })
      .then((r) => {
        const contentType = r.headers.get("content-type");
        if (!r.ok || !contentType || !contentType.includes("application/json")) {
          return null;
        }
        return r.json();
      })
      .then((data) => {
        if (data && typeof data.noticeBanner === "string" && data.noticeBanner) {
          setMessage(data.noticeBanner);
        }
      })
      .catch((error) => {
        if (error instanceof DOMException && error.name === "AbortError") {
          return;
        }
      });

    return () => {
      controller.abort();
    };
  }, []);

  if (!message || dismissed) {
    return null;
  }

  return (
    <MessageBar intent="warning">
      <MessageBarBody>{message}</MessageBarBody>
      <MessageBarActions>
        <Button
          appearance="transparent"
          icon={<DismissRegular />}
          aria-label="Dismiss notice"
          onClick={() => setDismissed(true)}
        />
      </MessageBarActions>
    </MessageBar>
  );
}
