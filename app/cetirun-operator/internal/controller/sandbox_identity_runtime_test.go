// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func withNativeEntra(t *testing.T) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		ctx := context.Background()
		policy := &api.AgentPolicy{}
		if err := kube.Get(ctx, client.ObjectKey{Name: "policy-example"}, policy); err != nil {
			t.Fatal(err)
		}
		before := policy.DeepCopy()
		policy.Spec.EntraID.Enabled = true
		agent := &api.Agent{}
		if err := kube.Get(ctx, client.ObjectKey{Name: "example"}, agent); err != nil {
			t.Fatal(err)
		}
		config := &corev1.ConfigMap{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, config); err != nil {
			t.Fatal(err)
		}
		var document SandboxPlatformConfiguration
		if err := json.Unmarshal([]byte(config.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
			t.Fatal(err)
		}
		registration := document.Registrations[api.SandboxKata]
		registration.Capabilities.EntraID = true
		document.Registrations[api.SandboxKata] = registration
		platform := document.Platforms[api.SandboxKata]
		platform.KubernetesAudience = "https://kubernetes.default.svc"
		platform.Identities = []sandboxpolicy.ApprovedIdentity{{
			AgentUID: agent.UID, Namespace: instanceNamespace(agent.Name), ServiceAccountName: "example-entra",
			Revision: "example-approval", Entra: &sandboxpolicy.EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"},
		}}
		document.Platforms[api.SandboxKata] = platform
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		config.Data[SandboxPlatformConfigurationKey] = string(raw)
		if err := kube.Update(ctx, config); err != nil {
			t.Fatal(err)
		}
		if r.SandboxNativeCoordination == nil {
			if err := kube.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
		} else {
			ticket, err := r.SandboxNativeCoordination.AcquireConfigurationChange(ctx, before, policy)
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(kube, kube, before, policy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.SandboxNativeCoordination.ReconcileChange(ctx, ticket, adapter); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func nativeEntraFixture(t *testing.T, kind string, configure ...func(*AgentReconciler, client.Client)) *nativeSandboxFixture {
	t.Helper()
	configure = append([]func(*AgentReconciler, client.Client){
		withReconciledCoordination(t), withNativeEntra(t),
	}, configure...)
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
		configure...)
	for range 4 {
		f.reconcileOK(t)
	}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: instanceNamespace(f.agent.Name)}, &corev1.Namespace{}); err != nil {
		t.Fatalf("identity bootstrap missing: %v; status=%+v", err, f.current(t).Status)
	}
	return f
}

func TestNativeEntraCombinedKubernetesRuntime(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := nativeEntraFixture(t, kind, func(r *AgentReconciler, kube client.Client) {
				configureReconciledKubernetesAccess(t, r, kube, "example", "policy-example")
				config := &corev1.ConfigMap{}
				if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, config); err != nil {
					t.Fatal(err)
				}
				var document SandboxPlatformConfiguration
				if err := json.Unmarshal([]byte(config.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
					t.Fatal(err)
				}
				platform := document.Platforms[api.SandboxKata]
				platform.Identities[0].Entra = &sandboxpolicy.EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"}
				document.Platforms[api.SandboxKata] = platform
				raw, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				config.Data[SandboxPlatformConfigurationKey] = string(raw)
				if err := kube.Update(ctx, config); err != nil {
					t.Fatal(err)
				}
				installKubernetesFixtureCA(t, r, kube, "example")
				refreshKubernetesFixturePolicy(t, r, "policy-example")
			})
			for range 2 {
				f.reconcileOK(t)
			}
			initial := reconciledExpectationForTest(t, f)
			source := initial.IdentityServiceAccount
			if source == nil || source.Name != kubernetesFixtureAccount || source.UID == "" {
				t.Fatal("combined identity lost the approved bootstrap-owned ServiceAccount")
			}
			assertKubernetesFixtureGrants(t, f, true)
			spec := f.workload(t).Spec.Template.Spec
			audiences := map[string]string{}
			for _, volume := range spec.Volumes {
				if volume.Projected == nil {
					continue
				}
				for _, projection := range volume.Projected.Sources {
					if projection.ServiceAccountToken != nil {
						audiences[volume.Name] = projection.ServiceAccountToken.Audience
					}
				}
			}
			if len(audiences) != 2 || audiences["sandbox-kubernetes-api"] != "https://kubernetes.default.svc" ||
				audiences["sandbox-entra-exchange"] != sandboxpolicy.EntraAudience || spec.ServiceAccountName != source.Name {
				t.Fatal("combined identity did not retain separate bounded audience projections")
			}
			for _, suspended := range []bool{true, false} {
				agent := f.current(t)
				agent.Spec.Suspended = suspended
				agent.Generation++
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for range 4 {
					f.reconcileOK(t)
				}
				assertKubernetesFixtureGrants(t, f, !suspended)
				current := reconciledExpectationForTest(t, f)
				if current.IdentityServiceAccount == nil || current.IdentityServiceAccount.UID != source.UID ||
					current.Home != initial.Home || current.Namespace != initial.Namespace || current.Agent != initial.Agent {
					t.Fatal("combined identity lifecycle replaced its retained identity or HOME")
				}
			}
		})
	}
}

