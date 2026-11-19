// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
)

// The Skill Marketplace contract shared by the operator, its Skill Marketplace
// worker (cmd/skill-marketplace), the Agent chart and the Portal.
//
// One Git repository of skills is synchronized per installation. Each
// policy-bound Team selects skills by name in spec.skills: every entry
// without the "npm:" prefix names a marketplace skill while the marketplace
// is enabled. The marketplace publishes each Team's selection as an
// immutable release on read-only shared storage:
//
//	teams/<team-uid>/CURRENT                       release ID, replaced atomically
//	teams/<team-uid>/releases/<id>/<skill>/...     one directory per published skill
//	teams/<team-uid>/releases/<id>/.cetirun-selection   the release's selection digest
//	teams/<team-uid>/releases/<id>/.cetirun-release.json  SkillMarketplaceRelease
//
// An Agent's start copies only the release that CURRENT names, after its
// .cetirun-selection equals the selection digest the operator rendered.
const (
	// SkillMarketplaceCatalogConfigMap is the marketplace's published state,
	// in the operator namespace. The operator creates it; the worker writes
	// its content.
	SkillMarketplaceCatalogConfigMap = "cetirun-skill-marketplace"
	// SkillMarketplaceCatalogKey holds a JSON SkillMarketplaceCatalog.
	SkillMarketplaceCatalogKey = "catalog.json"
	// SkillMarketplaceCatalogVersion is the catalog document version.
	SkillMarketplaceCatalogVersion = "v1"

	// SkillMarketplaceTeamsDir is the storage directory of the Team subtrees.
	SkillMarketplaceTeamsDir = "teams"
	// SkillMarketplaceCurrentFile names the current release of a Team.
	SkillMarketplaceCurrentFile = "CURRENT"
	// SkillMarketplaceReleasesDir holds a Team's immutable releases.
	SkillMarketplaceReleasesDir = "releases"
	// SkillMarketplaceSelectionFile holds a release's selection digest on one line.
	SkillMarketplaceSelectionFile = ".cetirun-selection"
	// SkillMarketplaceReleaseManifest holds a JSON SkillMarketplaceRelease.
	SkillMarketplaceReleaseManifest = ".cetirun-release.json"

	// SkillMarketplaceManagedLabel marks the PersistentVolumes and claims that
	// the operator creates for the marketplace skills of Agents. Its value is
	// "true".
	SkillMarketplaceManagedLabel = "skills.cetirun.microsoft.com/marketplace"
	// SkillMarketplaceTeamUIDAnnotation records the Team whose subtree an
	// Agent's skills claim serves.
	SkillMarketplaceTeamUIDAnnotation = "skills.cetirun.microsoft.com/team-uid"

	// MaxSkillMarketplaceNameLength bounds a marketplace skill name.
	MaxSkillMarketplaceNameLength = 64
)

var (
	skillMarketplaceName      = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	skillMarketplaceReleaseID = regexp.MustCompile(`^[a-f0-9]{20}$`)
)

// IsSkillMarketplaceReference reports whether a Team or Agent skill entry
// refers to the marketplace (rather than an npm package) while the marketplace
// is enabled.
func IsSkillMarketplaceReference(entry string) bool {
	return !strings.HasPrefix(entry, "npm:")
}

// ValidSkillMarketplaceName reports whether name is a valid marketplace skill
// name: lowercase alphanumeric words separated by single hyphens, at most 64
// characters. It equals the skill's directory name and SKILL.md name.
func ValidSkillMarketplaceName(name string) bool {
	return len(name) <= MaxSkillMarketplaceNameLength && skillMarketplaceName.MatchString(name)
}

// ValidSkillMarketplaceReleaseID reports whether id has the release ID form.
func ValidSkillMarketplaceReleaseID(id string) bool {
	return skillMarketplaceReleaseID.MatchString(id)
}

// SkillMarketplaceSelection returns the sorted, de-duplicated marketplace
// references of a skills list, without npm entries.
func SkillMarketplaceSelection(skills []string) []string {
	selection := make([]string, 0, len(skills))
	for _, entry := range skills {
		if IsSkillMarketplaceReference(entry) {
			selection = append(selection, entry)
		}
	}
	slices.Sort(selection)
	return slices.Compact(selection)
}

