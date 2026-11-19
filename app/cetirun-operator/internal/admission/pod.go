// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplacement"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	strictjson "sigs.k8s.io/json"
)

const (
	PodValidationPath        = "/validate-sandbox-pods"
	PodExpectationKey        = sandboxworkload.PodExpectationKey
	DefaultPodRequestTimeout = 8 * time.Second
	podSourceReadConcurrency = 4
)

type PodConfig struct {
	// Only the controller may write expectation ConfigMaps in this namespace.
	// It must not be an Agent workload namespace. Admission matching must
	// independently protect that boundary and exclude its own repair resources.
	StateNamespace string
	// Optional placement registry also verifies Node-added topology labels.
	PlacementConfigMapName string
	// Same UID-pinned authority as configuration admission; never written here.
	CoordinationConfigMapName string
	CoordinationConfigMapUID  string
	// CetiAdminSource is the deployment-configured source that can own
	// separate runtime access bindings for a Sandbox Agent.
	CetiAdminSource types.NamespacedName
	// RequestTimeout covers all authoritative reads, not each individual GET.
	// The chart reserves 20% of the webhook timeout for transport and response.
	RequestTimeout time.Duration
}

type PodHandler struct {
	reader       client.Reader
	config       PodConfig
	placement    *sandboxplacement.Handler
	coordination *sandboxcoordination.Store
}

func (h *PodHandler) Handle(ctx context.Context, request admission.Request) admission.Response {
	return h.Validate(ctx, request)
}

// NewPodHandler requires mgr.GetAPIReader (or another uncached API reader).
// An informer, in-memory expectation or caller-supplied label is not authority.
func NewPodHandler(reader client.Reader, config PodConfig) (*PodHandler, error) {
	if reader == nil || config.StateNamespace == "" || len(validation.IsDNS1123Label(config.StateNamespace)) != 0 {
		return nil, fmt.Errorf("Pod admission requires an uncached reader and protected controller state namespace")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = DefaultPodRequestTimeout
	}
	if config.RequestTimeout <= 0 || config.RequestTimeout >= 30*time.Second {
		return nil, fmt.Errorf("Pod admission request timeout must be positive and less than 30 seconds")
	}
	handler := &PodHandler{reader: reader, config: config}
	if config.CoordinationConfigMapName != "" || config.CoordinationConfigMapUID != "" {
		identity := sandboxworkload.ObjectIdentity{Name: config.CoordinationConfigMapName, UID: types.UID(config.CoordinationConfigMapUID)}
		if err := sandboxcoordination.ValidateAuthorityIdentity(identity); err != nil {
			return nil, err
		}
		if config.CoordinationConfigMapName == config.PlacementConfigMapName {
			return nil, fmt.Errorf("Pod coordination and placement require separate ConfigMaps")
		}
		handler.coordination = &sandboxcoordination.Store{APIReader: reader, Namespace: config.StateNamespace, Identity: identity}
	}
	if config.PlacementConfigMapName != "" {
		placement, err := sandboxplacement.NewHandler(reader, sandboxplacement.Config{
			Namespace: config.StateNamespace, ConfigMapName: config.PlacementConfigMapName,
			RequestTimeout: config.RequestTimeout,
		})
		if err != nil {
			return nil, err
		}
		handler.placement = placement
	}
	return handler, nil
}

func PodExpectationName(namespace string, uid types.UID) string {
	return sandboxworkload.PodExpectationName(namespace, uid)
}

// PodExpectationObject produces controller state, not an execution permit.
// The Bundle's Expect method always starts held. Persisting this object requires
// separate controller authorization and must never be exposed as a tenant API.
func PodExpectationObject(stateNamespace string, expected sandboxworkload.Expectation) (*corev1.ConfigMap, error) {
	return sandboxworkload.PodExpectationObject(stateNamespace, expected)
}