func TestNativeEntraIndependentKubernetesApproval(t *testing.T) {
	for _, tc := range []struct {
		name       string
		requested  string
		kubernetes bool
		entra      bool
		wantError  bool
	}{
		{name: "Entra only", entra: true},
		{name: "unused Kubernetes approval", kubernetes: true, entra: true},
		{name: "same account independently approved", requested: "example-entra", kubernetes: true, entra: true},
		{name: "Kubernetes not approved", requested: "example-entra", entra: true, wantError: true},
		{name: "different Kubernetes account", requested: "other", kubernetes: true, entra: true, wantError: true},
		{name: "Entra not approved", requested: "example-entra", kubernetes: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &AgentReconciler{
				SandboxPreparation:        &SandboxPreparation{NativeExecution: true},
				SandboxNativeCoordination: &sandboxcoordination.Store{},
			}
			agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "example-agent"}}
			approval := sandboxpolicy.ApprovedIdentity{
				AgentUID: agent.UID, Namespace: instanceNamespace(agent.Name), ServiceAccountName: "example-entra",
				Revision: "example-approval", Kubernetes: tc.kubernetes,
			}
			if tc.entra {
				approval.Entra = &sandboxpolicy.EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"}
			}
			resolution := &sandbox.Resolution{Sandbox: api.SandboxSpec{Type: api.SandboxKata}}
			resolution.AgentPolicy.EntraID.Enabled = true
			if tc.requested != "" {
				resolution.AgentPolicy.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{ServiceAccountName: tc.requested}
			}
			name, err := r.bootstrapIdentityAccountName(&sandboxPreparationAttempt{
				reconciled: true, agent: agent, resolution: resolution,
				configuration: SandboxConfigurationSnapshot{Platforms: map[api.SandboxType]sandboxpolicy.Platform{
					api.SandboxKata: {Identities: []sandboxpolicy.ApprovedIdentity{approval}},
				}},
			})
			if (err != nil) != tc.wantError || err == nil && name != "example-entra" {
				t.Fatalf("bootstrap account = %q, error = %v, want error %v", name, err, tc.wantError)
			}
		})
	}
}

func TestNativeEntraRetainsKubernetesAccountPin(t *testing.T) {
	ctx := context.Background()
	f := nativeEntraFixture(t, "hermes")
	agent := f.current(t)
	state, receipt, err := f.r.readSandboxBootstrap(ctx, agent)
	if err != nil || receipt.ServiceAccount == nil || !slices.Contains(agent.Finalizers, sandbox.BootstrapFinalizer) {
		t.Fatal("fixture requires a confirmed bootstrap-owned account:", err)
	}
	resolution := &sandbox.Resolution{}
	resolution.AgentPolicy.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{ServiceAccountName: receipt.ServiceAccount.Name}
	attempt := &sandboxPreparationAttempt{reconciled: true, agent: agent, resolution: resolution}
	pin, err := f.r.sandboxIdentityPin(ctx, attempt)
	if err != nil || pin == nil || pin.UID != receipt.ServiceAccount.UID || pin.BootstrapUID != state.UID {
		t.Fatal("removing Entra lost the retained Kubernetes account pin:", err)
	}
	resolution.AgentPolicy.KubernetesAccess.ServiceAccountName = "other-account"
	if _, err := f.r.sandboxIdentityPin(ctx, attempt); !errors.Is(err, sandboxpolicy.ErrUnavailable) {
		t.Fatal("a different Kubernetes account reused the retained pin:", err)
	}
	resolution.AgentPolicy.KubernetesAccess.ServiceAccountName = receipt.ServiceAccount.Name
	f.r.APIReader = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if _, ok := object.(*corev1.ConfigMap); ok && key == client.ObjectKeyFromObject(state) {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	if _, err := f.r.sandboxIdentityPin(ctx, attempt); err == nil {
		t.Fatal("an existing lifetime accepted a missing bootstrap authority")
	}
	attempt.agent = &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "fresh", UID: "fresh-agent"}}
	if pin, err := f.r.sandboxIdentityPin(ctx, attempt); err != nil || pin != nil {
		t.Fatal("a fresh Kubernetes-only lifetime unexpectedly required Entra bootstrap:", err)
	}
}

