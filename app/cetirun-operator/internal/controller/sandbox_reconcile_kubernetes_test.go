// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	admissionhandler "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const kubernetesFixtureAccount = "kubernetes-reader"

func configureReconciledKubernetesAccess(t *testing.T, r *AgentReconciler, author client.Client, agentName, policyName string) {
	t.Helper()
	ctx := context.Background()
	agent := &api.Agent{}
	policy := &api.AgentPolicy{}
	if err := author.Get(ctx, client.ObjectKey{Name: agentName}, agent); err != nil {
		t.Fatal(err)
	}
	if err := author.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
		t.Fatal(err)
	}
	box := &api.Sandbox{}
	if err := author.Get(ctx, client.ObjectKey{Name: agent.Spec.SandboxRef}, box); err != nil {
		t.Fatal(err)
	}
	if err := author.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "operations"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	roles := []*rbacv1.ClusterRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes-pods-reader"}, Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"},
		}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes-nodes-reader"}, Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"},
		}}},
	}
	for _, role := range roles {
		if err := author.Create(ctx, role); err != nil {
			t.Fatal(err)
		}
	}
	configuration := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}
	if err := author.Get(ctx, key, configuration); err != nil {
		t.Fatal(err)
	}
	var document SandboxPlatformConfiguration
	if err := json.Unmarshal([]byte(configuration.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
		t.Fatal(err)
	}
	registration := document.Registrations[box.Spec.Type]
	registration.Capabilities.KubernetesAccess = true
	document.Registrations[box.Spec.Type] = registration
	platform := document.Platforms[box.Spec.Type]
	platform.KubernetesAudience, platform.TokenExpirationSeconds = "https://kubernetes.default.svc", 600
	platform.Topology.KubernetesAPI = []sandboxpolicy.Destination{{
		Peer:  sandboxpolicy.Peer{Name: "kubernetes", UID: "synthetic-kubernetes-api", IPs: []string{"10.96.0.1"}},
		Ports: []api.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolTCP}},
	}}
	platform.Identities = []sandboxpolicy.ApprovedIdentity{{
		AgentUID: agent.UID, Namespace: instanceNamespace(agent.Name), ServiceAccountName: kubernetesFixtureAccount,
		Revision: "synthetic-kubernetes-identity", Kubernetes: true,
	}}
	for index, role := range roles {
		namespace := ""
		if index == 0 {
			namespace = "operations"
		}
		platform.Grants = append(platform.Grants, sandboxpolicy.ApprovedGrant{
			PolicyUID: policy.UID, Namespace: namespace,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			RoleUID: role.UID, RoleResourceVersion: role.ResourceVersion,
			Revision: "synthetic-kubernetes-approval", Rules: role.DeepCopy().Rules,
		})
	}
	document.Platforms[box.Spec.Type] = platform
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Data[SandboxPlatformConfigurationKey] = string(raw)
	if err := author.Update(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	before := policy.DeepCopy()
	policy.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
		ServiceAccountName:  kubernetesFixtureAccount,
		RoleBindings:        []api.AgentPolicyRoleBinding{{Namespace: "operations", ClusterRoleName: roles[0].Name}},
		ClusterRoleBindings: []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: roles[1].Name}},
	}
	if store := r.SandboxNativeCoordination; store != nil {
		adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(author, author, before, policy)
		if err != nil {
			t.Fatal(err)
		}
		ticket, err := store.AcquireConfigurationChange(ctx, before, policy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil {
			t.Fatal(err)
		}
	} else if err := author.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	refreshKubernetesFixturePolicy(t, r, policyName)
}

func refreshKubernetesFixturePolicy(t *testing.T, r *AgentReconciler, name string) {
	t.Helper()
	controller := &AgentPolicyReconciler{
		Client: r.Client, APIReader: r.APIReader, Source: r.SandboxPreparation.Configuration, NativeExecution: true,
	}
	if _, err := controller.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}}); err != nil {
		t.Fatal(err)
	}
}

func installKubernetesFixtureCA(t *testing.T, r *AgentReconciler, author client.Client, agentName string) {
	t.Helper()
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, object, opts...); err != nil {
				return err
			}
			if namespace, ok := object.(*corev1.Namespace); ok && namespace.Name == instanceNamespace(agentName) {
				return author.Create(ctx, &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: namespace.Name},
					Data:       map[string]string{"ca.crt": "synthetic-public-test-ca"},
				})
			}
			return nil
		},
	})
}

