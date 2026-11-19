// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	strictjson "sigs.k8s.io/json"
)

const (
	ValidationPath        = "/validate-sandbox-placement"
	DefaultRequestTimeout = 8 * time.Second
)

type Config struct {
	// Namespace and ConfigMapName identify protected operator-owned state, never
	// an object selected from an incoming Pod, label or tenant namespace.
	Namespace      string
	ConfigMapName  string
	RequestTimeout time.Duration
}

type Handler struct {
	reader client.Reader
	config Config
}

// NewHandler requires an uncached API reader. Enabling and matching this
// read-only handler, and protecting its registry writer, are deployment duties.
func NewHandler(reader client.Reader, config Config) (*Handler, error) {
	if reader == nil || config.Namespace == "" || len(validation.IsDNS1123Label(config.Namespace)) != 0 ||
		!validName(config.ConfigMapName) {
		return nil, fmt.Errorf("placement admission requires an uncached reader and a protected operator ConfigMap")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = DefaultRequestTimeout
	}
	if config.RequestTimeout <= 0 || config.RequestTimeout >= 30*time.Second {
		return nil, fmt.Errorf("placement request timeout must be positive and less than 30 seconds")
	}
	return &Handler{reader: reader, config: config}, nil
}

func (h *Handler) Handle(ctx context.Context, request admission.Request) admission.Response {
	return h.Validate(ctx, request)
}

func (h *Handler) Validate(ctx context.Context, request admission.Request) admission.Response {
	if request.Resource == (metav1.GroupVersionResource{Version: "v1", Resource: "bindings"}) &&
		request.Operation == admissionv1.Create && request.SubResource == "" {
		return reject(ctx, request, denied("LegacyBindings", "the legacy bindings endpoint is forbidden; use pods/binding"))
	}
	if request.Resource.Group != "" || request.Resource.Version != "v1" ||
		!slices.Contains([]string{"pods", "nodes"}, request.Resource.Resource) ||
		(request.RequestResource != nil && *request.RequestResource != request.Resource) ||
		(request.RequestKind != nil && *request.RequestKind != request.Kind) ||
		(request.RequestSubResource != "" && request.RequestSubResource != request.SubResource) {
		return reject(ctx, request, denied("UnsupportedRequest", "unsupported or ambiguous placement request"))
	}
	if request.Resource.Resource == "nodes" {
		return h.validateNode(ctx, request)
	}
	if request.Operation == admissionv1.Delete || request.SubResource == "status" {
		return admission.Allowed("Pod deletion and status do not grant placement")
	}
	binding := request.Operation == admissionv1.Create && request.SubResource == "binding"
	if (!binding && (request.Operation != admissionv1.Create && request.Operation != admissionv1.Update ||
		!slices.Contains([]string{"", "ephemeralcontainers", "resize"}, request.SubResource))) ||
		(request.Operation == admissionv1.Create && !binding && request.SubResource != "") {
		return reject(ctx, request, denied("UnsupportedRequest", "unsupported placement operation"))
	}
	wantKind := "Pod"
	if binding {
		wantKind = "Binding"
	}
	if request.Kind != (metav1.GroupVersionKind{Version: "v1", Kind: wantKind}) ||
		request.Namespace == "" || len(validation.IsDNS1123Label(request.Namespace)) != 0 {
		return reject(ctx, request, invalid("InvalidRequest", "invalid placement request kind or namespace"))
	}
	ctx, cancel := context.WithTimeout(ctx, h.config.RequestTimeout)
	defer cancel()
	state := &requestState{reader: h.reader}
	registry, failure := state.readRegistry(ctx, h.config)
	if failure != nil {
		return reject(ctx, request, failure)
	}
	pod, current, target, failure := state.readRequestPod(ctx, request, binding)
	if failure != nil {
		return reject(ctx, request, failure)
	}
	if current != nil && !current.DeletionTimestamp.IsZero() {
		if !podFinalizerRemoval(request, pod, current) {
			return reject(ctx, request, denied("PodTerminating", "terminating Pods may only remove existing finalizers without other changes"))
		}
		if failure := state.recheck(ctx); failure != nil {
			return reject(ctx, request, failure)
		}
		return admission.Allowed("exact finalizer-removal cleanup does not change Pod placement")
	}
	assignment, failure := registry.agentAssignment(pod)
	if failure != nil {
		return reject(ctx, request, failure)
	}
	if current != nil {
		previous, failure := registry.agentAssignment(current)
		if failure != nil {
			return reject(ctx, request, failure)
		}
		if previous != nil && (assignment == nil || *previous != *assignment) {
			return reject(ctx, request, denied("AgentAssignmentChanged", "Pod update cannot remove or change its registered Agent assignment"))
		}
	}
	daemon, failure := registry.daemonForPod(pod)
	if failure != nil {
		return reject(ctx, request, failure)
	}
	// Registered controller claims are checked even before scheduling. An
	// unscheduled CREATE must not become a route for forged controller owners.
	if daemon != nil && request.Operation == admissionv1.Create && !binding {
		if request.UserInfo.Username != daemon.TrustedCreator ||
			slices.Contains(request.UserInfo.Groups, "system:unauthenticated") {
			return reject(ctx, request, denied("DaemonSetCreatorMismatch", "registered DaemonSet Pods require their exact trusted creator"))
		}
		if failure := state.validateDaemonPod(ctx, registry, daemon, pod); failure != nil {
			return reject(ctx, request, failure)
		}
	}
	agentValidated := false
	if target.Name != "" {
		node, registration, failure := state.readNode(ctx, registry, target.Name)
		if failure != nil {
			return reject(ctx, request, failure)
		}
		if (target.UID != "" && target.UID != node.UID) ||
			(target.ResourceVersion != "" && target.ResourceVersion != node.ResourceVersion) {
			return reject(ctx, request, denied("TargetMismatch", "binding target differs from the current registered Node"))
		}
		if assignment != nil && (registration.Reservation == nil || *registration.Reservation != *assignment) {
			return reject(ctx, request, denied("AgentPlacementMismatch", "an assigned Agent Pod cannot be placed outside its exact registered reservation"))
		}
		if registration.Reservation != nil {
			if daemon != nil {
				if binding || request.Operation == admissionv1.Update {
					if !daemon.enrolled(current) {
						return reject(ctx, request, denied("PlatformPodNotEnrolled", "reserved placement requires trusted enrollment of the persisted platform Pod name and UID"))
					}
					if failure := state.validateDaemonPod(ctx, registry, daemon, current); failure != nil {
						return reject(ctx, request, failure)
					}
				}
				if binding || request.Operation == admissionv1.Update {
					if failure := state.validateDaemonPod(ctx, registry, daemon, pod); failure != nil {
						return reject(ctx, request, failure)
					}
				}
				if failure := daemonTargetsNode(pod, node.Name); failure != nil {
					return reject(ctx, request, failure)
				}
			} else {
				if failure := state.validateAgentPod(ctx, h.config.Namespace, registry, registration.Reservation, pod, node, binding); failure != nil {
					return reject(ctx, request, failure)
				}
				agentValidated = true
			}
		}
	}
	if !agentValidated && daemon == nil {
		if failure := state.validateOptionalAgentPlacement(ctx, h.config.Namespace, registry, pod); failure != nil {
			return reject(ctx, request, failure)
		}
	}
	if failure := state.recheck(ctx); failure != nil {
		return reject(ctx, request, failure)
	}
	return admission.Allowed("placement matches the current registered inventory; execution admission remains independent")
}

