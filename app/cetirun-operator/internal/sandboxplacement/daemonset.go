// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	daemonGenerationLabel      = "pod-template-generation"
	daemonGenerationAnnotation = appsv1.DeprecatedTemplateGeneration
)

// CanonicalDaemonSetDigest hashes the typed selector and complete Pod template
// of the current API-server-defaulted DaemonSet. Callers must register the same
// persisted object, not a locally authored template before API defaulting.
func CanonicalDaemonSetDigest(daemon *appsv1.DaemonSet) (string, error) {
	if daemon == nil || daemon.Spec.Selector == nil || len(daemon.Spec.Template.Spec.Containers) == 0 {
		return "", fmt.Errorf("DaemonSet digest requires a selector and Pod template")
	}
	selector, err := metav1.LabelSelectorAsSelector(daemon.Spec.Selector)
	if err != nil || selector.Empty() || !selector.Matches(labels.Set(daemon.Spec.Template.Labels)) {
		return "", fmt.Errorf("DaemonSet selector must match its template")
	}
	raw, err := json.Marshal(struct {
		Selector *metav1.LabelSelector  `json:"selector"`
		Template corev1.PodTemplateSpec `json:"template"`
	}{Selector: daemon.Spec.Selector, Template: daemon.Spec.Template})
	if err != nil {
		return "", fmt.Errorf("DaemonSet selector and template cannot be encoded")
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (r *registry) daemonForPod(pod *corev1.Pod) (*registeredDaemonSet, *failure) {
	var match *registeredDaemonSet
	for i := range r.PlatformDaemonSets {
		daemon := &r.PlatformDaemonSets[i]
		claimed := false
		for _, owner := range pod.OwnerReferences {
			if owner.UID == daemon.UID || (owner.Kind == "DaemonSet" && owner.Name == daemon.Name && pod.Namespace == daemon.Namespace.Name) {
				claimed = true
			}
		}
		for _, enrolled := range daemon.BootstrapPods {
			if (pod.UID != "" && pod.UID == enrolled.UID) || (pod.Namespace == daemon.Namespace.Name && pod.Name == enrolled.Name) {
				claimed = true
			}
		}
		if claimed {
			if match != nil {
				return nil, denied("DaemonSetClaimAmbiguous", "Pod claims more than one registered DaemonSet")
			}
			match = daemon
		}
	}
	return match, nil
}

func (d *registeredDaemonSet) enrolled(pod *corev1.Pod) bool {
	if pod == nil || pod.Namespace != d.Namespace.Name || pod.UID == "" || pod.ResourceVersion == "" {
		return false
	}
	for _, registered := range d.BootstrapPods {
		if registered.Name == pod.Name && registered.UID == pod.UID {
			return true
		}
	}
	return false
}

func (s *requestState) validateDaemonPod(ctx context.Context, registry *registry, registered *registeredDaemonSet, pod *corev1.Pod) *failure {
	if pod == nil || pod.Namespace != registered.Namespace.Name {
		return denied("DaemonSetNamespaceMismatch", "platform Pod differs from the registered DaemonSet namespace")
	}
	owner := metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "DaemonSet", Name: registered.Name, UID: registered.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
	if !daemonPodNameMatches(pod, registered.Name) || len(pod.Finalizers) != 0 ||
		!equality.Semantic.DeepEqual(pod.OwnerReferences, []metav1.OwnerReference{owner}) {
		return denied("DaemonSetOwnerMismatch", "platform Pod requires the exact registered controller owner and generated identity")
	}
	namespace := &corev1.Namespace{}
	if failure := s.read(ctx, client.ObjectKey{Name: registered.Namespace.Name}, namespace, "Namespace"); failure != nil {
		return failure
	}
	if namespace.UID != registered.Namespace.UID {
		return denied("DaemonSetNamespaceMismatch", "platform namespace UID differs from its registration")
	}
	daemon := &appsv1.DaemonSet{}
	if failure := s.read(ctx, client.ObjectKey{Namespace: registered.Namespace.Name, Name: registered.Name}, daemon, "DaemonSet"); failure != nil {
		return failure
	}
	digest, err := CanonicalDaemonSetDigest(daemon)
	if err != nil || daemon.UID != registered.UID || digest != registered.TemplateDigest {
		return denied("DaemonSetTemplateChanged", "current DaemonSet UID, selector or template differs from its registration")
	}
	target := daemonTarget(pod)
	if target == "" || (pod.Spec.NodeName != "" && pod.Spec.NodeName != target) {
		return denied("DaemonSetSchedulingMismatch", "platform Pod requires one exact controller-generated Node affinity target")
	}
	node, _, failure := s.readNode(ctx, registry, target)
	if failure != nil {
		return failure
	}
	if !NodeMatchesSpec(node, &daemon.Spec.Template.Spec) ||
		(daemon.Spec.Template.Spec.NodeName != "" && daemon.Spec.Template.Spec.NodeName != target) {
		return denied("DaemonSetSchedulingMismatch", "registered Node does not satisfy the DaemonSet template's scheduling intent")
	}
	template := daemon.Spec.Template.DeepCopy()
	if !equality.Semantic.DeepEqual(template.ObjectMeta, metav1.ObjectMeta{Labels: template.Labels, Annotations: template.Annotations}) {
		return denied("DaemonSetTemplateInvalid", "platform template metadata must contain only labels and annotations")
	}
	if _, found := template.Labels[appsv1.ControllerRevisionHashLabelKey]; found {
		return denied("DaemonSetTemplateInvalid", "platform template cannot preselect a controller revision label")
	}
	if _, found := template.Labels[daemonGenerationLabel]; found {
		return denied("DaemonSetTemplateInvalid", "platform template cannot preselect a generated template label")
	}
	revision := pod.Labels[appsv1.ControllerRevisionHashLabelKey]
	if revision == "" || len(validation.IsValidLabelValue(revision)) != 0 {
		return denied("DaemonSetTemplateMismatch", "platform Pod requires a controller revision label")
	}
	if template.Labels == nil {
		template.Labels = map[string]string{}
	}
	template.Labels[appsv1.ControllerRevisionHashLabelKey] = revision
	if generation, found := daemon.Annotations[daemonGenerationAnnotation]; found {
		value, err := strconv.ParseInt(generation, 10, 64)
		if err != nil || value < 0 || strconv.FormatInt(value, 10) != generation {
			return denied("DaemonSetTemplateInvalid", "DaemonSet template generation annotation is invalid")
		}
		template.Labels[daemonGenerationLabel] = generation
	}
	if !applyNodeTopology(pod, node, template) {
		return denied("DaemonSetTopologyMismatch", "platform Pod topology labels differ from the current pinned Node")
	}
	template.Spec.NodeName = pod.Spec.NodeName
	if template.Spec.ServiceAccountName == "" {
		template.Spec.ServiceAccountName = "default"
	}
	if template.Spec.Affinity == nil {
		template.Spec.Affinity = &corev1.Affinity{}
	}
	if template.Spec.Affinity.NodeAffinity == nil {
		template.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = targetAffinity(target)
	addDaemonTolerations(&template.Spec)
	if failure := s.applyDaemonPriority(ctx, pod, &template.Spec); failure != nil {
		return failure
	}
	defaultDaemonResourceRequests(&template.Spec)
	actual := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: pod.Labels, Annotations: pod.Annotations},
		Spec:       *pod.Spec.DeepCopy(),
	}
	// Kubernetes defaults this field on Pods, not on embedded PodTemplates.
	if template.Spec.EnableServiceLinks == nil {
		template.Spec.EnableServiceLinks = ptr.To(corev1.DefaultEnableServiceLinks)
	}
	if actual.Spec.EnableServiceLinks == nil {
		actual.Spec.EnableServiceLinks = ptr.To(corev1.DefaultEnableServiceLinks)
	}
	if !sandboxworkload.SamePodTemplate(actual, *template) {
		return denied("DaemonSetTemplateMismatch", "platform Pod fields differ from the current registered DaemonSet template")
	}
	account := &corev1.ServiceAccount{}
	return s.read(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: template.Spec.ServiceAccountName}, account, "ServiceAccount")
}

