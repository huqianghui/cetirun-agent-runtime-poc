// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxbinding

import (
	"context"
	"errors"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func bindingFixture(t *testing.T) (*api.Agent, *api.Team, sandbox.Resources) {
	t.Helper()
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "example-agent", UID: "agent-uid", ResourceVersion: "1"},
		Spec:       api.AgentSpec{Team: "example-team", SandboxRef: "example-box"},
	}
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.Team, UID: "team-uid"},
		Spec:       api.TeamSpec{AgentPolicyRef: "example-policy", SandboxRefs: []string{agent.Spec.SandboxRef}, DefaultSandboxRef: agent.Spec.SandboxRef},
	}
	resources := sandbox.Resources{
		Sandboxes: map[string]*api.Sandbox{
			agent.Spec.SandboxRef: {ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.SandboxRef, UID: "box-uid"}},
		},
		AgentPolicies: map[string]*api.AgentPolicy{
			team.Spec.AgentPolicyRef: {ObjectMeta: metav1.ObjectMeta{Name: team.Spec.AgentPolicyRef, UID: "policy-uid"}},
		},
	}
	record, err := Snapshot(agent, team, resources)
	if err != nil {
		t.Fatal(err)
	}
	agent.Annotations = map[string]string{AuthorizationAnnotation: record}
	return agent, team, resources
}

func TestSnapshotPersistsBeforeAgentNameUIDOrSuccessfulApply(t *testing.T) {
	agent, team, resources := bindingFixture(t)
	agent.Name, agent.UID, agent.ResourceVersion, agent.GenerateName = "", "", "", "example-agent-"
	record, err := Snapshot(agent, team, resources)
	if err != nil {
		t.Fatal("creation authorization required an API-server-assigned identity:", err)
	}
	if record != agent.Annotations[AuthorizationAnnotation] {
		t.Fatal("creation metadata must not bind a caller-supplied Agent name or UID")
	}
	if !strings.HasPrefix(record, `{"version":"v1",`) {
		t.Fatalf("authorization must use the v1 format: %s", record)
	}
	if _, err := AuthorizedBinding(agent); err == nil {
		t.Fatal("an unpersisted create request is not authorized for execution")
	}
	agent.Name, agent.UID = "example-agent-abcde", "server-assigned-uid"
	before, err := AuthorizedBinding(agent)
	if err != nil || before.AgentUID != agent.UID {
		t.Fatalf("stored authorization lost the API-assigned Agent UID: %v, %v", before, err)
	}
	agent.Status.Sandbox = &api.SandboxStatus{
		TeamUID: "old-team", SandboxUID: "old-box", AgentPolicyUID: "old-policy", AppliedRevision: "previous-apply",
	}
	after, err := AuthorizedBinding(agent)
	if err != nil || before != after {
		t.Fatalf("last-applied status changed authorization: %v, %v", after, err)
	}
	agent.Status.Sandbox = nil
	after, err = AuthorizedBinding(agent)
	if err != nil || before != after {
		t.Fatal("first successful apply must not be required for durable UID authorization")
	}
}

