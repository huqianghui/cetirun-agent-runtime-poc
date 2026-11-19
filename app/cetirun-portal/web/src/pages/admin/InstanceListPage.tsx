// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useMemo, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  Title2,
  Spinner,
  MessageBar,
  Table,
  TableHeader,
  TableHeaderCell,
  TableRow,
  TableBody,
  TableCell,
  Badge,
  Text,
  Input,
  Field,
  Select,
  tokens,
} from "@fluentui/react-components";
import { listAllInstances, type AdminInstanceSummary } from "../../api/adminInstance";
import { agentTypeLabel } from "../../utils/instance";
import { GovernanceNotice } from "../../components/GovernanceNotice";
import { useMe } from "../../context/MeContext";
import type { Me } from "../../api/me";

// Ingress and the Portal API let only an Agent's owner into its workspace, so
// the list links administrators only to the Agents they own.
function ownsAgent(me: Me | undefined, instance: AdminInstanceSummary): boolean {
  if (!me) return false;
  const sameAlias = !!me.alias && instance.alias.toLowerCase() === me.alias.toLowerCase();
  const sameEmail = !!me.email && !!instance.email && instance.email.toLowerCase() === me.email.toLowerCase();
  return sameAlias || sameEmail;
}

function formatAge(createdAt?: string): string {
  if (!createdAt) return "—";
  const created = new Date(createdAt).getTime();
  if (Number.isNaN(created)) return "—";
  const seconds = Math.max(0, Math.floor((Date.now() - created) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.floor(hours / 24);
  return `${days}d`;
}

export function InstanceListPage() {
  const location = useLocation();
  const { me } = useMe();
  const [nameFilter, setNameFilter] = useState("");
  const [ownerFilter, setOwnerFilter] = useState("");
  const [teamFilter, setTeamFilter] = useState("");
  const [typeFilter, setTypeFilter] = useState("");
  const [phaseFilter, setPhaseFilter] = useState("");
  const { data, isLoading, error } = useQuery<AdminInstanceSummary[], Error>({
    queryKey: ["admin", "instances"],
    queryFn: listAllInstances,
    refetchInterval: 30_000,
  });
  const agents = useMemo(() => (data || []).filter((instance) => {
    const name = nameFilter.trim().toLowerCase();
    const owner = ownerFilter.trim().toLowerCase();
    const team = teamFilter.trim().toLowerCase();
    return (!name || instance.name.toLowerCase().includes(name)) &&
      (!owner || instance.alias.toLowerCase().includes(owner) || instance.email?.toLowerCase().includes(owner)) &&
      (!team || instance.team?.toLowerCase().includes(team)) &&
      (!typeFilter || instance.agent.type === typeFilter) &&
      (!phaseFilter || instance.phase === phaseFilter);
  }), [data, nameFilter, ownerFilter, teamFilter, typeFilter, phaseFilter]);
  const types = useMemo(() => [...new Set((data || []).map((instance) => instance.agent.type))].sort(), [data]);
  const phases = useMemo(() => [...new Set((data || []).map((instance) => instance.phase).filter(Boolean))].sort(), [data]);

  return (
    <div>
      <Title2 style={{ marginBottom: 16 }}>All agents</Title2>
      {location.state?.deletedAgent && <MessageBar intent="success">Agent {location.state.deletedAgent} has been deleted.</MessageBar>}
      {location.state?.missingAgent && <MessageBar intent="info">Agent {location.state.missingAgent} no longer exists.</MessageBar>}
      {isLoading && <Spinner label="Loading agents..." />}
      {error && <MessageBar intent="error">Failed to load: {error.message}</MessageBar>}
      {data && (
        <>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(170px, 1fr))", gap: 12, marginBottom: 16 }}>
          <Field label="Name"><Input value={nameFilter} onChange={(_, value) => setNameFilter(value.value)} /></Field>
          <Field label="Owner"><Input value={ownerFilter} onChange={(_, value) => setOwnerFilter(value.value)} /></Field>
          <Field label="Team"><Input value={teamFilter} onChange={(_, value) => setTeamFilter(value.value)} /></Field>
          <Field label="Type"><Select aria-label="Type" value={typeFilter} onChange={(event) => setTypeFilter(event.target.value)}><option value="">All types</option>{types.map((type) => <option key={type} value={type}>{agentTypeLabel(type)}</option>)}</Select></Field>
          <Field label="Phase"><Select aria-label="Phase" value={phaseFilter} onChange={(event) => setPhaseFilter(event.target.value)}><option value="">All phases</option>{phases.map((phase) => <option key={phase} value={phase}>{phase}</option>)}</Select></Field>
        </div>
        <Table aria-label="All agents">
          <TableHeader>
            <TableRow>
              <TableHeaderCell>Name</TableHeaderCell>
              <TableHeaderCell>Alias</TableHeaderCell>
              <TableHeaderCell>Display name</TableHeaderCell>
              <TableHeaderCell>Team</TableHeaderCell>
              <TableHeaderCell>Agent</TableHeaderCell>
              <TableHeaderCell>Status</TableHeaderCell>
              <TableHeaderCell>Age</TableHeaderCell>
              <TableHeaderCell>Actions</TableHeaderCell>
            </TableRow>
          </TableHeader>
          <TableBody>
            {agents.length === 0 && (
              <TableRow>
                <TableCell colSpan={8}>
                  <Text>{data.length === 0 ? "No agents." : "No agents match the filters."}</Text>
                </TableCell>
              </TableRow>
            )}
            {agents.map((ins) => (
              <TableRow key={ins.name}>
                <TableCell><Link to={`/admin/instances/${encodeURIComponent(ins.name)}`}>{ins.name}</Link></TableCell>
                <TableCell>{ins.alias}</TableCell>
                <TableCell>{ins.displayName || "—"}</TableCell>
                <TableCell>{ins.team || "—"}</TableCell>
                <TableCell>
                  <Badge appearance="outline">
                    {agentTypeLabel(ins.agent.type)}
                  </Badge>
                </TableCell>
                <TableCell>
                  <Badge appearance="filled" color={ins.ready ? "success" : "warning"}>
                    {ins.phase || "Unknown"}
                  </Badge>
                  <GovernanceNotice instance={ins} compact />
                </TableCell>
                <TableCell>{formatAge(ins.createdAt)}</TableCell>
                <TableCell>
                  {ownsAgent(me, ins)
                    ? <Link to={`/instances/${encodeURIComponent(ins.name)}`}>Open agent</Link>
                    : <Text title="Only the Agent's owner can open its workspace" style={{ color: tokens.colorNeutralForegroundDisabled }}>Owner only</Text>}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        </>
      )}
    </div>
  );
}
