// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useState } from "react";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { SkillMarketplaceCatalog } from "../../api/skillMarketplace";
import {
  filterSkills,
  mergeSkillSelection,
  npmSkillsProblem,
  splitTeamSkills,
  TEAM_SKILLS_RESTART_NOTE,
  TeamSkillsSection,
  toggleSkillSelection,
} from "./TeamSkillsSection";

const catalog: SkillMarketplaceCatalog = {
  version: "v1",
  repository: { url: "https://git.example.com/org/skills.git" },
  commit: "0123456789abcdef0123456789abcdef01234567",
  skills: [
    { name: "code-review", description: "Reviews pull requests for defects.", version: "aaaaaaaaaaaa", path: "skills/code-review", files: 3, bytes: 2048 },
    { name: "pdf-tools", description: "Reads and fills PDF forms.", version: "bbbbbbbbbbbb", path: "skills/pdf-tools", files: 5, bytes: 4096 },
    {
      name: "web-search", description: "Searches the web through the approved proxy.", version: "cccccccccccc", path: "skills/web-search",
      files: 1, bytes: 512, updatedAt: "2026-11-01T10:00:00Z",
    },
  ],
};

/** The section with its selection kept like the Team editor keeps it. */
function Harness({ initial, allowed, catalog: shown = catalog, onChange }: {
  initial: string[];
  allowed?: string[];
  catalog?: SkillMarketplaceCatalog | null;
  onChange?: (names: string[]) => void;
}) {
  const [value, setValue] = useState(initial);
  return <TeamSkillsSection value={value} allowed={allowed} catalog={shown} onChange={(names) => { setValue(names); onChange?.(names); }} />;
}

describe("Team skill helpers", () => {
  it("splits, merges and toggles Team skills the way the Team API stores them", () => {
    expect(splitTeamSkills(["pdf-tools", "npm:@scope/tool", "web-search"])).toEqual({ marketplace: ["pdf-tools", "web-search"], npm: ["npm:@scope/tool"] });
    expect(splitTeamSkills(undefined)).toEqual({ marketplace: [], npm: [] });
    expect(mergeSkillSelection(["web-search", "pdf-tools", "pdf-tools"], [" npm:@scope/tool ", "", "npm:@scope/tool"]))
      .toEqual(["npm:@scope/tool", "pdf-tools", "web-search"]);
    expect(toggleSkillSelection(["pdf-tools"], "web-search", true)).toEqual(["pdf-tools", "web-search"]);
    expect(toggleSkillSelection(["pdf-tools", "web-search"], "pdf-tools", false)).toEqual(["web-search"]);
    expect(toggleSkillSelection(["pdf-tools"], "pdf-tools", true)).toEqual(["pdf-tools"]);
  });

  it("accepts only npm: entries in the npm skill packages", () => {
    expect(npmSkillsProblem([])).toBeNull();
    expect(npmSkillsProblem(["npm:@scope/tool", "npm:left-pad"])).toBeNull();
    expect(npmSkillsProblem(["@scope/tool", "npm:", "npm:ok"])).toBe(
      "Enter each npm skill package as npm:<package>, for example npm:@scope/skill-a. Check: @scope/tool, npm:.",
    );
  });

  it("filters by every word of the search in a skill's name or description, ignoring case", () => {
    const skills = catalog.skills!;
    expect(filterSkills(skills, "  ")).toBe(skills);
    expect(filterSkills(skills, "PDF").map((skill) => skill.name)).toEqual(["pdf-tools"]);
    expect(filterSkills(skills, " the  WEB ").map((skill) => skill.name)).toEqual(["web-search"]);
    expect(filterSkills(skills, "review proxy")).toEqual([]);
  });
});

