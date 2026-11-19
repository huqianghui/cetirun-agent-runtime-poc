// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	corev1 "k8s.io/api/core/v1"
)

// AdminSkillMarketplaceHandler serves the Skill Marketplace catalog, from
// which administrators choose Team skills. Authorization is enforced by the
// admin middleware.
type AdminSkillMarketplaceHandler struct {
	Kube   *kube.Client
	Config config.Config
}

// SkillMarketplaceResponse is the JSON response for
// GET /api/v1/admin/skill-marketplace.
type SkillMarketplaceResponse struct {
	// Enabled reports whether the deployment uses the Skill Marketplace.
	Enabled bool `json:"enabled"`
	// Known is false until the marketplace has published its catalog.
	Known bool `json:"known"`
	// Catalog is the published catalog, or null.
	Catalog *cetirunv1alpha1.SkillMarketplaceCatalog `json:"catalog"`
}

// GetSkillMarketplace handles GET /api/v1/admin/skill-marketplace.
func (h *AdminSkillMarketplaceHandler) GetSkillMarketplace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !h.Config.SkillMarketplaceEnabled {
		writeJSON(w, http.StatusOK, SkillMarketplaceResponse{})
		return
	}
	catalog, known, err := h.Kube.GetSkillMarketplaceCatalog(r.Context(), h.Config.SkillMarketplaceCatalogNamespace)
	if err != nil {
		slog.Error("admin: read the Skill Marketplace catalog failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "skill_marketplace_unavailable", "The Skill Marketplace catalog could not be read. Check the Portal logs.")
		return
	}
	response := SkillMarketplaceResponse{Enabled: true, Known: known}
	if known {
		response.Catalog = &catalog
	}
	writeJSON(w, http.StatusOK, response)
}

// shortenedForMessage shortens a value for an error message.
func shortenedForMessage(value string) string {
	const limit = 120
	if utf8.RuneCountInString(value) > limit {
		return string([]rune(value)[:limit]) + "..."
	}
	return value
}

// invalidMarketplaceSkillName describes the first Team skill that is neither
// an npm package nor a valid Skill Marketplace skill name, or returns "".
// While the marketplace is enabled every entry without the npm: prefix names
// a marketplace skill, so an entry it can never publish is refused.
func invalidMarketplaceSkillName(skills []string) string {
	for i, skill := range skills {
		if cetirunv1alpha1.IsSkillMarketplaceReference(skill) && !cetirunv1alpha1.ValidSkillMarketplaceName(skill) {
			return fmt.Sprintf("spec.skills[%d]: %s is not a valid Skill Marketplace skill name. Use at most %d lowercase letters "+
				"and digits, in words joined by single hyphens, or the npm: prefix for an npm package.",
				i, strconv.Quote(shortenedForMessage(skill)), cetirunv1alpha1.MaxSkillMarketplaceNameLength)
		}
	}
	return ""
}

// validateNewMarketplaceSkills checks that each Skill Marketplace skill the
// Team does not select yet is in the marketplace catalog. previous holds the
// stored Team's skills, nil on creation. Skills the Team already selects may
// stay after they leave the repository: the marketplace keeps their last
// published version. The caller has checked the names.
func (h *AdminTeamHandler) validateNewMarketplaceSkills(w http.ResponseWriter, r *http.Request, skills, previous []string) bool {
	var added []int
	for i, skill := range skills {
		if cetirunv1alpha1.IsSkillMarketplaceReference(skill) && !slices.Contains(previous, skill) {
			added = append(added, i)
		}
	}
	if len(added) == 0 {
		return true
	}
	catalog, known, err := h.Kube.GetSkillMarketplaceCatalog(r.Context(), h.Config.SkillMarketplaceCatalogNamespace)
	if err != nil {
		slog.Error("admin: read the Skill Marketplace catalog for a Team failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "skill_marketplace_unavailable",
			"The Skill Marketplace catalog could not be read; new Team skills cannot be validated. Try again later, or check the Portal logs.")
		return false
	}
	if !known {
		writeError(w, http.StatusUnprocessableEntity, "invalid_team", "The Skill Marketplace catalog is not available yet; new Team skills cannot be validated")
		return false
	}
	for _, i := range added {
		name := skills[i]
		if slices.ContainsFunc(catalog.Skills, func(skill cetirunv1alpha1.SkillMarketplaceSkill) bool { return skill.Name == name }) {
			continue
		}
		message := fmt.Sprintf("spec.skills[%d]: Skill %s is not in the Skill Marketplace catalog. Choose a skill the marketplace has synchronized from its repository.", i, name)
		if problem := slices.IndexFunc(catalog.Invalid, func(problem cetirunv1alpha1.SkillMarketplaceProblem) bool {
			return problem.Name == name && problem.Reason != ""
		}); problem >= 0 {
			message = fmt.Sprintf("spec.skills[%d]: Skill %s is not available; the Skill Marketplace rejected the repository's copy: %s",
				i, name, shortenedForMessage(catalog.Invalid[problem].Reason))
		}
		writeError(w, http.StatusUnprocessableEntity, "invalid_team", message)
		return false
	}
	return true
}