// Validate has no namespace-wide or platform-helper exemption. The API server
// must send only protected namespaces here; matching is a separate repair-safe
// control. Missing/stale controller state denies protected writes except the
// garbage collector's finalizer-only completion of an already deleting Pod.
func (h *PodHandler) Validate(ctx context.Context, request admission.Request) admission.Response {
	if request.Operation == admissionv1.Delete || request.SubResource == "status" {
		return admission.Allowed("Pod deletion and status remain independent of policy admission")
	}
	if request.Resource.Group == "" && request.Resource.Version == "v1" && request.Operation == admissionv1.Create &&
		((request.Resource.Resource == "pods" && request.SubResource == "binding") ||
			(request.Resource.Resource == "bindings" && request.SubResource == "")) {
		return h.validateMigrationBinding(ctx, request)
	}
	if request.Resource.Group == "" && request.Resource.Resource == "pods" && request.Resource.Version == "v1" &&
		request.Operation == admissionv1.Connect && slices.Contains([]string{"exec", "attach", "portforward"}, request.SubResource) {
		if request.SubResource == "exec" && terminalConnectPreflight(request) {
			return h.validateTerminalConnect(ctx, request)
		}
		return rejectPod(ctx, request, podDenied("ConnectForbidden", "interactive access to protected Pods is not supported"))
	}
	if request.Resource.Group != "" || request.Resource.Resource != "pods" || request.Resource.Version != "v1" ||
		(request.Operation != admissionv1.Create && request.Operation != admissionv1.Update) ||
		(request.SubResource != "" && request.SubResource != "ephemeralcontainers" && request.SubResource != "resize") {
		return rejectPod(ctx, request, podDenied("UnsupportedRequest", "unsupported protected Pod request"))
	}
	ctx, cancel := context.WithTimeout(ctx, h.config.RequestTimeout)
	defer cancel()
	var pod corev1.Pod
	if err := decodePod(request.Object.Raw, &pod); err != nil || pod.Namespace != request.Namespace || request.Namespace == "" ||
		(request.Name != "" && request.Name != pod.Name) {
		return rejectPod(ctx, request, &podFailure{http.StatusBadRequest, "InvalidPod", "invalid or ambiguous protected Pod"})
	}
	if podGarbageCollectorFinalizerUpdate(request, &pod) {
		return admission.Allowed("garbage collector Pod finalization remains independent of policy admission")
	}
	if changed, add := migrationFinalizerChange(request, &pod); changed {
		return h.validateMigrationFinalizer(ctx, request, &pod, add)
	}
	releaseFinalizer := nativePodFinalizerRemoval(request, &pod)
	if pod.Annotations[sandboxworkload.ReconciledAnnotation] == "true" {
		if response, handled := h.validateReconciledPod(ctx, request, &pod); handled {
			return response
		}
	}
	expected, state, failure := h.readPodExpectation(ctx, request.Namespace, releaseFinalizer)
	if failure != nil {
		return rejectPod(ctx, request, failure)
	}
	if releaseFinalizer {
		return h.releaseNativePodFinalizer(ctx, request, &pod, expected, state)
	}
	agent, binding, err := sandboxbinding.Read(ctx, h.reader, client.ObjectKey{Name: expected.Agent.Name})
	if err != nil || agent.UID != expected.Agent.UID || agent.Generation != expected.AgentGeneration ||
		binding != expected.Binding || expected.Binding.AgentUID != expected.Agent.UID {
		return rejectPod(ctx, request, podDenied("BindingUnauthorized", "Pod binding is missing, stale or no longer authorized"))
	}
	if expected.ExecutionHeld || agent.Spec.Suspended || slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) {
		return rejectPod(ctx, request, podDenied("ExecutionHeld", sandboxworkload.ErrExecutionHeld.Error()))
	}
	if expected.SinglePod && expected.PodUID != "" && request.Operation == admissionv1.Create {
		return rejectPod(ctx, request, podDenied("ReplacementUnsupported", "native Sandbox Pod replacement requires a new Agent"))
	}
	if expected.Placement != nil && h.placement == nil {
		return rejectPod(ctx, request, podDenied("PlacementUnavailable", "recorded placement requires the configured authoritative placement reader"))
	}
	if failure := h.validateNativeReservation(ctx, expected); failure != nil {
		return rejectPod(ctx, request, failure)
	}
	if failure := h.validatePodSources(ctx, expected, agent); failure != nil {
		return rejectPod(ctx, request, failure)
	}
	workload := &appsv1.StatefulSet{}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}, workload); err != nil {
		return rejectPod(ctx, request, podReadFailure("Workload", err))
	}
	owner := sandboxworkload.AgentOwner(agent.Name, agent.UID)
	if workload.UID != expected.Workload.UID || !workload.DeletionTimestamp.IsZero() ||
		!equality.Semantic.DeepEqual(workload.OwnerReferences, []metav1.OwnerReference{owner}) ||
		workload.Annotations[sandboxworkload.RevisionAnnotation] != expected.Revision ||
		workload.Annotations[sandboxworkload.AgentUIDAnnotation] != string(agent.UID) ||
		ptr.Deref(workload.Spec.Replicas, 0) != 1 || workload.Spec.ServiceName != workload.Name ||
		len(workload.Spec.VolumeClaimTemplates) != 0 ||
		!sandboxworkload.SamePodTemplate(workload.Spec.Template, expected.Template) {
		return rejectPod(ctx, request, podDenied("WorkloadMismatch", "current workload ownership, lifecycle intent or template differs from the expectation"))
	}
	if expected.ControllerRevision != workload.Status.UpdateRevision || expected.ControllerRevision == "" {
		return rejectPod(ctx, request, podDenied("ControllerRevisionUnobserved", "StatefulSet controller revision has not been observed"))
	}
	if request.Operation == admissionv1.Update {
		var old corev1.Pod
		if err := decodePod(request.OldObject.Raw, &old); err != nil {
			return rejectPod(ctx, request, &podFailure{http.StatusBadRequest, "InvalidPreviousPod", "invalid previous Pod"})
		}
		current := &corev1.Pod{}
		if err := h.reader.Get(ctx, client.ObjectKeyFromObject(&pod), current); err != nil {
			return rejectPod(ctx, request, podReadFailure("PreviousPod", err))
		}
		if pod.UID == "" || current.UID != pod.UID || old.UID != current.UID || old.ResourceVersion != current.ResourceVersion ||
			pod.ResourceVersion != current.ResourceVersion || old.Name != pod.Name || old.Namespace != pod.Namespace ||
			old.Spec.NodeName != current.Spec.NodeName {
			return rejectPod(ctx, request, podDenied("PreviousPodMismatch", "Pod update does not match the authoritative previous UID and revision"))
		}
		// Scheduling is only grandfathered from the persisted Pod, not supplied
		// by a new Pod or inferred from its caller, labels or ServiceAccount.
		expected.Template.Spec.NodeName = current.Spec.NodeName
	}
	if h.placement != nil && pod.Spec.NodeName != "" {
		if response := h.placement.ValidateScheduledPod(ctx, request, &pod, expected); !response.Allowed {
			return response
		}
	} else if err := sandboxworkload.ValidatePod(&pod, expected); err != nil {
		message := "Pod does not match the protected workload contract"
		var mismatch *sandboxworkload.PodMismatchError
		if errors.As(err, &mismatch) {
			message = mismatch.Error()
		}
		return rejectPod(ctx, request, podDenied("TemplateMismatch", message))
	}
	currentState := &corev1.ConfigMap{}
	if err := h.reader.Get(ctx, client.ObjectKeyFromObject(state), currentState); err != nil {
		return rejectPod(ctx, request, podReadFailure("Expectation", err))
	}
	if currentState.UID != state.UID || currentState.ResourceVersion != state.ResourceVersion {
		return rejectPod(ctx, request, podDenied("ExpectationChanged", "Pod expectation changed during validation"))
	}
	if failure := h.validateNativeReservation(ctx, expected); failure != nil {
		return rejectPod(ctx, request, failure)
	}
	if ctx.Err() != nil {
		return rejectPod(ctx, request, podReadFailure("Expectation", ctx.Err()))
	}
	return admission.Allowed("final Pod matches current controller-owned expectations")
}

