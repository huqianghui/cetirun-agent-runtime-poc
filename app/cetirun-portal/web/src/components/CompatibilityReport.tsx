// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useState } from "react";
import { Badge, Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow, Text } from "@fluentui/react-components";
import { Link } from "react-router-dom";
import type { SandboxCompatibilityStatus, TeamCompatibilityStatus, TeamDTO } from "../api/adminTeam";

export type CompatibilityPresentationState = "Compatible" | "Incompatible" | "Unknown" | "Pending" | "Not configured";

export interface CompatibilityPresentation {
  state: CompatibilityPresentationState;
  reason?: string;
  report?: TeamCompatibilityStatus;
}

export type CompatibilityDependency =
  | { kind: "policy"; name: string; uid: string; generation: number; deleting?: boolean }
  | { kind: "sandbox"; name: string; uid: string; generation: number; deleting?: boolean };

function timestamp(value: string | undefined): number | undefined {
  if (!value) return undefined;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function matchesDependency(reference: { name: string; uid: string; generation: number }, dependency: CompatibilityDependency) {
  return reference.name === dependency.name && reference.uid === dependency.uid && reference.generation === dependency.generation;
}

function currentReport(team: TeamDTO, now: number, dependency?: CompatibilityDependency): TeamCompatibilityStatus | undefined {
  const report = team.compatibility;
  if (team.deleting || !report || report.team.name !== team.name || report.team.uid !== team.uid || report.team.generation !== team.generation) return undefined;
  const reportSandboxes = report.sandboxes ?? [];
  if (report.policy.name !== team.agentPolicyRef || reportSandboxes.length !== team.sandboxRefs.length ||
      reportSandboxes.some(({ sandbox }) => !team.sandboxRefs.includes(sandbox.name))) return undefined;
  const checkedAt = timestamp(report.checkedAt);
  const validUntil = timestamp(report.validUntil);
  if (!checkedAt || checkedAt > now || !validUntil || validUntil <= now || validUntil <= checkedAt || validUntil - checkedAt > 30_000) return undefined;
  if (dependency?.deleting) return undefined;
  if (dependency?.kind === "policy" && !matchesDependency(report.policy, dependency)) return undefined;
  if (dependency?.kind === "sandbox" && !reportSandboxes.some(({ sandbox }) => matchesDependency(sandbox, dependency))) return undefined;
  return report;
}

export function compatibilityPresentation(team: TeamDTO, now = Date.now(), dependency?: CompatibilityDependency): CompatibilityPresentation {
  if (!team.agentPolicyRef && team.sandboxRefs.length === 0 && !team.defaultSandboxRef) return { state: "Not configured" };
  if (!team.compatibility) return { state: "Pending", reason: "Compatibility is pending." };
  const report = currentReport(team, now, dependency);
  if (!report) {
    const validUntil = timestamp(team.compatibility?.validUntil);
    return { state: "Unknown", reason: validUntil && validUntil <= now ? "The compatibility report has expired." : "The saved compatibility report no longer matches this configuration." };
  }
  return { state: report.state, reason: report.message || report.reason, report };
}

function useCompatibilityNow(validUntil?: string) {
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const expiresAt = timestamp(validUntil);
    if (!expiresAt || expiresAt <= Date.now()) return undefined;
    // Browsers cap a timeout at a signed 32-bit delay. Re-arm long-lived
    // reports so they still become stale at their exact expiry.
    const delay = Math.min(2_147_483_647, Math.max(0, expiresAt - Date.now()) + 1);
    const timer = window.setTimeout(() => setTick((current) => current + 1), delay);
    return () => window.clearTimeout(timer);
  }, [validUntil, tick]);
  return Date.now();
}

function color(state: CompatibilityPresentationState): "success" | "danger" | "warning" | "informative" | "subtle" {
  switch (state) {
    case "Compatible": return "success";
    case "Incompatible": return "danger";
    case "Unknown": return "warning";
    case "Pending": return "informative";
    default: return "subtle";
  }
}

