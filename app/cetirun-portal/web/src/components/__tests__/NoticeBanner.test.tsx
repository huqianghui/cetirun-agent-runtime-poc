// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NoticeBanner } from "../NoticeBanner";

function mockConfig(noticeBanner: string, ok = true) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok,
      headers: { get: () => (ok ? "application/json" : "text/html") },
      json: () => Promise.resolve({ noticeBanner }),
    }),
  );
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe("NoticeBanner", () => {
  it("renders nothing when noticeBanner is empty", async () => {
    mockConfig("");
    const { container } = render(<NoticeBanner />);
    await waitFor(() => expect(fetch).toHaveBeenCalledOnce());
    expect(container.firstChild).toBeNull();
  });

  it("renders the banner message when noticeBanner is set", async () => {
    mockConfig("Scheduled maintenance tonight");
    render(<NoticeBanner />);
    await waitFor(() =>
      expect(screen.getByText("Scheduled maintenance tonight")).toBeInTheDocument(),
    );
  });

  it("hides the banner after dismiss", async () => {
    mockConfig("Dismiss me");
    render(<NoticeBanner />);
    await waitFor(() =>
      expect(screen.getByText("Dismiss me")).toBeInTheDocument(),
    );
    await userEvent.click(screen.getByRole("button", { name: /dismiss/i }));
    expect(screen.queryByText("Dismiss me")).not.toBeInTheDocument();
  });

  it("renders nothing when config fetch returns non-JSON response", async () => {
    mockConfig("", false);
    const { container } = render(<NoticeBanner />);
    await waitFor(() => expect(fetch).toHaveBeenCalledOnce());
    expect(container.firstChild).toBeNull();
  });
});
