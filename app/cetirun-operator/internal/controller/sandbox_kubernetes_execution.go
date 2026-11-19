// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"google.golang.org/protobuf/proto"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	sandboxExecutionRequeue = 5 * time.Second
	nativeStartAnnotation   = w.NativeStartAnnotation
)

// Recorded single-start lifetimes retain their owned execution contract.
// New native lifetimes use common reconciliation.
func (r *AgentReconciler) reconcileRecordedNativeSandbox(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	var namespaceErr, projectionErr error
	if r.AIGateway.Enabled {
		namespace := &corev1.Namespace{}
		namespaceErr = r.customReader().Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace)
		if namespaceErr == nil {
			namespaceErr = r.validateSandboxBootstrapNamespace(ctx, agent, namespace)
			if namespaceErr == nil {
				projectionErr = r.reconcileAIGateway(ctx, agent, true)
				projectionErr = errors.Join(projectionErr, r.updateStatus(ctx, agent))
			}
		}
	}
	var result ctrl.Result
	var executionErr error
	if namespaceErr == nil && projectionErr == nil && r.SandboxPreparation != nil && r.SandboxPreparation.NativeExecution {
		result, executionErr = r.reconcileKubernetesSandbox(ctx, agent)
	} else {
		result, executionErr = r.holdSandboxWithPolicyFence(ctx, agent, nil)
	}
	if apierrors.IsNotFound(namespaceErr) {
		namespaceErr = nil
	}
	return result, errors.Join(namespaceErr, projectionErr, executionErr)
}

func validateNativeSandboxProfile(attempt *sandboxPreparationAttempt) error {
	agent, box, policy := attempt.agent, attempt.resolution.Sandbox, attempt.resolution.AgentPolicy
	if agent.Spec.Agent.Type != api.AgentTypeOpenClaw ||
		box.Type != api.SandboxKata || box.Storage.Home.AccessMode != corev1.ReadWriteOncePod {
		return fmt.Errorf("%w: native execution requires OpenClaw, Kata and ReadWriteOncePod HOME", sandboxpolicy.ErrUnsupportedControl)
	}
	if !agent.Spec.SharedModelsEnabled() || agent.Spec.GitHubCopilotEnabled() ||
		agent.Spec.Tailscale != nil || agentIdentityRequested(agent) ||
		policy.EntraID.Enabled || policy.KubernetesAccess != nil || len(agent.Spec.Helm.Values) != 0 {
		return fmt.Errorf("%w: native execution supports shared Gateway models without sidecars, identity grants or Helm overrides", sandboxpolicy.ErrUnsupportedControl)
	}
	if len(policy.Network.AllowedInternalCIDRs) != 0 {
		return fmt.Errorf("%w: internal-network exceptions require the common executor", sandboxpolicy.ErrUnsupportedControl)
	}
	if policy.Network.Ingress.RequireMTLS {
		return fmt.Errorf("%w: managed ingress mTLS requires the common executor", sandboxpolicy.ErrUnsupportedControl)
	}
	if len(policy.Network.BlockedFQDNs) != 0 {
		return fmt.Errorf("%w: observed-SNI capture requires the common executor", sandboxpolicy.ErrUnsupportedControl)
	}
	if slices.ContainsFunc(policy.Network.BlockedPorts, func(port api.AgentPolicyPort) bool {
		return port.Protocol == corev1.ProtocolSCTP
	}) {
		return fmt.Errorf("%w: SCTP port rules require the common executor", sandboxpolicy.ErrUnsupportedControl)
	}
	if errs := sandbox.CheckNativeNetworkPolicy(policy.Network); len(errs) != 0 {
		return fmt.Errorf("%w: %w", sandboxpolicy.ErrUnsupportedControl, errs.ToAggregate())
	}
	return nil
}

func (r *AgentReconciler) checkNativeSandboxUnused(ctx context.Context, agent *api.Agent) error {
	receipt, _, err := r.readSandboxPreparationReceipt(ctx, agent)
	if apierrors.IsNotFound(err) && !controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) {
		return nil
	}
	if err != nil {
		return err
	}
	if receipt.Annotations[nativeStartAnnotation] != "" {
		return fmt.Errorf("%w: native execution is single-start; delete and recreate after a hold or configuration change", sandboxpolicy.ErrUnsupportedControl)
	}
	return nil
}

