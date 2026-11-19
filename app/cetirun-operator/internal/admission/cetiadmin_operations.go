// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const nativeAuthRotationAnnotation = "cetirun.microsoft.com/native-auth-rotation-request-id"

var rotationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func (h *CetiAdminHandler) validateCetiAdminOperation(ctx context.Context, request admission.Request, binding cetiadmin.Binding, state *cetiadmin.Installation) admission.Response {
	if request.Resource.Group == api.GroupVersion.Group && request.Resource.Resource == "agents" {
		return h.validateCetiAdminAgent(ctx, request, binding, state)
	}
	agent, target, err := cetiadmin.ResolveRuntimeTarget(ctx, h.reader, binding.Config(), state.Record, "", request.Namespace)
	if err != nil {
		return admission.Denied("CetiAdmin may operate only on a current bound target namespace")
	}
	if request.Operation == admissionv1.Connect {
		if request.Resource.Group != "" || request.Resource.Resource != "pods" || request.SubResource != "exec" || target.Protected {
			return admission.Denied("CetiAdmin exec is limited to ordinary bound Agent Pods")
		}
		pod := &corev1.Pod{}
		if h.reader.Get(ctx, client.ObjectKey{Name: request.Name, Namespace: request.Namespace}, pod) != nil || !h.confirmedTargetPod(ctx, pod, agent) {
			return admission.Denied("CetiAdmin exec requires a current runtime Pod")
		}
		return admission.Allowed("current CetiAdmin exec; target execution policy still applies")
	}
	if request.SubResource != "" {
		return admission.Denied("CetiAdmin cannot write status, scale or finalizer subresources")
	}
	switch request.Resource.Group + "/" + request.Resource.Resource {
	case "/pods":
		var pod corev1.Pod
		if target.Protected || decodeJSON(request.OldObject.Raw, &pod, false) != nil || !h.confirmedTargetPod(ctx, &pod, agent) {
			return admission.Denied("Pod restart requires the current workload UID on a healthy source Node")
		}
		return cetiAdminPodDelete(request, agent)
	case "/persistentvolumeclaims":
		return h.cetiAdminPVCGrowth(ctx, request, agent, target)
	case "/events", "events.k8s.io/events":
		return cetiAdminEvent(request, agent)
	case "/secrets":
		if request.Name == agent.Spec.GatewayTokenSecretRef && !target.Protected {
			return cetiAdminNativeAuth(request, agent)
		}
	}
	return h.cetiAdminHelmWrite(ctx, request, agent, target)
}

func cetiAdminAgentOwner(object metav1.Object, agent *api.Agent) bool {
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == api.GroupVersion.String() && owner.Kind == "Agent" && owner.Name == agent.Name && owner.UID == agent.UID && owner.UID != "" {
			return true
		}
	}
	return false
}

func protectedCetiAdminTarget(agent *api.Agent) bool {
	return agent.Spec.SandboxRef != "" || agent.Status.Sandbox != nil || slices.Contains(agent.Finalizers, sandbox.HoldFinalizer)
}

func targetPod(pod *corev1.Pod, agent *api.Agent) bool {
	if pod.UID == "" || pod.DeletionTimestamp != nil || pod.Namespace != agent.Status.Namespace ||
		pod.Labels["app.kubernetes.io/instance"] != agent.Status.HelmReleaseName {
		return false
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "StatefulSet" && owner.APIVersion == "apps/v1" && owner.Name == "openclaw-"+agent.Name+"-cetirun" && owner.UID != "" {
			return true
		}
	}
	return false
}

