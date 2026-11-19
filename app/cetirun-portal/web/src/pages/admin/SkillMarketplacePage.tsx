// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { ReactNode } from "react";
import {
  Badge,
  Button,
  MessageBar,
  MessageBarBody,
  MessageBarTitle,
  Spinner,
  Table,
  TableBody,
  TableCell,
  TableHeader,
  TableHeaderCell,
  TableRow,
  Text,
  Title2,
  tokens,
} from "@fluentui/react-components";
import { ArrowSync20Regular } from "@fluentui/react-icons";
import type { SkillMarketplaceCatalog, SkillMarketplaceSkill, SkillMarketplaceTeamPublication } from "../../api/skillMarketplace";
import { useSkillMarketplace } from "../../hooks/useSkillMarketplace";
import { formatSkillSize, formatSkillTime } from "../../utils/skillMarketplace";

const monospace = { fontFamily: tokens.fontFamilyMonospace, overflowWrap: "anywhere" } as const;

export type PublicationState = "upToDate" | "updating" | "pending";

const PUBLICATION_STATES: Record<PublicationState, { label: string; color: "success" | "informative" | "warning" }> = {
  upToDate: { label: "Up to date", color: "success" },
  updating: { label: "Update pending", color: "informative" },
  pending: { label: "Pending", color: "warning" },
};

/**
 * A Team's publication is pending until a release was published for the Team's current selection. It is updating
 * while that release lacks the repository's version of one of its skills, or a missing skill that the repository has
 * since gained. Otherwise it is up to date. skills are the valid skills of the repository.
 */
export function publicationState(team: SkillMarketplaceTeamPublication, skills: SkillMarketplaceSkill[]): PublicationState {
  if (team.releaseSelectionDigest !== team.selectionDigest) return "pending";
  const repository = new Map(skills.map((skill) => [skill.name, skill.version]));
  const behind = (team.skills ?? []).some((skill) => repository.has(skill.name) && repository.get(skill.name) !== skill.version)
    || (team.missing ?? []).some((name) => repository.has(name));
  return behind ? "updating" : "upToDate";
}

function Heading({ children }: { children: ReactNode }) {
  return <Text as="h2" weight="semibold" size={400} block style={{ margin: "0 0 8px" }}>{children}</Text>;
}

function RepositorySummary({ catalog }: { catalog: SkillMarketplaceCatalog }) {
  const entries: Array<[string, ReactNode]> = [
    ["Repository", <span style={monospace}>{catalog.repository.url || "—"}</span>],
    ["Ref", catalog.repository.ref || "Default branch"],
    ["Path", catalog.repository.path || "Repository root"],
    ["Commit", catalog.commit ? <span style={monospace}>{catalog.commit}</span> : "Not synchronized yet"],
    ["Synchronized", formatSkillTime(catalog.syncedAt)],
    ["Checked", formatSkillTime(catalog.checkedAt)],
  ];
  return <dl style={{ display: "grid", gridTemplateColumns: "minmax(120px, auto) minmax(0, 1fr)", gap: "8px 16px", margin: 0 }}>
    {entries.map(([label, value]) => <div key={label} style={{ display: "contents" }}>
      <dt style={{ color: tokens.colorNeutralForeground3 }}>{label}</dt>
      <dd style={{ margin: 0, overflowWrap: "anywhere" }}>{value}</dd>
    </div>)}
  </dl>;
}

function PublicationDetails({ team }: { team: SkillMarketplaceTeamPublication }) {
  const details = [
    team.retained?.length ? `Retained: ${team.retained.join(", ")}` : "",
    team.missing?.length ? `Missing: ${team.missing.join(", ")}` : "",
  ].filter(Boolean);
  if (details.length === 0 && !team.error) return <>—</>;
  return <div style={{ display: "flex", flexDirection: "column", gap: 2 }}>
    {details.map((detail) => <Text key={detail} size={200}>{detail}</Text>)}
    {team.error && <Text size={200} style={{ color: tokens.colorStatusDangerForeground1 }}>Error: {team.error}</Text>}
  </div>;
}

