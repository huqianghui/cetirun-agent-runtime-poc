// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

export interface ChatNotice {
  kind: "changed" | "cleared" | "signed-out";
  scope: string;
  agentUid?: string;
  conversationId?: string;
}
const flights = new Map<string, Set<AbortController>>();
const listeners = new Set<(notice: ChatNotice) => void>();
let channel: BroadcastChannel | undefined;
function ensureChannel() {
  if (!channel && typeof BroadcastChannel !== "undefined") {
    channel = new BroadcastChannel("cetirun-portal-chat");
    channel.onmessage = (event) => {
      const value = event.data;
      if (
        !value ||
        typeof value.scope !== "string" ||
        !["changed", "cleared", "signed-out"].includes(value.kind)
      )
        return;
      if (value.kind !== "changed") stopScope(value.scope);
      for (const listener of listeners) listener(value);
    };
  }
}
export function notify(notice: ChatNotice) {
  ensureChannel();
  channel?.postMessage(notice);
  if (notice.kind !== "changed")
    for (const listener of listeners) listener(notice);
}
export function subscribe(listener: (notice: ChatNotice) => void) {
  ensureChannel();
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
export function stopScope(scope: string) {
  for (const controller of flights.get(scope) ?? []) controller.abort();
}
export function stopAll() {
  for (const scope of flights.keys()) stopScope(scope);
}
export function flight(scope: string) {
  const controller = new AbortController();
  const set = flights.get(scope) ?? new Set<AbortController>();
  set.add(controller);
  flights.set(scope, set);
  return {
    controller,
    release: () => {
      set.delete(controller);
      if (set.size === 0) flights.delete(scope);
    },
  };
}
export function browserSupport(): string | undefined {
  if (!globalThis.isSecureContext)
    return "Chat needs a secure browser connection.";
  if (
    !globalThis.indexedDB ||
    !navigator.locks ||
    typeof BroadcastChannel === "undefined" ||
    typeof ReadableStream === "undefined"
  )
    return "This browser does not support the local history and tab coordination required for Chat.";
  return undefined;
}
export async function withAgentLock<T>(
  scope: string,
  agentUid: string,
  work: () => Promise<T>,
): Promise<T> {
  if (!navigator.locks)
    throw new Error("This browser cannot coordinate chat tabs.");
  return navigator.locks.request(
    `cetirun-chat:${scope}:${agentUid}`,
    { mode: "exclusive", ifAvailable: true },
    async (lock) => {
      if (!lock)
        throw new Error(
          "Another tab is using this Agent. Wait for it to finish or stop it there.",
        );
      return work();
    },
  );
}
