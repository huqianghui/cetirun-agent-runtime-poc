// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type recordingPolicyCoordination struct {
	storage client.Client
	before  client.Object
	after   client.Object
	deleted client.Object
}

func (r *recordingPolicyCoordination) RecoverUpdate(context.Context, client.Object, client.Object) error {
	return nil
}

func (r *recordingPolicyCoordination) Update(ctx context.Context, before, after client.Object) error {
	r.before = before.DeepCopyObject().(client.Object)
	r.after = after.DeepCopyObject().(client.Object)
	return r.storage.Update(ctx, after)
}

func (r *recordingPolicyCoordination) Delete(ctx context.Context, before client.Object) error {
	r.deleted = before.DeepCopyObject().(client.Object)
	return r.storage.Delete(ctx, before)
}

func TestAgentPolicyMutationsUseConfiguredCoordination(t *testing.T) {
	ctx := context.Background()
	policy := &api.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: types.UID("policy-uid")},
		Spec: api.AgentPolicySpec{
			Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}},
			Network:   api.AgentPolicyNetwork{BlockedCIDRs: []string{"10.0.0.0/8"}},
			AgentIdentity: api.AgentPolicyAgentIdentity{
				Allowed:            true,
				Required:           true,
				CredentialServices: []string{"azure-resource-manager"},
			},
			EntraID: api.AgentPolicyEntraID{Enabled: true},
		},
	}
	storage := newFakeClient(policy)
	current := &api.AgentPolicy{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(policy), current); err != nil {
		t.Fatal(err)
	}
	coordination := &recordingPolicyCoordination{storage: storage}
	portal := NewClient(storage)
	portal.SetConfigurationCoordination(coordination)
	desired := current.Spec
	desired.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
	desired.AgentIdentity = api.AgentPolicyAgentIdentity{}
	desired.EntraID = api.AgentPolicyEntraID{}
	updated, err := portal.UpdateAgentPolicySpec(ctx, current.Name, desired, AgentPolicyVersion{
		UID: current.UID, ResourceVersion: current.ResourceVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if coordination.before == nil || coordination.after == nil ||
		coordination.before.GetUID() != current.UID || coordination.before.GetResourceVersion() != current.ResourceVersion ||
		coordination.after.GetUID() != current.UID || coordination.after.GetResourceVersion() != current.ResourceVersion {
		t.Fatal("Portal did not coordinate the exact original policy identity and proposed write")
	}
	if got := updated.Spec.Network.BlockedCIDRs; len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Fatalf("coordinated update discarded an unmanaged field: %+v", updated.Spec)
	}
	if updated.Spec.AgentIdentity.Allowed || updated.Spec.AgentIdentity.Required || len(updated.Spec.AgentIdentity.CredentialServices) != 0 || updated.Spec.EntraID.Enabled {
		t.Fatalf("coordinated update did not apply identity fields: %+v", updated.Spec)
	}
	if err := portal.DeleteAgentPolicy(ctx, updated.Name, AgentPolicyVersion{UID: updated.UID, ResourceVersion: updated.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	if coordination.deleted == nil || coordination.deleted.GetUID() != updated.UID ||
		coordination.deleted.GetResourceVersion() != updated.ResourceVersion {
		t.Fatal("Portal did not coordinate the exact policy deletion")
	}
}

func TestDirectAgentPolicyDeleteUsesBackgroundPropagation(t *testing.T) {
	ctx := context.Background()
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: types.UID("policy-uid")}}
	storage := newFakeClient(policy)
	current := &api.AgentPolicy{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(policy), current); err != nil {
		t.Fatal(err)
	}
	background := false
	wrapped := interceptor.NewClient(storage.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
			options := (&client.DeleteOptions{}).ApplyOptions(opts)
			background = options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationBackground
			return c.Delete(ctx, object, opts...)
		},
	})
	portal := NewClient(wrapped)
	if err := portal.DeleteAgentPolicy(ctx, current.Name, AgentPolicyVersion{UID: current.UID, ResourceVersion: current.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	if !background {
		t.Fatal("AgentPolicy deletion omitted explicit Background propagation")
	}
}
