// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"google.golang.org/protobuf/proto"
	istioapi "istio.io/api/networking/v1beta1"
	istio "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	accessEpochAnnotation = "sandbox.cetirun.microsoft.com/access-epoch"
	accessPhaseAnnotation = "sandbox.cetirun.microsoft.com/access-phase"
	accessStopAnnotation  = "sandbox.cetirun.microsoft.com/access-stop"
	accessNamespaceUID    = "sandbox.cetirun.microsoft.com/access-namespace-uid"
)

// SandboxExecutionAccess implements API-side access publication, not ingress
// authentication or guest-external containment. Callers must obtain qualified
// evidence for the shared gateway and enforce all source-write barriers.
type SandboxExecutionAccess struct {
	Reconciler   *AgentReconciler
	Coordination sandboxcoordination.Store
	Now          func() time.Time
	MaxAge       time.Duration
}

// Enroll creates only a nonforwarding slot during verified held preparation.
// Capture the returned intent in coordination before any start. Publication is
// update-only, so a delayed request cannot recreate a deleted route.
func (a SandboxExecutionAccess) Enroll(ctx context.Context, intent sandboxcoordination.Intent) (sandboxcoordination.Intent, error) {
	intent = copyAccessIntent(intent)
	if err := a.configured(ctx); err != nil {
		return sandboxcoordination.Intent{}, err
	}
	if intent.Access != nil {
		return sandboxcoordination.Intent{}, fmt.Errorf("%w: access enrollment must not replace an existing binding", sandboxcoordination.ErrConflict)
	}
	if err := a.Reconciler.CheckSandboxExecutionSources(ctx, intent); err != nil {
		return sandboxcoordination.Intent{}, err
	}
	if intent.IngressMode == api.AgentPolicyIngressDisabled {
		if err := a.absent(ctx, intent); err != nil {
			return sandboxcoordination.Intent{}, err
		}
		return intent, nil
	}
	agent, service, gateway, err := a.inputs(ctx, intent)
	if err != nil {
		return sandboxcoordination.Intent{}, err
	}
	ingressNamespaces, err := a.ingressNamespaces(ctx, intent, false)
	if err != nil {
		return sandboxcoordination.Intent{}, err
	}
	binding := &sandboxcoordination.AccessBinding{
		Route:             w.ObjectIdentity{Name: virtualServiceName(intent.Agent.Name)},
		Service:           sandboxSource(service, "v1", "Service"),
		Gateway:           sandboxSource(gateway, "networking.istio.io/v1beta1", "Gateway"),
		IngressNamespaces: ingressNamespaces,
		Host:              a.Reconciler.DomainSuffix, Port: *agent.Spec.Agent.Port,
	}
	for _, source := range append([]w.SourceReference{binding.Service, binding.Gateway}, ingressNamespaces...) {
		found := false
		for _, existing := range intent.Sources {
			if w.SourceKey(existing) == w.SourceKey(source) {
				if existing != source {
					return sandboxcoordination.Intent{}, fmt.Errorf("%w: access source differs from preparation", sandboxcoordination.ErrConflict)
				}
				found = true
			}
		}
		if !found {
			intent.Sources = append(intent.Sources, source)
		}
		digest, err := a.Reconciler.executionSourceDigest(ctx, source, intent.SelectedSandbox(), true)
		if err != nil {
			return sandboxcoordination.Intent{}, err
		}
		intent.SourceDigests[w.SourceKey(source)] = digest
	}
	if len(intent.Sources) > sandboxcoordination.MaxSources {
		return sandboxcoordination.Intent{}, fmt.Errorf("%w: access sources exceed the execution inventory bound", sandboxcoordination.ErrUnavailable)
	}
	desired, err := accessRoute(intent, binding, "dormant")
	if err != nil {
		return sandboxcoordination.Intent{}, err
	}
	current := &istio.VirtualService{}
	err = a.Reconciler.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if err != nil && !apierrors.IsNotFound(err) {
		return sandboxcoordination.Intent{}, err
	}
	if err := a.Reconciler.CheckSandboxExecutionSources(ctx, intent); err != nil {
		return sandboxcoordination.Intent{}, err
	}
	if apierrors.IsNotFound(err) {
		if err := a.Reconciler.Create(ctx, desired); err != nil {
			return sandboxcoordination.Intent{}, fmt.Errorf("create dormant Sandbox access slot: %w", err)
		}
		if err := a.Reconciler.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
			return sandboxcoordination.Intent{}, err
		}
	}
	if !accessOwned(current, intent) || !current.DeletionTimestamp.IsZero() ||
		current.Annotations[accessPhaseAnnotation] != "dormant" || !proto.Equal(&current.Spec, &desired.Spec) {
		return sandboxcoordination.Intent{}, fmt.Errorf("%w: access slot is foreign, changed or already used", sandboxcoordination.ErrConflict)
	}
	binding.Route.UID = current.UID
	intent.Access = binding
	if err := intent.Validate(); err != nil {
		return sandboxcoordination.Intent{}, err
	}
	if err := a.Reconciler.CheckSandboxExecutionSources(ctx, intent); err != nil {
		return sandboxcoordination.Intent{}, err
	}
	return intent, nil
}