// allowSkillRemoval refuses to remove from a policy's allowed skills a Skill
// Marketplace skill that a Team using the policy still selects: a Team may
// select only skills its policy allows. It reports whether the update may
// continue. npm entries keep the earlier behaviour.
func (h *AdminPolicyHandler) allowSkillRemoval(w http.ResponseWriter, r *http.Request, policy string, before, after []string) bool {
	var removed []string
	for _, skill := range before {
		if cetirunv1alpha1.IsSkillMarketplaceReference(skill) && !slices.Contains(after, skill) {
			removed = append(removed, skill)
		}
	}
	if len(removed) == 0 {
		return true
	}
	teams, err := h.Kube.ListTeams(r.Context())
	if err != nil {
		slog.Error("admin: list teams for a policy skill removal failed", "policy", policy, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check which Teams select the removed skills")
		return false
	}
	slices.Sort(removed)
	slices.SortFunc(teams, func(a, b cetirunv1alpha1.Team) int { return strings.Compare(a.Name, b.Name) })
	for _, skill := range removed {
		for _, team := range teams {
			if team.Spec.AgentPolicyRef == policy && slices.Contains(team.Spec.Skills, skill) {
				writeError(w, http.StatusUnprocessableEntity, "invalid_policy",
					"Skill "+skill+" is selected by Team "+team.Name+"; remove it from the Team first")
				return false
			}
		}
	}
	return true
}

// Team skill states in the owner's view of an Agent.
const (
	// teamSkillPublished: the release of the Team's current selection has
	// the skill, at the repository's version.
	teamSkillPublished = "published"
	// teamSkillUpdating: that release has the skill at another version than
	// the repository. The marketplace has not published the repository's
	// version for the Team yet, or could not.
	teamSkillUpdating = "updating"
	// teamSkillRetained: the repository's copy is no longer valid, and the
	// Team's release keeps the last published version.
	teamSkillRetained = "retained"
	// teamSkillMissing: the skill was never published.
	teamSkillMissing = "missing"
	// teamSkillPending: the marketplace has not published the Team's
	// current selection yet.
	teamSkillPending = "pending"
)

// TeamSkill is one Skill Marketplace skill that the Agent's Team selects.
type TeamSkill struct {
	Name string `json:"name"`
	// Description comes from the marketplace catalog.
	Description string `json:"description"`
	// Version is the skill's version in the Team's release, which the Agent
	// copies at its start: the release of the Team's current selection, or
	// for a retained skill the release that keeps it. It is empty when that
	// release does not have the skill.
	Version string `json:"version"`
	// RepositoryVersion is the skill's version in the repository when it
	// differs from Version: the Team's release does not have it yet.
	RepositoryVersion string `json:"repositoryVersion,omitempty"`
	// State is published, updating, retained, missing or pending.
	State string `json:"state"`
}

// teamSkills returns the Skill Marketplace skills of the Agent's Team, or
// nil when the marketplace is disabled, the Team has no policy or selects no
// marketplace skill, or the Team or the catalog cannot be read. restart
// reports that the Agent runs an earlier release of the Team's current
// selection. failed reports that the catalog has a publication error for the
// Team; administrators see it on the Skills page. It never fails the Agent's
// response.
func (h *InstanceHandler) teamSkills(ctx context.Context, instance *cetirunv1alpha1.Agent) (skills []TeamSkill, restart, failed bool) {
	if !h.Config.SkillMarketplaceEnabled || instance.Spec.Team == "" {
		return nil, false, false
	}
	team, err := h.Kube.GetTeam(ctx, instance.Spec.Team)
	if err != nil {
		slog.Debug("Team skills unavailable: Team not readable", "team", instance.Spec.Team, "error", err)
		return nil, false, false
	}
	if team.Spec.AgentPolicyRef == "" || len(cetirunv1alpha1.SkillMarketplaceSelection(team.Spec.Skills)) == 0 {
		return nil, false, false
	}
	catalog, _, err := h.Kube.GetSkillMarketplaceCatalog(ctx, h.Config.SkillMarketplaceCatalogNamespace)
	if err != nil {
		slog.Warn("Team skills unavailable: the Skill Marketplace catalog is not readable", "team", team.Name, "error", err)
		return nil, false, false
	}
	entry := catalogTeam(catalog, team)
	return teamSkillStates(team, catalog), h.teamSkillsRestartNeeded(ctx, instance, team, catalog), entry != nil && entry.Error != ""
}

// catalogTeam returns the catalog's entry for the Team. It matches the UID,
// so a Team created again under the same name never sees its predecessor's
// release.
func catalogTeam(catalog cetirunv1alpha1.SkillMarketplaceCatalog, team *cetirunv1alpha1.Team) *cetirunv1alpha1.SkillMarketplaceTeam {
	for i := range catalog.Teams {
		if team.UID != "" && catalog.Teams[i].UID == string(team.UID) {
			return &catalog.Teams[i]
		}
	}
	return nil
}

// teamSkillStates reports the Team's marketplace skills in name order.
// Without a catalog entry for the Team, every skill is pending. The version
// of a skill is the one that the Team's release has: a failed or unfinished
// publication keeps the earlier release, also after the repository changed.
func teamSkillStates(team *cetirunv1alpha1.Team, catalog cetirunv1alpha1.SkillMarketplaceCatalog) []TeamSkill {
	entry := catalogTeam(catalog, team)
	current := entry != nil && entry.ReleaseSelectionDigest == cetirunv1alpha1.SkillMarketplaceSelectionDigest(team.Spec.Skills)
	selection := cetirunv1alpha1.SkillMarketplaceSelection(team.Spec.Skills)
	skills := make([]TeamSkill, 0, len(selection))
	for _, name := range selection {
		skill := TeamSkill{Name: name, State: teamSkillPending}
		repository := ""
		if i := slices.IndexFunc(catalog.Skills, func(skill cetirunv1alpha1.SkillMarketplaceSkill) bool { return skill.Name == name }); i >= 0 {
			skill.Description, repository = catalog.Skills[i].Description, catalog.Skills[i].Version
		}
		if entry != nil {
			released := slices.IndexFunc(entry.Skills, func(skill cetirunv1alpha1.SkillMarketplaceReleaseSkill) bool { return skill.Name == name })
			switch {
			// A retained skill is in the release too; retained says that it
			// no longer follows the repository.
			case slices.Contains(entry.Retained, name):
				skill.State = teamSkillRetained
			case current && released >= 0:
				skill.State = teamSkillPublished
				if repository != "" && repository != entry.Skills[released].Version {
					skill.State = teamSkillUpdating
				}
			case slices.Contains(entry.Missing, name):
				skill.State = teamSkillMissing
			}
			// An Agent starts only with the release of the current selection,
			// so an earlier selection's release does not give the version.
			if released >= 0 && (current || skill.State == teamSkillRetained) {
				skill.Version = entry.Skills[released].Version
			}
		}
		if repository != skill.Version {
			skill.RepositoryVersion = repository
		}
		skills = append(skills, skill)
	}
	return skills
}

// The HOME initializer of an Agent copies the release of the selection digest
// in its environment, and reports the release in its termination message
// (app/cetirun-chart/configs/init-home-common.sh).
const (
	skillsInitializer   = "init-home-data"
	skillsSelectionEnv  = "CETIRUN_SKILLS_RELEASE_SELECTION"
	skillsReleaseReport = "cetirun.skills.release="
)

// teamSkillsRestartNeeded reports whether the Agent's newest Pod copied
// another release of the Team's current selection than the catalog's current
// one: the marketplace published a repository change after the Pod started,
// and only a restart copies it. A changed selection restarts the Agent by
// itself, and a Pod that reports no release, as from an earlier chart, never
// asks for a restart.
func (h *InstanceHandler) teamSkillsRestartNeeded(ctx context.Context, instance *cetirunv1alpha1.Agent,
	team *cetirunv1alpha1.Team, catalog cetirunv1alpha1.SkillMarketplaceCatalog) bool {
	entry := catalogTeam(catalog, team)
	selection := cetirunv1alpha1.SkillMarketplaceSelectionDigest(team.Spec.Skills)
	if entry == nil || entry.Release == "" || entry.ReleaseSelectionDigest != selection {
		return false
	}
	pods, err := h.Kube.GetPods(ctx, "cetirun-"+instance.Name)
	if err != nil {
		slog.Debug("Team skills restart unknown: Pods not readable", "instance", instance.Name, "error", err)
		return false
	}
	var newest *corev1.Pod
	copied := ""
	for i := range pods {
		pod := &pods[i]
		release, ok := copiedSkillsRelease(pod, selection)
		if ok && (newest == nil || newest.CreationTimestamp.Before(&pod.CreationTimestamp)) {
			newest, copied = pod, release
		}
	}
	return newest != nil && copied != entry.Release
}

// copiedSkillsRelease returns the release that the HOME initializer of the
// Pod reported for the selection digest. A Pod that is being deleted reports
// nothing.
func copiedSkillsRelease(pod *corev1.Pod, selection string) (string, bool) {
	if pod.DeletionTimestamp != nil {
		return "", false
	}
	initializer := slices.IndexFunc(pod.Spec.InitContainers, func(container corev1.Container) bool {
		return container.Name == skillsInitializer
	})
	if initializer < 0 || !slices.ContainsFunc(pod.Spec.InitContainers[initializer].Env, func(env corev1.EnvVar) bool {
		return env.Name == skillsSelectionEnv && env.Value == selection && env.ValueFrom == nil
	}) {
		return "", false
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name != skillsInitializer || status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
			continue
		}
		for _, line := range strings.Split(status.State.Terminated.Message, "\n") {
			if release, found := strings.CutPrefix(line, skillsReleaseReport); found &&
				cetirunv1alpha1.ValidSkillMarketplaceReleaseID(release) {
				return release, true
			}
		}
	}
	return "", false
}
