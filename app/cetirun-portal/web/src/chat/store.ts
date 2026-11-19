// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import {
  byteLength,
  buildContext,
  conversationKey,
  LIMITS,
  type ChatMessage,
  type Conversation,
  type ConversationKey,
  type ResultState,
  type Turn,
} from "./types";

const DB_NAME = "cetirun-portal-chat";
type StoreName = "accounts" | "conversations" | "turns" | "messages";
interface Account {
  scope: string;
  epoch: number;
  bytes: number;
  reserved: number;
}
let connection: Promise<IDBDatabase> | undefined;
const changedError = () =>
  new Error("Local history changed. Refresh before continuing.");
const range = (prefix: IDBValidKey[]) =>
  IDBKeyRange.bound(prefix, [...prefix, []]);

function request<T>(value: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    value.onsuccess = () => resolve(value.result);
    value.onerror = () =>
      reject(value.error ?? new Error("Browser storage failed."));
  });
}

function database(): Promise<IDBDatabase> {
  if (connection) return connection;
  connection = new Promise((resolve, reject) => {
    if (!globalThis.indexedDB) {
      reject(new Error("This browser cannot save chat history."));
      return;
    }
    const open = indexedDB.open(DB_NAME, 1);
    let blocked = false;
    open.onupgradeneeded = () => {
      if (blocked) {
        open.transaction?.abort();
        return;
      }
      const db = open.result;
      db.createObjectStore("accounts", { keyPath: "scope" });
      const conversations = db.createObjectStore("conversations", {
        keyPath: ["scope", "agentUid", "id"],
      });
      conversations.createIndex("byScope", "scope");
      conversations.createIndex("byAccount", ["scope", "updatedAt", "id"]);
      conversations.createIndex("byAgent", [
        "scope",
        "agentUid",
        "updatedAt",
        "id",
      ]);
      const turns = db.createObjectStore("turns", {
        keyPath: ["scope", "agentUid", "conversationId", "id"],
      });
      turns.createIndex("byConversation", [
        "scope",
        "agentUid",
        "conversationId",
        "seq",
      ]);
      db.createObjectStore("messages", {
        keyPath: ["scope", "agentUid", "conversationId", "seq"],
      });
    };
    open.onblocked = () => {
      blocked = true;
      connection = undefined;
      reject(
        new Error("Close older Portal tabs before updating chat storage."),
      );
    };
    open.onerror = () => {
      connection = undefined;
      reject(open.error ?? new Error("Chat storage could not be opened."));
    };
    open.onsuccess = () => {
      if (blocked) {
        open.result.close();
        return;
      }
      open.result.onversionchange = () => {
        open.result.close();
        connection = undefined;
        window.dispatchEvent(new Event("cetirun-chat-storage-closed"));
      };
      open.result.onclose = () => {
        connection = undefined;
        window.dispatchEvent(new Event("cetirun-chat-storage-closed"));
      };
      resolve(open.result);
    };
  });
  return connection;
}

async function transaction<T>(
  stores: StoreName[],
  mode: IDBTransactionMode,
  work: (tx: IDBTransaction) => Promise<T>,
): Promise<T> {
  const db = await database();
  const tx = db.transaction(stores, mode);
  const done = new Promise<void>((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onabort = () =>
      reject(
        tx.error ?? new Error("Chat storage transaction was interrupted."),
      );
    tx.onerror = () =>
      reject(tx.error ?? new Error("Chat history could not be saved."));
  });
  // The completion listener is installed before issuing any requests.
  void done.catch(() => {});
  try {
    const result = await work(tx);
    await done;
    return result;
  } catch (error) {
    try {
      tx.abort();
    } catch {
      /* Already completed or aborted. */
    }
    await done.catch(() => {});
    throw error;
  }
}

async function account(
  tx: IDBTransaction,
  scope: string,
  create = false,
): Promise<Account> {
  const stored = await request<Account | undefined>(
    tx.objectStore("accounts").get(scope),
  );
  if (stored) return stored;
  if (!create) throw changedError();
  const value = { scope, epoch: 1, bytes: 0, reserved: 0 };
  await request(tx.objectStore("accounts").put(value));
  return value;
}

function capacity(value: Account) {
  if (value.bytes + value.reserved > LIMITS.accountBytes)
    throw new Error(
      "Local history is full. Delete older conversations before sending.",
    );
}

export async function checkStorage(scope: string) {
  if (!scope) throw new Error("Your chat identity is unavailable.");
  return transaction(["accounts"], "readwrite", async (tx) => {
    const value = await account(tx, scope, true);
    await request(tx.objectStore("accounts").put(value));
    return value.epoch;
  });
}