// Publish requires both the recorded running authority and separate enforcement
// evidence. It never writes Agent status or infers authentication from Istio CRs.
func (a SandboxExecutionAccess) Publish(ctx context.Context, ticket sandboxcoordination.Ticket,
	observation sandbox.Observation, guard func(context.Context) error,
) error {
	ticket = ticket.DeepCopy()
	check := func(ctx context.Context) error { return a.publicationGuard(ctx, ticket, observation, guard) }
	if err := check(ctx); err != nil {
		return err
	}
	intent := ticket.Execution.Intent
	if intent.IngressMode == api.AgentPolicyIngressDisabled {
		if err := a.absent(ctx, intent); err != nil {
			return err
		}
		return check(ctx)
	}
	agent, service, gateway, err := a.inputs(ctx, intent)
	if err != nil {
		return err
	}
	binding := intent.Access
	if binding == nil || service.UID != binding.Service.UID || gateway.UID != binding.Gateway.UID ||
		binding.Host != a.Reconciler.DomainSuffix || binding.Port != *agent.Spec.Agent.Port {
		return fmt.Errorf("%w: access inputs differ from enrollment", sandboxcoordination.ErrConflict)
	}
	current, err := a.route(ctx, intent)
	if err != nil {
		return err
	}
	if !current.DeletionTimestamp.IsZero() || current.Annotations[accessPhaseAnnotation] == "withdrawn" {
		return fmt.Errorf("%w: access slot is withdrawn or terminating", sandboxcoordination.ErrConflict)
	}
	if current.Annotations[accessPhaseAnnotation] != "dormant" && current.Annotations[accessPhaseAnnotation] != "published" {
		return fmt.Errorf("%w: access slot phase is unknown", sandboxcoordination.ErrConflict)
	}
	desired, err := accessRoute(intent, binding, "published")
	if err != nil {
		return err
	}
	native := buildVirtualService(agent, binding.Host, binding.Gateway.Namespace+"/"+binding.Gateway.Name)
	if native == nil {
		return fmt.Errorf("%w: native access renderer is unavailable", sandboxcoordination.ErrUnavailable)
	}
	path := "/ui/" + intent.Agent.Name
	for _, route := range native.Spec.Http {
		nativeUI := len(route.Match) > 0
		for _, match := range route.Match {
			if match.Uri == nil || (match.Uri.GetExact() != path && match.Uri.GetPrefix() != path+"/") {
				nativeUI = false
			}
		}
		if nativeUI {
			desired.Spec.Http = append(desired.Spec.Http, proto.Clone(route).(*istioapi.HTTPRoute))
		}
	}
	if len(desired.Spec.Http) != 3 {
		return fmt.Errorf("%w: expected only native UI redirect, HTTP and WebSocket routes", sandboxcoordination.ErrUnavailable)
	}
	if !proto.Equal(&current.Spec, &desired.Spec) || current.Annotations[accessPhaseAnnotation] != "published" {
		current.Spec = *desired.Spec.DeepCopy()
		current.Annotations[accessPhaseAnnotation] = "published"
		if err := check(ctx); err != nil {
			return err
		}
		if err := a.Reconciler.Update(ctx, current); err != nil {
			return fmt.Errorf("publish Sandbox access; conditional outcome must be observed: %w", err)
		}
	}
	if err := check(ctx); err != nil {
		return err
	}
	confirmed, err := a.route(ctx, intent)
	if err != nil {
		return err
	}
	if !confirmed.DeletionTimestamp.IsZero() || confirmed.Annotations[accessPhaseAnnotation] != "published" ||
		!proto.Equal(&confirmed.Spec, &desired.Spec) {
		return fmt.Errorf("%w: published access differs at readback", sandboxcoordination.ErrConflict)
	}
	return check(ctx)
}