func sandboxGatewayProjectionReady(agent *api.Agent) bool {
	if agent == nil || agent.Status.Policy == nil || agent.Status.Policy.EffectivePolicyRef == nil ||
		agent.Status.Policy.ObservedGeneration != agent.Generation {
		return false
	}
	status := agent.Status.AIGateway
	return status != nil && status.Phase == api.AIGatewayPhaseReady && status.Routed && !status.Blocked &&
		status.PolicyRevision != "" && status.KeyID != "" &&
		status.PolicyRevision == agent.Status.Policy.Enforcement.Gateway.DesiredRevision
}

func (r *AgentReconciler) reconcileKubernetesSandbox(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	if r.SandboxPreparation == nil || !r.SandboxPreparation.NativeExecution || !r.AIGateway.Enabled || agent.Spec.Suspended ||
		!sandboxGatewayProjectionReady(agent) {
		return r.holdSandbox(ctx, agent)
	}
	if controllerutil.ContainsFinalizer(agent, sandbox.HoldFinalizer) {
		result, err := r.startKubernetesSandbox(ctx, agent)
		if err != nil {
			return r.failKubernetesSandbox(ctx, agent, err)
		}
		return result, nil
	}
	state, expected, err := r.readKubernetesSandboxExpectation(ctx, agent)
	if err != nil || expected.ExecutionHeld || !expected.SinglePod {
		return r.failKubernetesSandbox(ctx, agent, errors.Join(err, fmt.Errorf("native Sandbox start is not complete")))
	}
	result, err := r.observeKubernetesSandbox(ctx, agent, state, expected)
	if err != nil {
		return r.failKubernetesSandbox(ctx, agent, err)
	}
	return result, nil
}

func (r *AgentReconciler) failKubernetesSandbox(ctx context.Context, agent *api.Agent, cause error) (ctrl.Result, error) {
	result, holdErr := r.holdSandbox(ctx, agent)
	return result, errors.Join(cause, holdErr)
}

func (r *AgentReconciler) readKubernetesSandboxExpectation(ctx context.Context, agent *api.Agent) (*corev1.ConfigMap, *w.Expectation, error) {
	return r.readNativeSandboxExpectation(ctx, agent, false)
}

func (r *AgentReconciler) readNativeSandboxExpectation(ctx context.Context, agent *api.Agent, cleanup bool) (*corev1.ConfigMap, *w.Expectation, error) {
	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
		return nil, nil, err
	}
	if !sandboxBuiltinOwned(namespace, agent) || (!cleanup && !namespace.DeletionTimestamp.IsZero()) {
		return nil, nil, fmt.Errorf("native Sandbox namespace is not current and owned")
	}
	state := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: r.OperatorNamespace, Name: w.PodExpectationName(namespace.Name, namespace.UID)}
	if err := r.APIReader.Get(ctx, key, state); err != nil {
		return nil, nil, err
	}
	if state.UID == "" || state.ResourceVersion == "" || (!cleanup && !state.DeletionTimestamp.IsZero()) ||
		state.Annotations[w.AgentUIDAnnotation] != string(agent.UID) ||
		!equality.Semantic.DeepEqual(state.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}) {
		return nil, nil, fmt.Errorf("native Sandbox expectation ownership changed")
	}
	expected, err := decodeSandboxExecutionExpectation(state)
	if err != nil {
		return nil, nil, err
	}
	if expected.Agent != (w.ObjectIdentity{Name: agent.Name, UID: agent.UID}) ||
		expected.Namespace != (w.ObjectIdentity{Name: namespace.Name, UID: namespace.UID}) ||
		expected.WorkloadKind != w.AgentWorkload || expected.Workload.Name != instanceServiceName(agent.Name) ||
		expected.Binding.AgentUID != agent.UID || (!cleanup && expected.AgentGeneration != agent.Generation) {
		return nil, nil, fmt.Errorf("native Sandbox expectation differs from the current Agent")
	}
	return state, expected, nil
}