export async function createConversation(
  scope: string,
  agentUid: string,
  instanceName: string,
  expectedEpoch?: number,
): Promise<Conversation> {
  return transaction(["accounts", "conversations"], "readwrite", async (tx) => {
    const a = await account(tx, scope, true);
    if (expectedEpoch !== undefined && a.epoch !== expectedEpoch)
      throw changedError();
    if (
      (await request(
        tx.objectStore("conversations").index("byScope").count(scope),
      )) >= LIMITS.conversations
    )
      throw new Error(
        "Delete a conversation before creating another (limit 100).",
      );
    const now = Date.now();
    const value: Conversation = {
      scope,
      agentUid,
      instanceName,
      id: crypto.randomUUID(),
      title: "New conversation",
      model: "",
      draft: "",
      revision: 1,
      createdAt: now,
      updatedAt: now,
      nextSeq: 0,
    };
    await request(tx.objectStore("conversations").add(value));
    return value;
  });
}

export async function listConversations(
  scope: string,
  agentUid?: string,
  before?: IDBValidKey,
): Promise<{ items: Conversation[]; cursor?: IDBValidKey }> {
  const prefix = agentUid ? [scope, agentUid] : [scope];
  if (
    !scope ||
    (before !== undefined &&
      (!Array.isArray(before) ||
        before.length !== prefix.length + 2 ||
        prefix.some((part, i) => before[i] !== part)))
  )
    throw new Error(
      "The history cursor belongs to a different account or Agent.",
    );
  return transaction(
    ["conversations"],
    "readonly",
    (tx) =>
      new Promise((resolve, reject) => {
        const index = tx
          .objectStore("conversations")
          .index(agentUid ? "byAgent" : "byAccount");
        const bounds = before
          ? IDBKeyRange.bound(prefix, before, false, true)
          : range(prefix);
        const cursor = index.openCursor(bounds, "prev");
        const items: Conversation[] = [];
        cursor.onerror = () => reject(cursor.error);
        cursor.onsuccess = () => {
          const row = cursor.result;
          if (!row) {
            resolve({ items });
            return;
          }
          items.push(row.value);
          if (items.length === 30) {
            resolve({ items, cursor: row.key });
            return;
          }
          row.continue();
        };
      }),
  );
}

export function getConversation(
  key: ConversationKey,
): Promise<Conversation | undefined> {
  return transaction(["conversations"], "readonly", (tx) =>
    request(tx.objectStore("conversations").get(key)),
  );
}

// This binds cooperative application calls; it is not a same-origin script sandbox.
export function forAgent(scope: string | undefined, agentUid: string) {
  const key = (id: string): ConversationKey => {
    if (!scope || !agentUid || !id)
      throw new Error(
        "Choose an authenticated account, Agent and conversation.",
      );
    return [scope, agentUid, id];
  };
  const check = (value: Conversation | Turn) => {
    key(value.id);
    if (value.scope !== scope || value.agentUid !== agentUid)
      throw new Error("This record belongs to a different account or Agent.");
  };
  return Object.freeze({
    checkStorage: async () => {
      key("storage");
      return checkStorage(scope!);
    },
    createConversation: async (
      instanceName: string,
      expectedEpoch?: number,
    ) => {
      key("new");
      return createConversation(scope!, agentUid, instanceName, expectedEpoch);
    },
    listConversations: async (before?: IDBValidKey) => {
      key("list");
      return listConversations(scope!, agentUid, before);
    },
    getConversation: async (id: string) => getConversation(key(id)),
    listMessages: async (id: string, before?: number) =>
      listMessages(key(id), before),
    conversationHistory: async (id: string) => conversationHistory(key(id)),
    recoverTurns: async (id: string) => recoverTurns(key(id)),
    updateConversation: async (
      value: Conversation,
      change: Parameters<typeof updateConversation>[1],
    ) => {
      check(value);
      return updateConversation(value, change);
    },
    beginTurn: async (value: Conversation, input: string, model: string) => {
      check(value);
      return beginTurn(value, input, model);
    },
    saveTurn: async (
      value: Turn,
      text: string,
      state: ResultState,
      options?: Parameters<typeof saveTurn>[3],
    ) => {
      check(value);
      return saveTurn(value, text, state, options);
    },
    deleteConversation: async (value: Conversation) => {
      check(value);
      return deleteConversation(value);
    },
  });
}

