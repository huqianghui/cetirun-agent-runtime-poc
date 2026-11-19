// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart/loader"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
)

// --- Fake HelmClient ---

type fakeHelmClient struct {
	installCalled       bool
	installCalls        int
	installErr          error
	lastChartVersion    string
	lastValues          map[string]interface{}
	useHarness          bool
	appliedKey          string
	prepareCalls        int
	prepareErr          error
	disableChatContract bool
}

func (f *fakeHelmClient) ResolveImageUpgrade(_, _, _, _, image, _ string) (string, error) {
	return image, nil
}

// requireLegacyDirectRuntime identifies compatibility tests for Agents that
// have never opted into Sandbox or AgentPolicy execution.
func requireLegacyDirectRuntime(t *testing.T) {
	t.Helper()
}

func (f *fakeHelmClient) Prepare(namespace string, values map[string]any, chartVersion string) (*helm.PreparedRelease, error) {
	f.prepareCalls++
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	if f.useHarness {
		chrt, err := loader.Load("../../../cetirun-chart")
		if err != nil {
			return nil, err
		}
		chrt.Metadata.Version = chartVersion
		if f.disableChatContract {
			delete(chrt.Metadata.Annotations, "cetirun.microsoft.com/portal-chat")
		}
		return helm.PrepareChart(chrt, values)
	}
	return &helm.PreparedRelease{Values: values, ChartVersion: chartVersion}, nil
}

func (f *fakeHelmClient) Refresh(prepared *helm.PreparedRelease, values map[string]any) (*helm.PreparedRelease, error) {
	if f.useHarness {
		return prepared.Rebuild(values)
	}
	prepared.Values = values
	return prepared, nil
}

func TestReconcileBuiltinHarnessPinsThenDeploys(t *testing.T) {
	requireLegacyDirectRuntime(t)
	for _, typ := range []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeHermes, cetirunv1alpha1.AgentTypeOpenClaw} {
		t.Run(string(typ), func(t *testing.T) {
			cr := newCR("example", cetirunv1alpha1.PhasePending, nil, 1, 0, "")
			cr.UID = "example-agent-uid"
			cr.Spec.Agent = cetirunv1alpha1.HarnessSpec{Type: typ}
			scheme := testScheme()
			client := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&cetirunv1alpha1.Agent{}).WithObjects(cr).Build()
			hc := &fakeHelmClient{useHarness: true}
			r := &controller.AgentReconciler{
				Client: client, Scheme: scheme, HelmClient: hc, DefaultModel: "test-model",
				EventRecorder: record.NewFakeRecorder(100), DomainSuffix: "example.com", IstioRevision: "test",
			}
			controller.ConfigureTestGateway(t, r)
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}
			result, err := r.Reconcile(context.Background(), req)
			if err != nil || !result.Requeue || hc.installCalled {
				t.Fatalf("pinning must requeue without deployment: %+v %v", result, err)
			}
			current := &cetirunv1alpha1.Agent{}
			if err := client.Get(context.Background(), req.NamespacedName, current); err != nil {
				t.Fatal(err)
			}
			if current.Spec.Agent.Image == nil || current.Spec.Agent.Port == nil {
				t.Fatal("image and port were not persisted")
			}
			ns := &corev1.Namespace{}
			if err := client.Get(context.Background(), types.NamespacedName{Name: "cetirun-example"}, ns); !apierrors.IsNotFound(err) {
				t.Fatalf("namespace created before pinning completed: %v", err)
			}
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if !hc.installCalled || hc.lastValues["runtime"] == nil {
				t.Fatal("built-in did not deploy through the Harness runtime plan")
			}
			if err := client.Get(context.Background(), types.NamespacedName{Name: "cetirun-example"}, ns); err != nil ||
				len(ns.OwnerReferences) != 1 || ns.OwnerReferences[0].UID != cr.UID {
				t.Fatal("new namespace must bind the Agent UID before the Pod becomes Ready")
			}
			runtime := hc.lastValues["runtime"].(map[string]any)
			agent := runtime["agent"].(map[string]any)
			if agent["image"] != *current.Spec.Agent.Image || agent["type"] != string(typ) {
				t.Fatalf("deployed runtime differs from pinned configuration: %v", agent)
			}
			if hc.lastValues["agentRequest"] != nil {
				t.Fatal("internal request leaked into release values")
			}
		})
	}
}

func TestReconcileInvalidHarnessPreservesWorkload(t *testing.T) {
	requireLegacyDirectRuntime(t)
	for _, invalid := range []cetirunv1alpha1.HarnessSpec{
		{Type: cetirunv1alpha1.AgentTypeHermes, Port: ptr.To[int32](18790)},
		{Type: cetirunv1alpha1.AgentTypeOpenClaw, Image: ptr.To("registry.example.com/unqualified:1")},
	} {
		cr := newCR("example", cetirunv1alpha1.PhaseReady, nil, 2, 1, "")
		cr.Spec.Agent = invalid
		scheme := testScheme()
		sts := statefulSet("example", 1)
		client := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&cetirunv1alpha1.Agent{}).WithObjects(cr, sts).Build()
		hc := &fakeHelmClient{useHarness: true}
		r := &controller.AgentReconciler{
			Client: client, Scheme: scheme, HelmClient: hc, DefaultModel: "test-model",
			EventRecorder: record.NewFakeRecorder(100),
		}
		controller.ConfigureTestGateway(t, r)
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
		if err == nil || !strings.Contains(err.Error(), "AgentConfigInvalid") {
			t.Fatalf("expected explicit configuration rejection, got %v", err)
		}
		if hc.installCalled {
			t.Fatal("invalid configuration reached Helm apply")
		}
		current := &appsv1.StatefulSet{}
		if err := client.Get(context.Background(), types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name}, current); err != nil || *current.Spec.Replicas != 1 {
			t.Fatalf("existing workload was changed: %v", err)
		}
		ns := &corev1.Namespace{}
		if err := client.Get(context.Background(), types.NamespacedName{Name: "cetirun-example"}, ns); !apierrors.IsNotFound(err) {
			t.Fatal("invalid configuration created a namespace")
		}
	}
}