func restartEntraReconciler(r *AgentReconciler) *AgentReconciler {
	return &AgentReconciler{
		Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, EventRecorder: r.EventRecorder,
		HelmClient: r.HelmClient, OperatorNamespace: r.OperatorNamespace, OperatorSAName: r.OperatorSAName,
		ChartVersion: r.ChartVersion, ChartRegistry: r.ChartRegistry, DefaultModel: r.DefaultModel,
		IstioRevision: r.IstioRevision, IstioGateway: r.IstioGateway, DomainSuffix: r.DomainSuffix,
		CetiRunHelmValues: r.CetiRunHelmValues, MemoryImage: r.MemoryImage, MemoryEnabled: r.MemoryEnabled,
		MemoryRegistry: r.MemoryRegistry, SkillsStorageAccount: r.SkillsStorageAccount,
		SandboxPreparation: r.SandboxPreparation, SandboxNativeCoordination: r.SandboxNativeCoordination,
		AIGateway: r.AIGateway, AIGatewayHTTPClient: r.AIGatewayHTTPClient, aiGatewayClientFactory: r.aiGatewayClientFactory,
		CustomBindings: r.CustomBindings, ChatAccess: r.ChatAccess,
		ConnectionsMCPNativeEnabled: r.ConnectionsMCPNativeEnabled, ConnectionsMCPURL: r.ConnectionsMCPURL,
		ConnectionsMCPRuntimeTypes: r.ConnectionsMCPRuntimeTypes, ConnectionsMCPAudience: r.ConnectionsMCPAudience,
		ConnectionsMCPCASecretName: r.ConnectionsMCPCASecretName, ConnectionsMCPBrokerNamespace: r.ConnectionsMCPBrokerNamespace,
		ConnectionsMCPBrokerServiceAccount: r.ConnectionsMCPBrokerServiceAccount,
	}
}

func TestNativeEntraUsesBootstrapSourceBeforeCommonApply(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := nativeEntraFixture(t, kind)
			for range 2 {
				f.reconcileOK(t)
			}
			state, receipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
			if err != nil || receipt.ServiceAccount == nil || receipt.ServiceAccount.UID == "" || receipt.Version != "v3" {
				t.Fatal("dedicated SA has no durable bootstrap pin:", err)
			}
			expected := reconciledExpectationForTest(t, f)
			source := expected.IdentityServiceAccount
			if source == nil || source.UID != receipt.ServiceAccount.UID ||
				source.Annotations[sandboxBootstrapAnnotation] != string(state.UID) {
				t.Fatal("common expectation lost its bootstrap-owned identity")
			}
			for _, object := range expected.Controls {
				if object.GetKind() == "ServiceAccount" && object.GetName() == source.Name {
					t.Fatal("Helm was allowed to own the retained identity")
				}
			}
			account := &corev1.ServiceAccount{}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(source), account); err != nil {
				t.Fatal(err)
			}
			namespace := &corev1.Namespace{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: expected.Namespace.Name}, namespace); err != nil {
				t.Fatal(err)
			}
			if err := expected.ValidateIdentityBinding(namespace, account, w.ObjectIdentity{Name: source.Name, UID: source.UID}); err != nil {
				t.Fatal(err)
			}
			workload := f.workload(t)
			if ptr.Deref(workload.Spec.Replicas, 0) != 1 || workload.Spec.Template.Spec.ServiceAccountName != source.Name {
				t.Fatal("Entra did not reach the actual common executor")
			}
			if !slices.ContainsFunc(expected.Sources, func(ref w.SourceReference) bool {
				return ref.Kind == "ServiceAccount" && ref.UID == account.UID && ref.Name == account.Name
			}) {
				t.Fatal("Pod admission has no current identity source")
			}
			var bindings rbacv1.RoleBindingList
			if err := f.kube.List(ctx, &bindings); err != nil {
				t.Fatal(err)
			}
			for _, binding := range bindings.Items {
				for _, subject := range binding.Subjects {
					if subject.Kind == "ServiceAccount" && subject.Name == account.Name && subject.Namespace == account.Namespace {
						t.Fatal("Entra-only identity received Kubernetes RBAC")
					}
				}
			}
			workload.Status.ObservedGeneration = workload.Generation
			workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
			if err := f.kube.Status().Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			if f.current(t).Status.Phase != api.PhaseReady {
				t.Fatal("native Entra did not complete common runtime readiness")
			}
		})
	}
}

