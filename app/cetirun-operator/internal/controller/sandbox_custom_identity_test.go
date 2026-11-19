// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func withCustomConnections(t *testing.T) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		withNativeConnections(t)(r, kube)
		r.ConnectionsMCPRuntimeTypes = []string{"custom"}
		r.AgentIdentityBrokerURL = newSuccessfulAuthorizationCleanupServer(t, r)
	}
}

func TestCommonCustomIdentityLifecycle(t *testing.T) {
	for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		t.Run(string(backend), func(t *testing.T) {
			setup := []func(*AgentReconciler, client.Client){
				withCommonCustom(t, "headless"), withCustomConnections(t),
			}
			if backend == api.SandboxContainer {
				setup = append(setup, withContainerRuntime(t))
			}
			f := nativeEntraFixture(t, "custom", setup...)
			ctx := context.Background()
			before := reconciledExpectationForTest(t, f)
			key := f.current(t).Status.AIGateway.KeyID
			check := func() {
				t.Helper()
				current := reconciledExpectationForTest(t, f)
				if current.IdentityServiceAccount.UID != before.IdentityServiceAccount.UID || current.Home != before.Home ||
					current.Namespace != before.Namespace || f.current(t).Status.AIGateway.KeyID != key {
					t.Fatal("Custom identity transition replaced retained identity, HOME or accounting")
				}
				contract := &w.ConnectionsMCP{URL: f.r.ConnectionsMCPURL, Namespace: "cetirun-tools",
					Audience: f.r.ConnectionsMCPAudience, CASecretName: f.r.ConnectionsMCPCASecretName}
				if err := contract.ValidatePod(f.workload(t).Spec.Template.Spec); err != nil {
					t.Fatal(err)
				}
			}
			for _, request := range []string{"request-00000011", "request-00000012"} {
				agent := f.current(t)
				agent.Spec.RestartRequestID, agent.Generation = request, agent.Generation+1
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				check()
				if f.workload(t).Spec.Template.Annotations[api.AnnotationRestartRequestID] != request {
					t.Fatal("Custom identity restart did not update the retained template")
				}
			}
			for range 2 {
				agent := f.current(t)
				agent.Spec.Suspended, agent.Generation = true, agent.Generation+1
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				acknowledgeEntraStop(t, f)
				if !f.current(t).Status.AIGateway.Blocked {
					t.Fatal("Custom suspension did not block its existing Gateway key")
				}
				agent = f.current(t)
				agent.Spec.Suspended, agent.Generation = false, agent.Generation+1
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for range 4 {
					f.reconcileOK(t)
				}
				check()
				if ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
					t.Fatal("Custom identity resume did not restore compute")
				}
			}
			agent := f.current(t)
			agent.Spec.Suspended, agent.Generation = true, agent.Generation+1
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			acknowledgeEntraStop(t, f)
			if err := f.kube.Delete(ctx, f.current(t)); err != nil {
				t.Fatal(err)
			}
			namespace := &corev1.Namespace{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: before.Namespace.Name}, namespace); err != nil {
				t.Fatal(err)
			}
			if err := f.r.checkSandboxBootstrapCleanup(ctx, f.current(t)); err != nil {
				t.Fatal(err)
			}
			if err := f.r.releaseSandboxBootstrapResources(ctx, f.current(t), namespace); err != nil {
				t.Fatal(err)
			}
			account := &corev1.ServiceAccount{}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(before.IdentityServiceAccount), account); err != nil ||
				account.UID != before.IdentityServiceAccount.UID || slices.Contains(account.Finalizers, sandbox.BootstrapFinalizer) {
				t.Fatal("Custom deletion did not release its exact owned identity for namespace GC:", err)
			}
		})
	}
}

func TestCommonCustomConnectionsMigration(t *testing.T) {
	testPlannedMigrationRetainsHOMEForHarnesses(t, []string{"custom"}, withCommonCustom(t, "headless"), withCustomConnections(t))
}

func TestCommonCustomIdentityProjections(t *testing.T) {
	for _, mode := range []string{"headless", "chat", "web-ui"} {
		for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
			for _, connections := range []bool{false, true} {
				name := mode + "/" + string(backend) + "/entra"
				if connections {
					name += "-connections"
				}

				t.Run(name, func(t *testing.T) {
					setup := []func(*AgentReconciler, client.Client){
						withCommonCustom(t, mode), withReconciledCoordination(t), withNativeEntra(t),
					}
					if connections {
						setup = append(setup, withCustomConnections(t))
					}
					if backend == api.SandboxContainer {
						setup = append(setup, withContainerRuntime(t))
					}
					f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "custom", true, false, setup...)
					for range 5 {
						f.reconcileOK(t)
					}
					workload := f.workload(t)
					spec := workload.Spec.Template.Spec
					if spec.ServiceAccountName != "example-entra" || len(spec.Containers[0].Command) != 0 || len(spec.Containers[0].Args) != 0 {
						t.Fatal("Custom lost its approved ServiceAccount or image entrypoint")
					}
					expected := reconciledExpectationForTest(t, f)
					account := &corev1.ServiceAccount{}
					if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: workload.Namespace, Name: spec.ServiceAccountName}, account); err != nil {
						t.Fatal(err)
					}
					if err := expected.ValidateIdentityBinding(&corev1.Namespace{}, account, w.ObjectIdentity{Name: account.Name, UID: account.UID}); err == nil {
						t.Fatal("identity binding accepted an unobserved namespace")
					}
					entra := false
					for _, env := range spec.Containers[0].Env {
						entra = entra || env.Name == "AZURE_CLIENT_ID" && env.Value == "example-client"
					}
					if !entra {
						t.Fatal("Custom omitted its approved Azure identity environment")
					}
					if connections {
						contract := &w.ConnectionsMCP{URL: f.r.ConnectionsMCPURL, Namespace: "cetirun-tools",
							Audience: f.r.ConnectionsMCPAudience, CASecretName: f.r.ConnectionsMCPCASecretName}
						if err := contract.ValidatePod(spec); err != nil {
							t.Fatal("Custom omitted or widened its Connections proof:", err)
						}
					}
					for _, init := range spec.InitContainers {
						for _, env := range init.Env {
							if env.Name == "AZURE_CLIENT_ID" || env.Name == "CETIRUN_CONNECTIONS_MCP_URL" {
								t.Fatal("initializer inherited workload identity")
							}
						}
					}
				})
			}
		}
	}
}
