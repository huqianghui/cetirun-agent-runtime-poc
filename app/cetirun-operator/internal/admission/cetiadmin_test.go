// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestCetiAdminIstioManifestRecognitionDoesNotPermitWrites(t *testing.T) {
	for _, kind := range []string{"AuthorizationPolicy", "PeerAuthentication"} {
		original := unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "security.istio.io/v1", "kind": kind,
			"metadata": map[string]any{"name": "runtime-security", "namespace": "target", "uid": "security-uid"},
			"spec":     map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "synthetic-runtime"}}},
		}}
		if !cetiadmin.SupportedManifestKind("security.istio.io", kind) {
			t.Fatal("runtime security metadata must be recognized")
		}
		if safeMaintenanceObject(original, original) {
			t.Fatal("read-only Istio security kind gained a direct Helm update")
		}
		changed := original.DeepCopy()
		changed.Object["spec"] = map[string]any{"selector": map[string]any{}}
		if safeMaintenanceObject(*changed, original) {
			t.Fatal("Helm maintenance changed a security selector")
		}
	}
}

type cetiAdminFixture struct {
	guard          *CetiAdminHandler
	kube           client.Client
	source         *corev1.ConfigMap
	state          cetiadmin.Installation
	target         *api.Agent
	targetIdentity cetiadmin.TargetIdentity
	actor          authenticationv1.UserInfo
}

func newCetiAdminFixture(t *testing.T) cetiAdminFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	config := cetiadmin.DefaultConfig()
	config.Owner = cetiadmin.Owner{Alias: "owner", TenantID: "11111111-1111-1111-1111-111111111111", ObjectID: "22222222-2222-2222-2222-222222222222"}
	config.Model.InitialMonthlyBudgetUSD = "100"
	config.Kubernetes.APIAudience = "kubernetes-api"
	config.Kubernetes.APIEgressCIDRs = []string{"192.0.2.10/32"}
	config.PlatformReadNamespaces = []string{"operator-test"}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin-config", Namespace: "operator-test", UID: "source-uid", ResourceVersion: "1"}, Data: map[string]string{cetiadmin.ConfigKey: string(adminJSON(t, config))}}
	mark := map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID)}
	admin := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin", UID: "admin-uid", ResourceVersion: "1", Generation: 1,
		Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID), api.AnnotationOwnerTenantID: config.Owner.TenantID, api.AnnotationOwnerObjectID: config.Owner.ObjectID}},
		Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: api.UserSpec{Alias: "owner", Email: "owner@example.com"}, Team: "cetiadmin", GatewayTokenSecretRef: "admin-auth"}}
	owner := metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: admin.Name, UID: admin.UID, Controller: ptr.To(true)}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin", UID: "admin-team-uid", Annotations: mark}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin", UID: "admin-ns-uid", OwnerReferences: []metav1.OwnerReference{owner}}}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "openclaw-cetiadmin-cetirun", Namespace: namespace.Name, UID: "admin-sa-uid", OwnerReferences: []metav1.OwnerReference{owner}, Annotations: mark}, AutomountServiceAccountToken: ptr.To(false)}
	identity := func(object metav1.Object) cetiadmin.ObjectIdentity {
		return cetiadmin.ObjectIdentity{Name: object.GetName(), Namespace: object.GetNamespace(), UID: object.GetUID()}
	}
	hash, _ := cetiadmin.ConfigHash(config)
	state := cetiadmin.Installation{Record: cetiadmin.Record{SchemaVersion: 1, Source: identity(source), ConfigHash: hash,
		Agent: identity(admin), Team: identity(team), Namespace: identity(namespace), ServiceAccount: identity(account)},
		Configuration: config, Phase: cetiadmin.PhaseReady, RecordUID: "binding-uid", Bootstrapped: true}
	target := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "target-uid", ResourceVersion: "1", Generation: 4},
		Spec:   api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeHermes, Image: ptr.To("registry.example.com/hermes:old"), Username: ptr.To("vibe")}, User: api.UserSpec{Alias: "owner", Email: "owner@example.com"}, Team: "engineering", GatewayTokenSecretRef: "native-auth"},
		Status: api.AgentStatus{Namespace: "cetirun-example", HelmReleaseName: "openclaw-example", Phase: api.PhaseReady, ObservedGeneration: 4}}
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: target.Status.Namespace, UID: "target-ns-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: target.Name, UID: target.UID}}}}
	targetIdentity := cetiadmin.TargetIdentity{Agent: identity(target), Namespace: identity(targetNS), Runtime: "hermes", Release: target.Status.HelmReleaseName, Team: "engineering", AuthSecret: target.Spec.GatewayTokenSecretRef}
	checkpoint := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cetiadmin.InstallationName(source.Namespace, source.Name), Namespace: source.Namespace, UID: state.RecordUID, Annotations: mark}, Data: map[string]string{cetiadmin.BindingKey: string(adminJSON(t, state))}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, checkpoint, admin, team, namespace, account, target, targetNS, &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "engineering", UID: "target-team-uid"}}).Build()
	admissionConfig := testConfig()
	admissionConfig.CetiAdmin = &CetiAdminConfig{Namespace: source.Namespace, ConfigMapName: source.Name, RuntimeUsername: serviceAccountUsername(state.ServiceAccount), StopUsername: "system:serviceaccount:operator-test:cetiadmin-stop", AdminUsers: []string{"administrator"}}
	guard, err := NewCetiAdminHandler(kube, admissionConfig)
	if err != nil {
		t.Fatal(err)
	}
	return cetiAdminFixture{guard: guard, kube: kube, source: source, state: state, target: target, targetIdentity: targetIdentity,
		actor: authenticationv1.UserInfo{Username: serviceAccountUsername(state.ServiceAccount), UID: string(account.UID), Groups: []string{"system:authenticated"}}}
}

func adminJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func adminRequest(t *testing.T, operation admissionv1.Operation, group, kind, resourceName string, object, old client.Object, user authenticationv1.UserInfo) admission.Request {
	t.Helper()
	version := "v1"
	if group == api.GroupVersion.Group {
		version = "v1alpha1"
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "request-uid", Operation: operation,
		Resource: metav1.GroupVersionResource{Group: group, Version: version, Resource: resourceName}, Kind: metav1.GroupVersionKind{Group: group, Version: version, Kind: kind}, UserInfo: user}}
	if object != nil {
		request.Name, request.Namespace = object.GetName(), object.GetNamespace()
		copy := object.DeepCopyObject()
		copy.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
		request.Object.Raw = adminJSON(t, copy)
	}
	if old != nil {
		copy := old.DeepCopyObject()
		copy.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
		request.OldObject.Raw = adminJSON(t, copy)
		if object == nil {
			request.Name, request.Namespace = old.GetName(), old.GetNamespace()
		}
	}
	return request
}

func TestCetiAdminRollbackRestoresIntentWithoutStartingPods(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.updateTarget(t, func(a *api.Agent) {
		a.Spec.Suspended = true
		a.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-123456", ExpectedRevision: 10}
		a.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: "maintenance-123456", Revision: 10, ObservedGeneration: a.Generation, Phase: api.HelmMaintenancePhaseHeld}
	})
	old := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "openclaw-example-cetirun", Namespace: f.target.Status.Namespace, UID: "workload-uid",
		Annotations: map[string]string{"meta.helm.sh/release-name": f.target.Status.HelmReleaseName, "meta.helm.sh/release-namespace": f.target.Status.Namespace}},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(0)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "runtime-sa", Containers: []corev1.Container{{Name: "cetirun", Image: "registry.example.com/hermes:new"}}}}}}
	if err := f.kube.Create(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	f.heldMaintenance(t, []cetiadmin.ManagedResource{{APIVersion: "apps/v1", Kind: "StatefulSet", ObjectIdentity: cetiadmin.ObjectIdentity{Name: old.Name, Namespace: old.Namespace, UID: old.UID}}})
	next := old.DeepCopy()
	next.Spec.Replicas = ptr.To(int32(1))
	next.Spec.Template.Spec.Containers[0].Image = "registry.example.com/hermes:old"
	request := adminRequest(t, admissionv1.Update, "apps", "StatefulSet", "statefulsets", next, old, f.actor)
	if response := f.guard.Validate(context.Background(), request); !response.Allowed {
		t.Fatalf("native rollback intent denied: %v", response.Result)
	}
	next.Spec.Template.Spec.ServiceAccountName = "privileged"
	request = adminRequest(t, admissionv1.Update, "apps", "StatefulSet", "statefulsets", next, old, f.actor)
	if f.guard.Validate(context.Background(), request).Allowed {
		t.Fatal("Helm rollback changed workload identity")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: old.Name + "-0", Namespace: old.Namespace}}
	request = adminRequest(t, admissionv1.Create, "", "Pod", "pods", pod, nil, authenticationv1.UserInfo{Username: "system:serviceaccount:kube-system:statefulset-controller"})
	if f.guard.Validate(context.Background(), request).Allowed {
		t.Fatal("rollback replica intent started a Pod")
	}
}

func (f cetiAdminFixture) updateTarget(t *testing.T, mutate func(*api.Agent)) {
	t.Helper()
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.target.Name}, f.target); err != nil {
		t.Fatal(err)
	}
	mutate(f.target)
	if err := f.kube.Update(context.Background(), f.target); err != nil {
		t.Fatal(err)
	}
}

