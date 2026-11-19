// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/chatproxy"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (c *Client) ResolveChat(ctx context.Context, agent *api.Agent, caller chataccess.Caller) (chatproxy.Target, error) {
	return c.resolveChat(ctx, agent, caller, true)
}

// ObserveChat verifies an already-dispatched target without treating unrelated
// desired-generation changes as a new endpoint. The caller compares revisions.
func (c *Client) ObserveChat(ctx context.Context, agent *api.Agent, caller chataccess.Caller) (chatproxy.Target, error) {
	return c.resolveChat(ctx, agent, caller, false)
}

func (c *Client) resolveChat(ctx context.Context, agent *api.Agent, caller chataccess.Caller, newRequest bool) (chatproxy.Target, error) {
	target := chatproxy.Target{AgentUID: string(agent.UID), Limits: chatproxy.DefaultLimits(), Reason: "not_chat_agent"}
	if agent.Spec.Agent.Type != api.AgentTypeCustom || agent.Spec.Agent.Port == nil || agent.Spec.Agent.WebUI {
		return target, nil
	}
	target.Eligible = true
	target.Reason = "updating"
	if !agent.DeletionTimestamp.IsZero() {
		return target, nil
	}
	if agent.Spec.Suspended {
		target.Reason = "suspended"
		return target, nil
	}
	team := &api.Team{}
	if agent.Spec.Team != "" {
		observed, err := c.GetTeam(ctx, agent.Spec.Team)
		if err != nil {
			return target, err
		}
		if !observed.DeletionTimestamp.IsZero() {
			return target, fmt.Errorf("Chat Team is deleting")
		}
		team = observed
	}
	common := commonChatCandidate(agent, team)
	if (agent.Spec.SandboxRef != "" || agent.Status.Sandbox != nil || meta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady) != nil ||
		slices.Contains(agent.Finalizers, "cetirun.microsoft.com/sandbox-hold") || slices.Contains(agent.Finalizers, "cetirun.microsoft.com/sandbox-preparation") ||
		team.Spec.PolicyConfigured()) && !common {
		target.Reason = "sandbox_held"
		return target, nil
	}
	if newRequest && agent.Status.ObservedGeneration != agent.Generation || !meta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionAgentConfigReady) {
		return target, nil
	}
	if !meta.IsStatusConditionTrue(agent.Status.Conditions, chataccess.Condition) {
		target.Reason = "network_unavailable"
		if condition := meta.FindStatusCondition(agent.Status.Conditions, chataccess.Condition); condition != nil && condition.Reason == "ChatContractUnsupported" {
			target.Reason = "contract_unsupported"
		}
		return target, nil
	}
	if agent.Status.Phase != api.PhaseReady || !meta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionReady) {
		target.Reason = "runtime_not_ready"
		return target, nil
	}
	target.Reason = "runtime_not_ready"
	namespace, name := "cetirun-"+agent.Name, chataccess.ServiceName(agent.Name)
	if agent.Status.Namespace != namespace || agent.Status.HelmReleaseName != "openclaw-"+agent.Name || agent.Status.ObservedOperatorHash == "" {
		return target, nil
	}
	ns := &corev1.Namespace{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		return target, err
	}
	if ns.UID == "" || !ns.DeletionTimestamp.IsZero() || ns.Annotations[chataccess.AgentUIDAnnotation] != string(agent.UID) {
		return target, nil
	}
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := c.client.Get(ctx, key, sts); err != nil {
		return target, err
	}
	owner := metav1.GetControllerOf(sts)
	if owner == nil || owner.Kind != "Agent" || owner.UID != agent.UID || sts.UID == "" || !sts.DeletionTimestamp.IsZero() || ptr.Deref(sts.Spec.Replicas, 0) != 1 ||
		sts.Spec.Selector == nil || len(sts.Spec.Selector.MatchLabels) == 0 || len(sts.Spec.Selector.MatchExpressions) != 0 {
		return target, nil
	}
	var commonExpectation commonChatExpectation
	if common {
		var verified bool
		var err error
		commonExpectation, verified, err = c.verifyCommonChatExpectation(ctx, agent, ns, sts, caller)
		if err != nil {
			return target, err
		}
		if !verified {
			return target, nil
		}
	} else if sts.Annotations[chataccess.AgentUIDAnnotation] != string(agent.UID) || sts.Annotations[chataccess.WorkloadAnnotation] == "" {
		return target, nil
	}
	pod := &corev1.Pod{}
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name + "-0"}, pod); err != nil {
		return target, err
	}
	owner = metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "StatefulSet" || owner.UID != sts.UID || pod.UID == "" || !pod.DeletionTimestamp.IsZero() ||
		(!common && pod.Annotations[chataccess.WorkloadAnnotation] != sts.Annotations[chataccess.WorkloadAnnotation]) ||
		(common && (pod.Annotations[commonReconciledAnnotation] != "true" || pod.Annotations[commonWorkloadRevisionAnnotation] != commonExpectation.Revision ||
			pod.Annotations[chataccess.AgentUIDAnnotation] != string(agent.UID))) {
		return target, nil
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready || agent.Spec.Agent.Image == nil {
		return target, nil
	}
	containerID := ""
	var restartCount int32
	for _, container := range pod.Spec.Containers {
		if container.Name != "cetirun" {
			continue
		}
		if container.Image != *agent.Spec.Agent.Image {
			return target, nil
		}
		found := false
		for _, port := range container.Ports {
			if port.Name == "http" && port.ContainerPort == *agent.Spec.Agent.Port {
				found = true
			}
		}
		if !found {
			return target, nil
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == container.Name && status.Ready && status.State.Running != nil {
				containerID = status.ContainerID
				restartCount = status.RestartCount
			}
		}
	}
	if containerID == "" {
		return target, nil
	}
	svc := &corev1.Service{}
	if err := c.client.Get(ctx, key, svc); err != nil {
		return target, err
	}
	if svc.UID == "" || !svc.DeletionTimestamp.IsZero() || svc.Spec.Type != corev1.ServiceTypeClusterIP || net.ParseIP(svc.Spec.ClusterIP) == nil ||
		svc.Spec.ExternalName != "" || len(svc.Spec.ExternalIPs) != 0 || !maps.Equal(svc.Spec.Selector, sts.Spec.Selector.MatchLabels) ||
		(!common && !chatReleaseOwned(svc, agent)) || (common && !commonChatOwned(svc, agent)) {
		return target, nil
	}
	for k, v := range svc.Spec.Selector {
		if pod.Labels[k] != v {
			return target, nil
		}
	}
	chatPort := *agent.Spec.Agent.Port
	if common {
		var valid bool
		chatPort, valid = commonExpectation.chatPort(agent)
		if !valid {
			return target, nil
		}
	}
	portFound := false
	for _, port := range svc.Spec.Ports {
		targetMatches := port.TargetPort == intstr.FromInt32(chatPort) ||
			(chatPort == *agent.Spec.Agent.Port && port.TargetPort == intstr.FromString("http"))
		if port.Name == "http" && port.Protocol == corev1.ProtocolTCP && port.Port == *agent.Spec.Agent.Port && targetMatches {
			portFound = true
		}
	}
	if !portFound {
		return target, nil
	}
	target.Reason = "network_unavailable"
	var network any
	if common {
		network = commonExpectation.Revision
	} else {
		policy := &networkv1.NetworkPolicy{}
		if err := c.client.Get(ctx, key, policy); err != nil {
			return target, err
		}
		if !policy.DeletionTimestamp.IsZero() || !chatReleaseOwned(policy, agent) || !maps.Equal(policy.Spec.PodSelector.MatchLabels, svc.Spec.Selector) || len(policy.Spec.PodSelector.MatchExpressions) != 0 || !slices.Contains(policy.Spec.PolicyTypes, networkv1.PolicyTypeIngress) || !chataccess.Allows(policy, caller, *agent.Spec.Agent.Port) {
			return target, nil
		}
		network = policy.Spec
	}
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(chataccess.RouteGVK)
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: chataccess.RouteName(agent.Name)}, route); err != nil {
		return target, err
	}
	if chataccess.ValidatePrivateRoute(route, agent.Name, string(agent.UID), namespace, *agent.Spec.Agent.Port, caller) != nil {
		return target, nil
	}
	target.Revision = chataccess.Digest(struct {
		Agent, Namespace, Service, StatefulSet, Pod types.UID
		Route                                       types.UID
		Container, Workload                         string
		Restarts, Port                              int32
		Network                                     any
		RouteSpec                                   any
	}{agent.UID, ns.UID, svc.UID, sts.UID, pod.UID, route.GetUID(), containerID, sts.Annotations[chataccess.WorkloadAnnotation], restartCount, *agent.Spec.Agent.Port, network, route.Object["spec"]})
	target.BaseURL = "http://" + net.JoinHostPort(name+"."+namespace+".svc.cluster.local", strconv.Itoa(int(*agent.Spec.Agent.Port)))
	target.Available = true
	target.Reason = "ready"
	return target, nil
}

func chatReleaseOwned(object client.Object, agent *api.Agent) bool {
	return object.GetAnnotations()["meta.helm.sh/release-name"] == agent.Status.HelmReleaseName &&
		object.GetAnnotations()["meta.helm.sh/release-namespace"] == agent.Status.Namespace && object.GetLabels()["app.kubernetes.io/managed-by"] == "Helm"
}