// Withdraw changes an enrolled slot to nonforwarding state before cleanup can
// release coordination. Absence is safe only for these update-only route writes;
// it is never evidence that the Agent or its HOME writer is quiescent.
func (a SandboxExecutionAccess) Withdraw(ctx context.Context, ticket sandboxcoordination.Ticket, guard func(context.Context) error) error {
	ticket = ticket.DeepCopy()
	check := func(ctx context.Context) error {
		if err := a.configured(ctx); err != nil {
			return err
		}
		if ticket.Execution.Phase != sandboxcoordination.Stopping || guard == nil {
			return fmt.Errorf("%w: withdrawal requires an exact stopping authority", sandboxcoordination.ErrConflict)
		}
		if err := guard(ctx); err != nil {
			return err
		}
		return a.Coordination.Check(ctx, ticket)
	}
	if err := check(ctx); err != nil {
		return err
	}
	intent := ticket.Execution.Intent
	if intent.Access == nil {
		if intent.IngressMode != api.AgentPolicyIngressDisabled {
			return fmt.Errorf("%w: access lifetime was never enrolled", sandboxcoordination.ErrUnavailable)
		}
		if err := a.absent(ctx, intent); err != nil {
			return err
		}
		return check(ctx)
	}
	current, err := a.route(ctx, intent)
	if apierrors.IsNotFound(err) {
		return check(ctx)
	}
	if err != nil {
		return err
	}
	desired, err := accessRoute(intent, intent.Access, "withdrawn")
	if err != nil {
		return err
	}
	marker := sandboxEventDigest(string(ticket.AuthorityUID) + "/" + ticket.Execution.ID)
	if !proto.Equal(&current.Spec, &desired.Spec) || current.Annotations[accessPhaseAnnotation] != "withdrawn" ||
		current.Annotations[accessStopAnnotation] != marker {
		current.Spec = *desired.Spec.DeepCopy()
		current.Annotations[accessPhaseAnnotation], current.Annotations[accessStopAnnotation] = "withdrawn", marker
		if err := check(ctx); err != nil {
			return err
		}
		if err := a.Reconciler.Update(ctx, current); err != nil {
			return fmt.Errorf("withdraw Sandbox access: %w", err)
		}
	}
	if err := check(ctx); err != nil {
		return err
	}
	confirmed, err := a.route(ctx, intent)
	if apierrors.IsNotFound(err) {
		return check(ctx)
	}
	if err != nil {
		return err
	}
	if !proto.Equal(&confirmed.Spec, &desired.Spec) || confirmed.Annotations[accessStopAnnotation] != marker ||
		confirmed.Annotations[accessPhaseAnnotation] != "withdrawn" {
		return fmt.Errorf("%w: access withdrawal is not observed", sandboxcoordination.ErrConflict)
	}
	if err := check(ctx); err != nil {
		return err
	}
	uid, version := confirmed.UID, confirmed.ResourceVersion
	if err := a.Reconciler.Delete(ctx, confirmed, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("delete withdrawn Sandbox access slot: %w", err)
	}
	if _, err := a.route(ctx, intent); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: withdrawn access slot still awaits deletion", sandboxcoordination.ErrUnavailable)
	}
	return check(ctx)
}

func (a SandboxExecutionAccess) configured(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.Reconciler == nil || a.Reconciler.Client == nil || a.Reconciler.APIReader == nil || a.Coordination.Client == nil {
		return fmt.Errorf("%w: access requires resource and coordination writers plus an authoritative reader", sandboxcoordination.ErrUnavailable)
	}
	return nil
}

