// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	strictjson "sigs.k8s.io/json"
)

const (
	ReconcileExpectationVersion = "v2"
	ReconciledAnnotation        = "sandbox.cetirun.microsoft.com/reconciled"
)

// ReconcileExpectation replaces a one-Pod allowance with the current authorized
// chart output. It uses the same protected state object, not another authority.
type ReconcileExpectation struct {
	Version                string                      `json:"version"`
	Agent                  ObjectIdentity              `json:"agent"`
	AgentGeneration        int64                       `json:"agentGeneration"`
	Namespace              ObjectIdentity              `json:"namespace"`
	Binding                sandbox.Binding             `json:"binding"`
	Revision               string                      `json:"revision"`
	PlanHash               string                      `json:"planHash"`
	Home                   ObjectIdentity              `json:"home"`
	Workloads              []RenderedWorkload          `json:"workloads"`
	Sources                []SourceReference           `json:"sources"`
	SourceDigests          map[string]string           `json:"sourceDigests"`
	Controls               []unstructured.Unstructured `json:"controls"`
	IdentityServiceAccount *corev1.ServiceAccount      `json:"identityServiceAccount,omitempty"`
	Terminal               *TerminalTarget             `json:"terminal,omitempty"`
	CustomIngressTLS       *ingressmtls.Plan           `json:"customIngressTLS,omitempty"`
}

func (e ReconcileExpectation) approvedChatTLSControl(object unstructured.Unstructured) bool {
	plan := e.CustomIngressTLS
	if plan == nil || plan.Harness != "custom" || plan.AgentUID != e.Agent.UID || plan.Namespace != e.Namespace.Name ||
		plan.Binding.Validate() != nil || object.GetAPIVersion() != "networking.istio.io/v1beta1" || object.GetKind() != "DestinationRule" {
		return false
	}
	registered := false
	for _, client := range plan.ChatClients {
		if client.Secret.Namespace == object.GetNamespace() && slices.ContainsFunc(e.Sources, func(source SourceReference) bool {
			return source.APIVersion == "v1" && source.Kind == "Secret" && source.Namespace == client.Secret.Namespace &&
				source.Name == client.Secret.Name && source.UID == client.Secret.UID && source.ResourceVersion == client.Secret.ResourceVersion
		}) {
			registered = true
		}
	}
	if !registered {
		return false
	}
	expected, err := sandboxpolicy.IngressMTLSResources(sandboxpolicy.CompileInput{
		AgentName: e.Agent.Name, Namespace: e.Namespace.Name, Resolution: sandbox.Resolution{Binding: e.Binding},
	}, *plan, object.GetAnnotations()[sandboxpolicy.RevisionAnnotation])
	if err != nil {
		return false
	}
	return slices.ContainsFunc(expected, func(want unstructured.Unstructured) bool {
		return equality.Semantic.DeepEqual(want.Object, object.Object)
	})
}

// TerminalTarget is the controller-owned CONNECT target, never caller input.
type TerminalTarget struct {
	ServiceAccountName string `json:"serviceAccountName"`
	TargetWorkload     string `json:"targetWorkload"`
	Container          string `json:"container"`
}

type RenderedWorkload struct {
	Kind     string                 `json:"kind"`
	Identity ObjectIdentity         `json:"identity"`
	Replicas int32                  `json:"replicas"`
	Template corev1.PodTemplateSpec `json:"template"`
	Overhead corev1.ResourceList    `json:"overhead,omitempty"`
}

func (e ReconcileExpectation) Object(namespace string) (*corev1.ConfigMap, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if namespace == "" || namespace == e.Namespace.Name {
		return nil, fmt.Errorf("Sandbox expectations require an independent controller namespace")
	}
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > 512*1024 {
		return nil, fmt.Errorf("Sandbox reconciled expectation exceeds its encoding limit")
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: PodExpectationName(e.Namespace.Name, e.Namespace.UID),
			OwnerReferences: []metav1.OwnerReference{AgentOwner(e.Agent.Name, e.Agent.UID)},
			Annotations: map[string]string{
				AgentUIDAnnotation: string(e.Agent.UID), RevisionAnnotation: e.Revision,
			},
		},
		Data: map[string]string{PodExpectationKey: string(raw)},
	}, nil
}