func (f *fakeHelmClient) InstallOrUpgrade(namespace, releaseName string, prepared *helm.PreparedRelease) error {
	f.installCalled = true
	f.installCalls++
	f.lastChartVersion = prepared.ChartVersion
	f.lastValues = prepared.Values
	if f.installErr == nil {
		f.appliedKey = prepared.CacheKey()
	}
	return f.installErr
}

func (f *fakeHelmClient) CustomApplied(namespace, releaseName string, prepared *helm.PreparedRelease) (bool, error) {
	return f.appliedKey != "" && f.appliedKey == prepared.CacheKey(), nil
}

func (f *fakeHelmClient) Uninstall(namespace, releaseName string) error {
	return nil
}

// --- Test helpers ---

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(cetirunv1alpha1.AddToScheme(s))
	utilruntime.Must(appsv1.AddToScheme(s))
	utilruntime.Must(istiov1beta1.AddToScheme(s))
	// corev1 and rbacv1 are registered by clientgoscheme.AddToScheme
	_ = rbacv1.AddToScheme(s)
	return s
}

// newCR builds a minimal Agent for tests. The finalizer is pre-set
// so the reconciler does not spend a loop adding it. Runtime pins describe a
// previously prepared Agent; pinning itself is tested separately.
func newCR(alias string, phase string, conditions []metav1.Condition, generation int64, observedGen int64, observedHash string) *cetirunv1alpha1.Agent {
	cr := &cetirunv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       alias,
			UID:        types.UID("agent-" + alias),
			Generation: generation,
			Finalizers: []string{cetirunv1alpha1.Finalizer},
		},
		Spec: cetirunv1alpha1.AgentSpec{
			Agent: cetirunv1alpha1.HarnessSpec{
				Type:  cetirunv1alpha1.AgentTypeOpenClaw,
				Image: ptr.To("mcr.microsoft.com/aks/ai-runtime/cetirun/openclaw-gateway:0.1.1"), Port: ptr.To[int32](18789),
			},
			User: cetirunv1alpha1.UserSpec{
				Alias:       alias,
				DisplayName: "Test User",
				Team:        "test-team",
			},
			GatewayTokenSecretRef: "gateway-token",
			Helm: cetirunv1alpha1.HelmSpec{
				Version: "0.1.28", // Pre-set to avoid backfill loop in tests.
				Values:  map[string]runtime.RawExtension{},
			},
		},
		Status: cetirunv1alpha1.AgentStatus{
			Phase:                phase,
			Conditions:           conditions,
			ObservedGeneration:   observedGen,
			ObservedOperatorHash: observedHash,
		},
	}
	return cr
}

func gatewayTokenSecret(alias string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-token",
			Namespace: "cetirun-" + alias,
		},
		Data: map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte("test-token"),
			"CETIRUN_AUTH_KEY":       []byte("test-auth-key"),
		},
	}
}

func instanceNamespace(alias string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cetirun-" + alias,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: cetirunv1alpha1.GroupVersion.String(), Kind: "Agent", Name: alias, UID: types.UID("agent-" + alias),
			}},
		},
	}
}

func statefulSet(alias string, readyReplicas int32) *appsv1.StatefulSet {
	return statefulSetFull(alias, readyReplicas, readyReplicas, 1, 1)
}

func statefulSetFull(alias string, readyReplicas, updatedReplicas int32, generation, observedGen int64) *appsv1.StatefulSet {
	replicas := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "openclaw-" + alias + "-cetirun",
			Namespace:  "cetirun-" + alias,
			Generation: generation,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: cetirunv1alpha1.GroupVersion.String(), Kind: "Agent", Name: alias, UID: types.UID("agent-" + alias),
			}},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Env: []corev1.EnvVar{
					{Name: "CETIRUN_AI_GATEWAY", Value: "true"},
					{Name: "OPENAI_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "ai-gateway-virtual-key"}, Key: "api-key"}}},
				}}},
				InitContainers: []corev1.Container{{Name: "init-home-data", Env: []corev1.EnvVar{{Name: "CETIRUN_AI_GATEWAY", Value: "true"}}}},
			}},
		},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas:      readyReplicas,
			UpdatedReplicas:    updatedReplicas,
			ObservedGeneration: observedGen,
		},
	}
}

func statefulSetSuspended(alias string) *appsv1.StatefulSet {
	replicas := int32(0)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "openclaw-" + alias + "-cetirun",
			Namespace:  "cetirun-" + alias,
			Generation: 1,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
		},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas:      0,
			UpdatedReplicas:    0,
			ObservedGeneration: 1,
		},
	}
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// --- Tests ---

