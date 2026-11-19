// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	admissionhandler "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func reconciledPodForTest(t *testing.T, f *nativeSandboxFixture, workload w.RenderedWorkload) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	namespace := instanceNamespace(f.agent.Name)
	pod := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: *workload.Template.ObjectMeta.DeepCopy(), Spec: *workload.Template.Spec.DeepCopy(),
	}
	pod.Namespace = namespace
	pod.Spec.Overhead = workload.Overhead.DeepCopy()
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
		Key: "node.kubernetes.io/memory-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	})
	const revision = "test-revision"
	if workload.Kind == "StatefulSet" {
		stateful := &appsv1.StatefulSet{}
		if err := f.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: workload.Identity.Name}, stateful); err != nil {
			t.Fatal(err)
		}
		stateful.Status.UpdateRevision = revision
		if err := f.kube.Status().Update(ctx, stateful); err != nil {
			t.Fatal(err)
		}
		pod.Name = stateful.Name + "-0"
		pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(stateful, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
		pod.Labels["statefulset.kubernetes.io/pod-name"] = pod.Name
		pod.Labels["apps.kubernetes.io/pod-index"] = "0"
		pod.Labels["controller-revision-hash"] = revision
		pod.Spec.Hostname, pod.Spec.Subdomain = pod.Name, stateful.Name
	} else {
		deployment := &appsv1.Deployment{}
		if err := f.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: workload.Identity.Name}, deployment); err != nil {
			t.Fatal(err)
		}
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: deployment.Name + "-" + revision, Namespace: namespace,
				Labels:          maps.Clone(deployment.Spec.Template.Labels),
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
			},
			Spec: appsv1.ReplicaSetSpec{
				Replicas: ptr.To[int32](1), Selector: deployment.Spec.Selector.DeepCopy(),
				Template: *deployment.Spec.Template.DeepCopy(),
			},
		}
		rs.Labels["pod-template-hash"] = revision
		rs.Spec.Template.Labels["pod-template-hash"] = revision
		if err := f.kube.Create(ctx, rs); err != nil {
			t.Fatal(err)
		}
		pod.Name = rs.Name + "-example"
		pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}
		pod.Labels["pod-template-hash"] = revision
	}
	return pod
}

func reconciledPodRequest(t *testing.T, pod *corev1.Pod) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "synthetic-admission", Operation: admissionv1.Create, Namespace: pod.Namespace, Name: pod.Name,
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Kind:     metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
		Object:   runtime.RawExtension{Raw: raw},
	}}
}

func TestReconciledSandboxOptionalCoordinationCoversCurrentPods(t *testing.T) {
	ctx := context.Background()
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, true,
		func(r *AgentReconciler, kube client.Client) {
			authority, err := sandboxcoordination.NewState(r.OperatorNamespace, "reconciled-coordination")
			if err != nil {
				t.Fatal(err)
			}
			if err := kube.Create(ctx, authority); err != nil {
				t.Fatal(err)
			}
			r.SandboxNativeCoordination = &sandboxcoordination.Store{
				Client: kube, APIReader: kube, Namespace: authority.Namespace,
				Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID},
			}
		})
	store := f.r.SandboxNativeCoordination
	ticket, found, err := store.ReadNative(ctx, f.agent.UID)
	if err != nil || !found || !ticket.Reservation.Intent.Reconciled || ticket.Reservation.Intent.Workload.UID != "" {
		t.Fatal("common execution lost its source reservation or invented a physical workload identity:", err)
	}
	expected := reconciledExpectationForTest(t, f)
	handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{
		StateNamespace: f.r.OperatorNamespace, CoordinationConfigMapName: store.Identity.Name,
		CoordinationConfigMapUID: string(store.Identity.UID),
	})
	if err != nil {
		t.Fatal(err)
	}
	var pods []*corev1.Pod
	for _, workload := range expected.Workloads {
		pod := reconciledPodForTest(t, f, workload)
		pods = append(pods, pod)
		if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); !response.Allowed {
			t.Fatal("reserved current Pod denied:", response.Result)
		}
	}
	intent := ticket.Reservation.Intent
	before := &api.AgentPolicy{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: intent.Sources[2].Name}, before); err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
	if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("current common execution did not exclude a conflicting shared policy change:", err)
	}
	agent := f.current(t)
	agent.Generation++
	agent.Spec.Model = testSharedModelOne
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	current, found, err := store.ReadNative(ctx, agent.UID)
	if err != nil || !found || current.Reservation.ID != ticket.Reservation.ID ||
		current.Reservation.Intent.AgentGeneration != agent.Generation {
		t.Fatal("an ordinary model selection did not retain its source reservation:", err)
	}
	for _, workload := range reconciledExpectationForTest(t, f).Workloads {
		if workload.Kind == "StatefulSet" {
			pod := reconciledPodForTest(t, f, workload)
			if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); !response.Allowed {
				t.Fatal("updated current Pod denied before stop:", response.Result)
			}
			pods = append(pods, pod)
		}
	}
	if _, err := store.BeginNativeStop(ctx, current); err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods {
		response := handler.Validate(ctx, reconciledPodRequest(t, pod))
		if response.Allowed || !strings.Contains(response.Result.Message, "NativeReservationRequired") {
			t.Fatal("Pod admission did not enforce its stopping reservation")
		}
	}
}

