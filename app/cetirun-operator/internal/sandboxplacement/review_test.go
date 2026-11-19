// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestLegacyBindingsAreExplicitlyForbidden(t *testing.T) {
	f := newFixture(t)
	reader := &countingReader{Reader: f.client}
	f.handler.reader = reader
	for _, namespace := range []string{f.platformPod.Namespace, f.agentPod.Namespace, "ordinary-workloads"} {
		pod := ordinaryPod()
		pod.Namespace = namespace
		request := bindingRequest(t, pod, f.reserved.Name)
		request.Resource.Resource, request.SubResource = "bindings", ""
		request.RequestKind, request.RequestResource = &request.Kind, &request.Resource
		request.UserInfo.Username = "system:kube-controller-manager"
		request.UserInfo.Groups = []string{"system:masters"}
		response := f.handler.Validate(context.Background(), request)
		expectDenied(t, response, "LegacyBindings")
		if response.Result.Code != http.StatusForbidden {
			t.Fatal("legacy endpoint did not return a stable forbidden response")
		}
	}
	if reader.gets.Load() != 0 || reader.lists.Load() != 0 {
		t.Fatal("unsupported legacy binding depended on placement authority reads")
	}
}

func TestBindingAnnotationsAreForbiddenForAllWorkloads(t *testing.T) {
	for _, workload := range []string{"registered DaemonSet", "assigned Agent", "ordinary Pod"} {
		t.Run(workload, func(t *testing.T) {
			f := unheldBindingFixture(t)
			pod, node := f.platformPod, f.reserved.Name
			switch workload {
			case "assigned Agent":
				pod = f.agentPod
			case "ordinary Pod":
				pod, node = ordinaryPod(), f.ordinary.Name
				pod.ResourceVersion = ""
				if err := f.client.Create(context.Background(), pod); err != nil {
					t.Fatal(err)
				}
			}
			expectAllowed(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, node)))
			for _, annotations := range []map[string]string{
				{"example.com/new": "true"},
				{sandboxworkload.AgentUIDAnnotation: "forged-agent-uid"},
				{sandboxworkload.AgentUIDAnnotation: string(f.agent.Agent.UID)},
				{"example.com/empty": ""},
			} {
				request := bindingRequest(t, pod, node)
				var binding corev1.Binding
				if err := json.Unmarshal(request.Object.Raw, &binding); err != nil {
					t.Fatal(err)
				}
				binding.Annotations = annotations
				binding.ResourceVersion = pod.ResourceVersion
				request.Object.Raw = marshal(t, binding)
				response := f.handler.Validate(context.Background(), request)
				expectDenied(t, response, "BindingAnnotationsForbidden")
				if response.Result.Code != http.StatusForbidden {
					t.Fatal("Binding annotations did not receive an explicit forbidden response")
				}
			}
		})
	}
}

func TestTerminatingPodAllowsOnlyFinalizerRemoval(t *testing.T) {
	for name, finalizers := range map[string][]string{
		"remove all":    nil,
		"remove middle": {"example.com/first", "example.com/last"},
		"retain last":   {"example.com/last"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			current := terminatingSubject(t, f)
			pod := current.DeepCopy()
			pod.Finalizers = finalizers
			pod.ManagedFields = []metav1.ManagedFieldsEntry{{
				Manager: "example-cleanup", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "v1",
			}}
			reader := &countingReader{Reader: f.client}
			f.handler.reader = reader
			response := f.handler.Validate(context.Background(), podRequest(t, pod, current))
			expectAllowed(t, response)
			if !strings.Contains(response.Result.Message, "finalizer-removal cleanup") || reader.gets.Load() != 4 || reader.lists.Load() != 0 {
				t.Fatal("non-placement cleanup did not use only subject and registry snapshots")
			}
			persisted := &corev1.Pod{}
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(current), persisted); err != nil {
				t.Fatal(err)
			}
			if len(persisted.Finalizers) != 3 || persisted.ResourceVersion != current.ResourceVersion {
				t.Fatal("read-only cleanup admission changed its subject")
			}
		})
	}
}