func (f cetiAdminFixture) heldMaintenance(t *testing.T, resources []cetiadmin.ManagedResource) *corev1.ConfigMap {
	t.Helper()
	if !cetiadmin.MaintenanceHeld(f.target) {
		t.Fatal("fixture requires a Held Agent")
	}
	target := f.targetIdentity
	target.Resources = resources
	record := cetiAdminMaintenanceRecord{Version: 1, Target: target, Request: *f.target.Spec.Helm.Maintenance,
		RequestGeneration: f.target.Generation, Phase: api.HelmMaintenancePhaseHeld, IntentHash: "synthetic-intent-hash"}
	hash := sha256.Sum256([]byte(f.target.UID))
	checkpoint := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin-maintenance-" + hex.EncodeToString(hash[:16]), Namespace: f.source.Namespace, UID: "maintenance-record-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(f.target, api.GroupVersion.WithKind("Agent"))}}, Data: map[string]string{"maintenance.json": string(adminJSON(t, record))}}
	if err := f.kube.Create(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: target.Namespace.Name}, namespace); err != nil {
		t.Fatal(err)
	}
	namespace.Labels = map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}
	namespace.Annotations = map[string]string{cetiadmin.MaintenanceAgentName: f.target.Name, cetiadmin.MaintenanceAgentUID: string(f.target.UID), cetiadmin.MaintenanceRequestID: record.Request.RequestID}
	if err := f.kube.Update(context.Background(), namespace); err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func (f cetiAdminFixture) runtimePod(t *testing.T) *corev1.Pod {
	t.Helper()
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "openclaw-" + f.target.Name + "-cetirun", Namespace: f.target.Status.Namespace, UID: "runtime-workload-uid",
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
		Annotations: map[string]string{"meta.helm.sh/release-name": f.target.Status.HelmReleaseName, "meta.helm.sh/release-namespace": f.target.Status.Namespace}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "runtime-node", UID: "runtime-node-uid"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workload.Name + "-0", Namespace: workload.Namespace, UID: "runtime-pod-uid",
		Labels: map[string]string{"app.kubernetes.io/instance": f.target.Status.HelmReleaseName}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}},
		Spec: corev1.PodSpec{NodeName: node.Name}}
	for _, object := range []client.Object{workload, node, pod} {
		if err := f.kube.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	return pod
}

func TestCetiAdminAdmissionAgentScope(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*api.Agent)
		want bool
	}{
		{"model", func(a *api.Agent) { a.Spec.Model = "auto" }, true},
		{"suspend", func(a *api.Agent) { a.Spec.Suspended = true }, true},
		{"paired upgrade", func(a *api.Agent) {
			a.Spec.Helm.Version = "0.1.2"
			a.Spec.Agent.Image = ptr.To("registry.example.com/hermes:new")
		}, true},
		{"owner", func(a *api.Agent) { a.Spec.User.Alias = "another-owner" }, false},
		{"team", func(a *api.Agent) { a.Spec.Team = "other" }, false},
		{"username", func(a *api.Agent) { a.Spec.Agent.Username = ptr.To("another") }, false},
		{"source marker", func(a *api.Agent) { a.Annotations = map[string]string{cetiadmin.AnnotationSourceUID: "copied"} }, false},
		{"finalizer", func(a *api.Agent) { a.Finalizers = []string{"example.com/owned"} }, false},
		{"status", func(a *api.Agent) { a.Status.Phase = api.PhaseFailed }, false},
		{"mounts", func(a *api.Agent) {
			a.Spec.Helm.Values = map[string]runtime.RawExtension{"extraVolumes": {Raw: []byte(`[{"name":"escape","hostPath":{"path":"/"}}]`)}}
		}, false},
		{"cpu", func(a *api.Agent) {
			a.Spec.Helm.Values = map[string]runtime.RawExtension{"resources": {Raw: []byte(`{"requests":{"cpu":"2"}}`)}}
		}, true},
		{"unknown resource", func(a *api.Agent) {
			a.Spec.Helm.Values = map[string]runtime.RawExtension{"resources": {Raw: []byte(`{"priorityClassName":"system-node-critical"}`)}}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			next := f.target.DeepCopy()
			test.edit(next)
			request := adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, f.target, f.actor)
			response := f.guard.Validate(context.Background(), request)
			if response.Allowed != test.want {
				t.Fatalf("allowed=%v want=%v: %v", response.Allowed, test.want, response.Result)
			}
		})
	}
}

