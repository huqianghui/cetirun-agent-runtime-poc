// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
)

var lifecycleEventReasons = []string{"InstanceCreating", "IngressConfigured", "InstanceReady", "InstanceSuspended"}

// lifecycleEvents drains the recorder and counts the lifecycle events by
// reason, with the message of the last IngressConfigured event.
func lifecycleEvents(recorder *record.FakeRecorder) (map[string]int, string) {
	counts := map[string]int{}
	ingress := ""
	for {
		select {
		case event := <-recorder.Events:
			for _, reason := range lifecycleEventReasons {
				if strings.Contains(event, " "+reason+" ") {
					counts[reason]++
					if reason == "IngressConfigured" {
						ingress = event
					}
				}
			}
		default:
			return counts, ingress
		}
	}
}

func TestEnsureVirtualServiceReportsWhatItChanged(t *testing.T) {
	agent := aiGatewayTestAgent("events-team")
	agent.Spec.SandboxRef = ""
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, cetirunv1alpha1.AddToScheme, istiov1beta1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	r := &AgentReconciler{Client: kube, Scheme: scheme, DomainSuffix: "example.com"}
	ctx := context.Background()
	key := types.NamespacedName{Namespace: instanceNamespace(agent.Name), Name: virtualServiceName(agent.Name)}

	if result, err := r.ensureVirtualService(ctx, agent); err != nil || result != controllerutil.OperationResultCreated {
		t.Fatalf("first ensure = %q, %v; want created", result, err)
	}
	created := &istiov1beta1.VirtualService{}
	if err := kube.Get(ctx, key, created); err != nil {
		t.Fatal(err)
	}
	if result, err := r.ensureVirtualService(ctx, agent); err != nil || result != controllerutil.OperationResultNone {
		t.Fatalf("repeated ensure = %q, %v; want unchanged", result, err)
	}
	unchanged := &istiov1beta1.VirtualService{}
	if err := kube.Get(ctx, key, unchanged); err != nil || unchanged.ResourceVersion != created.ResourceVersion {
		t.Fatalf("an unchanged route was written again: %v", err)
	}
	r.IstioGateway = "other-namespace/other-gateway"
	if result, err := r.ensureVirtualService(ctx, agent); err != nil || result != controllerutil.OperationResultUpdated {
		t.Fatalf("changed ensure = %q, %v; want updated", result, err)
	}
}

// A Ready Agent is reconciled in full whenever a verification falls due, by
// default every ten minutes. Lifecycle events report state changes, so such a
// pass records none; creation, readiness and suspension are reported once.
func TestLifecycleEventsReportTransitionsOnly(t *testing.T) {
	r, agent, _ := legacyGatewayFixture(t)
	chart := &chartHelm{}
	r.HelmClient = chart
	recorder := record.NewFakeRecorder(100)
	r.EventRecorder = recorder
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: instanceNamespace(agent.Name), Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, ObservedGeneration: 1},
	}
	if err := r.Create(ctx, sts); err != nil {
		t.Fatal(err)
	}
	stored := func() *cetirunv1alpha1.Agent {
		t.Helper()
		current := &cetirunv1alpha1.Agent{}
		if err := r.Get(ctx, req.NamespacedName, current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	reconcileUntil := func(phase string) {
		t.Helper()
		for i := 0; i < 4 && stored().Status.Phase != phase; i++ {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile %d: %v", i, err)
			}
		}
		if got := stored().Status.Phase; got != phase {
			t.Fatalf("phase = %q, want %q", got, phase)
		}
	}
	// A verification that is due makes the next reconcile take the full path,
	// as the periodic requeue does.
	verificationDue := func() {
		t.Helper()
		current := stored()
		if current.Status.AIGateway == nil {
			t.Fatal("the Agent has no AI Gateway status to verify")
		}
		current.Status.AIGateway.LastVerifiedTime = &metav1.Time{Time: time.Now().Add(-2 * aiGatewayVerificationInterval)}
		if err := r.Status().Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(step string, want map[string]int) {
		t.Helper()
		got, ingress := lifecycleEvents(recorder)
		for _, reason := range lifecycleEventReasons {
			if got[reason] != want[reason] {
				t.Errorf("%s: %d %s events, want %d", step, got[reason], reason, want[reason])
			}
		}
		if want["IngressConfigured"] > 0 && !strings.Contains(ingress, "VirtualService created") {
			t.Errorf("%s: IngressConfigured does not say the route was created: %q", step, ingress)
		}
	}

	reconcileUntil(cetirunv1alpha1.PhaseReady)
	if chart.installs != 1 {
		t.Fatalf("installs = %d, want 1", chart.installs)
	}
	expect("creation", map[string]int{"InstanceCreating": 1, "IngressConfigured": 1, "InstanceReady": 1})

	for pass := 1; pass <= 2; pass++ {
		verificationDue()
		verified := stored().Status.AIGateway.LastVerifiedTime.Time
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		current := stored()
		if !current.Status.AIGateway.LastVerifiedTime.After(verified) || current.Status.Phase != cetirunv1alpha1.PhaseReady {
			t.Fatalf("pass %d did not run in full and stay Ready: %+v", pass, current.Status.AIGateway)
		}
		expect("periodic pass", nil)
	}
	if chart.installs != 1 {
		t.Fatalf("a periodic pass reinstalled the chart: installs = %d", chart.installs)
	}

	// A deployed Agent that returns to Pending, as it does while it waits for
	// registry credentials, is not created again.
	current := stored()
	current.Status.Phase = cetirunv1alpha1.PhasePending
	if err := r.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileUntil(cetirunv1alpha1.PhaseReady)
	expect("return from Pending", map[string]int{"InstanceReady": 1})

	current = stored()
	current.Spec.Suspended = true
	current.Generation++ // the fake client does not maintain the generation
	if err := r.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileUntil(cetirunv1alpha1.PhaseSuspended)
	expect("suspension", map[string]int{"InstanceSuspended": 1})
	verificationDue()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	expect("suspended periodic pass", nil)
}

// A creation that takes several passes, here because the StatefulSet is not
// ready yet, is reported once.
func TestInstanceCreatingIsRecordedOncePerCreation(t *testing.T) {
	r, agent, _ := legacyGatewayFixture(t)
	r.HelmClient = &chartHelm{}
	recorder := record.NewFakeRecorder(100)
	r.EventRecorder = recorder
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: instanceNamespace(agent.Name), Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
		Status:     appsv1.StatefulSetStatus{UpdatedReplicas: 1, ObservedGeneration: 1},
	}
	if err := r.Create(ctx, sts); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	current := &cetirunv1alpha1.Agent{}
	if err := r.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != cetirunv1alpha1.PhaseProvisioning {
		t.Fatalf("phase = %q, want Provisioning while the Pod is not ready", current.Status.Phase)
	}
	got, _ := lifecycleEvents(recorder)
	if got["InstanceCreating"] != 1 || got["IngressConfigured"] != 1 || got["InstanceReady"] != 0 {
		t.Fatalf("events while waiting for the Pod: %v", got)
	}
}

