// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import "fake-indexeddb/auto";
import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { flushSync } from "react-dom";
import { setImmediate } from "node:timers";
import type { Instance } from "../types/instance";
import ChatPage from "./ChatPage";
import * as store from "./store";
import { conversationKey, type ChatDescription } from "./types";
import { ChatError, describeChat, sendChat } from "./api";
import * as runtime from "./runtime";

const identity = vi.hoisted(() => ({
  scope: "",
  checking: false,
  invalidate: vi.fn(),
}));
vi.mock("./ChatAccount", () => ({
  useChatAccount: () => ({ ...identity, verify: async () => identity.scope }),
}));
vi.mock("./runtime", async (original) => ({
  ...(await original<typeof import("./runtime")>()),
  browserSupport: () => undefined,
  subscribe: () => () => {},
  notify: vi.fn(),
  withAgentLock: async (
    _scope: string,
    _uid: string,
    work: () => Promise<unknown>,
  ) => work(),
}));
vi.mock("./api", async (original) => ({
  ...(await original<typeof import("./api")>()),
  describeChat: vi.fn().mockResolvedValue({
    eligible: true,
    available: true,
    reason: "ready",
    agentUid: "synthetic-uid",
    bindingRevision: "binding",
    limits: {},
  }),
  chatModels: vi.fn().mockResolvedValue(["example-agent"]),
  sendChat: vi.fn(),
}));

const instance: Instance = {
  uid: "synthetic-uid",
  resourceVersion: "1",
  instanceName: "example",
  displayName: "Research workspace",
  agent: { type: "custom", port: 8080 },
  alias: "alice",
  team: "example-team",
  phase: "Ready",
  upgradeAvailable: false,
  createdAt: "2026-10-05T00:00:00Z",
};
const input = () =>
  screen.getByRole("textbox", { name: "Message Research workspace" });
// Match WorkspacePage's account/Agent lifetime boundary.
const page = (value = instance, id?: string) => (
  <MemoryRouter
    initialEntries={[
      `/instances/example/chat${id ? `?conversation=${id}` : ""}`,
    ]}
  >
    <ChatPage key={`${value.uid}:${identity.scope}`} instance={value} />
  </MemoryRouter>
);
async function open(id?: string) {
  const view = render(page(instance, id));
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Chat model" })).toHaveValue(
      "example-agent",
    ),
  );
  await waitFor(() => expect(input()).not.toBeDisabled());
  return view;
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

const finalReads = [
  "getConversation",
  "conversationHistory",
  "listMessages",
  "listConversations",
] as const;

function pauseFinalRead(method: (typeof finalReads)[number]) {
  const agent = store.forAgent(identity.scope, instance.uid);
  const pending = deferred<void>();
  const reached = vi.fn();
  let finalized = false;
  const pause = <Args extends unknown[], Result>(
    name: (typeof finalReads)[number],
    read: (...args: Args) => Promise<Result>,
  ) =>
    async (...args: Args) => {
      const value = await read(...args);
      if (finalized && name === method) {
        reached();
        await pending.promise;
      }
      return value;
    };
  vi.spyOn(store, "forAgent").mockReturnValue({
    ...agent,
    getConversation: pause("getConversation", agent.getConversation),
    conversationHistory: pause(
      "conversationHistory",
      agent.conversationHistory,
    ),
    listMessages: pause("listMessages", agent.listMessages),
    listConversations: pause("listConversations", agent.listConversations),
    saveTurn: async (...args) => {
      const value = await agent.saveTurn(...args);
      if (args[2] !== "pending") finalized = true;
      return value;
    },
  });
  return { pending, reached };
}

function sendMessage(text = "Synthetic question") {
  fireEvent.change(input(), { target: { value: text } });
  fireEvent.click(
    screen.getByRole("button", { name: "Send message" }),
  );
}

async function afterWindowTeardown(work: () => Promise<void>) {
  // An empty act only drains its own queue, not callbacks already scheduled
  // outside act. Yield to Node's scheduler while window is still available.
  // Deferred Chat completion stays pending until the access trap is armed.
  await act(async () => {
    await new Promise<void>((resolve) => setImmediate(resolve));
  });
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, "window");
  if (!descriptor) throw new Error("The test needs a jsdom window.");
  // Trap stale React dispatches even if their missing-window error is caught.
  const access = vi.fn(() => {
    throw new ReferenceError("window is not defined");
  });
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    get: access,
  });
  try {
    await work();
  } finally {
    Object.defineProperty(globalThis, "window", descriptor);
  }
  expect(access).not.toHaveBeenCalled();
}

