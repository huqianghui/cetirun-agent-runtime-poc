// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

func newAdminTeamHandler(objs ...client.Object) *AdminTeamHandler {
	objs = append(objs,
		&cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard-policy"}, Spec: cetirunv1alpha1.AgentPolicySpec{Skills: cetirunv1alpha1.AgentPolicySkills{Allowed: []string{"npm:@scope/skill"}}}},
		&cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "governed-policy"}},
		&cetirunv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "standard"}},
		&cetirunv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "restricted"}})
	for _, obj := range objs {
		if obj.GetUID() == "" {
			obj.SetUID("test-" + types.UID(obj.GetName()))
		}
	}
	c := newFakeClient(objs...)
	return &AdminTeamHandler{Kube: kube.NewClient(c)}
}

func TestAdminTeam_List(t *testing.T) {
	team := &cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "aks"},
		Spec: cetirunv1alpha1.TeamSpec{
			MemberContext: cetirunv1alpha1.TeamContextSpec{Role: "AKS Engineer"},
			Skills:        []string{"@anthropic/mcp-server-fetch"},
		},
	}
	h := newAdminTeamHandler(team)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/teams", nil)
	w := httptest.NewRecorder()
	h.ListTeams(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var resp struct {
		Teams []TeamDTO `json:"teams"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Teams) != 1 || resp.Teams[0].Name != "aks" || resp.Teams[0].Role != "AKS Engineer" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestAdminTeam_Get(t *testing.T) {
	h := newAdminTeamHandler(&cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "aks"},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/teams/aks", nil)
	req.SetPathValue("name", "aks")
	w := httptest.NewRecorder()
	h.GetTeam(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", w.Code, w.Body.String())
	}
}

func TestAdminTeam_Get_NotFound(t *testing.T) {
	h := newAdminTeamHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/teams/missing", nil)
	req.SetPathValue("name", "missing")
	w := httptest.NewRecorder()
	h.GetTeam(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

func TestAdminTeam_Create(t *testing.T) {
	h := newAdminTeamHandler()
	body, _ := json.Marshal(TeamUpsertRequest{
		Name:           "sre",
		AgentPolicyRef: "standard-policy", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard",
		Role:   "SRE",
		Skills: []string{"npm:@scope/skill"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/teams", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateTeam(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d, body=%s", w.Code, w.Body.String())
	}
	var got TeamDTO
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "sre" {
		t.Errorf("unexpected DTO: %+v", got)
	}
}

func TestAdminTeam_Create_InvalidName(t *testing.T) {
	h := newAdminTeamHandler()
	body, _ := json.Marshal(TeamUpsertRequest{Name: "1bad"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/teams", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateTeam(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", w.Code)
	}
}

func TestAdminTeam_WriteErrors(t *testing.T) {
	missingGovernance := apierrors.NewInvalid(schema.GroupKind{Group: cetirunv1alpha1.GroupVersion.Group, Kind: "Team"}, "example-team", field.ErrorList{
		field.Required(field.NewPath("spec", "agentPolicyRef"), ""),
		field.Required(field.NewPath("spec", "defaultSandboxRef"), ""),
		field.Required(field.NewPath("spec", "sandboxRefs"), ""),
	})
	invalidSkill := apierrors.NewInvalid(schema.GroupKind{Kind: "Team"}, "example-team", field.ErrorList{
		field.Invalid(field.NewPath("spec", "skills").Index(0), "sensitive-rejected-value", "sensitive-admission-detail"),
		field.Invalid(field.NewPath("metadata", "annotations").Key("sensitive-map-key"), "value", "detail"),
	})
	tests := []struct {
		name     string
		err      error
		status   int
		code     string
		contains []string
	}{
		{
			name: "required governance", err: missingGovernance, status: http.StatusUnprocessableEntity, code: "invalid_team",
			contains: []string{"spec.agentPolicyRef: Required value", "spec.defaultSandboxRef: Required value", "spec.sandboxRefs: Required value", "Check the Team fields"},
		},
		{
			name: "invalid skill", err: invalidSkill, status: http.StatusUnprocessableEntity, code: "invalid_team",
			contains: []string{"spec.skills[0]: Invalid value", "Check the Team fields"},
		},
		{
			name: "invalid without details", err: &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonInvalid, Message: "sensitive-status-message", Code: 422}},
			status: http.StatusUnprocessableEntity, code: "invalid_team", contains: []string{"Team configuration was rejected", "Portal logs"},
		},
		{
			name: "unknown failure", err: errors.New("sensitive-transport-detail"), status: http.StatusInternalServerError, code: "internal_error",
			contains: []string{"Unable to"},
		},
		{
			name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Resource: "teams"}, "example-team", errors.New("sensitive-rbac-detail")),
			status: http.StatusForbidden, code: "rbac_forbidden", contains: []string{"Check ClusterRole bindings"},
		},
		{
			name: "admission forbidden", err: &apierrors.StatusError{ErrStatus: metav1.Status{
				Reason: metav1.StatusReasonForbidden, Code: 403,
				Message: `admission webhook "sandbox-configuration.cetirun.microsoft.com" denied the request: sensitive-admission-detail`,
			}},
			status: http.StatusForbidden, code: "admission_rejected", contains: []string{"Policy and Sandbox prerequisites", "Portal logs"},
		},
		{
			name: "wrapped admission forbidden", err: fmt.Errorf("create Team: %w", &apierrors.StatusError{ErrStatus: metav1.Status{
				Reason: metav1.StatusReasonForbidden, Code: 403,
				Message: `admission webhook "synthetic.example.com" denied the request: sensitive-admission-detail`,
			}}),
			status: http.StatusForbidden, code: "admission_rejected", contains: []string{"Policy and Sandbox prerequisites", "Portal logs"},
		},
	}
	for _, operation := range []string{"create", "update"} {
		for _, tt := range tests {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				base := newFakeClient(
					&cetirunv1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "example-team", UID: "team-uid"}},
					&cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard"}},
					&cetirunv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "standard"}},
				)
				c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return tt.err },
					Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
						return tt.err
					},
				})
				h := &AdminTeamHandler{Kube: kube.NewClient(c)}
				current, err := h.Kube.GetTeam(context.Background(), "example-team")
				if err != nil {
					t.Fatal(err)
				}
				body, _ := json.Marshal(TeamUpsertRequest{Name: "example-team", Role: "Engineer", UID: string(current.UID), ResourceVersion: current.ResourceVersion, AgentPolicyRef: "standard", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard"})
				w := httptest.NewRecorder()
				if operation == "create" {
					h.CreateTeam(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/teams", bytes.NewReader(body)))
				} else {
					r := httptest.NewRequest(http.MethodPut, "/api/v1/admin/teams/example-team", bytes.NewReader(body))
					r.SetPathValue("name", "example-team")
					h.UpdateTeam(w, r)
				}
				if w.Code != tt.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
				}
				var response struct {
					Error struct{ Code, Message string }
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.Error.Code != tt.code {
					t.Errorf("code = %q, want %q", response.Error.Code, tt.code)
				}
				for _, want := range tt.contains {
					if !strings.Contains(response.Error.Message, want) {
						t.Errorf("message %q does not contain %q", response.Error.Message, want)
					}
				}
				if strings.Contains(w.Body.String(), "sensitive-") {
					t.Errorf("response exposed raw error details: %s", w.Body.String())
				}
			})
		}
	}
}

func TestAdminTeam_Create_InvalidSkillStorage(t *testing.T) {
	h := newAdminTeamHandler()
	// skillStorage present but missing required containerName
	body, _ := json.Marshal(TeamUpsertRequest{
		Name: "sre",
		SkillStorage: &TeamSkillStorageDTO{
			ResourceGroup:  "rg",
			StorageAccount: "sa",
			// ContainerName intentionally empty
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/teams", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateTeam(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestAdminTeam_Update_InvalidSkillStorage(t *testing.T) {
	h := newAdminTeamHandler(&cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "aks"},
	})
	body, _ := json.Marshal(TeamUpsertRequest{
		SkillStorage: &TeamSkillStorageDTO{ResourceGroup: " "}, // whitespace-only
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/teams/aks", bytes.NewReader(body))
	req.SetPathValue("name", "aks")
	w := httptest.NewRecorder()
	h.UpdateTeam(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestAdminTeam_Update(t *testing.T) {
	h := newAdminTeamHandler(&cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "aks"},
		Spec: cetirunv1alpha1.TeamSpec{
			MemberContext: cetirunv1alpha1.TeamContextSpec{Role: "old"},
		},
	})
	current, _ := h.Kube.GetTeam(context.Background(), "aks")
	body, _ := json.Marshal(TeamUpsertRequest{Role: "new", UID: string(current.UID), ResourceVersion: current.ResourceVersion,
		AgentPolicyRef: "standard-policy", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/teams/aks", bytes.NewReader(body))
	req.SetPathValue("name", "aks")
	w := httptest.NewRecorder()
	h.UpdateTeam(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", w.Code, w.Body.String())
	}
	var got TeamDTO
	_ = json.NewDecoder(w.Body).Decode(&got)
	if got.Role != "new" {
		t.Errorf("role = %q, want new", got.Role)
	}
}

func TestAdminTeamUpdatePersistsSelectedGovernance(t *testing.T) {
	team := &cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "governed"},
		Spec: cetirunv1alpha1.TeamSpec{
			AgentPolicyRef: "governed-policy",
			SandboxRefs:    []string{"standard", "restricted"}, DefaultSandboxRef: "standard",
			MemberContext: cetirunv1alpha1.TeamContextSpec{Role: "old"},
		},
	}
	h := newAdminTeamHandler(team)
	current, _ := h.Kube.GetTeam(context.Background(), team.Name)
	body, _ := json.Marshal(TeamUpsertRequest{Role: "new", UID: string(current.UID), ResourceVersion: current.ResourceVersion,
		AgentPolicyRef: "standard-policy", SandboxRefs: []string{"restricted"}, DefaultSandboxRef: "restricted"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/teams/governed", bytes.NewReader(body))
	req.SetPathValue("name", team.Name)
	w := httptest.NewRecorder()
	h.UpdateTeam(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got, err := h.Kube.GetTeam(context.Background(), team.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.AgentPolicyRef != "standard-policy" || len(got.Spec.SandboxRefs) != 1 || got.Spec.SandboxRefs[0] != "restricted" || got.Spec.DefaultSandboxRef != "restricted" || got.Spec.MemberContext.Role != "new" {
		t.Fatalf("binding update lost fields: %+v", got.Spec)
	}
}

func TestAdminTeam_Delete(t *testing.T) {
	h := newAdminTeamHandler(&cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "aks"},
	})
	current, _ := h.Kube.GetTeam(context.Background(), "aks")
	body, _ := json.Marshal(map[string]string{"uid": string(current.UID), "resourceVersion": current.ResourceVersion})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/teams/aks", bytes.NewReader(body))
	req.SetPathValue("name", "aks")
	w := httptest.NewRecorder()
	h.DeleteTeam(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status %d, want 204", w.Code)
	}
}

func TestAdminTeamRejectsIncompleteOrUnavailableBindings(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*TeamUpsertRequest)
	}{
		{"missing policy", func(r *TeamUpsertRequest) { r.AgentPolicyRef = "" }},
		{"missing Sandboxes", func(r *TeamUpsertRequest) { r.SandboxRefs = nil }},
		{"invalid default", func(r *TeamUpsertRequest) { r.DefaultSandboxRef = "restricted" }},
		{"duplicate Sandbox", func(r *TeamUpsertRequest) { r.SandboxRefs = []string{"standard", "standard"} }},
		{"unknown policy", func(r *TeamUpsertRequest) { r.AgentPolicyRef = "missing" }},
		{"unknown Sandbox", func(r *TeamUpsertRequest) { r.SandboxRefs = []string{"missing"}; r.DefaultSandboxRef = "missing" }},
		{"forbidden Skill", func(r *TeamUpsertRequest) { r.Skills = []string{"unapproved"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAdminTeamHandler()
			request := TeamUpsertRequest{Name: "example", AgentPolicyRef: "standard-policy", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard"}
			test.change(&request)
			body, _ := json.Marshal(request)
			w := httptest.NewRecorder()
			h.CreateTeam(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/teams", bytes.NewReader(body)))
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("unexpected result: %d %s", w.Code, w.Body.String())
			}
			_, err := h.Kube.GetTeam(context.Background(), "example")
			if !kube.IsNotFound(err) {
				t.Fatal("invalid binding created a Team")
			}
		})
	}
}

func TestAdminTeamRequiresPolicyForMultipleSandboxes(t *testing.T) {
	request := TeamUpsertRequest{
		Name: "example", AgentPolicyRef: "standard-policy", SandboxRefs: []string{"standard", "restricted"}, DefaultSandboxRef: "standard",
	}
	body, _ := json.Marshal(request)
	h := newAdminTeamHandler()
	w := httptest.NewRecorder()
	h.CreateTeam(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/teams", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "allows only one Sandbox") {
		t.Fatalf("single-sandbox policy accepted multiple refs: %d %s", w.Code, w.Body.String())
	}

	allowMultiple := &cetirunv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-policy"},
		Spec:       cetirunv1alpha1.AgentPolicySpec{SandboxSelection: cetirunv1alpha1.SandboxSelectionPolicy{AllowMultiple: true}},
	}
	h = newAdminTeamHandler(allowMultiple)
	request.AgentPolicyRef = "multi-policy"
	body, _ = json.Marshal(request)
	w = httptest.NewRecorder()
	h.CreateTeam(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/teams", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("multiple-sandbox policy rejected multiple refs: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminTeamRequiresCurrentVersion(t *testing.T) {
	for _, version := range []string{"", "stale"} {
		h := newAdminTeamHandler(&cetirunv1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "example"}})
		current, _ := h.Kube.GetTeam(context.Background(), "example")
		request := TeamUpsertRequest{Name: current.Name, UID: string(current.UID), ResourceVersion: version,
			AgentPolicyRef: "standard-policy", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard"}
		body, _ := json.Marshal(request)
		r := httptest.NewRequest(http.MethodPut, "/api/v1/admin/teams/example", bytes.NewReader(body))
		r.SetPathValue("name", "example")
		w := httptest.NewRecorder()
		h.UpdateTeam(w, r)
		want := http.StatusConflict
		if version == "" {
			want = http.StatusPreconditionRequired
		}
		if w.Code != want {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}
