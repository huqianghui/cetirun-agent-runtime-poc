// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

func marketplaceConfig() config.Config {
	return config.Config{SkillMarketplaceEnabled: true, SkillMarketplaceCatalogNamespace: "cetirun-system"}
}

func marketplaceCatalogConfigMap(catalog api.SkillMarketplaceCatalog) *corev1.ConfigMap {
	raw, _ := json.Marshal(catalog)
	return marketplaceCatalogData(string(raw))
}

func marketplaceCatalogData(raw string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: api.SkillMarketplaceCatalogConfigMap, Namespace: "cetirun-system"},
		Data:       map[string]string{api.SkillMarketplaceCatalogKey: raw},
	}
}

// exampleMarketplaceCatalog lists two valid skills and one the marketplace
// rejected. retired-skill and unpublished-skill are not in the repository.
func exampleMarketplaceCatalog() api.SkillMarketplaceCatalog {
	return api.SkillMarketplaceCatalog{
		Version:    api.SkillMarketplaceCatalogVersion,
		Repository: api.SkillMarketplaceRepository{URL: "https://git.example.com/skills.git", Ref: "main"},
		Commit:     strings.Repeat("c", 40),
		SyncedAt:   "2026-11-03T00:00:00Z",
		Skills: []api.SkillMarketplaceSkill{
			{Name: "pdf-tools", Description: "Read and write PDF files", Version: "0123456789ab", Path: "skills/pdf-tools", Files: 2, Bytes: 2048},
			{Name: "release-notes", Description: "Draft release notes", Version: "ba9876543210", Path: "skills/release-notes", Files: 1, Bytes: 512},
		},
		Invalid: []api.SkillMarketplaceProblem{{Path: "skills/broken", Name: "broken", Reason: "SKILL.md has no description"}},
	}
}

func decodeErrorResponse(t *testing.T, w *httptest.ResponseRecorder) ErrorDetail {
	t.Helper()
	var response ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return response.Error
}

func TestAdminSkillMarketplaceCatalog(t *testing.T) {
	published := marketplaceCatalogConfigMap(exampleMarketplaceCatalog())
	for _, tc := range []struct {
		name     string
		cfg      config.Config
		objects  []client.Object
		status   int
		wantBody string
	}{
		{name: "disabled", objects: []client.Object{published}, status: http.StatusOK, wantBody: `{"enabled":false,"known":false,"catalog":null}`},
		{name: "not published yet", cfg: marketplaceConfig(), status: http.StatusOK, wantBody: `{"enabled":true,"known":false,"catalog":null}`},
		{name: "published", cfg: marketplaceConfig(), objects: []client.Object{published}, status: http.StatusOK},
		{name: "unreadable", cfg: marketplaceConfig(), objects: []client.Object{marketplaceCatalogData(`{"version":"v1",`)}, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &AdminSkillMarketplaceHandler{Kube: kube.NewClient(newFakeClient(tc.objects...)), Config: tc.cfg}
			w := httptest.NewRecorder()
			h.GetSkillMarketplace(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/skill-marketplace", nil))
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("the catalog must not be cached")
			}
			if tc.wantBody != "" && strings.TrimSpace(w.Body.String()) != tc.wantBody {
				t.Fatalf("body %s, want %s", w.Body.String(), tc.wantBody)
			}
			switch tc.name {
			case "published":
				var response SkillMarketplaceResponse
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if !response.Enabled || !response.Known || response.Catalog == nil || !reflect.DeepEqual(*response.Catalog, exampleMarketplaceCatalog()) {
					t.Fatalf("response = %s", w.Body.String())
				}
			case "unreadable":
				if detail := decodeErrorResponse(t, w); detail.Code != "skill_marketplace_unavailable" || strings.Contains(detail.Message, "version") {
					t.Fatalf("error = %+v", detail)
				}
			}
		})
	}
}