func newReconciledKubernetesFixture(t *testing.T, kind string, mode corev1.PersistentVolumeAccessMode, setup ...func(*AgentReconciler, client.Client)) *nativeSandboxFixture {
	t.Helper()
	fixture := testfixture.New(kind)
	options := []func(*AgentReconciler, client.Client){
		withNativeHomeAccessMode(t, mode), func(r *AgentReconciler, author client.Client) {
			configureReconciledKubernetesAccess(t, r, author, fixture.Agent.Name, fixture.Policy.Name)
			installKubernetesFixtureCA(t, r, author, fixture.Agent.Name)
		},
	}
	return newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false, append(options, setup...)...)
}

func assertKubernetesFixtureGrants(t *testing.T, f *nativeSandboxFixture, granted bool) []sandboxpolicy.BindingReference {
	t.Helper()
	expected := reconciledExpectationForTest(t, f)
	bindings, err := expected.KubernetesBindings()
	if err != nil || (granted && len(bindings) != 2) || (!granted && len(bindings) != 0) {
		t.Fatalf("current Kubernetes binding inventory, granted=%t: %v %v", granted, bindings, err)
	}
	journal := f.r.sandboxGrantJournal()
	recorded, err := journal.Read(context.Background(), f.agent.Name, f.agent.UID)
	if err != nil {
		t.Fatal(err)
	}
	workloadBindings := slices.DeleteFunc(slices.Clone(recorded), func(ref sandboxpolicy.BindingReference) bool {
		return ref.RoleRef.Kind != "ClusterRole"
	})
	if len(workloadBindings) != len(bindings) {
		t.Fatal("write-ahead journal differs from current workload grants")
	}
	if granted {
		if err := journal.VerifyKubernetesBindings(context.Background(), f.agent.Name, f.agent.UID, bindings); err != nil {
			t.Fatal("Kubernetes bindings were not confirmed through the existing journal:", err)
		}
	}
	return workloadBindings
}

func TestReconciledSandboxKubernetesIngressMTLS(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			input := testfixture.New(kind)
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
				withReconciledCoordination(t), withIngressMTLS(t, kind),
				func(r *AgentReconciler, author client.Client) {
					configureReconciledKubernetesAccess(t, r, author, input.Agent.Name, input.Policy.Name)
					installKubernetesFixtureCA(t, r, author, input.Agent.Name)
				})
			f.reconcileOK(t)
			assertKubernetesFixtureGrants(t, f, true)
			expected := reconciledExpectationForTest(t, f)
			if !slices.ContainsFunc(expected.Controls, func(object unstructured.Unstructured) bool {
				return object.GetKind() == "DestinationRule"
			}) {
				t.Fatal("Kubernetes access omitted the managed ingress TLS control")
			}
			refs, err := f.r.sandboxGrantJournal().Read(context.Background(), f.agent.Name, f.agent.UID)
			if err != nil || len(refs) != 4 {
				t.Fatalf("expected two source-reader and two workload grants, got %d: %v", len(refs), err)
			}
			for _, ref := range refs {
				if ref.UID == "" {
					t.Fatal("combined Kubernetes and TLS access left an unconfirmed grant")
				}
			}
		})
	}
}

func TestReconciledSandboxKubernetesAccess(t *testing.T) {
	testReconciledSandboxKubernetesAccess(t)
}

func TestContainerSandboxKubernetesAccess(t *testing.T) {
	testReconciledSandboxKubernetesAccess(t, withContainerRuntime(t))
}

