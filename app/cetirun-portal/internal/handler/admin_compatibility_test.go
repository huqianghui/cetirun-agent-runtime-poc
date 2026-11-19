// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func compatibilityPortalFixture() (*api.Team, *api.AgentPolicy, *api.Sandbox) {
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "research", UID: "team-uid", Generation: 2},
		Spec: api.TeamSpec{AgentPolicyRef: "standard", SandboxRefs: []string{"small"}, DefaultSandboxRef: "small"}}
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "policy-uid", Generation: 3}}
	box := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "small", UID: "sandbox-uid", Generation: 4}}
	now := time.Now().Truncate(time.Second)
	team.Status.Compatibility = &api.TeamCompatibilityStatus{
		State: api.CompatibilityCompatible, Team: api.CompatibilityReference{Name: team.Name, UID: string(team.UID), Generation: team.Generation},
		Policy: api.CompatibilityReference{Name: policy.Name, UID: string(policy.UID), Generation: policy.Generation},
		Sandboxes: []api.SandboxCompatibilityStatus{{State: api.CompatibilityCompatible,
			Sandbox: api.CompatibilityReference{Name: box.Name, UID: string(box.UID), Generation: box.Generation}}},
		CheckedAt: metav1.NewTime(now), ValidUntil: metav1.NewTime(now.Add(30 * time.Second)),
	}
	return team, policy, box
}

func TestAdminTeamCompatibilityCurrentReferences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*api.Team, *api.AgentPolicy, *api.Sandbox)
		want   api.CompatibilityState
	}{
		{"current", func(*api.Team, *api.AgentPolicy, *api.Sandbox) {}, api.CompatibilityCompatible},
		{"policy edited", func(_ *api.Team, p *api.AgentPolicy, _ *api.Sandbox) { p.Generation++ }, api.CompatibilityUnknown},
		{"policy replaced", func(_ *api.Team, p *api.AgentPolicy, _ *api.Sandbox) { p.UID = "replacement" }, api.CompatibilityUnknown},
		{"sandbox edited", func(_ *api.Team, _ *api.AgentPolicy, b *api.Sandbox) { b.Generation++ }, api.CompatibilityUnknown},
		{"sandbox replaced", func(_ *api.Team, _ *api.AgentPolicy, b *api.Sandbox) { b.UID = "replacement" }, api.CompatibilityUnknown},
		{"team edited", func(t *api.Team, _ *api.AgentPolicy, _ *api.Sandbox) { t.Generation++ }, api.CompatibilityUnknown},
		{"expired", func(t *api.Team, _ *api.AgentPolicy, _ *api.Sandbox) {
			t.Status.Compatibility.ValidUntil = metav1.NewTime(time.Now().Add(-time.Second))
		}, api.CompatibilityUnknown},
		{"future observation", func(t *api.Team, _ *api.AgentPolicy, _ *api.Sandbox) {
			t.Status.Compatibility.CheckedAt = metav1.NewTime(time.Now().Add(time.Minute))
		}, api.CompatibilityUnknown},
		{"unbounded lifetime", func(t *api.Team, _ *api.AgentPolicy, _ *api.Sandbox) {
			t.Status.Compatibility.ValidUntil = metav1.NewTime(time.Now().Add(time.Hour))
		}, api.CompatibilityUnknown},
		{"sandbox selection changed", func(t *api.Team, _ *api.AgentPolicy, _ *api.Sandbox) {
			t.Spec.SandboxRefs = append(t.Spec.SandboxRefs, "large")
		}, api.CompatibilityUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			team, policy, box := compatibilityPortalFixture()
			tc.change(team, policy, box)
			h := &AdminTeamHandler{Kube: kube.NewClient(newFakeClient(team, policy, box))}
			for _, list := range []bool{false, true} {
				request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/teams/research", nil)
				request.SetPathValue("name", team.Name)
				response := httptest.NewRecorder()
				var dto TeamDTO
				if list {
					h.ListTeams(response, request)
					var body struct {
						Teams []TeamDTO `json:"teams"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Teams) != 1 {
						t.Fatalf("list: %s", response.Body.String())
					}
					dto = body.Teams[0]
				} else {
					h.GetTeam(response, request)
					if err := json.Unmarshal(response.Body.Bytes(), &dto); err != nil {
						t.Fatal(err)
					}
				}
				if response.Code != http.StatusOK || dto.Compatibility == nil || dto.Compatibility.State != tc.want {
					t.Fatalf("status: %d; report: %+v", response.Code, dto.Compatibility)
				}
				if tc.want == api.CompatibilityUnknown && dto.Compatibility.Sandboxes[0].State != api.CompatibilityUnknown {
					t.Fatal("stale child remained compatible")
				}
			}
			if team.Status.Compatibility.State != api.CompatibilityCompatible {
				t.Fatal("response mutated source status")
			}
		})
	}
}

func TestAdminTeamCompatibilityUnavailableDoesNotHideTeams(t *testing.T) {
	team, policy, box := compatibilityPortalFixture()
	storage := interceptor.NewClient(newFakeClient(team, policy, box).(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*api.AgentPolicyList); ok {
			return errors.New("synthetic unavailable source")
		}
		return c.List(ctx, list, opts...)
	}})
	h := &AdminTeamHandler{Kube: kube.NewClient(storage)}
	response := httptest.NewRecorder()
	h.ListTeams(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/teams", nil))
	var body struct {
		Teams []TeamDTO `json:"teams"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(body.Teams) != 1 || body.Teams[0].Compatibility.Reason != "DependenciesUnavailable" {
		t.Fatalf("response: %s", response.Body.String())
	}
}

func compatibilityAdmissionError(t *testing.T) error {
	t.Helper()
	message, _ := api.CompatibilityIssueMessage("sandbox_incompatible")
	issue := api.CompatibilityIssue{Code: "sandbox_incompatible", Field: "spec.network.blockedPorts[0].protocol", Message: message,
		Team: &api.CompatibilityReference{Name: "research"}, Sandbox: &api.CompatibilityReference{Name: "small"}}
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	return &apierrors.StatusError{ErrStatus: metav1.Status{Code: 403, Reason: metav1.StatusReasonForbidden,
		Message: "sensitive-webhook-internals", Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{Type: "CetiRunCompatibility", Field: issue.Field, Message: string(raw)}}}}}
}

func TestAdminCompatibilityErrorDetails(t *testing.T) {
	err := compatibilityAdmissionError(t)
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeConfigurationError(response, "update", kind, "example", err)
			var body ErrorResponse
			if e := json.Unmarshal(response.Body.Bytes(), &body); e != nil {
				t.Fatal(e)
			}
			if response.Code != 422 || body.Error.Code != "configuration_incompatible" || len(body.Error.Details) != 1 ||
				strings.Contains(response.Body.String(), "sensitive-webhook-internals") {
				t.Fatalf("response: %s", response.Body.String())
			}
		})
	}
}

