// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	"github.com/Azure/CetiRun/app/internal/podtemplate"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	commonExpectationKey             = "expectation.json"
	commonReconciledAnnotation       = "sandbox.cetirun.microsoft.com/reconciled"
	commonWorkloadRevisionAnnotation = "cetirun.microsoft.com/sandbox-revision"
	commonPolicyRevisionAnnotation   = "sandbox.cetirun.microsoft.com/revision"
)

type commonChatIdentity struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

type commonChatBinding struct {
	AgentUID       types.UID `json:"agentUID"`
	TeamUID        types.UID `json:"teamUID"`
	SandboxUID     types.UID `json:"sandboxUID"`
	AgentPolicyUID types.UID `json:"agentPolicyUID"`
}

type commonChatWorkload struct {
	Kind     string                 `json:"kind"`
	Identity commonChatIdentity     `json:"identity"`
	Replicas int32                  `json:"replicas"`
	Template corev1.PodTemplateSpec `json:"template"`
}

type commonChatSource struct {
	APIVersion      string    `json:"apiVersion"`
	Kind            string    `json:"kind"`
	Namespace       string    `json:"namespace,omitempty"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion,omitempty"`
	Generation      int64     `json:"generation,omitempty"`
	IntentDigest    string    `json:"intentDigest,omitempty"`
}

type commonChatExpectation struct {
	Version          string                      `json:"version"`
	Agent            commonChatIdentity          `json:"agent"`
	AgentGeneration  int64                       `json:"agentGeneration"`
	Namespace        commonChatIdentity          `json:"namespace"`
	Binding          commonChatBinding           `json:"binding"`
	Revision         string                      `json:"revision"`
	PlanHash         string                      `json:"planHash"`
	Workloads        []commonChatWorkload        `json:"workloads"`
	Sources          []commonChatSource          `json:"sources"`
	Controls         []unstructured.Unstructured `json:"controls"`
	CustomIngressTLS *commonChatIngressTLS       `json:"customIngressTLS,omitempty"`
}

type commonChatIngressTLS struct {
	AgentUID    types.UID          `json:"agentUID"`
	Namespace   string             `json:"namespace"`
	Harness     string             `json:"harness"`
	Port        int32              `json:"port"`
	ServiceHost string             `json:"serviceHost"`
	ServerName  string             `json:"serverName"`
	ChatClients []commonChatClient `json:"chatClients"`
}

type commonChatClient struct {
	Secret    commonChatSource  `json:"secret"`
	PodLabels map[string]string `json:"podLabels"`
}

func (e commonChatExpectation) chatPort(agent *api.Agent) (int32, bool) {
	if e.CustomIngressTLS == nil {
		return *agent.Spec.Agent.Port, true
	}
	plan := e.CustomIngressTLS
	host := chataccess.ServiceName(agent.Name) + "." + e.Namespace.Name + ".svc.cluster.local"
	hash := sha256.Sum256([]byte(agent.UID))
	if plan.AgentUID != agent.UID || plan.Namespace != e.Namespace.Name || plan.Harness != "custom" ||
		plan.Port != *agent.Spec.Agent.Port || plan.ServiceHost != host ||
		plan.ServerName != "agent-"+hex.EncodeToString(hash[:16])+"."+host {
		return 0, false
	}
	return chataccess.MTLSAPIPort, true
}

func commonExpectationName(namespace string, uid types.UID) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + string(uid)))
	return "sandbox-pod-" + hex.EncodeToString(sum[:20])
}

func commonChatCandidate(agent *api.Agent, team *api.Team) bool {
	if agent.Spec.SandboxRef == "" || agent.Status.Sandbox == nil || team == nil || !team.Spec.PolicyConfigured() ||
		team.UID == "" || team.UID != agent.Status.Sandbox.TeamUID ||
		slices.Contains(agent.Finalizers, api.SandboxHoldFinalizer) || slices.Contains(agent.Finalizers, api.SandboxPreparationFinalizer) {
		return false
	}
	condition := meta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == agent.Generation &&
		agent.Status.Sandbox.SandboxUID != "" && agent.Status.Sandbox.AgentPolicyUID != "" && agent.Status.Sandbox.AppliedRevision != "" &&
		meta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionPolicyApplied)
}

func commonChatOwned(object client.Object, agent *api.Agent) bool {
	owner := metav1.GetControllerOf(object)
	return len(object.GetOwnerReferences()) == 1 && owner != nil &&
		owner.APIVersion == api.GroupVersion.String() && owner.Kind == "Agent" && owner.Name == agent.Name && owner.UID == agent.UID
}