func TestCetiAdminAdmissionCurrentAuthority(t *testing.T) {
	for _, mutation := range []string{"caller UID", "account UID", "namespace UID", "source UID", "disabled", "configuration changed", "target UID", "stale target Team", "installation hold", "self suspended"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			switch mutation {
			case "caller UID":
				f.actor.UID = "old-account"
			case "account UID", "namespace UID", "source UID":
				var object client.Object
				var key client.ObjectKey
				if mutation == "account UID" {
					object, key = &corev1.ServiceAccount{}, client.ObjectKey{Name: f.state.ServiceAccount.Name, Namespace: f.state.ServiceAccount.Namespace}
				} else if mutation == "namespace UID" {
					object, key = &corev1.Namespace{}, client.ObjectKey{Name: f.state.Namespace.Name}
				} else {
					object, key = &corev1.ConfigMap{}, client.ObjectKeyFromObject(f.source)
				}
				if err := f.kube.Get(context.Background(), key, object); err != nil {
					t.Fatal(err)
				}
				object.SetUID("replaced")
				if err := f.kube.Update(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			case "disabled", "configuration changed":
				config := f.state.Configuration
				if mutation == "disabled" {
					config.Enabled = false
				} else {
					config.PlatformReadNamespaces = []string{"another-platform"}
				}
				f.source.Data[cetiadmin.ConfigKey] = string(adminJSON(t, config))
				if err := f.kube.Update(context.Background(), f.source); err != nil {
					t.Fatal(err)
				}
			case "target UID":
				changed := f.target.DeepCopy()
				changed.UID = "another-agent"
				if err := f.kube.Update(context.Background(), changed); err != nil {
					t.Fatal(err)
				}
			case "stale target Team":
				changed := f.target.DeepCopy()
				changed.Spec.Team = "other"
				if err := f.kube.Update(context.Background(), changed); err != nil {
					t.Fatal(err)
				}
			case "installation hold":
				object := &corev1.ConfigMap{}
				if err := f.kube.Get(context.Background(), client.ObjectKey{Name: cetiadmin.InstallationName(f.source.Namespace, f.source.Name), Namespace: f.source.Namespace}, object); err != nil {
					t.Fatal(err)
				}
				f.state.Hold = true
				object.Data[cetiadmin.BindingKey] = string(adminJSON(t, f.state))
				if err := f.kube.Update(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			case "self suspended":
				object := &api.Agent{}
				if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.state.Agent.Name}, object); err != nil {
					t.Fatal(err)
				}
				object.Spec.Suspended = true
				if err := f.kube.Update(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			}
			next := f.target.DeepCopy()
			next.Spec.Model = "auto"
			response := f.guard.Validate(context.Background(), adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, f.target, f.actor))
			if response.Allowed {
				t.Fatal("stale or disabled authority was accepted")
			}
		})
	}
}

func TestCetiAdminSourceAndStopAuthority(t *testing.T) {
	for _, identity := range []struct {
		user string
		want bool
	}{{"administrator", true}, {"system:serviceaccount:test:portal", false}, {"system:serviceaccount:operator-test:cetiadmin-stop", true}, {"ordinary", false}} {
		t.Run(identity.user, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			next := f.source.DeepCopy()
			config := f.state.Configuration
			config.Enabled = false
			next.Data[cetiadmin.ConfigKey] = string(adminJSON(t, config))
			request := adminRequest(t, admissionv1.Update, "", "ConfigMap", "configmaps", next, f.source, authenticationv1.UserInfo{Username: identity.user})
			response := f.guard.Validate(context.Background(), request)
			if response.Allowed != identity.want {
				t.Fatalf("allowed=%v: %v", response.Allowed, response.Result)
			}
			if strings.HasSuffix(identity.user, ":cetiadmin-stop") {
				config.Model.Default = "changed"
				next.Data[cetiadmin.ConfigKey] = string(adminJSON(t, config))
				request.Object.Raw = adminJSON(t, next)
				if f.guard.Validate(context.Background(), request).Allowed {
					t.Fatal("stop hook changed model")
				}
			}
		})
	}
}

