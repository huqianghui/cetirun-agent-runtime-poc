// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestDaemonSetExplicitPriorityClassDefaults(t *testing.T) {
	for _, test := range []struct {
		name     string
		priority int32
		policy   *corev1.PreemptionPolicy
	}{
		{"nonzero", 1000, ptr.To(corev1.PreemptLowerPriority)},
		{"nonpreempting", 1000, ptr.To(corev1.PreemptNever)},
		{"default policy", 1000, nil},
		{"negative priority", -1000, ptr.To(corev1.PreemptLowerPriority)},
		{"zero priority", 0, ptr.To(corev1.PreemptLowerPriority)},
	} {
		for _, operation := range []string{"create", "direct", "binding", "update", "ephemeralcontainers", "resize"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				f, _ := priorityDaemonFixture(t, test.priority, test.policy)
				request := daemonAdmissionRequest(t, f, operation)
				before := f.platformPod.DeepCopy()
				expectAllowed(t, f.handler.Validate(context.Background(), request))
				current := &corev1.Pod{}
				if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.platformPod), current); err != nil {
					t.Fatal(err)
				}
				if current.ResourceVersion != before.ResourceVersion || !reflect.DeepEqual(current.Spec, before.Spec) {
					t.Fatal("priority normalization wrote to the platform Pod")
				}
				digest, err := CanonicalDaemonSetDigest(f.daemon)
				if err != nil || digest != f.registry.PlatformDaemonSets[0].TemplateDigest ||
					f.daemon.Spec.Template.Spec.Priority != nil || f.daemon.Spec.Template.Spec.PreemptionPolicy != nil {
					t.Fatal("Pod-only priority defaults changed the registered template")
				}
			})
		}
	}
}

func TestDaemonSetPriorityClassRejectsConflictingPodFields(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.PodSpec){
		"wrong class":      func(s *corev1.PodSpec) { s.PriorityClassName = "other-priority" },
		"missing class":    func(s *corev1.PodSpec) { s.PriorityClassName = "" },
		"wrong priority":   func(s *corev1.PodSpec) { s.Priority = ptr.To(int32(2000)) },
		"zero priority":    func(s *corev1.PodSpec) { s.Priority = ptr.To(int32(0)) },
		"missing priority": func(s *corev1.PodSpec) { s.Priority = nil },
		"wrong policy":     func(s *corev1.PodSpec) { s.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority) },
		"missing policy":   func(s *corev1.PodSpec) { s.PreemptionPolicy = nil },
		"unsupported policy": func(s *corev1.PodSpec) {
			s.PreemptionPolicy = ptr.To(corev1.PreemptionPolicy("synthetic-private-policy"))
		},
	} {
		for _, operation := range []string{"create", "binding", "update"} {
			t.Run(name+"/"+operation, func(t *testing.T) {
				f, _ := priorityDaemonFixture(t, 1000, ptr.To(corev1.PreemptNever))
				mutate(&f.platformPod.Spec)
				if err := f.client.Update(context.Background(), f.platformPod); err != nil {
					t.Fatal(err)
				}
				expectDenied(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, operation)), "DaemonSetPriorityMismatch")
			})
		}
	}
}

func TestDaemonSetPriorityClassPreservesExplicitTemplateFields(t *testing.T) {
	for _, conflict := range []string{"none", "priority", "policy", "both Pod and template"} {
		t.Run(conflict, func(t *testing.T) {
			f, class := priorityDaemonFixture(t, 1000, ptr.To(corev1.PreemptNever))
			f.daemon.Spec.Template.Spec.Priority = ptr.To(class.Value)
			f.daemon.Spec.Template.Spec.PreemptionPolicy = ptr.To(corev1.PreemptNever)
			switch conflict {
			case "priority":
				f.daemon.Spec.Template.Spec.Priority = ptr.To(int32(2000))
			case "policy":
				f.daemon.Spec.Template.Spec.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
			case "both Pod and template":
				f.daemon.Spec.Template.Spec.Priority = ptr.To(int32(2000))
				f.platformPod.Spec.Priority = ptr.To(int32(2000))
			}
			if err := f.client.Update(context.Background(), f.daemon); err != nil {
				t.Fatal(err)
			}
			digest, err := CanonicalDaemonSetDigest(f.daemon)
			if err != nil {
				t.Fatal(err)
			}
			f.registry.PlatformDaemonSets[0].TemplateDigest = digest
			f.saveRegistry(t)
			response := f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, "create"))
			if conflict == "none" {
				expectAllowed(t, response)
			} else {
				expectDenied(t, response, "DaemonSetPriorityMismatch")
			}
		})
	}
}

func TestDaemonSetPriorityClassRequiresLivePersistedAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*schedulingv1.PriorityClass){
		"missing UID":      func(c *schedulingv1.PriorityClass) { c.UID = "" },
		"missing RV":       func(c *schedulingv1.PriorityClass) { c.ResourceVersion = "" },
		"wrong name":       func(c *schedulingv1.PriorityClass) { c.Name = "other-class" },
		"wrong namespace":  func(c *schedulingv1.PriorityClass) { c.Namespace = "tenant" },
		"terminating":      func(c *schedulingv1.PriorityClass) { c.DeletionTimestamp = ptr.To(metav1.Unix(1, 0)) },
		"invalid policy":   func(c *schedulingv1.PriorityClass) { c.PreemptionPolicy = ptr.To(corev1.PreemptionPolicy("unknown")) },
		"changed priority": func(c *schedulingv1.PriorityClass) { c.Value++ },
		"changed policy":   func(c *schedulingv1.PriorityClass) { c.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority) },
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := priorityDaemonFixture(t, 1000, ptr.To(corev1.PreemptNever))
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if class, ok := object.(*schedulingv1.PriorityClass); ok {
					mutate(class)
				}
			}}
			expectDenied(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, "create")), "")
		})
	}
	f, class := priorityDaemonFixture(t, 1000, ptr.To(corev1.PreemptNever))
	if err := f.client.Delete(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	expectDenied(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, "create")), "PriorityClassMissing")
}

