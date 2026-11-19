// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useRef, useState } from "react";
import {
  Button,
  DrawerBody,
  DrawerHeader,
  DrawerHeaderTitle,
  OverlayDrawer,
  Spinner,
} from "@fluentui/react-components";
import { Dismiss24Regular } from "@fluentui/react-icons";
import { clearAccount, forAgent, listConversations } from "./store";
import { type ChatMessage, type Conversation, type Turn } from "./types";
import { notify, stopScope, withAgentLock } from "./runtime";

export default function HistoryDrawer({
  scope,
  open,
  onClose,
}: {
  scope: string;
  open: boolean;
  onClose: () => void;
}) {
  const [items, setItems] = useState<Conversation[]>([]);
  const [cursor, setCursor] = useState<IDBValidKey>();
  const [selected, setSelected] = useState<Conversation>();
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [turns, setTurns] = useState<Turn[]>([]);
  const [persistence, setPersistence] = useState<string>();
  const [requestingPersistence, setRequestingPersistence] = useState(false);
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(false);
  const [confirmClear, setConfirmClear] = useState(false);
  const sequence = useRef(0);
  const refresh = async () => {
    const current = sequence.current;
    const value = await listConversations(scope);
    if (current !== sequence.current) return;
    setItems(value.items);
    setCursor(value.cursor);
  };
  useEffect(() => {
    if (!open) return;
    sequence.current++;
    setLoading(true);
    setSelected(undefined);
    setMessages([]);
    setTurns([]);
    setError(undefined);
    void refresh()
      .catch((reason) => setError(String(reason)))
      .finally(() => setLoading(false));
    return () => {
      sequence.current++;
    };
  }, [scope, open]);
  const show = async (value: Conversation) => {
    const current = ++sequence.current;
    if (value.scope !== scope) throw new Error("The local account changed.");
    const historyStore = forAgent(scope, value.agentUid);
    const [rows, history] = await Promise.all([
      historyStore.listMessages(value.id),
      historyStore.conversationHistory(value.id),
    ]);
    if (current !== sequence.current) return;
    setSelected(value);
    setMessages(rows);
    setTurns(history.turns);
  };
  const remove = async (value: Conversation) => {
    try {
      if (value.scope !== scope) throw new Error("The local account changed.");
      const historyStore = forAgent(scope, value.agentUid);
      await withAgentLock(scope, value.agentUid, async () => {
        await historyStore.recoverTurns(value.id);
        const current = await historyStore.getConversation(value.id);
        if (current) await historyStore.deleteConversation(current);
      });
      setSelected(undefined);
      setMessages([]);
      await refresh();
      notify({
        kind: "changed",
        scope,
        agentUid: value.agentUid,
        conversationId: value.id,
      });
    } catch (reason) {
      setError(
        reason instanceof Error
          ? reason.message
          : "Could not delete this conversation.",
      );
    }
  };
  const clear = async () => {
    sequence.current++;
    try {
      stopScope(scope);
      await clearAccount(scope);
      notify({ kind: "cleared", scope });
      setItems([]);
      setCursor(undefined);
      setSelected(undefined);
      setMessages([]);
      setConfirmClear(false);
    } catch {
      setError(
        "Local records could not be cleared. They have not been reported as deleted.",
      );
    }
  };
  const requestPersistence = async () => {
    setRequestingPersistence(true);
    try {
      const granted = await navigator.storage.persist();
      setPersistence(
        granted
          ? "Automatic eviction protection is enabled for this site. It is still not a backup."
          : "The browser did not grant eviction protection. Your history still uses normal browser storage.",
      );
    } catch {
      setPersistence(
        "Eviction protection could not be requested. No records were changed.",
      );
    } finally {
      setRequestingPersistence(false);
    }
  };
  return (
    <OverlayDrawer
      open={open}
      onOpenChange={(_, data) => {
        if (!data.open) onClose();
      }}
      position="end"
      size="large"
    >
      <DrawerHeader>
        <DrawerHeaderTitle
          action={
            <Button
              appearance="subtle"
              icon={<Dismiss24Regular />}
              aria-label="Close local history"
              onClick={onClose}
            />
          }
        >
          Local chat history
        </DrawerHeaderTitle>
      </DrawerHeader>
      <DrawerBody>
        <p style={{ fontSize: 13, lineHeight: 1.7 }}>
          These records belong to this account on this browser. Viewing them
          does not start an Agent or grant access to an instance.
        </p>
        <p style={{ fontSize: 12, lineHeight: 1.6 }}>
          History is not encrypted. People sharing this browser profile may read
          it. Private browsing, site-data cleanup or browser eviction can remove
          it.
        </p>
        {typeof navigator.storage?.persist === "function" && (
          <Button
            size="small"
            disabled={requestingPersistence}
            onClick={() => void requestPersistence()}
          >
            Request eviction protection
          </Button>
        )}
        {persistence && <p role="status">{persistence}</p>}
        {error && <p role="alert">{error}</p>}
        {loading && <Spinner size="small" />}
        {selected ? (
          <>
            <Button
              appearance="subtle"
              onClick={() => {
                setSelected(undefined);
                setMessages([]);
              }}
            >
              Back to conversations
            </Button>
            <h2>{selected.title}</h2>
            <p style={{ fontSize: 12 }}>
              Read-only · {selected.instanceName} ·{" "}
              {selected.agentUid.slice(0, 8)}
            </p>
            {messages[0]?.seq > 0 && (
              <Button
                onClick={() =>
                  void forAgent(scope, selected.agentUid)
                    .listMessages(selected.id, messages[0].seq)
                    .then((rows) => setMessages((old) => [...rows, ...old]))
                }
              >
                Load earlier messages
              </Button>
            )}
            {messages.map((message) => (
              <div
                key={message.seq}
                style={{
                  padding: "16px 0",
                  whiteSpace: "pre-wrap",
                  overflowWrap: "anywhere",
                  fontSize: 14,
                  lineHeight: 1.7,
                }}
              >
                <strong>{message.role === "user" ? "You" : "Agent"}</strong>
                <div>{message.text || "No text received."}</div>
                {message.role === "assistant" && (
                  <small>
                    {turns.find((turn) => turn.id === message.turnId)
                      ?.resultState === "completed"
                      ? "Completed"
                      : "Not completed · execution may be uncertain"}
                  </small>
                )}
              </div>
            ))}
            {selected.draft && (
              <div
                style={{
                  whiteSpace: "pre-wrap",
                  overflowWrap: "anywhere",
                  padding: "16px 0",
                }}
              >
                <strong>Unsent draft</strong>
                <div>{selected.draft}</div>
              </div>
            )}
            <Button onClick={() => void remove(selected)}>
              Delete this local conversation
            </Button>
          </>
        ) : (
          <>
            {items.map((item) => (
              <Button
                key={`${item.agentUid}:${item.id}`}
                appearance="subtle"
                style={{
                  display: "flex",
                  width: "100%",
                  justifyContent: "flex-start",
                  padding: "12px",
                  marginBottom: 4,
                }}
                onClick={() =>
                  void show(item).catch((reason) => setError(String(reason)))
                }
              >
                <span style={{ textAlign: "left" }}>
                  {item.title}
                  <span
                    style={{ display: "block", fontSize: 11, opacity: 0.7 }}
                  >
                    {item.instanceName} ·{" "}
                    {new Date(item.updatedAt).toLocaleDateString()}
                  </span>
                </span>
              </Button>
            ))}
            {!items.length && !loading && <p>No local conversations yet.</p>}
            {cursor && (
              <Button
                onClick={() =>
                  void listConversations(scope, undefined, cursor).then(
                    (value) => {
                      setItems((old) => [...old, ...value.items]);
                      setCursor(value.cursor);
                    },
                  )
                }
              >
                Load more
              </Button>
            )}
            {items.length > 0 && (
              <div
                style={{
                  marginTop: 24,
                  borderTop: "1px solid var(--colorNeutralStroke2)",
                  paddingTop: 16,
                }}
              >
                {confirmClear ? (
                  <>
                    <p>
                      Clear all chat records for this account on this browser?
                      Active requests will stop receiving. Agent files and
                      memory are unchanged.
                    </p>
                    <Button onClick={() => setConfirmClear(false)}>
                      Cancel
                    </Button>
                    <Button appearance="primary" onClick={() => void clear()}>
                      Clear this account's history
                    </Button>
                  </>
                ) : (
                  <Button onClick={() => setConfirmClear(true)}>
                    Clear this account's local history
                  </Button>
                )}
              </div>
            )}
          </>
        )}
      </DrawerBody>
    </OverlayDrawer>
  );
}