// newMarketplaceTeamHandler serves a policy that allows every skill of the
// tests, and the given objects.
func newMarketplaceTeamHandler(cfg config.Config, objects ...client.Object) *AdminTeamHandler {
	objects = append(objects, &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "marketplace-policy"}, Spec: api.AgentPolicySpec{
		Skills: api.AgentPolicySkills{Allowed: []string{"npm:@scope/skill", "pdf-tools", "release-notes", "retired-skill", "unpublished-skill", "broken", "@anthropic/mcp-server-fetch"}},
	}})
	h := newAdminTeamHandler(objects...)
	h.Config = cfg
	return h
}

func createMarketplaceTeam(h *AdminTeamHandler, policy string, skills ...string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(TeamUpsertRequest{Name: "research", AgentPolicyRef: policy, SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard", Skills: skills})
	w := httptest.NewRecorder()
	h.CreateTeam(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/teams", bytes.NewReader(body)))
	return w
}

func TestCreateTeamChecksMarketplaceSkills(t *testing.T) {
	published := marketplaceCatalogConfigMap(exampleMarketplaceCatalog())
	for _, tc := range []struct {
		name     string
		cfg      config.Config
		objects  []client.Object
		policy   string
		skills   []string
		status   int
		code     string
		contains []string
	}{
		{name: "skills in the catalog", cfg: marketplaceConfig(), objects: []client.Object{published}, skills: []string{"npm:@scope/skill", "pdf-tools", "release-notes"}, status: http.StatusCreated},
		{name: "skill not in the catalog", cfg: marketplaceConfig(), objects: []client.Object{published}, skills: []string{"pdf-tools", "unpublished-skill"}, status: http.StatusUnprocessableEntity,
			code: "invalid_team", contains: []string{"spec.skills[1]: Skill unpublished-skill is not in the Skill Marketplace catalog"}},
		{name: "skill the marketplace rejected", cfg: marketplaceConfig(), objects: []client.Object{published}, skills: []string{"broken"}, status: http.StatusUnprocessableEntity,
			code: "invalid_team", contains: []string{"spec.skills[0]: Skill broken is not available", "SKILL.md has no description"}},
		{name: "catalog not published yet", cfg: marketplaceConfig(), skills: []string{"pdf-tools"}, status: http.StatusUnprocessableEntity,
			code: "invalid_team", contains: []string{"The Skill Marketplace catalog is not available yet; new Team skills cannot be validated"}},
		{name: "catalog unreadable", cfg: marketplaceConfig(), objects: []client.Object{marketplaceCatalogData("{")}, skills: []string{"pdf-tools"}, status: http.StatusServiceUnavailable,
			code: "skill_marketplace_unavailable", contains: []string{"could not be read"}},
		{name: "invalid skill name", cfg: marketplaceConfig(), objects: []client.Object{published}, skills: []string{"npm:@scope/skill", "@anthropic/mcp-server-fetch"}, status: http.StatusUnprocessableEntity,
			code: "invalid_team", contains: []string{`spec.skills[1]: "@anthropic/mcp-server-fetch" is not a valid Skill Marketplace skill name`, "npm: prefix"}},
		{name: "invalid name is refused before the policy check", cfg: marketplaceConfig(), objects: []client.Object{published}, policy: "standard-policy", skills: []string{"PDF_Tools"}, status: http.StatusUnprocessableEntity,
			code: "invalid_team", contains: []string{`spec.skills[0]: "PDF_Tools" is not a valid`}},
		{name: "npm packages need no catalog", cfg: marketplaceConfig(), skills: []string{"npm:@scope/skill"}, status: http.StatusCreated},
		{name: "the policy still decides", cfg: marketplaceConfig(), objects: []client.Object{published}, policy: "standard-policy", skills: []string{"pdf-tools"}, status: http.StatusBadRequest,
			code: "invalid_team_binding", contains: []string{"must be allowed by the selected Agent Policy"}},
		{name: "disabled keeps the earlier checks", skills: []string{"unpublished-skill", "@anthropic/mcp-server-fetch"}, status: http.StatusCreated},
		{name: "disabled still applies the policy", policy: "standard-policy", skills: []string{"pdf-tools"}, status: http.StatusBadRequest, code: "invalid_team_binding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newMarketplaceTeamHandler(tc.cfg, tc.objects...)
			policy := tc.policy
			if policy == "" {
				policy = "marketplace-policy"
			}
			w := createMarketplaceTeam(h, policy, tc.skills...)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			_, err := h.Kube.GetTeam(context.Background(), "research")
			if created := err == nil; created != (tc.status == http.StatusCreated) {
				t.Fatalf("Team created=%v after status %d (%v)", created, w.Code, err)
			}
			if tc.code == "" {
				return
			}
			detail := decodeErrorResponse(t, w)
			if detail.Code != tc.code {
				t.Errorf("code %q, want %q", detail.Code, tc.code)
			}
			for _, want := range tc.contains {
				if !strings.Contains(detail.Message, want) {
					t.Errorf("message %q does not contain %q", detail.Message, want)
				}
			}
		})
	}
}

func TestUpdateTeamChecksOnlyNewMarketplaceSkills(t *testing.T) {
	published := marketplaceCatalogConfigMap(exampleMarketplaceCatalog())
	stored := func(skills ...string) *api.Team {
		return &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "research"}, Spec: api.TeamSpec{
			AgentPolicyRef: "marketplace-policy", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard", Skills: skills,
		}}
	}
	for _, tc := range []struct {
		name     string
		cfg      config.Config
		catalog  *corev1.ConfigMap
		stored   []string
		skills   []string
		status   int
		contains string
	}{
		{name: "a selected skill may stay after it leaves the repository", cfg: marketplaceConfig(), catalog: published,
			stored: []string{"retired-skill", "pdf-tools"}, skills: []string{"retired-skill", "pdf-tools"}, status: http.StatusOK},
		{name: "a new skill in the catalog", cfg: marketplaceConfig(), catalog: published,
			stored: []string{"retired-skill"}, skills: []string{"retired-skill", "release-notes"}, status: http.StatusOK},
		{name: "a new skill not in the catalog", cfg: marketplaceConfig(), catalog: published,
			stored: []string{"pdf-tools"}, skills: []string{"pdf-tools", "retired-skill"}, status: http.StatusUnprocessableEntity,
			contains: "spec.skills[1]: Skill retired-skill is not in the Skill Marketplace catalog"},
		{name: "no new skills without a catalog", cfg: marketplaceConfig(),
			stored: []string{"retired-skill", "pdf-tools"}, skills: []string{"pdf-tools"}, status: http.StatusOK},
		{name: "a new skill without a catalog", cfg: marketplaceConfig(),
			stored: []string{"pdf-tools"}, skills: []string{"pdf-tools", "release-notes"}, status: http.StatusUnprocessableEntity,
			contains: "The Skill Marketplace catalog is not available yet; new Team skills cannot be validated"},
		{name: "a stored entry that can never be published", cfg: marketplaceConfig(), catalog: published,
			stored: []string{"@anthropic/mcp-server-fetch"}, skills: []string{"@anthropic/mcp-server-fetch"}, status: http.StatusUnprocessableEntity,
			contains: `spec.skills[0]: "@anthropic/mcp-server-fetch" is not a valid Skill Marketplace skill name`},
		{name: "disabled", stored: []string{"pdf-tools"}, skills: []string{"pdf-tools", "unpublished-skill", "@anthropic/mcp-server-fetch"}, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{stored(tc.stored...)}
			if tc.catalog != nil {
				objects = append(objects, tc.catalog)
			}
			h := newMarketplaceTeamHandler(tc.cfg, objects...)
			current, err := h.Kube.GetTeam(context.Background(), "research")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(TeamUpsertRequest{UID: string(current.UID), ResourceVersion: current.ResourceVersion, Role: "Researcher",
				AgentPolicyRef: "marketplace-policy", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard", Skills: tc.skills})
			r := httptest.NewRequest(http.MethodPut, "/api/v1/admin/teams/research", bytes.NewReader(body))
			r.SetPathValue("name", "research")
			w := httptest.NewRecorder()
			h.UpdateTeam(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			after, err := h.Kube.GetTeam(context.Background(), "research")
			if err != nil {
				t.Fatal(err)
			}
			want := tc.stored
			if tc.status == http.StatusOK {
				want = tc.skills
			}
			if !reflect.DeepEqual(after.Spec.Skills, want) {
				t.Fatalf("stored skills %v, want %v", after.Spec.Skills, want)
			}
			if tc.contains != "" {
				if detail := decodeErrorResponse(t, w); detail.Code != "invalid_team" || !strings.Contains(detail.Message, tc.contains) {
					t.Fatalf("error %+v, want invalid_team containing %q", detail, tc.contains)
				}
			}
		})
	}
}

func TestPolicyKeepsMarketplaceSkillsATeamSelects(t *testing.T) {
	policyRequest := func(allowed ...string) AgentPolicyUpsertRequest {
		req := validPolicyRequest()
		req.Name = "marketplace-policy"
		req.Skills.Allowed = allowed
		return req
	}
	team := func(name, policy string, skills ...string) *api.Team {
		return &api.Team{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.TeamSpec{AgentPolicyRef: policy, SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard", Skills: skills}}
	}
	initial := []string{"npm:@scope/skill", "pdf-tools", "release-notes"}
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		allowed []string
		message string
	}{
		{name: "a skill Teams of the policy select", cfg: marketplaceConfig(), allowed: []string{"npm:@scope/skill", "release-notes"},
			message: "Skill pdf-tools is selected by Team research; remove it from the Team first"},
		{name: "several skills", cfg: marketplaceConfig(), allowed: []string{"npm:@scope/skill"},
			message: "Skill pdf-tools is selected by Team research; remove it from the Team first"},
		{name: "a skill only a Team of another policy selects", cfg: marketplaceConfig(), allowed: []string{"npm:@scope/skill", "pdf-tools"}},
		{name: "an npm package keeps the earlier behaviour", cfg: marketplaceConfig(), allowed: []string{"pdf-tools", "release-notes"}},
		{name: "adding a skill", cfg: marketplaceConfig(), allowed: append(append([]string{}, initial...), "new-skill")},
		{name: "disabled", allowed: []string{"npm:@scope/skill"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "marketplace-policy", UID: "policy-uid"}, Spec: toAgentPolicySpec(policyRequest(initial...))}
			h := newAdminPolicyHandler(stored,
				team("triage", "marketplace-policy", "pdf-tools"),
				team("research", "marketplace-policy", "npm:@scope/skill", "pdf-tools"),
				team("support", "other-policy", "release-notes"))
			h.Config = tc.cfg
			current, err := h.Kube.GetAgentPolicy(context.Background(), "marketplace-policy")
			if err != nil {
				t.Fatal(err)
			}
			req := policyRequest(tc.allowed...)
			req.UID, req.ResourceVersion = string(current.UID), current.ResourceVersion
			w := putPolicy(t, h, "marketplace-policy", req)
			want, status := tc.allowed, http.StatusOK
			if tc.message != "" {
				want, status = initial, http.StatusUnprocessableEntity
			}
			if w.Code != status {
				t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
			}
			after, err := h.Kube.GetAgentPolicy(context.Background(), "marketplace-policy")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after.Spec.Skills.Allowed, want) {
				t.Fatalf("allowed skills %v, want %v", after.Spec.Skills.Allowed, want)
			}
			if tc.message != "" {
				if detail := decodeErrorResponse(t, w); detail.Code != "invalid_policy" || detail.Message != tc.message {
					t.Fatalf("error %+v, want %q", detail, tc.message)
				}
			}
		})
	}
}

