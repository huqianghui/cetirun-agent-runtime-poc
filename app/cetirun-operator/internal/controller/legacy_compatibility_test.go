// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func legacyGatewayFixture(t *testing.T) (*AgentReconciler, *api.Agent, *fakeAIGateway) {
	t.Helper()
	agent := aiGatewayTestAgent("legacy-team")
	agent.Spec.SandboxRef, agent.Spec.ModelAccess, agent.Status.Policy = "", nil, nil
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.Team}}
	r, kube, gateway := aiGatewayTestReconciler(t, agent, team)
	r.AIGateway.CopilotProxyImage = ""
	namespace := aiGatewayNamespace()
	namespace.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(agent, api.GroupVersion.WithKind("Agent"))}
	if err := kube.Create(context.Background(), namespace); err != nil {
		t.Fatal(err)
	}
	return r, agent, gateway
}

func TestLegacyGatewayPreservesModelsBudgetAndKeyIdentity(t *testing.T) {
	r, agent, gateway := legacyGatewayFixture(t)
	ctx := context.Background()
	gateway.shared = append(gateway.shared, aigateway.Deployment{
		ID: "legacy-model", ModelName: "legacy-shared", AccessGroups: []string{aiGatewaySharedAccessGroup},
	})
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || gateway.generated[0].MaxBudget != r.AIGateway.DefaultBudget ||
		!slices.Equal(gateway.generated[0].Models, sortedCopy([]string{aiGatewaySharedAccessGroup, testManagedGroup})) {
		t.Fatalf("legacy grant did not retain both catalog formats and the default budget: %+v", gateway.generated)
	}
	if agent.Status.Policy != nil || agent.Status.AIGateway.PolicyRevision != "" ||
		!slices.Equal(agent.Status.AIGateway.Models, []string{testSharedModelOne, "legacy-shared"}) {
		t.Fatalf("legacy model configuration was replaced by policy requirements: %+v", agent.Status)
	}
	token := aigateway.Token(gateway.generated[0].Key)
	key := gateway.keys[token]
	reset := time.Now().Add(time.Hour).UTC()
	key.MaxBudget, key.Spend, key.BudgetDuration, key.BudgetResetAt = ptr.To(25.0), ptr.To(3.0), "7d", &reset
	for range 2 {
		if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
			t.Fatal(err)
		}
	}
	key = gateway.keys[token]
	if len(gateway.generated) != 1 || *key.MaxBudget != 25 || *key.Spend != 3 ||
		key.BudgetDuration != "7d" || !key.BudgetResetAt.Equal(reset) {
		t.Fatal("legacy reconcile recreated a key or overwrote administrator budget/spend")
	}
	key.Blocked = true
	if err := r.reconcileAIGateway(ctx, agent, true); err == nil || !key.Blocked {
		t.Fatal("legacy compatibility cleared an administrator emergency block")
	}
}

func TestLegacyCompatibilityDoesNotBypassPolicyIntent(t *testing.T) {
	for name, mutate := range map[string]func(*api.Agent){
		"selected sandbox": func(a *api.Agent) { a.Spec.SandboxRef = "missing-sandbox" },
		"policy status":    func(a *api.Agent) { a.Status.Policy = &api.PolicyStatus{Phase: "Pending"} },
		"policy grant":     func(a *api.Agent) { a.Status.AIGateway = &api.AIGatewayStatus{PolicyRevision: "previous"} },
		"held lifetime":    func(a *api.Agent) { a.Finalizers = append(a.Finalizers, sandbox.HoldFinalizer) },
		"policy budget": func(a *api.Agent) {
			a.Spec.ModelAccess = &api.ModelAccessSpec{MonthlyBudgetUSD: "5"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, agent, gateway := legacyGatewayFixture(t)
			mutate(agent)
			required, err := r.agentPolicyRequired(context.Background(), agent)
			if err != nil || !required {
				t.Fatalf("policy intent classified as legacy: %v %v", required, err)
			}
			if _, err := r.resolveAIGatewayGrant(context.Background(), gateway, agent); err == nil || len(gateway.generated) != 0 {
				t.Fatal("an invalid governed binding received a legacy Gateway grant")
			}
		})
	}
	t.Run("incomplete Team configuration", func(t *testing.T) {
		r, agent, gateway := legacyGatewayFixture(t)
		team := &api.Team{}
		if err := r.Get(context.Background(), client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
			t.Fatal(err)
		}
		team.Spec.AgentPolicyRef = "missing-policy"
		if err := r.Update(context.Background(), team); err != nil {
			t.Fatal(err)
		}
		if _, err := r.resolveAIGatewayGrant(context.Background(), gateway, agent); err == nil {
			t.Fatal("partial Team policy silently used a legacy grant")
		}
	})
	t.Run("unreadable Team", func(t *testing.T) {
		r, agent, gateway := legacyGatewayFixture(t)
		r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
				if _, ok := object.(*api.Team); ok {
					return errors.New("synthetic Team read outage")
				}
				return c.Get(ctx, key, object, options...)
			},
		})
		if _, err := r.resolveAIGatewayGrant(context.Background(), gateway, agent); err == nil {
			t.Fatal("Team read failure silently used a legacy grant")
		}
	})
}