func (h *PodHandler) validateNativeReservation(ctx context.Context, expected sandboxworkload.Expectation) *podFailure {
	if !expected.SinglePod || h.coordination == nil {
		return nil
	}
	intent, err := sandboxcoordination.NativeIntentFor(expected)
	if err != nil {
		return podReadFailure("NativeReservation", err)
	}
	if err := h.coordination.AuthorizeNativeStart(ctx, intent); err != nil {
		if errors.Is(err, sandboxcoordination.ErrConflict) {
			return podDenied("NativeReservationRequired", "native Sandbox requires its current non-stopping source reservation")
		}
		return podReadFailure("NativeReservation", err)
	}
	return nil
}

func decodePod(raw []byte, target *corev1.Pod) error {
	problems, err := strictjson.UnmarshalStrict(raw, target)
	if err != nil || len(problems) != 0 || target.Kind != "Pod" || target.APIVersion != "v1" {
		return fmt.Errorf("invalid Pod representation")
	}
	return nil
}

func podGarbageCollectorFinalizerUpdate(request admission.Request, pod *corev1.Pod) bool {
	// Keep this boundary aligned with the rendered Pod webhook match condition:
	// the API server must also permit this exact cleanup when serving is down.
	if request.Operation != admissionv1.Update || request.SubResource != "" || request.RequestSubResource != "" ||
		request.Resource != (metav1.GroupVersionResource{Version: "v1", Resource: "pods"}) ||
		request.Kind != (metav1.GroupVersionKind{Version: "v1", Kind: "Pod"}) ||
		(request.RequestResource != nil && *request.RequestResource != request.Resource) ||
		(request.RequestKind != nil && *request.RequestKind != request.Kind) ||
		request.Namespace == "" || request.Namespace != pod.Namespace ||
		request.Name == "" || request.Name != pod.Name || pod.UID == "" || pod.ResourceVersion == "" {
		return false
	}
	var old corev1.Pod
	if err := decodePod(request.OldObject.Raw, &old); err != nil {
		return false
	}
	return garbageCollectorFinalizerUpdate(request, pod, &old)
}

