// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRetainedRestartValuesPreserveRuntimeAndHOME(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		t.Run(string(kind), func(t *testing.T) {
			agent, _, _ := restartObservationFixture()
			agent.Spec.RestartRequestID = ""
			agent.Spec.Helm.Values = map[string]runtime.RawExtension{}
			agent.Spec.Agent.Type = kind
			before, err := buildHelmValues(agent, nil, "example.com", "", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			agent.Spec.RestartRequestID = "request-00000001"
			agent.Spec.Helm.Values["podAnnotations"] = runtime.RawExtension{Raw: []byte(`{"cetirun.microsoft.com/restart-request-id":"untrusted-value"}`)}
			after, err := buildHelmValues(agent, nil, "example.com", "", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			annotations, ok := after["podAnnotations"].(map[string]any)
			if !ok || annotations[api.AnnotationRestartRequestID] != agent.Spec.RestartRequestID {
				t.Fatal("the caller overrode the controller-owned restart ID")
			}
			delete(annotations, api.AnnotationRestartRequestID)
			if len(annotations) == 0 {
				delete(after, "podAnnotations")
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("restart changed settings other than the rollout annotation")
			}
		})
	}
}

func restartObservationFixture() (*api.Agent, *appsv1.StatefulSet, *corev1.Pod) {
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "agent-uid", Generation: 2, Finalizers: []string{api.Finalizer}},
		Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeOpenClaw},
			User: api.UserSpec{Alias: "alice", DisplayName: "Alice"}, GatewayTokenSecretRef: "gateway-token"}}
	agent.Spec.RestartRequestID = "request-00000001"
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name),
		Namespace: instanceNamespace(agent.Name), UID: "workload-uid", Generation: 2},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "alice"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "alice"},
				Annotations: map[string]string{api.AnnotationRestartRequestID: agent.Spec.RestartRequestID}}}},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: 1, UpdatedReplicas: 1}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workload.Name + "-0", Namespace: workload.Namespace, UID: "replacement-pod",
		Labels: workload.Spec.Template.Labels, Annotations: workload.Spec.Template.Annotations,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	return agent, workload, pod
}

func restartTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := newTestScheme(t)
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestRetainedRestartWaitsForTheCurrentReadyPod(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*api.Agent, *appsv1.StatefulSet, *corev1.Pod)
		ready  bool
	}{
		{"current Pod", func(*api.Agent, *appsv1.StatefulSet, *corev1.Pod) {}, true},
		{"old Pod still Ready", func(_ *api.Agent, _ *appsv1.StatefulSet, p *corev1.Pod) { p.Annotations = nil }, false},
		{"old template", func(_ *api.Agent, s *appsv1.StatefulSet, _ *corev1.Pod) { s.Spec.Template.Annotations = nil }, false},
		{"foreign Pod", func(_ *api.Agent, _ *appsv1.StatefulSet, p *corev1.Pod) {
			p.OwnerReferences[0].UID = "foreign-workload"
		}, false},
		{"terminating Pod", func(_ *api.Agent, _ *appsv1.StatefulSet, p *corev1.Pod) {
			p.DeletionTimestamp = ptr.To(metav1.Now())
			p.Finalizers = []string{"example.com/hold"}
		}, false},
		{"unready Pod", func(_ *api.Agent, _ *appsv1.StatefulSet, p *corev1.Pod) { p.Status.Conditions = nil }, false},
		{"suspension wins", func(a *api.Agent, _ *appsv1.StatefulSet, _ *corev1.Pod) { a.Spec.Suspended = true }, false},
		{"stopped workload", func(_ *api.Agent, s *appsv1.StatefulSet, _ *corev1.Pod) { s.Spec.Replicas = ptr.To(int32(0)) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent, workload, pod := restartObservationFixture()
			test.change(agent, workload, pod)
			fc := fake.NewClientBuilder().WithScheme(restartTestScheme(t)).WithObjects(workload, pod).Build()
			r := &AgentReconciler{Client: fc, APIReader: fc}
			ready, _, err := r.observeWorkload(context.Background(), agent)
			if err != nil || ready != test.ready {
				t.Fatalf("ready=%v want=%v err=%v", ready, test.ready, err)
			}
			setWorkloadReadiness(agent, ready)
			if (agent.Status.ObservedRestartRequestID == agent.Spec.RestartRequestID) != test.ready {
				t.Fatal("request completion did not match current Pod observation")
			}
		})
	}
}