func TestNativeEntraPinsOmittedRuntimeBeforeBootstrap(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, container := range []bool{false, true} {
			for _, pinned := range []bool{false, true} {
				name := kind + "/container=" + map[bool]string{false: "false", true: "true"}[container] +
					"/explicit-image=" + map[bool]string{false: "false", true: "true"}[pinned]
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					options := []func(*AgentReconciler, client.Client){withReconciledCoordination(t), withNativeEntra(t)}
					if container {
						options = append(options, withContainerRuntime(t))
					}
					f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false, options...)
					agent := f.current(t)
					original := agent.DeepCopy()
					if !pinned {
						agent.Spec.Helm.Version = ""
						agent.Spec.Agent.Image = nil
					}
					agent.Spec.Agent.Port = nil
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					for range 6 {
						current := f.current(t)
						if current.Spec.Helm.Version == "" || current.Spec.Agent.Image == nil || current.Spec.Agent.Port == nil {
							if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
								t.Fatal("runtime defaulting bypassed owned namespace bootstrap:", err)
							}
							if f.installer.stages != 0 {
								t.Fatal("missing runtime pins reached workload execution")
							}
						}
						f.reconcileOK(t)
					}
					current := f.current(t)
					if current.Spec.Helm.Version != original.Spec.Helm.Version ||
						!reflect.DeepEqual(current.Spec.Agent.Image, original.Spec.Agent.Image) ||
						!reflect.DeepEqual(current.Spec.Agent.Port, original.Spec.Agent.Port) {
						t.Fatal("trusted runtime defaults were not pinned or replaced explicit coordinates")
					}
					_, receipt, err := f.r.readSandboxBootstrap(ctx, current)
					if err != nil || receipt.ServiceAccount == nil || receipt.ServiceAccount.UID == "" {
						t.Fatal("defaulted runtime omitted its durable ServiceAccount pin:", err)
					}
					expected := reconciledExpectationForTest(t, f)
					if expected.IdentityServiceAccount == nil || expected.IdentityServiceAccount.UID != receipt.ServiceAccount.UID ||
						f.installer.stages == 0 || ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
						t.Fatal("defaulted runtime did not execute with the recorded identity source")
					}
				})
			}
		}
	}
}

func TestNativeEntraRuntimeDefaultsWaitForApproval(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "hermes", true, false,
		withReconciledCoordination(t), withNativeEntra(t))
	agent := f.current(t)
	agent.Spec.Helm.Version = ""
	agent.Spec.Agent.Image, agent.Spec.Agent.Port = nil, nil
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	configuration := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: "sandbox-platform"}, configuration); err != nil {
		t.Fatal(err)
	}
	approved := configuration.Data[SandboxPlatformConfigurationKey]
	var document SandboxPlatformConfiguration
	if err := json.Unmarshal([]byte(approved), &document); err != nil {
		t.Fatal(err)
	}
	platform := document.Platforms[api.SandboxKata]
	platform.Identities = nil
	document.Platforms[api.SandboxKata] = platform
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Data[SandboxPlatformConfigurationKey] = string(raw)
	if err := f.kube.Update(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f.reconcileOK(t)
	}
	current := f.current(t)
	if current.Spec.Helm.Version != "" || current.Spec.Agent.Image != nil || current.Spec.Agent.Port != nil ||
		f.installer.stages != 0 {
		t.Fatal("missing identity approval allowed runtime defaulting or execution")
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatal("missing identity approval created a namespace:", err)
	}
	configuration.Data[SandboxPlatformConfigurationKey] = approved
	if err := f.kube.Update(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		f.reconcileOK(t)
	}
	expected := reconciledExpectationForTest(t, f)
	if expected.IdentityServiceAccount == nil || expected.IdentityServiceAccount.UID == "" ||
		ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
		t.Fatal("supplying approval did not recover the defaulted identity lifetime")
	}
}

