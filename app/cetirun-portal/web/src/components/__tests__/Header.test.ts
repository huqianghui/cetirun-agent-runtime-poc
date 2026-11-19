// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, it, expect } from "vitest";
import { buildSignOutUrl } from "../Header";

function loc(overrides: Partial<Parameters<typeof buildSignOutUrl>[0]> = {}) {
  return {
    protocol: "https:",
    host: "example.com",
    pathname: "/",
    search: "",
    hash: "",
    ...overrides,
  };
}

describe("buildSignOutUrl", () => {
  it("uses same-origin sign-out for the apex host", () => {
    const url = buildSignOutUrl(loc());
    expect(url).toBe(
      "/oauth2/sign_out?rd=https%3A%2F%2Fexample.com%2F",
    );
  });

  it("preserves path, search, and hash in the return-to URL", () => {
    const url = buildSignOutUrl(
      loc({ pathname: "/instances", search: "?tab=all", hash: "#top" }),
    );
    expect(url).toContain(
      encodeURIComponent("https://example.com/instances?tab=all#top"),
    );
  });

  it("uses relative /oauth2/sign_out for localhost", () => {
    const url = buildSignOutUrl(
      loc({ protocol: "http:", host: "localhost:5173" }),
    );
    expect(url).toMatch(/^\/oauth2\/sign_out\?rd=/);
    expect(url).toContain(encodeURIComponent("http://localhost:5173/"));
  });

  it("uses relative /oauth2/sign_out for bare hostname (no dots)", () => {
    const url = buildSignOutUrl(loc({ host: "devbox" }));
    expect(url).toMatch(/^\/oauth2\/sign_out\?rd=/);
  });

  it("uses relative /oauth2/sign_out for IPv4 addresses", () => {
    const url = buildSignOutUrl(
      loc({ protocol: "http:", host: "127.0.0.1:5173" }),
    );
    expect(url).toMatch(/^\/oauth2\/sign_out\?rd=/);
    expect(url).toContain(encodeURIComponent("http://127.0.0.1:5173/"));
  });

  it("uses relative /oauth2/sign_out for IPv6 addresses", () => {
    const url = buildSignOutUrl(
      loc({ protocol: "http:", host: "[::1]:5173" }),
    );
    expect(url).toMatch(/^\/oauth2\/sign_out\?rd=/);
  });

  it("uses same-origin sign-out for subdomains", () => {
    const url = buildSignOutUrl(
      loc({ host: "alice.example.com" }),
    );
    expect(url).toBe(
      "/oauth2/sign_out?rd=https%3A%2F%2Falice.example.com%2F",
    );
  });

  it("preserves the return-to port", () => {
    const url = buildSignOutUrl(loc({ host: "portal.example.com:8443" }));
    expect(url).toBe(
      "/oauth2/sign_out?rd=https%3A%2F%2Fportal.example.com%3A8443%2F",
    );
  });
});