func TestCetiAdminCopilotMetadataAuthoring(t *testing.T) {
	for _, test := range []struct {
		name string
		want bool
	}{
		{"portal", true},
		{"administrator", true},
		{"self", false},
		{"ordinary", false},
		{"other-portal", false},
		{"unauthenticated", false},
		{"source-marker", false},
		{"owner-binding", false},
		{"exec-binding", false},
		{"model", false},
		{"suspension", false},
		{"unknown-annotation", false},
		{"labels", false},
		{"replacement-uid", false},
		{"other-agent", false},
		{"future-spec", false},
		{"bootstrap-pending", false},
		{"not-bootstrapped", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			old := &api.Agent{}
			if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.state.Agent.Name}, old); err != nil {
				t.Fatal(err)
			}
			next := old.DeepCopy()
			next.Annotations[api.AnnotationCopilotModels] = `[{"id":"gpt-5.5"}]`
			next.Annotations[api.AnnotationCopilotSignInGeneration] = "signin-generation"
			next.Annotations[api.AnnotationCopilotSignedInAt] = "2026-01-01T00:00:00Z"
			user := authenticationv1.UserInfo{Username: "system:serviceaccount:test:portal", UID: "portal-uid", Groups: []string{"system:authenticated"}}
			switch test.name {
			case "administrator":
				user.Username = "administrator"
			case "self":
				user = f.actor
			case "ordinary":
				user.Username = "ordinary-writer"
			case "other-portal":
				user.Username = "system:serviceaccount:other:portal"
			case "unauthenticated":
				user.Groups = []string{"system:unauthenticated"}
			case "source-marker":
				next.Annotations[cetiadmin.AnnotationSourceUID] = "different-source"
			case "owner-binding":
				next.Annotations[api.AnnotationOwnerObjectID] = "different-owner"
			case "exec-binding":
				next.Annotations[registryauth.ExecAdminBindingAnnotation] = "{}"
			case "model":
				next.Spec.Model = "different-model"
			case "suspension":
				next.Spec.Suspended = true
			case "unknown-annotation":
				next.Annotations["example.com/unknown"] = "changed"
			case "labels":
				next.Labels = map[string]string{"example.com/unknown": "changed"}
			case "replacement-uid":
				next.UID = "replacement-agent"
			case "other-agent":
				next.Name, old.Name = "other-marked-agent", "other-marked-agent"
			case "bootstrap-pending", "not-bootstrapped":
				checkpoint := &corev1.ConfigMap{}
				key := client.ObjectKey{Namespace: f.source.Namespace, Name: cetiadmin.InstallationName(f.source.Namespace, f.source.Name)}
				if err := f.kube.Get(context.Background(), key, checkpoint); err != nil {
					t.Fatal(err)
				}
				state := f.state
				if test.name == "bootstrap-pending" {
					state.BootstrapReleasePending = true
				} else {
					state.Bootstrapped = false
				}
				checkpoint.Data[cetiadmin.BindingKey] = string(adminJSON(t, state))
				if err := f.kube.Update(context.Background(), checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			request := adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, old, user)
			if test.name == "future-spec" {
				var raw map[string]any
				if err := json.Unmarshal(request.Object.Raw, &raw); err != nil {
					t.Fatal(err)
				}
				spec, ok := raw["spec"].(map[string]any)
				if !ok {
					t.Fatal("fixture is missing its spec")
				}
				spec["futureAuthority"] = true
				request.Object.Raw = adminJSON(t, raw)
			}
			response := f.guard.Validate(context.Background(), request)
			if response.Allowed != test.want {
				t.Fatalf("allowed=%v, want %v: %v", response.Allowed, test.want, response.Result)
			}
		})
	}
}

func TestCetiAdminNativeAuthReceiptsCannotBeForgedByOrdinaryWriters(t *testing.T) {
	f := newCetiAdminFixture(t)
	old := f.target.DeepCopy()
	next := old.DeepCopy()
	next.Annotations = map[string]string{nativeAuthRotationAnnotation: "rotation-12345678"}
	request := adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, old, authenticationv1.UserInfo{Username: "ordinary-writer"})
	if f.guard.Validate(context.Background(), request).Allowed {
		t.Fatal("ordinary writer forged a native auth intent")
	}
	before := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "native-auth", Namespace: f.target.Status.Namespace, UID: "secret-uid", ResourceVersion: "1"}}
	after := before.DeepCopy()
	after.Annotations = next.Annotations
	request = adminRequest(t, admissionv1.Update, "", "Secret", "secrets", after, before, authenticationv1.UserInfo{Username: "ordinary-writer"})
	if f.guard.Validate(context.Background(), request).Allowed {
		t.Fatal("ordinary writer forged a native auth receipt")
	}
}

func TestCetiAdminCurrentTeamPolicyPreventsStaleExecAuthority(t *testing.T) {
	f := newCetiAdminFixture(t)
	pod := f.runtimePod(t)
	request := adminRequest(t, admissionv1.Connect, "", "Pod", "pods", pod, nil, f.actor)
	request.SubResource = "exec"
	if response := f.guard.Validate(context.Background(), request); !response.Allowed {
		t.Fatalf("ordinary runtime exec denied: %v", response.Result)
	}
	team := &api.Team{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.target.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	team.Spec.AgentPolicyRef = "new-protected-policy"
	if err := f.kube.Update(context.Background(), team); err != nil {
		t.Fatal(err)
	}
	if f.guard.Validate(context.Background(), request).Allowed {
		t.Fatal("exec ignored a newly protected Team")
	}
}

func TestCetiAdminProtectsMaintenanceCheckpointAndExecBinding(t *testing.T) {
	f := newCetiAdminFixture(t)
	checkpoint := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin-maintenance-record", Namespace: f.source.Namespace}, Data: map[string]string{"maintenance.json": "{}"}}
	for _, user := range []string{testController, "administrator", "ordinary-writer", f.actor.Username} {
		request := adminRequest(t, admissionv1.Create, "", "ConfigMap", "configmaps", checkpoint, nil, authenticationv1.UserInfo{Username: user})
		if got := f.guard.Validate(context.Background(), request).Allowed; got != (user == testController) {
			t.Fatalf("checkpoint protection for %s: %v", user, got)
		}
	}
	next := f.target.DeepCopy()
	next.Annotations = map[string]string{registryauth.ExecAdminBindingAnnotation: "{}"}
	for _, user := range []string{testController, "administrator", "ordinary-writer", f.actor.Username} {
		request := adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, f.target, authenticationv1.UserInfo{Username: user})
		if got := f.guard.Validate(context.Background(), request).Allowed; got != (user == testController) {
			t.Fatalf("exec binding protection for %s: %v", user, got)
		}
	}
}