func TestNativeEntraBootstrapRetriesPreserveFirstUID(t *testing.T) {
	for _, failure := range []string{"create response lost", "pin write rejected", "pin response lost"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "hermes", true, false,
				withReconciledCoordination(t), withNativeEntra(t))
			fired, creates := false, 0
			kube := interceptor.NewClient(f.kube, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					account, match := object.(*corev1.ServiceAccount)
					if match && account.Name == "example-entra" {
						creates++
					}
					err := c.Create(ctx, object, opts...)
					if err == nil && match && account.Name == "example-entra" && failure == "create response lost" && !fired {
						fired = true
						return errors.New("synthetic accepted create response lost")
					}
					return err
				},
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					state, match := object.(*corev1.ConfigMap)
					if match && strings.HasPrefix(state.Name, "sandbox-bootstrap-") && !fired && failure != "create response lost" {
						var receipt sandboxBootstrapReceipt
						if json.Unmarshal([]byte(state.Data[sandboxBootstrapReceiptKey]), &receipt) == nil &&
							receipt.ServiceAccount != nil && receipt.ServiceAccount.UID != "" {
							fired = true
							if failure == "pin response lost" {
								if err := c.Patch(ctx, object, patch, opts...); err != nil {
									return err
								}
							}
							return errors.New("synthetic identity pin interruption")
						}
					}
					return c.Patch(ctx, object, patch, opts...)
				},
			})
			f.kube, f.r.Client, f.r.APIReader = kube, kube, kube
			var interrupted error
			for range 4 {
				_, interrupted = f.reconcile()
				if fired {
					break
				}
			}
			if !fired || interrupted == nil {
				t.Fatal("identity bootstrap interruption was not exercised:", interrupted)
			}
			if f.installer.stages != 0 {
				t.Fatal("unconfirmed identity reached a workload write")
			}
			account := &corev1.ServiceAccount{}
			if err := kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(f.agent.Name), Name: "example-entra"}, account); err != nil {
				t.Fatal(err)
			}
			first := account.UID
			// Restart the reconciler with the same persisted API state.
			f.r = restartEntraReconciler(f.r)
			for range 4 {
				f.reconcileOK(t)
			}
			_, receipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
			if err != nil || receipt.ServiceAccount.UID != first || creates != 1 {
				t.Fatal("retry re-created or changed the originally observed account:", err)
			}
			if ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
				t.Fatal("confirmed identity did not recover to real common apply")
			}
		})
	}
}

func TestNativeEntraReplacementAndMetadataDriftStayHeld(t *testing.T) {
	for _, change := range []string{"UID", "client", "owner", "automount"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f := nativeEntraFixture(t, "openclaw")
			_, before, err := f.r.readSandboxBootstrap(ctx, f.current(t))
			if err != nil {
				t.Fatal(err)
			}
			pin := before.ServiceAccount.UID
			reader := interceptor.NewClient(f.kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					account, ok := object.(*corev1.ServiceAccount)
					if !ok || account.Name != before.ServiceAccount.Name {
						return nil
					}
					switch change {
					case "UID":
						account.UID = "same-name-replacement"
					case "client":
						account.Annotations["azure.workload.identity/client-id"] = "unapproved-client"
					case "owner":
						account.OwnerReferences[0].UID = "different-agent"
					case "automount":
						account.AutomountServiceAccountToken = ptr.To(true)
					}
					return nil
				},
			})
			f.r.Client, f.r.APIReader = reader, reader
			_, _ = f.reconcile()
			_, after, err := f.r.readSandboxBootstrap(ctx, f.current(t))
			if err != nil || after.ServiceAccount.UID != pin {
				t.Fatal("drift replaced the durable identity pin:", err)
			}
			f.assertNotReady(t)
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("identity drift left a workload running")
			}
		})
	}
}

func acknowledgeEntraStop(t *testing.T, f *nativeSandboxFixture) {
	t.Helper()
	workload := f.workload(t)
	workload.Status.ObservedGeneration = workload.Generation
	workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 0, 0, 0
	if err := f.kube.Status().Update(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f.reconcileOK(t)
	}
}

