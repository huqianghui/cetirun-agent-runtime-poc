// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { publicationState, SkillMarketplacePage } from "./SkillMarketplacePage";
import * as api from "../../api/skillMarketplace";

vi.mock("../../api/skillMarketplace");

const marketplace: api.SkillMarketplace = {
  enabled: true,
  known: true,
  catalog: {
    version: "v1",
    repository: { url: "https://git.example.com/org/skills.git", ref: "main", path: "skills" },
    commit: "0123456789abcdef0123456789abcdef01234567",
    syncedAt: "2026-11-02T10:00:00Z",
    checkedAt: "2026-11-02T10:05:00Z",
    error: "fetch https://git.example.com/org/skills.git: connection refused",
    skills: [
      { name: "pdf-tools", description: "Reads and fills PDF forms.", version: "bbbbbbbbbbbb", path: "skills/pdf-tools", files: 5, bytes: 4096, updatedAt: "2026-11-01T09:00:00Z" },
      { name: "web-search", description: "Searches the web.", version: "cccccccccccc", path: "skills/web-search", files: 1, bytes: 512 },
    ],
    invalid: [{ path: "skills/Broken_Skill", name: "Broken_Skill", reason: "The skill name must be lowercase words joined by hyphens." }],
    teams: [
      {
        name: "research", uid: "team-uid-1", selectionDigest: "sha256:aaaa", release: "0123456789abcdef0123", releaseSelectionDigest: "sha256:aaaa",
        publishedAt: "2026-11-02T10:01:00Z", skills: [{ name: "pdf-tools", version: "bbbbbbbbbbbb" }, { name: "old-skill", version: "dddddddddddd" }],
        retained: ["old-skill"],
      },
      {
        name: "support", uid: "team-uid-2", selectionDigest: "sha256:bbbb", release: "fedcba9876543210fedc", releaseSelectionDigest: "sha256:cccc",
        skills: [{ name: "pdf-tools", version: "bbbbbbbbbbbb" }], missing: ["unknown-skill"], error: "The release could not be written.",
      },
      {
        name: "writing", uid: "team-uid-3", selectionDigest: "sha256:dddd", release: "00112233445566778899", releaseSelectionDigest: "sha256:dddd",
        skills: [{ name: "pdf-tools", version: "aaaaaaaaaaaa" }], error: "publication failed: synthetic storage error",
      },
    ],
    truncated: true,
  },
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><SkillMarketplacePage /></MemoryRouter></QueryClientProvider>);
}

