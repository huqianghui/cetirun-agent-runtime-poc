// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, expect, it } from "vitest";
import { validCIDR } from "./InternalNetworkRules";

describe("validCIDR", () => {
  it.each([
    "10.20.0.0/24",
    "2001:db8::/32",
    "2001:db8::192.0.2.1/128",
    "2001:db8:0:0:0:0:192.0.2.1/128",
  ])("accepts %s", (cidr) => {
    expect(validCIDR(cidr)).toBe(true);
  });

  it.each([
    "10.20.0.01/24",
    "192.0.2.1/24\n",
    "10.20.0.0/33",
    "2001:db8::g/64",
    "2001:db8::192.0.2.999/128",
    "2001:db8::192.0.2.1:1/128",
    "192.0.2.1::/128",
    "2001:db8::192.0.2.1/01",
    "2001:db8::192.0.2.1/129",
    "2001:db8::1\n/128",
    "::ffff:192.0.2.1/128",
    "0:0:0:0:0:ffff:c000:201/128",
  ])("rejects %s", (cidr) => {
    expect(validCIDR(cidr)).toBe(false);
  });
});
