// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  Dialog,
  DialogActions,
  DialogBody,
  DialogContent,
  DialogSurface,
  DialogTitle,
  Input,
  Select,
  Spinner,
  Textarea,
} from "@fluentui/react-components";
import {
  Add20Regular,
  ArrowLeft20Regular,
  ArrowUp20Filled,
  ArrowSync20Regular,
  Chat20Regular,
  Checkmark16Regular,
  Copy20Regular,
  Delete20Regular,
  Dismiss20Regular,
  Edit20Regular,
  PanelLeft20Regular,
  Stop20Filled,
} from "@fluentui/react-icons";
import { useNavigate, useSearchParams } from "react-router-dom";
import type { Instance } from "../types/instance";
import { useChatAccount } from "./ChatAccount";
import { ChatError, chatModels, describeChat, sendChat } from "./api";
import { ResponseCheckpoint } from "./checkpoint";
import * as storage from "./store";
import {
  browserSupport,
  flight,
  notify,
  subscribe,
  withAgentLock,
} from "./runtime";
import {
  byteLength,
  buildContext,
  LIMITS,
  type ChatDescription,
  type ChatMessage,
  type Conversation,
  type Turn,
} from "./types";
import "./chat.css";

const reasonText: Record<string, string> = {
  suspended: "This Agent is paused. Your local history is still here.",
  updating:
    "The Agent is updating. You can send when its new configuration is ready.",
  sandbox_held: "Platform policy is holding this Agent. Chat cannot start it.",
  runtime_not_ready: "Waiting for the Agent to become ready.",
  network_unavailable: "Private Chat access has not been configured.",
  contract_unsupported:
    "This Agent needs a runtime Chart that supports Portal Chat.",
  not_chat_agent: "This instance does not serve the Custom Chat protocol.",
};
const errorText = (value: unknown) =>
  value instanceof Error
    ? value.message
    : "Chat could not complete. Your request will not be retried.";

const MessageView = memo(function MessageView({
  message,
  text,
  name,
  state,
}: {
  message: ChatMessage;
  text: string;
  name: string;
  state?: string;
}) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      setCopied(false);
    }
  };
  return (
    <article
      className={`chat-message chat-message--${message.role}`}
      aria-label={message.role === "user" ? "Your message" : `${name} response`}
    >
      <div className="chat-message-label">
        {message.role === "user" ? "You" : name}
      </div>
      <div className="chat-message-text">
        {text ||
          (state === "pending" ? (
            <span className="chat-thinking">
              Working<span aria-hidden="true">…</span>
            </span>
          ) : (
            "No text received."
          ))}
      </div>
      <div className="chat-message-actions">
        <Button
          appearance="subtle"
          size="small"
          icon={copied ? <Checkmark16Regular /> : <Copy20Regular />}
          aria-label="Copy message"
          onClick={() => void copy()}
          disabled={!text}
        />
        {state && state !== "completed" && (
          <span className={`chat-result chat-result--${state}`}>
            {state === "pending"
              ? "Receiving"
              : state === "interrupted"
                ? "Interrupted · execution may continue"
                : state === "incomplete"
                  ? "Incomplete response"
                  : state === "failed"
                    ? "Request did not complete"
                    : state === "unsaved"
                      ? "Not saved on this browser"
                      : state}
          </span>
        )}
      </div>
    </article>
  );
});