func TestCetiAdminNativeAuthFields(t *testing.T) {
	for _, runtimeType := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw} {
		for _, corruption := range []string{"", "model key", "identity", "wrong marker", "Hermes env", "unchanged key"} {
			t.Run(string(runtimeType)+"/"+corruption, func(t *testing.T) {
				f := newCetiAdminFixture(t)
				agent := f.target.DeepCopy()
				agent.Spec.Agent.Type = runtimeType
				agent.Annotations = map[string]string{nativeAuthRotationAnnotation: "rotation-12345678"}
				key := "OPENCLAW_GATEWAY_TOKEN"
				if runtimeType == api.AgentTypeHermes {
					key = "API_SERVER_KEY"
				}
				old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.GatewayTokenSecretRef, Namespace: agent.Status.Namespace, UID: "secret-uid", ResourceVersion: "4", Labels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator", "app.kubernetes.io/instance": agent.Spec.User.Alias}}, Type: corev1.SecretTypeOpaque,
					Data: map[string][]byte{key: []byte("old-native-key"), "CETIRUN_AUTH_KEY": []byte("old-ui-key"), "CETIRUN_GATEWAY_API_KEY": []byte("model-key-kept"), "hermes.env": []byte("API_SERVER_KEY=old-native-key\nOTHER=preserved\n")}}
				next := old.DeepCopy()
				next.Data[key] = bytes.Repeat([]byte("a"), 43)
				next.Data["CETIRUN_AUTH_KEY"] = bytes.Repeat([]byte("b"), 43)
				next.Annotations = map[string]string{nativeAuthRotationAnnotation: "rotation-12345678"}
				if runtimeType == api.AgentTypeHermes {
					next.Data["hermes.env"] = []byte("API_SERVER_KEY=" + strings.Repeat("a", 43) + "\nOTHER=preserved\n")
				}
				switch corruption {
				case "model key":
					next.Data["CETIRUN_GATEWAY_API_KEY"] = []byte("replaced")
				case "identity":
					next.UID = "another-secret"
				case "wrong marker":
					next.Annotations[nativeAuthRotationAnnotation] = "different-1234567"
				case "Hermes env":
					next.Data["hermes.env"] = []byte("OTHER=replaced")
				case "unchanged key":
					next.Data[key] = old.Data[key]
				}
				response := cetiAdminNativeAuth(adminRequest(t, admissionv1.Update, "", "Secret", "secrets", next, old, f.actor), agent)
				if response.Allowed != (corruption == "") {
					t.Fatalf("allowed=%v: %v", response.Allowed, response.Result)
				}
			})
		}
	}
}

func TestCetiAdminMaintenanceAllCreators(t *testing.T) {
	for _, actor := range []string{testController, "system:serviceaccount:kube-system:statefulset-controller", "system:serviceaccount:kube-system:job-controller", "administrator"} {
		t.Run(actor, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.updateTarget(t, func(a *api.Agent) {
				a.Spec.Suspended = true
				a.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-123456", ExpectedRevision: 10}
				a.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: "maintenance-123456", Revision: 10, ObservedGeneration: a.Generation, Phase: api.HelmMaintenancePhaseHeld}
			})
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new-writer", Namespace: f.target.Status.Namespace}}
			request := adminRequest(t, admissionv1.Create, "", "Pod", "pods", pod, nil, authenticationv1.UserInfo{Username: actor})
			if f.guard.Validate(context.Background(), request).Allowed {
				t.Fatal("maintenance allowed a new writer")
			}
		})
	}
}