func TestReconcile_UnsupportedAgentDoesNotProvision(t *testing.T) {
	requireLegacyDirectRuntime(t)
	for _, typ := range []cetirunv1alpha1.AgentType{"", "unknown"} {
		t.Run(string(typ), func(t *testing.T) {
			cr := newCR("example-agent", "", nil, 1, 0, "")
			cr.Spec.Agent = cetirunv1alpha1.HarnessSpec{
				Type: typ, Image: ptr.To("registry.example.com/agent:1"),
				RegistrySecretRef: &cetirunv1alpha1.RegistrySecretRef{Name: "source"},
			}
			cr.Spec.GatewayTokenSecretRef = ""
			s := testScheme()
			kube := fake.NewClientBuilder().WithScheme(s).WithObjects(cr).
				WithStatusSubresource(cr).Build()
			helm := &fakeHelmClient{}
			r := &controller.AgentReconciler{
				Client: kube, Scheme: s, HelmClient: helm,
				EventRecorder: record.NewFakeRecorder(10),
			}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
			if err == nil || helm.installCalled {
				t.Fatal("unsupported agent must fail before Helm is called")
			}
			ns := &corev1.Namespace{}
			err = kube.Get(context.Background(), types.NamespacedName{Name: "cetirun-" + cr.Name}, ns)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("must not create runtime namespace, got %v", err)
			}
			secrets := &corev1.SecretList{}
			if err := kube.List(context.Background(), secrets); err != nil || len(secrets.Items) != 0 {
				t.Fatalf("must not create mixed or registry Secrets: %v", err)
			}
			got := &cetirunv1alpha1.Agent{}
			if err := kube.Get(context.Background(), types.NamespacedName{Name: cr.Name}, got); err != nil {
				t.Fatal(err)
			}
			condition := findCondition(got.Status.Conditions, cetirunv1alpha1.ConditionAgentConfigReady)
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "AgentRuntimeNotImplemented" {
				t.Fatalf("missing explicit runtime boundary condition: %v", condition)
			}
		})
	}
}

// TestPhaseConstants verifies phase constant values.
func TestPhaseConstants(t *testing.T) {
	phases := []string{
		cetirunv1alpha1.PhasePending,
		cetirunv1alpha1.PhaseProvisioning,
		cetirunv1alpha1.PhaseReady,
		cetirunv1alpha1.PhaseFailed,
		cetirunv1alpha1.PhaseDeleting,
		cetirunv1alpha1.PhaseSuspended,
	}
	for _, p := range phases {
		if p == "" {
			t.Errorf("phase constant is empty")
		}
	}
}

// TestFinalizerConstant verifies the stable finalizer name.
func TestFinalizerConstant(t *testing.T) {
	if cetirunv1alpha1.Finalizer != "cetirun.microsoft.com/finalizer" {
		t.Errorf("unexpected finalizer: %s", cetirunv1alpha1.Finalizer)
	}
}

func TestReconcile_PodNotReady_StaysProvisioning(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "alice"

	cr := newCR(alias, cetirunv1alpha1.PhaseProvisioning, nil, 1, 0, "")
	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias), statefulSet(alias, 0)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{useHarness: true}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      "example.com",
	}
	controller.ConfigureTestGateway(t, r)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s, got %v", result.RequeueAfter)
	}

	// Fetch updated CR.
	updated := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: alias}, updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}
	if updated.Status.Phase != cetirunv1alpha1.PhaseProvisioning {
		t.Errorf("expected phase=%s, got %s", cetirunv1alpha1.PhaseProvisioning, updated.Status.Phase)
	}

	podReady := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionPodReady)
	if podReady == nil {
		t.Fatal("expected PodReady condition to be set")
	}
	if podReady.Status != metav1.ConditionFalse {
		t.Errorf("expected PodReady=False, got %s", podReady.Status)
	}
	if podReady.Reason != "PodNotReady" {
		t.Errorf("expected PodReady reason=PodNotReady, got %s", podReady.Reason)
	}
}

func TestReconcile_PodReady_MarksReady(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "bob"

	cr := newCR(alias, cetirunv1alpha1.PhaseProvisioning, nil, 1, 0, "")
	cr.Status.Conditions = []metav1.Condition{{
		Type: cetirunv1alpha1.ConditionAgentConfigReady, Status: metav1.ConditionFalse,
		Reason: "AgentRuntimeNotImplemented", Message: "A removed override was unsupported",
		LastTransitionTime: metav1.Now(),
	}}
	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias), statefulSet(alias, 1)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{useHarness: true}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      "example.com",
	}
	controller.ConfigureTestGateway(t, r)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	if result.RequeueAfter != 10*time.Minute {
		t.Errorf("expected RequeueAfter=10m, got %v", result.RequeueAfter)
	}

	updated := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: alias}, updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}
	if updated.Status.Phase != cetirunv1alpha1.PhaseReady {
		t.Errorf("expected phase=%s, got %s", cetirunv1alpha1.PhaseReady, updated.Status.Phase)
	}
	if condition := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionAgentConfigReady); condition == nil ||
		condition.Status != metav1.ConditionTrue || condition.Reason != "AgentConfigResolved" {
		t.Fatal("resolved runtime must replace the stale failure condition")
	}

	podReady := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionPodReady)
	if podReady == nil {
		t.Fatal("expected PodReady condition to be set")
	}
	if podReady.Status != metav1.ConditionTrue {
		t.Errorf("expected PodReady=True, got %s", podReady.Status)
	}

	ready := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition to be set")
	}
	if ready.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True, got %s", ready.Status)
	}
}