func TestNativeEntraDisableResumePreservesRetainedIdentity(t *testing.T) {
	ctx := context.Background()
	f := nativeEntraFixture(t, "hermes")
	before := reconciledExpectationForTest(t, f)
	pin, home, key := before.IdentityServiceAccount.UID, before.Home, f.current(t).Status.AIGateway.KeyID
	agent := f.current(t)
	agent.Spec.Suspended = true
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}

	f.reconcileOK(t)
	acknowledgeEntraStop(t, f)
	policy := &api.AgentPolicy{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: "policy-example"}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.EntraID.Enabled = false
	if err := f.kube.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	agent = f.current(t)
	agent.Spec.Suspended = false
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		f.reconcileOK(t)
	}
	after := reconciledExpectationForTest(t, f)
	if after.IdentityServiceAccount != nil || after.Home != home || f.current(t).Status.AIGateway.KeyID != key {
		t.Fatal("disabling Entra changed retained HOME/key or retained an active projection")
	}
	_, receipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
	if err != nil || receipt.ServiceAccount.UID != pin {
		t.Fatal("disabling identity erased its durable pin:", err)
	}
	account := &corev1.ServiceAccount{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: before.Namespace.Name, Name: before.IdentityServiceAccount.Name}, account); err != nil || account.UID != pin {
		t.Fatal("Helm deleted or recreated the bootstrap-owned account:", err)
	}
	for _, volume := range f.workload(t).Spec.Template.Spec.Volumes {
		if volume.Name == "sandbox-entra-exchange" {
			t.Fatal("disabled identity remained mounted")
		}
	}
}

func TestNativeEntraRetainedRestartKeepsSource(t *testing.T) {
	ctx := context.Background()
	f := nativeEntraFixture(t, "openclaw")
	before := reconciledExpectationForTest(t, f)
	key := f.current(t).Status.AIGateway.KeyID
	for _, request := range []string{"request-00000011", "request-00000012"} {
		agent := f.current(t)
		agent.Spec.RestartRequestID = request
		agent.Generation++
		if err := f.kube.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			f.reconcileOK(t)
		}
		after := reconciledExpectationForTest(t, f)
		if after.IdentityServiceAccount.UID != before.IdentityServiceAccount.UID || after.Home != before.Home ||
			f.current(t).Status.AIGateway.KeyID != key ||
			f.workload(t).Spec.Template.Annotations[api.AnnotationRestartRequestID] != request {
			t.Fatal("retained restart replaced the pinned identity, HOME or key")
		}
	}
}

func TestNativeEntraApprovalRefreshKeepsUID(t *testing.T) {
	ctx := context.Background()
	f := nativeEntraFixture(t, "hermes")
	before := reconciledExpectationForTest(t, f)
	key := f.current(t).Status.AIGateway.KeyID
	agent := f.current(t)
	agent.Spec.Suspended = true
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	acknowledgeEntraStop(t, f)
	config := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: "sandbox-platform"}, config); err != nil {
		t.Fatal(err)
	}
	var document SandboxPlatformConfiguration
	if err := json.Unmarshal([]byte(config.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
		t.Fatal(err)
	}
	document.Platforms[api.SandboxKata].Identities[0].Revision = "approved-refresh"
	document.Platforms[api.SandboxKata].Identities[0].Entra.ClientID = "refreshed-client"
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	config.Data[SandboxPlatformConfigurationKey] = string(raw)
	if err := f.kube.Update(ctx, config); err != nil {
		t.Fatal(err)
	}
	agent = f.current(t)
	agent.Spec.Suspended = false
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		f.reconcileOK(t)
	}
	after := reconciledExpectationForTest(t, f)
	if after.IdentityServiceAccount.UID != before.IdentityServiceAccount.UID || after.Home != before.Home ||
		after.IdentityServiceAccount.Annotations["azure.workload.identity/client-id"] != "refreshed-client" ||
		after.Revision == before.Revision || f.current(t).Status.AIGateway.KeyID != key {
		t.Fatal("approval refresh lost its source pin, current metadata or retained lifetime")
	}
}