beforeEach(() => {
  identity.scope = `test-${crypto.randomUUID()}`;
  identity.checking = false;
  identity.invalidate.mockReset();
  vi.mocked(runtime.notify).mockClear();
  vi.mocked(describeChat).mockClear();
  vi.mocked(sendChat)
    .mockReset()
    .mockImplementation(
      async (_target, _model, _messages, _id, signal, onText) => {
        signal.throwIfAborted();
        await onText("Synthetic reply");
        return "stop";
      },
    );
});
afterEach(() => vi.restoreAllMocks());

describe("window teardown test boundary", () => {
  it("drains committed React work before removing window", async () => {
    const effect = vi.fn();
    function CommittedPage() {
      useEffect(effect, []);
      return null;
    }
    const root = createRoot(document.createElement("div"));
    const actEnvironment = Object.getOwnPropertyDescriptor(
      globalThis,
      "IS_REACT_ACT_ENVIRONMENT",
    );
    // Deliberately commit outside act, as async work can while waitFor polls.
    Object.defineProperty(globalThis, "IS_REACT_ACT_ENVIRONMENT", {
      configurable: true,
      writable: true,
      value: false,
    });
    try {
      flushSync(() => root.render(<CommittedPage />));
      root.unmount();
    } finally {
      if (actEnvironment)
        Object.defineProperty(
          globalThis,
          "IS_REACT_ACT_ENVIRONMENT",
          actEnvironment,
        );
      else Reflect.deleteProperty(globalThis, "IS_REACT_ACT_ENVIRONMENT");
    }
    expect(effect).toHaveBeenCalledTimes(1);
    await afterWindowTeardown(async () => {
      await new Promise<void>((resolve) => setImmediate(resolve));
    });
  });

  it("still detects a caught stale React dispatch and restores window", async () => {
    let dispatch!: () => void;
    function StalePage() {
      const [, setValue] = useState(0);
      dispatch = () => setValue(1);
      return null;
    }
    const view = render(<StalePage />);
    view.unmount();
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, "window");
    await expect(
      afterWindowTeardown(async () => {
        try {
          dispatch();
        } catch {
          // Production code may catch the missing-window error.
        }
      }),
    ).rejects.toThrow("to not be called");
    expect(Object.getOwnPropertyDescriptor(globalThis, "window")).toEqual(
      descriptor,
    );
  });
});

