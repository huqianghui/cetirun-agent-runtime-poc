// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type registryRecoveryRaceClient struct {
	client.Client
	t       *testing.T
	deleted bool
}

func (c *registryRecoveryRaceClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return c.Client.Delete(ctx, object, options...)
	}
	deletion := &client.DeleteOptions{}
	for _, option := range options {
		option.ApplyToDelete(deletion)
	}
	if deletion.Preconditions == nil || deletion.Preconditions.UID == nil || *deletion.Preconditions.UID != pod.UID ||
		deletion.Preconditions.ResourceVersion == nil || *deletion.Preconditions.ResourceVersion != pod.ResourceVersion {
		c.t.Fatal("recovery deletion lacks UID/resourceVersion preconditions")
	}
	c.deleted = true
	changed := pod.DeepCopy()
	changed.Labels["new-observation"] = "true"
	if err := c.Client.Update(ctx, changed); err != nil {
		c.t.Fatal(err)
	}
	return c.Client.Delete(ctx, pod, options...)
}

func TestRegistryRecoveryOnlyReplacesUnstartedOwnedPullFailure(t *testing.T) {
	for _, scenario := range []string{"initial", "custom", "bound-reference", "ready", "running", "previous-writer", "unobserved", "other-failure", "foreign-pod", "foreign-workload", "changed-image", "changed-init-image", "missing-container", "changed-config", "suspended", "raced-pod", "permission-change"} {
		t.Run(scenario, func(t *testing.T) {
			r, agent, _ := registryFixture(t)
			ctx := context.Background()
			name, err := r.syncRegistryCredentials(ctx, agent, "default")
			if err != nil {
				t.Fatal(err)
			}
			ns := instanceNamespace(agent.Name)
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: ns, UID: "workload-uid",
				Annotations: map[string]string{"meta.helm.sh/release-name": helmReleaseName(agent.Name), "meta.helm.sh/release-namespace": ns}},
				Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1), Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "example"}, Annotations: map[string]string{"checksum/config": "config-one"}},
					Spec: corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: name}},
						InitContainers: []corev1.Container{{Name: "init-home-data", Image: *agent.Spec.Agent.Image}},
						Containers:     []corev1.Container{{Name: "cetirun", Image: *agent.Spec.Agent.Image}}}}}}
			pod := &corev1.Pod{ObjectMeta: *sts.Spec.Template.ObjectMeta.DeepCopy(), Spec: *sts.Spec.Template.Spec.DeepCopy(),
				Status: corev1.PodStatus{Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{{Name: "init-home-data", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}},
					ContainerStatuses:     []corev1.ContainerStatus{{Name: "cetirun", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}}}
			pod.Name, pod.Namespace, pod.UID = sts.Name+"-0", ns, "pod-uid"
			pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name, UID: sts.UID, Controller: ptr.To(true)}}
			pod.Spec.ImagePullSecrets = nil
			switch scenario {
			case "custom":
				agent.Spec.Agent.Type = api.AgentTypeCustom
				sts.OwnerReferences = []metav1.OwnerReference{registryauth.Owner(agent)}
				sts.Annotations[customAgentUID] = string(agent.UID)
				if err := r.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
			case "bound-reference":
				pod.Spec.ImagePullSecrets = sts.Spec.Template.Spec.ImagePullSecrets
			case "ready":
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			case "running":
				pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			case "previous-writer":
				pod.Status.ContainerStatuses[0].RestartCount = 1
			case "unobserved":
				pod.Status.ContainerStatuses = nil
			case "other-failure":
				pod.Status.InitContainerStatuses[0].State.Waiting.Reason = "CrashLoopBackOff"
			case "foreign-pod":
				pod.OwnerReferences[0].UID = "foreign-workload"
			case "foreign-workload":
				sts.Annotations["meta.helm.sh/release-name"] = "another-release"
			case "changed-image":
				pod.Spec.Containers[0].Image = "registry.example.com/old:1"
			case "changed-init-image":
				pod.Spec.InitContainers[0].Image = "registry.example.com/old:1"
			case "missing-container":
				pod.Spec.Containers = nil
			case "changed-config":
				pod.Annotations["checksum/config"] = "old-config"
			case "suspended":
				agent.Spec.Suspended = true
			}
			for _, object := range []client.Object{sts, pod} {
				if err := r.Create(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			before := sts.DeepCopy()
			if scenario == "raced-pod" {
				r.Client = &registryRecoveryRaceClient{Client: r.Client, t: t}
			}
			if scenario == "permission-change" {
				c := &registryAuditClient{Client: r.Client}
				c.allow.Store(true)
				r.Client = c
			}
			err = r.recoverRegistryPull(ctx, agent)
			if (err != nil) != (scenario == "foreign-workload" || scenario == "raced-pod" || scenario == "permission-change") {
				t.Fatalf("unexpected recovery error: %v", err)
			}
			observed := &corev1.Pod{}
			getErr := r.Get(ctx, client.ObjectKeyFromObject(pod), observed)
			wantDeleted := scenario == "initial" || scenario == "custom"
			if apierrors.IsNotFound(getErr) != wantDeleted {
				t.Fatalf("deleted=%v, want %v: %v", apierrors.IsNotFound(getErr), wantDeleted, getErr)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil || !reflect.DeepEqual(before, sts) {
				t.Fatal("pull recovery changed the workload or its storage policy")
			}
		})
	}
}

func TestRegistryNamespaceCreationDoesNotAdoptExistingNamespace(t *testing.T) {
	r, agent, _ := registryFixture(t)
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace(agent.Name), UID: "older-namespace"}}
	if err := r.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNamespace(ctx, ns.Name, agent.Spec.User.Alias, agent); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(ns), ns); err != nil || len(ns.OwnerReferences) != 0 {
		t.Fatal("an existing namespace was silently adopted")
	}
	if err := r.ensureRegistryNamespace(ctx, agent, ""); err == nil {
		t.Fatal("an unowned pending namespace was accepted for registry credentials")
	}
}
