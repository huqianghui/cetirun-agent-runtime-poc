// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/utils/ptr"
)

func TestCustomSuspensionPreservesGatewayKeyAndSpend(t *testing.T) {
	p, agent, gateway, b := gatewayCustomFixture(t)
	r := p.reconciler
	ctx := context.Background()
	if err := r.bindCustomGateway(ctx, agent, &b); err != nil {
		t.Fatal(err)
	}
	keyID := agent.Status.AIGateway.KeyID
	for _, key := range gateway.keys {
		key.Spend = ptr.To(4.25)
	}
	agent.Spec.Suspended = true
	if err := r.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := r.suspendGatewayKey(ctx, agent); err != nil {
		t.Fatal(err)
	}
	for _, key := range gateway.keys {
		if !key.Blocked || *key.Spend != 4.25 {
			t.Fatal("suspension replaced accounting or did not block key")
		}
	}
	agent.Spec.Suspended = false
	if err := r.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := r.bindCustomGateway(ctx, agent, &b); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || agent.Status.AIGateway.KeyID != keyID || agent.Status.AIGateway.SpendUSD != "4.25" {
		t.Fatal("resume replaced Gateway accounting")
	}
}

func TestGatewayRequiredStopsOwnedNativeWorkload(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		t.Run(string(kind), func(t *testing.T) {
			r, agent, _ := legacyGatewayFixture(t)
			agent.Spec.Agent.Type = kind
			workload := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: instanceNamespace(agent.Name),
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(agent, api.GroupVersion.WithKind("Agent"))}},
				Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
			}
			ctx := context.Background()
			if err := r.Create(ctx, workload); err != nil {
				t.Fatal(err)
			}
			if _, err := r.gatewayRuntimeWaiting(ctx, agent, "AIGatewayRequired"); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(workload), workload); err != nil {
				t.Fatal(err)
			}
			if *workload.Spec.Replicas != 0 || agent.Status.Phase != api.PhasePending || agent.Status.AccessURL != "" {
				t.Fatal("missing Gateway did not hold execution")
			}
		})
	}
}

func TestGatewayDisabledDoesNotInstallNativeWorkload(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		t.Run(string(kind), func(t *testing.T) {
			r, agent, gateway := legacyGatewayFixture(t)
			agent.Spec.Agent.Type = kind
			if err := r.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			r.AIGateway.Enabled = false
			chart := &chartHelm{}
			r.HelmClient = chart
			for range 3 {
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}); err != nil {
					t.Fatal(err)
				}
			}
			if chart.installs != 0 || len(gateway.generated) != 0 {
				t.Fatal("disabled Gateway allowed workload installation")
			}
		})
	}
}
