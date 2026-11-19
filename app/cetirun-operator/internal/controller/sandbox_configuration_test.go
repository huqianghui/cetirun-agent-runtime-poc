// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

func sandboxConfigurationFixture() (*api.Sandbox, controller.SandboxConfigurationSnapshot, []client.Object) {
	resources := api.SandboxContainerResources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
	}
	object := &api.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "example-sandbox", UID: "example-sandbox-uid", Generation: 4},
		Spec: api.SandboxSpec{
			Type: api.SandboxKata, Resources: api.SandboxResources{Agent: resources, Helpers: resources},
			Storage: api.SandboxStorage{
				Home:    api.SandboxHomeStorage{StorageClassName: "example-home", Size: resource.MustParse("10Gi"), AccessMode: corev1.ReadWriteOncePod},
				Scratch: api.SandboxScratchStorage{SizeLimit: resource.MustParse("1Gi")},
			},
		},
	}
	capabilities := sandbox.Capabilities{
		RuntimeClassName: "example-kata", Revision: "synthetic-qualified",
		HostResourceLimits: true, ReadOnlyRootFilesystem: true,
		HomeAccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
		IPFamilies:           []corev1.IPFamily{corev1.IPv4Protocol},
		Protocols:            []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP},
		GuestExternalNetwork: true, DenyPrecedence: true, NodeTraffic: true, DNSIPRestrictions: true,
		BlockedPorts: true, AuthenticatedIngress: true, DisabledIngress: true,
		EnforcementObservation: true, RestrictiveFailure: true, KubernetesAccess: true, EntraID: true,
	}
	platform := sandboxpolicy.Platform{
		Adapter: sandboxpolicy.AdapterRegistration{Name: sandboxpolicy.AzureCNIOverlayCilium, Qualified: true, Available: true, Revision: "synthetic-only"},
		Topology: sandboxpolicy.Topology{
			Revision: "synthetic-topology", ManagedPeersComplete: true,
			PodCIDRs: []string{"10.244.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
			NodeCIDRs: []string{"192.168.0.0/16"}, ManagementCIDRs: []string{"10.1.0.0/16"},
			MetadataCIDRs:  []string{"169.254.169.254/32"},
			DNSResolverIPs: []string{"10.96.0.10"},
			DNS:            []sandboxpolicy.Peer{{Name: "dns", Namespace: "kube-system", UID: "example-dns", IPs: []string{"10.96.0.10"}}},
			Gateway: []sandboxpolicy.Destination{{
				Peer:  sandboxpolicy.Peer{Name: "gateway", Namespace: "platform", UID: "example-gateway", IPs: []string{"10.96.1.50"}},
				Ports: []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}},
			}},
			Ingress: []sandboxpolicy.Destination{{
				Peer:  sandboxpolicy.Peer{Name: "ingress", Namespace: "platform", UID: "example-ingress", IPs: []string{"10.96.1.60"}},
				Ports: []api.AgentPolicyPort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
			}},
		},
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	snapshot := controller.SandboxConfigurationSnapshot{
		ObservedAt:    now,
		Registrations: map[api.SandboxType]sandbox.Registration{api.SandboxKata: {Available: true, Capabilities: capabilities}},
		Platforms:     map[api.SandboxType]sandboxpolicy.Platform{api.SandboxKata: platform},
		RuntimeClasses: map[string]controller.SandboxRuntimePrerequisite{
			"example-kata": {UID: "example-runtime-uid", Handler: "kata"},
		},
		StorageClasses: map[string]controller.SandboxStoragePrerequisite{
			"example-home": {UID: "example-storage-uid", Provisioner: "csi.example.com"},
		},
	}
	objects := []client.Object{
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "example-kata", UID: "example-runtime-uid"}, Handler: "kata"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "example-home", UID: "example-storage-uid"}, Provisioner: "csi.example.com"},
	}
	return object, snapshot, objects
}

func TestConfigurationChangePredicateIgnoresStatusOnlyUpdates(t *testing.T) {
	filter := controller.ConfigurationChangePredicate()
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "engineering", UID: "team-uid", Generation: 3}}
	statusOnly := team.DeepCopy()
	statusOnly.Status.Compatibility = &api.TeamCompatibilityStatus{State: api.CompatibilityCompatible}
	if filter.Update(event.UpdateEvent{ObjectOld: team, ObjectNew: statusOnly}) {
		t.Fatal("status-only update requeued Team compatibility reconciliation")
	}
	for name, changed := range map[string]*api.Team{
		"generation": func() *api.Team { object := team.DeepCopy(); object.Generation++; return object }(),
		"uid":        func() *api.Team { object := team.DeepCopy(); object.UID = "replacement-uid"; return object }(),
		"deletion": func() *api.Team {
			object := team.DeepCopy()
			object.DeletionTimestamp = &metav1.Time{Time: time.Now().UTC()}
			return object
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if !filter.Update(event.UpdateEvent{ObjectOld: team, ObjectNew: changed}) {
				t.Fatal("meaningful configuration update was filtered")
			}
		})
	}
	if !filter.Create(event.CreateEvent{Object: team}) || !filter.Delete(event.DeleteEvent{Object: team}) {
		t.Fatal("create or delete event was filtered")
	}
	if filter.Generic(event.GenericEvent{Object: team}) {
		t.Fatal("generic event must not bypass configuration refresh cadence")
	}
}