func TestRetainedRestartObservationErrorsAreNotCompletion(t *testing.T) {
	agent, workload, _ := restartObservationFixture()
	unavailable := errors.New("synthetic Pod read unavailable")
	fc := fake.NewClientBuilder().WithScheme(restartTestScheme(t)).WithObjects(workload).
		WithInterceptorFuncs(interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return unavailable
		}}).Build()
	r := &AgentReconciler{Client: fc, APIReader: fc}
	ready, _, err := r.observeWorkload(context.Background(), agent)
	if ready || !errors.Is(err, unavailable) || agent.Status.ObservedRestartRequestID != "" {
		t.Fatalf("unknown Pod observation completed restart: ready=%v err=%v", ready, err)
	}
}

func TestRetainedRestartApplyRechecksCurrentIntent(t *testing.T) {
	for _, change := range []func(*api.Agent){
		func(a *api.Agent) { a.UID = "replacement-agent" },
		func(a *api.Agent) { a.Generation++ },
		func(a *api.Agent) { a.Spec.RestartRequestID = "request-00000002" },
		func(a *api.Agent) { a.Spec.Suspended = true },
		func(a *api.Agent) { a.DeletionTimestamp = ptr.To(metav1.Now()) },
	} {
		expected, _, _ := restartObservationFixture()
		current := expected.DeepCopy()
		change(current)
		fc := fake.NewClientBuilder().WithScheme(restartTestScheme(t)).WithObjects(current).Build()
		r := &AgentReconciler{Client: fc, APIReader: fc}
		if err := r.checkAgentGeneration(context.Background(), expected); err == nil {
			t.Fatal("stale restart was authorized after current intent changed")
		}
	}
}

func TestRetainedRestartInvalidatesOlderOperatorCache(t *testing.T) {
	for _, phase := range []string{api.PhaseReady, api.PhaseProvisioning} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			f := newDeployedAgent(t)
			agent := f.agent(t)
			oldHash, keyID := agent.Status.ObservedOperatorHash, agent.Status.AIGateway.KeyID
			agent.Spec.RestartRequestID = "request-00000001"
			agent.Generation++
			if err := f.r.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			agent = f.agent(t)
			// An older controller acknowledges the generation without rendering
			// the new field, leaving its pre-restart applied hash unchanged.
			agent.Status.Phase = phase
			agent.Status.ObservedGeneration = agent.Generation
			agent.Status.ObservedOperatorHash = oldHash
			if err := f.r.Status().Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			installs := f.chart.installs
			if _, err := f.r.Reconcile(ctx, f.req); err != nil {
				t.Fatal(err)
			}
			if f.chart.installs != installs+1 {
				t.Fatal("older observedGeneration consumed a restart without a new Helm apply")
			}
			annotations, ok := f.chart.lastValues["podAnnotations"].(map[string]any)
			if !ok || annotations[api.AnnotationRestartRequestID] != agent.Spec.RestartRequestID {
				t.Fatal("recovered apply did not contain the requested restart marker")
			}
			current := f.agent(t)
			if current.Status.ObservedOperatorHash == oldHash || current.Status.ObservedRestartRequestID != "" ||
				current.Status.Phase != api.PhaseProvisioning || current.Status.AIGateway.KeyID != keyID {
				t.Fatal("recovery lost pending observation or changed the Gateway identity")
			}
			for range 2 {
				if _, err := f.r.Reconcile(ctx, f.req); err != nil {
					t.Fatal(err)
				}
			}
			if f.chart.installs != installs+1 {
				t.Fatal("waiting for the replacement Pod repeated the same restart apply")
			}
		})
	}
}

func TestRetainedRestartHashChangesOnlyWithRequestedIntent(t *testing.T) {
	r := &AgentReconciler{DomainSuffix: "example.com"}
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		agent, _, _ := restartObservationFixture()
		agent.Spec.Agent.Type = kind
		agent.Spec.RestartRequestID = ""
		legacy := r.configHash(agent, nil, "synthetic-hash", "")
		agent.Spec.RestartRequestID = "request-00000001"
		requested := r.configHash(agent, nil, "synthetic-hash", "")
		if requested == legacy {
			t.Fatal("restart intent did not invalidate the old Operator cache")
		}
		agent.Status.ObservedRestartRequestID = agent.Spec.RestartRequestID
		if got := r.configHash(agent, nil, "synthetic-hash", ""); got != requested {
			t.Fatal("completion observation triggered another restart apply")
		}
		agent.Spec.RestartRequestID = "request-00000002"
		if got := r.configHash(agent, nil, "synthetic-hash", ""); got == requested {
			t.Fatal("a different restart request reused the applied hash")
		}
	}
}
