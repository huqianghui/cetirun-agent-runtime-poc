// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useState, type FormEvent } from "react";
import { Button, Dialog, DialogActions, DialogBody, DialogContent, DialogSurface, DialogTitle, Field, Input, MessageBar, Text, tokens } from "@fluentui/react-components";
import type { AgentPolicyCIDRRule, AgentPolicyProtocol } from "../../api/adminPolicy";

export interface InternalNetworkRule {
  cidr: string;
  ports: string;
}

function matchesEntireValue(value: string, expression: RegExp): boolean {
  return expression.exec(value)?.[0] === value;
}

export function formatInternalNetworkRules(rules: AgentPolicyCIDRRule[]): InternalNetworkRule[] {
  return rules.map((rule) => ({
    cidr: rule.cidr,
    ports: rule.ports.map(({ port, protocol }) =>
      !protocol || protocol === "TCP" ? String(port) : `${port}/${protocol}`,
    ).join(", "),
  }));
}

export function parseInternalNetworkRules(rules: InternalNetworkRule[]): AgentPolicyCIDRRule[] {
  return rules.map((rule, index) => {
    const ports = rule.ports.split(",").map((value) => {
      const match = value.trim().match(/^(\d{1,5})(?:\s*\/\s*(TCP|UDP|SCTP))?$/i);
      if (!match || Number(match[1]) < 1 || Number(match[1]) > 65535) {
        throw new Error(`Rule ${index + 1}: enter ports from 1 to 65535, separated by commas, such as 443, 5432, 53/UDP.`);
      }
      return { port: Number(match[1]), protocol: (match[2]?.toUpperCase() || "TCP") as AgentPolicyProtocol };
    });
    if (new Set(ports.map(({ port, protocol }) => `${port}/${protocol}`)).size !== ports.length) {
      throw new Error(`Rule ${index + 1}: each port and protocol must appear only once.`);
    }
    return { cidr: rule.cidr.trim(), ports };
  });
}

function parseIPv4Address(value: string): number[] | undefined {
  const octets = value.split(".");
  if (octets.length !== 4 || !octets.every((octet) => matchesEntireValue(octet, /^(0|[1-9][0-9]{0,2})$/) && Number(octet) <= 255)) return undefined;
  return octets.map(Number);
}

function validIPv6Address(value: string): boolean {
  const halves = value.split("::");
  if (halves.length > 2) return false;
  if (halves.length === 2 && halves[0].includes(".")) return false;
  const parseGroups = (half: string) => {
    if (!half) return [] as number[] | undefined;
    const groups = half.split(":");
    const values: number[] = [];
    for (let index = 0; index < groups.length; index++) {
      const group = groups[index];
      if (group.includes(".")) {
        const ipv4 = index === groups.length - 1 ? parseIPv4Address(group) : undefined;
        if (!ipv4) return undefined;
        values.push(ipv4[0] * 256 + ipv4[1], ipv4[2] * 256 + ipv4[3]);
      } else if (matchesEntireValue(group, /^[0-9a-fA-F]{1,4}$/)) {
        values.push(Number.parseInt(group, 16));
      } else {
        return undefined;
      }
    }
    return values;
  };
  const left = parseGroups(halves[0]);
  const right = parseGroups(halves[1] || "");
  if (!left || !right) return false;
  const groupCount = left.length + right.length;
  if (halves.length === 2 ? groupCount >= 8 : groupCount !== 8) return false;
  // An embedded IPv4 tail consumes two 16-bit groups; reject mapped aliases like ::ffff:192.0.2.1.
  const groups = halves.length === 2 ? [...left, ...Array(8 - groupCount).fill(0), ...right] : left;
  return !(groups.slice(0, 5).every((group) => group === 0) && groups[5] === 0xffff);
}

export function validCIDR(value: string): boolean {
  const slash = value.lastIndexOf("/");
  if (slash <= 0) return false;
  const address = value.slice(0, slash);
  const bits = value.slice(slash + 1);
  if (!matchesEntireValue(bits, /^(0|[1-9][0-9]*)$/)) return false;
  if (address.includes(":")) return validIPv6Address(address) && Number(bits) <= 128;
  return !!parseIPv4Address(address) && Number(bits) <= 32;
}