func (h *PodHandler) readExpectation(ctx context.Context, namespace string) (sandboxworkload.Expectation, *corev1.ConfigMap, *podFailure) {
	return h.readPodExpectation(ctx, namespace, false)
}

func (h *PodHandler) readPodExpectation(ctx context.Context, namespace string, cleanup bool) (sandboxworkload.Expectation, *corev1.ConfigMap, *podFailure) {
	var expected sandboxworkload.Expectation
	if namespace == h.config.StateNamespace {
		return expected, nil, podDenied("ControllerNamespace", "controller state is not an Agent namespace")
	}
	ns := &corev1.Namespace{}
	if err := h.reader.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		return expected, nil, podReadFailure("Namespace", err)
	}
	if ns.UID == "" || (!cleanup && !ns.DeletionTimestamp.IsZero()) ||
		ns.Labels[sandboxworkload.ProtectedNamespaceLabel] != "true" {
		return expected, nil, podDenied("NamespaceInactive", "namespace is unprotected, absent or terminating")
	}
	state := &corev1.ConfigMap{}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: h.config.StateNamespace, Name: PodExpectationName(namespace, ns.UID)}, state); err != nil {
		return expected, nil, podReadFailure("Expectation", err)
	}
	raw := state.Data[PodExpectationKey]
	if len(raw) == 0 || len(raw) > 512*1024 {
		return expected, nil, podDenied("ExpectationInvalid", "controller expectation is empty or exceeds the size limit")
	}
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &expected)
	if err != nil || len(problems) != 0 ||
		len(state.Data) != 1 || len(state.BinaryData) != 0 || state.UID == "" || state.ResourceVersion == "" ||
		(!cleanup && !state.DeletionTimestamp.IsZero()) || expected.Version != sandboxworkload.ExpectationVersion ||
		expected.Namespace.Name != ns.Name || expected.Namespace.UID != ns.UID ||
		expected.WorkloadKind != sandboxworkload.AgentWorkload || expected.Workload.Name == "" || expected.Workload.UID == "" ||
		expected.Agent.Name == "" || expected.Agent.UID == "" || expected.Revision == "" ||
		state.Annotations[sandboxworkload.RevisionAnnotation] != expected.Revision ||
		state.Annotations[sandboxworkload.AgentUIDAnnotation] != string(expected.Agent.UID) ||
		!equality.Semantic.DeepEqual(state.OwnerReferences, []metav1.OwnerReference{sandboxworkload.AgentOwner(expected.Agent.Name, expected.Agent.UID)}) {
		return expected, nil, podDenied("ExpectationInvalid", "invalid controller-owned expectation")
	}
	if ns.Annotations[sandboxworkload.AgentNameAnnotation] != expected.Agent.Name ||
		ns.Annotations[sandboxworkload.AgentUIDAnnotation] != string(expected.Agent.UID) ||
		!equality.Semantic.DeepEqual(ns.OwnerReferences, []metav1.OwnerReference{sandboxworkload.AgentOwner(expected.Agent.Name, expected.Agent.UID)}) {
		return expected, nil, podDenied("NamespaceOwnershipChanged", "namespace ownership differs from the controller-owned expectation")
	}
	if err := expected.ValidateSkills(); err != nil {
		return expected, nil, podDenied("ExpectationInvalid", "invalid controller-owned skills source")
	}
	return expected, state, nil
}