func ReadReconcileExpectation(state *corev1.ConfigMap) (ReconcileExpectation, error) {
	return readReconcileExpectation(state, false)
}

func ReadReconcileExpectationForCleanup(state *corev1.ConfigMap) (ReconcileExpectation, error) {
	return readReconcileExpectation(state, true)
}

func readReconcileExpectation(state *corev1.ConfigMap, cleanup bool) (ReconcileExpectation, error) {
	var e ReconcileExpectation
	if state == nil || len(state.Data) != 1 || len(state.BinaryData) != 0 ||
		state.UID == "" || state.ResourceVersion == "" || (!cleanup && !state.DeletionTimestamp.IsZero()) {
		return e, fmt.Errorf("Sandbox reconciled expectation is unavailable")
	}
	raw := state.Data[PodExpectationKey]
	if len(raw) == 0 || len(raw) > 512*1024 {
		return e, fmt.Errorf("Sandbox reconciled expectation is empty or oversized")
	}
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &e)
	if err != nil || len(problems) != 0 {
		return e, fmt.Errorf("Sandbox reconciled expectation is invalid")
	}
	if err := e.Validate(); err != nil {
		return e, err
	}
	if state.Name != PodExpectationName(e.Namespace.Name, e.Namespace.UID) ||
		state.Namespace == e.Namespace.Name ||
		state.Annotations[AgentUIDAnnotation] != string(e.Agent.UID) ||
		state.Annotations[RevisionAnnotation] != e.Revision ||
		!equality.Semantic.DeepEqual(state.OwnerReferences, []metav1.OwnerReference{AgentOwner(e.Agent.Name, e.Agent.UID)}) {
		return e, fmt.Errorf("Sandbox reconciled expectation ownership differs")
	}
	return e, nil
}