func testReconciledSandboxKubernetesAccess(t *testing.T, setup ...func(*AgentReconciler, client.Client)) {
	t.Helper()
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
			t.Run(kind+"/"+string(mode), func(t *testing.T) {
				f := newReconciledKubernetesFixture(t, kind, mode, setup...)
				f.reconcileOK(t)
				assertKubernetesFixtureGrants(t, f, true)
				assertNativeHomeAccessMode(t, f, mode)
				expected := reconciledExpectationForTest(t, f)
				for _, role := range []string{"kubernetes-pods-reader", "kubernetes-nodes-reader"} {
					if !slices.ContainsFunc(expected.Sources, func(source w.SourceReference) bool {
						return source.IsClusterRole() && source.Name == role && source.ValidIdentity()
					}) {
						t.Fatal("current admission omitted its approved role pin")
					}
				}
				if !slices.ContainsFunc(expected.Sources, func(source w.SourceReference) bool {
					return source.Kind == "ConfigMap" && source.Name == "kube-root-ca.crt"
				}) {
					t.Fatal("current admission omitted the Kubernetes CA source")
				}
				pod := reconciledPodForTest(t, f, expected.Workloads[0])
				if pod.Spec.ServiceAccountName != kubernetesFixtureAccount || ptr.Deref(pod.Spec.AutomountServiceAccountToken, true) {
					t.Fatal("common workload did not use the explicit non-automount identity")
				}
				handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{StateNamespace: f.r.OperatorNamespace})
				if err != nil {
					t.Fatal(err)
				}
				if response := handler.Validate(context.Background(), reconciledPodRequest(t, pod)); !response.Allowed {
					t.Fatal("current Kubernetes Pod was rejected:", response.Result)
				}
				for name, mutate := range map[string]func(*corev1.Pod){
					"account":   func(p *corev1.Pod) { p.Spec.ServiceAccountName = "default" },
					"automount": func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) },
					"audience": func(p *corev1.Pod) {
						for index := range p.Spec.Volumes {
							if p.Spec.Volumes[index].Name == "sandbox-kubernetes-api" {
								p.Spec.Volumes[index].Projected.Sources[0].ServiceAccountToken.Audience = "other-service"
							}
						}
					},
					"helper mount": func(p *corev1.Pod) {
						p.Spec.InitContainers[0].VolumeMounts = append(p.Spec.InitContainers[0].VolumeMounts,
							corev1.VolumeMount{Name: "sandbox-kubernetes-api", MountPath: "/unapproved", ReadOnly: true})
					},
				} {
					t.Run(name, func(t *testing.T) {
						changed := pod.DeepCopy()
						mutate(changed)
						if response := handler.Validate(context.Background(), reconciledPodRequest(t, changed)); response.Allowed {
							t.Fatal("Pod admission accepted altered Kubernetes identity")
						}
					})
				}
			})
		}
	}
}

func TestReconciledSandboxKubernetesRevocationAndResume(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newReconciledKubernetesFixture(t, kind, corev1.ReadWriteOnce)
			before := reconciledExpectationForTest(t, f)
			old := assertKubernetesFixtureGrants(t, f, true)
			agent := f.current(t)
			agent.Spec.Suspended, agent.Generation = true, agent.Generation+1
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			assertKubernetesFixtureGrants(t, f, false)
			for _, ref := range old {
				object := &unstructured.Unstructured{}
				object.SetAPIVersion(rbacv1.SchemeGroupVersion.String())
				object.SetKind(ref.Kind)
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, object); !apierrors.IsNotFound(err) {
					t.Fatal("suspension recreated or retained a Kubernetes grant:", err)
				}
			}
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || f.current(t).Status.Phase != api.PhaseSuspended {
				t.Fatal("Kubernetes access suspension did not stop compute")
			}
			agent = f.current(t)
			agent.Spec.Suspended, agent.Generation = false, agent.Generation+1
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				f.reconcileOK(t)
			}
			resumed := assertKubernetesFixtureGrants(t, f, true)
			for _, ref := range resumed {
				if slices.ContainsFunc(old, func(previous sandboxpolicy.BindingReference) bool { return previous.UID == ref.UID }) {
					t.Fatal("resume silently reused a revoked binding UID")
				}
			}
			policy := &api.AgentPolicy{}
			policyName := testfixture.New(kind).Policy.Name
			if err := f.kube.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
				t.Fatal(err)
			}
			policy.Spec.KubernetesAccess, policy.Generation = nil, policy.Generation+1
			if err := f.kube.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			refreshKubernetesFixturePolicy(t, f.r, policyName)
			for range 3 {
				f.reconcileOK(t)
			}
			assertKubernetesFixtureGrants(t, f, false)
			after := reconciledExpectationForTest(t, f)
			if after.Home != before.Home || f.workload(t).Spec.Template.Spec.ServiceAccountName != "default" ||
				slices.ContainsFunc(f.workload(t).Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == "sandbox-kubernetes-api" }) {
				t.Fatal("nil access failed to preserve HOME and restore token-free isolation")
			}
		})
	}
}