func (a SandboxExecutionAccess) publicationGuard(ctx context.Context, ticket sandboxcoordination.Ticket,
	observation sandbox.Observation, guard func(context.Context) error,
) error {
	if err := a.configured(ctx); err != nil {
		return err
	}
	if guard == nil || a.Now == nil || a.MaxAge <= 0 || a.MaxAge > time.Minute ||
		ticket.Execution.Phase != sandboxcoordination.Running {
		return fmt.Errorf("%w: access publication requires running authority, evidence and freshness", sandboxcoordination.ErrUnavailable)
	}
	if err := guard(ctx); err != nil {
		return err
	}
	if err := a.Coordination.Check(ctx, ticket); err != nil {
		return err
	}
	if _, err := a.Reconciler.SandboxRunningStatusInput(ctx, ticket.Execution.Intent); err != nil {
		return err
	}
	if intent := ticket.Execution.Intent; intent.IngressMode == api.AgentPolicyIngressAuthenticated {
		agent, service, gateway, err := a.inputs(ctx, intent)
		if err != nil {
			return err
		}
		binding := intent.Access
		if binding == nil || binding.Host != a.Reconciler.DomainSuffix || binding.Port != *agent.Spec.Agent.Port ||
			binding.Service.UID != service.UID || binding.Gateway.UID != gateway.UID {
			return fmt.Errorf("%w: access binding differs from the current platform inputs", sandboxcoordination.ErrConflict)
		}
		namespaces, err := a.ingressNamespaces(ctx, intent, true)
		if err != nil {
			return err
		}
		if len(binding.IngressNamespaces) != len(namespaces) {
			return fmt.Errorf("%w: ingress workload namespaces differ from enrollment", sandboxcoordination.ErrConflict)
		}
		for i, namespace := range namespaces {
			if namespace.Name != binding.IngressNamespaces[i].Name || namespace.UID != binding.IngressNamespaces[i].UID {
				return fmt.Errorf("%w: ingress workload namespace identity changed", sandboxcoordination.ErrConflict)
			}
		}
	}
	if err := a.Coordination.Check(ctx, ticket); err != nil {
		return err
	}
	return sandbox.VerifyObservation(ticket.Execution.Intent.Expected, &observation, a.Now(), a.MaxAge)
}

func (a SandboxExecutionAccess) route(ctx context.Context, intent sandboxcoordination.Intent) (*istio.VirtualService, error) {
	current := &istio.VirtualService{}
	err := a.Reconciler.APIReader.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Access.Route.Name}, current)
	if err != nil {
		return nil, err
	}
	if current.UID != intent.Access.Route.UID || !accessOwned(current, intent) {
		return nil, fmt.Errorf("%w: access slot identity or ownership changed", sandboxcoordination.ErrConflict)
	}
	return current, nil
}

func (a SandboxExecutionAccess) absent(ctx context.Context, intent sandboxcoordination.Intent) error {
	if err := a.Reconciler.APIReader.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name,
		Name: virtualServiceName(intent.Agent.Name)}, &istio.VirtualService{}); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("%w: disabled ingress still has an access route object", sandboxcoordination.ErrConflict)
}