export function CompatibilityBadge({ team }: { team: TeamDTO }) {
  const now = useCompatibilityNow(team.compatibility?.validUntil);
  const presentation = compatibilityPresentation(team, now);
  return <Badge appearance="filled" color={color(presentation.state)}>{presentation.state}</Badge>;
}

function SandboxIssues({ status }: { status: SandboxCompatibilityStatus }) {
  return <>
    {status.reason && <Text size={200}>{status.reason}</Text>}
    {status.issues?.map((issue, index) => <div key={`${issue.code}-${issue.field}-${index}`}><Text size={200}>{issue.message}</Text></div>)}
  </>;
}

export function CompatibilityReport({ team, sandboxName, dependency }: { team: TeamDTO; sandboxName?: string; dependency?: CompatibilityDependency }) {
  const now = useCompatibilityNow(team.compatibility?.validUntil);
  const presentation = compatibilityPresentation(team, now, dependency);
  const report = presentation.report;
  const sandboxes = report?.sandboxes ?? [];
  const displayedSandboxes = sandboxName ? sandboxes.filter(({ sandbox }) => sandbox.name === sandboxName) : sandboxes;
  const topIssues = report?.issues?.filter((issue) => !issue.sandbox);
  return <div aria-label={`Compatibility report for ${team.name}`} style={{ display: "flex", flexDirection: "column", gap: 4 }}>
    <Text size={200}>
      {sandboxName && team.sandboxRefs.length > 1 ? "Team compatibility" : "Compatibility"}:{" "}
      <Badge appearance="filled" color={color(presentation.state)}>{presentation.state}</Badge>
    </Text>
    <Text size={200}>Policy: {team.agentPolicyRef
      ? <Link to={`/admin/policies/${encodeURIComponent(team.agentPolicyRef)}/edit`}>{team.agentPolicyRef}</Link>
      : "Not configured"}</Text>
    {presentation.reason && <Text size={200}>{presentation.reason}</Text>}
    {sandboxes.length === 1 ? displayedSandboxes.map((status) => <div key={status.sandbox.uid || status.sandbox.name}>
      <Text size={200}>Sandbox: <Link to={`/admin/sandboxes/${encodeURIComponent(status.sandbox.name)}/edit`}>{status.sandbox.name}</Link></Text>
      <div><SandboxIssues status={status} /></div>
    </div>) : displayedSandboxes.length > 0 && <Table size="small" aria-label={`Sandbox compatibility with Policy ${team.agentPolicyRef}`}>
      <TableHeader><TableRow>
        <TableHeaderCell>Sandbox</TableHeaderCell>
        <TableHeaderCell>Compatibility</TableHeaderCell>
        <TableHeaderCell>Reason</TableHeaderCell>
      </TableRow></TableHeader>
      <TableBody>{displayedSandboxes.map((status) => <TableRow key={status.sandbox.uid || status.sandbox.name}>
        <TableCell><Link to={`/admin/sandboxes/${encodeURIComponent(status.sandbox.name)}/edit`}>{status.sandbox.name}</Link></TableCell>
        <TableCell><Badge appearance="filled" color={color(status.state)}>{status.state}</Badge></TableCell>
        <TableCell>{status.reason || status.issues?.length ? <SandboxIssues status={status} /> : "—"}</TableCell>
      </TableRow>)}</TableBody>
    </Table>}
    {topIssues?.map((issue, index) =>
      <Text key={`${issue.code}-${issue.field}-${index}`} size={200}>{issue.message}</Text>)}
    {report && <Text size={200}>Checked: {new Date(report.checkedAt).toLocaleString()}</Text>}
  </div>;
}

export function CompatibilityReferences({ teams, sandboxName, dependency }: { teams: TeamDTO[]; sandboxName?: string; dependency?: CompatibilityDependency }) {
  if (teams.length === 0) return <Text size={200}>No Teams reference this configuration.</Text>;
  return <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
    {teams.map((team) => <div key={team.name}>
      <Text weight="semibold"><Link to={`/admin/teams/${encodeURIComponent(team.name)}/edit`}>{team.name}</Link></Text>
      <CompatibilityReport team={team} sandboxName={sandboxName} dependency={dependency} />
    </div>)}
  </div>;
}