func (c *Client) verifyCommonChatExpectation(ctx context.Context, agent *api.Agent, namespace *corev1.Namespace,
	sts *appsv1.StatefulSet, caller chataccess.Caller,
) (commonChatExpectation, bool, error) {
	var expected commonChatExpectation
	state := &corev1.ConfigMap{}
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: c.sandboxStateNamespace, Name: commonExpectationName(namespace.Name, namespace.UID)}, state); err != nil {
		return expected, false, err
	}
	if state.UID == "" || !state.DeletionTimestamp.IsZero() || len(state.Data) != 1 || len(state.BinaryData) != 0 ||
		state.Annotations[chataccess.AgentUIDAnnotation] != string(agent.UID) || !commonChatOwned(state, agent) ||
		!commonChatOwned(namespace, agent) || !commonChatOwned(sts, agent) {
		return expected, false, nil
	}
	raw := state.Data[commonExpectationKey]
	intentDigest, err := agent.SandboxExecutionIntentDigest()
	if err != nil {
		return expected, false, err
	}
	if len(raw) == 0 || len(raw) > 512*1024 || json.Unmarshal([]byte(raw), &expected) != nil ||
		expected.Version != "v2" || expected.Agent != (commonChatIdentity{Name: agent.Name, UID: agent.UID}) ||
		expected.AgentGeneration != agent.Generation || expected.Namespace != (commonChatIdentity{Name: namespace.Name, UID: namespace.UID}) ||
		expected.Revision == "" || expected.Revision != state.Annotations[commonWorkloadRevisionAnnotation] ||
		expected.Revision != agent.Status.Sandbox.AppliedRevision || expected.PlanHash == "" ||
		expected.Binding != (commonChatBinding{AgentUID: agent.UID, TeamUID: agent.Status.Sandbox.TeamUID,
			SandboxUID: agent.Status.Sandbox.SandboxUID, AgentPolicyUID: agent.Status.Sandbox.AgentPolicyUID}) {
		return expected, false, nil
	}
	agentSources := 0
	for _, source := range expected.Sources {
		if source.APIVersion == api.GroupVersion.String() && source.Kind == "Agent" {
			if source.Namespace != "" || source.ResourceVersion != "" || source.Name != agent.Name || source.UID != agent.UID ||
				source.Generation != agent.Generation || source.IntentDigest != intentDigest {
				return expected, false, nil
			}
			agentSources++
		}
	}
	if agentSources != 1 || !slices.ContainsFunc(expected.Workloads, func(workload commonChatWorkload) bool {
		return workload.Kind == "StatefulSet" && workload.Identity == (commonChatIdentity{Name: sts.Name, UID: sts.UID}) &&
			workload.Replicas == 1 && workload.Template.Annotations[commonReconciledAnnotation] == "true" &&
			workload.Template.Annotations[chataccess.AgentUIDAnnotation] == string(agent.UID) &&
			podtemplate.Same(workload.Template, sts.Spec.Template)
	}) {
		return expected, false, nil
	}
	port, approved, err := c.verifyCommonChatIngress(ctx, agent, expected, caller)
	if err != nil || !approved {
		return expected, false, err
	}
	for _, control := range expected.Controls {
		if !commonChatCiliumAllows(control, agent, namespace.Name, caller, port) {
			continue
		}
		current, err := c.commonChatControlCurrent(ctx, agent, expected.Revision, control)
		return expected, current, err
	}
	return expected, false, nil
}

func (c *Client) commonChatControlCurrent(ctx context.Context, agent *api.Agent, revision string, control unstructured.Unstructured) (bool, error) {
	actual := &unstructured.Unstructured{}
	actual.SetGroupVersionKind(control.GroupVersionKind())
	if err := c.client.Get(ctx, client.ObjectKeyFromObject(&control), actual); err != nil {
		return false, err
	}
	return actual.GetUID() != "" && actual.GetDeletionTimestamp().IsZero() && commonChatOwned(actual, agent) &&
		actual.GetAnnotations()[commonPolicyRevisionAnnotation] == revision &&
		equality.Semantic.DeepEqual(actual.Object["spec"], control.Object["spec"]), nil
}