func daemonPodGenerateName(daemonName string) string {
	prefix := daemonName + "-"
	// Match Kubernetes controller.getPodsPrefix, including prefix validation
	// rather than a length check on the unmasked trailing dash.
	if len(apivalidation.NameIsDNSSubdomain(prefix, true)) != 0 {
		return daemonName
	}
	return prefix
}

func daemonPodNameMatches(pod *corev1.Pod, daemonName string) bool {
	prefix := daemonPodGenerateName(daemonName)
	if pod.GenerateName != "" && pod.GenerateName != prefix {
		return false
	}
	prefix = prefix[:min(len(prefix), names.MaxGeneratedNameLength)]
	// SimpleNameGenerator appends five characters from util/rand's alphabet;
	// only the generated name, not metadata.generateName, is truncated.
	return validName(pod.Name) && len(pod.Name) == len(prefix)+5 && strings.HasPrefix(pod.Name, prefix) &&
		strings.Trim(pod.Name[len(prefix):], "bcdfghjklmnpqrstvwxz2456789") == ""
}

func (s *requestState) applyDaemonPriority(ctx context.Context, pod *corev1.Pod, template *corev1.PodSpec) *failure {
	if template.PriorityClassName == "" {
		return nil
	}
	if !validName(template.PriorityClassName) || pod.Spec.PriorityClassName != template.PriorityClassName {
		return denied("DaemonSetPriorityMismatch", "platform Pod must retain the template's explicit PriorityClass")
	}
	class := &schedulingv1.PriorityClass{}
	if failure := s.read(ctx, client.ObjectKey{Name: template.PriorityClassName}, class, "PriorityClass"); failure != nil {
		return failure
	}
	policy := ptr.Deref(class.PreemptionPolicy, corev1.PreemptLowerPriority)
	if policy != corev1.PreemptLowerPriority && policy != corev1.PreemptNever {
		return denied("PriorityClassInvalid", "platform PriorityClass has an unsupported preemption policy")
	}
	if (template.Priority != nil && *template.Priority != class.Value) ||
		(template.PreemptionPolicy != nil && *template.PreemptionPolicy != policy) ||
		pod.Spec.Priority == nil || *pod.Spec.Priority != class.Value ||
		pod.Spec.PreemptionPolicy == nil || *pod.Spec.PreemptionPolicy != policy {
		return denied("DaemonSetPriorityMismatch", "platform Pod or explicit template priority fields differ from the current PriorityClass")
	}
	// Only supply Pod-only admission fields from freshly read authority.
	template.Priority, template.PreemptionPolicy = ptr.To(class.Value), ptr.To(policy)
	return nil
}