func TestAgentDetailsShowTeamSkills(t *testing.T) {
	selected := []string{"npm:@scope/skill", "unpublished-skill", "release-notes", "retired-skill", "pdf-tools"}
	newTeam := func(policy string, skills ...string) *api.Team {
		return &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "research", UID: "team-uid"}, Spec: api.TeamSpec{
			AgentPolicyRef: policy, Skills: skills,
		}}
	}
	// The release of the current selection keeps retired-skill at its last
	// published version; unpublished-skill was never published.
	release := func(uid string, skills []string) api.SkillMarketplaceTeam {
		digest := api.SkillMarketplaceSelectionDigest(skills)
		entry := api.SkillMarketplaceTeam{Name: "research", UID: uid, SelectionDigest: digest, Release: strings.Repeat("d", 20), ReleaseSelectionDigest: digest,
			Retained: []string{"retired-skill"}, Missing: []string{"unpublished-skill"}}
		for _, skill := range []api.SkillMarketplaceReleaseSkill{{Name: "pdf-tools", Version: "0123456789ab"}, {Name: "release-notes", Version: "ba9876543210"}, {Name: "retired-skill", Version: "1111aaaa2222"}} {
			if slices.Contains(skills, skill.Name) {
				entry.Skills = append(entry.Skills, skill)
			}
		}
		return entry
	}
	catalogWith := func(teams ...api.SkillMarketplaceTeam) *corev1.ConfigMap {
		catalog := exampleMarketplaceCatalog()
		catalog.Teams = teams
		return marketplaceCatalogConfigMap(catalog)
	}
	// behind is the release of the current selection from before the
	// repository changed pdf-tools, with the error of the publication after it.
	behind := func(failure string) api.SkillMarketplaceTeam {
		entry := release("team-uid", selected)
		for i := range entry.Skills {
			if entry.Skills[i].Name == "pdf-tools" {
				entry.Skills[i].Version = "aaaaaaaaaaaa"
			}
		}
		entry.Error = failure
		return entry
	}
	pdf := TeamSkill{Name: "pdf-tools", Description: "Read and write PDF files", Version: "0123456789ab", State: "published"}
	notes := TeamSkill{Name: "release-notes", Description: "Draft release notes", Version: "ba9876543210", State: "published"}
	retained := TeamSkill{Name: "retired-skill", Version: "1111aaaa2222", State: "retained"}
	missing := TeamSkill{Name: "unpublished-skill", State: "missing"}
	updating := TeamSkill{Name: "pdf-tools", Description: "Read and write PDF files", Version: "aaaaaaaaaaaa", RepositoryVersion: "0123456789ab", State: "updating"}
	// Before the release of the current selection, the Agent's next start
	// waits for it, so only the repository's version is known.
	pending := func(skill TeamSkill) TeamSkill {
		skill.State = "pending"
		if skill.Description != "" {
			skill.RepositoryVersion = skill.Version
		}
		skill.Version = ""
		return skill
	}
	// The repository has unpublished-skill since the Team's last publication.
	gained := exampleMarketplaceCatalog()
	gained.Skills = append(gained.Skills, api.SkillMarketplaceSkill{Name: "unpublished-skill", Description: "Added later", Version: "eeeeeeeeeeee", Path: "skills/unpublished-skill"})
	gained.Teams = []api.SkillMarketplaceTeam{release("team-uid", selected)}
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		team    *api.Team
		catalog *corev1.ConfigMap
		want    []TeamSkill
		failed  bool
	}{
		{name: "current release", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", selected...),
			catalog: catalogWith(release("team-uid", selected)), want: []TeamSkill{pdf, notes, retained, missing}},
		{name: "a newer repository version not published yet", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", selected...),
			catalog: catalogWith(behind("")), want: []TeamSkill{updating, notes, retained, missing}},
		{name: "the publication of a newer repository version failed", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", selected...),
			catalog: catalogWith(behind("publication failed: synthetic storage error")), want: []TeamSkill{updating, notes, retained, missing}, failed: true},
		{name: "a missing skill that the repository has now", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", selected...),
			catalog: marketplaceCatalogConfigMap(gained),
			want:    []TeamSkill{pdf, notes, retained, {Name: "unpublished-skill", Description: "Added later", RepositoryVersion: "eeeeeeeeeeee", State: "missing"}}},
		{name: "selection not published yet", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", selected...),
			catalog: catalogWith(release("team-uid", []string{"pdf-tools", "retired-skill", "unpublished-skill"})),
			want:    []TeamSkill{pending(pdf), pending(notes), retained, missing}},
		{name: "the publication of the selection failed", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", selected...),
			catalog: catalogWith(func() api.SkillMarketplaceTeam {
				entry := release("team-uid", []string{"pdf-tools", "retired-skill", "unpublished-skill"})
				entry.Error = "publication failed: synthetic storage error"
				return entry
			}()),
			want: []TeamSkill{pending(pdf), pending(notes), retained, missing}, failed: true},
		{name: "release of an earlier Team with the same name", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", selected...),
			catalog: catalogWith(release("earlier-team-uid", selected)), want: []TeamSkill{pending(pdf), pending(notes), pending(retained), pending(missing)}},
		{name: "catalog not published yet", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", selected...),
			want: []TeamSkill{
				{Name: "pdf-tools", State: "pending"}, {Name: "release-notes", State: "pending"},
				{Name: "retired-skill", State: "pending"}, {Name: "unpublished-skill", State: "pending"},
			}},
		{name: "catalog unreadable", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", selected...), catalog: marketplaceCatalogData("{")},
		{name: "disabled", team: newTeam("marketplace-policy", selected...), catalog: catalogWith(release("team-uid", selected))},
		{name: "Team without a policy", cfg: marketplaceConfig(), team: newTeam("", selected...), catalog: catalogWith(release("team-uid", selected))},
		{name: "only npm packages", cfg: marketplaceConfig(), team: newTeam("marketplace-policy", "npm:@scope/skill"), catalog: catalogWith()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := customManagementInstance()
			instance.Spec.Team = tc.team.Name
			objects := []client.Object{instance, tc.team}
			if tc.catalog != nil {
				objects = append(objects, tc.catalog)
			}
			h := &InstanceHandler{Kube: kube.NewClient(newFakeClient(objects...)), Config: tc.cfg}
			w := httptest.NewRecorder()
			h.GetInstanceByName(w, managementRequest(http.MethodGet, ""))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var response InstanceResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(response.TeamSkills, tc.want) {
				t.Fatalf("teamSkills = %+v, want %+v", response.TeamSkills, tc.want)
			}
			if tc.want == nil && strings.Contains(w.Body.String(), "teamSkills") {
				t.Fatalf("response has teamSkills: %s", w.Body.String())
			}
			if response.TeamSkillsPublicationFailed != tc.failed || strings.Contains(w.Body.String(), "teamSkillsPublicationFailed") != tc.failed {
				t.Fatalf("teamSkillsPublicationFailed = %t, want %t: %s", response.TeamSkillsPublicationFailed, tc.failed, w.Body.String())
			}
		})
	}

	// The JSON names every field, also the empty ones, except the repository
	// version and the publication failure, which are there only when set.
	instance := customManagementInstance()
	instance.Spec.Team = "research"
	for catalog, wants := range map[*corev1.ConfigMap][]string{
		catalogWith(release("team-uid", selected)): {
			`"teamSkills":[{"name":"pdf-tools","description":"Read and write PDF files","version":"0123456789ab","state":"published"},`,
			`{"name":"unpublished-skill","description":"","version":"","state":"missing"}]`,
		},
		catalogWith(behind("publication failed: synthetic storage error")): {
			`"teamSkills":[{"name":"pdf-tools","description":"Read and write PDF files","version":"aaaaaaaaaaaa","repositoryVersion":"0123456789ab","state":"updating"},`,
			`"teamSkillsPublicationFailed":true`,
		},
	} {
		h := &InstanceHandler{Kube: kube.NewClient(newFakeClient(instance.DeepCopy(), newTeam("marketplace-policy", selected...), catalog)), Config: marketplaceConfig()}
		w := httptest.NewRecorder()
		h.GetInstanceByName(w, managementRequest(http.MethodGet, ""))
		for _, want := range wants {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("response %s does not contain %s", w.Body.String(), want)
			}
		}
	}
}

