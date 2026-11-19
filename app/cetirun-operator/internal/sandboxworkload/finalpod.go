// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/Azure/CetiRun/app/internal/podtemplate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// PodMismatchError exposes bounded field paths only, never either Pod's values.
type PodMismatchError struct{ Fields []string }

func (e *PodMismatchError) Error() string {
	return "final Pod differs from the protected template at " + strings.Join(e.Fields, ", ")
}

// ValidatePod compares every final PodSpec field, not just selected security
// fields, with a controller-owned chart-rendered expectation. Only documented
// Kubernetes and StatefulSet defaults are normalized. It makes no lifecycle or
// authorization decision; callers must freshly verify persisted ownership.
func ValidatePod(pod *corev1.Pod, expected Expectation) error {
	if pod == nil || expected.Version != ExpectationVersion || expected.WorkloadKind != AgentWorkload ||
		expected.Workload.Name == "" || expected.Workload.UID == "" || expected.Agent.UID == "" ||
		expected.Namespace.UID == "" || expected.Revision == "" || expected.Binding.AgentUID != expected.Agent.UID ||
		expected.Template.Annotations[KindAnnotation] != AgentWorkload ||
		expected.Template.Annotations[AgentUIDAnnotation] != string(expected.Agent.UID) ||
		expected.Template.Annotations[RevisionAnnotation] != expected.Revision ||
		expected.Template.Annotations["sidecar.istio.io/inject"] != "false" {
		return fmt.Errorf("a complete Agent workload expectation is required; helper classification is not supported")
	}
	if expected.SinglePod && expected.PodUID != "" && pod.UID != expected.PodUID {
		return &PodMismatchError{[]string{"metadata.uid"}}
	}
	if err := expected.ValidateSkills(); err != nil {
		return err
	}
	if expected.Placement != nil {
		if err := expected.Placement.MatchesExpectation(expected); err != nil {
			return err
		}
		if pod.Spec.NodeName != "" && !expected.Placement.containsNode(pod.Spec.NodeName) {
			return &PodMismatchError{[]string{"spec.nodeName"}}
		}
	}
	if err := validateExpectedInventory(expected.Template.Spec); err != nil {
		return err
	}
	name := expected.Workload.Name + "-0"
	if pod.Name != name || pod.Namespace != expected.Namespace.Name ||
		(pod.GenerateName != "" && pod.GenerateName != expected.Workload.Name+"-") {
		return fmt.Errorf("Pod does not match the expected single-Agent workload")
	}
	owner := metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "StatefulSet", Name: expected.Workload.Name, UID: expected.Workload.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
	if !equality.Semantic.DeepEqual(pod.OwnerReferences, []metav1.OwnerReference{owner}) {
		return &PodMismatchError{[]string{"metadata.ownerReferences"}}
	}
	finalizersValid := len(pod.Finalizers) == 0 && !expected.SinglePod
	if expected.SinglePod && slices.Equal(expected.Template.Finalizers, []string{NativePodFinalizer}) {
		finalizersValid = slices.Equal(pod.Finalizers, []string{NativePodFinalizer}) ||
			(expected.PodUID == pod.UID && expected.PodUID != "" && len(pod.Finalizers) == 0)
	}
	if !finalizersValid {
		return &PodMismatchError{[]string{"metadata.finalizers"}}
	}
	if !equalJSON(pod.Annotations, expected.Template.Annotations) {
		return &PodMismatchError{[]string{"metadata.annotations"}}
	}
	labels := map[string]string{}
	for key, value := range expected.Template.Labels {
		labels[key] = value
	}
	labels["statefulset.kubernetes.io/pod-name"] = name
	labels["apps.kubernetes.io/pod-index"] = "0"
	if expected.ControllerRevision != "" {
		labels["controller-revision-hash"] = expected.ControllerRevision
	}
	if !equality.Semantic.DeepEqual(pod.Labels, labels) {
		return &PodMismatchError{[]string{"metadata.labels"}}
	}
	want := expected.Template.Spec.DeepCopy()
	want.Hostname, want.Subdomain, want.Overhead = name, expected.Workload.Name, expected.Overhead.DeepCopy()
	got := pod.Spec.DeepCopy()
	if err := defaultPod(want); err != nil {
		return fmt.Errorf("normalize expected Pod: %w", err)
	}
	if err := defaultPod(got); err != nil {
		return fmt.Errorf("normalize final Pod: %w", err)
	}
	if !equality.Semantic.DeepEqual(want, got) {
		var fields []string
		podDifferencePaths(reflect.ValueOf(want), reflect.ValueOf(got), "spec", &fields)
		return &PodMismatchError{fields}
	}
	return nil
}

