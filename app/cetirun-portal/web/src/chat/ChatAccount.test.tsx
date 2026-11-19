// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import "fake-indexeddb/auto";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  act,
  fireEvent,
  render,
  renderHook,
  screen,
  waitFor,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fetchMe } from "../api/me";
import { ChatAccountProvider, useChatAccount } from "./ChatAccount";
import { flight } from "./runtime";
import * as runtime from "./runtime";
import * as store from "./store";

vi.mock("../api/me", () => ({ fetchMe: vi.fn() }));
const alice = {
  alias: "alice",
  displayName: "Alice",
  email: "alice@example.com",
  isAdmin: false,
  chatStorageScope: "scope-alice",
};
function View() {
  const account = useChatAccount();
  return (
    <>
      <span>
        {account?.checking ? "checking" : (account?.scope ?? "no scope")}
      </span>
      <button onClick={() => account?.invalidate("scope-alice")}>
        Reject old request
      </button>
    </>
  );
}
async function open() {
  const cache = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={cache}>
      <ChatAccountProvider>
        <View />
      </ChatAccountProvider>
    </QueryClientProvider>,
  );
  await screen.findByText("scope-alice");
  return cache;
}
async function openAccount(expectedScope = alice.chatStorageScope) {
  const cache = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const view = renderHook(() => useChatAccount()!, {
    wrapper: ({ children }) => (
      <QueryClientProvider client={cache}>
        <ChatAccountProvider>{children}</ChatAccountProvider>
      </QueryClientProvider>
    ),
  });
  await waitFor(() => expect(view.result.current.scope).toBe(expectedScope));
  return { ...view, cache };
}
function deferClearing() {
  const clearAccount = store.clearAccount;
  let finish!: () => void;
  const clearing = new Promise<void>((resolve) => {
    finish = resolve;
  });
  vi.spyOn(store, "clearAccount").mockImplementationOnce(async (scope) => {
    await clearing;
    await clearAccount(scope);
  });
  return finish;
}
beforeEach(async () => {
  vi.mocked(fetchMe).mockReset().mockResolvedValue(alice);
  await store.clearAccount(alice.chatStorageScope);
});
afterEach(() => vi.restoreAllMocks());