func (a SandboxExecutionAccess) inputs(ctx context.Context, intent sandboxcoordination.Intent) (*api.Agent, *corev1.Service, *istio.Gateway, error) {
	fail := func() (*api.Agent, *corev1.Service, *istio.Gateway, error) {
		return nil, nil, nil, fmt.Errorf("%w: access requires explicit native Service and HTTPS Gateway bindings", sandboxcoordination.ErrUnavailable)
	}
	host := a.Reconciler.DomainSuffix
	gatewayKey := strings.Split(a.Reconciler.IstioGateway, "/")
	if len(validation.IsDNS1123Subdomain(host)) != 0 || host == "" || len(gatewayKey) != 2 ||
		len(validation.IsDNS1123Label(gatewayKey[0])) != 0 || len(validation.IsDNS1123Subdomain(gatewayKey[1])) != 0 {
		return fail()
	}
	agent := &api.Agent{}
	if err := a.Reconciler.APIReader.Get(ctx, client.ObjectKey{Name: intent.Agent.Name}, agent); err != nil {
		return nil, nil, nil, err
	}
	if agent.UID != intent.Agent.UID || agent.Generation != intent.AgentGeneration ||
		agent.Spec.Agent.Type != api.AgentTypeOpenClaw || !sandboxPreparationSupported(agent) ||
		intent.Workload.Name != instanceServiceName(agent.Name) ||
		agent.Spec.Agent.Port == nil || *agent.Spec.Agent.Port < 1 || *agent.Spec.Agent.Port > 65535 ||
		!sandboxAccessNameAllowed(agent.Name) {
		return fail()
	}
	service := &corev1.Service{}
	if err := a.Reconciler.APIReader.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Workload.Name}, service); err != nil {
		return nil, nil, nil, err
	}
	workload := &appsv1.StatefulSet{}
	if err := a.Reconciler.APIReader.Get(ctx, client.ObjectKey{Namespace: intent.Namespace.Name, Name: intent.Workload.Name}, workload); err != nil {
		return nil, nil, nil, err
	}
	if service.UID == "" || service.ResourceVersion == "" || !service.DeletionTimestamp.IsZero() ||
		!equality.Semantic.DeepEqual(service.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(intent.Agent.Name, intent.Agent.UID)}) ||
		service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.ExternalName != "" ||
		len(service.Spec.ExternalIPs) != 0 || service.Spec.LoadBalancerClass != nil || service.Spec.LoadBalancerIP != "" ||
		workload.UID != intent.Workload.UID || workload.Spec.Selector == nil || len(workload.Spec.Selector.MatchExpressions) != 0 ||
		len(service.Spec.Selector) == 0 || !maps.Equal(service.Spec.Selector, workload.Spec.Selector.MatchLabels) {
		return fail()
	}
	found := false
	for _, port := range service.Spec.Ports {
		if port.NodePort != 0 {
			return fail()
		}
		if port.Port != *agent.Spec.Agent.Port || port.Protocol != corev1.ProtocolTCP {
			continue
		}
		if port.TargetPort.Type == intstr.Int && port.TargetPort.IntVal == port.Port {
			found = true
		}
		if port.TargetPort.Type == intstr.String {
			for _, container := range workload.Spec.Template.Spec.Containers {
				for _, target := range container.Ports {
					if target.Name == port.TargetPort.StrVal && target.ContainerPort == port.Port && target.Protocol == corev1.ProtocolTCP {
						found = true
					}
				}
			}
		}
	}
	if !found {
		return fail()
	}
	gateway := &istio.Gateway{}
	if err := a.Reconciler.APIReader.Get(ctx, client.ObjectKey{Namespace: gatewayKey[0], Name: gatewayKey[1]}, gateway); err != nil {
		return nil, nil, nil, err
	}
	if gateway.UID == "" || gateway.ResourceVersion == "" || !gateway.DeletionTimestamp.IsZero() || len(gateway.Spec.Selector) == 0 {
		return fail()
	}
	for _, server := range gateway.Spec.Servers {
		if server.Port != nil && server.Port.Number == 443 && server.Port.Protocol == "HTTPS" && server.Tls != nil &&
			server.Tls.Mode == istioapi.ServerTLSSettings_SIMPLE && server.Tls.CredentialName != "" && slices.Contains(server.Hosts, host) {
			return agent, service, gateway, nil
		}
	}
	return fail()
}

func (a SandboxExecutionAccess) ingressNamespaces(ctx context.Context, intent sandboxcoordination.Intent, running bool) ([]w.SourceReference, error) {
	attempt, err := a.Reconciler.checkSandboxExecutionSources(ctx, intent, running)
	if err != nil {
		return nil, err
	}
	platform := attempt.configuration.Platforms[attempt.resolution.Sandbox.Type]
	names := make([]string, 0, len(platform.Topology.Ingress))
	for _, destination := range platform.Topology.Ingress {
		if len(validation.IsDNS1123Label(destination.Peer.Namespace)) != 0 {
			return nil, fmt.Errorf("%w: ingress workloads require explicit namespaces in trusted topology", sandboxcoordination.ErrUnavailable)
		}
		names = append(names, destination.Peer.Namespace)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: trusted ingress workload namespaces are unavailable", sandboxcoordination.ErrUnavailable)
	}
	names = uniqueReferences(names)
	if len(names) > sandboxcoordination.MaxIngressNamespaces {
		return nil, fmt.Errorf("%w: ingress namespace inventory exceeds its bound", sandboxcoordination.ErrUnavailable)
	}
	sources := make([]w.SourceReference, 0, len(names))
	for _, name := range names {
		namespace := &corev1.Namespace{}
		if err := a.Reconciler.APIReader.Get(ctx, client.ObjectKey{Name: name}, namespace); err != nil {
			return nil, err
		}
		if namespace.Name != name || namespace.UID == "" || namespace.ResourceVersion == "" || !namespace.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("%w: ingress workload namespace is unavailable or terminating", sandboxcoordination.ErrUnavailable)
		}
		sources = append(sources, sandboxSource(namespace, "v1", "Namespace"))
	}
	for _, observed := range []time.Time{attempt.configuration.ObservedAt, attempt.prerequisites.ObservedAt} {
		if err := configurationSnapshotTime(observed, configurationNow(a.Reconciler.SandboxPreparation.Now),
			a.Reconciler.SandboxPreparation.MaxSnapshotAge); err != nil {
			return nil, err
		}
	}
	return sources, ctx.Err()
}

