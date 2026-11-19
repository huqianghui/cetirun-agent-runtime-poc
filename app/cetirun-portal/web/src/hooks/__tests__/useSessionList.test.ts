// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, expect, it } from "vitest";
import { PAGE_SIZE, relativeTime, sortByActivityDesc, type SessionSummary } from "../useSessionList";

describe("useSessionList helpers", () => {
  it("PAGE_SIZE is 10", () => {
    expect(PAGE_SIZE).toBe(10);
  });

  it("sorts sessions by lastActivity descending", () => {
    const input: SessionSummary[] = [
      { id: "a", title: "A", lastActivity: 100 },
      { id: "b", title: "B", lastActivity: 300 },
      { id: "c", title: "C", lastActivity: 200 },
    ];
    expect(sortByActivityDesc(input).map((s) => s.id)).toEqual(["b", "c", "a"]);
  });

  it("formats relative time buckets", () => {
    const now = 1_000_000_000_000;
    expect(relativeTime(now, now)).toBe("just now");
    expect(relativeTime(now - 5 * 60_000, now)).toBe("5m ago");
    expect(relativeTime(now - 3 * 3_600_000, now)).toBe("3h ago");
    expect(relativeTime(now - 2 * 86_400_000, now)).toBe("2d ago");
  });
});