function CatalogView({ catalog }: { catalog: SkillMarketplaceCatalog }) {
  const skills = catalog.skills ?? [];
  const invalid = catalog.invalid ?? [];
  const teams = catalog.teams ?? [];
  return <>
    {catalog.error && <MessageBar intent="error">
      <MessageBarBody><MessageBarTitle>The last check of the repository failed.</MessageBarTitle>{catalog.error}</MessageBarBody>
    </MessageBar>}
    {catalog.truncated && <MessageBar intent="warning">
      <MessageBarBody>Some lists were shortened to fit the catalog.</MessageBarBody>
    </MessageBar>}

    <section aria-label="Repository">
      <Heading>Repository</Heading>
      <RepositorySummary catalog={catalog} />
    </section>

    <section aria-label="Skills">
      <Heading>Skills ({skills.length})</Heading>
      {skills.length === 0 ? <Text>The repository has no valid skills.</Text> : <Table aria-label="Skills in the repository" size="small">
        <TableHeader>
          <TableRow>
            <TableHeaderCell>Name</TableHeaderCell>
            <TableHeaderCell>Description</TableHeaderCell>
            <TableHeaderCell>Version</TableHeaderCell>
            <TableHeaderCell>Updated</TableHeaderCell>
            <TableHeaderCell>Files</TableHeaderCell>
            <TableHeaderCell>Size</TableHeaderCell>
          </TableRow>
        </TableHeader>
        <TableBody>
          {skills.map((skill) => <TableRow key={skill.name}>
            <TableCell>
              <div style={{ display: "flex", flexDirection: "column", overflowWrap: "anywhere" }}>
                <Text weight="semibold">{skill.name}</Text>
                <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>{skill.path}</Text>
              </div>
            </TableCell>
            <TableCell>{skill.description}</TableCell>
            <TableCell><span style={monospace}>{skill.version}</span></TableCell>
            <TableCell>{formatSkillTime(skill.updatedAt)}</TableCell>
            <TableCell>{skill.files}</TableCell>
            <TableCell>{formatSkillSize(skill.bytes)}</TableCell>
          </TableRow>)}
        </TableBody>
      </Table>}
    </section>

    {invalid.length > 0 && <section aria-label="Invalid skills">
      <Heading>Invalid skills ({invalid.length})</Heading>
      <Text size={200} block style={{ marginBottom: 8 }}>Teams cannot choose these skill directories until they are fixed in the repository.</Text>
      <Table aria-label="Invalid skill directories" size="small">
        <TableHeader>
          <TableRow>
            <TableHeaderCell>Path</TableHeaderCell>
            <TableHeaderCell>Reason</TableHeaderCell>
          </TableRow>
        </TableHeader>
        <TableBody>
          {invalid.map((problem) => <TableRow key={problem.path}>
            <TableCell><span style={monospace}>{problem.path}</span></TableCell>
            <TableCell>{problem.reason}</TableCell>
          </TableRow>)}
        </TableBody>
      </Table>
    </section>}

    <section aria-label="Team publications">
      <Heading>Team publications</Heading>
      <Text size={200} block style={{ marginBottom: 8 }}>
        Each policy-bound Team's selection is published as a release. Its Agents copy the current release when they start.
      </Text>
      {teams.length === 0 ? <Text>No Team has a publication yet.</Text> : <Table aria-label="Team publications" size="small">
        <TableHeader>
          <TableRow>
            <TableHeaderCell>Team</TableHeaderCell>
            <TableHeaderCell>Release</TableHeaderCell>
            <TableHeaderCell>Published</TableHeaderCell>
            <TableHeaderCell>Skills</TableHeaderCell>
            <TableHeaderCell>State</TableHeaderCell>
            <TableHeaderCell>Details</TableHeaderCell>
          </TableRow>
        </TableHeader>
        <TableBody>
          {teams.map((team) => {
            const state = PUBLICATION_STATES[publicationState(team, skills)];
            return <TableRow key={team.uid || team.name}>
              <TableCell>{team.name}</TableCell>
              <TableCell>{team.release ? <span style={monospace}>{team.release}</span> : "—"}</TableCell>
              <TableCell>{formatSkillTime(team.publishedAt)}</TableCell>
              <TableCell>{team.skills?.length ?? 0}</TableCell>
              <TableCell>
                <Badge appearance="filled" color={state.color}>{state.label}</Badge>
              </TableCell>
              <TableCell><PublicationDetails team={team} /></TableCell>
            </TableRow>;
          })}
        </TableBody>
      </Table>}
    </section>
  </>;
}

/** The Skill Marketplace: its repository, its skills, and the publication of each Team's selection. */
export function SkillMarketplacePage() {
  const { data, isLoading, isFetching, error, refetch } = useSkillMarketplace();
  const catalog = data?.catalog;
  return (
    <div style={{ maxWidth: 1100, display: "flex", flexDirection: "column", gap: 16 }}>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12 }}>
        <Title2>Skills</Title2>
        <Button icon={<ArrowSync20Regular />} disabled={isFetching} onClick={() => void refetch()}>Refresh</Button>
      </div>
      <Text>
        Teams choose their skills from one Git repository. The Skill Marketplace checks the repository regularly and publishes
        each Team's selection for its Agents.
      </Text>
      {isLoading && <Spinner label="Loading the Skill Marketplace..." />}
      {error && <MessageBar intent="error"><MessageBarBody>Failed to load: {error.message}</MessageBarBody></MessageBar>}
      {data && !data.enabled && <MessageBar intent="info">
        <MessageBarBody>The Skill Marketplace is not enabled in this installation.</MessageBarBody>
      </MessageBar>}
      {data?.enabled && !catalog && <MessageBar intent="info">
        <MessageBarBody>The Skill Marketplace has not published its catalog yet.</MessageBarBody>
      </MessageBar>}
      {catalog && <CatalogView catalog={catalog} />}
    </div>
  );
}