func (h *CetiAdminHandler) confirmedTargetPod(ctx context.Context, pod *corev1.Pod, agent *api.Agent) bool {
	if !targetPod(pod, agent) || pod.Spec.NodeName == "" {
		return false
	}
	workload := &appsv1.StatefulSet{}
	if h.reader.Get(ctx, client.ObjectKey{Name: "openclaw-" + agent.Name + "-cetirun", Namespace: agent.Status.Namespace}, workload) != nil ||
		workload.DeletionTimestamp != nil || workload.Annotations["meta.helm.sh/release-name"] != agent.Status.HelmReleaseName ||
		workload.Annotations["meta.helm.sh/release-namespace"] != agent.Status.Namespace {
		return false
	}
	owned := false
	for _, owner := range pod.OwnerReferences {
		owned = owned || (owner.Kind == "StatefulSet" && owner.Name == workload.Name && owner.UID == workload.UID)
	}
	if !owned {
		return false
	}
	node := &corev1.Node{}
	if h.reader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node) != nil || node.DeletionTimestamp != nil {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (h *CetiAdminHandler) validateCetiAdminAgent(ctx context.Context, request admission.Request, binding cetiadmin.Binding, state *cetiadmin.Installation) admission.Response {
	if request.SubResource != "" || (request.Operation != admissionv1.Create && request.Operation != admissionv1.Update) {
		return admission.Denied("CetiAdmin may create or update Agent intent, not delete or edit status/finalizers")
	}
	var next, old api.Agent
	if decodeJSON(request.Object.Raw, &next, true) != nil {
		return admission.Denied("CetiAdmin Agent input must use the current API schema")
	}
	if request.Operation == admissionv1.Create {
		return h.cetiAdminCreate(ctx, request, &next, binding.Config())
	}
	if decodeJSON(request.OldObject.Raw, &old, false) != nil {
		return admission.Denied("CetiAdmin update requires the previous Agent")
	}
	current, err := cetiadmin.ResolveManagedAgent(ctx, h.reader, binding.Config(), state.Record, next.Name)
	if err != nil || old.UID == "" || next.UID != old.UID || current.UID != old.UID ||
		next.Name != request.Name || old.Name != current.Name || next.Namespace != "" || old.Namespace != "" ||
		next.ResourceVersion == "" || next.ResourceVersion != old.ResourceVersion || old.ResourceVersion != current.ResourceVersion {
		return admission.Denied("CetiAdmin update requires the bound current Agent UID and resourceVersion")
	}
	if !cetiAdminAgentIntentOnly(request) {
		return admission.Denied("CetiAdmin cannot change owner, Team, identity, permissions, mounts or arbitrary Helm values")
	}
	if !reflect.DeepEqual(next.Spec.Helm.Values["resources"], old.Spec.Helm.Values["resources"]) &&
		!safeCetiAdminValues(map[string]runtime.RawExtension{"resources": next.Spec.Helm.Values["resources"]}) {
		return admission.Denied("resource changes must use positive CPU, memory or ephemeral-storage quantities")
	}
	rotation := !sameAnnotation(&next, &old, nativeAuthRotationAnnotation)
	if rotation || next.Spec.Helm.Maintenance != nil {
		_, target, err := cetiadmin.ResolveRuntimeTarget(ctx, h.reader, binding.Config(), state.Record, current.Name, "")
		if err != nil || target.Protected {
			return admission.Denied("native auth rotation and Helm maintenance require a current ordinary runtime")
		}
	}
	if rotation {
		if !rotationIDPattern.MatchString(next.Annotations[nativeAuthRotationAnnotation]) ||
			current.Status.Phase != api.PhaseReady || current.Spec.Suspended || current.Spec.PlannedMigration != nil ||
			!reflect.DeepEqual(next.Spec, old.Spec) {
			return admission.Denied("native auth rotation intent requires a ready ordinary target and no other changes")
		}
	}
	return admission.Allowed("bound CetiAdmin Agent intent; existing policy authorization still applies")
}

func (h *CetiAdminHandler) cetiAdminCreate(ctx context.Context, request admission.Request, agent *api.Agent, config cetiadmin.Config) admission.Response {
	// The StatefulSet revision label limits instance names to 35 characters.
	if len(agent.Name) > 35 || len(validation.IsDNS1123Label(agent.Name)) != 0 {
		return admission.Denied("instance name must be a DNS label of at most 35 characters")
	}
	// Match Portal route reservations, not its owner-alias naming convention.
	switch agent.Name {
	case "oauth2", "api", "internal", "healthz", "readyz", "assets", "portal", "instances", "connections", "admin", "ui":
		return admission.Denied("instance name is reserved for a platform route")
	}
	if agent.Name == config.InstanceName || agent.Namespace != "" || agent.Spec.Team == config.InstanceName || agent.Spec.Team == "" ||
		(config.MigrationSource != nil && agent.Name == config.MigrationSource.Agent.Name) ||
		(agent.Spec.Agent.Type != api.AgentTypeHermes && agent.Spec.Agent.Type != api.AgentTypeOpenClaw && agent.Spec.Agent.Type != api.AgentTypeCustom) ||
		agent.Spec.AgentIdentity != nil || agent.Spec.Tailscale != nil || agent.Spec.Agent.RegistrySecretRef != nil ||
		agent.Spec.PlannedMigration != nil || len(agent.OwnerReferences) != 0 || len(agent.Finalizers) != 0 {
		return admission.Denied("CetiAdmin creation requires an ordinary approved runtime and Team without privilege or identity overrides")
	}
	for key := range agent.Labels {
		if strings.HasPrefix(key, "cetirun.microsoft.com/") {
			return admission.Denied("CetiAdmin cannot create protected markers")
		}
	}
	for key := range agent.Annotations {
		if key != api.AnnotationOwnerTenantID && key != api.AnnotationOwnerObjectID && key != sandboxbinding.AuthorizationAnnotation {
			return admission.Denied("CetiAdmin cannot submit authorization or platform annotations")
		}
	}
	// A new owner must already be represented by the platform's trusted owner
	// metadata; copying an arbitrary email or minting claims is not authorization.
	owners := &api.AgentList{}
	if err := h.reader.List(ctx, owners); err != nil {
		return admission.Denied("existing owner identity cannot be confirmed")
	}
	knownOwner := false
	for _, existing := range owners.Items {
		knownOwner = knownOwner || (existing.DeletionTimestamp == nil && existing.Spec.User.Alias == agent.Spec.User.Alias &&
			strings.EqualFold(existing.Spec.User.Email, agent.Spec.User.Email) &&
			existing.Annotations[api.AnnotationOwnerTenantID] != "" && existing.Annotations[api.AnnotationOwnerObjectID] != "" &&
			strings.EqualFold(existing.Annotations[api.AnnotationOwnerTenantID], agent.Annotations[api.AnnotationOwnerTenantID]) &&
			strings.EqualFold(existing.Annotations[api.AnnotationOwnerObjectID], agent.Annotations[api.AnnotationOwnerObjectID]))
	}
	team := &api.Team{}
	if !knownOwner || h.reader.Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team) != nil || team.DeletionTimestamp != nil {
		return admission.Denied("CetiAdmin creation requires an existing owner identity and live Team")
	}
	if !safeCetiAdminValues(agent.Spec.Helm.Values) {
		return admission.Denied("CetiAdmin creation only accepts resource limits and HOME size Helm values")
	}
	return admission.Allowed("CetiAdmin creation uses a confirmed owner and Team; normal runtime/policy checks remain required")
}