describe("Custom Chat interactions", () => {
  it("flushes a draft when creating a conversation before the debounce fires", async () => {
    await store.checkStorage(identity.scope);
    const original = await store.createConversation(
      identity.scope,
      instance.uid,
      instance.instanceName,
    );
    await open(original.id);
    fireEvent.change(input(), {
      target: { value: "Keep this immediate draft" },
    });
    fireEvent.click(
      screen.getByRole("button", { name: "New chat", exact: true }),
    );
    await waitFor(async () =>
      expect(
        (await store.getConversation(conversationKey(original)))?.draft,
      ).toBe("Keep this immediate draft"),
    );
    await waitFor(() => expect(input()).toHaveValue(""));
    expect(sendChat).not.toHaveBeenCalled();
  });

  it("flushes a draft when selecting a different conversation", async () => {
    await store.checkStorage(identity.scope);
    const first = await store.createConversation(
      identity.scope,
      instance.uid,
      instance.instanceName,
    );
    const second = await store.createConversation(
      identity.scope,
      instance.uid,
      instance.instanceName,
    );
    await store.updateConversation(second, { title: "Other conversation" });
    await open(first.id);
    fireEvent.change(input(), { target: { value: "Unsent text" } });
    fireEvent.click(
      screen.getByRole("button", { name: "Other conversation", exact: true }),
    );
    await waitFor(() => expect(input()).toHaveValue(""));
    expect((await store.getConversation(conversationKey(first)))?.draft).toBe(
      "Unsent text",
    );
    expect(sendChat).not.toHaveBeenCalled();
  });

  it("sends one request per click and supplies completed history to the next turn", async () => {
    await open();
    fireEvent.change(input(), { target: { value: "First question" } });
    fireEvent.click(
      screen.getByRole("button", { name: "Send message", exact: true }),
    );
    await screen.findByText("Synthetic reply");
    await waitFor(() => expect(input()).not.toBeDisabled());
    fireEvent.change(input(), { target: { value: "Second question" } });
    fireEvent.click(
      screen.getByRole("button", { name: "Send message", exact: true }),
    );
    await waitFor(() => expect(sendChat).toHaveBeenCalledTimes(2));
    expect(vi.mocked(sendChat).mock.calls[1][2]).toEqual([
      { role: "user", content: "First question" },
      { role: "assistant", content: "Synthetic reply" },
      { role: "user", content: "Second question" },
    ]);
    await waitFor(() => expect(input()).not.toBeDisabled());
  });

  it("does not offer sending when the initial storage transaction fails", async () => {
    const original = store.forAgent;
    vi.spyOn(store, "forAgent").mockImplementation((scope, uid) => ({
      ...original(scope, uid),
      checkStorage: async () => {
        throw new Error("storage unavailable");
      },
    }));
    render(
      <MemoryRouter>
        <ChatPage instance={instance} />
      </MemoryRouter>,
    );
    await screen.findByText("storage unavailable");
    expect(
      screen.getByRole("button", { name: "Send message", exact: true }),
    ).toBeDisabled();
    expect(sendChat).not.toHaveBeenCalled();
  });
  it("invalidates the local view when authentication expires during a send", async () => {
    vi.mocked(sendChat).mockRejectedValueOnce(
      new ChatError("Sign in again", "not_authenticated", true, 401),
    );
    await open();
    fireEvent.change(input(), { target: { value: "Private question" } });
    fireEvent.click(
      screen.getByRole("button", { name: "Send message", exact: true }),
    );
    await waitFor(() => expect(identity.invalidate).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(input()).not.toBeDisabled());
    expect(sendChat).toHaveBeenCalledTimes(1);
  });
});

