// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package testfixture supplies synthetic workload inputs for offline tests.
// No fixture is a production registration, writer fence or execution receipt.
package testfixture

import (
	"encoding/json"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

type Fixture struct {
	Input          sandboxworkload.Input
	Agent          *api.Agent
	Team           *api.Team
	Sandbox        *api.Sandbox
	Policy         *api.AgentPolicy
	Namespace      *corev1.Namespace
	ServiceAccount *corev1.ServiceAccount
}

func New(agentType string) Fixture {
	resources := api.SandboxContainerResources{
		Requests: ResourceList("1", "1Gi", "1Gi"), Limits: ResourceList("2", "2Gi", "2Gi"),
	}
	box := &api.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-example", UID: "sandbox-uid", ResourceVersion: "1", Generation: 1},
		Spec: api.SandboxSpec{
			Type: api.SandboxKata, Resources: api.SandboxResources{Agent: resources, Helpers: api.SandboxContainerResources{
				Requests: ResourceList("100m", "128Mi", "128Mi"), Limits: ResourceList("500m", "512Mi", "512Mi"),
			}},
			Storage: api.SandboxStorage{
				Home:    api.SandboxHomeStorage{StorageClassName: "example-csi", Size: resource.MustParse("10Gi"), AccessMode: corev1.ReadWriteOnce},
				Scratch: api.SandboxScratchStorage{SizeLimit: resource.MustParse("1Gi")},
			},
		},
	}
	policy := &api.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "policy-example", UID: "policy-uid", ResourceVersion: "1", Generation: 1},
		Spec: api.AgentPolicySpec{
			Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{
				api.AgentTypeHermes,
				api.AgentTypeOpenClaw,
			}},
			Security: api.AgentPolicySecurity{ReadOnlyRootFilesystem: ptr.To(true)},
			Network:  api.AgentPolicyNetwork{Ingress: api.AgentPolicyIngress{Mode: api.AgentPolicyIngressDisabled}},
		},
	}
	if agentType == "custom" {
		policy.Spec.Harnesses.Allowed = append(policy.Spec.Harnesses.Allowed, api.AgentTypeCustom)
	}
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-example", UID: "team-uid", ResourceVersion: "1", Generation: 1},
		Spec:       api.TeamSpec{SandboxRefs: []string{box.Name}, DefaultSandboxRef: box.Name, AgentPolicyRef: policy.Name},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "agent-uid", ResourceVersion: "1", Generation: 1},
		Spec: api.AgentSpec{
			Team: team.Name, SandboxRef: box.Name,
			Agent: api.HarnessSpec{Type: api.AgentType(agentType)}, GatewayTokenSecretRef: "runtime-secret",
			User: api.UserSpec{Alias: "example", DisplayName: "Example"},
		},
	}
	capabilities := sandbox.Capabilities{
		RuntimeClassName: "example-kata", Revision: "synthetic-only", HostResourceLimits: true,
		ReadOnlyRootFilesystem: true, HomeAccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol}, Protocols: []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP},
		GuestExternalNetwork: true, DenyPrecedence: true, NodeTraffic: true, DNSIPRestrictions: true,
		DisabledIngress: true, EnforcementObservation: true, RestrictiveFailure: true,
	}
	objects := sandbox.Resources{Sandboxes: map[string]*api.Sandbox{box.Name: box}, AgentPolicies: map[string]*api.AgentPolicy{policy.Name: policy}}
	record, err := sandboxbinding.Snapshot(agent, team, objects)
	if err != nil {
		panic(err)
	}
	agent.Annotations = map[string]string{sandboxbinding.AuthorizationAnnotation: record}
	binding, err := sandboxbinding.AuthorizedBinding(agent)
	if err != nil {
		panic(err)
	}
	resolution, err := sandbox.Resolve(sandbox.ResolveInput{
		Agent: agent, Team: team, Resources: objects, AuthorizedBinding: binding,
		Registrations: map[api.SandboxType]sandbox.Registration{api.SandboxKata: {Capabilities: capabilities, Available: true}},
	})
	if err != nil {
		panic(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "cetirun-example", UID: "namespace-uid", ResourceVersion: "1",
		Labels: map[string]string{sandboxworkload.ProtectedNamespaceLabel: "true"},
		Annotations: map[string]string{
			sandboxworkload.AgentNameAnnotation: agent.Name, sandboxworkload.AgentUIDAnnotation: string(agent.UID),
		},
		OwnerReferences: []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)},
	}}
	home := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-example-0", Namespace: ns.Name, UID: "home-uid", ResourceVersion: "1",
			Annotations: map[string]string{sandboxworkload.AgentUIDAnnotation: string(agent.UID)}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: ptr.To("example-csi"),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
		},
	}
	skills := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "skills-example", Namespace: ns.Name, UID: "skills-uid", ResourceVersion: "1"}}
	runtimeClass := &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "example-kata", UID: "runtime-uid", ResourceVersion: "1"},
		Handler:    "example-kata", Overhead: &nodev1.Overhead{PodFixed: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
		}},
	}
	revision := "synthetic-controls-revision"
	bindingJSON, err := json.Marshal(binding)
	if err != nil {
		panic(err)
	}
	network := &sandboxpolicy.CiliumNetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "cilium.io/v2", Kind: "CiliumNetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-example", Namespace: ns.Name,
			Labels: map[string]string{sandboxpolicy.AgentUIDLabel: string(agent.UID)},
			Annotations: map[string]string{
				sandboxpolicy.BindingAnnotation: string(bindingJSON), sandboxpolicy.RevisionAnnotation: revision,
			},
			OwnerReferences: []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)}},
		Spec: sandboxpolicy.CiliumPolicySpec{
			EndpointSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				"k8s:" + sandboxpolicy.AgentUIDLabel: string(agent.UID), "k8s:io.kubernetes.pod.namespace": ns.Name,
			}},
			EnableDefaultDeny: sandboxpolicy.DefaultDeny{Ingress: true, Egress: true},
			Ingress:           []sandboxpolicy.IngressRule{}, IngressDeny: []sandboxpolicy.IngressRule{},
			Egress: []sandboxpolicy.EgressRule{}, EgressDeny: []sandboxpolicy.EgressRule{},
		},
	}
	networkObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(network)
	if err != nil {
		panic(err)
	}
	controls, err := sandboxworkload.ControlsFromPlan(sandboxpolicy.Plan{
		Binding: binding, Revision: revision, ResolutionRevision: resolution.Revision,
		Network: sandboxpolicy.NetworkPlan{
			DNSIPs: []string{"10.96.0.10"}, IngressMode: resolution.AgentPolicy.Network.Ingress.Mode,
		},
		Identity:  sandboxpolicy.IdentityPlan{ServiceAccountName: "default"},
		Resources: []unstructured.Unstructured{{Object: networkObject}},
	})
	if err != nil {
		panic(err)
	}
	return Fixture{
		Input: sandboxworkload.Input{
			Resolution: resolution, AgentName: agent.Name, AgentGeneration: 1, AgentType: agentType,
			Namespace:    sandboxworkload.ObjectIdentity{Name: ns.Name, UID: ns.UID},
			WorkloadName: "example", ReleaseName: "example", RuntimeClass: runtimeClass,
			Home: home, Skills: skills, Helpers: sandboxworkload.Helpers{Doctor: agentType == "openclaw", Desktop: true, Logger: true},
			Controls: controls, AuthorizedSecrets: []string{"runtime-secret"},
		},
		Agent: agent, Team: team, Sandbox: box, Policy: policy, Namespace: ns,
		ServiceAccount: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: "default", Namespace: ns.Name, UID: "serviceaccount-uid", ResourceVersion: "1",
		}, AutomountServiceAccountToken: ptr.To(false)},
	}
}

