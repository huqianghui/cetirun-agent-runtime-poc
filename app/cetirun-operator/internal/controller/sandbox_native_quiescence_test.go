// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNativeSandboxQuiescenceRequiresExactOwnedRWOP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.Agent, *SandboxPrerequisites, *corev1.Namespace, *corev1.PersistentVolumeClaim, *appsv1.StatefulSet)
	}{
		{"Agent lifetime", func(_ *api.Agent, p *SandboxPrerequisites, _ *corev1.Namespace, _ *corev1.PersistentVolumeClaim, _ *appsv1.StatefulSet) {
			p.AgentUID = "other-agent-uid"
		}},
		{"namespace lifetime", func(_ *api.Agent, p *SandboxPrerequisites, _ *corev1.Namespace, _ *corev1.PersistentVolumeClaim, _ *appsv1.StatefulSet) {
			p.NamespaceUID = "other-namespace-uid"
		}},
		{"namespace ownership", func(_ *api.Agent, _ *SandboxPrerequisites, ns *corev1.Namespace, _ *corev1.PersistentVolumeClaim, _ *appsv1.StatefulSet) {
			ns.OwnerReferences = []metav1.OwnerReference{w.AgentOwner("foreign", "foreign-agent-uid")}
		}},
		{"HOME lifetime", func(_ *api.Agent, p *SandboxPrerequisites, _ *corev1.Namespace, _ *corev1.PersistentVolumeClaim, _ *appsv1.StatefulSet) {
			p.Home.UID = "other-home-uid"
		}},
		{"HOME ownership", func(_ *api.Agent, _ *SandboxPrerequisites, _ *corev1.Namespace, home *corev1.PersistentVolumeClaim, _ *appsv1.StatefulSet) {
			home.Annotations[w.AgentUIDAnnotation] = "foreign-agent-uid"
		}},
		{"RWO is not RWOP", func(_ *api.Agent, _ *SandboxPrerequisites, _ *corev1.Namespace, home *corev1.PersistentVolumeClaim, _ *appsv1.StatefulSet) {
			home.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		}},
		{"terminating HOME before deletion", func(_ *api.Agent, _ *SandboxPrerequisites, _ *corev1.Namespace, home *corev1.PersistentVolumeClaim, _ *appsv1.StatefulSet) {
			now := metav1.Now()
			home.DeletionTimestamp, home.Finalizers = &now, []string{"example.com/protect"}
		}},
		{"running workload", func(_ *api.Agent, _ *SandboxPrerequisites, _ *corev1.Namespace, _ *corev1.PersistentVolumeClaim, workload *appsv1.StatefulSet) {
			workload.Spec.Replicas = ptr.To[int32](1)
		}},
		{"unknown replicas", func(_ *api.Agent, _ *SandboxPrerequisites, _ *corev1.Namespace, _ *corev1.PersistentVolumeClaim, workload *appsv1.StatefulSet) {
			workload.Spec.Replicas = nil
		}},
		{"foreign workload", func(_ *api.Agent, _ *SandboxPrerequisites, _ *corev1.Namespace, _ *corev1.PersistentVolumeClaim, workload *appsv1.StatefulSet) {
			workload.OwnerReferences = []metav1.OwnerReference{w.AgentOwner("foreign", "foreign-agent-uid")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, inputs, namespace, home, workload := nativeQuiescenceObjects()
			baseline := &KubernetesNativeQuiescence{APIReader: fake.NewClientBuilder().WithObjects(namespace.DeepCopy(), home.DeepCopy(), workload.DeepCopy()).Build()}
			if err := baseline.CheckQuiesced(context.Background(), agent, inputs); err != nil {
				t.Fatal("owned stopped RWOP baseline rejected:", err)
			}
			tc.mutate(agent, &inputs, namespace, home, workload)
			source := &KubernetesNativeQuiescence{APIReader: fake.NewClientBuilder().WithObjects(namespace, home, workload).Build()}
			if err := source.CheckQuiesced(context.Background(), agent, inputs); err == nil {
				t.Fatal("unconfirmed native quiescence was accepted")
			}
		})
	}
}

func TestNativeSandboxQuiescenceWaitsForEveryPod(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed} {
		for _, terminating := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/terminating=%t", phase, terminating), func(t *testing.T) {
				agent, inputs, namespace, home, workload := nativeQuiescenceObjects()
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled-pod", Namespace: namespace.Name, UID: "pod-uid"}, Status: corev1.PodStatus{Phase: phase}}
				if terminating {
					now := metav1.Now()
					pod.DeletionTimestamp, pod.Finalizers = &now, []string{"example.com/protect"}
				}
				kube := fake.NewClientBuilder().WithObjects(namespace, home, workload, pod).Build()
				source := &KubernetesNativeQuiescence{APIReader: kube}
				if err := source.CheckQuiesced(context.Background(), agent, inputs); err == nil || !strings.Contains(err.Error(), "ordinary Pod termination") {
					t.Fatal("remaining Pod did not block ordinary cleanup:", err)
				}
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
					t.Fatal("quiescence removed a Pod:", err)
				}
			})
		}
	}
}

