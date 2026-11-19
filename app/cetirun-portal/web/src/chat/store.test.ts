// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import "fake-indexeddb/auto";
import { afterEach, describe, it, expect, vi } from "vitest";
import * as store from "./store";
import { conversationKey, LIMITS } from "./types";

afterEach(() => vi.restoreAllMocks());
const fixture = async () => {
  const scope = `scope-${crypto.randomUUID()}`;
  await store.checkStorage(scope);
  const conversation = await store.createConversation(
    scope,
    "agent-uid",
    "example",
  );
  return { scope, conversation, key: conversationKey(conversation) };
};

describe("browser chat history", () => {
  it("binds reads and mutations to the selected account and Agent", async () => {
    const scope = `scope-${crypto.randomUUID()}`;
    const a = store.forAgent(scope, "uid-a"),
      b = store.forAgent(scope, "uid-b");
    const c = await a.createConversation("example");
    await b.createConversation("example");
    const active = await a.beginTurn(c, "Only for A", "agent");
    await expect(b.getConversation(c.id)).resolves.toBeUndefined();
    await expect(b.listMessages(c.id)).resolves.toEqual([]);
    await expect(
      b.updateConversation(active.conversation, { title: "B" }),
    ).rejects.toThrow(/different/);
    await expect(
      b.beginTurn(active.conversation, "B", "agent"),
    ).rejects.toThrow(/different/);
    await expect(b.saveTurn(active.turn, "B", "completed")).rejects.toThrow(
      /different/,
    );
    await expect(b.deleteConversation(active.conversation)).rejects.toThrow(
      /different/,
    );
    await expect(
      store
        .forAgent("another-account", "uid-a")
        .deleteConversation(active.conversation),
    ).rejects.toThrow(/different/);
    await a.saveTurn(active.turn, "A reply", "completed");
    expect((await a.listMessages(c.id)).map((m) => m.text)).toEqual([
      "Only for A",
      "A reply",
    ]);
    expect((await b.listConversations()).items).toHaveLength(1);
  });
  it("does not expand an Agent list with a foreign pagination cursor", async () => {
    const { scope } = await fixture();
    const a = store.forAgent(scope, "uid-a");
    await expect(
      a.listConversations([scope, "uid-b", Date.now(), "id"]),
    ).rejects.toThrow(/cursor/);
    await expect(
      store.listConversations(scope, undefined, [
        "other-account",
        Date.now(),
        "id",
      ]),
    ).rejects.toThrow(/cursor/);
    await expect(
      store.forAgent(undefined, "uid-a").listConversations(),
    ).rejects.toThrow(/authenticated/);
  });
  it("persists messages and builds the next request with no native history API", async () => {
    const { conversation, key } = await fixture();
    const first = await store.beginTurn(
      conversation,
      "First question",
      "example-agent",
    );
    await store.saveTurn(first.turn, "", "pending", {
      dispatchState: "dispatching",
    });
    await store.saveTurn(first.turn, "First answer", "completed", {
      finishReason: "stop",
    });
    const current = await store.getConversation(key);
    const second = await store.beginTurn(
      current!,
      "Second question",
      "example-agent",
    );
    expect(second.messages).toEqual([
      { role: "user", content: "First question" },
      { role: "assistant", content: "First answer" },
      { role: "user", content: "Second question" },
    ]);
    expect((await store.listMessages(key)).map((m) => m.text)).toEqual([
      "First question",
      "First answer",
      "Second question",
      "",
    ]);
  });
  it("isolates accounts and actual Agent UIDs even with the same instance name", async () => {
    const { scope, conversation } = await fixture();
    await store.createConversation(scope, "replacement-uid", "example");
    const other = `scope-${crypto.randomUUID()}`;
    await store.createConversation(other, conversation.agentUid, "example");
    expect(
      (await store.listConversations(scope, conversation.agentUid)).items.map(
        (c) => c.id,
      ),
    ).toEqual([conversation.id]);
    expect(
      (await store.listConversations(scope, "replacement-uid")).items,
    ).toHaveLength(1);
    expect(
      await store.getConversation([
        other,
        conversation.agentUid,
        conversation.id,
      ]),
    ).toBeUndefined();
  });
  it("rejects stale edits rather than losing another tab's draft", async () => {
    const { conversation, key } = await fixture();
    await store.updateConversation(conversation, { draft: "other tab" });
    await expect(
      store.updateConversation(conversation, { draft: "stale" }),
    ).rejects.toThrow(/changed/);
    expect((await store.getConversation(key))?.draft).toBe("other tab");
  });
  it("retains partial replies and does not promote them to context or success", async () => {
    const { conversation, key } = await fixture();
    const prepared = await store.beginTurn(
      conversation,
      "Unknown work",
      "agent",
    );
    await store.saveTurn(prepared.turn, "partial", "pending", {
      dispatchState: "dispatching",
    });
    await store.recoverTurns(key);
    const { turns, history } = await store.conversationHistory(key);
    expect(turns[0].resultState).toBe("interrupted");
    expect(history).toEqual([]);
    expect((await store.listMessages(key))[1].text).toBe("partial");
  });
  it("distinguishes prepared-but-unsent after a crash", async () => {
    const { conversation, key } = await fixture();
    await store.beginTurn(conversation, "Question", "agent");
    await store.recoverTurns(key);
    const { turns } = await store.conversationHistory(key);
    expect(turns[0].dispatchState).toBe("not-sent");
    expect(turns[0].resultState).toBe("failed");
    expect(turns[0].reservedBytes).toBe(0);
  });
  it("clearing an account prevents late responses from resurrecting data", async () => {
    const { scope, conversation, key } = await fixture();
    const prepared = await store.beginTurn(conversation, "Question", "agent");
    await store.clearAccount(scope);
    await expect(
      store.saveTurn(prepared.turn, "late secret", "completed"),
    ).rejects.toThrow(/changed/);
    expect(await store.getConversation(key)).toBeUndefined();
    expect((await store.listConversations(scope)).items).toEqual([]);
    const fresh = await store.createConversation(
      scope,
      conversation.agentUid,
      "example",
    );
    const next = await store.beginTurn(fresh, "New", "agent");
    expect(next.turn.epoch).toBeGreaterThan(prepared.turn.epoch);
  });
  it("rolls back an entire send preparation when storage fails", async () => {
    const { conversation, key } = await fixture();
    const original = IDBObjectStore.prototype.put;
    const spy = vi
      .spyOn(IDBObjectStore.prototype, "put")
      .mockImplementation(function (
        this: IDBObjectStore,
        ...args: Parameters<IDBObjectStore["put"]>
      ) {
        if (this.name === "accounts")
          throw new DOMException("quota", "QuotaExceededError");
        return original.apply(this, args);
      });
    await expect(
      store.beginTurn(conversation, "Must not dispatch", "agent"),
    ).rejects.toThrow();
    spy.mockRestore();
    expect(await store.listMessages(key)).toEqual([]);
    expect((await store.conversationHistory(key)).turns).toEqual([]);
    expect((await store.getConversation(key))?.revision).toBe(
      conversation.revision,
    );
  });
  it("refuses deleting an active conversation and bounds output", async () => {
    const { conversation, key } = await fixture();
    const prepared = await store.beginTurn(conversation, "Question", "agent");
    await expect(
      store.deleteConversation(prepared.conversation),
    ).rejects.toThrow(/active/);
    await expect(
      store.saveTurn(
        prepared.turn,
        "x".repeat(LIMITS.outputBytes + 1),
        "completed",
      ),
    ).rejects.toThrow(/size/);
    await store.saveTurn(prepared.turn, "answer", "completed");
    const current = await store.getConversation(key);
    await store.deleteConversation(current!);
    expect(await store.listMessages(key)).toEqual([]);
  });
  it("does not clear another account's records", async () => {
    const a = await fixture(),
      b = await fixture();
    await store.clearAccount(a.scope);
    expect(await store.getConversation(b.key)).toBeDefined();
  });
  it("rejects late draft creation after the account was cleared", async () => {
    const { scope, conversation } = await fixture();
    const epoch = await store.checkStorage(scope);
    await store.clearAccount(scope);
    await expect(
      store.createConversation(scope, conversation.agentUid, "example", epoch),
    ).rejects.toThrow(/changed/);
    expect((await store.listConversations(scope)).items).toEqual([]);
  });
  it("does not edit a conversation with a pending request", async () => {
    const { conversation } = await fixture();
    const active = await store.beginTurn(conversation, "Question", "agent");
    await expect(
      store.updateConversation(active.conversation, { draft: "other tab" }),
    ).rejects.toThrow(/active/);
    await expect(
      store.updateConversation(active.conversation, { title: "rename" }),
    ).rejects.toThrow(/active/);
  });
  it("bounds titles by Unicode code point instead of splitting characters", async () => {
    const { conversation } = await fixture();
    const updated = await store.updateConversation(conversation, {
      title: "🙂".repeat(121),
    });
    expect(updated.title).toBe("🙂".repeat(120));
  });
});