func TestAuthorizationRejectsMalformedOrDetachedRecords(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*api.Agent)
	}{
		{"missing", func(a *api.Agent) { a.Annotations = nil }},
		{"empty", func(a *api.Agent) { a.Annotations[AuthorizationAnnotation] = "" }},
		{"null", func(a *api.Agent) { a.Annotations[AuthorizationAnnotation] = "null" }},
		{"unknown", func(a *api.Agent) {
			a.Annotations[AuthorizationAnnotation] = strings.Replace(a.Annotations[AuthorizationAnnotation], `"version":`, `"unknown":true,"version":`, 1)
		}},
		{"unsupported v2", func(agent *api.Agent) {
			agent.Annotations[AuthorizationAnnotation] = strings.Replace(agent.Annotations[AuthorizationAnnotation], `"v1"`, `"v2"`, 1)
		}},
		{"unsupported v3", func(agent *api.Agent) {
			agent.Annotations[AuthorizationAnnotation] = strings.Replace(agent.Annotations[AuthorizationAnnotation], `"v1"`, `"v3"`, 1)
		}},
		{"trailing", func(a *api.Agent) { a.Annotations[AuthorizationAnnotation] += " {}" }},
		{"duplicate", func(a *api.Agent) {
			a.Annotations[AuthorizationAnnotation] = strings.Replace(a.Annotations[AuthorizationAnnotation], `"version":`, `"version":"v1","version":`, 1)
		}},
		{"empty UID", func(a *api.Agent) {
			a.Annotations[AuthorizationAnnotation] = strings.Replace(a.Annotations[AuthorizationAnnotation], `"team-uid"`, `""`, 1)
		}},
		{"wildcard UID", func(a *api.Agent) {
			a.Annotations[AuthorizationAnnotation] = strings.Replace(a.Annotations[AuthorizationAnnotation], `"team-uid"`, `"*"`, 1)
		}},
		{"different Team", func(a *api.Agent) { a.Spec.Team = "other-team" }},
		{"different Sandbox", func(a *api.Agent) { a.Spec.SandboxRef = "other-box" }},
		{"unexpected binding field", func(a *api.Agent) {
			a.Annotations[AuthorizationAnnotation] = strings.Replace(a.Annotations[AuthorizationAnnotation], `"agentPolicy":`, `"unexpected":true,"agentPolicy":`, 1)
		}},
		{"no Agent UID", func(a *api.Agent) { a.UID = "" }},
		{"no Agent name", func(a *api.Agent) { a.Name = "" }},
		{"namespaced Agent", func(a *api.Agent) { a.Namespace = "unexpected" }},
		{"terminating Agent", func(a *api.Agent) { now := metav1.Now(); a.DeletionTimestamp = &now }},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent, _, _ := bindingFixture(t)
			test.edit(agent)
			if _, err := AuthorizedBinding(agent); err == nil {
				t.Fatal("invalid or detached authorization was trusted")
			}
		})
	}
	if _, err := AuthorizedBinding(nil); err == nil {
		t.Fatal("nil Agent authorized")
	}
}

func TestCurrentBindingRejectsMissingReplacedOrTerminatingIdentities(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*api.Agent, *api.Team, sandbox.Resources)
	}{
		{"unselected box", func(a *api.Agent, _ *api.Team, _ sandbox.Resources) { a.Spec.SandboxRef = "other" }},
		{"missing policy", func(_ *api.Agent, team *api.Team, _ sandbox.Resources) { team.Spec.AgentPolicyRef = "" }},
		{"missing box", func(a *api.Agent, _ *api.Team, r sandbox.Resources) { delete(r.Sandboxes, a.Spec.SandboxRef) }},
		{"missing policy", func(_ *api.Agent, team *api.Team, r sandbox.Resources) {
			delete(r.AgentPolicies, team.Spec.AgentPolicyRef)
		}},
		{"Team UID absent", func(_ *api.Agent, team *api.Team, _ sandbox.Resources) { team.UID = "" }},
		{"Team name changed", func(_ *api.Agent, team *api.Team, _ sandbox.Resources) { team.Name = "different" }},
		{"policy name changed", func(_ *api.Agent, team *api.Team, r sandbox.Resources) {
			r.AgentPolicies[team.Spec.AgentPolicyRef].Name = "different"
		}},
		{"box name changed", func(a *api.Agent, _ *api.Team, r sandbox.Resources) {
			r.Sandboxes[a.Spec.SandboxRef].Name = "different"
		}},
		{"Team terminating", func(_ *api.Agent, team *api.Team, _ sandbox.Resources) {
			now := metav1.Now()
			team.DeletionTimestamp = &now
		}},
		{"box terminating", func(a *api.Agent, _ *api.Team, r sandbox.Resources) {
			now := metav1.Now()
			r.Sandboxes[a.Spec.SandboxRef].DeletionTimestamp = &now
		}},
		{"policy terminating", func(_ *api.Agent, team *api.Team, r sandbox.Resources) {
			now := metav1.Now()
			r.AgentPolicies[team.Spec.AgentPolicyRef].DeletionTimestamp = &now
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent, team, resources := bindingFixture(t)
			test.edit(agent, team, resources)
			if _, err := CurrentBinding(agent, team, resources); err == nil {
				t.Fatal("invalid current reference accepted")
			}
		})
	}
}