func TestDaemonSetPriorityClassSnapshotsAreRechecked(t *testing.T) {
	for _, operation := range []string{"create", "binding", "update"} {
		for _, drift := range []string{"UID", "RV", "value", "policy", "metadata without RV", "termination", "missing"} {
			t.Run(operation+"/"+drift, func(t *testing.T) {
				f, _ := priorityDaemonFixture(t, 1000, ptr.To(corev1.PreemptNever))
				initialReads := 1
				if operation != "create" {
					initialReads = 2
				}
				var reads int
				f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
					class, ok := object.(*schedulingv1.PriorityClass)
					if !ok {
						return
					}
					reads++
					if drift == "missing" && reads == initialReads {
						if err := f.client.Delete(context.Background(), class); err != nil {
							t.Fatal(err)
						}
					}
					if reads <= initialReads {
						return
					}
					switch drift {
					case "UID":
						class.UID = "replacement-class-uid"
					case "RV":
						class.ResourceVersion = "changed-version"
					case "value":
						class.Value++
					case "policy":
						class.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
					case "metadata without RV":
						class.Annotations = map[string]string{"example.com/drift": "changed"}
					case "termination":
						class.DeletionTimestamp = ptr.To(metav1.Unix(1, 0))
					}
				}}
				reason := "PriorityClassChanged"
				if drift == "missing" {
					reason = "PriorityClassMissing"
				}
				expectDenied(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, operation)), reason)
			})
		}
	}
}

func TestDaemonSetPriorityClassAbsentRetainsDefaultPath(t *testing.T) {
	f := defaultedDaemonFixture(t, false)
	class := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: "global-priority", UID: "global-priority-uid"},
		Value:      1000, PreemptionPolicy: ptr.To(corev1.PreemptNever), GlobalDefault: true,
	}
	if err := f.client.Create(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	var classReads int
	f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
		if _, ok := object.(*schedulingv1.PriorityClass); ok {
			classReads++
		}
	}}
	expectAllowed(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, "create")))
	f.platformPod.Spec.PriorityClassName, f.platformPod.Spec.Priority, f.platformPod.Spec.PreemptionPolicy =
		class.Name, ptr.To(class.Value), class.PreemptionPolicy
	expectDenied(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, "create")), "DaemonSetTemplateMismatch")
	if classReads != 0 {
		t.Fatal("a Pod-selected or implicit global default class became authority")
	}
}

func TestDaemonSetPriorityClassReadOnlyConcurrentValidation(t *testing.T) {
	f, _ := priorityDaemonFixture(t, 1000, ptr.To(corev1.PreemptNever))
	reader := &countingReader{Reader: f.client}
	var classReads atomic.Int64
	f.handler.reader = mutateReader{Reader: reader, mutate: func(object client.Object) {
		if _, ok := object.(*schedulingv1.PriorityClass); ok {
			classReads.Add(1)
		}
	}}
	request := daemonAdmissionRequest(t, f, "create")
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			response := f.handler.Validate(context.Background(), request)
			if !response.Allowed || len(response.Patches) != 0 || len(response.Patch) != 0 {
				t.Errorf("read-only concurrent class validation failed: %+v", response.Result)
			}
		})
	}
	workers.Wait()
	if classReads.Load() != 16 || reader.lists.Load() != 0 {
		t.Fatal("each request must GET and recheck its explicit class without listing")
	}
}

func priorityDaemonFixture(t *testing.T, value int32, policy *corev1.PreemptionPolicy) (*placementFixture, *schedulingv1.PriorityClass) {
	t.Helper()
	f := defaultedDaemonFixture(t, false)
	class := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-priority", UID: "priority-class-uid"},
		Value:      value, PreemptionPolicy: policy,
	}
	if err := f.client.Create(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	f.daemon.Spec.Template.Spec.PriorityClassName = class.Name
	if err := f.client.Update(context.Background(), f.daemon); err != nil {
		t.Fatal(err)
	}
	digest, err := CanonicalDaemonSetDigest(f.daemon)
	if err != nil {
		t.Fatal(err)
	}
	f.registry.PlatformDaemonSets[0].TemplateDigest = digest
	f.saveRegistry(t)
	f.platformPod.Spec.PriorityClassName = class.Name
	f.platformPod.Spec.Priority = ptr.To(class.Value)
	f.platformPod.Spec.PreemptionPolicy = ptr.To(ptr.Deref(policy, corev1.PreemptLowerPriority))
	if err := f.client.Update(context.Background(), f.platformPod); err != nil {
		t.Fatal(err)
	}
	return f, class
}

func daemonAdmissionRequest(t *testing.T, f *placementFixture, operation string) admission.Request {
	t.Helper()
	switch operation {
	case "create", "direct":
		pod := f.platformPod.DeepCopy()
		if operation == "direct" {
			pod.Spec.NodeName = f.reserved.Name
		}
		request := podRequest(t, pod, nil)
		request.UserInfo.Username = f.registry.PlatformDaemonSets[0].TrustedCreator
		return request
	case "binding":
		return bindingRequest(t, f.platformPod, f.reserved.Name)
	default:
		f.platformPod.Spec.NodeName = f.reserved.Name
		if err := f.client.Update(context.Background(), f.platformPod); err != nil {
			t.Fatal(err)
		}
		request := podRequest(t, f.platformPod, f.platformPod)
		if operation != "update" {
			request.SubResource = operation
		}
		return request
	}
}