describe("Chat account boundaries", () => {
  it("ignores an old request's rejection while a new identity is being verified or has arrived", async () => {
    await open();
    let resolve!: (me: typeof alice) => void;
    vi.mocked(fetchMe).mockReturnValueOnce(
      new Promise((done) => {
        resolve = done;
      }),
    );
    fireEvent.focus(window);
    fireEvent.click(screen.getByRole("button", { name: "Reject old request" }));
    expect(screen.getByText("checking")).toBeInTheDocument();
    await act(async () => resolve({ ...alice, chatStorageScope: "scope-bob" }));
    await screen.findByText("scope-bob");
    fireEvent.click(screen.getByRole("button", { name: "Reject old request" }));
    expect(screen.getByText("scope-bob")).toBeInTheDocument();
  });
  it("hides content during focus verification without cancelling the same account", async () => {
    await open();
    const work = flight("scope-alice");
    let resolve!: (me: typeof alice) => void;
    vi.mocked(fetchMe).mockReturnValueOnce(
      new Promise((done) => {
        resolve = done;
      }),
    );
    fireEvent.focus(window);
    expect(screen.getByText("checking")).toBeInTheDocument();
    expect(work.controller.signal.aborted).toBe(false);
    await act(async () => resolve(alice));
    await screen.findByText("scope-alice");
    expect(work.controller.signal.aborted).toBe(false);
    work.release();
  });
  it("cancels the old account and drops private query caches after an account change", async () => {
    const cache = await open();
    cache.setQueryData(["private-instance"], "synthetic private content");
    const work = flight("scope-alice");
    vi.mocked(fetchMe).mockResolvedValueOnce({
      ...alice,
      alias: "bob",
      chatStorageScope: "scope-bob",
    });
    fireEvent.focus(window);
    await screen.findByText("scope-bob");
    expect(work.controller.signal.aborted).toBe(true);
    expect(cache.getQueryData(["private-instance"])).toBeUndefined();
    work.release();
  });
  it("hides before a page is frozen in back-forward cache", async () => {
    await open();
    const work = flight("scope-alice");
    act(() =>
      window.dispatchEvent(
        new PageTransitionEvent("pagehide", { persisted: true }),
      ),
    );
    expect(screen.getByText("checking")).toBeInTheDocument();
    expect(work.controller.signal.aborted).toBe(true);
    fireEvent(window, new PageTransitionEvent("pageshow", { persisted: true }));
    await screen.findByText("scope-alice");
    work.release();
  });
  it("does not disrupt ordinary Portal identity when Chat has no configured realm", async () => {
    const cache = await open();
    vi.mocked(fetchMe).mockResolvedValueOnce({
      ...alice,
      chatStorageScope: undefined,
    });
    fireEvent.focus(window);
    await screen.findByText("no scope");
    await waitFor(() =>
      expect(cache.getQueryData(["me"])).toMatchObject({ alias: "alice" }),
    );
  });

  it("keeps the account and other tabs usable when clearing fails, then allows a retry", async () => {
    const first = await openAccount();
    const other = await openAccount();
    for (const view of [first, other])
      view.cache.setQueryData(["private-instance"], "synthetic private content");
    const conversation = await store.createConversation(
      alice.chatStorageScope,
      "agent-uid",
      "example",
    );
    const notices = vi.spyOn(runtime, "notify");
    const original = IDBObjectStore.prototype.put;
    const failure = vi.spyOn(IDBObjectStore.prototype, "put").mockImplementation(
      function (
        this: IDBObjectStore,
        ...args: Parameters<IDBObjectStore["put"]>
      ) {
        if (this.name === "accounts")
          throw new DOMException(
            "synthetic storage failure",
            "QuotaExceededError",
          );
        return original.apply(this, args);
      },
    );
    const work = flight(alice.chatStorageScope);
    await act(async () => {
      await expect(first.result.current.signOut(true)).rejects.toThrow();
    });
    failure.mockRestore();
    expect(work.controller.signal.aborted).toBe(true);
    work.release();
    expect(notices).not.toHaveBeenCalled();
    expect(
      (await store.listConversations(alice.chatStorageScope)).items,
    ).toEqual([conversation]);
    for (const view of [first, other]) {
      expect(view.result.current.scope).toBe(alice.chatStorageScope);
      expect(view.cache.getQueryData(["private-instance"])).toBe(
        "synthetic private content",
      );
      await act(async () => {
        await expect(view.result.current.verify()).resolves.toBe(
          alice.chatStorageScope,
        );
      });
    }
    await act(async () => first.result.current.signOut(true));
    expect(
      (await store.listConversations(alice.chatStorageScope)).items,
    ).toEqual([]);
    for (const view of [first, other]) {
      expect(view.result.current.scope).toBeUndefined();
      expect(view.cache.getQueryData(["private-instance"])).toBeUndefined();
    }
    expect(notices.mock.calls.map(([notice]) => notice.kind)).toEqual([
      "cleared",
      "signed-out",
    ]);
  });

  it("commits sign-out only after history clearing completes and fences late writes", async () => {
    const view = await openAccount();
    view.cache.setQueryData(["private-instance"], "synthetic private content");
    const conversation = await store.createConversation(
      alice.chatStorageScope,
      "agent-uid",
      "example",
    );
    const { turn } = await store.beginTurn(
      conversation,
      "Question",
      "example-model",
    );
    const notices = vi.spyOn(runtime, "notify");
    const finish = deferClearing();
    let signingOut!: Promise<void>;
    act(() => {
      signingOut = view.result.current.signOut(true);
    });
    expect(notices).not.toHaveBeenCalled();
    expect(view.result.current.scope).toBe(alice.chatStorageScope);
    expect(view.cache.getQueryData(["private-instance"])).toBe(
      "synthetic private content",
    );
    await act(async () => {
      finish();
      await signingOut;
    });
    expect(view.result.current.scope).toBeUndefined();
    expect(view.cache.getQueryData(["private-instance"])).toBeUndefined();
    await expect(
      store.saveTurn(turn, "Late answer", "completed"),
    ).rejects.toThrow(/changed/);
    expect(
      (await store.listConversations(alice.chatStorageScope)).items,
    ).toEqual([]);
    expect(notices.mock.calls.map(([notice]) => notice.kind)).toEqual([
      "cleared",
      "signed-out",
    ]);
    await expect(view.result.current.verify()).rejects.toThrow(/Sign in again/);
  });

  it("does not sign out a newly verified account when an old account finishes clearing", async () => {
    const view = await openAccount();
    const notices = vi.spyOn(runtime, "notify");
    const finish = deferClearing();
    let signingOut!: Promise<void>;
    act(() => {
      signingOut = view.result.current.signOut(true);
    });
    const outcome = expect(signingOut).rejects.toThrow(/identity.*changed/i);
    vi.mocked(fetchMe).mockResolvedValue({
      ...alice,
      chatStorageScope: "scope-bob",
    });
    await act(async () => {
      await view.result.current.verify();
    });
    view.cache.setQueryData(["private-instance"], "Bob's private content");
    await act(async () => {
      finish();
      await outcome;
    });
    expect(view.result.current.scope).toBe("scope-bob");
    expect(view.cache.getQueryData(["private-instance"])).toBe(
      "Bob's private content",
    );
    expect(notices.mock.calls.map(([notice]) => notice)).toEqual([
      { kind: "cleared", scope: alice.chatStorageScope },
    ]);
    await act(async () => {
      await expect(view.result.current.verify()).resolves.toBe("scope-bob");
    });
  });

  it("signs out without clearing history and leaves another account's tab alone", async () => {
    const first = await openAccount();
    const peer = await openAccount();
    vi.mocked(fetchMe).mockResolvedValueOnce({
      ...alice,
      chatStorageScope: "scope-bob",
    });
    const other = await openAccount("scope-bob");
    other.cache.setQueryData(["private-instance"], "Bob's private content");
    const conversation = await store.createConversation(
      alice.chatStorageScope,
      "agent-uid",
      "example",
    );
    const clear = vi.spyOn(store, "clearAccount");
    const notices = vi.spyOn(runtime, "notify");
    const work = flight(alice.chatStorageScope);
    await act(async () => first.result.current.signOut(false));
    expect(work.controller.signal.aborted).toBe(true);
    work.release();
    expect(clear).not.toHaveBeenCalled();
    expect(
      (await store.listConversations(alice.chatStorageScope)).items,
    ).toEqual([conversation]);
    expect(first.result.current.scope).toBeUndefined();
    expect(peer.result.current.scope).toBeUndefined();
    expect(other.result.current.scope).toBe("scope-bob");
    expect(other.cache.getQueryData(["private-instance"])).toBe(
      "Bob's private content",
    );
    expect(notices).toHaveBeenCalledExactlyOnceWith({
      kind: "signed-out",
      scope: alice.chatStorageScope,
    });
  });
});