export function InternalNetworkRules({ rules, onChange, onEditingChange }: {
  rules: InternalNetworkRule[];
  onChange: (rules: InternalNetworkRule[]) => void;
  onEditingChange?: (editing: boolean) => void;
}) {
  const [editing, setEditing] = useState<{ index: number | null; rule: InternalNetworkRule } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const openEditor = (index: number | null) => {
    setError(null);
    setEditing({ index, rule: index === null ? { cidr: "", ports: "443" } : { ...rules[index] } });
    onEditingChange?.(true);
  };
  const closeEditor = () => {
    setEditing(null);
    onEditingChange?.(false);
  };
  const applyRule = (event: FormEvent) => {
    event.preventDefault();
    event.stopPropagation();
    if (!editing) return;
    if (!validCIDR(editing.rule.cidr.trim())) {
      setError("Enter a valid destination CIDR, such as 10.20.0.0/24.");
      return;
    }
    if (rules.some((rule, index) => index !== editing.index && rule.cidr.trim() === editing.rule.cidr.trim())) {
      setError("This destination already has a rule. Edit that rule to change its allowed ports.");
      return;
    }
    try {
      const [rule] = formatInternalNetworkRules(parseInternalNetworkRules([editing.rule]));
      onChange(editing.index === null ? [...rules, rule] : rules.map((current, index) => index === editing.index ? rule : current));
      closeEditor();
      setError(null);
    } catch (error) {
      setError(error instanceof Error ? error.message.replace(/^Rule 1: /, "") : "Check the network rule.");
    }
  };

  return <section aria-label="Internal network access" style={{ display: "flex", flexDirection: "column", gap: 12 }}>
    <Text weight="semibold">Internal network access</Text>
    <Text>Each rule allows access to one destination network on all the ports listed with it.</Text>
    {rules.length === 0 && <Text style={{ color: tokens.colorNeutralForeground3 }}>No additional internal access configured.</Text>}
    {rules.map((rule, index) => <div key={index} role="group" aria-label={`Internal network rule ${index + 1}`} style={{
      display: "flex", flexDirection: "column", gap: 12, minWidth: 0,
      padding: 16, border: `1px solid ${tokens.colorNeutralStroke2}`, borderRadius: tokens.borderRadiusMedium,
    }}>
      <Text weight="semibold">Rule {index + 1}</Text>
      <Text style={{ overflowWrap: "anywhere" }}>Allow access to <strong>{rule.cidr}</strong> on <strong>{rule.ports}</strong>.</Text>
      <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
        <Button type="button" style={{ minWidth: 0, maxWidth: "100%" }} onClick={() => openEditor(index)}>Edit rule</Button>
        <Button type="button" style={{ minWidth: 0, maxWidth: "100%" }} onClick={() => onChange(rules.filter((_, currentIndex) => currentIndex !== index))}>Delete rule</Button>
      </div>
    </div>)}
    <Button type="button" style={{ alignSelf: "flex-start" }} onClick={() => openEditor(null)}>Add network rule</Button>
    <Text size={200}>Ports use TCP unless another protocol is shown. Save the policy to apply rule changes.</Text>
    <Text size={200}>Requires a Sandbox runtime that supports internal network access. Native single-start does not support these rules.</Text>
    <Dialog open={editing !== null} onOpenChange={(_, data) => { if (!data.open) closeEditor(); }}>
      <DialogSurface>
        <form onSubmit={applyRule}>
          <DialogBody>
            <DialogTitle>{editing?.index === null ? "Add network rule" : "Edit network rule"}</DialogTitle>
            <DialogContent style={{ display: "flex", flexDirection: "column", gap: 16 }}>
              <Text>The destination and its allowed ports form one rule.</Text>
              <Field label="Destination (CIDR)" required>
                <Input value={editing?.rule.cidr || ""} placeholder="e.g. 10.20.0.0/24" style={{ minWidth: 0, width: "100%" }}
                  onChange={(_, data) => { if (editing) setEditing({ ...editing, rule: { ...editing.rule, cidr: data.value } }); }} />
              </Field>
              <Field label="Allowed ports" required hint="Separate ports with commas, for example 443, 5432, 53/UDP. TCP is the default; /UDP and /SCTP are also supported.">
                <Input value={editing?.rule.ports || ""} placeholder="e.g. 443, 5432, 53/UDP" style={{ minWidth: 0, width: "100%" }}
                  onChange={(_, data) => { if (editing) setEditing({ ...editing, rule: { ...editing.rule, ports: data.value } }); }} />
              </Field>
              {error && <MessageBar intent="error">{error}</MessageBar>}
            </DialogContent>
            <DialogActions>
              <Button type="button" onClick={closeEditor}>Cancel</Button>
              <Button appearance="primary" type="submit">{editing?.index === null ? "Add rule" : "Update rule"}</Button>
            </DialogActions>
          </DialogBody>
        </form>
      </DialogSurface>
    </Dialog>
  </section>;
}
