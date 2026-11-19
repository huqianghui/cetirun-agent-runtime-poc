// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubernetesNativeQuiescence observes ordinary Kubernetes shutdown, not a
// physical writer fence. Retained single-start lifetimes remain RWOP-only.
type KubernetesNativeQuiescence struct {
	APIReader  client.Reader
	Reconciled bool
}

func (s *KubernetesNativeQuiescence) CheckQuiesced(ctx context.Context, agent *api.Agent, inputs SandboxPrerequisites) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.APIReader == nil || agent == nil || agent.UID == "" ||
		inputs.AgentUID != agent.UID || inputs.NamespaceUID == "" || inputs.Home.Name == "" || inputs.Home.UID == "" {
		return fmt.Errorf("%w: exact native Sandbox ownership is required", sandbox.ErrObservationUnavailable)
	}
	namespace := &corev1.Namespace{}
	name := instanceNamespace(agent.Name)
	if err := s.APIReader.Get(ctx, client.ObjectKey{Name: name}, namespace); err != nil {
		if apierrors.IsNotFound(err) && !agent.DeletionTimestamp.IsZero() {
			return ctx.Err()
		}
		return fmt.Errorf("inspect native Sandbox namespace: %w", err)
	}
	if namespace.UID != inputs.NamespaceUID || !sandboxBuiltinOwned(namespace, agent) {
		return fmt.Errorf("native Sandbox namespace ownership changed")
	}
	home := &corev1.PersistentVolumeClaim{}
	err := s.APIReader.Get(ctx, client.ObjectKey{Namespace: name, Name: inputs.Home.Name}, home)
	deleting := !agent.DeletionTimestamp.IsZero() && !namespace.DeletionTimestamp.IsZero()
	if err != nil && !(apierrors.IsNotFound(err) && deleting) {
		return fmt.Errorf("inspect native Sandbox HOME: %w", err)
	}
	if err == nil && (home.UID != inputs.Home.UID ||
		(!home.DeletionTimestamp.IsZero() && !deleting) ||
		!(equality.Semantic.DeepEqual(home.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}) ||
			(s.Reconciled && equality.Semantic.DeepEqual(home.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}))) ||
		home.Annotations[sandboxworkload.AgentUIDAnnotation] != string(agent.UID)) {
		return fmt.Errorf("native Sandbox HOME must be the exact owned claim with a supported access mode")
	}
	workload := &appsv1.StatefulSet{}
	err = s.APIReader.Get(ctx, client.ObjectKey{Namespace: name, Name: instanceServiceName(agent.Name)}, workload)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("inspect native Sandbox workload: %w", err)
	}
	if err == nil && (!sandboxBuiltinOwned(workload, agent) || ptr.Deref(workload.Spec.Replicas, -1) != 0) {
		return fmt.Errorf("native Sandbox workload has not stopped")
	}
	pods := &corev1.PodList{}
	if err := s.APIReader.List(ctx, pods, client.InNamespace(name), client.Limit(1)); err != nil {
		return fmt.Errorf("inspect native Sandbox Pods: %w", err)
	}
	if len(pods.Items) != 0 {
		return fmt.Errorf("native Sandbox waits for ordinary Pod termination; force deletion is not supported")
	}
	return ctx.Err()
}
