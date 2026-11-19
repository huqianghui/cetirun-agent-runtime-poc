// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect, vi, afterEach } from "vitest";
import { buildContext, byteLength, LIMITS, type Turn } from "./types";
import { ChatError, EventParser, sendChat } from "./api";

const turn = (id: string, resultState: Turn["resultState"] = "completed") =>
  ({ id, resultState }) as Turn;
describe("chat context", () => {
  it("uses text history without native session IDs or failed turns", () => {
    const result = buildContext(
      [
        { turn: turn("one"), user: "First", assistant: "Answer" },
        {
          turn: turn("failed", "interrupted"),
          user: "Unknown action",
          assistant: "Partial",
        },
      ],
      "Next",
      "agent",
    );
    expect(result.messages).toEqual([
      { role: "user", content: "First" },
      { role: "assistant", content: "Answer" },
      { role: "user", content: "Next" },
    ]);
    expect(result.turnIds).toEqual(["one"]);
  });
  it("keeps at most 49 complete pairs and reports omitted history", () => {
    const result = buildContext(
      Array.from({ length: 60 }, (_, i) => ({
        turn: turn(String(i)),
        user: `Question ${i}`,
        assistant: `Answer ${i}`,
      })),
      "Next",
      "agent",
    );
    expect(result.messages).toHaveLength(99);
    expect(result.omitted).toBe(11);
    expect(result.turnIds[0]).toBe("11");
  });
  it("bounds exact encoded bytes and never jumps over a too-large recent pair", () => {
    const history = [
      { turn: turn("old"), user: "old", assistant: "old" },
      { turn: turn("new"), user: "new", assistant: "中".repeat(400000) },
    ];
    const result = buildContext(history, "Next", "agent");
    expect(result.messages).toHaveLength(1);
    expect(result.omitted).toBe(2);
    expect(history[1].assistant.length).toBe(400000);
    expect(
      byteLength(
        JSON.stringify({
          model: "agent",
          messages: result.messages,
          stream: true,
        }),
      ),
    ).toBeLessThan(LIMITS.requestBytes);
  });
  it("rejects oversized or blank current input without silently shortening it", () => {
    expect(() => buildContext([], " ", "agent")).toThrow();
    expect(() => buildContext([], "中".repeat(22000), "agent")).toThrow();
  });
});
describe("SSE framing", () => {
  it("preserves split CRLF and multi-line data", () => {
    const parser = new EventParser();
    expect(parser.push(": heartbeat\r\ndata: {\r")).toEqual([]);
    expect(parser.push('\ndata: "a":1}\r\n\r\n')).toEqual(['{\n"a":1}']);
    expect(parser.push("data: [DONE]\n\n")).toEqual(["[DONE]"]);
  });

  describe("chat terminal failures", () => {
    afterEach(() => vi.unstubAllGlobals());

    it.each(["agent_reported_error", "agent_stream_interrupted"])(
      "preserves %s without trusting the upstream message or replaying",
      async (code) => {
        const fetcher = vi.fn().mockResolvedValue(
          new Response(
            `data: ${JSON.stringify({ error: { code, message: "secret-sentinel" } })}\n\n`,
            { headers: { "Content-Type": "text/event-stream" } },
          ),
        );
        vi.stubGlobal("fetch", fetcher);
        const result = sendChat(
          { name: "fixture", uid: "fixture-uid", scope: "fixture-scope" },
          "fixture-model",
          [{ role: "user", content: "Synthetic request." }],
          "fixture-request",
          new AbortController().signal,
          async () => {},
        );
        await expect(result).rejects.toBeInstanceOf(ChatError);
        await expect(result).rejects.toMatchObject({
          code,
          message:
            code === "agent_reported_error"
              ? "The Agent reported a failed response. Earlier tool actions may have completed; the request will not be replayed."
              : "The response was interrupted. The Agent may still be running.",
        });
        expect(fetcher).toHaveBeenCalledTimes(1);
      },
    );
  });
  it("does not mistake a network chunk for a complete event", () => {
    const parser = new EventParser();
    expect(parser.push("data: hello")).toEqual([]);
    expect(parser.push(" world\n\n")).toEqual(["hello world"]);
  });
  it("bounds unfinished events", () => {
    expect(() => new EventParser().push("x".repeat(256 * 1024 + 1))).toThrow();
  });
});
