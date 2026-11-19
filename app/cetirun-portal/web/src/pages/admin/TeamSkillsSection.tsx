// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { MessageBar, MessageBarBody, Text } from "@fluentui/react-components";
import type { SkillMarketplaceCatalog } from "../../api/skillMarketplace";
import { NPM_SKILL_PREFIX, validSkillMarketplaceName } from "../../utils/skillMarketplace";
import { SkillChecklist, type SkillChoice } from "./SkillChecklist";

export { filterSkills } from "./SkillChecklist";

export const NOT_ALLOWED_BY_POLICY = "Not allowed by the Agent Policy";

export const TEAM_SKILLS_RESTART_NOTE =
  "Saving changes to Team skills restarts the Team's running Agents once. Changes in the skills repository take effect at an Agent's next start.";

const NOT_A_SKILL_NAME = "Not a Skill Marketplace skill name. Remove it, or add it as an npm package with the npm: prefix.";

/** A Team's skills split into Skill Marketplace names and npm: entries, each in stored order. */
export function splitTeamSkills(skills: string[] | undefined): { marketplace: string[]; npm: string[] } {
  const marketplace: string[] = [];
  const npm: string[] = [];
  for (const entry of skills ?? []) (entry.startsWith(NPM_SKILL_PREFIX) ? npm : marketplace).push(entry);
  return { marketplace, npm };
}

/** The Team's skills as the Team API takes them: marketplace names and npm entries, trimmed, sorted and unique. */
export function mergeSkillSelection(marketplace: string[], npm: string[]): string[] {
  return [...new Set([...marketplace, ...npm].map((entry) => entry.trim()).filter(Boolean))].sort();
}

/** The selection with one name added or removed; the other names keep their order. */
export function toggleSkillSelection(selection: string[], name: string, checked: boolean): string[] {
  const others = selection.filter((entry) => entry !== name);
  return checked ? [...others, name] : others;
}

/** Why the entries of the npm skill packages field cannot be saved, or null. */
export function npmSkillsProblem(entries: string[]): string | null {
  const wrong = entries.filter((entry) => !entry.startsWith(NPM_SKILL_PREFIX) || entry.length === NPM_SKILL_PREFIX.length);
  if (wrong.length === 0) return null;
  return `Enter each npm skill package as npm:<package>, for example npm:@scope/skill-a. Check: ${wrong.join(", ")}.`;
}

/**
 * The checklist of a Team's marketplace skills: first the chosen names the
 * catalog does not have, so they can be removed, then the catalog's skills.
 * Without a catalog the chosen names are listed as they are. A skill the
 * policy does not allow cannot be added; a chosen one stays so it can be removed.
 */
export function teamSkillChoices(
  catalog: SkillMarketplaceCatalog | null | undefined,
  selection: string[],
  allowed?: string[],
): SkillChoice[] {
  const permitted = (name: string) => allowed === undefined || allowed.includes(name);
  const skills = catalog?.skills ?? [];
  const inCatalog = new Set(skills.map((skill) => skill.name));
  const others = [...new Set(selection)].filter((name) => !inCatalog.has(name)).sort().map((name): SkillChoice => ({
    name,
    unavailable: !!catalog,
    checked: true,
    note: !validSkillMarketplaceName(name) ? NOT_A_SKILL_NAME : permitted(name) ? undefined : NOT_ALLOWED_BY_POLICY,
  }));
  return [...others, ...skills.map((skill): SkillChoice => {
    const checked = selection.includes(skill.name);
    return {
      name: skill.name,
      description: skill.description,
      version: skill.version,
      updatedAt: skill.updatedAt,
      checked,
      disabled: !checked && !permitted(skill.name),
      note: permitted(skill.name) ? undefined : NOT_ALLOWED_BY_POLICY,
    };
  })];
}

/**
 * The Team's Skill Marketplace skills, chosen from the marketplace catalog.
 * npm packages are not part of it; the Team editor keeps them separately.
 */
export function TeamSkillsSection({ value, onChange, allowed, catalog }: {
  /** The Team's Skill Marketplace skill names, without npm: entries. */
  value: string[];
  onChange: (names: string[]) => void;
  /** The selected Agent Policy's allowed skills; undefined while no policy is selected. */
  allowed?: string[];
  /** The marketplace catalog; null or undefined while it is not known. */
  catalog?: SkillMarketplaceCatalog | null;
}) {
  const count = new Set(value).size;
  return (
    <section aria-label="Team skills" style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      <Text size={400} weight="semibold">Skills</Text>
      <Text size={300}>
        Choose the skills the Team's Agents get from the Skill Marketplace. The Team's Agent Policy must allow each skill.
      </Text>
      <Text size={200}>{TEAM_SKILLS_RESTART_NOTE}</Text>
      {!catalog && (
        <MessageBar intent="info">
          <MessageBarBody>The skill catalog is not available, so no skill can be added now. Selected skills are kept.</MessageBarBody>
        </MessageBar>
      )}
      <SkillChecklist
        label="Marketplace skills"
        choices={teamSkillChoices(catalog, value, allowed)}
        emptyText={catalog ? "The skills repository has no valid skills yet." : "No skills selected."}
        onToggle={(name, checked) => onChange(toggleSkillSelection(value, name, checked))}
      />
      <Text size={200}>{count === 1 ? "1 skill selected." : `${count} skills selected.`}</Text>
    </section>
  );
}