func TestNativeSandboxQuiescenceRequiresObservedDeletionAndAvailableReads(t *testing.T) {
	agent, inputs, namespace, home, workload := nativeQuiescenceObjects()
	kube := fake.NewClientBuilder().WithObjects(namespace, home, workload).Build()
	for _, source := range []*KubernetesNativeQuiescence{nil, {}, {APIReader: kube}} {
		requested := agent
		if source != nil && source.APIReader != nil {
			requested = nil
		}
		if err := source.CheckQuiesced(context.Background(), requested, inputs); !errors.Is(err, sandbox.ErrObservationUnavailable) {
			t.Fatal("missing native quiescence identity or reader was accepted:", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&KubernetesNativeQuiescence{APIReader: kube}).CheckQuiesced(ctx, agent, inputs); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was ignored:", err)
	}
	readErr := errors.New("synthetic API read outage")
	source := &KubernetesNativeQuiescence{APIReader: interceptor.NewClient(kube, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return readErr },
	})}
	if err := source.CheckQuiesced(context.Background(), agent, inputs); !errors.Is(err, readErr) {
		t.Fatal("Pod-list outage was mistaken for absence:", err)
	}
	source.APIReader = fake.NewClientBuilder().Build()
	if err := source.CheckQuiesced(context.Background(), agent, inputs); !apierrors.IsNotFound(err) {
		t.Fatal("namespace absence authorized a non-deleting lifetime:", err)
	}
	now := metav1.Now()
	agent.DeletionTimestamp = &now
	if err := source.CheckQuiesced(context.Background(), agent, inputs); err != nil {
		t.Fatal("deleted namespace did not finish ordinary cleanup:", err)
	}
	source.APIReader = fake.NewClientBuilder().WithObjects(namespace, workload).Build()
	if err := source.CheckQuiesced(context.Background(), agent, inputs); !apierrors.IsNotFound(err) {
		t.Fatal("HOME disappeared before namespace deletion:", err)
	}
	namespace.DeletionTimestamp, namespace.Finalizers = &now, []string{"example.com/protect"}
	source.APIReader = fake.NewClientBuilder().WithObjects(namespace, workload).Build()
	if err := source.CheckQuiesced(context.Background(), agent, inputs); err != nil {
		t.Fatal("normal namespace deletion could not observe an already removed HOME:", err)
	}
}

func TestNativeSandboxCleanupWaitsWithoutForceDeletingPods(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	f.ready(t)
	ctx := context.Background()
	agent, pod := f.current(t), f.pod(t)
	if err := f.kube.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	var podDeletes int
	f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
			if _, ok := object.(*corev1.Pod); ok {
				podDeletes++
			}
			return c.Delete(ctx, object, opts...)
		},
		DeleteAllOf: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteAllOfOption) error {
			if _, ok := object.(*corev1.Pod); ok {
				podDeletes++
			}
			return c.DeleteAllOf(ctx, object, opts...)
		},
	})
	if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "ordinary Pod termination") {
		t.Fatal("deletion did not wait for ordinary Pod termination:", err)
	}
	if len(f.gateway.deletedTokens) == 0 {
		t.Fatal("Gateway revocation waited for Pod termination")
	}
	f.assertNotReady(t)
	if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || f.pod(t).UID != pod.UID || podDeletes != 0 {
		t.Fatal("cleanup bypassed scale-down and ordinary Pod termination")
	}
	if !slices.Contains(f.current(t).Finalizers, sandbox.PreparationFinalizer) {
		t.Fatal("cleanup released its durable ownership receipt while a Pod remained")
	}
	namespace := &corev1.Namespace{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: pod.Namespace}, namespace); err != nil || !namespace.DeletionTimestamp.IsZero() {
		t.Fatal("namespace deletion preceded Pod termination:", err)
	}
	// Simulate completion by the StatefulSet controller, not an Operator force delete.
	if err := f.kube.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.finishNamespaceGC(t)
	f.reconcileOK(t)
	if podDeletes != 0 {
		t.Fatal("Operator issued a Pod deletion during normal RWOP cleanup")
	}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatal("ordinary cleanup did not release the deleting Agent:", err)
	}
	if len(f.gateway.deletedTokens) == 0 {
		t.Fatal("deletion did not revoke the fake Gateway key")
	}
}

func nativeQuiescenceObjects() (*api.Agent, SandboxPrerequisites, *corev1.Namespace, *corev1.PersistentVolumeClaim, *appsv1.StatefulSet) {
	fixture := testfixture.New("openclaw")
	home := fixture.Input.Home
	home.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: instanceServiceName(fixture.Agent.Name), Namespace: fixture.Namespace.Name, UID: "workload-uid",
		OwnerReferences: []metav1.OwnerReference{w.AgentOwner(fixture.Agent.Name, fixture.Agent.UID)},
	}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](0)}}
	inputs := SandboxPrerequisites{
		AgentUID: fixture.Agent.UID, AgentGeneration: fixture.Agent.Generation, NamespaceUID: fixture.Namespace.UID,
		Home: w.ObjectIdentity{Name: home.Name, UID: home.UID},
	}
	return fixture.Agent, inputs, fixture.Namespace, home, workload
}