func TestReconciledSandboxKubernetesRoleDriftStopsExecution(t *testing.T) {
	for _, mutation := range []string{"rules", "resourceVersion", "replacement"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			f := newReconciledKubernetesFixture(t, "openclaw", corev1.ReadWriteOncePod)
			expected := reconciledExpectationForTest(t, f)
			pod := reconciledPodForTest(t, f, expected.Workloads[0])
			handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{StateNamespace: f.r.OperatorNamespace})
			if err != nil {
				t.Fatal(err)
			}
			role := &rbacv1.ClusterRole{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: "kubernetes-pods-reader"}, role); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "rules":
				role.Rules[0].Verbs = append(role.Rules[0].Verbs, "delete")
			case "resourceVersion":
				role.Annotations = map[string]string{"example.com/review": "changed"}
			case "replacement":
				role.UID = "replacement-cluster-role"
			}
			if err := f.kube.Update(ctx, role); err != nil {
				t.Fatal(err)
			}
			if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); response.Allowed {
				t.Fatal("current Pod admission ignored an approved role's UID/version/rules drift")
			}
			_, _ = f.reconcile()
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("role approval drift did not stop the existing workload")
			}
			recorded, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID)
			if err != nil || len(recorded) != 0 {
				t.Fatal("role drift retained workload or admission grants:", err)
			}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(role), role); err != nil {
				t.Fatal("revocation deleted the shared ClusterRole:", err)
			}
		})
	}
}

func TestReconciledSandboxKubernetesPartialApplyIsRevoked(t *testing.T) {
	ctx := context.Background()
	fixture := testfixture.New("hermes")
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "hermes", true, false,
		func(r *AgentReconciler, author client.Client) {
			configureReconciledKubernetesAccess(t, r, author, fixture.Agent.Name, fixture.Policy.Name)
			installKubernetesFixtureCA(t, r, author, fixture.Agent.Name)
		})
	injected := errors.New("synthetic lost Kubernetes binding acknowledgement")
	wrote := false
	f.installer.kube = interceptor.NewClient(f.kube, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if object.GetObjectKind().GroupVersionKind().Kind == "RoleBinding" && object.GetNamespace() == "operations" {
				refs, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID)
				if err != nil || !slices.ContainsFunc(refs, func(ref sandboxpolicy.BindingReference) bool {
					return ref.Name == object.GetName() && ref.Namespace == object.GetNamespace() && ref.UID == ""
				}) {
					t.Fatal("Kubernetes binding write preceded its durable intent:", err)
				}
				if err := c.Create(ctx, object, opts...); err != nil {
					return err
				}
				wrote = true
				return injected
			}
			return c.Create(ctx, object, opts...)
		},
	})
	var err error
	for range 4 {
		_, err = f.reconcile()
		if err != nil {
			break
		}
	}
	if !wrote || !errors.Is(err, injected) {
		t.Fatal("fixture did not exercise a lost binding write acknowledgement:", err)
	}
	recorded, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID)
	if err != nil || len(recorded) != 0 {
		t.Fatal("partial apply did not revoke its journaled bindings:", err)
	}
	f.installer.kube = f.kube
	for range 3 {
		f.reconcileOK(t)
	}
	assertKubernetesFixtureGrants(t, f, true)
}

func TestReconciledSandboxKubernetesPostApplyApprovalRace(t *testing.T) {
	f := newReconciledKubernetesFixture(t, "openclaw", corev1.ReadWriteOncePod)
	ctx := context.Background()
	installer := f.r.HelmClient.(*sharedSandboxInstaller)
	installer.beforeApply = func(*helm.PreparedRelease) error {
		role := &rbacv1.ClusterRole{}
		if err := f.kube.Get(ctx, client.ObjectKey{Name: "kubernetes-pods-reader"}, role); err != nil {
			return err
		}

		role.Rules[0].Verbs = append(role.Rules[0].Verbs, "delete")
		return f.kube.Update(ctx, role)
	}
	agent := f.current(t)
	agent.Generation++
	agent.Spec.Model = testSharedModelOne
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil {
		t.Fatal("an approval race at the final apply guard was accepted")
	}
	if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
		t.Fatal("approval race left compute running")
	}
}

