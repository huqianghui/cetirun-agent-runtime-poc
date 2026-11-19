// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect } from "vitest";
import { webLightTheme, webDarkTheme } from "@fluentui/react-components";
import { resolveTheme } from "../ThemeContext";

describe("resolveTheme", () => {
  it("returns light theme when mode is 'light' regardless of system preference", () => {
    expect(resolveTheme("light", false)).toBe(webLightTheme);
    expect(resolveTheme("light", true)).toBe(webLightTheme);
  });

  it("returns dark theme when mode is 'dark' regardless of system preference", () => {
    expect(resolveTheme("dark", false)).toBe(webDarkTheme);
    expect(resolveTheme("dark", true)).toBe(webDarkTheme);
  });

  it("returns dark theme when mode is 'system' and system prefers dark", () => {
    expect(resolveTheme("system", true)).toBe(webDarkTheme);
  });

  it("returns light theme when mode is 'system' and system prefers light", () => {
    expect(resolveTheme("system", false)).toBe(webLightTheme);
  });
});