// ValidatePodOnNode permits only scheduling and topology labels verified against
// the assigned Node. It does not supply independent runtime or fencing evidence.
func ValidatePodOnNode(pod *corev1.Pod, node *corev1.Node, expected Expectation) error {
	template, err := podTemplateOnNode(pod, node, expected.Template)
	if err != nil {
		return err
	}
	expected.Template = template
	return ValidatePod(pod, expected)
}

func podTemplateOnNode(pod *corev1.Pod, node *corev1.Node, template corev1.PodTemplateSpec) (corev1.PodTemplateSpec, error) {
	if pod == nil || node == nil || node.Name == "" || pod.Spec.NodeName != node.Name {
		return corev1.PodTemplateSpec{}, &PodMismatchError{[]string{"spec.nodeName"}}
	}
	template = *template.DeepCopy()
	if template.Spec.NodeName == "" {
		template.Spec.NodeName = node.Name
	}
	for _, key := range []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone} {
		value, present := pod.Labels[key]
		if !present {
			continue
		}
		current, found := node.Labels[key]
		if !found || value != current {
			return corev1.PodTemplateSpec{}, &PodMismatchError{[]string{"metadata.labels"}}
		}
		if _, pinned := template.Labels[key]; !pinned {
			if template.Labels == nil {
				template.Labels = map[string]string{}
			}
			template.Labels[key] = current
		}
	}
	return template, nil
}

func podDifferencePaths(a, b reflect.Value, path string, fields *[]string) {
	if len(*fields) >= 8 || equality.Semantic.DeepEqual(a.Interface(), b.Interface()) {
		return
	}
	switch a.Kind() {
	case reflect.Pointer:
		if !a.IsNil() && !b.IsNil() {
			podDifferencePaths(a.Elem(), b.Elem(), path, fields)
			return
		}
	case reflect.Struct:
		if !a.Type().Implements(reflect.TypeFor[json.Marshaler]()) &&
			!reflect.PointerTo(a.Type()).Implements(reflect.TypeFor[json.Marshaler]()) {
			before := len(*fields)
			for i := range a.NumField() {
				field := a.Type().Field(i)
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				if field.IsExported() && name != "" && name != "-" {
					podDifferencePaths(a.Field(i), b.Field(i), path+"."+name, fields)
				}
			}
			if len(*fields) != before {
				return
			}
		}
	case reflect.Slice:
		if a.Len() == b.Len() && a.IsNil() == b.IsNil() {
			for i := range a.Len() {
				podDifferencePaths(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i), fields)
			}
			return
		}
	}
	// Maps are leaves: user-controlled map keys are not diagnostic field names.
	*fields = append(*fields, path)
}

func validateExpectedInventory(spec corev1.PodSpec) error {
	if ptr.Deref(spec.RuntimeClassName, "") == "" || spec.Resources == nil || spec.HostNetwork || spec.HostPID || spec.HostIPC ||
		spec.DNSPolicy != corev1.DNSNone || spec.DNSConfig == nil ||
		ptr.Deref(spec.ShareProcessNamespace, false) || spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
		len(spec.EphemeralContainers) != 0 || len(spec.Containers) == 0 || spec.Containers[0].Name != "cetirun" ||
		len(spec.InitContainers) == 0 || spec.InitContainers[0].Name != "init-home-data" {
		return fmt.Errorf("controller expectation is not a protected native workload")
	}
	if err := validateDNSIPs(spec.DNSConfig.Nameservers); err != nil {
		return err
	}
	if len(spec.DNSConfig.Searches) != 0 || len(spec.DNSConfig.Options) != 0 {
		return fmt.Errorf("controller expectation contains unsupported DNS search or option overrides")
	}
	seen := map[string]bool{}
	for _, group := range []struct {
		containers []corev1.Container
		allowed    []string
	}{
		{spec.Containers, []string{"cetirun", "logger", "desktop"}},
		{spec.InitContainers, []string{"init-home-data", "doctor", "retire-copilot-sign-in", "retire-direct-model-auth"}},
	} {
		for _, container := range group.containers {
			sc := container.SecurityContext
			if seen[container.Name] || !slices.Contains(group.allowed, container.Name) || sc == nil ||
				!ptr.Deref(sc.RunAsNonRoot, false) || ptr.Deref(sc.RunAsUser, 0) <= 0 ||
				sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || ptr.Deref(sc.Privileged, false) ||
				sc.ReadOnlyRootFilesystem == nil || sc.Capabilities == nil ||
				!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0 ||
				sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				return fmt.Errorf("controller expectation contains an unapproved helper or security context")
			}
			for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
				request, limit := container.Resources.Requests[name], container.Resources.Limits[name]
				if request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
					return fmt.Errorf("controller expectation has an incomplete container resource budget")
				}
			}
			seen[container.Name] = true
		}
	}
	return nil
}