func defaultDaemonResourceRequests(spec *corev1.PodSpec) {
	// Kubernetes derives missing requests from limits on Pods, not templates.
	for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for i := range containers {
			resources := &containers[i].Resources
			if len(resources.Limits) == 0 {
				continue
			}
			if resources.Requests == nil {
				resources.Requests = corev1.ResourceList{}
			}
			for name, limit := range resources.Limits {
				if _, explicit := resources.Requests[name]; !explicit {
					resources.Requests[name] = limit.DeepCopy()
				}
			}
		}
	}
}

func daemonTargetsNode(pod *corev1.Pod, name string) *failure {
	if daemonTarget(pod) != name {
		return denied("DaemonSetSchedulingMismatch", "binding destination differs from the platform Pod's exact Node affinity target")
	}
	return nil
}

func daemonTarget(pod *corev1.Pod) string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 0 || len(terms[0].MatchFields) != 1 {
		return ""
	}
	field := terms[0].MatchFields[0]
	if field.Key != "metadata.name" || field.Operator != corev1.NodeSelectorOpIn ||
		len(field.Values) != 1 || !validName(field.Values[0]) {
		return ""
	}
	return field.Values[0]
}

func targetAffinity(node string) *corev1.NodeSelector {
	return &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
	}}}
}

func addDaemonTolerations(spec *corev1.PodSpec) {
	for _, entry := range []struct {
		key    string
		effect corev1.TaintEffect
	}{
		{corev1.TaintNodeNotReady, corev1.TaintEffectNoExecute},
		{corev1.TaintNodeUnreachable, corev1.TaintEffectNoExecute},
		{corev1.TaintNodeDiskPressure, corev1.TaintEffectNoSchedule},
		{corev1.TaintNodeMemoryPressure, corev1.TaintEffectNoSchedule},
		{corev1.TaintNodePIDPressure, corev1.TaintEffectNoSchedule},
		{corev1.TaintNodeUnschedulable, corev1.TaintEffectNoSchedule},
		{corev1.TaintNodeNetworkUnavailable, corev1.TaintEffectNoSchedule},
	} {
		if entry.key == corev1.TaintNodeNetworkUnavailable && !spec.HostNetwork {
			continue
		}
		toleration := corev1.Toleration{Key: entry.key, Operator: corev1.TolerationOpExists, Effect: entry.effect}
		found := false
		for i := range spec.Tolerations {
			if spec.Tolerations[i].MatchToleration(&toleration) {
				spec.Tolerations[i], found = toleration, true
				break
			}
		}
		if !found {
			spec.Tolerations = append(spec.Tolerations, toleration)
		}
	}
}