type objectSnapshot struct {
	object client.Object
	stage  string
}

type requestState struct {
	reader         client.Reader
	snapshots      []objectSnapshot
	registrySource sandboxworkload.SourceReference
}

func (s *requestState) readRegistry(ctx context.Context, config Config) (*registry, *failure) {
	object := &corev1.ConfigMap{}
	if failure := s.read(ctx, client.ObjectKey{Namespace: config.Namespace, Name: config.ConfigMapName}, object, "Registry"); failure != nil {
		return nil, failure
	}
	if len(object.Data) != 1 || len(object.BinaryData) != 0 {
		return nil, denied("RegistryInvalid", "registry must contain only placement.json")
	}
	registry, err := decodeRegistry(object.Data[ConfigMapKey], config.Namespace)
	if err != nil {
		return nil, denied("RegistryInvalid", err.Error())
	}
	s.registrySource = sandboxworkload.SourceReference{
		APIVersion: "v1", Kind: "ConfigMap", Namespace: object.Namespace, Name: object.Name,
		UID: object.UID, ResourceVersion: object.ResourceVersion,
	}
	return registry, nil
}

func (s *requestState) read(ctx context.Context, key client.ObjectKey, object client.Object, stage string) *failure {
	if failure := s.readSnapshot(ctx, key, object, stage); failure != nil {
		return failure
	}
	if !object.GetDeletionTimestamp().IsZero() {
		return denied(stage+"Invalid", "authoritative "+stage+" must have a non-terminating identity")
	}
	return nil
}