// PodSourceReferences lists names whose current UID/resourceVersion must be
// added to the controller's source snapshot. It never reads Secret contents.
func PodSourceReferences(spec corev1.PodSpec, namespace string) []SourceReference {
	seen := map[string]SourceReference{}
	add := func(kind, name string) {
		if name != "" {
			seen[kind+"/"+name] = SourceReference{APIVersion: "v1", Kind: kind, Namespace: namespace, Name: name}
		}
	}
	add("ServiceAccount", spec.ServiceAccountName)
	for _, secret := range spec.ImagePullSecrets {
		add("Secret", secret.Name)
	}
	for _, volume := range spec.Volumes {
		if volume.ConfigMap != nil {
			add("ConfigMap", volume.ConfigMap.Name)
		}
		if volume.Secret != nil {
			add("Secret", volume.Secret.SecretName)
		}
		if volume.PersistentVolumeClaim != nil {
			add("PersistentVolumeClaim", volume.PersistentVolumeClaim.ClaimName)
		}
		if volume.Projected != nil {
			for _, projection := range volume.Projected.Sources {
				if projection.ConfigMap != nil {
					add("ConfigMap", projection.ConfigMap.Name)
				}
				if projection.Secret != nil {
					add("Secret", projection.Secret.Name)
				}
			}
		}
	}
	for _, container := range append(slices.Clone(spec.Containers), spec.InitContainers...) {
		for _, env := range container.EnvFrom {
			if env.SecretRef != nil {
				add("Secret", env.SecretRef.Name)
			}
			if env.ConfigMapRef != nil {
				add("ConfigMap", env.ConfigMapRef.Name)
			}
		}
		for _, env := range container.Env {
			if env.ValueFrom == nil {
				continue
			}
			if env.ValueFrom.SecretKeyRef != nil {
				add("Secret", env.ValueFrom.SecretKeyRef.Name)
			}
			if env.ValueFrom.ConfigMapKeyRef != nil {
				add("ConfigMap", env.ValueFrom.ConfigMapKeyRef.Name)
			}
		}
	}
	refs := make([]SourceReference, 0, len(seen))
	for _, ref := range seen {
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b SourceReference) int { return strings.Compare(a.Kind+"/"+a.Name, b.Kind+"/"+b.Name) })
	return refs
}

func SamePodTemplate(actual, expected corev1.PodTemplateSpec) bool {
	a, b := actual.DeepCopy(), expected.DeepCopy()
	if defaultPod(&a.Spec) != nil || defaultPod(&b.Spec) != nil {
		return false
	}
	return equalJSON(a.Labels, b.Labels) && equalJSON(a.Annotations, b.Annotations) &&
		slices.Equal(a.Finalizers, b.Finalizers) && len(a.OwnerReferences) == 0 && equality.Semantic.DeepEqual(a.Spec, b.Spec)
}

func defaultPod(spec *corev1.PodSpec) error {
	if err := podtemplate.DefaultSpec(spec); err != nil {
		return err
	}
	if spec.Priority == nil {
		spec.Priority = ptr.To(int32(0))
	}
	if spec.PreemptionPolicy == nil {
		spec.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
	}
	for _, key := range []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"} {
		defaultToleration(spec, corev1.Toleration{
			Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300)),
		})
	}
	// TaintToleration admission adds this for every non-BestEffort Pod, not
	// just Pods with RuntimeClass scheduling constraints.
	budgets := []corev1.ResourceRequirements{AccountResources(*spec, nil).Pod}
	if spec.Resources != nil {
		budgets = append(budgets, *spec.Resources)
	}
	nonBestEffort := false
	for _, budget := range budgets {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			request, limit := budget.Requests[name], budget.Limits[name]
			nonBestEffort = nonBestEffort || request.Sign() > 0 || limit.Sign() > 0
		}
	}
	if nonBestEffort {
		defaultToleration(spec, corev1.Toleration{
			Key: "node.kubernetes.io/memory-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		})
	}
	slices.SortFunc(spec.Tolerations, func(a, b corev1.Toleration) int {
		return strings.Compare(a.Key+"/"+string(a.Effect)+"/"+string(a.Operator)+"/"+a.Value, b.Key+"/"+string(b.Effect)+"/"+string(b.Operator)+"/"+b.Value)
	})
	// JSON's omitempty makes zero-length slices/maps equivalent to omitted
	// fields, without discarding any non-empty or newly added Pod field.
	normalized, err := clone(*spec)
	if err != nil {
		return err
	}
	*spec = normalized
	return nil
}

func defaultToleration(spec *corev1.PodSpec, expected corev1.Toleration) {
	for _, toleration := range spec.Tolerations {
		if toleration.Key == expected.Key && toleration.Effect == expected.Effect {
			return
		}
	}
	spec.Tolerations = append(spec.Tolerations, expected)
}