// NodeMatchesSpec checks node selectors and required node affinity, not health,
// taints, storage compatibility, or permission to bind a Pod.
func NodeMatchesSpec(node *corev1.Node, spec *corev1.PodSpec) bool {
	for key, value := range spec.NodeSelector {
		if actual, found := node.Labels[key]; !found || actual != value {
			return false
		}
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil ||
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}
	for _, term := range spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if len(term.MatchExpressions)+len(term.MatchFields) == 0 {
			continue
		}
		matches := true
		for _, requirement := range term.MatchExpressions {
			matches = matches && matchesRequirement(labels.Set(node.Labels), requirement)
		}
		for _, requirement := range term.MatchFields {
			if requirement.Key != "metadata.name" || len(requirement.Values) != 1 {
				matches = false
				break
			}
			var selector fields.Selector
			switch requirement.Operator {
			case corev1.NodeSelectorOpIn:
				selector = fields.OneTermEqualSelector(requirement.Key, requirement.Values[0])
			case corev1.NodeSelectorOpNotIn:
				selector = fields.OneTermNotEqualSelector(requirement.Key, requirement.Values[0])
			default:
				matches = false
				continue
			}
			matches = matches && selector.Matches(fields.Set{"metadata.name": node.Name})
		}
		if matches {
			return true
		}
	}
	return false
}

func matchesRequirement(values labels.Set, requirement corev1.NodeSelectorRequirement) bool {
	operators := map[corev1.NodeSelectorOperator]selection.Operator{
		corev1.NodeSelectorOpIn: selection.In, corev1.NodeSelectorOpNotIn: selection.NotIn,
		corev1.NodeSelectorOpExists: selection.Exists, corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist,
		corev1.NodeSelectorOpGt: selection.GreaterThan, corev1.NodeSelectorOpLt: selection.LessThan,
	}
	operator, found := operators[requirement.Operator]
	if !found {
		return false
	}
	selector, err := labels.NewRequirement(requirement.Key, operator, requirement.Values)
	return err == nil && selector.Matches(values)
}