func nativePolicyHarnesses() api.AgentPolicyHarnesses {
	return api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes}}
}

func nativeConfigurationSnapshot() (*api.Sandbox, controller.SandboxConfigurationSnapshot, []client.Object) {
	box, snapshot, objects := sandboxConfigurationFixture()
	registration := snapshot.Registrations[api.SandboxKata]
	registration.Capabilities.Profile = sandbox.ProfileNativeSingleStart
	registration.Capabilities.EnforcementObservation, registration.Capabilities.RestrictiveFailure = false, false
	registration.Capabilities.BlockedPorts, registration.Capabilities.KubernetesAccess, registration.Capabilities.EntraID = false, false, false
	snapshot.Registrations[api.SandboxKata] = registration
	platform := snapshot.Platforms[api.SandboxKata]
	platform.Adapter.Name, platform.Adapter.Qualified = sandboxpolicy.Cilium, false
	snapshot.Platforms[api.SandboxKata] = platform
	return box, snapshot, objects
}

func TestNativeConfigurationRequiresExplicitExecutionMode(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "default rejects native", true: "native accepts baseline"}[native], func(t *testing.T) {
			box, snapshot, objects := nativeConfigurationSnapshot()
			policy := &api.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "example-policy", UID: "example-policy-uid", Generation: 1},
				Spec:       api.AgentPolicySpec{Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}}},
			}
			kube := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(box, policy).
				WithObjects(append(objects, box, policy)...).Build()
			source := controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) {
				return snapshot, nil
			})
			now := func() time.Time { return snapshot.ObservedAt }
			boxController := &controller.SandboxReconciler{Client: kube, Source: source, Now: now, NativeExecution: native}
			policyController := &controller.AgentPolicyReconciler{Client: kube, Source: source, Now: now, NativeExecution: native}
			ctx := context.Background()
			if _, err := boxController.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(box)}); err != nil {
				t.Fatal(err)
			}
			if _, err := policyController.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
				t.Fatal(err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(box), box); err != nil {
				t.Fatal(err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			want := metav1.ConditionFalse
			if native {
				want = metav1.ConditionTrue
			}
			for _, conditions := range [][]metav1.Condition{box.Status.Conditions, policy.Status.Conditions} {
				if len(conditions) != 1 || conditions[0].Status != want ||
					(!native && conditions[0].Reason != "UnsupportedControl") {
					t.Fatalf("configuration status ignored execution mode: %+v", conditions)
				}
			}
		})
	}
}

func configurationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, nodev1.AddToScheme, rbacv1.AddToScheme, storagev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func TestSandboxConfigurationPrerequisiteConditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.Sandbox, *controller.SandboxConfigurationSnapshot, *[]client.Object)
		status metav1.ConditionStatus
		reason string
	}{
		{"ready prerequisites", func(*api.Sandbox, *controller.SandboxConfigurationSnapshot, *[]client.Object) {}, metav1.ConditionTrue, "PrerequisitesReady"},
		{"invalid configuration", func(o *api.Sandbox, _ *controller.SandboxConfigurationSnapshot, _ *[]client.Object) {
			o.Spec.Resources.Agent.Limits = nil
		}, metav1.ConditionFalse, "InvalidConfiguration"},
		{"unregistered runtime", func(_ *api.Sandbox, s *controller.SandboxConfigurationSnapshot, _ *[]client.Object) {
			s.Registrations = nil
		}, metav1.ConditionFalse, "UnsupportedControl"},
		{"runtime unavailable", func(_ *api.Sandbox, s *controller.SandboxConfigurationSnapshot, _ *[]client.Object) {
			r := s.Registrations[api.SandboxKata]
			r.Available = false
			s.Registrations[api.SandboxKata] = r
		}, metav1.ConditionFalse, "RuntimeUnavailable"},
		{"missing runtime reference", func(_ *api.Sandbox, _ *controller.SandboxConfigurationSnapshot, o *[]client.Object) { *o = (*o)[1:] }, metav1.ConditionFalse, "ReferenceMissing"},
		{"replaced runtime reference", func(_ *api.Sandbox, s *controller.SandboxConfigurationSnapshot, _ *[]client.Object) {
			s.RuntimeClasses["example-kata"] = controller.SandboxRuntimePrerequisite{UID: "old-runtime", Handler: "kata"}
		}, metav1.ConditionFalse, "ReferenceChanged"},
		{"changed runtime handler", func(_ *api.Sandbox, _ *controller.SandboxConfigurationSnapshot, o *[]client.Object) {
			(*o)[0].(*nodev1.RuntimeClass).Handler = "other-handler"
		}, metav1.ConditionFalse, "ReferenceChanged"},
		{"missing storage reference", func(_ *api.Sandbox, _ *controller.SandboxConfigurationSnapshot, o *[]client.Object) { *o = (*o)[:1] }, metav1.ConditionFalse, "ReferenceMissing"},
		{"storage not approved", func(_ *api.Sandbox, s *controller.SandboxConfigurationSnapshot, _ *[]client.Object) {
			s.StorageClasses = nil
		}, metav1.ConditionFalse, "UnapprovedStorage"},
		{"replaced storage reference", func(_ *api.Sandbox, s *controller.SandboxConfigurationSnapshot, _ *[]client.Object) {
			s.StorageClasses["example-home"] = controller.SandboxStoragePrerequisite{UID: "old-storage", Provisioner: "csi.example.com"}
		}, metav1.ConditionFalse, "ReferenceChanged"},
		{"missing platform", func(_ *api.Sandbox, s *controller.SandboxConfigurationSnapshot, _ *[]client.Object) {
			s.Platforms = nil
		}, metav1.ConditionUnknown, "PrerequisitesUnavailable"},
		{"unavailable adapter", func(_ *api.Sandbox, s *controller.SandboxConfigurationSnapshot, _ *[]client.Object) {
			p := s.Platforms[api.SandboxKata]
			p.Adapter.Available = false
			s.Platforms[api.SandboxKata] = p
		}, metav1.ConditionUnknown, "PrerequisitesUnavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object, snapshot, prerequisites := sandboxConfigurationFixture()
			tc.mutate(object, &snapshot, &prerequisites)
			objects := append([]client.Object{object}, prerequisites...)
			c := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(object).WithObjects(objects...).Build()
			r := &controller.SandboxReconciler{
				Client: c, Now: func() time.Time { return snapshot.ObservedAt },
				Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil }),
			}
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: object.Name}})
			if err != nil || result.RequeueAfter <= 0 {
				t.Fatalf("configuration reconcile: %+v, %v", result, err)
			}
			got := &api.Sandbox{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(object), got); err != nil {
				t.Fatal(err)
			}
			condition := apimeta.FindStatusCondition(got.Status.Conditions, api.ConditionReady)
			if condition == nil || condition.Status != tc.status || condition.Reason != tc.reason ||
				condition.ObservedGeneration != object.Generation || got.Status.ObservedGeneration != object.Generation {
				t.Fatalf("unexpected prerequisite status: %+v", got.Status)
			}
		})
	}
}

func TestSandboxConfigurationStableTransitionsAndSourceFreshness(t *testing.T) {
	object, snapshot, prerequisites := sandboxConfigurationFixture()
	c := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(object).
		WithObjects(append([]client.Object{object}, prerequisites...)...).Build()
	now := snapshot.ObservedAt
	r := &controller.SandboxReconciler{
		Client: c, Now: func() time.Time { return now },
		Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil }),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: object.Name}}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	first := &api.Sandbox{}
	if err := c.Get(context.Background(), request.NamespacedName, first); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	second := &api.Sandbox{}
	if err := c.Get(context.Background(), request.NamespacedName, second); err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != second.ResourceVersion ||
		!first.Status.Conditions[0].LastTransitionTime.Equal(&second.Status.Conditions[0].LastTransitionTime) {
		t.Fatal("unchanged prerequisite status fabricated a transition or write")
	}
	now = now.Add(time.Minute)
	if _, err := r.Reconcile(context.Background(), request); !errors.Is(err, sandboxpolicy.ErrUnavailable) {
		t.Fatal("stale platform snapshot must not remain Ready", err)
	}
	if err := c.Get(context.Background(), request.NamespacedName, second); err != nil {
		t.Fatal(err)
	}
	if second.Status.Conditions[0].Status != metav1.ConditionUnknown ||
		!second.Status.Conditions[0].LastTransitionTime.Time.Equal(now) {
		t.Fatal("lost prerequisite freshness did not produce a truthful transition")
	}
	r.Source = nil
	if _, err := r.Reconcile(context.Background(), request); !errors.Is(err, sandboxpolicy.ErrUnavailable) {
		t.Fatal("an absent production source must not imply readiness", err)
	}
}

