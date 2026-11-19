// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDaemonSetLimitsDefaultOnlyMissingRequests(t *testing.T) {
	for _, test := range []struct {
		name     string
		requests corev1.ResourceList
	}{
		{"omitted", nil},
		{"empty", corev1.ResourceList{}},
		{"partial", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
		{"explicit zero", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")}},
		{"explicit memory", corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")}},
	} {
		for _, operation := range []string{"create", "direct", "binding", "update", "ephemeralcontainers", "resize"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				f := resourceDaemonFixture(t, test.requests)
				request := daemonAdmissionRequest(t, f, operation)
				before := f.platformPod.DeepCopy()
				daemonBefore := f.daemon.DeepCopy()
				expectAllowed(t, f.handler.Validate(context.Background(), request))
				current := &corev1.Pod{}
				if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.platformPod), current); err != nil {
					t.Fatal(err)
				}
				if current.ResourceVersion != before.ResourceVersion || !reflect.DeepEqual(current.Spec, before.Spec) {
					t.Fatal("resource normalization modified the persisted Pod")
				}
				currentDaemon := &appsv1.DaemonSet{}
				if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.daemon), currentDaemon); err != nil {
					t.Fatal(err)
				}
				digest, err := CanonicalDaemonSetDigest(currentDaemon)
				if err != nil || digest != f.registry.PlatformDaemonSets[0].TemplateDigest {
					t.Fatal("Pod-only defaults changed the registered DaemonSet digest")
				}
				if currentDaemon.ResourceVersion != daemonBefore.ResourceVersion ||
					!reflect.DeepEqual(currentDaemon.Spec, daemonBefore.Spec) {
					t.Fatal("Pod-only defaults changed the persisted template")
				}
			})
		}
	}
}

func TestDaemonSetDefaultedResourcesStillRequireExactValues(t *testing.T) {
	for _, group := range []string{"container", "init", "restartable init"} {
		for name, mutate := range map[string]func(*corev1.ResourceRequirements){
			"wrong defaulted request": func(r *corev1.ResourceRequirements) {
				r.Requests[corev1.ResourceMemory] = resource.MustParse("32Mi")
			},
			"overwritten explicit request": func(r *corev1.ResourceRequirements) {
				r.Requests[corev1.ResourceCPU] = resource.MustParse("1")
			},
			"missing explicit request": func(r *corev1.ResourceRequirements) {
				delete(r.Requests, corev1.ResourceCPU)
			},
			"extra request": func(r *corev1.ResourceRequirements) {
				r.Requests["example.com/other"] = resource.MustParse("1")
			},
			"changed limit": func(r *corev1.ResourceRequirements) {
				r.Limits[corev1.ResourceMemory] = resource.MustParse("128Mi")
			},
			"wrong device request": func(r *corev1.ResourceRequirements) {
				r.Requests["example.com/device"] = resource.MustParse("2")
			},
		} {
			t.Run(group+"/"+name, func(t *testing.T) {
				f := resourceDaemonFixture(t, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")})
				target := &f.platformPod.Spec.Containers[0].Resources
				if group == "init" {
					target = &f.platformPod.Spec.InitContainers[0].Resources
				} else if group == "restartable init" {
					target = &f.platformPod.Spec.InitContainers[1].Resources
				}
				mutate(target)
				if err := f.client.Update(context.Background(), f.platformPod); err != nil {
					t.Fatal(err)
				}
				for _, operation := range []string{"create", "binding", "update"} {
					expectDenied(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, operation)), "DaemonSetTemplateMismatch")
				}
			})
		}
	}
}

func resourceDaemonFixture(t *testing.T, requests corev1.ResourceList) *placementFixture {
	t.Helper()
	f := defaultedDaemonFixture(t, false)
	limits := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("64Mi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("128Mi"), "example.com/device": resource.MustParse("1"),
	}
	f.daemon.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits: limits.DeepCopy(), Requests: requests.DeepCopy(),
	}
	init := *f.daemon.Spec.Template.Spec.Containers[0].DeepCopy()
	init.Name = "init-platform"
	sidecar := *init.DeepCopy()
	sidecar.Name, sidecar.RestartPolicy = "platform-sidecar", ptr.To(corev1.ContainerRestartPolicyAlways)
	f.daemon.Spec.Template.Spec.InitContainers = []corev1.Container{init, sidecar}
	if err := f.client.Update(context.Background(), f.daemon); err != nil {
		t.Fatal(err)
	}
	digest, err := CanonicalDaemonSetDigest(f.daemon)
	if err != nil {
		t.Fatal(err)
	}
	f.registry.PlatformDaemonSets[0].TemplateDigest = digest
	f.saveRegistry(t)
	f.platformPod.Spec.Containers = []corev1.Container{*f.daemon.Spec.Template.Spec.Containers[0].DeepCopy()}
	f.platformPod.Spec.InitContainers = f.daemon.Spec.Template.Spec.DeepCopy().InitContainers
	for _, containers := range [][]corev1.Container{f.platformPod.Spec.Containers, f.platformPod.Spec.InitContainers} {
		for i := range containers {
			containers[i].Resources.Requests = corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("64Mi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("128Mi"), "example.com/device": resource.MustParse("1"),
			}
			for key, value := range requests {
				containers[i].Resources.Requests[key] = value.DeepCopy()
			}
		}
	}
	if err := f.client.Update(context.Background(), f.platformPod); err != nil {
		t.Fatal(err)
	}
	return f
}