func (r *AgentReconciler) startKubernetesSandbox(ctx context.Context, requested *api.Agent) (ctrl.Result, error) {
	prepareContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := r.prepareSandboxHeld(prepareContext, requested)
	cancel()
	if err != nil {
		statusErr := r.sandboxPreparationStatus(ctx, requested, false, err)
		return ctrl.Result{RequeueAfter: sandboxExecutionRequeue}, errors.Join(err, statusErr)
	}
	agent := &api.Agent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: requested.Name}, agent); err != nil {
		return ctrl.Result{}, err
	}
	if agent.UID != requested.UID || agent.Generation != requested.Generation || agent.Spec.Suspended ||
		!agent.DeletionTimestamp.IsZero() || !sandboxGatewayProjectionReady(agent) {
		return ctrl.Result{}, fmt.Errorf("native Sandbox intent changed before start")
	}
	state, expected, err := r.readKubernetesSandboxExpectation(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	workload := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}
	if err := r.APIReader.Get(ctx, key, workload); err != nil {
		return ctrl.Result{}, err
	}
	if !expected.ExecutionHeld || !sandboxBuiltinOwned(workload, agent) ||
		workload.UID != expected.Workload.UID || !workload.DeletionTimestamp.IsZero() ||
		workload.Annotations[w.RevisionAnnotation] != expected.Revision ||
		!w.SamePodTemplate(workload.Spec.Template, expected.Template) ||
		workload.Status.UpdateRevision != expected.ControllerRevision || expected.ControllerRevision == "" ||
		ptr.Deref(workload.Spec.Replicas, -1) != 0 {
		return ctrl.Result{}, fmt.Errorf("native Sandbox prepared workload changed before start")
	}
	receipt, _, err := r.readSandboxPreparationReceipt(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if receipt.Annotations[nativeStartAnnotation] != "" {
		return ctrl.Result{}, fmt.Errorf("native Sandbox start was already submitted; automatic replay is unsupported")
	}
	expected.Sources = selectedSandboxExecutionSources(expected.Sources, expected.Binding)
	expected.SourceDigests = map[string]string{}
	for _, source := range expected.Sources {
		if !w.EffectiveSource(source) {
			continue
		}
		digest, err := r.nativeSourceDigest(ctx, source, agent.Spec.SandboxRef, true)
		if err != nil {
			return ctrl.Result{}, err
		}
		expected.SourceDigests[w.SourceKey(source)] = digest
	}
	if err := r.reserveNativeSandboxSources(ctx, agent, expected); err != nil {
		return ctrl.Result{}, err
	}
	// Agent admission validates this native snapshot while Pod creation remains
	// held. Persist it before requesting the one permitted Agent hold release.
	if err := r.saveNativeExpectation(ctx, state, expected); err != nil {
		return ctrl.Result{}, fmt.Errorf("record native start inputs: %w", err)
	}
	// Persist the one-shot boundary before admission can release a workload.
	// An uncertain start remains held and requires delete/recreate, not replay.
	baseReceipt := receipt.DeepCopy()
	if receipt.Annotations == nil {
		receipt.Annotations = map[string]string{}
	}
	receipt.Annotations[nativeStartAnnotation] = expected.Revision
	if err := r.Patch(ctx, receipt, client.MergeFromWithOptions(baseReceipt, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, fmt.Errorf("record native Sandbox start: %w", err)
	}
	finalizers := slices.DeleteFunc(slices.Clone(agent.Finalizers), func(value string) bool {
		return value == sandbox.HoldFinalizer
	})
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": agent.ResourceVersion, "finalizers": finalizers,
	}})
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Patch(ctx, agent, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return ctrl.Result{}, fmt.Errorf("release native Sandbox hold: %w", err)
	}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: agent.Name}, agent); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.checkNativeSandboxReservation(ctx, expected); err != nil {
		return ctrl.Result{}, err
	}
	base := workload.DeepCopy()
	workload.Spec.Replicas = ptr.To[int32](1)
	if err := r.Patch(ctx, workload, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, fmt.Errorf("start native Sandbox workload: %w", err)
	}
	if err := r.releaseKubernetesSandboxExpectation(ctx, state, expected, agent); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: sandboxExecutionRequeue},
		r.sandboxExecutionPendingStatus(ctx, agent, "SandboxStarting", "Waiting for the native Sandbox application.")
}

