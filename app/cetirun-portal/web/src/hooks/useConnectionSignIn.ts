// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useCallback, useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { getConnectionAuthorization, listConnections, type Connection, type ConnectionMode, type ConnectionStart } from "../api/toolAuthorization";

/** A popup notification is a hint. Only the owner-scoped transaction result can
 * select an account; this hook never creates an Agent association. */
export function useConnectionSignIn(scope: string) {
  const client = useQueryClient();
  const currentScope = useRef(scope);
  currentScope.current = scope;
  const cleanup = useRef<() => void>(() => {});
  const generation = useRef(0);
  const [pending, setPending] = useState(false);
  const [message, setMessage] = useState("");
  const cancel = useCallback(() => { generation.current++; cleanup.current(); setPending(false); }, []);
  useEffect(() => { cancel(); setMessage(""); return () => { generation.current++; cleanup.current(); }; }, [scope, cancel]);

  const begin = (start: () => Promise<ConnectionStart>, complete: (connection: Connection) => void, expectedMode: ConnectionMode = "delegated") => {
    cancel(); setMessage("");
    const popup = window.open("about:blank", "cetirun-account-signin", "popup,width=620,height=720");
    if (!popup) { setMessage("Allow popups for this Portal to sign in."); return; }
    const attempt = generation.current;
    const controller = new AbortController();
    let interval: ReturnType<typeof setInterval> | undefined;
    let listener: ((event: MessageEvent) => void) | undefined;
    let verifying = false;
    const active = () => generation.current === attempt && currentScope.current === scope && !controller.signal.aborted;
    cleanup.current = () => {
      controller.abort();
      if (interval) clearInterval(interval);
      if (listener) window.removeEventListener("message", listener);
      popup.close();
    };
    const finish = (message: string) => {
      if (!active()) return;
      cleanup.current(); generation.current++; setPending(false); setMessage(message);
    };
    setPending(true);
    void start().then((authorization) => {
      if (!active()) return;
      const expires = Date.parse(authorization.expiresAt);
      if (!authorization.authorizationId || !Number.isFinite(expires) || expires <= Date.now()) { finish("Sign-in expired. Try again."); return; }
      const verify = async (closed = false) => {
        if (!active() || verifying) return;
        verifying = true;
        try {
          const result = await getConnectionAuthorization(authorization.authorizationId, controller.signal);
          if (!active()) return;
          if (result.authorizationId !== authorization.authorizationId || result.status !== "complete" || !result.connectionId) {
            finish(closed && result.authorizationId === authorization.authorizationId && result.status === "pending"
              ? "Sign-in cancelled. No agent was connected."
              : "Sign-in could not be completed. Try again.");
            return;
          }
          const connections = await listConnections(controller.signal);
          if (!active()) return;
          const connection = connections.find((c) => c.id === result.connectionId && c.mode === expectedMode && (c.status === "connected" || (expectedMode === "installation" && c.status === "installation_required")));
          if (!connection) { finish("The signed-in account is unavailable. Try again."); return; }
          client.setQueryData(["tool-connections"], connections);
          void client.invalidateQueries({ queryKey: ["connection-usage"] });
          finish(""); complete(connection);
        } catch { finish("Could not verify sign-in. Try again."); }
      };
      listener = (event) => {
        if (event.origin !== window.location.origin || event.source !== popup || event.data?.type !== "cetirun-connection" || event.data.authorizationId !== authorization.authorizationId) return;
        void verify();
      };
      window.addEventListener("message", listener);
      interval = setInterval(() => {
        if (Date.now() >= expires) finish("Sign-in expired. Try again.");
        // The callback closes immediately after posting its message. Verify the
        // transaction even if this timer wins the race with message delivery.
        else if (popup.closed) void verify(true);
      }, 300);
      popup.location.replace(authorization.authorizationUrl);
    }).catch(() => finish("Sign-in could not be started. Try again."));
  };
  return { begin, cancel, pending, message };
}
