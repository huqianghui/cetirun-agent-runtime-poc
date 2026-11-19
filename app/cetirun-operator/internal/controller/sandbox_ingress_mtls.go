// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	istioapi "istio.io/api/networking/v1beta1"
	istio "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var errSandboxIngressMTLS = errors.New("managed ingress TLS prerequisites are unavailable")

func (r *AgentReconciler) bootstrapPendingIngressMTLS(ctx context.Context, agent *api.Agent, pending bool) error {
	if !pending || agent.Spec.Suspended {
		return nil
	}
	common, err := r.usesReconciledSandbox(ctx, agent)
	if err != nil || !common {
		return err
	}
	// Reuse owned prerequisite bootstrap only. This does not compile an
	// incomplete TLS plan, publish a route, or authorize a Pod.
	return r.bootstrapReconciledSandbox(ctx, agent)
}

func (r *AgentReconciler) enqueueIngressMTLSSource(ctx context.Context, object client.Object) []reconcile.Request {
	if r.SandboxPreparation == nil || !r.SandboxPreparation.NativeExecution {
		return nil
	}
	snapshot, err := configurationSnapshot(ctx, r.SandboxPreparation.Configuration, r.SandboxPreparation.Now, r.SandboxPreparation.MaxSnapshotAge)
	if err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, platform := range snapshot.Platforms {
		if platform.IngressMTLS == nil {
			continue
		}
		for _, binding := range platform.IngressMTLS.Bindings {
			for _, ref := range append([]ingressmtls.Reference{binding.ServerSecret, binding.Gateway}, binding.ClientSecrets()...) {
				if ref.Namespace != object.GetNamespace() || ref.Name != object.GetName() {
					continue
				}
				name := strings.TrimPrefix(binding.Namespace, "cetirun-")
				agent := &api.Agent{}
				if r.customReader().Get(ctx, client.ObjectKey{Name: name}, agent) == nil && agent.UID == binding.AgentUID {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
				}
				break
			}
		}
	}
	return requests
}

func (r *AgentReconciler) enqueueAgentSecret(ctx context.Context, object client.Object) []reconcile.Request {
	return append(r.enqueueRegistrySecret(ctx, object), r.enqueueIngressMTLSSource(ctx, object)...)
}

func (r *AgentReconciler) sandboxIngressMTLSInputs(ctx context.Context, attempt *sandboxPreparationAttempt) error {
	if !attempt.resolution.AgentPolicy.Network.Ingress.RequireMTLS {
		return nil
	}
	platform := attempt.configuration.Platforms[attempt.resolution.Sandbox.Type]
	if !attempt.reconciled || platform.IngressMTLS == nil {
		return fmt.Errorf("mTLS requires current common-executor certificate registration")
	}
	plan, err := platform.IngressMTLS.Resolve(attempt.agent.UID, attempt.input.Namespace.Name, attempt.input.WorkloadName)
	if err != nil {
		return err
	}
	if plan.Harness != string(attempt.agent.Spec.Agent.Type) || attempt.agent.Spec.Agent.Port == nil ||
		plan.Port != *attempt.agent.Spec.Agent.Port ||
		plan.Gateway.Namespace+"/"+plan.Gateway.Name != r.IstioGateway {
		return fmt.Errorf("mTLS approval differs from this Agent's actual harness, port or shared Gateway")
	}
	if slices.Contains(attempt.input.AuthorizedSecrets, plan.ServerSecret.Name) {
		return fmt.Errorf("mTLS endpoint credentials must be distinct from application and model Secrets")
	}
	for _, ref := range append([]ingressmtls.Reference{plan.ServerSecret}, plan.ClientSecrets()...) {
		object := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
		if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, object); err != nil {
			return fmt.Errorf("mTLS approved Secret is unavailable: %w", err)
		}
		if object.UID != ref.UID || object.ResourceVersion != ref.ResourceVersion || !object.DeletionTimestamp.IsZero() {
			return fmt.Errorf("mTLS Secret UID/version approval changed")
		}
		attempt.sources = append(attempt.sources, sandboxSource(object, "v1", "Secret"))
	}
	gateway := &istio.Gateway{}
	ref := plan.Gateway
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, gateway); err != nil {
		return fmt.Errorf("mTLS shared Gateway is unavailable: %w", err)
	}
	if gateway.UID != ref.UID || gateway.ResourceVersion != ref.ResourceVersion || !gateway.DeletionTimestamp.IsZero() {
		return fmt.Errorf("mTLS Gateway identity changed")
	}
	https := false
	for _, server := range gateway.Spec.Servers {
		https = https || server.Port != nil && server.Port.Number == 443 && server.Port.Protocol == "HTTPS" &&
			server.Tls != nil && server.Tls.Mode == istioapi.ServerTLSSettings_SIMPLE &&
			server.Tls.CredentialName != "" && slices.Contains(server.Hosts, r.DomainSuffix)
	}
	if !https || len(platform.Topology.Ingress) == 0 {
		return fmt.Errorf("mTLS must retain the shared browser HTTPS Gateway")
	}
	for _, destination := range platform.Topology.Ingress {
		if destination.Peer.Namespace != gateway.Namespace || !maps.Equal(destination.Peer.PodSelector, gateway.Spec.Selector) {
			return fmt.Errorf("mTLS Gateway selector differs from the protected Cilium ingress peers")
		}
	}
	attempt.sources = append(attempt.sources, sandboxSource(gateway, "networking.istio.io/v1beta1", "Gateway"))
	return nil
}

