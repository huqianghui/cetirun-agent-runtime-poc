// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useQueryClient } from "@tanstack/react-query";
import { flushSync } from "react-dom";
import { fetchMe } from "../api/me";
import { clearAccount } from "./store";
import { notify, stopAll, stopScope, subscribe } from "./runtime";

interface AccountContext {
  scope?: string;
  checking: boolean;
  error?: string;
  storageError?: string;
  verify: () => Promise<string>;
  invalidate: (expectedScope: string) => void;
  signOut: (clear: boolean) => Promise<void>;
}
const Context = createContext<AccountContext | undefined>(undefined);
export function ChatAccountProvider({ children }: { children: ReactNode }) {
  const cache = useQueryClient();
  const [scope, setScope] = useState<string>();
  const [checking, setChecking] = useState(true);
  const [error, setError] = useState<string>();
  const [storageError, setStorageError] = useState<string>();
  const current = useRef<string | undefined>(undefined);
  const sequence = useRef(0);
  const signedOut = useRef(false);
  const verifying = useRef(false);
  const invalidate = useCallback(
    (expectedScope: string) => {
      if (current.current !== expectedScope || verifying.current) return;
      sequence.current++;
      stopAll();
      current.current = undefined;
      setScope(undefined);
      setChecking(false);
      setError("Sign in again to view local history.");
      cache.clear();
    },
    [cache],
  );
  const verify = useCallback(async () => {
    if (signedOut.current)
      throw new Error("Sign in again to view local history.");
    const seq = ++sequence.current;
    verifying.current = true;
    setChecking(true);
    try {
      const me = await fetchMe(AbortSignal.timeout(5000));
      if (seq !== sequence.current || signedOut.current)
        throw new Error("Identity verification changed.");
      const next = me.chatStorageScope;
      if (current.current && current.current !== next) {
        stopScope(current.current);
        cache.clear();
      }
      current.current = next;
      verifying.current = false;
      setScope(next);
      setError(
        next ? undefined : "Chat identity is not configured for this Portal.",
      );
      setChecking(false);
      cache.setQueryData(["me"], me);
      return next ?? "";
    } catch (reason) {
      if (seq === sequence.current) {
        verifying.current = false;
        stopAll();
        current.current = undefined;
        setScope(undefined);
        setChecking(false);
        cache.clear();
        setError(
          reason instanceof Error
            ? reason.message
            : "Sign in again to use Chat.",
        );
      }
      throw reason;
    }
  }, [cache]);
  useEffect(() => {
    const refresh = () => {
      void verify().catch(() => {});
    };
    const hide = () => {
      stopAll();
      flushSync(() => setChecking(true));
    };
    const closed = () => {
      stopAll();
      setStorageError(
        "Chat storage changed. Refresh this page before sending.",
      );
    };
    refresh();
    window.addEventListener("focus", refresh);
    window.addEventListener("pageshow", refresh);
    window.addEventListener("pagehide", hide);
    window.addEventListener("cetirun-chat-storage-closed", closed);
    const unsubscribe = subscribe((notice) => {
      if (notice.scope === current.current && notice.kind === "signed-out") {
        signedOut.current = true;
        sequence.current++;
        stopAll();
        setScope(undefined);
        setChecking(false);
        setError("Signed out. Reload to sign in again.");
        cache.clear();
      }
    });
    return () => {
      sequence.current++;
      stopAll();
      unsubscribe();
      window.removeEventListener("focus", refresh);
      window.removeEventListener("pageshow", refresh);
      window.removeEventListener("pagehide", hide);
      window.removeEventListener("cetirun-chat-storage-closed", closed);
    };
  }, [verify, cache]);
  const signOut = useCallback(
    async (clear: boolean) => {
      const value = current.current;
      if (clear && value) {
        stopScope(value);
        await clearAccount(value);
        notify({ kind: "cleared", scope: value });
        if (current.current !== value)
          throw new Error("Chat identity changed. Try signing out again.");
      }
      signedOut.current = true;
      sequence.current++;
      stopAll();
      setScope(undefined);
      setChecking(false);
      if (value) notify({ kind: "signed-out", scope: value });
      cache.clear();
    },
    [cache],
  );
  return (
    <Context.Provider
      value={{
        scope,
        checking,
        error,
        storageError,
        verify,
        invalidate,
        signOut,
      }}
    >
      {children}
    </Context.Provider>
  );
}
export const useChatAccount = () => useContext(Context);