describe("SkillMarketplacePage", () => {
  beforeEach(() => vi.resetAllMocks());

  it("shows the repository, its skills, the rejected directories and each Team's publication", async () => {
    vi.mocked(api.getSkillMarketplace).mockResolvedValue(marketplace);
    renderPage();
    const repository = within(await screen.findByRole("region", { name: "Repository" }));
    expect(repository.getByText("https://git.example.com/org/skills.git")).toBeInTheDocument();
    expect(repository.getByText("main")).toBeInTheDocument();
    expect(repository.getByText("skills")).toBeInTheDocument();
    expect(repository.getByText("0123456789abcdef0123456789abcdef01234567")).toBeInTheDocument();
    expect(repository.getByText(new Date("2026-11-02T10:00:00Z").toLocaleString())).toBeInTheDocument();
    expect(repository.getByText(new Date("2026-11-02T10:05:00Z").toLocaleString())).toBeInTheDocument();
    expect(screen.getByText("fetch https://git.example.com/org/skills.git: connection refused")).toBeInTheDocument();
    expect(screen.getByText("Some lists were shortened to fit the catalog.")).toBeInTheDocument();

    const skills = within(screen.getByRole("table", { name: "Skills in the repository" }));
    expect(skills.getAllByRole("columnheader").map((cell) => cell.textContent)).toEqual(["Name", "Description", "Version", "Updated", "Files", "Size"]);
    const [, pdf, web] = skills.getAllByRole("row");
    expect(within(pdf).getAllByRole("cell").map((cell) => cell.textContent)).toEqual([
      "pdf-toolsskills/pdf-tools", "Reads and fills PDF forms.", "bbbbbbbbbbbb", new Date("2026-11-01T09:00:00Z").toLocaleString(), "5", "4.0 KiB",
    ]);
    expect(within(web).getAllByRole("cell").map((cell) => cell.textContent)).toEqual([
      "web-searchskills/web-search", "Searches the web.", "cccccccccccc", "—", "1", "512 B",
    ]);

    const invalid = within(screen.getByRole("table", { name: "Invalid skill directories" }));
    expect(invalid.getByText("skills/Broken_Skill")).toBeInTheDocument();
    expect(invalid.getByText("The skill name must be lowercase words joined by hyphens.")).toBeInTheDocument();

    const teams = within(screen.getByRole("table", { name: "Team publications" }));
    const [, research, support, writing] = teams.getAllByRole("row");
    expect(within(research).getByText("research")).toBeInTheDocument();
    expect(within(research).getByText("0123456789abcdef0123")).toBeInTheDocument();
    expect(within(research).getByText(new Date("2026-11-02T10:01:00Z").toLocaleString())).toBeInTheDocument();
    expect(within(research).getByText("Up to date")).toBeInTheDocument();
    expect(within(research).getByText("Retained: old-skill")).toBeInTheDocument();
    expect(within(support).getByText("Pending")).toBeInTheDocument();
    expect(within(support).getByText("Missing: unknown-skill")).toBeInTheDocument();
    expect(within(support).getByText("Error: The release could not be written.")).toBeInTheDocument();
    // The release serves the current selection, with an earlier version of pdf-tools than the repository.
    expect(within(writing).getByText("Update pending")).toBeInTheDocument();
    expect(within(writing).getByText("Error: publication failed: synthetic storage error")).toBeInTheDocument();
  });

  it("calls a release up to date only when it serves the current selection with the repository's versions", () => {
    const skills: api.SkillMarketplaceSkill[] = [{ name: "pdf-tools", description: "", version: "bbbbbbbbbbbb", path: "skills/pdf-tools", files: 1, bytes: 1 }];
    const team: api.SkillMarketplaceTeamPublication = { name: "a", uid: "u", selectionDigest: "sha256:aaaa", releaseSelectionDigest: "sha256:aaaa" };
    expect(publicationState(team, skills)).toBe("upToDate");
    expect(publicationState({ ...team, skills: [{ name: "pdf-tools", version: "bbbbbbbbbbbb" }] }, skills)).toBe("upToDate");
    expect(publicationState({ ...team, skills: [{ name: "pdf-tools", version: "aaaaaaaaaaaa" }] }, skills)).toBe("updating");
    expect(publicationState({ ...team, missing: ["pdf-tools"] }, skills)).toBe("updating");
    // A retained skill is no longer valid in the repository, so the release keeps its last version.
    expect(publicationState({ ...team, skills: [{ name: "old-skill", version: "dddddddddddd" }], retained: ["old-skill"] }, skills)).toBe("upToDate");
    expect(publicationState({ ...team, releaseSelectionDigest: "sha256:bbbb" }, skills)).toBe("pending");
    expect(publicationState({ name: "a", uid: "u", selectionDigest: "sha256:aaaa" }, skills)).toBe("pending");
  });

  it("says when the Skill Marketplace is off", async () => {
    vi.mocked(api.getSkillMarketplace).mockResolvedValue({ enabled: false, known: false, catalog: null });
    renderPage();
    expect(await screen.findByText("The Skill Marketplace is not enabled in this installation.")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Repository" })).not.toBeInTheDocument();
  });

  it("says when the catalog is not published yet", async () => {
    vi.mocked(api.getSkillMarketplace).mockResolvedValue({ enabled: true, known: false, catalog: null });
    renderPage();
    expect(await screen.findByText("The Skill Marketplace has not published its catalog yet.")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Repository" })).not.toBeInTheDocument();
  });

  it("shows empty lists and a catalog that was never synchronized", async () => {
    vi.mocked(api.getSkillMarketplace).mockResolvedValue({ enabled: true, known: true,
      catalog: { version: "v1", repository: { url: "git@git.example.com:org/skills.git" } } });
    renderPage();
    const repository = within(await screen.findByRole("region", { name: "Repository" }));
    expect(repository.getByText("Default branch")).toBeInTheDocument();
    expect(repository.getByText("Repository root")).toBeInTheDocument();
    expect(repository.getByText("Not synchronized yet")).toBeInTheDocument();
    expect(screen.getByText("The repository has no valid skills.")).toBeInTheDocument();
    expect(screen.getByText("No Team has a publication yet.")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Invalid skills" })).not.toBeInTheDocument();
  });

  it("shows why the catalog could not be loaded", async () => {
    vi.mocked(api.getSkillMarketplace).mockRejectedValue(new Error("The Skill Marketplace catalog could not be read. Check the Portal logs."));
    renderPage();
    expect(await screen.findByText("Failed to load: The Skill Marketplace catalog could not be read. Check the Portal logs.")).toBeInTheDocument();
  });
});