// Use the existing grant journal for this additional, name-scoped source read.
// The Gateway client key is never copied or mounted into the Agent namespace.
func (r *AgentReconciler) sandboxMTLSSourceAccess(attempt *sandboxPreparationAttempt, secret ingressmtls.Reference) (*rbacv1.Role, *rbacv1.RoleBinding, sandboxpolicy.BindingReference, error) {
	plan := attempt.input.Controls.IngressMTLS
	if plan == nil {
		return nil, nil, sandboxpolicy.BindingReference{}, nil
	}
	revision, err := sandboxPreparationDigest(plan)
	if err != nil {
		return nil, nil, sandboxpolicy.BindingReference{}, err
	}
	nameDigest, err := sandboxPreparationDigest(attempt.agent.UID)
	if err != nil {
		return nil, nil, sandboxpolicy.BindingReference{}, err
	}
	if secret != plan.ClientSecret {
		nameDigest, err = sandboxPreparationDigest(string(attempt.agent.UID) + "/" + secret.Namespace + "/" + secret.Name)
		if err != nil {
			return nil, nil, sandboxpolicy.BindingReference{}, err
		}
	}
	account := r.SandboxPreparation.AdmissionServiceAccount
	metadata := metav1.ObjectMeta{
		Name: "cetirun-mtls-sources-" + nameDigest[:20], Namespace: secret.Namespace,
		OwnerReferences: []metav1.OwnerReference{w.AgentOwner(attempt.agent.Name, attempt.agent.UID)},
		Labels:          map[string]string{sandboxpolicy.AgentUIDLabel: string(attempt.agent.UID)},
		Annotations:     map[string]string{sandboxpolicy.GrantRevisionAnnotation: revision},
	}
	role := &rbacv1.Role{ObjectMeta: *metadata.DeepCopy(), Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{secret.Name}, Verbs: []string{"get"},
	}}}
	binding := &rbacv1.RoleBinding{ObjectMeta: *metadata.DeepCopy(),
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: r.OperatorNamespace, Name: account}},
	}
	ref := sandboxpolicy.BindingReference{Kind: "RoleBinding", Namespace: binding.Namespace, Name: binding.Name,
		AgentName: attempt.agent.Name, AgentUID: attempt.agent.UID, RoleRef: binding.RoleRef,
		ServiceAccountName: account, ServiceAccountNamespace: r.OperatorNamespace, GrantRevision: revision}
	return role, binding, ref, nil
}