func ResourceList(cpu, memory, storage string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory),
		corev1.ResourceEphemeralStorage: resource.MustParse(storage),
	}
}

func Workload(input sandboxworkload.Input, template corev1.PodTemplateSpec) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: input.WorkloadName, Namespace: input.Namespace.Name, UID: "statefulset-uid", ResourceVersion: "1",
			OwnerReferences: []metav1.OwnerReference{sandboxworkload.AgentOwner(input.AgentName, input.Resolution.Binding.AgentUID)},
			Annotations: map[string]string{
				sandboxworkload.AgentUIDAnnotation: string(input.Resolution.Binding.AgentUID), sandboxworkload.RevisionAnnotation: input.Controls.Revision,
			}},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(1)), ServiceName: input.WorkloadName, Template: template,
			Selector: &metav1.LabelSelector{MatchLabels: template.Labels},
		},
		Status: appsv1.StatefulSetStatus{UpdateRevision: "example-revision"},
	}
}

func Pod(expected sandboxworkload.Expectation) *corev1.Pod {
	name := expected.Workload.Name + "-0"
	pod := &corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: *expected.Template.ObjectMeta.DeepCopy(), Spec: *expected.Template.Spec.DeepCopy()}
	pod.Name, pod.Namespace = name, expected.Namespace.Name
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "StatefulSet", Name: expected.Workload.Name, UID: expected.Workload.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}}
	pod.Labels["statefulset.kubernetes.io/pod-name"] = name
	pod.Labels["apps.kubernetes.io/pod-index"] = "0"
	if expected.ControllerRevision != "" {
		pod.Labels["controller-revision-hash"] = expected.ControllerRevision
	}
	pod.Spec.Hostname, pod.Spec.Subdomain, pod.Spec.Overhead = name, expected.Workload.Name, expected.Overhead.DeepCopy()
	return pod
}