func TestReconcile_RolloutIncomplete_StaysProvisioning(t *testing.T) {
	requireLegacyDirectRuntime(t)
	cases := []struct {
		name            string
		readyReplicas   int32
		updatedReplicas int32
		stsGeneration   int64
		stsObservedGen  int64
	}{
		{
			name:            "old pod ready, no updated replicas (rolling update not started)",
			readyReplicas:   1,
			updatedReplicas: 0,
			stsGeneration:   2,
			stsObservedGen:  2,
		},
		{
			name:            "updated but not yet ready",
			readyReplicas:   0,
			updatedReplicas: 1,
			stsGeneration:   2,
			stsObservedGen:  2,
		},
		{
			name:            "STS controller not yet observed new generation",
			readyReplicas:   1,
			updatedReplicas: 1,
			stsGeneration:   2,
			stsObservedGen:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alias := "rollout"

			cr := newCR(alias, cetirunv1alpha1.PhaseProvisioning, nil, 1, 0, "")
			sts := statefulSetFull(alias, tc.readyReplicas, tc.updatedReplicas, tc.stsGeneration, tc.stsObservedGen)

			s := testScheme()
			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias), sts).
				WithStatusSubresource(cr).
				Build()

			r := &controller.AgentReconciler{
				Client:            fakeClient,
				Scheme:            s,
				EventRecorder:     record.NewFakeRecorder(10),
				HelmClient:        &fakeHelmClient{useHarness: true},
				OperatorNamespace: "cetirun-system",
				OperatorSAName:    "cetirun-operator-controller-manager",
				ChartVersion:      "0.1.28",
				DomainSuffix:      "example.com",
			}
			controller.ConfigureTestGateway(t, r)

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: alias},
			})
			if err != nil {
				t.Fatalf("Reconcile returned unexpected error: %v", err)
			}
			if result.RequeueAfter != 15*time.Second {
				t.Errorf("expected RequeueAfter=15s, got %v", result.RequeueAfter)
			}

			updated := &cetirunv1alpha1.Agent{}
			if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: alias}, updated); err != nil {
				t.Fatalf("failed to get updated CR: %v", err)
			}
			if updated.Status.Phase != cetirunv1alpha1.PhaseProvisioning {
				t.Errorf("expected phase=%s, got %s", cetirunv1alpha1.PhaseProvisioning, updated.Status.Phase)
			}

			podReady := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionPodReady)
			if podReady == nil {
				t.Fatal("expected PodReady condition to be set")
			}
			if podReady.Status != metav1.ConditionFalse {
				t.Errorf("expected PodReady=False, got %s", podReady.Status)
			}
		})
	}
}

func TestReconcile_StatefulSetNotFound_Requeues(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "carol"

	cr := newCR(alias, cetirunv1alpha1.PhaseProvisioning, nil, 1, 0, "")
	s := testScheme()
	// No StatefulSet seeded.
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{useHarness: true}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      "example.com",
	}
	controller.ConfigureTestGateway(t, r)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s, got %v", result.RequeueAfter)
	}

	updated := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: alias}, updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}

	podReady := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionPodReady)
	if podReady == nil {
		t.Fatal("expected PodReady condition to be set")
	}
	if podReady.Status != metav1.ConditionFalse {
		t.Errorf("expected PodReady=False, got %s", podReady.Status)
	}
	if podReady.Reason != "StatefulSetNotFound" {
		t.Errorf("expected PodReady reason=StatefulSetNotFound, got %s", podReady.Reason)
	}
}

func TestReconcile_AlreadyDeployed_SkipsHelm(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "dave"
	domainSuffix := "example.com"

	conditions := []metav1.Condition{
		{
			Type:               cetirunv1alpha1.ConditionHelmDeployed,
			Status:             metav1.ConditionTrue,
			Reason:             "ChartInstalled",
			Message:            "Helm chart successfully installed",
			LastTransitionTime: metav1.Now(),
		},
	}

	gwSecretData := gatewayTokenSecret(alias).Data
	opHash := computeOperatorHash(domainSuffix, computeSecretHash(gwSecretData), "", "", "", "", "", "", "", "", "")

	cr := newCR(alias, cetirunv1alpha1.PhaseProvisioning, conditions, 1, 1, opHash)
	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias), statefulSet(alias, 0)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{useHarness: true}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      domainSuffix,
	}
	controller.ConfigureTestGateway(t, r)
	// Establish the Gateway projection and actual rendered cache key first.
	for range 2 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: alias}}); err != nil {
			t.Fatal(err)
		}
	}
	if !helmFake.installCalled {
		t.Fatal("fixture never deployed its Gateway workload")
	}
	helmFake.installCalled = false

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s, got %v", result.RequeueAfter)
	}

	if helmFake.installCalled {
		t.Error("expected HelmClient.InstallOrUpgrade NOT to be called, but it was")
	}

	updated := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: alias}, updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}

	podReady := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionPodReady)
	if podReady == nil {
		t.Fatal("expected PodReady condition to be set")
	}
	if podReady.Status != metav1.ConditionFalse {
		t.Errorf("expected PodReady=False, got %s", podReady.Status)
	}
}