// SkillMarketplaceSelectionDigest identifies a Team's marketplace selection.
// The marketplace writes it into each release; the operator renders it for
// the Agent's start, which waits until the current release carries it.
func SkillMarketplaceSelectionDigest(skills []string) string {
	sum := sha256.Sum256([]byte(strings.Join(SkillMarketplaceSelection(skills), "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SkillMarketplaceTeamPath is the storage subtree of one Team, relative to
// the marketplace container root.
func SkillMarketplaceTeamPath(teamUID string) string {
	return SkillMarketplaceTeamsDir + "/" + teamUID
}

// SkillMarketplaceCatalog is the content of the marketplace catalog
// ConfigMap. Times are RFC 3339 UTC strings.
type SkillMarketplaceCatalog struct {
	// Version is SkillMarketplaceCatalogVersion.
	Version string `json:"version"`
	// Repository is the configured source, without credentials.
	Repository SkillMarketplaceRepository `json:"repository"`
	// Commit is the last successfully synchronized commit.
	Commit string `json:"commit,omitempty"`
	// SyncedAt is when Commit was last synchronized successfully.
	SyncedAt string `json:"syncedAt,omitempty"`
	// CheckedAt is when the repository was last checked.
	CheckedAt string `json:"checkedAt,omitempty"`
	// Error describes the last failed check of the repository, without
	// secrets. A failed publication is the Error of its Team.
	Error string `json:"error,omitempty"`
	// Skills are the valid skills at Commit, sorted by name.
	Skills []SkillMarketplaceSkill `json:"skills,omitempty"`
	// Invalid lists the skill directories that were rejected, and why.
	Invalid []SkillMarketplaceProblem `json:"invalid,omitempty"`
	// Teams reports each policy-bound Team's publication, sorted by name.
	Teams []SkillMarketplaceTeam `json:"teams,omitempty"`
	// Truncated is true when lists were shortened to fit the ConfigMap.
	Truncated bool `json:"truncated,omitempty"`
}

// SkillMarketplaceRepository describes the synchronized repository.
type SkillMarketplaceRepository struct {
	URL  string `json:"url"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path,omitempty"`
}

// SkillMarketplaceSkill is one valid skill of the repository.
type SkillMarketplaceSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Version identifies the skill's content: the first 12 hexadecimal
	// characters of its directory's Git tree hash.
	Version string `json:"version"`
	// Path is the skill directory, relative to the repository root.
	Path string `json:"path"`
	// UpdatedAt is when the marketplace first saw this Version.
	UpdatedAt string `json:"updatedAt,omitempty"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
}

// SkillMarketplaceProblem explains a rejected skill directory.
type SkillMarketplaceProblem struct {
	Path   string `json:"path"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

// SkillMarketplaceTeam is the publication state of one Team.
type SkillMarketplaceTeam struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
	// SelectionDigest is SkillMarketplaceSelectionDigest of the Team's current spec.skills.
	SelectionDigest string `json:"selectionDigest"`
	// Release is the current release ID, empty before the first publication.
	Release string `json:"release,omitempty"`
	// ReleaseSelectionDigest is the selection digest of Release. The Team's
	// Agents can start once it equals SelectionDigest.
	ReleaseSelectionDigest string `json:"releaseSelectionDigest,omitempty"`
	PublishedAt            string `json:"publishedAt,omitempty"`
	// Skills are the skill versions in Release.
	Skills []SkillMarketplaceReleaseSkill `json:"skills,omitempty"`
	// Retained lists selected skills that are no longer valid in the
	// repository; Release keeps their last published version.
	Retained []string `json:"retained,omitempty"`
	// Missing lists selected skills that were never published.
	Missing []string `json:"missing,omitempty"`
	// Error describes a failed publication.
	Error string `json:"error,omitempty"`
}

// SkillMarketplaceReleaseSkill is one skill version in a release.
type SkillMarketplaceReleaseSkill struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// SkillMarketplaceRelease is the manifest of one immutable Team release.
type SkillMarketplaceRelease struct {
	// Version is SkillMarketplaceCatalogVersion.
	Version         string                         `json:"version"`
	ID              string                         `json:"id"`
	TeamName        string                         `json:"teamName"`
	TeamUID         string                         `json:"teamUID"`
	SelectionDigest string                         `json:"selectionDigest"`
	Commit          string                         `json:"commit,omitempty"`
	CreatedAt       string                         `json:"createdAt"`
	Skills          []SkillMarketplaceReleaseSkill `json:"skills,omitempty"`
}