export default function ChatPage({ instance }: { instance: Instance }) {
  const account = useChatAccount();
  const scope = account?.scope;
  const store = useMemo(
    () => storage.forAgent(scope, instance.uid),
    [scope, instance.uid],
  );
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const conversationID = params.get("conversation") || null;
  const lastURL = useRef(conversationID);
  const pendingNavigation = useRef<string | null | undefined>(undefined);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [cursor, setCursor] = useState<IDBValidKey>();
  const [selected, setSelected] = useState<Conversation>();
  const selectedRef = useRef<Conversation | undefined>(undefined);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [turns, setTurns] = useState<Turn[]>([]);
  const contextHistory = useRef<
    Awaited<ReturnType<typeof store.conversationHistory>>
  >({ history: [], turns: [], completedCount: 0 });
  const [draft, setDraft] = useState("");
  const draftRef = useRef(draft);
  draftRef.current = draft;
  const [model, setModel] = useState("");
  const [models, setModels] = useState<string[]>([]);
  const [description, setDescription] = useState<ChatDescription>();
  const [loading, setLoading] = useState(true);
  const [switching, setSwitching] = useState(false);
  const switchingRef = useRef(false);
  const [readyStorage, setReadyStorage] = useState(false);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [live, setLive] = useState<{ turnId: string; text: string }>();
  const [saved, setSaved] = useState(true);
  const [railOpen, setRailOpen] = useState(false);
  const [rename, setRename] = useState(false);
  const [title, setTitle] = useState("");
  const [deleteOpen, setDeleteOpen] = useState(false);
  const storageEpoch = useRef<number | undefined>(undefined);
  const [contextCount, setContextCount] = useState({ included: 0, omitted: 0 });
  const controllerRef = useRef<AbortController | undefined>(undefined);
  const draftWork = useRef<Promise<unknown>>(Promise.resolve());
  const draftTimer = useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined,
  );
  const transcript = useRef<HTMLDivElement>(null);
  const follow = useRef(true);
  const mounted = useRef(true);
  const name = instance.displayName.trim() || instance.instanceName;
  const support = browserSupport() ?? account?.storageError;
  const checkIdentityError = useCallback(
    (reason: unknown) => {
      if (
        reason instanceof ChatError &&
        (reason.status === 401 ||
          ["identity_changed", "not_authenticated"].includes(reason.code))
      )
        account?.invalidate(scope ?? "");
    },
    [account?.invalidate, scope],
  );
  const choose = useCallback((value: Conversation | undefined) => {
    selectedRef.current = value;
    setSelected(value);
  }, []);
  const refreshList = useCallback(async () => {
    if (!scope || !mounted.current) return;
    const result = await store.listConversations();
    if (!mounted.current) return;
    setConversations(result.items);
    setCursor(result.cursor);
  }, [scope, instance.uid]);
  const loadConversation = useCallback(
    async (value: Conversation) => {
      if (!scope || busyRef.current) return;
      await withAgentLock(scope, instance.uid, () =>
        store.recoverTurns(value.id),
      ).catch(() => {});
      const current = await store.getConversation(value.id);
      if (!current) return;
      const [rows, history] = await Promise.all([
        store.listMessages(current.id),
        store.conversationHistory(current.id),
      ]);
      choose(current);
      setMessages(rows);
      setTurns(history.turns);
      contextHistory.current = history;
      setDraft(current.draft);
      setModel(
        !models.length || models.includes(current.model)
          ? current.model
          : models.length === 1
            ? models[0]
            : "",
      );
      setLive(undefined);
      setSaved(true);
      setError(undefined);
      setRailOpen(false);
      follow.current = true;
      const included = Math.min(history.completedCount, 49);
      setContextCount({ included, omitted: history.completedCount - included });
    },
    [scope, instance.uid, choose, models],
  );
  const refreshTarget = useCallback(
    async (signal?: AbortSignal) => {
      if (!scope) return;
      const target = { scope, name: instance.instanceName, uid: instance.uid };
      const value = await describeChat(target, signal).catch((reason) => {
        checkIdentityError(reason);
        throw reason;
      });
      setDescription(value);
      if (value.available && value.bindingRevision) {
        const ids = await chatModels(
          { ...target, binding: value.bindingRevision },
          signal,
        ).catch((reason) => {
          setModels([]);
          setModel("");
          checkIdentityError(reason);
          throw reason;
        });
        setModels(ids);
        setModel((old) =>
          ids.includes(old) ? old : ids.length === 1 ? ids[0] : "",
        );
      } else setModels([]);
    },
    [scope, instance.instanceName, instance.uid, checkIdentityError],
  );
  const flushDraft = useCallback(async () => {
    clearTimeout(draftTimer.current);
    await draftWork.current;
    if (!scope || !mounted.current) return;
    const epoch = storageEpoch.current;
    if (!readyStorage || epoch === undefined)
      throw new Error(
        "Local history is unavailable. Keep your draft before leaving.",
      );
    let value = selectedRef.current;
    const text = draftRef.current;
    if (!value && !text) return;
    if (!value) {
      value = await store.createConversation(instance.instanceName, epoch);
      if (!mounted.current || epoch !== storageEpoch.current) return;
      choose(value);
    }
    if (value.draft === text && value.model === model) return;
    const updated = await store.updateConversation(value, {
      draft: text,
      model,
    });
    if (mounted.current && selectedRef.current?.id === updated.id) {
      choose(updated);
      setSaved(true);
      await refreshList();
    }
  }, [
    scope,
    readyStorage,
    model,
    instance.uid,
    instance.instanceName,
    choose,
    refreshList,
  ]);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      controllerRef.current?.abort();
    };
  }, []);
  useEffect(() => {
    if (!scope) return;
    let disposed = false;
    const abort = new AbortController();
    setLoading(true);
    setReadyStorage(false);
    void (async () => {
      try {
        storageEpoch.current = await store.checkStorage();
        if (disposed) return;
        setReadyStorage(true);
        const list = await store.listConversations();
        if (disposed) return;
        setConversations(list.items);
        setCursor(list.cursor);
        const id = params.get("conversation");
        const found = id ? await store.getConversation(id) : undefined;
        if (!disposed && found) await loadConversation(found);
        if (!disposed) await refreshTarget(abort.signal);
      } catch (reason) {
        if (!disposed) setError(errorText(reason));
      } finally {
        if (!disposed) setLoading(false);
      }
    })();
    return () => {
      disposed = true;
      abort.abort();
    };
    // The URL is updated by explicit selection, not an effect that resends work.
  }, [scope, instance.uid]);
  useEffect(() => {
    if (lastURL.current !== conversationID) {
      lastURL.current = conversationID;
      pendingNavigation.current = conversationID;
    }
    if (pendingNavigation.current === undefined) return;
    if (!scope || loading || switchingRef.current) return;
    const desired = pendingNavigation.current;
    if (desired === (selectedRef.current?.id ?? null)) {
      pendingNavigation.current = undefined;
      return;
    }
    if (busyRef.current) {
      controllerRef.current?.abort();
      return;
    }
    pendingNavigation.current = undefined;
    switchingRef.current = true;
    setSwitching(true);
    void (async () => {
      await flushDraft();
      if (lastURL.current !== desired) return;
      if (!desired) {
        choose(undefined);
        setMessages([]);
        setTurns([]);
        setDraft("");
        setLive(undefined);
        contextHistory.current = { history: [], turns: [], completedCount: 0 };
        setContextCount({ included: 0, omitted: 0 });
      } else {
        const value = await store.getConversation(desired);
        if (value && lastURL.current === desired) await loadConversation(value);
      }
    })()
      .catch((reason) => setError(errorText(reason)))
      .finally(() => {
        switchingRef.current = false;
        setSwitching(false);
      });
  }, [
    conversationID,
    scope,
    instance.uid,
    loading,
    switching,
    busy,
    loadConversation,
    flushDraft,
    choose,
  ]);
  useEffect(() => {
    if (!scope) return;
    return subscribe((notice) => {
      if (notice.scope !== scope) return;
      if (notice.kind === "cleared") {
        storageEpoch.current = undefined;
        setReadyStorage(false);
        controllerRef.current?.abort();
        choose(undefined);
        setMessages([]);
        setTurns([]);
        setDraft("");
        setLive(undefined);
        setConversations([]);
        setParams({}, { replace: true });
        void store
          .checkStorage()
          .then((epoch) => {
            storageEpoch.current = epoch;
            setReadyStorage(true);
          })
          .catch((reason) => setError(errorText(reason)));
      } else if (notice.kind === "changed" && !busyRef.current) {
        void refreshList().catch((reason) => setError(errorText(reason)));
        if (
          selectedRef.current &&
          draftRef.current !== selectedRef.current.draft
        ) {
          setError(
            "This conversation changed in another tab. Your unsent draft is still here; refresh to review the latest history.",
          );
          setSaved(false);
          setReadyStorage(false);
          return;
        }
        if (
          selectedRef.current &&
          notice.conversationId === selectedRef.current.id
        )
          void loadConversation(selectedRef.current).catch((reason) =>
            setError(errorText(reason)),
          );
      }
    });
  }, [scope, refreshList, loadConversation, choose, setParams]);
  useEffect(() => {
    const value = selectedRef.current;
    if (
      !scope ||
      busy ||
      !readyStorage ||
      (!value && !draft) ||
      (draft === value?.draft && model === value?.model)
    )
      return;
    if (byteLength(draft) > LIMITS.inputBytes) {
      setError(
        "A message cannot exceed 64 KiB. Shorten this draft before sending.",
      );
      setSaved(false);
      return;
    }
    const epoch = storageEpoch.current;
    const timer = setTimeout(() => {
      setSaved(false);
      draftWork.current = draftWork.current
        .catch(() => {})
        .then(async () => {
          if (
            !mounted.current ||
            busyRef.current ||
            switchingRef.current ||
            epoch === undefined ||
            epoch !== storageEpoch.current
          )
            return;
          let current = selectedRef.current;
          if (value && current?.id !== value.id) return;
          if (!current) {
            current = await store.createConversation(
              instance.instanceName,
              epoch,
            );
            if (!mounted.current || epoch !== storageEpoch.current) return;
            choose(current);
            setParams({ conversation: current.id }, { replace: true });
          }
          const updated = await store.updateConversation(current, {
            draft,
            model,
          });
          if (mounted.current && selectedRef.current?.id === updated.id) {
            choose(updated);
            setSaved(true);
            await refreshList();
          }
        })
        .catch((reason) => {
          if (mounted.current) {
            setError(errorText(reason));
            setSaved(false);
            setReadyStorage(false);
          }
        });
    }, 500);
    draftTimer.current = timer;
    return () => clearTimeout(timer);
  }, [
    draft,
    model,
    busy,
    readyStorage,
    choose,
    scope,
    instance.uid,
    instance.instanceName,
    setParams,
    refreshList,
  ]);
  useEffect(() => {
    if (follow.current && transcript.current)
      transcript.current.scrollTop = transcript.current.scrollHeight;
  }, [messages, live]);

  const newChat = async () => {
    if (!scope || busyRef.current || switchingRef.current) return;
    switchingRef.current = true;
    setSwitching(true);
    try {
      await flushDraft();
      const value = await store.createConversation(
        instance.instanceName,
        storageEpoch.current,
      );
      await loadConversation(value);
      setParams({ conversation: value.id });
      await refreshList();
    } catch (reason) {
      setError(errorText(reason));
    } finally {
      switchingRef.current = false;
      setSwitching(false);
    }
  };
  const selectConversation = async (value: Conversation) => {
    if (busyRef.current || switchingRef.current) return;
    switchingRef.current = true;
    setSwitching(true);
    try {
      await flushDraft();
      await loadConversation(value);
      setParams({ conversation: value.id });
    } finally {
      switchingRef.current = false;
      setSwitching(false);
    }
  };
  const send = async () => {
    if (
      !scope ||
      !account ||
      busyRef.current ||
      switchingRef.current ||
      !draft.trim() ||
      !model ||
      !models.includes(model) ||
      !readyStorage ||
      support
    )
      return;
    busyRef.current = true;
    clearTimeout(draftTimer.current);
    setBusy(true);
    setError(undefined);
    setLive(undefined);
    follow.current = true;
    const input = draft;
    const sendEpoch = storageEpoch.current;
    let turn: Turn | undefined;
    let text = "";
    let storageFailed = false;
    let receivedFinish: string | undefined;
    let dispatchStarted = false;
    const work = flight(scope);
    controllerRef.current = work.controller;
    const checkpoint = new ResponseCheckpoint(
      (snapshot) => store.saveTurn(turn!, snapshot, "pending"),
      () => {
        storageFailed = true;
        work.controller.abort();
      },
    );
    try {
      await draftWork.current;
      work.controller.signal.throwIfAborted();
      const verified = await account.verify();
      if (verified !== scope)
        throw new ChatError(
          "Your account changed. Reload this conversation.",
          "identity_changed",
          true,
        );
      work.controller.signal.throwIfAborted();
      if (sendEpoch === undefined || sendEpoch !== storageEpoch.current)
        throw new ChatError(
          "Local history was cleared. Start a new request.",
          "history_changed",
          true,
        );
      await withAgentLock(scope, instance.uid, async () => {
        work.controller.signal.throwIfAborted();
        let current = selectedRef.current;
        if (!current) {
          current = await store.createConversation(
            instance.instanceName,
            sendEpoch,
          );
          work.controller.signal.throwIfAborted();
          choose(current);
          setParams({ conversation: current.id });
        }
        await store.recoverTurns(current.id);
        work.controller.signal.throwIfAborted();
        current = await store.getConversation(current.id);
        work.controller.signal.throwIfAborted();
        if (!current) throw new Error("This local conversation was removed.");
        const history = await store.conversationHistory(current.id);
        work.controller.signal.throwIfAborted();
        if (
          history.turns.some(
            (t) =>
              t.resultState !== "completed" &&
              t.dispatchState === "dispatching" &&
              t.seq > (current!.reviewedThrough ?? -1),
          )
        )
          throw new Error(
            "Review the interrupted request before starting a new one.",
          );
        const target = {
          scope,
          name: instance.instanceName,
          uid: instance.uid,
        };
        const resolved = await describeChat(target, work.controller.signal);
        work.controller.signal.throwIfAborted();
        setDescription(resolved);
        if (!resolved.available || !resolved.bindingRevision)
          throw new ChatError(
            reasonText[resolved.reason] ?? "The Agent is not available.",
            "chat_unavailable",
            true,
          );
        const prepared = await store.beginTurn(current, input, model);
        turn = prepared.turn;
        work.controller.signal.throwIfAborted();
        const rows = await store.listMessages(current.id);
        work.controller.signal.throwIfAborted();
        choose(prepared.conversation);
        setDraft("");
        setContextCount({
          included: turn.contextTurnIds.length,
          omitted: turn.omittedTurns,
        });
        setMessages(rows);
        setTurns([...history.turns, turn]);
        setLive({ turnId: turn.id, text: "" });
        setSaved(true);
        turn = await store.saveTurn(turn, "", "pending", {
          dispatchState: "dispatching",
        });
        work.controller.signal.throwIfAborted();
        let lastPaint = 0;
        dispatchStarted = true;
        const finish = await sendChat(
          { ...target, binding: resolved.bindingRevision },
          model,
          prepared.messages,
          turn.requestId,
          work.controller.signal,
          async (delta) => {
            await checkpoint.append(delta);
            text = checkpoint.text;
            const now = performance.now();
            if (now - lastPaint >= 70) {
              if (mounted.current) setLive({ turnId: turn!.id, text });
              lastPaint = now;
            }
          },
        );
        receivedFinish = finish;
        await checkpoint.stop();
        await store.saveTurn(
          turn,
          text,
          finish === "stop" ? "completed" : "incomplete",
          { finishReason: finish },
        );
        if (mounted.current) {
          setLive({ turnId: turn.id, text });
          setSaved(true);
        }
      });
    } catch (reason) {
      await checkpoint.stop().catch(() => {
        storageFailed = true;
      });
      checkIdentityError(reason);
      text = checkpoint.text;
      const stopped = work.controller.signal.aborted;
      if (receivedFinish) storageFailed = true;
      const message = storageFailed
        ? receivedFinish
          ? "The response finished, but could not be saved. Copy it before refreshing."
          : "Local history could not be saved. Copy the visible response before leaving."
        : stopped
          ? "Stopped receiving. The Agent may still be running; no request will be replayed."
          : errorText(reason);
      if (mounted.current) {
        setError(message);
        if (turn) setLive({ turnId: turn.id, text });
      }
      if (turn && !storageFailed) {
        const notSent =
          !dispatchStarted || (reason instanceof ChatError && reason.notSent);
        try {
          await store.saveTurn(
            turn,
            text,
            notSent ||
              (reason instanceof ChatError &&
                ["agent_reported_error", "chat_busy"].includes(reason.code))
              ? "failed"
              : "interrupted",
            {
              dispatchState: notSent ? "not-sent" : "dispatching",
              errorCode:
                reason instanceof ChatError ? reason.code : "interrupted",
            },
          );
        } catch {
          storageFailed = true;
        }
      }
      if (storageFailed && mounted.current) {
        setSaved(false);
        setReadyStorage(false);
      }
    } finally {
      await checkpoint.stop().catch(() => {});
      try {
        if (mounted.current) {
          if (selectedRef.current && !storageFailed) {
            const value = await store.getConversation(selectedRef.current.id);
            if (mounted.current && value) {
              const history = await store.conversationHistory(value.id);
              if (mounted.current) {
                const rows = await store.listMessages(value.id);
                if (mounted.current && selectedRef.current?.id === value.id) {
                  choose(value);
                  setTurns(history.turns);
                  contextHistory.current = history;
                  setMessages(rows);
                }
              }
            }
          }
          await refreshList();
        }
      } catch (reason) {
        if (mounted.current) {
          setSaved(false);
          setReadyStorage(false);
          setError(errorText(reason));
        }
      } finally {
        work.release();
        controllerRef.current = undefined;
        busyRef.current = false;
        if (mounted.current) setBusy(false);
        if (turn)
          notify({
            kind: "changed",
            scope,
            agentUid: instance.uid,
            conversationId: turn.conversationId,
          });
      }
    }
  };

  const mutateTitle = async () => {
    if (!selected || busy) return;
    try {
      const updated = await store.updateConversation(selected, { title });
      choose(updated);
      setRename(false);
      await refreshList();
      notify({
        kind: "changed",
        scope: selected.scope,
        agentUid: instance.uid,
        conversationId: selected.id,
      });
    } catch (reason) {
      setError(errorText(reason));
    }
  };
  const remove = async () => {
    if (!selected || !scope || busy) return;
    try {
      await withAgentLock(scope, instance.uid, () =>
        store.deleteConversation(selected),
      );
      choose(undefined);
      setMessages([]);
      setTurns([]);
      setDraft("");
      setDeleteOpen(false);
      setParams({});
      await refreshList();
      notify({
        kind: "changed",
        scope,
        agentUid: instance.uid,
        conversationId: selected.id,
      });
    } catch (reason) {
      setError(errorText(reason));
    }
  };
  const refreshChat = async () => {
    if (!scope || busyRef.current) return;
    try {
      storageEpoch.current = await store.checkStorage();
      const value = selectedRef.current;
      if (value) {
        await withAgentLock(scope, instance.uid, () =>
          store.recoverTurns(value.id),
        );
        const current = await store.getConversation(value.id);
        if (current) {
          choose(current);
          const history = await store.conversationHistory(current.id);
          setTurns(history.turns);
          contextHistory.current = history;
          setMessages(await store.listMessages(current.id));
          setLive(undefined);
        }
      }
      setReadyStorage(true);
      setError(undefined);
      await refreshTarget();
    } catch (reason) {
      setError(errorText(reason));
    }
  };
  const unknown = turns.some(
    (t) =>
      t.resultState !== "completed" &&
      t.resultState !== "pending" &&
      t.dispatchState === "dispatching" &&
      t.seq > (selected?.reviewedThrough ?? -1),
  );
  const disabled =
    busy ||
    switching ||
    loading ||
    !description?.available ||
    !model ||
    !models.includes(model) ||
    !readyStorage ||
    Boolean(support) ||
    Boolean(account?.checking) ||
    byteLength(draft) > LIMITS.inputBytes ||
    unknown;

  let preview = contextCount;
  if (draft.trim() && model && selected) {
    try {
      const value = buildContext(contextHistory.current.history, draft, model);
      preview = {
        included: value.turnIds.length,
        omitted:
          value.omitted +
          contextHistory.current.completedCount -
          contextHistory.current.history.length,
      };
    } catch {
      /* Input validation is shown next to the composer. */
    }
  }

  if (account?.checking)
    return (
      <div className="chat-identity">
        <Spinner size="small" label="Checking your sign-in…" />
      </div>
    );
  if (!scope)
    return (
      <div className="chat-identity">
        <Chat20Regular />
        <h2>Chat is not available</h2>
        <p>{account?.error ?? "Sign in to view your local history."}</p>
        <Button onClick={() => window.location.reload()}>
          Refresh sign-in
        </Button>
        <Button
          appearance="subtle"
          onClick={() =>
            navigate(`/instances/${instance.instanceName}/overview`)
          }
        >
          Back to overview
        </Button>
      </div>
    );

  return (
    <section
      className="custom-chat"
      aria-label={`${name} chat`}
      onKeyDown={(event) => {
        if (event.key === "Escape") setRailOpen(false);
      }}
    >
      <aside
        className={`chat-rail ${railOpen ? "chat-rail--open" : ""}`}
        aria-label="Chat conversations"
      >
        <Button
          className="chat-rail-close"
          appearance="subtle"
          icon={<Dismiss20Regular />}
          aria-label="Close conversation list"
          onClick={() => setRailOpen(false)}
        />
        <Button
          appearance="subtle"
          icon={<ArrowLeft20Regular />}
          onClick={() =>
            navigate(`/instances/${instance.instanceName}/overview`)
          }
        >
          Agent overview
        </Button>
        <div className="chat-agent-name">
          {name}
          <span>Custom Agent</span>
        </div>
        <Button
          className="chat-new"
          appearance="outline"
          icon={<Add20Regular />}
          onClick={() => void newChat()}
          disabled={busy || !readyStorage}
        >
          New chat
        </Button>
        <div className="chat-recents-label">Recent conversations</div>
        <div className="chat-conversations">
          {conversations.map((c) => (
            <button
              key={c.id}
              className={`chat-conversation ${selected?.id === c.id ? "is-selected" : ""}`}
              onClick={() =>
                void selectConversation(c).catch((reason) =>
                  setError(errorText(reason)),
                )
              }
              disabled={busy}
              title={c.title}
            >
              <Chat20Regular />
              <span>{c.title}</span>
            </button>
          ))}
          {!conversations.length && !loading && (
            <p className="chat-no-recents">
              Your conversations will appear here.
            </p>
          )}
          {cursor && (
            <Button
              appearance="subtle"
              size="small"
              onClick={() =>
                void store.listConversations(cursor).then((value) => {
                  setConversations((old) => [...old, ...value.items]);
                  setCursor(value.cursor);
                })
              }
            >
              Load more
            </Button>
          )}
        </div>
        <div className="chat-local-note">
          <span className="chat-local-dot" />
          Saved on this browser only
          <span>No cloud sync. Clearing site data removes your history.</span>
        </div>
      </aside>
      {railOpen && (
        <button
          className="chat-rail-backdrop"
          aria-label="Dismiss conversation backdrop"
          tabIndex={-1}
          onClick={() => setRailOpen(false)}
        />
      )}
      <div
        className={`chat-main ${!loading && !messages.length ? "chat-main--empty" : ""}`}
      >
        <header className="chat-toolbar">
          <Button
            className="chat-rail-toggle"
            appearance="subtle"
            icon={<PanelLeft20Regular />}
            aria-label="Toggle conversations"
            aria-expanded={railOpen}
            onClick={() => setRailOpen(!railOpen)}
          />
          <div className="chat-toolbar-title">
            {selected?.title ?? "New chat"}
            <span>
              {description?.available
                ? "Available"
                : description?.reason
                  ? "Not available"
                  : "Checking connection"}
            </span>
          </div>
          <div className="chat-toolbar-actions">
            <Button
              appearance="subtle"
              icon={<ArrowSync20Regular />}
              aria-label="Refresh chat"
              disabled={busy || switching || loading}
              onClick={() => void refreshChat()}
            />
            {selected && (
              <>
                <Button
                  appearance="subtle"
                  icon={<Edit20Regular />}
                  aria-label="Rename conversation"
                  disabled={busy}
                  onClick={() => {
                    setTitle(selected.title);
                    setRename(true);
                  }}
                />
                <Button
                  appearance="subtle"
                  icon={<Delete20Regular />}
                  aria-label="Delete conversation"
                  disabled={busy}
                  onClick={() => setDeleteOpen(true)}
                />
              </>
            )}
          </div>
        </header>
        <div
          ref={transcript}
          className="chat-transcript"
          onScroll={() => {
            const el = transcript.current;
            if (el)
              follow.current =
                el.scrollHeight - el.scrollTop - el.clientHeight < 100;
          }}
        >
          <div className="chat-column">
            {loading ? (
              <div className="chat-loading">
                <Spinner size="small" label="Opening chat…" />
              </div>
            ) : !messages.length ? (
              <div className="chat-empty">
                <div className="chat-empty-mark">
                  <Chat20Regular />
                </div>
                <h1>What would you like to work on?</h1>
                <p>
                  Start a conversation with {name}.<br />
                  Your history stays in this browser.
                </p>
              </div>
            ) : (
              <>
                {messages[0]?.seq > 0 && (
                  <Button
                    appearance="subtle"
                    onClick={() => {
                      if (selected)
                        void store
                          .listMessages(selected.id, messages[0].seq)
                          .then((rows) =>
                            setMessages((old) => [...rows, ...old]),
                          );
                    }}
                  >
                    Load earlier messages
                  </Button>
                )}
                {messages.map((message) => (
                  <MessageView
                    key={message.seq}
                    message={message}
                    text={
                      message.role === "assistant" &&
                      live?.turnId === message.turnId
                        ? live.text
                        : message.text
                    }
                    name={name}
                    state={
                      message.role === "assistant"
                        ? live?.turnId === message.turnId && !saved
                          ? "unsaved"
                          : turns.find((t) => t.id === message.turnId)
                              ?.resultState
                        : undefined
                    }
                  />
                ))}
              </>
            )}
          </div>
        </div>
        <div className="chat-compose-wrap">
          <div className="chat-column">
            {(error ||
              support ||
              (!description?.available && description) ||
              (!loading && description?.available && !models.length)) && (
              <div className="chat-notice" role="status">
                {error ??
                  support ??
                  (description?.available && !models.length
                    ? "The Agent has not provided an available model list."
                    : reasonText[description?.reason ?? ""]) ??
                  "Chat is unavailable."}
                <Button
                  appearance="subtle"
                  size="small"
                  disabled={busy}
                  onClick={() => void refreshChat()}
                >
                  Refresh
                </Button>
              </div>
            )}
            {unknown && !busy && (
              <div className="chat-risk">
                A previous request may have changed this Agent's state.
                <Button
                  appearance="subtle"
                  size="small"
                  onClick={() => {
                    if (!selected) return;
                    void draftWork.current
                      .then(async () => {
                        const current = await store.getConversation(
                          selected.id,
                        );
                        if (!current) return;
                        const updated = await store.updateConversation(
                          current,
                          {
                            reviewedThrough: Math.max(
                              ...turns.map((t) => t.seq),
                            ),
                          },
                        );
                        choose(updated);
                      })
                      .catch((reason) => setError(errorText(reason)));
                  }}
                >
                  I've reviewed it · start a new request
                </Button>
              </div>
            )}
            <form
              className={`chat-composer ${busy ? "chat-composer--busy" : ""}`}
              onSubmit={(event) => {
                event.preventDefault();
                void send();
              }}
            >
              <Textarea
                appearance="outline"
                resize="none"
                className="chat-input"
                aria-label={`Message ${name}`}
                placeholder={`Message ${name}…`}
                value={draft}
                disabled={busy || switching || loading || Boolean(support)}
                readOnly={!readyStorage}
                onChange={(_, data) => setDraft(data.value)}
                onKeyDown={(event) => {
                  if (
                    event.key === "Enter" &&
                    (event.ctrlKey || event.metaKey) &&
                    !event.nativeEvent.isComposing
                  ) {
                    event.preventDefault();
                    if (!disabled) void send();
                  }
                }}
              />
              <div className="chat-compose-tools">
                <Select
                  aria-label="Chat model"
                  value={model}
                  disabled={busy || switching || !models.length}
                  onChange={(_, data) => setModel(data.value)}
                >
                  {models.length !== 1 && (
                    <option value="">Choose a model</option>
                  )}
                  {models.map((id) => (
                    <option key={id} value={id}>
                      {id}
                    </option>
                  ))}
                </Select>
                <span className="chat-shortcut">Ctrl / ⌘ Enter</span>
                {busy ? (
                  <Button
                    className="chat-send"
                    appearance="primary"
                    icon={<Stop20Filled />}
                    aria-label="Stop response"
                    onClick={() => controllerRef.current?.abort()}
                  />
                ) : (
                  <Button
                    className="chat-send"
                    type="submit"
                    appearance="primary"
                    icon={<ArrowUp20Filled />}
                    aria-label="Send message"
                    disabled={disabled || !draft.trim()}
                  />
                )}
              </div>
            </form>
            <div className="chat-compose-footer">
              <span>
                {saved
                  ? "History is local to this browser"
                  : "Not saved · copy your text before leaving"}
              </span>
              <span>
                {preview.included} prior turns
                {preview.omitted
                  ? ` · ${preview.omitted} older turns omitted`
                  : ""}
              </span>
            </div>
          </div>
        </div>
      </div>
      <Dialog open={rename} onOpenChange={(_, data) => setRename(data.open)}>
        <DialogSurface>
          <DialogBody>
            <DialogTitle>Rename conversation</DialogTitle>
            <DialogContent>
              <Input
                aria-label="Conversation title"
                value={title}
                maxLength={240}
                onChange={(_, data) => setTitle(data.value)}
              />
            </DialogContent>
            <DialogActions>
              <Button onClick={() => setRename(false)}>Cancel</Button>
              <Button appearance="primary" onClick={() => void mutateTitle()}>
                Save
              </Button>
            </DialogActions>
          </DialogBody>
        </DialogSurface>
      </Dialog>
      <Dialog
        open={deleteOpen}
        onOpenChange={(_, data) => setDeleteOpen(data.open)}
      >
        <DialogSurface>
          <DialogBody>
            <DialogTitle>Delete local conversation?</DialogTitle>
            <DialogContent>
              This removes this conversation from this browser. It does not
              delete files, native sessions or memory stored by the Agent.
            </DialogContent>
            <DialogActions>
              <Button onClick={() => setDeleteOpen(false)}>Cancel</Button>
              <Button appearance="primary" onClick={() => void remove()}>
                Delete conversation
              </Button>
            </DialogActions>
          </DialogBody>
        </DialogSurface>
      </Dialog>
    </section>
  );
}