func TestReconcile_TeamCompatibilityRefreshDoesNotUpgradeRuntime(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "team-cache"
	team := &cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "engineering", UID: "team-uid", Generation: 1},
		Spec:       cetirunv1alpha1.TeamSpec{MemberContext: cetirunv1alpha1.TeamContextSpec{Role: "Engineer"}},
	}
	cr := newCR(alias, cetirunv1alpha1.PhaseProvisioning, nil, 1, 1, "")
	cr.Spec.Team = team.Name
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cr, team, instanceNamespace(alias), gatewayTokenSecret(alias), statefulSet(alias, 0)).
		WithStatusSubresource(&cetirunv1alpha1.Agent{}, &cetirunv1alpha1.Team{}).
		Build()
	helmFake := &fakeHelmClient{useHarness: true}
	r := &controller.AgentReconciler{
		Client: fakeClient, Scheme: scheme, EventRecorder: record.NewFakeRecorder(10), HelmClient: helmFake,
		OperatorNamespace: "cetirun-system", OperatorSAName: "cetirun-operator-controller-manager",
		ChartVersion: "0.1.28", DomainSuffix: "example.com",
	}
	controller.ConfigureTestGateway(t, r)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: alias}}
	for range 2 {
		if _, err := r.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if helmFake.installCalls == 0 {
		t.Fatal("fixture did not install its initial runtime")
	}
	initialCalls := helmFake.installCalls

	currentTeam := &cetirunv1alpha1.Team{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(team), currentTeam); err != nil {
		t.Fatal(err)
	}
	currentTeam.Status.Compatibility = &cetirunv1alpha1.TeamCompatibilityStatus{
		State:     cetirunv1alpha1.CompatibilityCompatible,
		Team:      cetirunv1alpha1.CompatibilityReference{Name: currentTeam.Name, UID: string(currentTeam.UID), Generation: currentTeam.Generation},
		CheckedAt: metav1.Now(), ValidUntil: metav1.NewTime(time.Now().Add(30 * time.Second)),
	}
	if err := fakeClient.Status().Update(context.Background(), currentTeam); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if helmFake.installCalls != initialCalls {
		t.Fatalf("compatibility refresh installed the runtime %d additional times", helmFake.installCalls-initialCalls)
	}

	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(team), currentTeam); err != nil {
		t.Fatal(err)
	}
	currentTeam.Spec.MemberContext.Role = "Reviewer"
	if err := fakeClient.Update(context.Background(), currentTeam); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if helmFake.installCalls != initialCalls+1 {
		t.Fatalf("Team context update installed %d times, want exactly once", helmFake.installCalls-initialCalls)
	}
}

// TestReconcile_SecretChanged_TriggersHelm verifies that when the gateway Secret
// content changes (e.g. token rotation), a Ready instance triggers a full reconcile
// with Helm upgrade instead of being skipped.
func TestReconcile_SecretChanged_TriggersHelm(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "eve"
	domainSuffix := "example.com"

	conditions := []metav1.Condition{
		{
			Type:               cetirunv1alpha1.ConditionHelmDeployed,
			Status:             metav1.ConditionTrue,
			Reason:             "ChartInstalled",
			Message:            "Helm chart successfully installed",
			LastTransitionTime: metav1.Now(),
		},
		{
			Type:               cetirunv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "InstanceReady",
			Message:            "Instance is fully operational",
			LastTransitionTime: metav1.Now(),
		},
	}

	cr := newCR(alias, cetirunv1alpha1.PhaseReady, conditions, 1, 1, "")

	// Start with the original token and establish the Gateway workload.
	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-token",
			Namespace: "cetirun-" + alias,
		},
		Data: map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte("original-token"),
		},
	}

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), newSecret, statefulSet(alias, 1)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{useHarness: true}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      domainSuffix,
	}
	controller.ConfigureTestGateway(t, r)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: alias}}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	helmFake.installCalled = false
	if _, err := r.Reconcile(context.Background(), request); err != nil || helmFake.installCalled {
		t.Fatalf("unchanged Gateway workload did not settle: %v", err)
	}
	rotated := &corev1.Secret{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(newSecret), rotated); err != nil {
		t.Fatal(err)
	}
	rotated.Data["OPENCLAW_GATEWAY_TOKEN"] = []byte("new-rotated-token")
	if err := fakeClient.Update(context.Background(), rotated); err != nil {
		t.Fatal(err)
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}

	// Should NOT have been skipped (10m requeue = skip path).
	// Instead, full reconcile should run and reach Ready (10m) with Helm called.
	if result.RequeueAfter != 10*time.Minute {
		t.Errorf("expected RequeueAfter=10m (full reconcile → Ready), got %v", result.RequeueAfter)
	}

	if !helmFake.installCalled {
		t.Error("expected HelmClient.InstallOrUpgrade to be called after Secret content changed, but it was NOT")
	}
}