export async function updateConversation(
  value: Conversation,
  change: {
    title?: string;
    model?: string;
    draft?: string;
    reviewedThrough?: number;
  },
): Promise<Conversation> {
  return transaction(
    ["accounts", "conversations", "turns"],
    "readwrite",
    async (tx) => {
      const stored = await request<Conversation | undefined>(
        tx.objectStore("conversations").get(conversationKey(value)),
      );
      if (!stored || stored.revision !== value.revision) throw changedError();
      const active = await request<Turn[]>(
        tx
          .objectStore("turns")
          .index("byConversation")
          .getAll(range(conversationKey(value))),
      );
      if (active.some((turn) => turn.resultState === "pending"))
        throw new Error(
          "This conversation is active in another tab. Keep your draft and refresh after it finishes.",
        );
      const a = await account(tx, value.scope);
      if (change.draft !== undefined) {
        if (byteLength(change.draft) > LIMITS.inputBytes)
          throw new Error("A draft cannot exceed 64 KiB.");
        a.bytes += byteLength(change.draft) - byteLength(stored.draft);
      }
      const updated = {
        ...stored,
        ...change,
        title:
          [...(change.title ?? stored.title).trim()].slice(0, 120).join("") ||
          "Untitled conversation",
        revision: stored.revision + 1,
        updatedAt: Date.now(),
      };
      capacity(a);
      await request(tx.objectStore("accounts").put(a));
      await request(tx.objectStore("conversations").put(updated));
      return updated;
    },
  );
}

export async function conversationHistory(key: ConversationKey) {
  return transaction(["turns", "messages"], "readonly", async (tx) => {
    const turns = await request<Turn[]>(
      tx.objectStore("turns").index("byConversation").getAll(range(key)),
    );
    const history: { turn: Turn; user: string; assistant: string }[] = [];
    for (const turn of turns
      .filter((t) => t.resultState === "completed")
      .slice(-49)) {
      const user = await request<ChatMessage>(
        tx.objectStore("messages").get([...key, turn.seq]),
      );
      const assistant = await request<ChatMessage>(
        tx.objectStore("messages").get([...key, turn.seq + 1]),
      );
      if (!user || !assistant)
        throw new Error(
          "Local history is incomplete. Start a new conversation.",
        );
      history.push({ turn, user: user.text, assistant: assistant.text });
    }
    return {
      history,
      turns,
      completedCount: turns.filter((t) => t.resultState === "completed").length,
    };
  });
}

export async function listMessages(
  key: ConversationKey,
  before = Number.MAX_SAFE_INTEGER,
): Promise<ChatMessage[]> {
  return transaction(
    ["messages"],
    "readonly",
    (tx) =>
      new Promise((resolve, reject) => {
        const rows: ChatMessage[] = [];
        const cursor = tx
          .objectStore("messages")
          .openCursor(
            IDBKeyRange.bound(key, [...key, before], false, true),
            "prev",
          );
        cursor.onerror = () => reject(cursor.error);
        cursor.onsuccess = () => {
          const row = cursor.result;
          if (!row || rows.length === 40) {
            resolve(rows.reverse());
            return;
          }
          rows.push(row.value);
          row.continue();
        };
      }),
  );
}

export async function beginTurn(
  conversation: Conversation,
  input: string,
  model: string,
) {
  const key = conversationKey(conversation);
  const history = await conversationHistory(key);
  const context = buildContext(history.history, input, model);
  context.omitted += history.completedCount - history.history.length;
  return transaction(
    ["accounts", "conversations", "turns", "messages"],
    "readwrite",
    async (tx) => {
      const c = await request<Conversation>(
        tx.objectStore("conversations").get(key),
      );
      if (!c || c.revision !== conversation.revision) throw changedError();
      if (c.nextSeq + 2 > LIMITS.messages)
        throw new Error("This conversation is full. Start a new conversation.");
      const a = await account(tx, c.scope);
      a.bytes += byteLength(input) - byteLength(c.draft);
      a.reserved += LIMITS.outputBytes;
      capacity(a);
      const now = Date.now();
      const id = crypto.randomUUID();
      const turn: Turn = {
        scope: c.scope,
        agentUid: c.agentUid,
        conversationId: c.id,
        id,
        requestId: crypto.randomUUID(),
        seq: c.nextSeq,
        model,
        dispatchState: "prepared",
        resultState: "pending",
        createdAt: now,
        contextTurnIds: context.turnIds,
        omittedTurns: context.omitted,
        reservedBytes: LIMITS.outputBytes,
        epoch: a.epoch,
      };
      const message = (
        role: "user" | "assistant",
        text: string,
        seq: number,
      ): ChatMessage => ({
        scope: c.scope,
        agentUid: c.agentUid,
        conversationId: c.id,
        turnId: id,
        seq,
        role,
        text,
        bytes: byteLength(text),
        updatedAt: now,
      });
      await request(
        tx.objectStore("messages").add(message("user", input, c.nextSeq)),
      );
      await request(
        tx.objectStore("messages").add(message("assistant", "", c.nextSeq + 1)),
      );
      await request(tx.objectStore("turns").add(turn));
      const updated: Conversation = {
        ...c,
        title:
          c.nextSeq === 0 && c.title === "New conversation"
            ? [...input.trim()].slice(0, 60).join("")
            : c.title,
        draft: "",
        model,
        revision: c.revision + 1,
        updatedAt: now,
        nextSeq: c.nextSeq + 2,
      };
      await request(tx.objectStore("conversations").put(updated));
      await request(tx.objectStore("accounts").put(a));
      return { turn, conversation: updated, messages: context.messages };
    },
  );
}