func cetiAdminAgentIntentOnly(request admission.Request) bool {
	var next, old map[string]any
	if decodeJSON(request.Object.Raw, &next, false) != nil || decodeJSON(request.OldObject.Raw, &old, false) != nil {
		return false
	}
	for _, object := range []map[string]any{next, old} {
		metadata, _ := object["metadata"].(map[string]any)
		delete(metadata, "managedFields")
		delete(metadata, "generation")
		annotations, _ := metadata["annotations"].(map[string]any)
		delete(annotations, nativeAuthRotationAnnotation)
		if len(annotations) == 0 {
			delete(metadata, "annotations")
		}
		spec, _ := object["spec"].(map[string]any)
		if spec == nil {
			return false
		}
		for _, key := range []string{"suspended", "restartRequestID", "model", "skillOverrides"} {
			delete(spec, key)
		}
		runtime, _ := spec["agent"].(map[string]any)
		delete(runtime, "image")
		delete(runtime, "port")
		helm, _ := spec["helm"].(map[string]any)
		delete(helm, "version")
		delete(helm, "maintenance")
		values, _ := helm["values"].(map[string]any)
		delete(values, "resources")
		persistence, _ := values["persistence"].(map[string]any)
		delete(persistence, "size")
		if len(persistence) == 0 {
			delete(values, "persistence")
		}
		if len(values) == 0 {
			delete(helm, "values")
		}
		if len(helm) == 0 {
			delete(spec, "helm")
		}
	}
	return reflect.DeepEqual(next, old)
}