func TestReadKeepsAuthorizedUIDsRatherThanRefreshingReferences(t *testing.T) {
	agent, team, resources := bindingFixture(t)
	original, err := AuthorizedBinding(agent)
	if err != nil {
		t.Fatal(err)
	}
	team.UID = "replacement-team"
	resources.Sandboxes[agent.Spec.SandboxRef].UID = "replacement-box"
	resources.AgentPolicies[team.Spec.AgentPolicyRef].UID = "replacement-policy"
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, team,
		resources.Sandboxes[agent.Spec.SandboxRef], resources.AgentPolicies[team.Spec.AgentPolicyRef]).Build()
	read, binding, err := Read(context.Background(), cl, client.ObjectKeyFromObject(agent))
	if err != nil || binding != original || read.UID != agent.UID {
		t.Fatalf("read lost the durable snapshot: %v, %v", binding, err)
	}
	current, err := CurrentBinding(read, team, resources)
	if err != nil || current == binding {
		t.Fatal("reference replacement silently renewed authorization")
	}
	if _, _, err := Read(context.Background(), nil, client.ObjectKeyFromObject(agent)); err == nil {
		t.Fatal("nil authoritative reader accepted")
	}
	if _, _, err := Read(context.Background(), cl, client.ObjectKey{Name: agent.Name, Namespace: "unexpected"}); err == nil {
		t.Fatal("namespaced lookup accepted")
	}
	if _, _, err := Read(context.Background(), cl, client.ObjectKey{Name: "missing"}); err == nil {
		t.Fatal("missing Agent authorized")
	}
	delete(agent.Annotations, AuthorizationAnnotation)
	if err := cl.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	read, _, err = Read(context.Background(), cl, client.ObjectKeyFromObject(agent))
	if !errors.Is(err, sandbox.ErrUnauthorizedBinding) || read == nil {
		t.Fatalf("missing protected authorization was not reported with the current Agent: %v", err)
	}
}

func TestExplicitReauthorizationFormatIsCompleteAndStrict(t *testing.T) {
	agent, _, _ := bindingFixture(t)
	binding, err := AuthorizedBinding(agent)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := FormatReauthorization("operation-1", binding)
	if err != nil {
		t.Fatal(err)
	}
	request, err := ParseReauthorization(raw)
	if err != nil || request.RequestID != "operation-1" || request.Binding != binding {
		t.Fatalf("request roundtrip failed: %#v, %v", request, err)
	}
	for _, raw := range []string{
		"", "null", "{}", raw + " {}", strings.Replace(raw, `"requestID":`, `"unknown":true,"requestID":`, 1),
		strings.Replace(raw, `"operation-1"`, `""`, 1), strings.Replace(raw, `"agent-uid"`, `"*"`, 1),
		strings.Replace(raw, `"team-uid"`, `""`, 1), strings.Replace(raw, `"binding":`, `"unexpected":true,"binding":`, 1),
	} {
		if _, err := ParseReauthorization(raw); err == nil {
			t.Fatalf("invalid explicit request accepted: %s", raw)
		}
	}
	if _, err := FormatReauthorization("", binding); err == nil {
		t.Fatal("missing request ID accepted")
	}
}

func TestPresentIncludesUntrustedOrMalformedMetadata(t *testing.T) {
	if Present(nil) || Present(&api.Agent{}) {
		t.Fatal("absent metadata acquired policy intent")
	}
	for _, key := range []string{AuthorizationAnnotation, ReauthorizeAnnotation} {
		if !Present(&api.Agent{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{key: ""}}}) {
			t.Fatal("empty protected metadata was overlooked")
		}
	}
}