func (r *AgentReconciler) releaseKubernetesSandboxExpectation(ctx context.Context, state *corev1.ConfigMap, expected *w.Expectation, agent *api.Agent) error {
	if agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() || agent.UID != expected.Agent.UID ||
		agent.Generation != expected.AgentGeneration || controllerutil.ContainsFinalizer(agent, sandbox.HoldFinalizer) ||
		!sandboxGatewayProjectionReady(agent) {
		return fmt.Errorf("native Sandbox intent changed before admission release")
	}
	if err := r.checkNativeSandboxReservation(ctx, expected); err != nil {
		return err
	}
	agentSource, err := w.AgentSourceReference(agent)
	if err != nil {
		return err
	}
	found := false
	for i := range expected.Sources {
		if expected.Sources[i].IsAgent() {
			if found {
				return fmt.Errorf("native Sandbox expectation has duplicate Agent sources")
			}
			expected.Sources[i], found = agentSource, true
		}
	}
	if !found {
		return fmt.Errorf("native Sandbox expectation has no Agent source")
	}
	expected.ExecutionHeld, expected.SinglePod = false, true
	return r.saveNativeExpectation(ctx, state, expected)
}

func (r *AgentReconciler) saveNativeExpectation(ctx context.Context, state *corev1.ConfigMap, expected *w.Expectation) error {
	raw, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	base := state.DeepCopy()
	state.Data = map[string]string{w.PodExpectationKey: string(raw)}
	if expected.SinglePod {
		controllerutil.AddFinalizer(state, w.NativePodFinalizer)
	}
	return r.Patch(ctx, state, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *AgentReconciler) observeKubernetesSandbox(ctx context.Context, agent *api.Agent, state *corev1.ConfigMap, expected *w.Expectation) (ctrl.Result, error) {
	if err := r.checkNativeSandboxReservation(ctx, expected); err != nil {
		return ctrl.Result{}, err
	}
	attempt, err := r.resolveRunningKubernetesSandbox(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if expected.Binding != attempt.plan.Binding || expected.Revision != attempt.plan.Revision {
		return ctrl.Result{}, fmt.Errorf("native Sandbox configuration changed; recreate is required")
	}
	if err := r.verifyNativeSandboxSources(ctx, expected, agent); err != nil {
		return ctrl.Result{}, err
	}
	workload := &appsv1.StatefulSet{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}, workload); err != nil {
		return ctrl.Result{}, err
	}
	if !sandboxBuiltinOwned(workload, agent) || workload.UID != expected.Workload.UID || !workload.DeletionTimestamp.IsZero() ||
		ptr.Deref(workload.Spec.Replicas, 0) != 1 || workload.Status.UpdateRevision != expected.ControllerRevision ||
		!w.SamePodTemplate(workload.Spec.Template, expected.Template) {
		return ctrl.Result{}, fmt.Errorf("running native Sandbox workload differs from its expectation")
	}
	pod := &corev1.Pod{}
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name + "-0"}, pod)
	if apierrors.IsNotFound(err) && expected.PodUID == "" {
		return ctrl.Result{RequeueAfter: sandboxExecutionRequeue},
			r.sandboxExecutionPendingStatus(ctx, agent, "SandboxStarting", "Waiting for the native Sandbox Pod.")
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.recordNativePodLifetime(ctx, state, expected, pod); err != nil {
		return ctrl.Result{}, err
	}
	if !pod.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, fmt.Errorf("native Sandbox Pod is terminating; replacement is unsupported")
	}
	if pod.Spec.NodeName == "" || !sandboxPodReady(pod) {
		if agent.Status.Phase == api.PhaseReady {
			return ctrl.Result{}, fmt.Errorf("native Sandbox application readiness was lost")
		}
		return ctrl.Result{RequeueAfter: sandboxExecutionRequeue},
			r.sandboxExecutionPendingStatus(ctx, agent, "SandboxStarting", "Waiting for the native Sandbox application readiness probe.")
	}
	node := &corev1.Node{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
		return ctrl.Result{}, err
	}
	if node.UID == "" || !node.DeletionTimestamp.IsZero() || !sandboxNodeReady(node) {
		return ctrl.Result{}, fmt.Errorf("native Sandbox Node is not ready")
	}
	if err := w.ValidatePodOnNode(pod, node, *expected); err != nil {
		return ctrl.Result{}, err
	}
	current := &api.Agent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: agent.Name}, current); err != nil {
		return ctrl.Result{}, err
	}
	source, err := w.AgentSourceReference(current)
	if err != nil || !slices.Contains(expected.Sources, source) || !sandboxGatewayProjectionReady(current) {
		return ctrl.Result{}, fmt.Errorf("native Sandbox intent changed before readiness")
	}
	if err := r.checkNativeSandboxReservation(ctx, expected); err != nil {
		return ctrl.Result{}, err
	}
	if attempt.plan.Network.IngressMode == api.AgentPolicyIngressAuthenticated {
		if err := r.ensureNativeSandboxRoute(ctx, current); err != nil {
			return ctrl.Result{}, err
		}
		current.Status.AccessURL = accessURLPathBased(current.Name, r.DomainSuffix)
	} else {
		if err := r.withdrawKubernetesSandboxRoute(ctx, current); err != nil {
			return ctrl.Result{}, err
		}
		current.Status.AccessURL = ""
	}
	binding := attempt.plan.Binding
	current.Status.Namespace = expected.Namespace.Name
	current.Status.Sandbox = &api.SandboxStatus{
		TeamUID: binding.TeamUID, SandboxUID: binding.SandboxUID,
		AgentPolicyUID: binding.AgentPolicyUID, AppliedRevision: attempt.plan.Revision,
	}
	current.Status.Phase, current.Status.ObservedGeneration = api.PhaseReady, current.Generation
	for _, kind := range []string{api.ConditionSandboxReady, api.ConditionReady, api.ConditionIngressReady} {
		apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: kind, Status: metav1.ConditionTrue, Reason: "NativeSandboxReady",
			Message:            "The supported native workload is Ready and its Kubernetes configuration matches; physical failover fencing is not provided.",
			ObservedGeneration: current.Generation, LastTransitionTime: metav1.Now(),
		})
	}
	if err := r.updateStatus(ctx, current); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *AgentReconciler) resolveRunningKubernetesSandbox(ctx context.Context, agent *api.Agent) (*sandboxPreparationAttempt, error) {
	attempt, err := r.resolveSandboxExecutionInputs(ctx, agent, false)
	if err != nil {
		return nil, err
	}
	if err := validateNativeSandboxProfile(attempt); err != nil {
		return nil, err
	}
	source, ok := r.SandboxPreparation.Prerequisites.(interface {
		ResolveRunning(context.Context, *api.Agent, *api.Team) (SandboxPrerequisites, error)
	})
	if !ok {
		return nil, fmt.Errorf("native Sandbox running prerequisite reader is unavailable")
	}
	attempt.prerequisites, err = source.ResolveRunning(ctx, attempt.agent.DeepCopy(), attempt.team.DeepCopy())
	if err != nil {
		return nil, err
	}
	if err := r.inheritSandboxPlacement(ctx, attempt.agent, &attempt.prerequisites); err != nil {
		return nil, err
	}
	if err := r.sandboxPreparationInput(ctx, attempt); err != nil {
		return nil, err
	}
	platform := attempt.configuration.Platforms[attempt.resolution.Sandbox.Type]
	if err := sandboxpolicy.VerifyPolicyGrants(ctx, r.APIReader, attempt.selectedPolicy(), platform.Grants); err != nil {
		return nil, err
	}
	attempt.plan, err = sandboxpolicy.Compile(sandboxpolicy.CompileInput{
		NativeExecution: r.SandboxPreparation.NativeExecution,
		Resolution:      *attempt.resolution, AgentName: agent.Name, Namespace: attempt.input.Namespace.Name, Platform: platform,
	})
	return attempt, err
}