func safeCetiAdminValues(values map[string]runtime.RawExtension) bool {
	for key, raw := range values {
		var value map[string]any
		if decodeJSON(raw.Raw, &value, false) != nil || value == nil {
			return false
		}
		switch key {
		case "persistence":
			if len(value) != 1 {
				return false
			}
			text, ok := value["size"].(string)
			quantity, err := resource.ParseQuantity(text)
			if !ok || err != nil || quantity.Sign() <= 0 {
				return false
			}
		case "resources":
			for bound, quantities := range value {
				if bound != "limits" && bound != "requests" {
					return false
				}
				entries, ok := quantities.(map[string]any)
				if !ok {
					return false
				}
				for name, input := range entries {
					if name != "cpu" && name != "memory" && name != "ephemeral-storage" {
						return false
					}
					text, ok := input.(string)
					quantity, err := resource.ParseQuantity(text)
					if !ok || err != nil || quantity.Sign() <= 0 {
						return false
					}
				}
			}
		default:
			return false
		}
	}
	return true
}

func cetiAdminPodDelete(request admission.Request, agent *api.Agent) admission.Response {
	var pod corev1.Pod
	var options metav1.DeleteOptions
	if request.Operation != admissionv1.Delete || protectedCetiAdminTarget(agent) || agent.Spec.Suspended || agent.Spec.PlannedMigration != nil ||
		decodeJSON(request.OldObject.Raw, &pod, false) != nil || !targetPod(&pod, agent) ||
		decodeJSON(request.Options.Raw, &options, false) != nil || options.Preconditions == nil || options.Preconditions.UID == nil ||
		*options.Preconditions.UID != pod.UID || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != pod.ResourceVersion ||
		(options.GracePeriodSeconds != nil && *options.GracePeriodSeconds == 0) {
		return admission.Denied("Pod restart requires a current ordinary runtime Pod and non-force UID/version-conditional deletion")
	}
	return admission.Allowed("conditional ordinary runtime Pod restart")
}

func cetiAdminPVCGrowth(request admission.Request) admission.Response {
	var next, old corev1.PersistentVolumeClaim
	if request.Operation != admissionv1.Update || decodeJSON(request.Object.Raw, &next, false) != nil || decodeJSON(request.OldObject.Raw, &old, false) != nil ||
		next.UID == "" || next.UID != old.UID || next.ResourceVersion == "" || next.ResourceVersion != old.ResourceVersion {
		return admission.Denied("PVC expansion requires a current existing claim")
	}
	before, after := old.Spec.Resources.Requests[corev1.ResourceStorage], next.Spec.Resources.Requests[corev1.ResourceStorage]
	if after.Cmp(before) <= 0 || after.Cmp(resource.Quantity{}) <= 0 {
		return admission.Denied("CetiAdmin can only increase a PVC storage request")
	}
	next.Spec.Resources.Requests[corev1.ResourceStorage] = before
	next.ManagedFields = old.ManagedFields
	next.Generation = old.Generation
	if !reflect.DeepEqual(next, old) {
		return admission.Denied("PVC expansion cannot change storage or resource ownership")
	}
	return admission.Allowed("storage request increases without changing the claim binding")
}

func cetiAdminEvent(request admission.Request, agent *api.Agent) admission.Response {
	if request.Operation != admissionv1.Create && request.Operation != admissionv1.Update {
		return admission.Denied("CetiAdmin audit Events cannot be deleted")
	}
	var object map[string]any
	if decodeJSON(request.Object.Raw, &object, false) != nil {
		return admission.Denied("audit Event is invalid")
	}
	reference, _ := object["involvedObject"].(map[string]any)
	if request.Resource.Group == "events.k8s.io" {
		reference, _ = object["regarding"].(map[string]any)
	}
	if reference["kind"] != "Agent" || reference["name"] != agent.Name || reference["uid"] != string(agent.UID) {
		return admission.Denied("audit Event must refer to the bound target Agent UID")
	}
	return admission.Allowed("target-scoped operation Event")
}