// TestReconcile_ReadyInstanceBackfillsCetiRunAuthKey verifies that the
// skip-cache does not prevent upgrades of existing instances whose
// operator-managed gateway Secret predates CETIRUN_AUTH_KEY.
func TestReconcile_ReadyInstanceBackfillsCetiRunAuthKey(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "legacy-auth-key"
	domainSuffix := "example.com"
	legacySecretData := map[string][]byte{"OPENCLAW_GATEWAY_TOKEN": []byte("legacy-token")}
	legacyHash := computeOperatorHash(domainSuffix, computeSecretHash(legacySecretData), "", "", "", "", "", "", "", "", "")

	conditions := []metav1.Condition{
		{
			Type:               cetirunv1alpha1.ConditionHelmDeployed,
			Status:             metav1.ConditionTrue,
			Reason:             "ChartInstalled",
			Message:            "Helm chart successfully installed",
			LastTransitionTime: metav1.Now(),
		},
		{
			Type:               cetirunv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "InstanceReady",
			Message:            "Instance is fully operational",
			LastTransitionTime: metav1.Now(),
		},
	}
	cr := newCR(alias, cetirunv1alpha1.PhaseReady, conditions, 1, 1, legacyHash)
	legacySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-token", Namespace: "cetirun-" + alias},
		Data:       legacySecretData,
	}

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), legacySecret, statefulSet(alias, 1)).
		WithStatusSubresource(cr).
		Build()
	helmFake := &fakeHelmClient{useHarness: true}
	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      domainSuffix,
	}
	controller.ConfigureTestGateway(t, r)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: alias}}); err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	if !helmFake.installCalled {
		t.Fatal("expected Helm install or upgrade after CETIRUN_AUTH_KEY backfill")
	}
	podAnnotations, ok := helmFake.lastValues["podAnnotations"].(map[string]interface{})
	if !ok {
		t.Fatal("Helm values do not include podAnnotations")
	}
	if got, _ := podAnnotations["cetirun.microsoft.com/gateway-secret-resource-version"].(string); got == "" {
		t.Fatal("Helm values do not trigger a StatefulSet rollout after CETIRUN_AUTH_KEY backfill")
	}
	updatedSecret := &corev1.Secret{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "cetirun-" + alias, Name: "gateway-token"}, updatedSecret); err != nil {
		t.Fatalf("get updated gateway Secret: %v", err)
	}
	if got := string(updatedSecret.Data["CETIRUN_AUTH_KEY"]); got == "" {
		t.Fatal("CETIRUN_AUTH_KEY was not backfilled")
	}
}

func TestReconcile_RejectsReservedUIInstance(t *testing.T) {
	alias := "ui"
	cr := newCR(alias, cetirunv1alpha1.PhaseReady, nil, 1, 1, "")
	vs := &istiov1beta1.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cetirun-ui",
			Namespace: "cetirun-ui",
		},
	}

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, vs).
		WithStatusSubresource(cr).
		Build()
	helmFake := &fakeHelmClient{useHarness: true}
	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      "example.com",
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: alias}}); err == nil {
		t.Fatal("expected reconcile to reject the reserved instance name")
	}
	if helmFake.installCalled {
		t.Fatal("reserved instance must not run Helm deployment")
	}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "cetirun-ui", Namespace: "cetirun-ui"}, &istiov1beta1.VirtualService{}); err == nil {
		t.Fatal("VirtualService for reserved instance was not deleted")
	}

	updated := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: alias}, updated); err != nil {
		t.Fatalf("get reserved instance: %v", err)
	}
	if updated.Status.Phase != cetirunv1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", updated.Status.Phase)
	}
}

// computeOperatorHash reproduces the operatorConfigHash logic for test seeding.
// The optional config values mirror configHash's cache-invalidation inputs.
func computeOperatorHash(domainSuffix, secretHash, skillsStorageAccount, cetirunHelmValuesHash, chartRegistry string, config ...string) string {
	var payload string
	if skillsStorageAccount == "" && cetirunHelmValuesHash == "" && chartRegistry == "" && len(config) == 0 {
		payload = domainSuffix + "\x00" + secretHash
	} else {
		payload = domainSuffix + "\x00" + secretHash + "\x00" + skillsStorageAccount + "\x00" + cetirunHelmValuesHash + "\x00" + chartRegistry
		for _, v := range config {
			payload += "\x00" + v
		}
	}
	h := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(h[:8])
}

// computeSecretHash reproduces the secretContentHash logic for test seeding.
func computeSecretHash(data map[string][]byte) string {
	if len(data) == 0 {
		return ""
	}
	h := sha256.New()
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// TestReconcile_EmptyVersion_Backfills verifies that when spec.helm.version is empty,
// the reconciler backfills it with the operator's default chart version and requeues.
func TestReconcile_EmptyVersion_Backfills(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "frank"
	chartVersion := "0.1.28"

	cr := newCR(alias, cetirunv1alpha1.PhaseProvisioning, nil, 1, 0, "")
	// Clear the version to simulate a CR without a pinned version.
	cr.Spec.Helm.Version = ""

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{useHarness: true}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      chartVersion,
		DomainSuffix:      "example.com",
	}
	controller.ConfigureTestGateway(t, r)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("expected Requeue=true after backfill, got false")
	}

	// Helm should NOT have been called — backfill requeues before reaching Helm.
	if helmFake.installCalled {
		t.Error("expected HelmClient.InstallOrUpgrade NOT to be called during backfill, but it was")
	}

	// Verify the CR's spec.helm.version was backfilled.
	updated := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: alias}, updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}
	if updated.Spec.Helm.Version != chartVersion {
		t.Errorf("expected spec.helm.version=%q after backfill, got %q", chartVersion, updated.Spec.Helm.Version)
	}
}