func (e ReconcileExpectation) Validate() error {
	if e.Version != ReconcileExpectationVersion || e.Agent.Name == "" || e.Agent.UID == "" ||
		e.AgentGeneration < 1 || e.Namespace.Name == "" || e.Namespace.UID == "" ||
		e.Home.Name == "" || e.Home.UID == "" || e.Binding.AgentUID != e.Agent.UID ||
		e.Binding.TeamUID == "" || e.Binding.SandboxUID == "" || e.Binding.AgentPolicyUID == "" ||
		!ValidInputDigest(e.Revision) || !ValidInputDigest(e.PlanHash) ||
		len(e.Workloads) == 0 || len(e.Workloads) > 32 || len(e.Sources) > MaxPreparationSources ||
		len(e.Controls) < 3 || len(e.Controls) > MaxPreparationSources {
		return fmt.Errorf("Sandbox reconciled expectation requires complete bounded identities")
	}

	names := map[string]bool{}
	var main *RenderedWorkload
	for _, workload := range e.Workloads {
		key := workload.Kind + "/" + workload.Identity.Name
		if (workload.Kind != "StatefulSet" && workload.Kind != "Deployment") || workload.Identity.Name == "" ||
			names[key] || workload.Replicas < 0 || workload.Replicas > 1 || len(workload.Template.Spec.Containers) == 0 {
			return fmt.Errorf("Sandbox reconciled workload inventory is invalid")
		}
		names[key] = true
		if workload.Kind == "StatefulSet" {
			if main != nil {
				return fmt.Errorf("Sandbox requires exactly one retained HOME workload")
			}
			copy := workload
			main = &copy
		}
	}
	if main == nil {
		return fmt.Errorf("Sandbox has no retained HOME workload")
	}
	if plan := e.CustomIngressTLS; plan != nil {
		registration := ingressmtls.Registration{Image: plan.Image, Bindings: []ingressmtls.Binding{plan.Binding}}
		resolved, err := registration.Resolve(e.Agent.UID, e.Namespace.Name, main.Identity.Name)
		if err != nil || plan.Harness != "custom" || !equality.Semantic.DeepEqual(resolved, plan) {
			return fmt.Errorf("Custom ingress plan differs from its bound workload identity")
		}
		for _, ref := range append([]ingressmtls.Reference{plan.ServerSecret, plan.Gateway}, plan.ClientSecrets()...) {
			kind := "Secret"
			apiVersion := "v1"
			if ref == plan.Gateway {
				kind, apiVersion = "Gateway", "networking.istio.io/v1beta1"
			}
			if !slices.ContainsFunc(e.Sources, func(source SourceReference) bool {
				return source.APIVersion == apiVersion && source.Kind == kind && source.Namespace == ref.Namespace &&
					source.Name == ref.Name && source.UID == ref.UID && source.ResourceVersion == ref.ResourceVersion
			}) {
				return fmt.Errorf("Custom ingress plan omits an approved source identity")
			}
		}
	}
	if terminal := e.Terminal; terminal != nil {
		if len(validation.IsDNS1123Label(terminal.ServiceAccountName)) != 0 ||
			terminal.ServiceAccountName != terminalpolicy.WorkloadName(main.Identity.Name) ||
			terminal.TargetWorkload != main.Identity.Name || terminal.Container != "cetirun" {
			return fmt.Errorf("Sandbox terminal target is invalid")
		}
		if !slices.ContainsFunc(e.Workloads, func(workload RenderedWorkload) bool {
			return workload.Kind == "Deployment" && workload.Identity.Name == terminal.ServiceAccountName && workload.Replicas == 1
		}) {
			return fmt.Errorf("Sandbox terminal workload is absent")
		}
	}
	entra, kubernetes := false, false
	for _, volume := range main.Template.Spec.Volumes {
		if volume.Projected != nil {
			for _, projection := range volume.Projected.Sources {
				entra = entra || projection.ServiceAccountToken != nil && projection.ServiceAccountToken.Audience == sandboxpolicy.EntraAudience
				kubernetes = kubernetes || volume.Name == "sandbox-kubernetes-api" && projection.ServiceAccountToken != nil
			}
		}
	}
	if entra && e.IdentityServiceAccount == nil || e.IdentityServiceAccount != nil && !entra && !kubernetes {
		return fmt.Errorf("bootstrap identity source must match an active identity projection")
	}
	sources := map[string]bool{}
	required := map[string]bool{}
	effective := 0
	for _, source := range e.Sources {
		key := SourceKey(source)
		if !source.ValidIdentity() || sources[key] {
			return fmt.Errorf("Sandbox source identities must be complete and unique")
		}
		sources[key] = true
		if EffectiveSource(source) {
			effective++
			if !ValidInputDigest(e.SourceDigests[key]) {
				return fmt.Errorf("Sandbox effective source digest is missing")
			}
		}
		if source.APIVersion == api.GroupVersion.String() && source.Namespace == "" {
			switch source.Kind {
			case "Agent":
				required["Agent"] = source.Name == e.Agent.Name && source.UID == e.Agent.UID
			case "Team":
				required["Team"] = source.UID == e.Binding.TeamUID
			case "Sandbox":
				required["Sandbox"] = source.UID == e.Binding.SandboxUID
			case "AgentPolicy":
				required["AgentPolicy"] = source.UID == e.Binding.AgentPolicyUID
			}
		}
		if source.APIVersion == "v1" && source.Kind == "Namespace" {
			required["Namespace"] = source.Name == e.Namespace.Name && source.UID == e.Namespace.UID && source.Namespace == ""
		}
		if source.APIVersion == "v1" && source.Kind == "PersistentVolumeClaim" && source.Name == e.Home.Name && source.Namespace == e.Namespace.Name {
			required["HOME"] = source.UID == e.Home.UID
		}
		if source.APIVersion == "node.k8s.io/v1" && source.Kind == "RuntimeClass" {
			required["RuntimeClass"] = source.Namespace == "" && source.Name == ptr.Deref(main.Template.Spec.RuntimeClassName, "")
		}
		if source.APIVersion == "storage.k8s.io/v1" && source.Kind == "StorageClass" && source.Namespace == "" {
			required["StorageClass"] = true
		}
		if source.APIVersion == "v1" && source.Kind == "ConfigMap" && source.Namespace != "" && source.Namespace != e.Namespace.Name {
			required["Platform"] = true
		}
		if source.APIVersion == "v1" && source.Kind == "ServiceAccount" && source.Namespace == e.Namespace.Name &&
			source.Name == main.Template.Spec.ServiceAccountName {
			required["ServiceAccount"] = true
		}
	}
	if effective != len(e.SourceDigests) {
		return fmt.Errorf("Sandbox source digest inventory differs from its sources")
	}
	for _, kind := range []string{"Agent", "Team", "Sandbox", "AgentPolicy", "Namespace", "HOME", "RuntimeClass", "StorageClass", "Platform"} {
		if !required[kind] {
			return fmt.Errorf("Sandbox source inventory is missing %s", kind)
		}
		if account := e.IdentityServiceAccount; account != nil {
			if account.UID == "" || account.Namespace != e.Namespace.Name || account.Name != main.Template.Spec.ServiceAccountName ||
				!slices.ContainsFunc(e.Sources, func(source SourceReference) bool {
					return source.APIVersion == "v1" && source.Kind == "ServiceAccount" && source.Namespace == account.Namespace &&
						source.Name == account.Name && source.UID == account.UID
				}) || !slices.ContainsFunc(e.Sources, func(source SourceReference) bool {
				return source.APIVersion == "v1" && source.Kind == "ConfigMap" && source.Namespace != e.Namespace.Name &&
					string(source.UID) == account.Annotations["cetirun.microsoft.com/sandbox-bootstrap"]
			}) {
				return fmt.Errorf("bootstrap identity requires its exact persisted ServiceAccount source")
			}
		}
	}
	controls := map[string]bool{}
	for _, object := range e.Controls {
		if account := e.IdentityServiceAccount; account != nil && object.GetAPIVersion() == "v1" &&
			object.GetKind() == "ServiceAccount" && object.GetName() == account.Name {
			return fmt.Errorf("bootstrap ServiceAccount cannot also be chart owned")
		}
		key := SourceKey(SourceReference{APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName()})
		kind := object.GetAPIVersion() + "/" + object.GetKind()
		kubernetesBinding := kind == "rbac.authorization.k8s.io/v1/RoleBinding" || kind == "rbac.authorization.k8s.io/v1/ClusterRoleBinding"
		if controls[key] || object.GetName() == "" ||
			(!kubernetesBinding && object.GetNamespace() != e.Namespace.Name && !e.approvedChatTLSControl(object)) ||
			!equality.Semantic.DeepEqual(object.GetOwnerReferences(), []metav1.OwnerReference{AgentOwner(e.Agent.Name, e.Agent.UID)}) {
			return fmt.Errorf("Sandbox control inventory has ambiguous or foreign ownership")
		}

		controls[key] = true
		switch kind {
		case "rbac.authorization.k8s.io/v1/RoleBinding":
			if !KubernetesBindingResource(object) && e.Terminal == nil {
				return fmt.Errorf("Sandbox terminal RoleBindings require a terminal target")
			}
		case "v1/ConfigMap", "v1/Service", "v1/ServiceAccount", "networking.k8s.io/v1/NetworkPolicy", "cilium.io/v2/CiliumNetworkPolicy",
			"rbac.authorization.k8s.io/v1/ClusterRoleBinding",
			"networking.istio.io/v1beta1/DestinationRule":
		default:
			return fmt.Errorf("Sandbox control inventory contains an unsupported resource")
		}
		required[object.GetKind()] = true
	}
	if _, err := e.KubernetesBindings(); err != nil {
		return err
	}
	for _, kind := range []string{"ServiceAccount", "CiliumNetworkPolicy"} {
		if !required[kind] {
			return fmt.Errorf("Sandbox control inventory is missing %s", kind)
		}
	}
	if terminal := e.Terminal; terminal != nil {
		for _, source := range []SourceReference{
			{APIVersion: "v1", Kind: "ServiceAccount", Namespace: e.Namespace.Name, Name: terminal.ServiceAccountName},
			{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding", Namespace: e.Namespace.Name, Name: terminal.ServiceAccountName},
			{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding", Namespace: e.Namespace.Name, Name: terminal.ServiceAccountName + "-exec"},
		} {
			if !controls[SourceKey(source)] {
				return fmt.Errorf("Sandbox terminal identity controls are absent")
			}
		}
	}
	for _, workload := range e.Workloads {
		for _, source := range PodSourceReferences(workload.Template.Spec, e.Namespace.Name) {
			if !sources[SourceKey(source)] && !controls[SourceKey(source)] {
				return fmt.Errorf("Sandbox workload references an unrecorded %s", source.Kind)
			}
		}
	}
	return nil
}

func ValidateReconciledVolumes(spec corev1.PodSpec, claims []string, projections []corev1.Volume, scratchLimit resource.Quantity) error {
	for _, volume := range spec.Volumes {
		switch {
		case volume.PersistentVolumeClaim != nil:
			if volume.PersistentVolumeClaim.ClaimName == "" || !slices.Contains(claims, volume.PersistentVolumeClaim.ClaimName) {
				return fmt.Errorf("Sandbox component cannot mount unapproved persistent storage")
			}
		case volume.Projected != nil:
			for _, source := range volume.Projected.Sources {
				if source.Secret != nil || source.ConfigMap != nil || source.DownwardAPI != nil {
					continue
				}
				if source.ServiceAccountToken == nil || !slices.ContainsFunc(projections, func(approved corev1.Volume) bool {
					return equality.Semantic.DeepEqual(volume, approved)
				}) {
					return fmt.Errorf("Sandbox component cannot request an unapproved identity projection")
				}
			}
		case volume.EmptyDir != nil:
			if volume.EmptyDir.Medium == corev1.StorageMediumDefault &&
				(scratchLimit.Sign() <= 0 || volume.EmptyDir.SizeLimit == nil ||
					volume.EmptyDir.SizeLimit.Sign() <= 0 || volume.EmptyDir.SizeLimit.Cmp(scratchLimit) > 0) {
				return fmt.Errorf("Sandbox temporary volume %q requires a positive sizeLimit no greater than the selected scratch limit", volume.Name)
			}
		case volume.ConfigMap != nil, volume.Secret != nil, volume.DownwardAPI != nil, volume.Image != nil:
		default:
			return fmt.Errorf("Sandbox component volume source is not approved")
		}
	}
	return nil
}

func ReconciledObjectMatches(actual, desired *unstructured.Unstructured) bool {
	return actual != nil && actual.GetUID() != "" && ReconciledObjectContentMatches(actual, desired)
}

// ReconciledObjectContentMatches also validates an object before the API server
// assigns its UID. Persisted-object checks must use ReconciledObjectMatches.
func ReconciledObjectContentMatches(actual, desired *unstructured.Unstructured) bool {
	if actual == nil || desired == nil || actual.GetName() != desired.GetName() ||
		actual.GetNamespace() != desired.GetNamespace() || !actual.GetDeletionTimestamp().IsZero() ||
		!equality.Semantic.DeepEqual(actual.GetOwnerReferences(), desired.GetOwnerReferences()) {
		return false
	}
	for key, value := range desired.GetLabels() {
		if actual.GetLabels()[key] != value {
			return false
		}
	}
	for key, value := range desired.GetAnnotations() {
		if actual.GetAnnotations()[key] != value {
			return false
		}
	}
	if desired.GetAPIVersion() == "v1" && desired.GetKind() == "ServiceAccount" {
		for _, key := range []string{"secrets", "imagePullSecrets"} {
			if !reflect.DeepEqual(actual.Object[key], desired.Object[key]) {
				return false
			}
		}
	}
	for key, value := range desired.Object {
		if key == "spec" && desired.GetKind() == "Service" {
			want, _, wantErr := unstructured.NestedMap(desired.Object, "spec")
			got, _, gotErr := unstructured.NestedMap(actual.Object, "spec")
			if wantErr != nil || gotErr != nil {
				return false
			}
			for name, field := range want {
				if !reflect.DeepEqual(field, got[name]) {
					return false
				}
			}
			continue
		}
		if key != "metadata" && !reflect.DeepEqual(actual.Object[key], value) {
			return false
		}
	}
	return true
}

// ValidateRenderedPodOnNode permits only scheduling and topology additions
// verified against the assigned Node, retaining the approved template constraints.
func ValidateRenderedPodOnNode(pod *corev1.Pod, node *corev1.Node, workload RenderedWorkload, owner metav1.OwnerReference, revision string) error {
	template, err := podTemplateOnNode(pod, node, workload.Template)
	if err != nil {
		return err
	}
	workload.Template = template
	return ValidateRenderedPod(pod, workload, owner, revision)
}

// ValidateRenderedPod checks the current controller-approved template without
// fixing helper names or permanently consuming one Pod UID.
func ValidateRenderedPod(pod *corev1.Pod, workload RenderedWorkload, owner metav1.OwnerReference, revision string) error {
	if pod == nil || !equality.Semantic.DeepEqual(pod.OwnerReferences, []metav1.OwnerReference{owner}) ||
		!equalJSON(pod.Annotations, workload.Template.Annotations) ||
		!slices.Equal(pod.Finalizers, workload.Template.Finalizers) {
		return &PodMismatchError{[]string{"metadata"}}
	}
	labels := maps.Clone(workload.Template.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	want := workload.Template.Spec.DeepCopy()
	switch workload.Kind {
	case "StatefulSet":
		name := workload.Identity.Name + "-0"
		if pod.Name != name {
			return &PodMismatchError{[]string{"metadata.name"}}
		}
		labels["statefulset.kubernetes.io/pod-name"], labels["apps.kubernetes.io/pod-index"] = name, "0"
		if revision == "" {
			return fmt.Errorf("Sandbox StatefulSet controller revision is unavailable")
		}
		labels["controller-revision-hash"] = revision
		want.Hostname, want.Subdomain = name, workload.Identity.Name
	case "Deployment":
		if revision == "" {
			return fmt.Errorf("Sandbox ReplicaSet revision is unavailable")
		}
		labels["pod-template-hash"] = revision
	default:
		return fmt.Errorf("unsupported rendered workload kind")
	}
	if !equalJSON(pod.Labels, labels) {
		return &PodMismatchError{[]string{"metadata.labels"}}
	}
	want.Overhead = workload.Overhead.DeepCopy()
	got := pod.Spec.DeepCopy()
	if err := defaultPod(want); err != nil {
		return err
	}
	if err := defaultPod(got); err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(want, got) {
		var fields []string
		podDifferencePaths(reflect.ValueOf(want), reflect.ValueOf(got), "spec", &fields)
		return &PodMismatchError{fields}
	}
	return nil
}

func (b *Bundle) ValidateReconciledTemplate(template *corev1.PodTemplateSpec, image string, memoryEnabled bool, apiPort *int32) error {
	spec := template.Spec
	if err := b.validateHermesTemplate(spec, apiPort); err != nil {
		return err
	}
	if err := b.validateEgressTemplate(template); err != nil {
		return err
	}
	if err := b.validateIngressTemplate(spec); err != nil {
		return err
	}
	if plan := b.input.Controls.IngressMTLS; plan != nil && b.input.Controls.EgressSNI == nil {
		digest, err := plan.ConfigDigest()
		if err != nil || template.Annotations[ingressmtls.ConfigDigestAnnotation] != digest {
			return fmt.Errorf("mTLS static configuration requires its rollout checksum")
		}
	}
	if err := b.ValidateIdentityProjection(spec); err != nil {
		return err
	}
	if !b.input.Reconciled || len(spec.Containers) == 0 || spec.Containers[0].Image != image ||
		ptr.Deref(spec.RuntimeClassName, "") != b.input.RuntimeClass.Name ||
		spec.HostNetwork || spec.HostPID || spec.HostIPC || ptr.Deref(spec.ShareProcessNamespace, false) ||
		ptr.Deref(spec.AutomountServiceAccountToken, true) || spec.SecurityContext == nil || spec.DNSPolicy != corev1.DNSNone ||
		!equality.Semantic.DeepEqual(spec.DNSConfig, b.DNSConfig()) ||
		spec.ServiceAccountName != b.input.Controls.ServiceAccountName ||
		!equality.Semantic.DeepEqual(spec.Containers[0].Resources, b.agentResources()) {
		return fmt.Errorf("reconciled Sandbox runtime, DNS, identity or resources differ from required controls")
	}
	foundHome := false
	for _, volume := range spec.Volumes {
		if volume.HostPath != nil {
			return fmt.Errorf("reconciled Sandbox cannot mount host paths")
		}
		if volume.Name == "data" {
			foundHome = volume.PersistentVolumeClaim != nil &&
				volume.PersistentVolumeClaim.ClaimName == b.input.Home.Name && !volume.PersistentVolumeClaim.ReadOnly
		}
	}
	if !foundHome {
		return fmt.Errorf("reconciled Sandbox must retain its owned HOME")
	}
	projections := slices.Clone(b.input.Controls.Volumes)
	if b.input.ConnectionsMCP != nil {
		projections = append(projections, b.input.ConnectionsMCP.TokenVolume())
		if err := b.input.ConnectionsMCP.ValidatePod(spec); err != nil {
			return err
		}
	}
	if err := ValidateReconciledVolumes(spec, []string{b.input.Home.Name, skillsIdentity(b.input.Skills).Name}, projections,
		b.input.Resolution.Sandbox.Storage.Scratch.SizeLimit); err != nil {
		return err
	}
	if err := b.validateReconciledIdentity(spec); err != nil {
		return err
	}
	var pullSecrets []corev1.LocalObjectReference
	if b.input.ImagePullSecret != nil {
		pullSecrets = []corev1.LocalObjectReference{{Name: b.input.ImagePullSecret.Name}}
	}
	if !slices.Equal(spec.ImagePullSecrets, pullSecrets) {
		return fmt.Errorf("reconciled Sandbox image pulls differ from the approved Secret binding")
	}
	// Kubelet pull credentials must not authorize container environment or mounts.
	runtimeSources := spec
	runtimeSources.ImagePullSecrets = nil
	for _, source := range PodSourceReferences(runtimeSources, b.input.Namespace.Name) {
		endpointSecret := b.input.Controls.IngressMTLS != nil && source.Name == b.input.Controls.IngressMTLS.ServerSecret.Name
		if source.Kind == "Secret" && !slices.Contains(b.input.AuthorizedSecrets, source.Name) && !endpointSecret {
			return fmt.Errorf("reconciled Agent cannot consume an unauthorized Secret")
		}
		if source.Kind == "PersistentVolumeClaim" && source.Name != b.input.Home.Name && source.Name != skillsIdentity(b.input.Skills).Name {
			return fmt.Errorf("reconciled Agent cannot mount unapproved persistent storage")
		}
	}
	for _, group := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for _, container := range group {
			if b.input.Controls.EgressSNI != nil && container.Name == b.captureContainer().Name {
				// The exact placement, image, argv, mounts and security context were
				// checked above; this is the sole root/NET_ADMIN exception.
				continue
			}
			sc := container.SecurityContext
			imageUser := b.input.CustomAgent != nil && sc != nil && sc.RunAsUser == nil && spec.SecurityContext.RunAsUser == nil
			if sc == nil || ptr.Deref(sc.Privileged, false) || ptr.Deref(sc.AllowPrivilegeEscalation, true) ||
				!ptr.Deref(sc.RunAsNonRoot, ptr.Deref(spec.SecurityContext.RunAsNonRoot, false)) ||
				!imageUser && ptr.Deref(sc.RunAsUser, ptr.Deref(spec.SecurityContext.RunAsUser, 0)) <= 0 ||
				sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 || !slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL")) ||
				!ptr.Deref(sc.ReadOnlyRootFilesystem, false) && *b.input.Resolution.AgentPolicy.Security.ReadOnlyRootFilesystem {
				return fmt.Errorf("reconciled Sandbox container violates the selected security constraints")
			}
		}
	}
	if err := b.validateSkillMarketplaceTemplate(spec); err != nil {
		return err
	}
	if b.input.CustomAgent != nil {
		return b.validateCustomReconciledTemplate(&spec, memoryEnabled)
	}
	return nil
}
