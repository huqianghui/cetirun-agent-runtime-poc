// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useId, useState } from "react";
import { Checkbox, Field, SearchBox, Text, tokens } from "@fluentui/react-components";
import { formatSkillTime, shortSkillVersion } from "../../utils/skillMarketplace";

/** One skill a checklist offers: a catalog skill, or a chosen name the catalog does not have. */
export interface SkillChoice {
  name: string;
  description?: string;
  version?: string;
  updatedAt?: string;
  /** The catalog does not have the name; it is listed so it can be removed. */
  unavailable?: boolean;
  checked: boolean;
  disabled?: boolean;
  /** A short remark shown under the skill, such as why it cannot be chosen. */
  note?: string;
}

/** The skills whose name or description contains every word of the search, ignoring case. */
export function filterSkills<T extends { name: string; description?: string }>(skills: T[], search: string): T[] {
  const words = search.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return skills;
  return skills.filter((skill) => {
    const text = `${skill.name}\n${skill.description ?? ""}`.toLowerCase();
    return words.every((word) => text.includes(word));
  });
}

/**
 * A searchable list of skill checkboxes. Each checkbox is named by its skill
 * and described by the skill's description, version and remark.
 */
export function SkillChecklist({ label, choices, emptyText, onToggle }: {
  /** Names the list for assistive technology. */
  label: string;
  choices: SkillChoice[];
  /** Shown when there is nothing to choose. */
  emptyText: string;
  onToggle: (name: string, checked: boolean) => void;
}) {
  const id = useId();
  const [search, setSearch] = useState("");
  const shown = filterSkills(choices, search);
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: tokens.spacingVerticalS }}>
      {choices.length > 0 && (
        <Field label="Search skills">
          <SearchBox value={search} onChange={(_, data) => setSearch(data.value)} placeholder="Name or description" />
        </Field>
      )}
      <div role="group" aria-label={label}
        style={{ display: "flex", flexDirection: "column", gap: tokens.spacingVerticalXS, maxHeight: 420, overflowY: "auto" }}>
        {shown.map((choice, index) => {
          const details = [
            choice.version ? `Version ${shortSkillVersion(choice.version)}` : "",
            choice.updatedAt ? `updated ${formatSkillTime(choice.updatedAt)}` : "",
          ].filter(Boolean).join(" · ");
          const described = !!(choice.description || details || choice.note);
          const detailsId = `${id}-skill-${index}`;
          return (
            <div key={choice.name} style={{ display: "flex", flexDirection: "column" }}>
              <Checkbox
                label={choice.unavailable ? `${choice.name} (unavailable)` : choice.name}
                checked={choice.checked}
                disabled={choice.disabled}
                aria-describedby={described ? detailsId : undefined}
                onChange={(_, data) => onToggle(choice.name, data.checked === true)}
              />
              {described && (
                // Aligned with the checkbox label.
                <div id={detailsId} style={{ display: "flex", flexDirection: "column", paddingLeft: 32, overflowWrap: "anywhere" }}>
                  {choice.description && <Text size={200}>{choice.description}</Text>}
                  {details && <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>{details}</Text>}
                  {choice.note && <Text size={200} style={{ color: tokens.colorStatusWarningForeground1 }}>{choice.note}</Text>}
                </div>
              )}
            </div>
          );
        })}
      </div>
      {shown.length === 0 && <Text size={200}>{choices.length === 0 ? emptyText : "No skills match the search."}</Text>}
    </div>
  );
}