func (r *AgentReconciler) nativeSourceDigest(ctx context.Context, source w.SourceReference, box string, capture bool) (string, error) {
	if source.APIVersion != "v1" || source.Kind != "PersistentVolumeClaim" {
		return r.executionSourceDigest(ctx, source, box, capture)
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, claim); err != nil {
		return "", err
	}
	if claim.UID != source.UID || (capture && claim.ResourceVersion != source.ResourceVersion) {
		return "", fmt.Errorf("native Sandbox claim identity changed")
	}
	return w.NativeClaimDigest(claim)
}

func (r *AgentReconciler) verifyNativeSandboxSources(ctx context.Context, expected *w.Expectation, agent *api.Agent) error {
	digests := 0
	for _, source := range expected.Sources {
		switch {
		case source.APIVersion == "apps/v1" && source.Kind == "StatefulSet" &&
			source.UID == expected.Workload.UID && source.Name == expected.Workload.Name && source.Namespace == expected.Namespace.Name:
			// The complete live workload is checked separately after its scale.
		case w.EffectiveSource(source):
			digests++
			current, err := r.nativeSourceDigest(ctx, source, agent.Spec.SandboxRef, false)
			if err != nil {
				return err
			}
			if !w.ValidInputDigest(expected.SourceDigests[w.SourceKey(source)]) || current != expected.SourceDigests[w.SourceKey(source)] {
				return fmt.Errorf("native Sandbox execution source changed")
			}
		default:
			if _, err := r.readSandboxSource(ctx, source); err != nil {
				return err
			}
		}
	}
	if digests == 0 || len(expected.SourceDigests) != digests {
		return fmt.Errorf("native Sandbox source digests are incomplete")
	}
	return ctx.Err()
}

