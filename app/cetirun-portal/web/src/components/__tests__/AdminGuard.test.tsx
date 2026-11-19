// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AdminGuard } from "../AdminGuard";
import * as MeContext from "../../context/MeContext";

function renderGuard() {
  return render(
    <MemoryRouter initialEntries={["/admin"]}>
      <Routes>
        <Route path="/admin" element={<AdminGuard />}>
          <Route index element={<div>Admin Home</div>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

describe("AdminGuard", () => {
  it("renders Outlet for admin user", () => {
    vi.spyOn(MeContext, "useMe").mockReturnValue({
      me: { alias: "alice", displayName: "Alice", email: "a@x", isAdmin: true },
      isLoading: false,
      error: null,
    });
    renderGuard();
    expect(screen.getByText("Admin Home")).toBeInTheDocument();
  });

  it("renders 403 for non-admin user", () => {
    vi.spyOn(MeContext, "useMe").mockReturnValue({
      me: { alias: "bob", displayName: "Bob", email: "b@x", isAdmin: false },
      isLoading: false,
      error: null,
    });
    renderGuard();
    expect(screen.getByText(/403 Forbidden/)).toBeInTheDocument();
    expect(screen.queryByText("Admin Home")).not.toBeInTheDocument();
  });

  it("renders 403 when me query failed", () => {
    vi.spyOn(MeContext, "useMe").mockReturnValue({
      me: undefined,
      isLoading: false,
      error: new Error("boom"),
    });
    renderGuard();
    expect(screen.getByText(/403 Forbidden/)).toBeInTheDocument();
  });

  it("still admits admin when a background refetch errored but cached me is present", () => {
    vi.spyOn(MeContext, "useMe").mockReturnValue({
      me: { alias: "alice", displayName: "Alice", email: "a@x", isAdmin: true },
      isLoading: false,
      error: new Error("transient"),
    });
    renderGuard();
    expect(screen.getByText("Admin Home")).toBeInTheDocument();
  });

  it("renders spinner while loading", () => {
    vi.spyOn(MeContext, "useMe").mockReturnValue({
      me: undefined,
      isLoading: true,
      error: null,
    });
    renderGuard();
    expect(screen.queryByText("Admin Home")).not.toBeInTheDocument();
    expect(screen.queryByText(/403 Forbidden/)).not.toBeInTheDocument();
  });
});
