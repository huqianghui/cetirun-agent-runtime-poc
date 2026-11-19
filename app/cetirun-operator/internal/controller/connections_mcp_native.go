// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *AgentReconciler) nativeConnectionsRequested(agent *api.Agent) bool {
	if !r.ConnectionsMCPNativeEnabled || r.ConnectionsMCPURL == "" || !sandboxRequested(agent, nil) ||
		r.SandboxPreparation == nil || !r.SandboxPreparation.NativeExecution ||
		slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) {
		return false
	}
	allowed, err := connectionsMCPRuntimeAllowed(agent.Spec.Agent.Type, r.ConnectionsMCPRuntimeTypes)
	return err == nil && allowed
}

func (r *AgentReconciler) nativeConnectionsDestination(ctx context.Context, agent *api.Agent, platform sandboxpolicy.Platform) (*sandboxpolicy.Destination, *corev1.Service, error) {
	if !r.nativeConnectionsRequested(agent) {
		return nil, nil, nil
	}
	namespace, err := connectionsMCPNamespace(r.ConnectionsMCPURL)
	if err != nil {
		return nil, nil, err
	}
	endpoint, _ := url.Parse(r.ConnectionsMCPURL)
	name := endpoint.Hostname()[:len(endpoint.Hostname())-len("."+namespace+".svc.cluster.local")]
	port := int32(443)
	if endpoint.Port() != "" {
		parsed, err := strconv.ParseUint(endpoint.Port(), 10, 16)
		if err != nil || parsed == 0 {
			return nil, nil, fmt.Errorf("Connections MCP service port is invalid")
		}
		port = int32(parsed)
	}
	service := &corev1.Service{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, service); err != nil {
		return nil, nil, fmt.Errorf("read native Connections Service: %w", err)
	}
	if service.UID == "" || !service.DeletionTimestamp.IsZero() ||
		(service.Spec.Type != "" && service.Spec.Type != corev1.ServiceTypeClusterIP) ||
		service.Spec.ClusterIP == "" || service.Spec.ClusterIP == corev1.ClusterIPNone {
		return nil, nil, fmt.Errorf("native Connections requires a current ClusterIP Service")
	}
	peerUID := service.Spec.Selector[sandboxpolicy.PeerUIDLabel]
	var peer *sandboxpolicy.Peer
	for _, candidate := range platform.Topology.Peers {
		if candidate.Name == name && candidate.Namespace == namespace && string(candidate.UID) == peerUID && peerUID != "" {
			copy := candidate
			peer = &copy
		}
	}
	if peer == nil || !slices.Contains(peer.IPs, service.Spec.ClusterIP) {
		return nil, nil, fmt.Errorf("native Connections Service is not in the current protected managed-peer inventory")
	}
	for _, ip := range service.Spec.ClusterIPs {
		if !slices.Contains(peer.IPs, ip) {
			return nil, nil, fmt.Errorf("native Connections Service address is absent from the protected managed-peer inventory")
		}
	}
	ports := []api.AgentPolicyPort{{Protocol: corev1.ProtocolTCP, Port: port}}
	found := false
	for _, current := range service.Spec.Ports {
		if current.Port != port || current.Protocol != corev1.ProtocolTCP {
			continue
		}
		if found {
			return nil, nil, fmt.Errorf("native Connections HTTPS Service port is ambiguous")
		}
		target := current.TargetPort.IntVal
		if current.TargetPort.Type == intstr.String && current.TargetPort.StrVal == "https" {
			target = 8443 // The Connections serving contract fixes this named port.
		}
		if target == 0 && current.TargetPort.Type == intstr.Int {
			target = current.Port
		}
		if target < 1 || target > 65535 {
			return nil, nil, fmt.Errorf("native Connections target port is not supported")
		}
		if target != port {
			ports = append(ports, api.AgentPolicyPort{Protocol: corev1.ProtocolTCP, Port: target})
		}
		found = true
	}
	if !found {
		return nil, nil, fmt.Errorf("native Connections Service does not expose the configured HTTPS port")
	}
	return &sandboxpolicy.Destination{Peer: *peer, Ports: ports}, service, nil
}

func (r *AgentReconciler) nativeConnectionsInputs(ctx context.Context, attempt *sandboxPreparationAttempt) (*sandboxpolicy.Destination, error) {
	destination, service, err := r.nativeConnectionsDestination(ctx, attempt.agent,
		attempt.configuration.Platforms[attempt.resolution.Sandbox.Type])
	if err != nil || destination == nil {
		return destination, err
	}
	namespace, _ := connectionsMCPNamespace(r.ConnectionsMCPURL)
	audience := r.ConnectionsMCPAudience
	if audience == "" {
		audience = "cetirun-connections-mcp"
	}
	connection := &w.ConnectionsMCP{URL: r.ConnectionsMCPURL, Audience: audience,
		CASecretName: r.ConnectionsMCPCASecretName, Namespace: namespace}
	if err := connection.Validate(); err != nil {
		return nil, err
	}
	ca := &corev1.Secret{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: attempt.input.Namespace.Name, Name: connection.CASecretName}, ca); err != nil {
		return nil, err
	}
	if !connectionsMCPBrokerOwned(ca, attempt.agent) || ca.UID == "" || !ca.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("native Connections CA does not belong to this Agent")
	}
	if err := r.verifyConnectionsMCPBrokerRBAC(ctx, attempt.agent); err != nil {
		return nil, err
	}
	attempt.sources = append(attempt.sources, sandboxSource(service, "v1", "Service"), sandboxSource(ca, "v1", "Secret"))
	for _, object := range []client.Object{&rbacv1.Role{}, &rbacv1.RoleBinding{}} {
		if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: attempt.input.Namespace.Name, Name: connectionsMCPBrokerRoleName}, object); err != nil {
			return nil, err
		}
		kind := "Role"
		if _, ok := object.(*rbacv1.RoleBinding); ok {
			kind = "RoleBinding"
		}
		attempt.sources = append(attempt.sources, sandboxSource(object, rbacv1.SchemeGroupVersion.String(), kind))
	}
	attempt.input.AuthorizedSecrets = append(attempt.input.AuthorizedSecrets, ca.Name)
	attempt.input.ConnectionsMCP = connection
	return destination, nil
}

// Initial projection needs a live current Pod before the Broker can read back
// native admission evidence. This permits preparation, never PolicyApplied.
func (r *AgentReconciler) nativeConnectionsPodPending(ctx context.Context, agent *api.Agent) (bool, error) {
	if !r.nativeConnectionsRequested(agent) {
		return false, nil
	}
	pod := &corev1.Pod{}
	err := r.customReader().Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name) + "-0"}, pod)
	if errors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}
