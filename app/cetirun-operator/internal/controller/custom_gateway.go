// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *AgentReconciler) customGatewaySelected(instance *api.Agent) bool {
	provider, ok := r.CustomBindings.(interface{ UsesAIGateway(*api.Agent) bool })
	return ok && provider.UsesAIGateway(instance)
}

func (r *AgentReconciler) bindCustomGateway(ctx context.Context, instance *api.Agent, b *runtimebindings.Custom) (returnedErr error) {
	previous := instance.DeepCopy().Status
	defer func() {
		if err := r.updateCustomStatus(ctx, instance, previous); err != nil {
			returnedErr = err
		}
	}()
	if !r.AIGateway.Enabled {
		return aiGatewayDegraded(instance, aiGatewayReasonDisabled, fmt.Errorf("the approved Custom image requires the platform AI Gateway"))
	}
	namespace, port, err := aiGatewayEndpoint(r.AIGateway.URL)
	if err != nil {
		return err
	}
	endpoint, _ := url.Parse(r.AIGateway.URL)
	service := &corev1.Service{}
	if err := r.customReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: strings.Split(endpoint.Hostname(), ".")[0]}, service); err != nil {
		return err
	}
	selector := map[string]string{"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway"}
	if service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.ClusterIP == "" || service.Spec.ClusterIP == "None" ||
		len(service.Spec.Selector) == 0 || !service.DeletionTimestamp.IsZero() {
		return fmt.Errorf("AI Gateway requires a live private Service")
	}
	for key, value := range selector {
		if service.Spec.Selector[key] != value {
			return fmt.Errorf("AI Gateway Service does not select the platform Gateway")
		}
	}
	if !slices.ContainsFunc(service.Spec.Ports, func(value corev1.ServicePort) bool {
		return value.Port == int32(port) && value.Protocol == corev1.ProtocolTCP
	}) {
		return fmt.Errorf("AI Gateway Service port does not match its configured endpoint")
	}
	// Never let generic key recovery adopt a foreign Custom credential object.
	for _, name := range []string{aiGatewayKeySecretName, aiGatewayProxySecretName, api.CopilotTokenSecretName} {
		secret := &corev1.Secret{}
		err := r.customReader().Get(ctx, types.NamespacedName{Namespace: b.Namespace.Name, Name: name}, secret)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !hasAgentOwner(secret, instance) || secret.Labels[aiGatewayAgentUIDLabel] != string(instance.UID) || !secret.DeletionTimestamp.IsZero() {
			return fmt.Errorf("Custom Gateway credential has foreign ownership or is deleting")
		}
	}
	if err := r.reconcileAIGateway(ctx, instance, true); err != nil {
		return err
	}
	secret := &corev1.Secret{}
	if err := r.customReader().Get(ctx, types.NamespacedName{Namespace: b.Namespace.Name, Name: aiGatewayKeySecretName}, secret); err != nil {
		return err
	}
	if !hasAgentOwner(secret, instance) || secret.Labels[aiGatewayAgentUIDLabel] != string(instance.UID) ||
		!aigateway.ValidKeyValue(string(secret.Data[aiGatewayKeySecretField])) || !secret.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Custom Gateway key binding is not owned and ready")
	}
	b.Model = runtimebindings.ModelAccess{ObjectReference: objectReference(secret), Key: aiGatewayKeySecretField, BaseURL: r.AIGateway.URL + "/v1"}
	b.Network.Gateway = runtimebindings.Peer{Namespace: namespace, PodLabels: selector}
	b.Network.GatewayPort = int32(port)
	b.Network.Revision += ":gateway:" + string(service.UID)
	if r.aiGatewayCopilotActive(instance) {
		copilot := instance.Status.AIGateway.Copilot
		if copilot.ProxyKeyID == "" {
			return fmt.Errorf("Custom Copilot proxy credentials are not ready")
		}
		b.Copilot = &runtimebindings.CopilotProxy{Image: r.AIGateway.CopilotProxyImage, KeyID: copilot.ProxyKeyID}
		if generation := instance.Annotations[api.AnnotationCopilotSignInGeneration]; copilotSignInGenerationPattern.MatchString(generation) {
			b.Copilot.SignInGeneration = generation
		}
		models, _ := parseCopilotModels(instance.Annotations[api.AnnotationCopilotModels])
		for _, model := range models {
			if model.ResponsesOnly && copilotModelName(copilot, model.ID) != "" {
				b.Copilot.ResponsesModels = append(b.Copilot.ResponsesModels, model.ID)
			}
		}
		slices.Sort(b.Copilot.ResponsesModels)
	}
	return nil
}

func (r *AgentReconciler) stopCustomCopilot(ctx context.Context, instance *api.Agent) error {
	if instance.Status.AIGateway == nil {
		return nil
	}
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: instanceNamespace(instance.Name), Name: copilotProxyServiceName(instance)}
	if err := r.customReader().Get(ctx, key, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !hasAgentOwner(deployment, instance) || deployment.Annotations[customAgentUID] != string(instance.UID) {
		return fmt.Errorf("Custom Copilot proxy owner is not confirmed")
	}
	if ptr.Deref(deployment.Spec.Replicas, 0) == 0 {
		return nil
	}
	before := deployment.DeepCopy()
	deployment.Spec.Replicas = ptr.To[int32](0)
	return r.Patch(ctx, deployment, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