func TestReconciledSandboxAdmitsCurrentMainAndAuxiliaryPods(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
			t.Run(fmt.Sprintf("%s/%s", kind, mode), func(t *testing.T) {
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true, withNativeHomeAccessMode(t, mode))
				f.reconcileOK(t)
				expected := reconciledExpectationForTest(t, f)
				handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{StateNamespace: f.r.OperatorNamespace})
				if err != nil {
					t.Fatal(err)
				}
				for _, workload := range expected.Workloads {
					pod := reconciledPodForTest(t, f, workload)
					if response := handler.Validate(context.Background(), reconciledPodRequest(t, pod)); !response.Allowed {
						t.Fatalf("%s current %s Pod was rejected: %v", kind, workload.Kind, response.Result)
					}
					for name, mutate := range map[string]func(*corev1.Pod){
						"privilege": func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true) },
						"image":     func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.example.com/unapproved:example" },
						"owner":     func(p *corev1.Pod) { p.OwnerReferences[0].UID = "another-controller" },
						"dispatch":  func(p *corev1.Pod) { delete(p.Annotations, w.ReconciledAnnotation) },
						"extra HOME writer": func(p *corev1.Pod) {
							p.Name = workload.Identity.Name + "-1"
							p.Labels["statefulset.kubernetes.io/pod-name"] = p.Name
							p.Labels["apps.kubernetes.io/pod-index"] = "1"
							p.Spec.Hostname = p.Name
						},
						"home": func(p *corev1.Pod) {
							p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "unexpected", VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "unapproved-home"},
							}})
						},
					} {
						t.Run(workload.Kind+"/"+name, func(t *testing.T) {
							changed := pod.DeepCopy()
							mutate(changed)
							if handler.Validate(context.Background(), reconciledPodRequest(t, changed)).Allowed {
								t.Fatal("current-template admission accepted an unapproved Pod change")
							}
						})
					}
					var config corev1.ConfigMap
					for _, control := range expected.Controls {
						if control.GetKind() == "ConfigMap" {
							if err := f.kube.Get(context.Background(), client.ObjectKeyFromObject(&control), &config); err != nil {
								t.Fatal(err)
							}
							break
						}
					}
					before := config.DeepCopy()
					config.Data["unapproved"] = "changed"
					if err := f.kube.Update(context.Background(), &config); err != nil {
						t.Fatal(err)
					}
					response := handler.Validate(context.Background(), reconciledPodRequest(t, pod))
					if response.Allowed || !strings.Contains(response.Result.Message, "ControlMismatch") {
						t.Fatal("an unchanged Pod was admitted against changed chart configuration")
					}
					config.Data = before.Data
					if err := f.kube.Update(context.Background(), &config); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