func TestAgentPolicyAcceptedIsConfigurationOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.AgentPolicy, *controller.SandboxConfigurationSnapshot)
		status metav1.ConditionStatus
		reason string
	}{
		{"accepted", func(*api.AgentPolicy, *controller.SandboxConfigurationSnapshot) {}, metav1.ConditionTrue, "PolicyAccepted"},
		{"unsupported FQDN", func(p *api.AgentPolicy, _ *controller.SandboxConfigurationSnapshot) {
			p.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
		}, metav1.ConditionFalse, "UnsupportedControl"},
		{"invalid port", func(p *api.AgentPolicy, _ *controller.SandboxConfigurationSnapshot) {
			p.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 70000}}
		}, metav1.ConditionFalse, "InvalidConfiguration"},
		{"required DNS conflict", func(p *api.AgentPolicy, _ *controller.SandboxConfigurationSnapshot) {
			p.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 53}}
		}, metav1.ConditionFalse, "PolicyConflict"},
		{"unapproved grant", func(p *api.AgentPolicy, _ *controller.SandboxConfigurationSnapshot) {
			p.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "example-admin", ClusterRoleBindings: []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: "admin"}}}
		}, metav1.ConditionFalse, "UnapprovedGrant"},
		{"no registration", func(_ *api.AgentPolicy, s *controller.SandboxConfigurationSnapshot) { s.Registrations = nil }, metav1.ConditionFalse, "UnsupportedControl"},
		{"unavailable topology", func(_ *api.AgentPolicy, s *controller.SandboxConfigurationSnapshot) { s.Platforms = nil }, metav1.ConditionUnknown, "PrerequisitesUnavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, snapshot, _ := sandboxConfigurationFixture()
			policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "example-policy", UID: "example-policy-uid", Generation: 6}, Spec: api.AgentPolicySpec{Harnesses: nativePolicyHarnesses()}}
			tc.mutate(policy, &snapshot)
			agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example-agent", UID: "example-agent-uid"}}
			c := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(policy, agent).WithObjects(policy, agent).Build()
			r := &controller.AgentPolicyReconciler{
				Client: c, Now: func() time.Time { return snapshot.ObservedAt },
				Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil }),
			}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Name: policy.Name}}
			_, err := r.Reconcile(context.Background(), request)
			if tc.name == "unavailable topology" {
				if !errors.Is(err, sandboxpolicy.ErrUnavailable) {
					t.Fatal("unavailable topology must retain retryable classification", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			actual := &api.AgentPolicy{}
			if err := c.Get(context.Background(), request.NamespacedName, actual); err != nil {
				t.Fatal(err)
			}
			condition := apimeta.FindStatusCondition(actual.Status.Conditions, api.ConditionAgentPolicyAccepted)
			if condition == nil || condition.Status != tc.status || condition.Reason != tc.reason ||
				condition.ObservedGeneration != policy.Generation || actual.Status.ObservedGeneration != policy.Generation {
				t.Fatalf("unexpected policy configuration condition: %+v", actual.Status)
			}
			if condition.Status == metav1.ConditionTrue &&
				(!strings.Contains(condition.Message, "Standalone policy support") || !strings.Contains(condition.Message, "are not assessed")) {
				t.Fatal("standalone acceptance overstates workload readiness", condition.Message)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(agent), agent); err != nil {
				t.Fatal(err)
			}
			if agent.Status.Sandbox != nil || len(agent.Status.Conditions) != 0 {
				t.Fatal("configuration acceptance must not report an Agent's enforcement")
			}
		})
	}
}

func TestTeamDependencyReadFailureIsUnknownAndRetryable(t *testing.T) {
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "normal", UID: "normal-uid"}}
	box := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "standard-uid"}}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "team-uid", Generation: 3}, Spec: api.TeamSpec{
		AgentPolicyRef: policy.Name, DefaultSandboxRef: box.Name, SandboxRefs: []string{box.Name},
	}}
	base := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(team).
		WithObjects(policy, box, team).Build()
	failure := errors.New("synthetic Team dependency timeout")
	reader := &teamDependencyReader{Reader: base, failure: failure}
	r := &controller.TeamReconciler{Client: base, APIReader: reader}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(team)}
	if _, err := r.Reconcile(context.Background(), request); !errors.Is(err, failure) {
		t.Fatalf("dependency timeout must be returned for controller retry: %v", err)
	}
	current := &api.Team{}
	if err := base.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	condition := apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionTeamAccepted)
	if condition == nil || condition.Status != metav1.ConditionUnknown || condition.Reason != "DependenciesUnavailable" ||
		condition.ObservedGeneration != team.Generation || current.Status.ObservedGeneration != team.Generation {
		t.Fatalf("transient dependency failure became a definitive denial: %+v", current.Status)
	}

	reader.failure = nil
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	condition = apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionTeamAccepted)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("Team did not recover when its dependencies became readable: %+v", current.Status)
	}
}