func (r *AgentReconciler) restrictMTLSRoute(ctx context.Context, agent *api.Agent, route *istio.VirtualService) error {
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
		return err
	}
	state := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace,
		Name: w.PodExpectationName(namespace.Name, namespace.UID)}, state); err != nil {
		return err
	}
	expected, err := w.ReadReconcileExpectation(state)
	if err != nil || expected.Agent.UID != agent.UID || expected.AgentGeneration != agent.Generation {
		return fmt.Errorf("mTLS publication requires the current owned execution expectation")
	}
	for _, source := range expected.Sources {
		if !w.EffectiveSource(source) {
			if err := r.verifySandboxSources(ctx, []w.SourceReference{source}); err != nil {
				return err
			}
		} else {
			digest, err := r.nativeSourceDigest(ctx, source, agent.Spec.SandboxRef, false)
			if err != nil || digest != expected.SourceDigests[w.SourceKey(source)] {
				return fmt.Errorf("mTLS publication source changed")
			}
		}
	}
	ports := map[string]map[uint32]bool{}
	serviceName := instanceServiceName(agent.Name)
	serviceHost := serviceName + "." + instanceNamespace(agent.Name) + ".svc.cluster.local"
	hasTLS := false
	for _, desired := range expected.Controls {
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(desired.GroupVersionKind())
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(&desired), actual); err != nil {
			return err
		}
		if !w.ReconciledObjectMatches(actual, &desired) {
			return fmt.Errorf("mTLS publication controls changed")
		}
		if desired.GetKind() == "DestinationRule" && desired.GetName() == ingressmtls.ConfigName {
			mode, _, _ := unstructured.NestedString(desired.Object, "spec", "trafficPolicy", "tls", "mode")
			hasTLS = mode == "MUTUAL"
		}
		if desired.GetKind() == "Service" && (desired.GetName() == serviceName ||
			agent.Spec.Agent.Type == api.AgentTypeCustom && desired.GetName() == terminalpolicy.WorkloadName(serviceName)) {
			host := desired.GetName() + "." + instanceNamespace(agent.Name) + ".svc.cluster.local"
			ports[host] = map[uint32]bool{}
			list, _, _ := unstructured.NestedSlice(desired.Object, "spec", "ports")
			for _, raw := range list {
				port, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("mTLS Service port is malformed")
				}
				target, _, _ := unstructured.NestedInt64(port, "targetPort")
				number, _, _ := unstructured.NestedInt64(port, "port")
				if target < ingressmtls.APIPort || target > ingressmtls.DesktopPort {
					return fmt.Errorf("mTLS Service contains a plaintext target")
				}
				ports[host][uint32(number)] = true
			}
		}
	}
	if !hasTLS || len(ports) == 0 {
		return fmt.Errorf("mTLS publication requires its TLS destination policy and Service")
	}
	desktopPath, terminalPath := "/"+agent.Name+"/desktop", "/"+agent.Name+"/term"
	route.Spec.Http = slices.DeleteFunc(route.Spec.Http, func(http *istioapi.HTTPRoute) bool {
		for _, match := range http.Match {
			if agent.Spec.Agent.Type != api.AgentTypeCustom && match.Uri != nil &&
				((!ports[serviceHost][novncPort] && (match.Uri.GetExact() == desktopPath || match.Uri.GetPrefix() == desktopPath)) ||
					(!ports[serviceHost][ttydPort] && (match.Uri.GetExact() == terminalPath || match.Uri.GetPrefix() == terminalPath))) {
				return true
			}
		}
		for _, destination := range http.Route {
			if destination.Destination == nil || destination.Destination.Port == nil ||
				!ports[destination.Destination.Host][destination.Destination.Port.Number] {
				return true
			}
		}
		return false
	})
	return nil
}
