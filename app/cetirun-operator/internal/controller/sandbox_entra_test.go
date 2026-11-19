// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEntraNativePolicyHeldBeforeProvisioning(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			f, input := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false)
			policy := &api.AgentPolicy{}
			ctx := context.Background()
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(input.Policy), policy); err != nil {
				t.Fatal(err)
			}
			policy.Spec.EntraID.Enabled = true
			policy.Generation++
			if err := f.kube.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			// A current Accepted status alone cannot grant missing runtime support.
			policy.Status.ObservedGeneration = policy.Generation
			for i := range policy.Status.Conditions {
				policy.Status.Conditions[i].ObservedGeneration = policy.Generation
			}
			if err := f.kube.Status().Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				_, _ = f.reconcile()
			}
			f.assertNotReady(t)
			if f.installer.stages != 0 {
				t.Fatal("unsupported Entra policy reached common workload apply")
			}
			key := client.ObjectKey{Namespace: instanceNamespace(f.agent.Name), Name: instanceServiceName(f.agent.Name)}
			if err := f.kube.Get(ctx, key, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
				t.Fatal("unsupported Entra policy provisioned a workload:", err)
			}
		})
	}
}

func TestEntraNativeInvalidRegistrationHoldPreservesLifetime(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true)
			workload := f.workload(t)
			workload.Status.ObservedGeneration = workload.Generation
			workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
			if err := f.kube.Status().Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			expected := reconciledExpectationForTest(t, f)
			agent := f.current(t)
			keyID, stages := agent.Status.AIGateway.KeyID, f.installer.stages
			configuration := &corev1.ConfigMap{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: "sandbox-platform"}, configuration); err != nil {
				t.Fatal(err)
			}
			original := configuration.Data[SandboxPlatformConfigurationKey]
			var document SandboxPlatformConfiguration
			if err := json.Unmarshal([]byte(original), &document); err != nil {
				t.Fatal(err)
			}
			registration := document.Registrations[api.SandboxKata]
			registration.Capabilities.EntraID = true
			document.Registrations[api.SandboxKata] = registration
			platform := document.Platforms[api.SandboxKata]
			platform.KubernetesAudience = "https://kubernetes.default.svc"
			platform.Identities = []sandboxpolicy.ApprovedIdentity{{
				AgentUID: agent.UID, Namespace: expected.Namespace.Name, ServiceAccountName: "example-entra",
				Revision: "synthetic-unqualified", Kubernetes: true, Entra: &sandboxpolicy.EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"},
			}}
			document.Platforms[api.SandboxKata] = platform
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			configuration.Data[SandboxPlatformConfigurationKey] = string(raw)
			if err := f.kube.Update(ctx, configuration); err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.SandboxPreparation.Configuration.Snapshot(ctx); err == nil {
				t.Fatal("unqualified Entra registration was accepted by the platform source")
			}
			f.reconcileOK(t)
			current := f.current(t)
			if !slices.Contains(current.Finalizers, sandbox.HoldFinalizer) ||
				current.Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 || f.installer.stages != stages ||
				ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("identity registration drift did not hold the existing lifetime without reapplying")
			}
			f.assertNotReady(t)
			home := &corev1.PersistentVolumeClaim{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Home.Name}, home); err != nil ||
				home.UID != expected.Home.UID || !home.DeletionTimestamp.IsZero() {
				t.Fatal("identity hold replaced the retained HOME:", err)
			}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: "example-entra"},
				&corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
				t.Fatal("unsupported registration provisioned an identity:", err)
			}
			for _, key := range f.gateway.keys {
				if !key.Blocked {
					t.Fatal("identity hold left the existing Gateway key active")
				}
			}
			configuration.Data[SandboxPlatformConfigurationKey] = original
			if err := f.kube.Update(ctx, configuration); err != nil {
				t.Fatal(err)
			}
			workload = f.workload(t)
			workload.Status.ObservedGeneration, workload.Status.Replicas = workload.Generation, 0
			if err := f.kube.Status().Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				f.reconcileOK(t)
			}
			current = f.current(t)
			if slices.Contains(current.Finalizers, sandbox.HoldFinalizer) || current.Status.AIGateway.KeyID != keyID ||
				len(f.gateway.generated) != 1 || reconciledExpectationForTest(t, f).Home != expected.Home ||
				ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 {
				t.Fatal("restoring ordinary configuration did not preserve the owned lifetime")
			}
		})
	}
}