export async function saveTurn(
  turn: Turn,
  text: string,
  resultState: ResultState,
  options: {
    dispatchState?: Turn["dispatchState"];
    finishReason?: string;
    errorCode?: string;
  } = {},
) {
  const key: ConversationKey = [turn.scope, turn.agentUid, turn.conversationId];
  return transaction(
    ["accounts", "conversations", "turns", "messages"],
    "readwrite",
    async (tx) => {
      const a = await account(tx, turn.scope);
      const stored = await request<Turn | undefined>(
        tx.objectStore("turns").get([...key, turn.id]),
      );
      const c = await request<Conversation | undefined>(
        tx.objectStore("conversations").get(key),
      );
      if (
        a.epoch !== turn.epoch ||
        !stored ||
        !c ||
        stored.resultState !== "pending"
      )
        throw changedError();
      const old = await request<ChatMessage | undefined>(
        tx.objectStore("messages").get([...key, turn.seq + 1]),
      );
      if (!old) throw changedError();
      const bytes = byteLength(text);
      if (bytes > LIMITS.outputBytes || bytes < old.bytes)
        throw new Error("The response exceeded the supported output size.");
      a.bytes += bytes - old.bytes;
      a.reserved -= bytes - old.bytes;
      stored.reservedBytes -= bytes - old.bytes;
      if (resultState !== "pending") {
        a.reserved -= stored.reservedBytes;
        stored.reservedBytes = 0;
        stored.endedAt = Date.now();
      }
      Object.assign(stored, options, { resultState });
      capacity(a);
      await request(
        tx
          .objectStore("messages")
          .put({ ...old, text, bytes, updatedAt: Date.now() }),
      );
      await request(tx.objectStore("turns").put(stored));
      await request(
        tx
          .objectStore("conversations")
          .put({ ...c, revision: c.revision + 1, updatedAt: Date.now() }),
      );
      await request(tx.objectStore("accounts").put(a));
      return stored;
    },
  );
}

// Caller holds the Agent Web Lock; a timestamp alone never authorizes recovery.
export async function recoverTurns(key: ConversationKey) {
  const { turns } = await conversationHistory(key);
  for (const turn of turns.filter((t) => t.resultState === "pending")) {
    const messages = await transaction(["messages"], "readonly", (tx) =>
      request<ChatMessage>(
        tx.objectStore("messages").get([...key, turn.seq + 1]),
      ),
    );
    await saveTurn(
      turn,
      messages?.text ?? "",
      turn.dispatchState === "prepared" ? "failed" : "interrupted",
      {
        dispatchState:
          turn.dispatchState === "prepared" ? "not-sent" : "dispatching",
        errorCode: "page_interrupted",
      },
    );
  }
}

export async function deleteConversation(value: Conversation) {
  const key = conversationKey(value);
  return transaction(
    ["accounts", "conversations", "turns", "messages"],
    "readwrite",
    async (tx) => {
      const c = await request<Conversation | undefined>(
        tx.objectStore("conversations").get(key),
      );
      if (!c || c.revision !== value.revision) throw changedError();
      const turns = await request<Turn[]>(
        tx.objectStore("turns").index("byConversation").getAll(range(key)),
      );
      if (turns.some((t) => t.resultState === "pending"))
        throw new Error(
          "Stop the active request before deleting this conversation.",
        );
      const rows = await request<ChatMessage[]>(
        tx.objectStore("messages").getAll(range(key)),
      );
      const a = await account(tx, value.scope);
      a.bytes -= rows.reduce((n, m) => n + m.bytes, byteLength(c.draft));
      for (const turn of turns)
        await request(tx.objectStore("turns").delete([...key, turn.id]));
      await request(tx.objectStore("messages").delete(range(key)));
      await request(tx.objectStore("conversations").delete(key));
      await request(tx.objectStore("accounts").put(a));
    },
  );
}

export async function clearAccount(scope: string) {
  return transaction(
    ["accounts", "conversations", "turns", "messages"],
    "readwrite",
    async (tx) => {
      const a = await account(tx, scope, true);
      a.epoch++;
      a.bytes = 0;
      a.reserved = 0;
      for (const name of ["conversations", "turns", "messages"] as const)
        await request(tx.objectStore(name).delete(range([scope])));
      await request(tx.objectStore("accounts").put(a));
    },
  );
}