func TestCetiAdminMaintenanceTransitions(t *testing.T) {
	f := newCetiAdminFixture(t)
	old := f.target.DeepCopy()
	old.Spec.Suspended = true
	old.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-123456", ExpectedRevision: 10}
	old.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: old.Spec.Helm.Maintenance.RequestID, Revision: 10, ObservedGeneration: old.Generation, Phase: api.HelmMaintenancePhaseHeld}
	for _, test := range []struct {
		name   string
		change func(*api.Agent)
		want   bool
	}{
		{"clear", func(a *api.Agent) { a.Spec.Helm.Maintenance = nil }, true},
		{"clear and resume", func(a *api.Agent) { a.Spec.Helm.Maintenance = nil; a.Spec.Suspended = false }, false},
		{"revision change", func(a *api.Agent) { a.Spec.Helm.Maintenance.ExpectedRevision++ }, false},
		{"intent change", func(a *api.Agent) { a.Spec.Model = "other" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			next := old.DeepCopy()
			test.change(next)
			response := cetiAdminMaintenanceIntent(adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, old, f.actor))
			if response.Allowed != test.want {
				t.Fatalf("allowed=%v: %v", response.Allowed, response.Result)
			}
		})
	}
	old.Spec.Helm.Maintenance = nil
	old.Status.HelmMaintenance.Phase = api.HelmMaintenancePhaseReleased
	next := old.DeepCopy()
	next.Spec.Suspended = false
	request := adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, old, f.actor)
	if !cetiAdminMaintenanceIntent(request).Allowed {
		t.Fatal("current Released cannot resume")
	}
	for _, freshRequest := range []bool{false, true} {
		edited := old.DeepCopy()
		edited.Spec.Model = "another-model"
		if freshRequest {
			edited.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-next-request", ExpectedRevision: old.Status.HelmMaintenance.Revision}
		}
		request := adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", edited, old, f.actor)
		if response := cetiAdminMaintenanceIntent(request); response.Allowed != freshRequest {
			t.Fatalf("configuration edit without a fresh handoff=%t: %v", !freshRequest, response.Result)
		}
	}
	old.Status.HelmMaintenance.ObservedGeneration--
	request.OldObject.Raw = adminJSON(t, old)
	next.Status = old.Status
	request.Object.Raw = adminJSON(t, next)
	if cetiAdminMaintenanceIntent(request).Allowed {
		t.Fatal("stale Released resumed")
	}
}

func TestCetiAdminPVCOnlyGrows(t *testing.T) {
	f := newCetiAdminFixture(t)
	old := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: f.target.Status.Namespace, UID: "home-uid", ResourceVersion: "1"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "same-pv", Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}}}
	for _, size := range []string{"20Gi", "10Gi", "5Gi"} {
		next := old.DeepCopy()
		next.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(size)
		response := cetiAdminPVCGrowth(adminRequest(t, admissionv1.Update, "", "PersistentVolumeClaim", "persistentvolumeclaims", next, old, f.actor))
		if response.Allowed != (size == "20Gi") {
			t.Fatalf("%s: %v", size, response.Result)
		}
	}
}

