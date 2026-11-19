// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Pull references are immutable on a Pod. OnDelete preserves healthy writers,
// but an initial pull failure needs a fresh Pod to consume a newly added reference.
func (r *AgentReconciler) recoverRegistryPull(ctx context.Context, agent *api.Agent) error {
	if agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() || agent.Spec.Agent.RegistrySecretRef == nil || agent.Status.RegistryCredentials == nil {
		return nil
	}
	ns := instanceNamespace(agent.Name)
	sts := &appsv1.StatefulSet{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: ns, Name: instanceServiceName(agent.Name)}, sts); err != nil {
		return client.IgnoreNotFound(err)
	}
	name, err := registryauth.PullSecretName(agent.UID)
	if err != nil {
		return err
	}
	if sts.UID == "" || !sts.DeletionTimestamp.IsZero() || sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 ||
		len(sts.Spec.Template.Spec.ImagePullSecrets) != 1 || sts.Spec.Template.Spec.ImagePullSecrets[0].Name != name {
		return nil
	}
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		if !hasAgentOwner(sts, agent) || sts.Annotations[customAgentUID] != string(agent.UID) {
			return fmt.Errorf("RegistryRecoveryOwnershipConflict")
		}
	} else {
		namespace := &corev1.Namespace{}
		if r.customReader().Get(ctx, client.ObjectKey{Name: ns}, namespace) != nil || !hasAgentOwner(namespace, agent) ||
			sts.Annotations["meta.helm.sh/release-name"] != helmReleaseName(agent.Name) ||
			sts.Annotations["meta.helm.sh/release-namespace"] != ns {
			return fmt.Errorf("RegistryRecoveryOwnershipConflict")
		}
	}
	secret := &corev1.Secret{}
	if r.customReader().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, secret) != nil ||
		string(secret.UID) != agent.Status.RegistryCredentials.PullSecretUID || registryauth.Validate(secret, agent) != nil {
		return fmt.Errorf("RegistryRecoveryCredentialsPending")
	}
	pods := &corev1.PodList{}
	if err := r.customReader().List(ctx, pods, client.InNamespace(ns), client.MatchingLabels(sts.Spec.Template.Labels)); err != nil {
		return fmt.Errorf("RegistryRecoveryObservationPending")
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != "StatefulSet" || owner.UID != sts.UID || pod.Name != sts.Name+"-0" ||
			!registryUnstartedPullPod(pod, sts.Spec.Template, name) {
			continue
		}
		if err := registryauth.CheckIsolation(ctx, r.registryClient(), r.registryNamespace(), ns,
			sts.Spec.Template.Spec.ServiceAccountName, agent.Spec.Agent.RegistrySecretRef.Name); err != nil {
			return fmt.Errorf("RegistryIsolationUnverified")
		}
		if err := r.checkRegistryIntent(ctx, agent); err != nil {
			return err
		}
		uid, rv := pod.UID, pod.ResourceVersion
		if err := r.Delete(ctx, pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("RegistryRecoveryPodChanged")
		}
	}
	return nil
}

func registryUnstartedPullPod(pod *corev1.Pod, desired corev1.PodTemplateSpec, pullSecret string) bool {
	if pod.UID == "" || pod.ResourceVersion == "" || !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodPending {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return false
		}
	}
	for _, ref := range pod.Spec.ImagePullSecrets {
		if ref.Name == pullSecret {
			return false
		}
	}
	for key, value := range desired.Annotations {
		if pod.Annotations[key] != value {
			return false
		}
	}
	if !registryContainerImagesMatch(pod.Spec.Containers, desired.Spec.Containers) ||
		!registryContainerImagesMatch(pod.Spec.InitContainers, desired.Spec.InitContainers) {
		return false
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == "istio-proxy" {
			continue
		}
		observed := false
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != container.Name {
				continue
			}
			observed = true
			if status.State.Running != nil || status.State.Terminated != nil || status.LastTerminationState.Terminated != nil ||
				status.RestartCount != 0 || (status.Started != nil && *status.Started) {
				return false
			}
		}
		if !observed {
			return false
		}
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			if waiting := status.State.Waiting; waiting != nil && (waiting.Reason == "ErrImagePull" || waiting.Reason == "ImagePullBackOff") {
				return true
			}
		}
	}
	return false
}

func registryContainerImagesMatch(actual, desired []corev1.Container) bool {
	for _, expected := range desired {
		if !slices.ContainsFunc(actual, func(container corev1.Container) bool {
			return container.Name == expected.Name && container.Image == expected.Image
		}) {
			return false
		}
	}
	return true
}