// preparationHelm renders the chart like chartHelm, but its Prepare can be
// made to fail, as it does when the chart cannot be downloaded.
type preparationHelm struct {
	chartHelm
	failPrepare bool
}

func (h *preparationHelm) Prepare(namespace string, values map[string]any, chartVersion string) (*helm.PreparedRelease, error) {
	if h.failPrepare {
		return nil, errors.New("synthetic chart download failure")
	}
	return h.chartHelm.Prepare(namespace, values, chartVersion)
}

// An Agent whose chart could not be prepared before its first provisioning
// pass is Failed without having been reported as created. Its creation is
// reported when provisioning starts after all, and only once, also when a
// later step of that creation is retried.
func TestInstanceCreatingAfterAFailedFirstPreparation(t *testing.T) {
	r, agent, gateway := legacyGatewayFixture(t)
	chart := &preparationHelm{failPrepare: true}
	r.HelmClient = chart
	recorder := record.NewFakeRecorder(100)
	r.EventRecorder = recorder
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: instanceNamespace(agent.Name), Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, ObservedGeneration: 1},
	}
	if err := r.Create(ctx, sts); err != nil {
		t.Fatal(err)
	}
	stored := func() *cetirunv1alpha1.Agent {
		t.Helper()
		current := &cetirunv1alpha1.Agent{}
		if err := r.Get(ctx, req.NamespacedName, current); err != nil {
			t.Fatal(err)
		}
		return current
	}

	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("a failed chart preparation was not reported")
	}
	if phase := stored().Status.Phase; phase != cetirunv1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", phase)
	}
	if got, _ := lifecycleEvents(recorder); got["InstanceCreating"] != 0 {
		t.Fatalf("creation reported before provisioning started: %v", got)
	}

	// The chart can be prepared again, but the Agent's Gateway key cannot be
	// created yet: provisioning starts and is retried before the chart is
	// installed.
	chart.failPrepare = false
	gateway.getErr = errors.New("synthetic Gateway outage")
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if chart.installs != 0 {
		t.Fatalf("the chart was installed without the Agent's key: installs = %d", chart.installs)
	}
	if got, _ := lifecycleEvents(recorder); got["InstanceCreating"] != 1 {
		t.Fatalf("creation reported %d times while it was retried, want once", got["InstanceCreating"])
	}

	gateway.getErr = nil
	for i := 0; i < 4 && stored().Status.Phase != cetirunv1alpha1.PhaseReady; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if phase := stored().Status.Phase; phase != cetirunv1alpha1.PhaseReady || chart.installs != 1 {
		t.Fatalf("phase = %q, installs = %d; want Ready after one install", phase, chart.installs)
	}
	if got, _ := lifecycleEvents(recorder); got["InstanceCreating"] != 0 || got["IngressConfigured"] != 1 || got["InstanceReady"] != 1 {
		t.Fatalf("events when the creation completed: %v", got)
	}
}
