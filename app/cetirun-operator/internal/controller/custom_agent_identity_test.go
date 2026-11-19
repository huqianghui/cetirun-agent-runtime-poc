// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/azure"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"k8s.io/apimachinery/pkg/types"
)

func TestCustomAgentIdentityIsRejectedBeforeProvisioning(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Spec.Agent.Type = api.AgentTypeCustom
	r := &AgentReconciler{}
	bindings := runtimebindings.Custom{}
	ready, err := r.bindCustomAgentIdentity(context.Background(), instance, &bindings)
	if err == nil || ready || !strings.Contains(err.Error(), "Custom does not support Agent Identity") || bindings.AgentIdentity != nil {
		t.Fatalf("Custom identity was not rejected before provisioning: ready=%v binding=%v err=%v", ready, bindings.AgentIdentity, err)
	}
}

func TestCustomAgentIdentityRejectsProviderInjectionAndPreservesOptOut(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Spec.Agent.Type = api.AgentTypeCustom
	instance.Spec.AgentIdentity.Enabled = false
	r := &AgentReconciler{}
	bindings := runtimebindings.Custom{AgentIdentity: &runtimebindings.AgentIdentity{BrokerURL: "http://broker.credentials.svc.cluster.local:8080"}}
	if ready, err := r.bindCustomAgentIdentity(context.Background(), instance, &bindings); err == nil || ready {
		t.Fatal("general provider injected identity access")
	}
	bindings = runtimebindings.Custom{}
	if ready, err := r.bindCustomAgentIdentity(context.Background(), instance, &bindings); err != nil || !ready || bindings.AgentIdentity != nil {
		t.Fatalf("plain Custom deployment did not preserve its identity opt-out: ready=%v err=%v", ready, err)
	}
}

func TestCustomAgentIdentityOptOutCleansExistingBinding(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Spec.Agent.Type = api.AgentTypeCustom
	instance.Spec.AgentIdentity.Enabled = false
	instance.Status.AgentIdentity = &api.AgentIdentityStatus{
		Phase: api.AgentIdentityPhaseReady, ClientID: testAgentIdentityClientID, ObjectID: testAgentIdentityObjectID,
		DisplayName: testAgentIdentityDisplayName(), ObservedGeneration: instance.Generation,
	}
	directory := &fakeAgentIdentityDirectory{byObject: &azure.ServicePrincipal{
		ID: testAgentIdentityObjectID, AppID: testAgentIdentityClientID,
		DisplayName: testAgentIdentityDisplayName(), AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
	}}
	r := newAgentIdentityTestReconciler(t, instance, directory)
	r.AgentIdentityBrokerURL = newSuccessfulAuthorizationCleanupServer(t, r)

	ready, err := r.bindCustomAgentIdentity(context.Background(), instance, &runtimebindings.Custom{})
	if err != nil || !ready || directory.createCalls != 0 || directory.deleteCalls != 1 {
		t.Fatalf("Custom opt-out did not clean the old identity: ready=%v creates=%d deletes=%d err=%v", ready, directory.createCalls, directory.deleteCalls, err)
	}
	stored := &api.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: instance.Name}, stored); err != nil {
		t.Fatal(err)
	}
	status := stored.Status.AgentIdentity
	if status == nil || status.Phase != api.AgentIdentityPhaseDisabled || status.ClientID != "" || status.ObjectID != "" || status.DisplayName != "" {
		t.Fatalf("Custom opt-out did not persist disabled identity status: %#v", status)
	}
}

func TestCustomAgentIdentityOptOutBlocksOnCleanupFailure(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Spec.Agent.Type = api.AgentTypeCustom
	instance.Spec.AgentIdentity.Enabled = false
	instance.Status.AgentIdentity = &api.AgentIdentityStatus{
		Phase: api.AgentIdentityPhaseReady, ClientID: testAgentIdentityClientID, ObjectID: testAgentIdentityObjectID,
		DisplayName: testAgentIdentityDisplayName(), ObservedGeneration: instance.Generation,
	}
	directory := &fakeAgentIdentityDirectory{
		byObject: &azure.ServicePrincipal{ID: testAgentIdentityObjectID, AppID: testAgentIdentityClientID,
			DisplayName: testAgentIdentityDisplayName(), AgentIdentityBlueprintID: testAgentIdentityBlueprintID},
		deleteErr: errors.New("synthetic cleanup failure"),
	}
	r := newAgentIdentityTestReconciler(t, instance, directory)
	r.AgentIdentityBrokerURL = newSuccessfulAuthorizationCleanupServer(t, r)

	ready, err := r.bindCustomAgentIdentity(context.Background(), instance, &runtimebindings.Custom{})
	if err == nil || ready || directory.createCalls != 0 || directory.deleteCalls != 1 {
		t.Fatalf("Custom continued after identity cleanup failed: ready=%v creates=%d deletes=%d err=%v", ready, directory.createCalls, directory.deleteCalls, err)
	}
	stored := &api.Agent{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: instance.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if status := stored.Status.AgentIdentity; status == nil || status.Phase == api.AgentIdentityPhaseDisabled {
		t.Fatalf("failed Custom cleanup was marked disabled: %#v", status)
	}
}