func accessRoute(intent sandboxcoordination.Intent, binding *sandboxcoordination.AccessBinding, phase string) (*istio.VirtualService, error) {
	if phase != "withdrawn" && !sandboxAccessNameAllowed(intent.Agent.Name) {
		return nil, fmt.Errorf("%w: native access name is reserved or invalid", sandboxcoordination.ErrConflict)
	}
	epoch, err := sandboxPreparationDigest(intent.Expected)
	if err != nil {
		return nil, err
	}
	exportTo := make([]string, 0, len(binding.IngressNamespaces))
	for _, namespace := range binding.IngressNamespaces {
		exportTo = append(exportTo, namespace.Name)
	}
	if len(exportTo) == 0 {
		if phase != "withdrawn" {
			return nil, fmt.Errorf("%w: authenticated access requires enrolled ingress workload namespaces", sandboxcoordination.ErrUnavailable)
		}
		// Legacy bindings exported only to the Gateway resource namespace.
		// Preserve that scope solely for their nonforwarding cleanup update.
		exportTo = []string{binding.Gateway.Namespace}
	}
	route := &istio.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: intent.Namespace.Name, Name: binding.Route.Name,
			OwnerReferences: []metav1.OwnerReference{w.AgentOwner(intent.Agent.Name, intent.Agent.UID)},
			Labels:          map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"},
			Annotations: map[string]string{
				w.AgentUIDAnnotation: string(intent.Agent.UID), accessNamespaceUID: string(intent.Namespace.UID),
				accessEpochAnnotation: epoch, accessPhaseAnnotation: phase,
			},
		},
		Spec: istioapi.VirtualService{
			Hosts: []string{binding.Host}, Gateways: []string{binding.Gateway.Namespace + "/" + binding.Gateway.Name},
			ExportTo: exportTo,
		},
	}
	if phase != "published" {
		path := "/ui/" + intent.Agent.Name
		route.Spec.Http = []*istioapi.HTTPRoute{{
			Match: []*istioapi.HTTPMatchRequest{
				{Uri: &istioapi.StringMatch{MatchType: &istioapi.StringMatch_Exact{Exact: path}}},
				{Uri: &istioapi.StringMatch{MatchType: &istioapi.StringMatch_Prefix{Prefix: path + "/"}}},
			},
			DirectResponse: &istioapi.HTTPDirectResponse{Status: 404},
		}}
	}
	return route, nil
}

func accessOwned(route *istio.VirtualService, intent sandboxcoordination.Intent) bool {
	epoch, err := sandboxPreparationDigest(intent.Expected)
	return err == nil && route.UID != "" && route.ResourceVersion != "" &&
		route.Namespace == intent.Namespace.Name && route.Name == virtualServiceName(intent.Agent.Name) &&
		equality.Semantic.DeepEqual(route.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(intent.Agent.Name, intent.Agent.UID)}) &&
		route.Annotations[w.AgentUIDAnnotation] == string(intent.Agent.UID) &&
		route.Annotations[accessNamespaceUID] == string(intent.Namespace.UID) && route.Annotations[accessEpochAnnotation] == epoch
}

func sandboxAccessNameAllowed(name string) bool {
	return name != "" && name != "api" && name != "ui" && len(validation.IsDNS1123Subdomain(name)) == 0
}

func copyAccessIntent(intent sandboxcoordination.Intent) sandboxcoordination.Intent {
	return (sandboxcoordination.Ticket{Execution: sandboxcoordination.Execution{Intent: intent}}).DeepCopy().Execution.Intent
}
