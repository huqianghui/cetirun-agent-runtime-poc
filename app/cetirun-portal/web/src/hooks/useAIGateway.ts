// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useCallback, useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  cancelCopilotLogin,
  getAIGatewayCatalog,
  pollCopilotLogin,
  PortalError,
  startCopilotLogin,
  type CopilotLogin,
} from "../api/aiGateway";

export function useAIGatewayCatalog(enabled: boolean) {
  return useQuery({
    queryKey: ["ai-gateway", "catalog"],
    queryFn: getAIGatewayCatalog,
    enabled,
    staleTime: 60_000,
  });
}

const MIN_POLL_SECONDS = 5;

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** A client error (4xx) will not go away by polling again. */
function isFinal(err: unknown): boolean {
  return err instanceof PortalError && err.status >= 400 && err.status < 500;
}

/**
 * Runs the Portal's GitHub Copilot sign-in for an Agent: start, poll at the
 * interval GitHub asks for, cancel. Start and cancel name the Agent's UID.
 * The Portal backend keeps the sign-in, so when `resume` is set a sign-in
 * still pending after a reload is picked up.
 */
export function useCopilotGatewayLogin(instanceName: string, uid: string, resume = false) {
  const queryClient = useQueryClient();
  const [login, setLogin] = useState<CopilotLogin | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  // Each start, cancel or reset begins a new generation; a poll answered for
  // an earlier one is dropped instead of reopening a closed sign-in.
  const generation = useRef(0);
  const active = useRef(true);

  useEffect(() => {
    active.current = true;
    return () => {
      active.current = false;
      generation.current += 1;
      clearTimeout(timer.current);
    };
  }, []);

  const schedule = useCallback(
    (current: CopilotLogin, gen: number) => {
      clearTimeout(timer.current);
      if (current.state !== "pending") {
        if (current.state === "succeeded") {
          void queryClient.invalidateQueries({ queryKey: ["instance", instanceName] });
        }
        return;
      }
      // Keep asking while the Portal reports the sign-in pending: it reports
      // an outcome once the code expires, and once a completion's lease ends.
      // A sign-in whose token is being stored is pending past its code's
      // expiry, so that expiry is no reason to stop.
      const seconds = Math.max(current.interval ?? MIN_POLL_SECONDS, MIN_POLL_SECONDS);
      timer.current = setTimeout(async () => {
        try {
          const next = await pollCopilotLogin(instanceName);
          if (!active.current || gen !== generation.current) return;
          setLogin(next);
          setError(null);
          schedule(next, gen);
        } catch (err) {
          if (!active.current || gen !== generation.current) return;
          setError(message(err));
          if (!isFinal(err)) schedule(current, gen);
        }
      }, seconds * 1000);
    },
    [instanceName, queryClient],
  );

  useEffect(() => {
    if (!resume) return;
    const gen = generation.current;
    pollCopilotLogin(instanceName)
      .then((current) => {
        if (!active.current || gen !== generation.current || current.state !== "pending") return;
        setLogin(current);
        schedule(current, gen);
      })
      .catch(() => {
        // Nothing to resume.
      });
  }, [instanceName, resume, schedule]);

  const start = useCallback(async () => {
    const gen = ++generation.current;
    clearTimeout(timer.current);
    setBusy(true);
    setError(null);
    try {
      const started = await startCopilotLogin(instanceName, uid);
      if (!active.current || gen !== generation.current) return;
      setLogin(started);
      schedule(started, gen);
    } catch (err) {
      if (active.current && gen === generation.current) setError(message(err));
    } finally {
      if (active.current) setBusy(false);
    }
  }, [instanceName, uid, schedule]);

  const cancel = useCallback(async () => {
    const gen = ++generation.current;
    clearTimeout(timer.current);
    setLogin(null);
    setError(null);
    try {
      await cancelCopilotLogin(instanceName, uid);
    } catch (err) {
      if (!active.current || gen !== generation.current) return;
      setError(message(err));
      // The Portal refused the cancel, as it does while the sign-in's token
      // is being stored: read the sign-in again and follow it to its outcome.
      try {
        const current = await pollCopilotLogin(instanceName);
        if (!active.current || gen !== generation.current) return;
        if (current.state !== "none") setLogin(current);
        schedule(current, gen);
      } catch (readErr) {
        if (active.current && gen === generation.current && !isFinal(readErr)) {
          schedule({ state: "pending" }, gen);
        }
      }
    }
  }, [instanceName, uid, schedule]);

  const reset = useCallback(() => {
    generation.current += 1;
    clearTimeout(timer.current);
    setLogin(null);
    setError(null);
  }, []);

  return { login, error, busy, start, cancel, reset };
}