func TestNativeEntraDeletionReleasesOnlyRecordedAccount(t *testing.T) {
	ctx := context.Background()
	f := nativeEntraFixture(t, "hermes")
	before := reconciledExpectationForTest(t, f)
	agent := f.current(t)
	agent.Spec.Suspended = true
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	acknowledgeEntraStop(t, f)
	if err := f.kube.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	agent = f.current(t)
	namespace := &corev1.Namespace{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: before.Namespace.Name}, namespace); err != nil {
		t.Fatal(err)
	}
	if err := f.r.checkSandboxBootstrapCleanup(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := f.r.releaseSandboxBootstrapResources(ctx, agent, namespace); err != nil {
		t.Fatal(err)
	}
	account := &corev1.ServiceAccount{}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(before.IdentityServiceAccount), account); err != nil {
		t.Fatal(err)
	}
	if account.UID != before.IdentityServiceAccount.UID || slices.Contains(account.Finalizers, sandbox.BootstrapFinalizer) {
		t.Fatal("Agent deletion failed to release exactly the retained account for namespace GC")
	}
}

func TestNativeEntraFinalApplyGuardRejectsChangedSource(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "hermes", true, false,
		withReconciledCoordination(t), withNativeEntra(t))
	shared := f.r.HelmClient.(*sharedSandboxInstaller)
	changed := false
	shared.beforeApply = func(_ *helm.PreparedRelease) error {
		if changed {
			return nil
		}
		account := &corev1.ServiceAccount{}
		if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(f.agent.Name), Name: "example-entra"}, account); err != nil {
			return err
		}
		account.Annotations["azure.workload.identity/client-id"] = "changed-after-publication"
		if err := f.kube.Update(ctx, account); err != nil {
			return err
		}
		changed = true
		return nil
	}
	var failed error
	for range 5 {
		_, failed = f.reconcile()
		if changed {
			break
		}
	}
	if !changed || failed == nil {
		t.Fatal("last-moment source change was not rejected:", failed)
	}
	if f.installer.stages != 0 {
		t.Fatal("stale Entra source reached a workload write")
	}
	f.assertNotReady(t)
	_, receipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
	if err != nil || receipt.ServiceAccount.UID == "" {
		t.Fatal("failed apply lost the confirmed ServiceAccount pin:", err)
	}
}

func TestNativeEntraWithIngressMTLSAndRateLimits(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
			t.Run(kind+"/"+string(mode), func(t *testing.T) {
				ctx := context.Background()
				f := nativeEntraFixture(t, kind, withNativeHomeAccessMode(t, mode), withIngressMTLS(t, kind))
				workload := f.workload(t)
				helpers := 0
				for _, container := range workload.Spec.Template.Spec.Containers {
					if container.Name == ingressmtls.ContainerName {
						helpers++
						for _, mount := range container.VolumeMounts {
							if mount.Name == "sandbox-entra-exchange" {
								t.Fatal("ingress helper acquired the Entra projection")
							}
						}
					}
				}
				if helpers != 1 {
					t.Fatal("combined identity and mTLS must retain one ingress helper")
				}
				workload.Status.ObservedGeneration = workload.Generation
				workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
				if err := f.kube.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				if f.current(t).Status.Phase != api.PhaseReady {
					t.Fatal("combined identity and mTLS did not reach common readiness")
				}
				before, expected := f.workload(t), reconciledExpectationForTest(t, f)
				account := &corev1.ServiceAccount{}
				if err := f.kube.Get(ctx, client.ObjectKeyFromObject(expected.IdentityServiceAccount), account); err != nil {
					t.Fatal(err)
				}
				version, stages := account.ResourceVersion, f.installer.stages
				token := aigateway.Token(f.gateway.generated[0].Key)
				for _, limits := range []*api.AgentPolicyRateLimits{exampleRateLimits(), nil} {
					policy := &api.AgentPolicy{}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: "policy-example"}, policy); err != nil {
						t.Fatal(err)
					}
					policy.Spec.ModelAccess.RateLimits = limits
					policy.Generation++
					if err := f.kube.Update(ctx, policy); err != nil {
						t.Fatal(err)
					}
					f.reconcileOK(t)
					current := reconciledExpectationForTest(t, f)
					if err := f.kube.Get(ctx, client.ObjectKeyFromObject(account), account); err != nil {
						t.Fatal(err)
					}
					if f.current(t).Status.Phase != api.PhaseReady || f.installer.stages != stages ||
						!reflect.DeepEqual(before.Spec, f.workload(t).Spec) || account.ResourceVersion != version ||
						current.IdentityServiceAccount.UID != expected.IdentityServiceAccount.UID ||
						current.Revision != expected.Revision || current.PlanHash != expected.PlanHash {
						t.Fatal("rate-limit-only change restarted or rewrote the combined identity/mTLS workload")
					}
					want := aigateway.KeyLimits{}
					if limits != nil {
						want = exampleKeyLimits()
					}
					key := f.gateway.keys[token]
					if key == nil || key.Blocked || !key.Limits.Equal(want) || len(f.gateway.generated) != 1 {
						t.Fatal("rate limits did not update the existing unblocked Gateway key")
					}
				}
			})
		}
	}
}