func TestTeamCompatibilityStatusPinsSavedInputsAndBecomesUnknownWhenPlatformIsUnavailable(t *testing.T) {
	box, snapshot, prerequisites := sandboxConfigurationFixture()
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "normal", UID: "normal-uid", Generation: 4}, Spec: api.AgentPolicySpec{
		Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}},
	}}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "engineering", UID: "team-uid", Generation: 6}, Spec: api.TeamSpec{
		AgentPolicyRef: policy.Name, SandboxRefs: []string{box.Name}, DefaultSandboxRef: box.Name,
	}}
	base := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(team).
		WithObjects(append(prerequisites, box, policy, team)...).Build()
	source := controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil })
	r := &controller.TeamReconciler{Client: base, APIReader: base, Source: source, Now: func() time.Time { return snapshot.ObservedAt }}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(team)}
	if result, err := r.Reconcile(context.Background(), request); err != nil || result.RequeueAfter != 15*time.Second {
		t.Fatalf("compatibility reconcile = %+v, %v", result, err)
	}
	current := &api.Team{}
	if err := base.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	report := current.Status.Compatibility
	if report == nil || report.State != api.CompatibilityCompatible || report.Team.Generation != team.Generation ||
		report.Policy.Generation != policy.Generation || len(report.Sandboxes) != 1 || report.Sandboxes[0].Sandbox.Generation != box.Generation ||
		!report.ValidUntil.Time.After(report.CheckedAt.Time) {
		t.Fatalf("compatibility report = %+v", report)
	}
	r.Source = controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) {
		return controller.SandboxConfigurationSnapshot{}, errors.New("inventory unavailable")
	})
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("unavailable compatibility inventory must not change Team acceptance: %v", err)
	}
	if err := base.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if report = current.Status.Compatibility; report == nil || report.State != api.CompatibilityUnknown || !report.ValidUntil.Time.After(report.CheckedAt.Time) {
		t.Fatalf("unavailable compatibility report = %+v", report)
	}
	condition := apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionTeamAccepted)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("compatibility observation changed Team acceptance: %+v", current.Status)
	}
}

func TestTeamCompatibilityUsesClockAfterPlatformSnapshot(t *testing.T) {
	box, snapshot, prerequisites := sandboxConfigurationFixture()
	before := snapshot.ObservedAt.Add(-time.Second)
	after := snapshot.ObservedAt.Add(time.Second)
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "normal", UID: "normal-uid", Generation: 4}, Spec: api.AgentPolicySpec{
		Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}},
	}}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "engineering", UID: "team-uid", Generation: 6}, Spec: api.TeamSpec{
		AgentPolicyRef: policy.Name, SandboxRefs: []string{box.Name}, DefaultSandboxRef: box.Name,
	}}
	base := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(team).
		WithObjects(append(prerequisites, box, policy, team)...).Build()
	clockCalls := 0
	r := &controller.TeamReconciler{
		Client: base, APIReader: base,
		Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil }),
		Now: func() time.Time {
			clockCalls++
			if clockCalls == 1 {
				return before
			}
			return after
		},
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(team)}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	current := &api.Team{}
	if err := base.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Compatibility == nil || current.Status.Compatibility.State != api.CompatibilityCompatible {
		t.Fatalf("snapshot observed after the initial clock read became unavailable: %+v", current.Status.Compatibility)
	}
}

func TestUnconfiguredAndTerminatingTeamsDoNotRetainOrRefreshCompatibility(t *testing.T) {
	now := time.Now().UTC()
	for name, team := range map[string]*api.Team{
		"unconfigured": {ObjectMeta: metav1.ObjectMeta{Name: "legacy"}},
		"terminating": {
			ObjectMeta: metav1.ObjectMeta{Name: "terminating", Finalizers: []string{"test.example/finalizer"}, DeletionTimestamp: &metav1.Time{Time: now}},
			Spec:       api.TeamSpec{AgentPolicyRef: "policy", DefaultSandboxRef: "sandbox", SandboxRefs: []string{"sandbox"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			team.Status.Compatibility = &api.TeamCompatibilityStatus{State: api.CompatibilityCompatible}
			base := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(team).WithObjects(team).Build()
			sourceCalls := 0
			r := &controller.TeamReconciler{Client: base, APIReader: base, Now: func() time.Time { return now },
				Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) {
					sourceCalls++
					return controller.SandboxConfigurationSnapshot{}, nil
				}),
			}
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(team)})
			if err != nil {
				t.Fatal(err)
			}
			if result.RequeueAfter != 0 || sourceCalls != 0 {
				t.Fatalf("non-active Team compatibility was refreshed: result=%+v sourceCalls=%d", result, sourceCalls)
			}
			current := &api.Team{}
			if err := base.Get(context.Background(), client.ObjectKeyFromObject(team), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Compatibility != nil {
				t.Fatalf("non-active Team retained compatibility: %+v", current.Status.Compatibility)
			}
		})
	}
}