func TestAgentDetailsSurviveATeamThatCannotBeRead(t *testing.T) {
	instance := customManagementInstance()
	instance.Spec.Team = "removed-team"
	h := &InstanceHandler{Kube: kube.NewClient(newFakeClient(instance, marketplaceCatalogConfigMap(exampleMarketplaceCatalog()))), Config: marketplaceConfig()}
	w := httptest.NewRecorder()
	h.GetInstanceByName(w, managementRequest(http.MethodGet, ""))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "teamSkills") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestAgentDetailsAskForARestartForANewerTeamRelease(t *testing.T) {
	selected := []string{"pdf-tools", "release-notes"}
	selection := api.SkillMarketplaceSelectionDigest(selected)
	other := "sha256:" + strings.Repeat("f", 64)
	current, earlier := strings.Repeat("d", 20), strings.Repeat("a", 20)
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "research", UID: "team-uid"},
		Spec: api.TeamSpec{AgentPolicyRef: "marketplace-policy", Skills: selected}}
	catalog := func(releaseSelection string) *corev1.ConfigMap {
		published := exampleMarketplaceCatalog()
		published.Teams = []api.SkillMarketplaceTeam{{Name: "research", UID: "team-uid", SelectionDigest: selection,
			Release: current, ReleaseSelectionDigest: releaseSelection,
			Skills: []api.SkillMarketplaceReleaseSkill{{Name: "pdf-tools", Version: "0123456789ab"}, {Name: "release-notes", Version: "ba9876543210"}}}}
		return marketplaceCatalogConfigMap(published)
	}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// pod is a Pod of the Agent whose HOME initializer ended with exit and
	// message, after copying a release of podSelection.
	pod := func(name string, age time.Duration, podSelection, message string, exit int32) client.Object {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cetirun-alice", CreationTimestamp: metav1.NewTime(started.Add(-age))},
			Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init-home-data",
				Env: []corev1.EnvVar{{Name: "CETIRUN_SKILLS_RELEASE_SELECTION", Value: podSelection}}}}},
			Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "init-home-data",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: message}}}}},
		}
	}
	report := func(release string) string { return "cetirun.skills.release=" + release + "\n" }
	deleting := pod("deleting", 0, selection, report(earlier), 0)
	deleting.SetDeletionTimestamp(ptr.To(metav1.NewTime(started)))
	deleting.SetFinalizers([]string{"example.com/hold"})
	for _, tc := range []struct {
		name             string
		releaseSelection string
		pods             []client.Object
		want             bool
	}{
		{"an earlier release", selection, []client.Object{pod("p", 0, selection, report(earlier), 0)}, true},
		{"the current release", selection, []client.Object{pod("p", 0, selection, report(current), 0)}, false},
		{"other lines around the report", selection, []client.Object{pod("p", 0, selection, "note\n"+report(earlier)+"end", 0)}, true},
		{"no report, as from an earlier chart", selection, []client.Object{pod("p", 0, selection, "", 0)}, false},
		{"a malformed report", selection, []client.Object{pod("p", 0, selection, report("not-a-release"), 0)}, false},
		{"a failed initializer", selection, []client.Object{pod("p", 0, selection, report(earlier), 1)}, false},
		// A changed selection restarts the Agent without the owner.
		{"a Pod of another selection", selection, []client.Object{pod("p", 0, other, report(earlier), 0)}, false},
		{"the current selection not published yet", other, []client.Object{pod("p", 0, selection, report(earlier), 0)}, false},
		{"no Pod", selection, nil, false},
		{"the newest Pod has the current release", selection,
			[]client.Object{pod("old", time.Hour, selection, report(earlier), 0), pod("new", 0, selection, report(current), 0)}, false},
		{"the newest Pod has an earlier release", selection,
			[]client.Object{pod("old", time.Hour, selection, report(current), 0), pod("new", 0, selection, report(earlier), 0)}, true},
		{"a Pod being deleted", selection, []client.Object{deleting}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := customManagementInstance()
			instance.Spec.Team = "research"
			objects := []client.Object{instance, team.DeepCopy(), catalog(tc.releaseSelection)}
			for _, object := range tc.pods {
				objects = append(objects, object.DeepCopyObject().(client.Object))
			}
			h := &InstanceHandler{Kube: kube.NewClient(newFakeClient(objects...)), Config: marketplaceConfig()}
			w := httptest.NewRecorder()
			h.GetInstanceByName(w, managementRequest(http.MethodGet, ""))
			var response InstanceResponse
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if response.TeamSkillsRestartNeeded != tc.want || strings.Contains(w.Body.String(), "teamSkillsRestartNeeded") != tc.want {
				t.Fatalf("teamSkillsRestartNeeded = %t, want %t: %s", response.TeamSkillsRestartNeeded, tc.want, w.Body.String())
			}
			if len(response.TeamSkills) != len(selected) {
				t.Fatalf("teamSkills = %+v", response.TeamSkills)
			}
		})
	}
}