describe("Chat send lifecycle", () => {
  describe.each(finalReads)("finalizing %s", (method) => {
    it.each(["success", "failure"] as const)(
      "settles a late %s after window teardown without updating React",
      async (outcome) => {
        const { pending, reached } = pauseFinalRead(method);
        const view = await open();
        sendMessage();
        await waitFor(() => expect(reached).toHaveBeenCalledTimes(1));
        view.unmount();

        await afterWindowTeardown(async () => {
          if (outcome === "success") pending.resolve();
          else pending.reject(new Error("Synthetic history read failure"));
          await vi.waitFor(() =>
            expect(runtime.notify).toHaveBeenCalledTimes(1),
          );
        });

        const [notice] = vi.mocked(runtime.notify).mock.calls[0];
        expect(notice).toMatchObject({
          kind: "changed",
          scope: identity.scope,
          agentUid: instance.uid,
        });
        const history = await store.conversationHistory([
          identity.scope,
          instance.uid,
          notice.conversationId!,
        ]);
        expect(history.turns).toHaveLength(1);
        expect(history.turns[0].resultState).toBe("completed");
        expect(history.history[0].assistant).toBe("Synthetic reply");
        expect(sendChat).toHaveBeenCalledTimes(1);
      },
    );

    it("still reports a history read failure while mounted", async () => {
      const { pending, reached } = pauseFinalRead(method);
      await open();
      sendMessage();
      await waitFor(() => expect(reached).toHaveBeenCalledTimes(1));
      await act(async () => {
        pending.reject(new Error("Synthetic history read failure"));
        await vi.waitFor(() =>
          expect(runtime.notify).toHaveBeenCalledTimes(1),
        );
      });

      expect(screen.getByText("Synthetic history read failure")).toBeVisible();
      expect(input()).toHaveAttribute("readonly");
      expect(
        screen.getByText("Not saved · copy your text before leaving"),
      ).toBeVisible();
      expect(
        screen.getByRole("button", { name: "Send message" }),
      ).toBeDisabled();
      expect(sendChat).toHaveBeenCalledTimes(1);
    });
  });

  it("keeps sending and conversation changes disabled until history refresh finishes", async () => {
    const { pending, reached } = pauseFinalRead("getConversation");
    await open();
    sendMessage();
    await waitFor(() => expect(reached).toHaveBeenCalledTimes(1));
    const inputDisabled = input().hasAttribute("disabled");
    const newChatDisabled = screen
      .getByRole("button", { name: "New chat" })
      .hasAttribute("disabled");
    await act(async () => {
      pending.resolve();
      await vi.waitFor(() =>
        expect(runtime.notify).toHaveBeenCalledTimes(1),
      );
    });

    expect(inputDisabled).toBe(true);
    expect(newChatDisabled).toBe(true);
    expect(input()).not.toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "New chat" }));
    await waitFor(() => expect(input()).not.toBeDisabled());
    sendMessage("New conversation question");
    await waitFor(() => expect(runtime.notify).toHaveBeenCalledTimes(2));
    expect(vi.mocked(sendChat).mock.calls[1][2]).toEqual([
      { role: "user", content: "New conversation question" },
    ]);
  });

  it.each([
    ["success", "completed"],
    ["incomplete", "incomplete"],
    ["error", "failed"],
    ["abort", "interrupted"],
  ] as const)(
    "persists a late stream %s after unmount without replaying it",
    async (outcome, resultState) => {
      const response = deferred<string>();
      vi.mocked(sendChat).mockImplementationOnce(
        async (_target, _model, _messages, _id, _signal, onText) => {
          await onText("Synthetic partial response");
          return response.promise;
        },
      );
      const view = await open();
      sendMessage();
      await waitFor(() => expect(sendChat).toHaveBeenCalledTimes(1));
      const signal = vi.mocked(sendChat).mock.calls[0][4];
      view.unmount();
      expect(signal.aborted).toBe(true);

      await afterWindowTeardown(async () => {
        if (outcome === "success") response.resolve("stop");
        else if (outcome === "incomplete") response.resolve("length");
        else if (outcome === "error")
          response.reject(
            new ChatError("Synthetic stream failure", "agent_reported_error"),
          );
        else response.reject(new DOMException("Stopped", "AbortError"));
        await vi.waitFor(() =>
          expect(runtime.notify).toHaveBeenCalledTimes(1),
        );
      });

      const { items } = await store.listConversations(
        identity.scope,
        instance.uid,
      );
      expect(items).toHaveLength(1);
      const key = conversationKey(items[0]);
      const { turns } = await store.conversationHistory(key);
      expect(turns).toHaveLength(1);
      expect(turns[0]).toMatchObject({
        resultState,
        dispatchState: "dispatching",
        reservedBytes: 0,
      });
      if (outcome === "success" || outcome === "incomplete")
        expect(turns[0].finishReason).toBe(
          outcome === "success" ? "stop" : "length",
        );
      expect((await store.listMessages(key))[1].text).toBe(
        "Synthetic partial response",
      );
      expect(sendChat).toHaveBeenCalledTimes(1);
    },
  );

  it("does not prepare a turn when target discovery resolves after unmount", async () => {
    const target = deferred<ChatDescription>();
    const agent = store.forAgent(identity.scope, instance.uid);
    const beginTurn = vi.fn(agent.beginTurn);
    vi.spyOn(store, "forAgent").mockReturnValue({ ...agent, beginTurn });
    const flight = runtime.flight;
    const released = vi.fn();
    vi.spyOn(runtime, "flight").mockImplementation((scope) => {
      const work = flight(scope);
      return {
        ...work,
        release: () => {
          work.release();
          released();
        },
      };
    });
    const view = await open();
    const description = await vi.mocked(describeChat).mock.results[0].value;
    vi.mocked(describeChat).mockReturnValueOnce(target.promise);
    sendMessage();
    await waitFor(() => expect(describeChat).toHaveBeenCalledTimes(2));
    view.unmount();
    target.resolve(description);
    await vi.waitFor(() => expect(released).toHaveBeenCalledTimes(1));

    expect(beginTurn).not.toHaveBeenCalled();
    expect(sendChat).not.toHaveBeenCalled();
    expect(runtime.notify).not.toHaveBeenCalled();
  });

  it.each(["beginTurn", "listMessages", "saveTurn"] as const)(
    "records an aborted preparation as not sent when %s settles after unmount",
    async (method) => {
      const pending = deferred<void>();
      const reached = vi.fn();
      const agent = store.forAgent(identity.scope, instance.uid);
      const pause = async <T,>(value: T, step: typeof method) => {
        if (step === method) {
          reached();
          await pending.promise;
        }
        return value;
      };
      vi.spyOn(store, "forAgent").mockReturnValue({
        ...agent,
        beginTurn: async (...args) =>
          pause(await agent.beginTurn(...args), "beginTurn"),
        listMessages: async (...args) =>
          pause(await agent.listMessages(...args), "listMessages"),
        saveTurn: async (...args) => {
          const turn = await agent.saveTurn(...args);
          return args[3]?.dispatchState === "dispatching"
            ? pause(turn, "saveTurn")
            : turn;
        },
      });
      const view = await open();
      sendMessage();
      await waitFor(() => expect(reached).toHaveBeenCalledTimes(1));
      view.unmount();
      await afterWindowTeardown(async () => {
        pending.resolve();
        await vi.waitFor(() =>
          expect(runtime.notify).toHaveBeenCalledTimes(1),
        );
      });

      expect(sendChat).not.toHaveBeenCalled();
      const { items } = await agent.listConversations();
      expect(items).toHaveLength(1);
      const { turns } = await agent.conversationHistory(items[0].id);
      expect(turns).toHaveLength(1);
      expect(turns[0]).toMatchObject({
        resultState: "failed",
        dispatchState: "not-sent",
        reservedBytes: 0,
      });
      expect((await agent.listMessages(items[0].id))[0].text).toBe(
        "Synthetic question",
      );
    },
  );

  it.each(["account", "Agent UID"] as const)(
    "keeps a replacement %s chat independent of an old stream settling",
    async (change) => {
      const oldResponse = deferred<string>();
      const newResponse = deferred<string>();
      vi.mocked(sendChat)
        .mockImplementationOnce(
          async (_target, _model, _messages, _id, _signal, onText) => {
            await onText("Old synthetic response");
            return oldResponse.promise;
          },
        )
        .mockImplementationOnce(
          async (_target, _model, _messages, _id, _signal, onText) => {
            await onText("New synthetic response");
            return newResponse.promise;
          },
        );
      const view = await open();
      sendMessage("Old question");
      await waitFor(() => expect(sendChat).toHaveBeenCalledTimes(1));
      if (change === "account") identity.scope = `test-${crypto.randomUUID()}`;
      const next =
        change === "Agent UID"
          ? { ...instance, uid: "synthetic-replacement-uid" }
          : instance;
      view.rerender(page(next));
      await waitFor(() => expect(input()).not.toBeDisabled());
      expect(vi.mocked(sendChat).mock.calls[0][4].aborted).toBe(true);
      sendMessage("New question");
      await waitFor(() => expect(sendChat).toHaveBeenCalledTimes(2));
      await act(async () => {
        oldResponse.reject(
          new ChatError("Old synthetic failure", "agent_reported_error"),
        );
        await vi.waitFor(() =>
          expect(runtime.notify).toHaveBeenCalledTimes(1),
        );
      });

      expect(
        screen.queryByText("Old synthetic response"),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByText("Old synthetic failure"),
      ).not.toBeInTheDocument();
      expect(screen.getByText("New synthetic response")).toBeVisible();
      expect(input()).toBeDisabled();
      fireEvent.click(screen.getByRole("button", { name: "Stop response" }));
      expect(vi.mocked(sendChat).mock.calls[1][4].aborted).toBe(true);
      await act(async () => {
        newResponse.reject(new DOMException("Stopped", "AbortError"));
        await vi.waitFor(() =>
          expect(runtime.notify).toHaveBeenCalledTimes(2),
        );
      });

      const { items } = await store.listConversations(identity.scope, next.uid);
      expect(items).toHaveLength(1);
      expect(
        (await store.listMessages(conversationKey(items[0]))).map(
          (m) => m.text,
        ),
      ).toEqual(["New question", "New synthetic response"]);
      expect(sendChat).toHaveBeenCalledTimes(2);
    },
  );
});