func (c *Client) verifyCommonChatIngress(ctx context.Context, agent *api.Agent, expected commonChatExpectation, caller chataccess.Caller) (int32, bool, error) {
	port, valid := expected.chatPort(agent)
	if !valid {
		return 0, false, nil
	}
	plan := expected.CustomIngressTLS
	if plan == nil {
		return port, true, nil
	}
	host := plan.ServiceHost
	hash := sha256.Sum256([]byte(agent.UID))
	var clients []commonChatClient
	for _, candidate := range plan.ChatClients {
		if candidate.Secret.Namespace == caller.Namespace && maps.Equal(candidate.PodLabels, caller.PodLabels) {
			clients = append(clients, candidate)
		}
	}
	if len(clients) != 1 {
		return 0, false, nil
	}
	approved := clients[0]
	if approved.Secret.Name == "" || approved.Secret.UID == "" || approved.Secret.ResourceVersion == "" ||
		!slices.ContainsFunc(expected.Sources, func(source commonChatSource) bool {
			return source.APIVersion == "v1" && source.Kind == "Secret" && source.Namespace == approved.Secret.Namespace &&
				source.Name == approved.Secret.Name && source.UID == approved.Secret.UID && source.ResourceVersion == approved.Secret.ResourceVersion
		}) {
		return 0, false, nil
	}
	want := map[string]any{
		"host": host, "exportTo": []any{"."},
		"workloadSelector": map[string]any{"matchLabels": stringMapValues(caller.PodLabels)},
		"trafficPolicy": map[string]any{
			"tls": map[string]any{
				"mode": "MUTUAL", "credentialName": approved.Secret.Name,
				"sni": plan.ServerName, "subjectAltNames": []any{plan.ServerName}, "insecureSkipVerify": false,
			},
			"connectionPool": map[string]any{"tcp": map[string]any{"maxConnectionDuration": "300s"}},
		},
	}
	for _, control := range expected.Controls {
		if control.GetAPIVersion() == "networking.istio.io/v1beta1" && control.GetKind() == "DestinationRule" &&
			control.GetName() == fmt.Sprintf("cetirun-ingress-mtls-%x", hash[:16]) && control.GetNamespace() == caller.Namespace &&
			commonChatOwned(&control, agent) && control.GetAnnotations()[commonPolicyRevisionAnnotation] == expected.Revision &&
			equality.Semantic.DeepEqual(control.Object["spec"], want) {
			current, err := c.commonChatControlCurrent(ctx, agent, expected.Revision, control)
			return port, current, err
		}
	}
	return 0, false, nil
}

func stringMapValues(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func commonChatCiliumAllows(control unstructured.Unstructured, agent *api.Agent, namespace string, caller chataccess.Caller, port int32) bool {
	if control.GetAPIVersion() != "cilium.io/v2" || control.GetKind() != "CiliumNetworkPolicy" || control.GetNamespace() != namespace ||
		control.GetAnnotations()[commonPolicyRevisionAnnotation] != agent.Status.Sandbox.AppliedRevision || !commonChatOwned(&control, agent) {
		return false
	}
	selector, _, err := unstructured.NestedStringMap(control.Object, "spec", "endpointSelector", "matchLabels")
	if err != nil || selector["k8s:sandbox.cetirun.microsoft.com/agent-uid"] != string(agent.UID) ||
		selector["k8s:io.kubernetes.pod.namespace"] != namespace {
		return false
	}
	rules, _, err := unstructured.NestedSlice(control.Object, "spec", "ingress")
	if err != nil {
		return false
	}
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if ok && commonChatRuleAllows(rule, caller, port) {
			return true
		}
	}
	return false
}

func commonChatRuleAllows(rule map[string]any, caller chataccess.Caller, port int32) bool {
	endpoints, _, err := unstructured.NestedSlice(rule, "fromEndpoints")
	if err != nil {
		return false
	}
	want := map[string]string{"k8s:io.kubernetes.pod.namespace": caller.Namespace}
	for key, value := range caller.PodLabels {
		want["k8s:"+key] = value
	}
	matched := false
	for _, raw := range endpoints {
		selector, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		labels, _, err := unstructured.NestedStringMap(selector, "matchLabels")
		if err == nil && maps.Equal(labels, want) {
			matched = true
		}
	}
	if !matched {
		return false
	}
	toPorts, _, err := unstructured.NestedSlice(rule, "toPorts")
	if err != nil {
		return false
	}
	for _, raw := range toPorts {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ports, _, err := unstructured.NestedSlice(entry, "ports")
		if err != nil {
			continue
		}
		for _, raw := range ports {
			item, ok := raw.(map[string]any)
			if ok && item["port"] == strconv.Itoa(int(port)) && item["protocol"] == "TCP" {
				return true
			}
		}
	}
	return false
}