func TestReconciledSandboxKubernetesRenderingCannotOmitIdentity(t *testing.T) {
	for _, field := range []string{"identityVolumes", "identityMounts"} {
		t.Run(field, func(t *testing.T) {
			fixture := testfixture.New("hermes")
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "hermes", true, false,
				func(r *AgentReconciler, author client.Client) {
					configureReconciledKubernetesAccess(t, r, author, fixture.Agent.Name, fixture.Policy.Name)
					installKubernetesFixtureCA(t, r, author, fixture.Agent.Name)
					chrt := r.HelmClient.(*sharedSandboxInstaller).chart
					for _, template := range chrt.Templates {
						if template.Name == "templates/statefulset.yaml" {
							template.Data = []byte(strings.ReplaceAll(string(template.Data), "."+field, ".omittedIdentity"))
						}
					}
				})
			var err error
			for range 4 {
				_, err = f.reconcile()
				if err != nil {
					break
				}
			}
			if err == nil || !strings.Contains(err.Error(), "identity") || f.installer.stages != 0 {
				t.Fatal("changed chart was allowed to omit the requested identity:", err)
			}
		})
	}
}

func TestReconciledSandboxKubernetesForeignGrantBlocksCleanupNotShutdown(t *testing.T) {
	ctx := context.Background()
	f := newReconciledKubernetesFixture(t, "hermes", corev1.ReadWriteOncePod)
	refs := assertKubernetesFixtureGrants(t, f, true)
	ref := refs[slices.IndexFunc(refs, func(ref sandboxpolicy.BindingReference) bool { return ref.Kind == "RoleBinding" })]
	binding := &rbacv1.RoleBinding{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, binding); err != nil {
		t.Fatal(err)
	}
	binding.Subjects = append(binding.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Namespace: "operations", Name: "other"})
	if err := f.kube.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	agent := f.current(t)
	agent.Spec.Suspended, agent.Generation = true, agent.Generation+1
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil {
		t.Fatal("shared binding was silently deleted or adopted during suspension")
	}
	if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
		t.Fatal("a binding cleanup failure skipped independent workload shutdown")
	}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil || len(binding.Subjects) != 2 {
		t.Fatal("cleanup changed a shared binding:", err)
	}
	recorded, err := f.r.sandboxGrantJournal().Read(ctx, agent.Name, agent.UID)
	if err != nil || !slices.ContainsFunc(recorded, func(old sandboxpolicy.BindingReference) bool { return old.UID == ref.UID }) {
		t.Fatal("failed cleanup lost its durable binding inventory:", err)
	}
}

func TestReconciledSandboxKubernetesMissingJournalBlocksDeletion(t *testing.T) {
	ctx := context.Background()
	f := newReconciledKubernetesFixture(t, "openclaw", corev1.ReadWriteOncePod)
	refs := assertKubernetesFixtureGrants(t, f, true)
	journal := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, client.ObjectKey{
		Namespace: f.r.OperatorNamespace, Name: sandboxpolicy.GrantJournalName(f.agent.UID),
	}, journal); err != nil {
		t.Fatal(err)
	}
	// Model lost protected state, not an authorized journal deletion.
	journal.Finalizers = nil
	if err := f.kube.Update(ctx, journal); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, journal); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "grant journal") {
		t.Fatal("missing inventory was treated as evidence that external grants were revoked:", err)
	}
	if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || len(f.current(t).Finalizers) == 0 {
		t.Fatal("missing journal bypassed shutdown or released cleanup ownership")
	}
	for _, ref := range refs {
		object := &unstructured.Unstructured{}
		object.SetAPIVersion(rbacv1.SchemeGroupVersion.String())
		object.SetKind(ref.Kind)
		if err := f.kube.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, object); err != nil {
			t.Fatal("missing inventory authorized unjournaled binding deletion:", err)
		}
	}
}

func TestReconciledSandboxKubernetesDisabledSourcesStillRevoke(t *testing.T) {
	ctx := context.Background()
	f := newReconciledKubernetesFixture(t, "hermes", corev1.ReadWriteOncePod)
	refs := assertKubernetesFixtureGrants(t, f, true)
	f.r.SandboxPreparation = nil
	_, _ = f.reconcile()
	if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
		t.Fatal("disabling new starts skipped shutdown of the recorded workload")
	}
	recorded, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID)
	if err != nil || len(recorded) != 0 {
		t.Fatal("disabling sources left recorded Kubernetes grants active:", err)
	}
	for _, ref := range refs {
		object := &unstructured.Unstructured{}
		object.SetAPIVersion(rbacv1.SchemeGroupVersion.String())
		object.SetKind(ref.Kind)
		if err := f.kube.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, object); !apierrors.IsNotFound(err) {
			t.Fatal("recorded Kubernetes access survived disabled sources:", err)
		}
	}
}
