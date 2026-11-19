// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { afterEach, describe, expect, it, vi } from "vitest";
import { ResponseCheckpoint } from "./checkpoint";

afterEach(() => vi.useRealTimers());

describe("stream checkpoints", () => {
  it("saves a sparse response after 500 ms without waiting for another chunk", async () => {
    vi.useFakeTimers();
    const write = vi.fn().mockResolvedValue(undefined);
    const checkpoint = new ResponseCheckpoint(write, vi.fn());
    await checkpoint.append("first fragment");
    expect(write).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(500);
    expect(write).toHaveBeenCalledExactlyOnceWith("first fragment");
    await checkpoint.stop();
  });

  it("uses one writer and applies backpressure at the byte threshold", async () => {
    let release!: () => void;
    const blocked = new Promise<void>((resolve) => {
      release = resolve;
    });
    const write = vi
      .fn()
      .mockImplementationOnce(() => blocked)
      .mockResolvedValue(undefined);
    const checkpoint = new ResponseCheckpoint(write, vi.fn());
    const first = checkpoint.append("x".repeat(16384));
    const second = checkpoint.append("tail");
    expect(write).toHaveBeenCalledTimes(1);
    release();
    await Promise.all([first, second]);
    expect(write.mock.calls.map(([text]) => text.length)).toEqual([
      16384, 16388,
    ]);
    await checkpoint.stop();
  });

  it("cancels pending timers before committing the final result", async () => {
    vi.useFakeTimers();
    const write = vi.fn();
    const checkpoint = new ResponseCheckpoint(write, vi.fn());
    await checkpoint.append("final text");
    await checkpoint.stop();
    await vi.advanceTimersByTimeAsync(1000);
    expect(write).not.toHaveBeenCalled();
    expect(checkpoint.text).toBe("final text");
  });

  it("reports an asynchronous storage failure and stops accepting chunks", async () => {
    vi.useFakeTimers();
    const onError = vi.fn();
    const checkpoint = new ResponseCheckpoint(
      vi.fn().mockRejectedValue(new Error("quota")),
      onError,
    );
    await checkpoint.append("keep visible");
    await vi.advanceTimersByTimeAsync(500);
    expect(onError).toHaveBeenCalledTimes(1);
    await expect(checkpoint.append("later")).rejects.toThrow("quota");
    await expect(checkpoint.stop()).rejects.toThrow("quota");
    expect(checkpoint.text).toBe("keep visible");
  });
});
