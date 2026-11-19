// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chattest

import (
	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var Caller = chataccess.Caller{Namespace: "test-platform", PodLabels: map[string]string{"app": "test-portal"}}

func Objects() (*api.Agent, []client.Object) {
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "chat-example", UID: "11111111-1111-4111-8111-111111111111", Generation: 1}, Spec: api.AgentSpec{
		Agent: api.HarnessSpec{Type: api.AgentTypeCustom, Image: ptr.To("registry.example.com/agent:test"), Port: ptr.To[int32](8080)}, Team: "example-team", User: api.UserSpec{Alias: "alice", Email: "alice@example.com"},
	}, Status: api.AgentStatus{Phase: api.PhaseReady, ObservedGeneration: 1, ObservedOperatorHash: "synthetic-plan", Namespace: "cetirun-chat-example", HelmReleaseName: "openclaw-chat-example"}}
	for _, name := range []string{api.ConditionReady, api.ConditionAgentConfigReady, chataccess.Condition} {
		agent.Status.Conditions = append(agent.Status.Conditions, metav1.Condition{Type: name, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "SyntheticReady", LastTransitionTime: metav1.Now()})
	}
	labels := map[string]string{"app.kubernetes.io/name": "cetirun", "app.kubernetes.io/instance": agent.Status.HelmReleaseName}
	name := chataccess.ServiceName(agent.Name)
	metadata := func(kind string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: agent.Status.Namespace, UID: types.UID("synthetic-" + kind), Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": agent.Status.HelmReleaseName, "meta.helm.sh/release-namespace": agent.Status.Namespace}}
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agent.Status.Namespace, UID: "synthetic-namespace", Annotations: map[string]string{chataccess.AgentUIDAnnotation: string(agent.UID)}}}
	sts := &appsv1.StatefulSet{ObjectMeta: metadata("statefulset"), Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1), Selector: &metav1.LabelSelector{MatchLabels: labels}}}
	sts.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(agent, api.GroupVersion.WithKind("Agent"))}
	sts.Annotations[chataccess.AgentUIDAnnotation] = string(agent.UID)
	sts.Annotations[chataccess.WorkloadAnnotation] = "synthetic-workload"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: ns.Name, UID: "synthetic-pod", Labels: labels, Annotations: map[string]string{chataccess.WorkloadAnnotation: "synthetic-workload"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "cetirun", Image: *agent.Spec.Agent.Image, Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}}}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "cetirun", Ready: true, ContainerID: "containerd://synthetic", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	svc := &corev1.Service{ObjectMeta: metadata("service"), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.10", Selector: labels, Ports: []corev1.ServicePort{{Name: "http", Port: 8080, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP}}}}
	np := &networkv1.NetworkPolicy{ObjectMeta: metadata("policy"), Spec: networkv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: labels}, PolicyTypes: []networkv1.PolicyType{networkv1.PolicyTypeIngress}, Ingress: []networkv1.NetworkPolicyIngressRule{{From: []networkv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": Caller.Namespace}}, PodSelector: &metav1.LabelSelector{MatchLabels: Caller.PodLabels}}}, Ports: []networkv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(8080))}}}}}}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.Team, UID: "synthetic-team"}}
	route := chataccess.PrivateRoute(agent.Name, string(agent.UID), ns.Name, 8080, []chataccess.Caller{Caller})
	route.SetUID("synthetic-route")
	return agent, []client.Object{agent, ns, sts, pod, svc, np, team, route}
}
