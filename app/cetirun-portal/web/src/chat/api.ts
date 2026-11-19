// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import {
  LIMITS,
  byteLength,
  type ChatDescription,
  type TextMessage,
} from "./types";

export class ChatError extends Error {
  constructor(
    message: string,
    readonly code: string,
    readonly notSent = false,
    readonly status?: number,
  ) {
    super(message);
  }
}

export interface ChatTarget {
  name: string;
  uid: string;
  scope: string;
  binding?: string;
}
function headers(target: ChatTarget): Record<string, string> {
  return {
    "X-CetiRun-Chat-Scope": target.scope,
    "X-CetiRun-Agent-UID": target.uid,
    ...(target.binding ? { "X-CetiRun-Chat-Binding": target.binding } : {}),
  };
}

async function call(
  target: ChatTarget,
  suffix: string,
  options: RequestInit = {},
) {
  const response = await fetch(
    `/api/v1/instances/${encodeURIComponent(target.name)}/chat${suffix}`,
    {
      ...options,
      credentials: "same-origin",
      redirect: "error",
      cache: "no-store",
      headers: { ...headers(target), ...options.headers },
    },
  );
  if (!response.ok) {
    let code = "chat_unavailable",
      message =
        "Chat is unavailable. Refresh the instance before sending again.";
    if (response.headers.get("Content-Type")?.includes("application/json")) {
      const value = await response.json();
      if (typeof value.error?.code === "string") code = value.error.code;
      if (typeof value.error?.message === "string")
        message = value.error.message.slice(0, 1024);
    }
    throw new ChatError(
      message,
      code,
      response.headers.get("X-CetiRun-Chat-Dispatch") === "not-sent",
      response.status,
    );
  }
  return response;
}

export async function describeChat(
  target: ChatTarget,
  signal?: AbortSignal,
): Promise<ChatDescription> {
  const response = await call(target, "", { signal });
  if (!response.headers.get("Content-Type")?.includes("application/json"))
    throw new ChatError(
      "Sign in again to use Chat.",
      "not_authenticated",
      true,
    );
  const value = await response.json();
  if (
    value.agentUid !== target.uid ||
    typeof value.available !== "boolean" ||
    !value.limits
  )
    throw new ChatError(
      "The instance changed. Refresh the page.",
      "agent_changed",
      true,
    );
  return value;
}

export async function chatModels(
  target: ChatTarget,
  signal?: AbortSignal,
): Promise<string[]> {
  const response = await call(target, "/v1/models", { signal });
  if (!response.headers.get("Content-Type")?.includes("application/json"))
    throw new ChatError(
      "Sign in again to use Chat.",
      "not_authenticated",
      true,
    );
  const value = await response.json();
  if (
    value.object !== "list" ||
    !Array.isArray(value.data) ||
    value.data.length > 256 ||
    value.data.some(
      (m: { id?: unknown }) =>
        typeof m.id !== "string" || byteLength(m.id) > 256,
    )
  )
    throw new ChatError(
      "The Agent returned an invalid model list.",
      "agent_protocol_error",
      true,
    );
  return value.data.map((model: { id: string }) => model.id);
}

export class EventParser {
  private buffer = "";
  push(text: string): string[] {
    this.buffer += text;
    const frames: string[] = [];
    for (;;) {
      const match = /\r?\n\r?\n/.exec(this.buffer);
      if (!match) break;
      const frame = this.buffer.slice(0, match.index);
      if (byteLength(frame) > 256 * 1024)
        throw new Error("The response event is too large.");
      this.buffer = this.buffer.slice(match.index + match[0].length);
      const data = frame
        .split(/\r?\n/)
        .filter((line) => line.startsWith("data:"))
        .map((line) => line.slice(5).replace(/^ /, ""));
      if (data.length) frames.push(data.join("\n"));
    }
    if (byteLength(this.buffer) > 256 * 1024)
      throw new Error("The response event is too large.");
    return frames;
  }
}

export async function sendChat(
  target: ChatTarget,
  model: string,
  messages: TextMessage[],
  requestId: string,
  signal: AbortSignal,
  onText: (text: string) => Promise<void>,
): Promise<string> {
  const body = JSON.stringify({ model, messages, stream: true });
  if (byteLength(body) > LIMITS.requestBytes)
    throw new ChatError(
      "The conversation exceeds the request limit.",
      "chat_request_too_large",
      true,
    );
  const response = await call(target, "/v1/chat/completions", {
    method: "POST",
    signal,
    headers: {
      "Content-Type": "application/json",
      "X-CetiRun-Request-ID": requestId,
    },
    body,
  });
  if (
    !response.body ||
    !response.headers.get("Content-Type")?.includes("text/event-stream")
  )
    throw new ChatError(
      "The Agent did not return a chat stream.",
      "agent_protocol_error",
    );
  const reader = response.body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  const parser = new EventParser();
  let bytes = 0,
    finish: string | undefined;
  try {
    for (;;) {
      const chunk = await reader.read();
      if (chunk.done) break;
      bytes += chunk.value.byteLength;
      if (bytes > LIMITS.outputBytes)
        throw new Error("The response exceeded its size limit.");
      for (const data of parser.push(
        decoder.decode(chunk.value, { stream: true }),
      )) {
        if (data === "[DONE]") {
          if (!finish) throw new Error("The response ended without a result.");
          return finish;
        }
        const value = JSON.parse(data);
        if (value.error) {
          const reported = value.error.code === "agent_reported_error";
          throw new ChatError(
            reported
              ? "The Agent reported a failed response. Earlier tool actions may have completed; the request will not be replayed."
              : "The response was interrupted. The Agent may still be running.",
            reported ? "agent_reported_error" : "agent_stream_interrupted",
          );
        }
        if (
          !Array.isArray(value.choices) ||
          value.choices.length !== 1 ||
          value.choices[0].index !== 0 ||
          finish
        )
          throw new Error("The Agent returned an unsupported response.");
        const choice = value.choices[0];
        if (choice.delta?.tool_calls || choice.delta?.function_call)
          throw new Error("Client-side tools are not supported.");
        if (choice.delta?.content != null) {
          if (typeof choice.delta.content !== "string")
            throw new Error("Only text responses are supported.");
          await onText(choice.delta.content);
        }
        if (choice.finish_reason != null) {
          if (
            !["stop", "length", "content_filter"].includes(choice.finish_reason)
          )
            throw new Error("The response did not finish normally.");
          finish = choice.finish_reason;
        }
      }
    }
    throw new ChatError(
      "The connection ended before completion. The Agent may still be running.",
      "agent_stream_interrupted",
    );
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}