func (h *PodHandler) validatePodSources(ctx context.Context, expected sandboxworkload.Expectation, agent *api.Agent) *podFailure {
	if len(expected.Sources) < 9 || len(expected.Sources) > sandboxworkload.MaxPreparationSources {
		return podDenied("SourceInventoryInvalid", fmt.Sprintf("source inventory must contain 9 to %d identities", sandboxworkload.MaxPreparationSources))
	}
	type objectKey struct{ version, kind, namespace, name string }
	required := map[objectKey]types.UID{
		{api.GroupVersion.String(), "Agent", "", agent.Name}:                                           expected.Agent.UID,
		{api.GroupVersion.String(), "Team", "", agent.Spec.Team}:                                       expected.Binding.TeamUID,
		{api.GroupVersion.String(), "Sandbox", "", agent.Spec.SandboxRef}:                              expected.Binding.SandboxUID,
		{"v1", "Namespace", "", expected.Namespace.Name}:                                               expected.Namespace.UID,
		{"apps/v1", "StatefulSet", expected.Namespace.Name, expected.Workload.Name}:                    expected.Workload.UID,
		{"v1", "PersistentVolumeClaim", expected.Namespace.Name, expected.Home.Name}:                   expected.Home.UID,
		{"v1", "ServiceAccount", expected.Namespace.Name, expected.Template.Spec.ServiceAccountName}:   "",
		{"node.k8s.io/v1", "RuntimeClass", "", ptr.Deref(expected.Template.Spec.RuntimeClassName, "")}: "",
	}
	if expected.SkillsMode != sandboxworkload.SkillsModeEmpty {
		required[objectKey{"v1", "PersistentVolumeClaim", expected.Namespace.Name, expected.Skills.Name}] = expected.Skills.UID
	}
	for _, source := range sandboxworkload.PodSourceReferences(expected.Template.Spec, expected.Namespace.Name) {
		key := objectKey{source.APIVersion, source.Kind, source.Namespace, source.Name}
		if _, exists := required[key]; !exists {
			required[key] = ""
		}
	}
	seen := map[objectKey]bool{}
	policy := false
	for _, source := range expected.Sources {
		key := objectKey{source.APIVersion, source.Kind, source.Namespace, source.Name}
		if !source.ValidIdentity() || seen[key] {
			return podDenied("SourceInventoryInvalid", "source identities must be complete and unique")
		}
		if expected.SkillsMode == sandboxworkload.SkillsModeEmpty && source.APIVersion == "v1" &&
			(source.Kind == "PersistentVolume" || (source.Kind == "PersistentVolumeClaim" &&
				(source.Namespace != expected.Namespace.Name || source.Name != expected.Home.Name))) {
			return podDenied("SourceInventoryInvalid", "empty skills cannot pin a skills PVC or publication volume")
		}
		if source.IsAgent() && (source.Name != expected.Agent.Name || source.Generation != expected.AgentGeneration) {
			return podDenied("SourceBindingChanged", "Agent intent differs from the expectation")
		}
		if _, err := schema.ParseGroupVersion(source.APIVersion); err != nil || source.Kind == "" {
			return podDenied("SourceInventoryInvalid", "source inventory contains an invalid resource kind or version")
		}
		if uid, exists := required[key]; exists {
			if uid != "" && uid != source.UID {
				return podDenied("SourceBindingChanged", "required source binding differs from the expectation")
			}
			delete(required, key)
		}
		if source.APIVersion == api.GroupVersion.String() && source.Kind == "AgentPolicy" &&
			source.Namespace == "" && source.UID == expected.Binding.AgentPolicyUID {
			policy = true
		}
		seen[key] = true
	}
	if len(required) != 0 || !policy {
		return podDenied("SourceInventoryIncomplete", "required source identities are missing")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstFailure *podFailure
	var once sync.Once
	var workers sync.WaitGroup
	for worker := range podSourceReadConcurrency {
		workers.Go(func() {
			for i := worker; i < len(expected.Sources); i += podSourceReadConcurrency {
				if ctx.Err() != nil {
					return
				}
				if failure := h.readPodSource(ctx, expected, expected.Sources[i]); failure != nil {
					failure.message = fmt.Sprintf("expectation.sources[%d]: %s", i, failure.message)
					once.Do(func() {
						firstFailure = failure
						cancel()
					})
					return
				}
			}
		})
	}
	workers.Wait()
	if firstFailure != nil {
		return firstFailure
	}
	if ctx.Err() != nil {
		return podReadFailure("Source", ctx.Err())
	}
	return nil
}

func (h *PodHandler) readPodSource(ctx context.Context, expected sandboxworkload.Expectation, source sandboxworkload.SourceReference) *podFailure {
	if source.IsAgent() {
		agent := &api.Agent{}
		if err := h.reader.Get(ctx, client.ObjectKey{Name: source.Name}, agent); err != nil {
			return podReadFailure("Source", err)
		}
		current, err := sandboxworkload.AgentSourceReference(agent)
		if err != nil || current != source {
			return podDenied("SourceChanged", "Agent identity, authorization or lifecycle intent changed")
		}
		return nil
	}
	var object client.Object = &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: source.APIVersion, Kind: source.Kind}}
	effectiveSource := expected.SinglePod && sandboxworkload.EffectiveSource(source)
	if effectiveSource {
		version, err := schema.ParseGroupVersion(source.APIVersion)
		if err != nil {
			return podDenied("SourceInvalid", "native source version is invalid")
		}
		value := &unstructured.Unstructured{}
		value.SetGroupVersionKind(version.WithKind(source.Kind))
		object = value
		if source.APIVersion == api.GroupVersion.String() && source.Kind == "Team" {
			object = &api.Team{}
		}
		if source.APIVersion == "v1" && source.Kind == "PersistentVolumeClaim" {
			object = &corev1.PersistentVolumeClaim{}
		}
	}
	var runtimeClass *nodev1.RuntimeClass
	if source.APIVersion == "node.k8s.io/v1" && source.Kind == "RuntimeClass" && source.Namespace == "" &&
		source.Name == ptr.Deref(expected.Template.Spec.RuntimeClassName, "") {
		runtimeClass = &nodev1.RuntimeClass{}
		object = runtimeClass
	}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, object); err != nil {
		return podReadFailure("Source", err)
	}
	workloadSource := expected.SinglePod && source.APIVersion == "apps/v1" && source.Kind == "StatefulSet" &&
		source.Namespace == expected.Namespace.Name && source.Name == expected.Workload.Name && source.UID == expected.Workload.UID
	// Native startup changes replicas and the StatefulSet controller updates
	// status. Validate its complete current template/lifecycle in Validate.
	if object.GetUID() != source.UID ||
		((source.IsClusterRole() || (!workloadSource && !effectiveSource)) && object.GetResourceVersion() != source.ResourceVersion) ||
		!object.GetDeletionTimestamp().IsZero() {
		return podDenied("SourceChanged", "a source UID or revision changed, or the source is terminating")
	}
	if effectiveSource {
		box := ""
		for _, entry := range expected.Sources {
			if entry.APIVersion == api.GroupVersion.String() && entry.Kind == "Sandbox" && entry.UID == expected.Binding.SandboxUID {
				box = entry.Name
			}
		}
		digest, err := sandboxworkload.ExecutionSourceDigest(object, box)
		if claim, ok := object.(*corev1.PersistentVolumeClaim); ok {
			digest, err = sandboxworkload.NativeClaimDigest(claim)
		}
		pinned := expected.SourceDigests[sandboxworkload.SourceKey(source)]
		if err != nil || !sandboxworkload.ValidInputDigest(pinned) || digest != pinned {
			return podDenied("SourceChanged", "native Sandbox effective source inputs changed")
		}
	}
	if source.APIVersion == "v1" && source.Kind == "PersistentVolumeClaim" &&
		source.Namespace == expected.Namespace.Name && source.Name == expected.Home.Name &&
		object.GetAnnotations()[sandboxworkload.AgentUIDAnnotation] != string(expected.Agent.UID) {
		return podDenied("HomeOwnershipChanged", "HOME ownership differs from the expected Agent")
	}
	if runtimeClass != nil {
		var overhead corev1.ResourceList
		if runtimeClass.Overhead != nil && len(runtimeClass.Overhead.PodFixed) != 0 {
			overhead = runtimeClass.Overhead.PodFixed
		}
		if !equality.Semantic.DeepEqual(overhead, expected.Overhead) {
			return podDenied("RuntimeClassChanged", "RuntimeClass overhead differs from the expectation")
		}
	}
	return nil
}