func TestConfigurationReadFailureAndStatusFailureAreNotSuccess(t *testing.T) {
	object, snapshot, prerequisites := sandboxConfigurationFixture()
	base := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(object).
		WithObjects(append([]client.Object{object}, prerequisites...)...).Build()
	failure := errors.New("synthetic prerequisite read failure")
	reader := &configurationErrorReader{Reader: base, failure: failure}
	r := &controller.SandboxReconciler{
		Client: base, APIReader: reader, Now: func() time.Time { return snapshot.ObservedAt },
		Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil }),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: object.Name}}
	if _, err := r.Reconcile(context.Background(), request); !errors.Is(err, failure) {
		t.Fatal("prerequisite read failure was hidden", err)
	}
	current := &api.Sandbox{}
	if err := base.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Conditions[0].Status != metav1.ConditionUnknown {
		t.Fatal("unreadable prerequisite was accepted")
	}
	reader.failure = nil
	r.Client = &configurationErrorClient{Client: base, failure: failure}
	if _, err := r.Reconcile(context.Background(), request); !errors.Is(err, failure) {
		t.Fatal("status persistence failure was hidden", err)
	}
	if err := base.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Conditions[0].Status != metav1.ConditionUnknown {
		t.Fatal("failed status write appeared committed")
	}
}

func TestAgentPolicyConfigurationPreservesRegistrationFailureCauses(t *testing.T) {
	for _, unavailable := range []api.SandboxType{api.SandboxGVisor, api.SandboxKata} {
		t.Run(string(unavailable), func(t *testing.T) {
			_, snapshot, _ := sandboxConfigurationFixture()
			snapshot.Registrations[api.SandboxGVisor] = snapshot.Registrations[api.SandboxKata]
			snapshot.Platforms[api.SandboxGVisor] = snapshot.Platforms[api.SandboxKata]
			for kind, registration := range snapshot.Registrations {
				if kind == unavailable {
					delete(snapshot.Platforms, kind)
				} else {
					registration.Capabilities.GuestExternalNetwork = false
					snapshot.Registrations[kind] = registration
				}
			}
			policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "example-policy", UID: "example-policy-uid"}, Spec: api.AgentPolicySpec{Harnesses: nativePolicyHarnesses()}}
			c := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(policy).WithObjects(policy).Build()
			r := &controller.AgentPolicyReconciler{
				Client: c, Now: func() time.Time { return snapshot.ObservedAt },
				Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil }),
			}
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
			if !errors.Is(err, sandboxpolicy.ErrUnavailable) || !errors.Is(err, sandboxpolicy.ErrUnsupportedControl) || result.RequeueAfter != 0 {
				t.Fatalf("registration failure order lost causes or bypassed backoff: %+v, %v", result, err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			condition := apimeta.FindStatusCondition(policy.Status.Conditions, api.ConditionAgentPolicyAccepted)
			if condition == nil || condition.Status != metav1.ConditionUnknown || condition.Reason != "PrerequisitesUnavailable" {
				t.Fatal("a terminal registration masked unavailable evidence", condition)
			}
		})
	}
}

func configurationGrantFixture() (*api.AgentPolicy, controller.SandboxConfigurationSnapshot, *rbacv1.ClusterRole) {
	_, snapshot, _ := sandboxConfigurationFixture()
	policy := &api.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "example-policy", UID: "example-policy-uid", Generation: 3},
		Spec: api.AgentPolicySpec{Harnesses: nativePolicyHarnesses(), KubernetesAccess: &api.AgentPolicyAdminKubernetesAccess{
			ServiceAccountName: "example-admin",
			RoleBindings:       []api.AgentPolicyRoleBinding{{Namespace: "operations", ClusterRoleName: "example-view"}},
		}},
	}
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "example-view", UID: "example-role-uid", ResourceVersion: "7"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}},
	}
	platform := snapshot.Platforms[api.SandboxKata]
	platform.KubernetesAudience = "https://kubernetes.default.svc"
	platform.Topology.KubernetesAPI = []sandboxpolicy.Destination{{
		Peer:  sandboxpolicy.Peer{Name: "api", UID: "example-api-uid", IPs: []string{"203.0.113.10"}},
		Ports: []api.AgentPolicyPort{{Port: 6443, Protocol: corev1.ProtocolTCP}},
	}}
	platform.Grants = []sandboxpolicy.ApprovedGrant{{
		PolicyUID: policy.UID, Namespace: "operations",
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		RoleUID: role.UID, RoleResourceVersion: role.ResourceVersion, Revision: "synthetic-approval", Rules: role.DeepCopy().Rules,
	}}
	snapshot.Platforms[api.SandboxKata] = platform
	return policy, snapshot, role
}