func (s *requestState) readSubjectPod(ctx context.Context, key client.ObjectKey, pod *corev1.Pod) *failure {
	// Subject liveness is checked after matching its old/current UID and RV.
	// Authority objects, and Pods being bound, still use the live-only reader.
	return s.readSnapshot(ctx, key, pod, "Pod")
}

func (s *requestState) readSnapshot(ctx context.Context, key client.ObjectKey, object client.Object, stage string) *failure {
	if err := ctx.Err(); err != nil {
		return readFailure(stage, err)
	}
	if err := s.reader.Get(ctx, key, object); err != nil {
		return readFailure(stage, err)
	}
	if client.ObjectKeyFromObject(object) != key || object.GetUID() == "" || object.GetResourceVersion() == "" {
		return denied(stage+"Invalid", "authoritative "+stage+" must have an exact persisted identity")
	}
	copy, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return denied("SnapshotInvalid", "authoritative object cannot be snapshotted")
	}
	s.snapshots = append(s.snapshots, objectSnapshot{object: copy, stage: stage})
	return nil
}

func (s *requestState) recheck(ctx context.Context) *failure {
	// Recheck the registry last, after every object used to authorize placement.
	for i := 1; i <= len(s.snapshots); i++ {
		before := s.snapshots[i%len(s.snapshots)]
		if err := ctx.Err(); err != nil {
			return readFailure(before.stage, err)
		}
		after, ok := before.object.DeepCopyObject().(client.Object)
		if !ok {
			return denied("SnapshotInvalid", "authoritative object cannot be rechecked")
		}
		if err := s.reader.Get(ctx, client.ObjectKeyFromObject(before.object), after); err != nil {
			return readFailure(before.stage, err)
		}
		if !equality.Semantic.DeepEqual(before.object, after) {
			return denied(before.stage+"Changed", "an authoritative "+before.stage+" snapshot changed during validation")
		}
		if err := ctx.Err(); err != nil {
			return readFailure(before.stage, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return readFailure("Registry", err)
	}
	return nil
}

func (s *requestState) readNode(ctx context.Context, registry *registry, name string) (*corev1.Node, *registeredNode, *failure) {
	registered := registry.node(name)
	if registered == nil {
		return nil, nil, denied("NodeUnknown", "destination Node is absent from the complete registered inventory")
	}
	node := &corev1.Node{}
	if failure := s.read(ctx, client.ObjectKey{Name: name}, node, "Node"); failure != nil {
		return nil, nil, failure
	}
	if node.UID != registered.UID || node.Spec.ProviderID != registered.ProviderID {
		return nil, nil, denied("NodeIdentityMismatch", "destination Node UID or provider ID differs from its registration")
	}
	return node, registered, nil
}

func (s *requestState) readRequestPod(ctx context.Context, request admission.Request, binding bool) (*corev1.Pod, *corev1.Pod, corev1.ObjectReference, *failure) {
	var target corev1.ObjectReference
	if binding {
		var object corev1.Binding
		problems, err := strictjson.UnmarshalStrict(request.Object.Raw, &object)
		if err != nil || len(problems) != 0 || object.APIVersion != "v1" || object.Kind != "Binding" ||
			!validName(object.Name) || object.Name != request.Name || object.Namespace != request.Namespace || object.UID == "" ||
			object.Target.Kind != "Node" || (object.Target.APIVersion != "" && object.Target.APIVersion != "v1") ||
			object.Target.Namespace != "" || object.Target.FieldPath != "" || !validName(object.Target.Name) {
			return nil, nil, target, invalid("InvalidBinding", "binding requires an exact Pod UID and a Node target")
		}
		if len(object.Annotations) != 0 {
			return nil, nil, target, denied("BindingAnnotationsForbidden", "Binding annotations must not add or override Pod metadata")
		}
		pod := &corev1.Pod{}
		if failure := s.read(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.Name}, pod, "Pod"); failure != nil {
			return nil, nil, target, failure
		}
		if pod.UID != object.UID || (object.ResourceVersion != "" && object.ResourceVersion != pod.ResourceVersion) || pod.Spec.NodeName != "" {
			return nil, nil, target, denied("BindingPodMismatch", "binding must identify the current persisted unscheduled Pod")
		}
		// The API server applies Binding labels atomically with nodeName.
		// Validate that prospective Pod; annotations are explicitly forbidden.
		bound := pod.DeepCopy()
		bound.Spec.NodeName = object.Target.Name
		if bound.Labels == nil && len(object.Labels) != 0 {
			bound.Labels = map[string]string{}
		}
		for key, value := range object.Labels {
			bound.Labels[key] = value
		}
		return bound, pod, object.Target, nil
	}
	pod := &corev1.Pod{}
	if !decodePod(request.Object.Raw, pod) || pod.Namespace != request.Namespace || !validName(pod.Name) ||
		(request.Name != "" && request.Name != pod.Name) {
		return nil, nil, target, invalid("InvalidPod", "invalid or ambiguous placement Pod")
	}
	target.Name = pod.Spec.NodeName
	if request.Operation == admissionv1.Create {
		return pod, nil, target, nil
	}
	old := &corev1.Pod{}
	if !decodePod(request.OldObject.Raw, old) || old.Name != pod.Name || old.Namespace != pod.Namespace {
		return nil, nil, target, invalid("InvalidPreviousPod", "Pod update requires an unambiguous previous Pod")
	}
	current := &corev1.Pod{}
	if failure := s.readSubjectPod(ctx, client.ObjectKeyFromObject(pod), current); failure != nil {
		return nil, nil, target, failure
	}
	if pod.UID != current.UID || old.UID != current.UID || pod.ResourceVersion != current.ResourceVersion ||
		old.ResourceVersion != current.ResourceVersion || old.Spec.NodeName != current.Spec.NodeName {
		return nil, nil, target, denied("PreviousPodMismatch", "Pod update differs from the authoritative previous identity or placement")
	}
	if current.Spec.NodeName != "" && pod.Spec.NodeName != current.Spec.NodeName {
		return nil, nil, target, denied("SchedulingChanged", "an assigned Pod cannot change or remove its destination Node")
	}
	return pod, current, target, nil
}

func decodePod(raw []byte, pod *corev1.Pod) bool {
	problems, err := strictjson.UnmarshalStrict(raw, pod)
	return err == nil && len(problems) == 0 && pod.APIVersion == "v1" && pod.Kind == "Pod"
}

func podFinalizerRemoval(request admission.Request, pod, current *corev1.Pod) bool {
	if request.Operation != admissionv1.Update || request.SubResource != "" || request.RequestSubResource != "" ||
		current.DeletionTimestamp.IsZero() || len(pod.Finalizers) >= len(current.Finalizers) {
		return false
	}
	old := &corev1.Pod{}
	if !decodePod(request.OldObject.Raw, old) {
		return false
	}
	remaining := 0
	for _, finalizer := range current.Finalizers {
		if remaining < len(pod.Finalizers) && pod.Finalizers[remaining] == finalizer {
			remaining++
		}
	}
	if remaining != len(pod.Finalizers) {
		return false
	}
	before, after := current.DeepCopy(), pod.DeepCopy()
	for _, object := range []*corev1.Pod{old, before, after} {
		object.TypeMeta = metav1.TypeMeta{}
		object.ManagedFields = nil
	}
	if !equality.Semantic.DeepEqual(old, before) {
		return false
	}
	before.Finalizers, after.Finalizers = nil, nil
	return equality.Semantic.DeepEqual(before, after)
}

type failure struct {
	code    int32
	reason  string
	message string
}

func (f *failure) Error() string { return f.reason + ": " + f.message }

func denied(reason, message string) *failure {
	return &failure{code: http.StatusForbidden, reason: reason, message: message}
}

func invalid(reason, message string) *failure {
	return &failure{code: http.StatusBadRequest, reason: reason, message: message}
}

func readFailure(stage string, err error) *failure {
	if apierrors.IsNotFound(err) {
		return denied(stage+"Missing", "a required "+stage+" object is missing")
	}
	return &failure{code: http.StatusServiceUnavailable, reason: stage + "ReadFailed", message: "an authoritative " + stage + " read failed"}
}

func reject(ctx context.Context, request admission.Request, problem *failure) admission.Response {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		problem = &failure{code: http.StatusGatewayTimeout, reason: "RequestDeadlineExceeded", message: "placement validation exceeded its total request budget"}
	} else if errors.Is(ctx.Err(), context.Canceled) {
		problem = &failure{code: http.StatusServiceUnavailable, reason: "RequestCanceled", message: "placement validation was canceled"}
	}
	log.FromContext(ctx).Info("Sandbox placement admission rejected", "reason", problem.reason,
		"operation", request.Operation, "subresource", request.SubResource)
	if problem.code == http.StatusForbidden {
		return admission.Denied(problem.Error())
	}
	return admission.Errored(problem.code, problem)
}
