// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

export const LIMITS = {
  requestBytes: 1048576,
  inputBytes: 65536,
  outputBytes: 4194304,
  messageCount: 100,
  conversations: 100,
  messages: 2000,
  accountBytes: 50 * 1048576,
} as const;
export type ResultState =
  "pending" | "completed" | "incomplete" | "failed" | "interrupted";
export type DispatchState = "prepared" | "dispatching" | "not-sent";
export type ConversationKey = [string, string, string];
export interface Conversation {
  scope: string;
  agentUid: string;
  id: string;
  instanceName: string;
  title: string;
  model: string;
  draft: string;
  revision: number;
  createdAt: number;
  updatedAt: number;
  nextSeq: number;
  reviewedThrough?: number;
}
export interface Turn {
  scope: string;
  agentUid: string;
  conversationId: string;
  id: string;
  seq: number;
  requestId: string;
  model: string;
  dispatchState: DispatchState;
  resultState: ResultState;
  finishReason?: string;
  errorCode?: string;
  createdAt: number;
  endedAt?: number;
  contextTurnIds: string[];
  omittedTurns: number;
  reservedBytes: number;
  epoch: number;
}
export interface ChatMessage {
  scope: string;
  agentUid: string;
  conversationId: string;
  seq: number;
  turnId: string;
  role: "user" | "assistant";
  text: string;
  bytes: number;
  updatedAt: number;
}
export interface TextMessage {
  role: "user" | "assistant";
  content: string;
}
export interface ChatDescription {
  eligible: boolean;
  available: boolean;
  reason: string;
  agentUid: string;
  bindingRevision?: string;
  limits: {
    requestBytes: number;
    messageCount: number;
    inputBytes: number;
    outputBytes: number;
    requestTimeoutSeconds: number;
  };
}
export const conversationKey = (c: Conversation): ConversationKey => [
  c.scope,
  c.agentUid,
  c.id,
];
export const byteLength = (value: string) =>
  new TextEncoder().encode(value).length;

export function buildContext(
  history: { turn: Turn; user: string; assistant: string }[],
  input: string,
  model: string,
) {
  if (!input.trim() || byteLength(input) > LIMITS.inputBytes)
    throw new Error("Enter a message of at most 64 KiB.");
  const completed = history.filter(
    (item) => item.turn.resultState === "completed",
  );
  const messages: TextMessage[] = [{ role: "user", content: input }];
  const turnIds: string[] = [];
  let bytes = byteLength(JSON.stringify({ model, messages, stream: true }));
  for (let i = completed.length - 1; i >= 0 && turnIds.length < 49; i--) {
    const item = completed[i];
    const pair: TextMessage[] = [
      { role: "user", content: item.user },
      { role: "assistant", content: item.assistant },
    ];
    const extra =
      byteLength(JSON.stringify(pair[0])) +
      byteLength(JSON.stringify(pair[1])) +
      2;
    if (bytes + extra > LIMITS.requestBytes) break;
    bytes += extra;
    messages.unshift(...pair);
    turnIds.unshift(item.turn.id);
  }
  return { messages, turnIds, omitted: completed.length - turnIds.length };
}
