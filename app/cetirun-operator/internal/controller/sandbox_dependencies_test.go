// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func TestSandboxConfigurationWatchesIncludePendingBindings(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "research"},
		Spec: api.TeamSpec{
			SandboxRefs: []string{"kata"}, DefaultSandboxRef: "kata", AgentPolicyRef: "member",
		},
	}
	otherTeam := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "other"},
		Spec:       api.TeamSpec{SandboxRefs: []string{"other"}, AgentPolicyRef: "other"},
	}
	agent := func(name, team, sandbox string) *api.Agent {
		return &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.AgentSpec{Team: team, SandboxRef: sandbox}}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&api.Agent{}, instanceTeamIndex, func(object client.Object) []string {
			return []string{object.(*api.Agent).Spec.Team}
		}).
		WithIndex(&api.Agent{}, instanceSandboxIndex, agentSandboxReferences).
		WithIndex(&api.Team{}, teamSandboxIndex, teamSandboxReferences).
		WithIndex(&api.Team{}, teamPolicyIndex, teamPolicyReferences).
		WithObjects(team, otherTeam,
			agent("selected", "research", "kata"),
			agent("pending-default", "research", ""),
			agent("direct-binding", "", "kata"),
			agent("unrelated", "other", "other"),
		).Build()
	r := &AgentReconciler{Client: c}
	for _, test := range []struct {
		name string
		run  func() []reconcile.Request
		want []string
	}{
		{"sandbox", func() []reconcile.Request {
			return r.enqueueInstancesForSandbox(context.Background(), &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "kata"}})
		}, []string{"direct-binding", "pending-default", "selected"}},
		{"policy", func() []reconcile.Request {
			return r.enqueueInstancesForAgentPolicy(context.Background(), &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "member"}})
		}, []string{"pending-default", "selected"}},
		{"unexpected-type", func() []reconcile.Request {
			return r.enqueueInstancesForSandbox(context.Background(), team)
		}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got []string
			for _, request := range test.run() {
				got = append(got, request.Name)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("enqueued Agents %v, want %v", got, test.want)
			}
		})
	}
}

func TestSandboxDependencyIndexesDoNotMutateResources(t *testing.T) {
	team := &api.Team{Spec: api.TeamSpec{
		SandboxRefs: []string{"z", "a", "z", ""}, DefaultSandboxRef: "default",
		AgentPolicyRef: "shared",
	}}
	before := team.DeepCopy()
	if got := teamSandboxReferences(team); !reflect.DeepEqual(got, []string{"a", "default", "z"}) {
		t.Fatalf("Sandbox index = %v", got)
	}
	if got := teamPolicyReferences(team); !reflect.DeepEqual(got, []string{"shared"}) {
		t.Fatalf("policy index = %v", got)
	}
	if !reflect.DeepEqual(team, before) {
		t.Fatal("index extraction mutated a cached Team")
	}
}