func TestTerminatingPodCleanupRejectsOtherChanges(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"no removal": func(p *corev1.Pod) {
			p.Finalizers = []string{"example.com/first", "example.com/middle", "example.com/last"}
		},
		"add finalizer": func(p *corev1.Pod) {
			p.Finalizers = []string{"example.com/first", "example.com/new"}
		},
		"reorder finalizers": func(p *corev1.Pod) {
			p.Finalizers = []string{"example.com/last", "example.com/first"}
		},
		"duplicate finalizer": func(p *corev1.Pod) {
			p.Finalizers = []string{"example.com/first", "example.com/first"}
		},
		"change placement": func(p *corev1.Pod) { p.Spec.NodeName = "reserved-worker" },
		"remove placement": func(p *corev1.Pod) { p.Spec.NodeName = "" },
		"change spec":      func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.example.com/changed:1.0" },
		"change labels":    func(p *corev1.Pod) { p.Labels["example.com/job"] = "different" },
		"change annotations": func(p *corev1.Pod) {
			p.Annotations["example.com/unchanged"] = "different"
		},
		"change owner":       func(p *corev1.Pod) { p.OwnerReferences[0].UID = "another-job-uid" },
		"remove owner":       func(p *corev1.Pod) { p.OwnerReferences = nil },
		"change status":      func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
		"change UID":         func(p *corev1.Pod) { p.UID = "another-pod-uid" },
		"change RV":          func(p *corev1.Pod) { p.ResourceVersion = "another-version" },
		"remove termination": func(p *corev1.Pod) { p.DeletionTimestamp = nil },
		"change termination": func(p *corev1.Pod) {
			p.DeletionTimestamp = ptr.To(metav1.NewTime(p.DeletionTimestamp.Add(time.Second)))
		},
		"change grace period": func(p *corev1.Pod) { p.DeletionGracePeriodSeconds = ptr.To(int64(42)) },
		"change creation":     func(p *corev1.Pod) { p.CreationTimestamp = metav1.Now() },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			current := terminatingSubject(t, f)
			pod := current.DeepCopy()
			pod.Finalizers = []string{"example.com/first"}
			mutate(pod)
			request := podRequest(t, pod, current)
			request.UserInfo.Username = "system:kube-controller-manager"
			request.UserInfo.Groups = []string{"system:masters"}
			expectDenied(t, f.handler.Validate(context.Background(), request), "")
		})
	}
	f := newFixture(t)
	current := terminatingSubject(t, f)
	pod := current.DeepCopy()
	pod.Finalizers = nil
	for _, subresource := range []string{"ephemeralcontainers", "resize"} {
		request := podRequest(t, pod, current)
		request.SubResource = subresource
		expectDenied(t, f.handler.Validate(context.Background(), request), "PodTerminating")
	}
	old := current.DeepCopy()
	old.Annotations["example.com/unchanged"] = "forged-previous"
	pod.Annotations = old.Annotations
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, old)), "PodTerminating")
}

func TestTerminatingPodCleanupRechecksBothSnapshots(t *testing.T) {
	for _, kind := range []string{"Pod", "Registry"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			current := terminatingSubject(t, f)
			pod := current.DeepCopy()
			pod.Finalizers = nil
			var reads int
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if stageForObject(object, f.state.Name) == kind {
					reads++
					if reads > 1 {
						object.SetResourceVersion("changed-version")
					}
				}
			}}
			expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, current)), kind+"Changed")
		})
	}
}

func TestCleanupDoesNotWaiveAuthorityOrBindingLiveness(t *testing.T) {
	f := newFixture(t)
	current := terminatingSubject(t, f)
	pod := current.DeepCopy()
	pod.Finalizers = nil
	f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
		if _, ok := object.(*corev1.ConfigMap); ok {
			object.SetDeletionTimestamp(ptr.To(metav1.Now()))
		}
	}}
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, current)), "RegistryInvalid")
	f.handler.reader = f.client
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, current, f.ordinary.Name)), "PodInvalid")
	if err := f.client.Delete(context.Background(), f.state); err != nil {
		t.Fatal(err)
	}
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, current)), "RegistryMissing")
}

func TestLivePodCannotUseTerminationCleanupExemption(t *testing.T) {
	f := newFixture(t)
	pod := ordinaryPod()
	pod.ResourceVersion, pod.Spec.NodeName = "", f.reserved.Name
	pod.Finalizers = []string{"example.com/first"}
	if err := f.client.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	updated := pod.DeepCopy()
	updated.Finalizers = nil
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, updated, pod)), "ReservedNode")
}

func terminatingSubject(t *testing.T, f *placementFixture) *corev1.Pod {
	t.Helper()
	pod := ordinaryPod()
	pod.ResourceVersion, pod.Spec.NodeName = "", f.ordinary.Name
	pod.Labels = map[string]string{"example.com/job": "example-job"}
	pod.Annotations = map[string]string{"example.com/unchanged": "true"}
	pod.Finalizers = []string{"example.com/first", "example.com/middle", "example.com/last"}
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: "example-job", UID: "job-uid",
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}}
	pod.Status.Phase = corev1.PodRunning
	if err := f.client.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	if pod.DeletionTimestamp.IsZero() || len(pod.Finalizers) != 3 {
		t.Fatal("fixture must remain persisted and terminating behind its finalizers")
	}
	return pod
}