// TestReconcile_VersionSet_PassesToHelm verifies that when spec.helm.version is set,
// the reconciler passes it through to HelmClient.InstallOrUpgrade.
func TestReconcile_VersionSet_PassesToHelm(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "grace"
	pinnedVersion := "0.1.9"

	cr := newCR(alias, cetirunv1alpha1.PhaseProvisioning, nil, 1, 0, "")
	cr.Spec.Helm.Version = pinnedVersion

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias), statefulSet(alias, 1)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{useHarness: true}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28", // Operator default differs from pinned version.
		DomainSuffix:      "example.com",
	}
	controller.ConfigureTestGateway(t, r)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}

	if !helmFake.installCalled {
		t.Fatal("expected HelmClient.InstallOrUpgrade to be called, but it was NOT")
	}
	if helmFake.lastChartVersion != pinnedVersion {
		t.Errorf("expected chartVersion=%q passed to Helm, got %q", pinnedVersion, helmFake.lastChartVersion)
	}
}

// TestConditionTailscaleReady verifies the TailscaleReady condition constant.
func TestConditionTailscaleReady(t *testing.T) {
	if cetirunv1alpha1.ConditionTailscaleReady != "TailscaleReady" {
		t.Errorf("unexpected condition: %s", cetirunv1alpha1.ConditionTailscaleReady)
	}
}

// TestTailscaleSpec_TypeConstruction verifies CRD struct with Tailscale fields.
func TestTailscaleSpec_TypeConstruction(t *testing.T) {
	instance := &cetirunv1alpha1.Agent{
		Spec: cetirunv1alpha1.AgentSpec{
			Team: "example-team",
			User: cetirunv1alpha1.UserSpec{
				Alias:       "alice",
				DisplayName: "Alice Smith",
			},
			GatewayTokenSecretRef: "gateway-token",
			Tailscale: &cetirunv1alpha1.TailscaleSpec{
				AuthKeySecretRef: "tailscale-auth",
			},
		},
	}

	if instance.Spec.Tailscale == nil {
		t.Fatal("Tailscale spec should not be nil")
	}
	if instance.Spec.Tailscale.AuthKeySecretRef != "tailscale-auth" {
		t.Errorf("AuthKeySecretRef = %q, want %q", instance.Spec.Tailscale.AuthKeySecretRef, "tailscale-auth")
	}

	// Verify nil Tailscale is valid (feature disabled).
	instanceNoTS := &cetirunv1alpha1.Agent{
		Spec: cetirunv1alpha1.AgentSpec{
			Team: "example-team",
			User: cetirunv1alpha1.UserSpec{
				Alias:       "bob",
				DisplayName: "Bob",
			},
			GatewayTokenSecretRef: "gateway-token",
		},
	}
	if instanceNoTS.Spec.Tailscale != nil {
		t.Error("Tailscale spec should be nil when not configured")
	}
}

// TestReconcile_Suspended_MarksSuspendedPhase verifies that when spec.suspended=true,
// the reconciler sets phase=Suspended, PodReady=False, Ready=False, and calls Helm
// with replicaCount=0.
func TestReconcile_Suspended_MarksSuspendedPhase(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "suspended-user"

	cr := newCR(alias, cetirunv1alpha1.PhaseProvisioning, nil, 1, 0, "")
	cr.Spec.Suspended = true

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias), statefulSetSuspended(alias)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      "example.com",
	}
	controller.ConfigureTestGateway(t, r)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	if result.RequeueAfter != time.Minute {
		t.Errorf("expected RequeueAfter=1m, got %v", result.RequeueAfter)
	}

	// Suspension stops compute without resolving a Chart.
	if helmFake.installCalled {
		t.Error("suspension must not call Helm")
	}

	// Fetch updated CR and verify status.
	updated := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: alias}, updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}
	if updated.Status.Phase != cetirunv1alpha1.PhaseSuspended {
		t.Errorf("expected phase=%s, got %s", cetirunv1alpha1.PhaseSuspended, updated.Status.Phase)
	}

	podReady := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionPodReady)
	if podReady == nil {
		t.Fatal("expected PodReady condition to be set")
	}
	if podReady.Status != metav1.ConditionFalse {
		t.Errorf("expected PodReady=False, got %s", podReady.Status)
	}
	if podReady.Reason != "Suspended" {
		t.Errorf("expected PodReady reason=Suspended, got %s", podReady.Reason)
	}

	ready := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition to be set")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False, got %s", ready.Status)
	}
	if ready.Reason != "Suspended" {
		t.Errorf("expected Ready reason=Suspended, got %s", ready.Reason)
	}
}

// TestReconcile_Unsuspend_MarksReady verifies that when spec.suspended changes
// from true to false, the reconciler transitions through Provisioning back to Ready.
func TestReconcile_Unsuspend_MarksReady(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "unsuspend-user"

	// Start with a Suspended instance that now has suspended=false (unsuspending).
	cr := newCR(alias, cetirunv1alpha1.PhaseSuspended, nil, 2, 1, "")
	cr.Spec.Suspended = false

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias), statefulSet(alias, 1)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{useHarness: true}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      "example.com",
	}
	controller.ConfigureTestGateway(t, r)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	if result.RequeueAfter != 10*time.Minute {
		t.Errorf("expected RequeueAfter=10m, got %v", result.RequeueAfter)
	}

	updated := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: alias}, updated); err != nil {
		t.Fatalf("failed to get updated CR: %v", err)
	}
	if updated.Status.Phase != cetirunv1alpha1.PhaseReady {
		t.Errorf("expected phase=%s, got %s", cetirunv1alpha1.PhaseReady, updated.Status.Phase)
	}

	ready := findCondition(updated.Status.Conditions, cetirunv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition to be set")
	}
	if ready.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True, got %s", ready.Status)
	}
}