func maintenanceSecret(t *testing.T, agent *api.Agent, revision int64, status string, changes ...func(map[string]any)) *corev1.Secret {
	t.Helper()
	payload := map[string]any{"name": agent.Status.HelmReleaseName, "namespace": agent.Status.Namespace, "version": revision,
		"info": map[string]any{"status": status, "description": "synthetic"}, "manifest": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: runtime-config\n", "config": map[string]any{"retained": "synthetic-value"}}
	for _, change := range changes {
		change(payload)
	}
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(adminJSON(t, payload)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", agent.Status.HelmReleaseName, revision), Namespace: agent.Status.Namespace, UID: types.UID(fmt.Sprintf("revision-%d", revision)), ResourceVersion: "1", Labels: map[string]string{"name": agent.Status.HelmReleaseName, "owner": "helm", "version": fmt.Sprint(revision), "status": status}}, Type: "helm.sh/release.v1", Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))}}
}

func TestCetiAdminHelmRevisionPreservesTrustedHistory(t *testing.T) {
	f := newCetiAdminFixture(t)
	ctx := context.Background()
	f.updateTarget(t, func(agent *api.Agent) {
		agent.Spec.Suspended = true
		agent.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-history-one", ExpectedRevision: 2}
		agent.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: agent.Spec.Helm.Maintenance.RequestID, Revision: 2, ObservedGeneration: agent.Generation, Phase: api.HelmMaintenancePhaseHeld}
	})
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "runtime-config", Namespace: f.target.Status.Namespace, UID: "runtime-config-uid"}}
	if err := f.kube.Create(ctx, config); err != nil {
		t.Fatal(err)
	}
	f.heldMaintenance(t, []cetiadmin.ManagedResource{{APIVersion: "v1", Kind: "ConfigMap", ObjectIdentity: cetiadmin.ObjectIdentity{Name: config.Name, Namespace: config.Namespace, UID: config.UID}}})
	historical := func(payload map[string]any) {
		payload["chart"] = map[string]any{"metadata": map[string]any{"name": "runtime", "version": "1.0.0"}}
		payload["hooks"] = []any{map[string]any{"name": "retained-hook", "manifest": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: retained-hook\n", "events": []any{"post-install"}}}
	}
	previous := maintenanceSecret(t, f.target, 1, "deployed", historical)
	newer := func(payload map[string]any) {
		historical(payload)
		payload["chart"].(map[string]any)["metadata"].(map[string]any)["version"] = "2.0.0"
	}
	latest := maintenanceSecret(t, f.target, 2, "failed", newer, func(payload map[string]any) {
		payload["hooks"].([]any)[0].(map[string]any)["last_run"] = map[string]any{"phase": "Succeeded", "started_at": "2026-01-01T00:00:00Z"}
	})
	for _, object := range []client.Object{previous, latest} {
		object.SetResourceVersion("")
		if err := f.kube.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []string{"rollback", "new-hooks", "case-alias", "duplicate-fields", "new-chart", "new-config", "new-manifest", "future-source"} {
		t.Run(change, func(t *testing.T) {
			next := maintenanceSecret(t, f.target, 3, "pending-rollback", historical, func(payload map[string]any) {
				payload["info"].(map[string]any)["description"] = "Rollback to 1"
				switch change {
				case "new-hooks":
					payload["hooks"].([]any)[0].(map[string]any)["events"] = []any{"pre-delete"}
				case "case-alias":
					payload["Hooks"] = []any{}
				case "duplicate-fields":
					payload["chart"] = json.RawMessage(`{"metadata":{"annotations":{"extra":"different"}},"metadata":{"name":"runtime","version":"1.0.0"}}`)
				case "new-chart":
					payload["chart"] = map[string]any{"metadata": map[string]any{"name": "other", "version": "1.0.0"}}
				case "new-config":
					payload["config"] = map[string]any{"retained": "different"}
				case "new-manifest":
					payload["manifest"] = payload["manifest"].(string) + "data:\n  extra: changed\n"
				case "future-source":
					payload["info"].(map[string]any)["description"] = "Rollback to 3"
				}
			})
			next.UID, next.ResourceVersion = "", ""
			request := adminRequest(t, admissionv1.Create, "", "Secret", "secrets", next, nil, f.actor)
			if result := f.guard.Validate(ctx, request); result.Allowed != (change == "rollback") {
				t.Fatalf("history %s: %v", change, result.Result)
			}
		})
	}
	next := maintenanceSecret(t, f.target, 1, "superseded", historical)
	next.ResourceVersion = previous.ResourceVersion
	request := adminRequest(t, admissionv1.Update, "", "Secret", "secrets", next, previous, f.actor)
	if result := f.guard.Validate(ctx, request); !result.Allowed {
		t.Fatalf("native rollback could not supersede its older deployed revision: %v", result.Result)
	}
	next = maintenanceSecret(t, f.target, 3, "pending-upgrade", newer)
	next.UID, next.ResourceVersion = "", ""
	request = adminRequest(t, admissionv1.Create, "", "Secret", "secrets", next, nil, f.actor)
	if result := f.guard.Validate(ctx, request); !result.Allowed {
		t.Fatalf("unchanged no-hooks reapplication was rejected after a previous hook run: %v", result.Result)
	}
}

func TestCetiAdminHelmRevisionRepair(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.updateTarget(t, func(agent *api.Agent) {
		agent.Spec.Suspended = true
		agent.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-123456", ExpectedRevision: 1}
		agent.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: "maintenance-123456", ObservedGeneration: agent.Generation, Revision: 1, Phase: api.HelmMaintenancePhaseHeld}
	})
	f.heldMaintenance(t, nil)
	agent := f.target
	old, next := maintenanceSecret(t, agent, 1, "pending-install"), maintenanceSecret(t, agent, 1, "failed")
	request := adminRequest(t, admissionv1.Update, "", "Secret", "secrets", next, old, f.actor)
	if result := f.guard.cetiAdminHelmWrite(context.Background(), request, agent, f.targetIdentity); !result.Allowed {
		t.Fatal(result.Result)
	}
	next.Labels["status"] = "deployed"
	request.Object.Raw = adminJSON(t, next)
	if f.guard.cetiAdminHelmWrite(context.Background(), request, agent, f.targetIdentity).Allowed {
		t.Fatal("label/payload disagreement accepted")
	}
	agent.Status.HelmMaintenance.ObservedGeneration--
	request.Object.Raw = adminJSON(t, maintenanceSecret(t, agent, 1, "failed"))
	if f.guard.cetiAdminHelmWrite(context.Background(), request, agent, f.targetIdentity).Allowed {
		t.Fatal("stale maintenance accepted")
	}
}
