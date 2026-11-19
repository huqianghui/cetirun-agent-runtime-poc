// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNativeSandboxIgnoresUnselectedConfigurationEdits(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	ctx := context.Background()
	agent := f.current(t)
	team, box, policy := &api.Team{}, &api.Sandbox{}, &api.AgentPolicy{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: agent.Spec.SandboxRef}, box); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: team.Spec.AgentPolicyRef}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.SandboxSelection.AllowMultiple = true
	if err := f.kube.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	unusedBox, otherPolicy := box.DeepCopy(), policy.DeepCopy()
	unusedBox.ObjectMeta = metav1.ObjectMeta{Name: "native-unused-box"}
	otherPolicy.ObjectMeta = metav1.ObjectMeta{Name: "native-unused-policy"}
	for _, object := range []client.Object{unusedBox, otherPolicy} {
		if err := f.kube.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	team.Spec.SandboxRefs = append(team.Spec.SandboxRefs, unusedBox.Name)
	authorization, err := sandboxbinding.Snapshot(agent, team, sandbox.Resources{
		Sandboxes:     map[string]*api.Sandbox{box.Name: box, unusedBox.Name: unusedBox},
		AgentPolicies: map[string]*api.AgentPolicy{policy.Name: policy, otherPolicy.Name: otherPolicy},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.Annotations[sandboxbinding.AuthorizationAnnotation] = authorization
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.ready(t)
	_, expected := f.expectation(t)
	for _, source := range expected.Sources {
		if source.UID == unusedBox.UID || source.UID == otherPolicy.UID {
			t.Fatal("native execution retained an unselected configuration input")
		}
	}
	unusedBox.Spec.Resources.Agent.Limits[corev1.ResourceCPU] = resource.MustParse("3")
	if err := f.kube.Update(ctx, unusedBox); err != nil {
		t.Fatal(err)
	}
	otherPolicy.Spec.ModelAccess.GatewayBudget.DefaultMonthlyUSD = "11"
	if err := f.kube.Update(ctx, otherPolicy); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	f.assertReady(t)
}