func TestAgentPolicyConfigurationVerifiesAuthoritativeRoleObservations(t *testing.T) {
	failure := errors.New("synthetic authoritative role read failure")
	for _, tc := range []struct {
		name   string
		mutate func(*rbacv1.ClusterRole)
		status metav1.ConditionStatus
		reason string
	}{
		{"approved", func(*rbacv1.ClusterRole) {}, metav1.ConditionTrue, "PolicyAccepted"},
		{"rules changed", func(r *rbacv1.ClusterRole) { r.Rules[0].Resources = []string{"secrets"} }, metav1.ConditionFalse, "UnapprovedGrant"},
		{"UID changed", func(r *rbacv1.ClusterRole) { r.UID = "replacement-role" }, metav1.ConditionFalse, "UnapprovedGrant"},
		{"RV changed", func(r *rbacv1.ClusterRole) { r.ResourceVersion = "8" }, metav1.ConditionFalse, "UnapprovedGrant"},
		{"missing", func(*rbacv1.ClusterRole) {}, metav1.ConditionFalse, "UnapprovedGrant"},
		{"read failure", func(*rbacv1.ClusterRole) {}, metav1.ConditionUnknown, "PrerequisitesUnavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, snapshot, role := configurationGrantFixture()
			scheme := configurationScheme(t)
			cached := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(policy).WithObjects(policy, role.DeepCopy()).Build()
			tc.mutate(role)
			var objects []client.Object
			if tc.name != "missing" {
				objects = append(objects, role)
			}
			direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			reader := &configurationPrerequisiteReader{Reader: direct}
			if tc.name == "read failure" {
				reader.failure = failure
			}
			r := &controller.AgentPolicyReconciler{
				Client: cached, APIReader: reader, Now: func() time.Time { return snapshot.ObservedAt },
				Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil }),
			}
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
			if tc.name == "read failure" {
				if !errors.Is(err, sandboxpolicy.ErrUnavailable) || !errors.Is(err, failure) || result.RequeueAfter != 0 {
					t.Fatalf("read failure lost causes/backoff: %+v, %v", result, err)
				}
			} else if err != nil || result.RequeueAfter <= 0 {
				t.Fatalf("configuration assessment: %+v, %v", result, err)
			}
			if err := cached.Get(context.Background(), client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			condition := apimeta.FindStatusCondition(policy.Status.Conditions, api.ConditionAgentPolicyAccepted)
			if condition == nil || condition.Status != tc.status || condition.Reason != tc.reason {
				t.Fatal("cached approved role masked authoritative observation", condition)
			}
		})
	}
}

func TestSandboxConfigurationUsesCacheOnlyForPrimaryObject(t *testing.T) {
	object, snapshot, prerequisites := sandboxConfigurationFixture()
	scheme := configurationScheme(t)
	cached := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(object).
		WithObjects(append([]client.Object{object}, prerequisites...)...).Build()
	prerequisites[0] = prerequisites[0].DeepCopyObject().(client.Object)
	prerequisites[0].SetUID("replacement-runtime")
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(prerequisites...).Build()
	r := &controller.SandboxReconciler{
		Client: cached, APIReader: &configurationPrerequisiteReader{Reader: direct}, Now: func() time.Time { return snapshot.ObservedAt },
		Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil }),
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
		t.Fatal(err)
	}
	if err := cached.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
	condition := apimeta.FindStatusCondition(object.Status.Conditions, api.ConditionReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ReferenceChanged" {
		t.Fatal("cached prerequisite identity became authoritative success", condition)
	}
}

func TestConfigurationStalePrimaryCannotPublishAcceptance(t *testing.T) {
	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			object, snapshot, prerequisites := sandboxConfigurationFixture()
			policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "example-policy", UID: "example-policy-uid", Generation: 1}, Spec: api.AgentPolicySpec{Harnesses: nativePolicyHarnesses()}}
			base := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(object, policy).
				WithObjects(append([]client.Object{object, policy}, prerequisites...)...).Build()
			ctx := context.Background()
			if err := base.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
				t.Fatal(err)
			}
			if err := base.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			cached := &configurationStaleClient{Client: base, sandbox: object.DeepCopy(), policy: policy.DeepCopy()}
			source := controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return snapshot, nil })
			var request ctrl.Request
			var reconcile func(context.Context, ctrl.Request) (ctrl.Result, error)
			if kind == "Sandbox" {
				object.Generation++
				object.Spec.Storage.Home.StorageClassName = "unapproved-home"
				if err := base.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
				r := &controller.SandboxReconciler{Client: cached, APIReader: base, Source: source, Now: func() time.Time { return snapshot.ObservedAt }}
				request, reconcile = ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}, r.Reconcile
			} else {
				policy.Generation++
				policy.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 53}}
				if err := base.Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
				r := &controller.AgentPolicyReconciler{Client: cached, APIReader: base, Source: source, Now: func() time.Time { return snapshot.ObservedAt }}
				request, reconcile = ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}, r.Reconcile
			}
			if _, err := reconcile(ctx, request); !apierrors.IsConflict(err) {
				t.Fatal("stale primary observation was allowed to publish success", err)
			}
			if err := base.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
				t.Fatal(err)
			}
			if err := base.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			if len(object.Status.Conditions) != 0 || len(policy.Status.Conditions) != 0 {
				t.Fatal("stale status was persisted for a newer primary generation")
			}
		})
	}
}