describe("TeamSkillsSection", () => {
  it("searches the catalog and toggles skills", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(<Harness initial={["pdf-tools"]} onChange={onChange} />);
    expect(screen.getByText(TEAM_SKILLS_RESTART_NOTE)).toBeInTheDocument();
    const list = within(screen.getByRole("group", { name: "Marketplace skills" }));
    expect(list.getAllByRole("checkbox")).toHaveLength(3);
    expect(list.getByRole("checkbox", { name: "pdf-tools" })).toBeChecked();
    // Without a policy every skill can be chosen.
    const web = list.getByRole("checkbox", { name: "web-search" });
    expect(web).toBeEnabled();
    expect(web).not.toBeChecked();
    expect(web).toHaveAccessibleDescription(/Searches the web through the approved proxy\..*Version ccccccc · updated/);

    const search = screen.getByRole("searchbox", { name: "Search skills" });
    await user.type(search, "pdf");
    expect(list.getAllByRole("checkbox")).toHaveLength(1);
    await user.click(list.getByRole("checkbox", { name: "pdf-tools" }));
    expect(onChange).toHaveBeenLastCalledWith([]);
    expect(screen.getByText("0 skills selected.")).toBeInTheDocument();

    await user.clear(search);
    await user.click(list.getByRole("checkbox", { name: "web-search" }));
    expect(onChange).toHaveBeenLastCalledWith(["web-search"]);
    expect(list.getByRole("checkbox", { name: "web-search" })).toBeChecked();
    expect(screen.getByText("1 skill selected.")).toBeInTheDocument();

    await user.type(search, "spreadsheet");
    expect(list.queryAllByRole("checkbox")).toHaveLength(0);
    expect(screen.getByText("No skills match the search.")).toBeInTheDocument();
  });

  it("lists chosen skills the catalog does not have, so they can be removed", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(<Harness initial={["retired-skill", "pdf-tools", "@anthropic/skill-foo"]} onChange={onChange} />);
    const retired = screen.getByRole("checkbox", { name: "retired-skill (unavailable)" });
    expect(retired).toBeChecked();
    expect(retired).toBeEnabled();
    // The Team API refuses an entry that is neither an npm package nor a marketplace skill name.
    expect(screen.getByRole("checkbox", { name: "@anthropic/skill-foo (unavailable)" }))
      .toHaveAccessibleDescription(/Not a Skill Marketplace skill name/);
    expect(screen.getByText("3 skills selected.")).toBeInTheDocument();

    await user.click(retired);
    expect(onChange).toHaveBeenLastCalledWith(["pdf-tools", "@anthropic/skill-foo"]);
    expect(screen.queryByRole("checkbox", { name: /retired-skill/ })).not.toBeInTheDocument();
  });

  it("disables skills the Agent Policy does not allow but keeps chosen ones removable", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(<Harness initial={["code-review"]} allowed={["pdf-tools"]} onChange={onChange} />);
    const web = screen.getByRole("checkbox", { name: "web-search" });
    expect(web).toBeDisabled();
    expect(web).toHaveAccessibleDescription(/Not allowed by the Agent Policy/);
    const pdf = screen.getByRole("checkbox", { name: "pdf-tools" });
    expect(pdf).toBeEnabled();
    expect(pdf).not.toHaveAccessibleDescription(/Not allowed/);

    const review = screen.getByRole("checkbox", { name: "code-review" });
    expect(review).toBeChecked();
    expect(review).toBeEnabled();
    expect(review).toHaveAccessibleDescription(/Not allowed by the Agent Policy/);
    await user.click(review);
    expect(onChange).toHaveBeenLastCalledWith([]);
    // Once removed, it cannot be added again under this policy.
    expect(screen.getByRole("checkbox", { name: "code-review" })).toBeDisabled();
  });

  it("allows no skill under a policy that allows none", () => {
    render(<Harness initial={[]} allowed={[]} />);
    for (const name of ["code-review", "pdf-tools", "web-search"]) {
      expect(screen.getByRole("checkbox", { name })).toBeDisabled();
    }
  });

  it("keeps chosen skills removable while the catalog is not known", async () => {
    const user = userEvent.setup();
    render(<Harness initial={["pdf-tools"]} catalog={null} />);
    expect(screen.getByText(/The skill catalog is not available/)).toBeInTheDocument();
    // Without a catalog nothing says whether a name is in it.
    const chosen = screen.getByRole("checkbox", { name: "pdf-tools" });
    expect(chosen).toBeChecked();
    await user.click(chosen);
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    expect(screen.getByText("No skills selected.")).toBeInTheDocument();
  });

  it("says when the repository has no valid skill", () => {
    render(<Harness initial={[]} catalog={{ ...catalog, skills: [] }} />);
    expect(screen.getByText("The skills repository has no valid skills yet.")).toBeInTheDocument();
    expect(screen.queryByRole("searchbox")).not.toBeInTheDocument();
  });
});