type podFailure struct {
	code    int32
	reason  string
	message string
}

func (f *podFailure) Error() string { return f.reason + ": " + f.message }

func podDenied(reason, message string) *podFailure {
	return &podFailure{http.StatusForbidden, reason, message}
}

func podReadFailure(stage string, err error) *podFailure {
	if apierrors.IsNotFound(err) {
		return podDenied(stage+"Missing", "a required "+stage+" object is missing")
	}
	return &podFailure{http.StatusServiceUnavailable, stage + "ReadFailed", "an authoritative " + stage + " read failed"}
}

func rejectPod(ctx context.Context, request admission.Request, failure *podFailure) admission.Response {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		failure = &podFailure{http.StatusGatewayTimeout, "RequestDeadlineExceeded", "authoritative validation exceeded its request budget"}
	} else if errors.Is(ctx.Err(), context.Canceled) {
		failure = &podFailure{http.StatusServiceUnavailable, "RequestCanceled", "authoritative validation was canceled"}
	}
	// Reasons and diagnostics contain no source values, object diffs or raw API
	// errors. In particular, environment and Secret data must never be logged.
	log.FromContext(ctx).Info("Protected Pod admission rejected", "reason", failure.reason,
		"diagnostic", failure.message, "operation", request.Operation, "subresource", request.SubResource)
	podAdmissionDenials.WithLabelValues(failure.reason).Inc()
	if failure.code == http.StatusForbidden {
		return admission.Denied(failure.Error())
	}
	return admission.Errored(failure.code, failure)
}