func TestConfigurationSnapshotMustRemainFreshThroughAssessment(t *testing.T) {
	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			object, snapshot, prerequisites := sandboxConfigurationFixture()
			policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "example-policy", UID: "example-policy-uid"}, Spec: api.AgentPolicySpec{Harnesses: nativePolicyHarnesses()}}
			c := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(object, policy).
				WithObjects(append([]client.Object{object, policy}, prerequisites...)...).Build()
			now := snapshot.ObservedAt
			source := controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) {
				now = snapshot.ObservedAt.Add(30 * time.Second)
				return snapshot, nil
			})
			var err error
			var result ctrl.Result
			if kind == "Sandbox" {
				r := &controller.SandboxReconciler{Client: c, Source: source, Now: func() time.Time { return now }}
				result, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)})
			} else {
				r := &controller.AgentPolicyReconciler{Client: c, Source: source, Now: func() time.Time { return now }}
				result, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
			}
			if !errors.Is(err, sandboxpolicy.ErrUnavailable) || result.RequeueAfter != 0 {
				t.Fatalf("expired evidence became periodic success: %+v, %v", result, err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			conditions := object.Status.Conditions
			if kind == "AgentPolicy" {
				conditions = policy.Status.Conditions
			}
			if len(conditions) != 1 || conditions[0].Status != metav1.ConditionUnknown {
				t.Fatal("freshness loss during assessment was not reported", conditions)
			}
		})
	}
}

func TestConfigurationPreservesObservationAndStatusErrors(t *testing.T) {
	object, snapshot, _ := sandboxConfigurationFixture()
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "example-policy", UID: "example-policy-uid"}, Spec: api.AgentPolicySpec{Harnesses: nativePolicyHarnesses()}}
	base := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(object, policy).WithObjects(object, policy).Build()
	observationFailure, statusFailure := errors.New("synthetic observation failure"), errors.New("synthetic status failure")
	c := &configurationErrorClient{Client: base, failure: statusFailure}
	source := controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) {
		return controller.SandboxConfigurationSnapshot{}, observationFailure
	})
	reconcilers := []struct {
		key       client.ObjectKey
		reconcile func(context.Context, ctrl.Request) (ctrl.Result, error)
	}{
		{client.ObjectKeyFromObject(object), (&controller.SandboxReconciler{Client: c, Source: source, Now: func() time.Time { return snapshot.ObservedAt }}).Reconcile},
		{client.ObjectKeyFromObject(policy), (&controller.AgentPolicyReconciler{Client: c, Source: source, Now: func() time.Time { return snapshot.ObservedAt }}).Reconcile},
	}
	for _, reconciler := range reconcilers {
		result, err := reconciler.reconcile(context.Background(), ctrl.Request{NamespacedName: reconciler.key})
		if !errors.Is(err, observationFailure) || !errors.Is(err, statusFailure) || result.RequeueAfter != 0 {
			t.Fatalf("failed status write masked observation failure: %+v, %v", result, err)
		}
	}
}

type configurationPrerequisiteReader struct {
	client.Reader
	failure error
}

type teamDependencyReader struct {
	client.Reader
	failure error
}

func (r *teamDependencyReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*api.AgentPolicy); ok && r.failure != nil {
		return r.failure
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func (r *configurationPrerequisiteReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	switch object.(type) {
	case *api.Sandbox, *api.AgentPolicy:
		return errors.New("primary configuration object bypassed the cache")
	}
	if r.failure != nil {
		return r.failure
	}
	return r.Reader.Get(ctx, key, object, options...)
}

type configurationStaleClient struct {
	client.Client
	sandbox *api.Sandbox
	policy  *api.AgentPolicy
}

func (c *configurationStaleClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	switch object := object.(type) {
	case *api.Sandbox:
		*object = *c.sandbox.DeepCopy()
		return nil
	case *api.AgentPolicy:
		*object = *c.policy.DeepCopy()
		return nil
	}
	return c.Client.Get(ctx, key, object, options...)
}

type configurationErrorReader struct {
	client.Reader
	failure error
}

func (r *configurationErrorReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*nodev1.RuntimeClass); ok && r.failure != nil {
		return r.failure
	}
	return r.Reader.Get(ctx, key, object, options...)
}

type configurationErrorClient struct {
	client.Client
	failure error
}

func (c *configurationErrorClient) Status() client.SubResourceWriter {
	return configurationErrorWriter{SubResourceWriter: c.Client.Status(), failure: c.failure}
}

type configurationErrorWriter struct {
	client.SubResourceWriter
	failure error
}

func (w configurationErrorWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return w.failure
}