func TestNativeEntraEnableOnRetainedBootstrap(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
				withReconciledCoordination(t))
			before := reconciledExpectationForTest(t, f)
			key := f.current(t).Status.AIGateway.KeyID
			state, receipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
			if err != nil || receipt.ServiceAccount != nil {
				t.Fatal("fixture must start without an identity account:", err)
			}
			agent := f.current(t)
			agent.Spec.Suspended = true
			agent.Generation++
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			acknowledgeEntraStop(t, f)
			withNativeEntra(t)(f.r, f.kube)
			agent = f.current(t)
			agent.Spec.Suspended = false
			agent.Generation++
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			for range 5 {
				f.reconcileOK(t)
			}
			currentState, currentReceipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
			if err != nil || currentState.UID != state.UID || currentReceipt.ServiceAccount == nil ||
				currentReceipt.ServiceAccount.UID == "" || !reflect.DeepEqual(receipt.Objects, currentReceipt.Objects) {
				t.Fatal("enabling identity did not extend the existing bootstrap without replacing resources:", err)
			}
			after := reconciledExpectationForTest(t, f)
			if after.IdentityServiceAccount == nil || after.IdentityServiceAccount.UID != currentReceipt.ServiceAccount.UID ||
				after.Home != before.Home || f.current(t).Status.AIGateway.KeyID != key ||
				ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
				t.Fatal("enabling identity failed to preserve the retained workload lifetime")
			}
		})
	}
}

func TestNativeEntraPendingBootstrapKeepsConfirmedSuspension(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
				withReconciledCoordination(t))
			agent := f.current(t)
			agent.Spec.Suspended = true
			agent.Generation++
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			acknowledgeEntraStop(t, f)
			f.reconcileOK(t)
			if f.current(t).Status.Phase != api.PhaseSuspended {
				t.Fatal("fixture did not confirm suspension")
			}
			withNativeEntra(t)(f.r, f.kube)
			keyID, stages := f.current(t).Status.AIGateway.KeyID, f.installer.stages
			var version string
			for index := range 3 {
				_, reconcileErr := f.reconcile()
				current := f.current(t)
				if current.Status.Phase != api.PhaseSuspended || current.Status.AccessURL != "" ||
					current.Status.AIGateway.KeyID != keyID || !current.Status.AIGateway.Blocked ||
					current.Status.AIGateway.Reason != aiGatewayReasonSuspended ||
					ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || f.installer.stages != stages {
					t.Fatalf("pending identity changed suspension: phase=%s blocked=%t reason=%s replicas=%d stages=%d/%d error=%v",
						current.Status.Phase, current.Status.AIGateway.Blocked, current.Status.AIGateway.Reason,
						ptr.Deref(f.workload(t).Spec.Replicas, -1), f.installer.stages, stages, reconcileErr)
				}
				if index > 0 && current.ResourceVersion != version {
					t.Fatal("unchanged pending identity caused suspended status churn")
				}
				version = current.ResourceVersion
				_, receipt, err := f.r.readSandboxBootstrap(ctx, current)
				if err != nil || receipt.ServiceAccount != nil {
					t.Fatal("suspension must not create a new identity account:", err)
				}
			}
			agent = f.current(t)
			agent.Spec.Suspended = false
			agent.Generation++
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			for range 5 {
				f.reconcileOK(t)
			}
			_, receipt, err := f.r.readSandboxBootstrap(ctx, f.current(t))
			if err != nil || receipt.ServiceAccount == nil || receipt.ServiceAccount.UID == "" ||
				ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 || f.current(t).Status.AIGateway.KeyID != keyID {
				t.Fatal("explicit resume did not bootstrap the approved identity on the retained lifetime:", err)
			}
		})
	}
}