func TestAdminPolicyCreateReturnsCompatibilityDetails(t *testing.T) {
	storage := interceptor.NewClient(newFakeClient().(client.WithWatch), interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return compatibilityAdmissionError(t)
	}})
	h := &AdminPolicyHandler{Kube: kube.NewClient(storage)}
	raw, _ := json.Marshal(validPolicyRequest())
	response := httptest.NewRecorder()
	h.CreatePolicy(response, httptest.NewRequest(http.MethodPost, "/api/v1/admin/policies", bytes.NewReader(raw)))
	if response.Code != 422 || !strings.Contains(response.Body.String(), "configuration_incompatible") || !strings.Contains(response.Body.String(), "research") {
		t.Fatalf("response: %s", response.Body.String())
	}
}

func TestAdminCompatibilityPreservesAPIConflict(t *testing.T) {
	err := compatibilityAdmissionError(t).(*apierrors.StatusError)
	err.ErrStatus.Code = http.StatusConflict
	err.ErrStatus.Reason = metav1.StatusReasonConflict
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeConfigurationError(response, "update", kind, "example", err)
			if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "configuration_incompatible") {
				t.Fatalf("conflict became a compatibility rejection: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestAdminCompatibilityIgnoresUntrustedDetails(t *testing.T) {
	for _, change := range []func(*metav1.StatusCause){
		func(c *metav1.StatusCause) { c.Type = "Unknown" },
		func(c *metav1.StatusCause) {
			c.Message = strings.Replace(c.Message, "sandbox_incompatible", "unknown-code", 1)
		},
		func(c *metav1.StatusCause) {
			c.Message = strings.Replace(c.Message, "The selected Sandbox does not support one or more requested Policy controls.", "sensitive message", 1)
		},
		func(c *metav1.StatusCause) { c.Message += " {}" },
		func(c *metav1.StatusCause) { c.Field = "private-unrelated-field" },
		func(c *metav1.StatusCause) { c.Message = strings.Repeat("x", 4097) },
	} {
		err := compatibilityAdmissionError(t).(*apierrors.StatusError)
		change(&err.ErrStatus.Details.Causes[0])
		if details := compatibilityErrorDetails(err); len(details) != 0 {
			t.Fatalf("accepted malformed details: %+v", details)
		}
	}
}