func sandboxPodReady(pod *corev1.Pod) bool {
	return pod != nil && slices.ContainsFunc(pod.Status.Conditions, func(condition corev1.PodCondition) bool {
		return condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
	})
}

func sandboxNodeReady(node *corev1.Node) bool {
	return node != nil && slices.ContainsFunc(node.Status.Conditions, func(condition corev1.NodeCondition) bool {
		return condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue
	})
}

func (r *AgentReconciler) sandboxExecutionPendingStatus(ctx context.Context, expected *api.Agent, reason, message string) error {
	agent := &api.Agent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: expected.Name}, agent); err != nil {
		return err
	}
	if agent.UID != expected.UID || agent.Generation != expected.Generation ||
		agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() {
		return fmt.Errorf("native Sandbox Agent changed before status")
	}
	agent.Status.Phase, agent.Status.AccessURL = api.PhaseProvisioning, ""
	for _, kind := range []string{api.ConditionReady, api.ConditionSandboxReady, api.ConditionIngressReady} {
		apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
			Type: kind, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: agent.Generation,
		})
	}
	return r.updateStatus(ctx, agent)
}

func (r *AgentReconciler) ensureNativeSandboxRoute(ctx context.Context, agent *api.Agent) error {
	if r.DomainSuffix == "" || r.IstioGateway == "" {
		return fmt.Errorf("native Sandbox authenticated ingress requires the configured domain and Gateway")
	}
	desired := buildVirtualService(agent, r.DomainSuffix, r.IstioGateway)
	if desired == nil {
		return fmt.Errorf("native Sandbox route is unsupported")
	}
	desired.OwnerReferences = []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}
	desired.Annotations = map[string]string{w.AgentUIDAnnotation: string(agent.UID)}
	existing := &istiov1beta1.VirtualService{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	if !sandboxBuiltinOwned(existing, agent) || !existing.DeletionTimestamp.IsZero() {
		return fmt.Errorf("native Sandbox route belongs to a different lifetime")
	}
	if proto.Equal(&existing.Spec, &desired.Spec) && maps.Equal(existing.Labels, desired.Labels) {
		return nil
	}
	base := existing.DeepCopy()
	existing.Spec.Hosts, existing.Spec.Gateways = desired.Spec.Hosts, desired.Spec.Gateways
	existing.Spec.Http, existing.Spec.ExportTo = desired.Spec.Http, desired.Spec.ExportTo
	existing.Labels = desired.Labels
	return r.Patch(ctx, existing, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *AgentReconciler) withdrawKubernetesSandboxRoute(ctx context.Context, agent *api.Agent) error {
	route := &istiov1beta1.VirtualService{}
	key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: virtualServiceName(agent.Name)}
	if err := r.APIReader.Get(ctx, key, route); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !sandboxBuiltinOwned(route, agent) {
		return fmt.Errorf("native Sandbox route ownership is unconfirmed")
	}
	return client.IgnoreNotFound(r.Delete(ctx, route, client.Preconditions{UID: &route.UID, ResourceVersion: &route.ResourceVersion}))
}