// TestReconcile_SuspendedSkipCache verifies the skip-cache works for
// Suspended instances (no Helm call when nothing changed).
func TestReconcile_SuspendedSkipCache(t *testing.T) {
	requireLegacyDirectRuntime(t)
	alias := "skip-cache-suspended"

	// Compute the expected operator hash for skip-cache to match.
	gwSecretData := gatewayTokenSecret(alias).Data
	gwHash := computeSecretHash(gwSecretData)
	opHash := computeOperatorHash("example.com", gwHash, "", "", "", "", "", "", "", "", "")

	cr := newCR(alias, cetirunv1alpha1.PhaseSuspended, nil, 1, 1, opHash)
	cr.Spec.Suspended = true

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), gatewayTokenSecret(alias)).
		WithStatusSubresource(cr).
		Build()

	helmFake := &fakeHelmClient{}

	r := &controller.AgentReconciler{
		Client:            fakeClient,
		Scheme:            s,
		EventRecorder:     record.NewFakeRecorder(10),
		HelmClient:        helmFake,
		OperatorNamespace: "cetirun-system",
		OperatorSAName:    "cetirun-operator-controller-manager",
		ChartVersion:      "0.1.28",
		DomainSuffix:      "example.com",
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: alias},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	if result.RequeueAfter != time.Minute {
		t.Errorf("expected RequeueAfter=1m (suspended), got %v", result.RequeueAfter)
	}

	// Helm should NOT have been called (skip-cache hit).
	if helmFake.installCalled {
		t.Error("Helm InstallOrUpgrade should not be called when skip-cache matches for Suspended instance")
	}
}

// staleAgentClient serves an Agent as the informer can during a pass that
// starts before the previous pass's status arrived: an older revision.
type staleAgentClient struct {
	client.Client
	stale *cetirunv1alpha1.Agent
}

func (c staleAgentClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if agent, ok := obj.(*cetirunv1alpha1.Agent); ok && key.Name == c.stale.Name {
		c.stale.DeepCopyInto(agent)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestReconcile_StaleAgentCopyDoesNotInstall(t *testing.T) {
	requireLegacyDirectRuntime(t)
	ctx := context.Background()
	alias := "stale"
	cr := newCR(alias, cetirunv1alpha1.PhaseReady, nil, 1, 1, "outdated-hash")
	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cr, instanceNamespace(alias), statefulSet(alias, 1)).
		WithStatusSubresource(cr).
		Build()
	key := types.NamespacedName{Name: alias}
	var gatewayState *controller.AgentReconciler
	newReconciler := func(cl client.Client) (*controller.AgentReconciler, *fakeHelmClient) {
		helmFake := &fakeHelmClient{useHarness: true}
		r := &controller.AgentReconciler{
			Client: cl, APIReader: fakeClient, Scheme: s, EventRecorder: record.NewFakeRecorder(10),
			HelmClient: helmFake, OperatorNamespace: "cetirun-system",
			OperatorSAName: "cetirun-operator-controller-manager", ChartVersion: "0.1.28", DomainSuffix: "example.com",
		}
		if gatewayState == nil {
			controller.ConfigureTestGateway(t, r)
			gatewayState = r
		} else {
			controller.ReuseTestGateway(t, gatewayState, r)
		}
		return r, helmFake
	}
	reconcile := func(cl client.Client) (ctrl.Result, *fakeHelmClient) {
		t.Helper()
		r, helmFake := newReconciler(cl)
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("Reconcile returned unexpected error: %v", err)
		}
		return result, helmFake
	}
	for i := 0; i < 3; i++ {
		reconcile(fakeClient)
	}
	if _, helmFake := reconcile(fakeClient); helmFake.installCalled {
		t.Fatal("the fixture must reach a steady state")
	}
	// The copy the informer still serves comes from before the previous pass
	// recorded its render ...
	stale := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(ctx, key, stale); err != nil {
		t.Fatal(err)
	}
	stale.Status.ObservedOperatorHash = "before-the-previous-pass"
	// ... which the API server already holds, with a newer revision.
	latest := &cetirunv1alpha1.Agent{}
	if err := fakeClient.Get(ctx, key, latest); err != nil {
		t.Fatal(err)
	}
	latest.Annotations = map[string]string{"example.com/touched": "true"}
	if err := fakeClient.Update(ctx, latest); err != nil {
		t.Fatal(err)
	}
	result, helmFake := reconcile(staleAgentClient{Client: fakeClient, stale: stale})
	if helmFake.installCalled {
		t.Fatal("installed from an older copy of the Agent")
	}
	if !result.Requeue {
		t.Fatalf("expected a requeue to read the latest Agent, got %+v", result)
	}
	// The latest copy says the release is already rendered.
	if _, helmFake = reconcile(fakeClient); helmFake.installCalled {
		t.Fatal("the latest Agent was already installed")
	}
}
