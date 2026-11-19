// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// The durable primary HOME submission is independent of auxiliary writes.
	sandboxApplyStateAnnotation   = "sandbox.cetirun.microsoft.com/apply-state"
	sandboxApplyAttemptAnnotation = "sandbox.cetirun.microsoft.com/apply-attempt"
	sandboxApplyInitializing      = "initializing"
	sandboxApplyPending           = "pending"
	sandboxApplySubmitted         = "submitted"
	sandboxApplyCancelled         = "cancelled"
)

var errSandboxIntentChanged = errors.New("Sandbox intent changed")

func (r *AgentReconciler) usesReconciledSandbox(ctx context.Context, agent *api.Agent) (bool, error) {
	if !sandboxRequested(agent, nil) ||
		r.SandboxPreparation == nil || !r.SandboxPreparation.NativeExecution ||
		controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) {
		return false, nil
	}
	_, receipt, err := r.readSandboxBootstrap(ctx, agent)
	if apierrors.IsNotFound(err) && !controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read Sandbox lifetime mode: %w", err)
	}
	return receipt.Reconciled, nil
}

// suspendUnbootstrappedReconciledSandbox confirms the one case where a
// policy-bound Agent may be suspended before it has persistent HOME: no owned
// execution lifetime was ever created. It must run before policy resolution or
// bootstrap, because those paths can otherwise create the very evidence this
// check relies on. A confirmed existing lifetime falls through to normal stop
// handling. Any other durable or historical evidence is held Pending: it is
// unsafe to call bootstrap until an operator has resolved that evidence.
func (r *AgentReconciler) suspendUnbootstrappedReconciledSandbox(ctx context.Context, agent *api.Agent) (bool, ctrl.Result, error) {
	if !agent.Spec.Suspended || agent.Spec.SandboxRef == "" || agent.Spec.RestartRequestID != "" ||
		agent.Status.ObservedRestartRequestID != "" || agent.Spec.PlannedMigration != nil || agent.Status.PlannedMigration != nil ||
		controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) {
		return false, ctrl.Result{}, nil
	}
	// A protected receipt may describe either a completed lifetime or an
	// interrupted write. A complete inventory proof remains required before a
	// suspended lifetime can be treated as verified.  Stopping is narrower:
	// once the recorded namespace identity is confirmed, independently-owned
	// workload shutdown and Gateway key blocking must not be delayed by an
	// unrelated bootstrap inventory drift such as an operator Role update.
	// Otherwise do not touch a replacement namespace.
	if controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) {
		if err := r.verifySuspendedReconciledLifetime(ctx, agent); err != nil {
			return r.holdUnbootstrappedSuspension(ctx, agent)
		}
		return false, ctrl.Result{}, nil
	}
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err == nil {
		return false, ctrl.Result{}, nil
	} else if !apierrors.IsNotFound(err) {
		return r.holdUnbootstrappedSuspension(ctx, agent)
	}
	for _, key := range []client.ObjectKey{
		r.sandboxBootstrapKey(agent),
		r.sandboxPreparationKey(agent),
		{Namespace: r.sandboxGrantJournal().Namespace, Name: sandboxpolicy.GrantJournalName(agent.UID)},
	} {
		object := &corev1.ConfigMap{}
		if err := r.customReader().Get(ctx, key, object); err == nil {
			return r.holdUnbootstrappedSuspension(ctx, agent)
		} else if !apierrors.IsNotFound(err) {
			return r.holdUnbootstrappedSuspension(ctx, agent)
		}
	}
	if preBootstrapSuspensionHasHistory(agent) {
		return r.holdUnbootstrappedSuspension(ctx, agent)
	}
	agent.Status.Phase, agent.Status.AccessURL = api.PhaseSuspended, ""
	agent.Status.ObservedGeneration = agent.Generation
	for _, condition := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionSandboxReady, api.ConditionIngressReady} {
		setCondition(&agent.Status.Conditions, condition, metav1.ConditionFalse,
			"Suspended", "Suspension is confirmed before Sandbox bootstrap; provisioning waits for resume")
	}
	return true, ctrl.Result{RequeueAfter: 15 * time.Second}, r.updateStatus(ctx, agent)
}

// holdUnbootstrappedSuspension records an incomplete pause without creating a
// finalizer, contacting external services, or touching runtime resources. It
// handles receipt-before-finalizer interruptions and lost historical evidence
// conservatively: resuming is the explicit path that permits bootstrap again.
func (r *AgentReconciler) holdUnbootstrappedSuspension(ctx context.Context, agent *api.Agent) (bool, ctrl.Result, error) {
	agent.Status.Phase, agent.Status.AccessURL = api.PhasePending, ""
	for _, condition := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionSandboxReady, api.ConditionIngressReady} {
		setCondition(&agent.Status.Conditions, condition, metav1.ConditionFalse,
			"SuspensionPending", "Suspension requested; bootstrap evidence is incomplete")
	}
	return true, ctrl.Result{RequeueAfter: 15 * time.Second}, r.updateStatus(ctx, agent)
}

func preBootstrapSuspensionHasHistory(agent *api.Agent) bool {
	if agent.Status.Namespace != "" || agent.Status.HelmReleaseName != "" ||
		agent.Status.AIGateway != nil || agent.Status.Sandbox != nil ||
		agent.Status.HelmMaintenance != nil || agent.Status.RegistryCredentials != nil ||
		agent.Status.Phase == api.PhaseReady {
		return true
	}
	for _, condition := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionSandboxReady, api.ConditionIngressReady, api.ConditionHelmDeployed} {
		if isConditionTrue(agent.Status.Conditions, condition) {
			return true
		}
	}
	return false
}

func (r *AgentReconciler) bootstrapReconciledSandbox(ctx context.Context, agent *api.Agent) error {
	if r.SandboxPreparation.bootstrap == nil {
		return fmt.Errorf("Sandbox reconciliation requires owned bootstrap")
	}
	// Suspension never advances bootstrap. A complete receipt is sufficient for
	// the read-only verification this caller needs while the normal stop path
	// converges. A missing or partial receipt is held by the caller instead of
	// creating the next namespace, HOME, Secret, or workload object.
	if agent.Spec.Suspended {
		return r.verifySuspendedReconciledBootstrap(ctx, agent)
	}
	attempt, err := r.resolveSandboxInputs(ctx, agent, false, true)
	if err != nil {
		return err
	}
	if err := r.bootstrapSandboxOwned(ctx, attempt); err != nil {
		return err
	}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		return err
	}
	recorded, err := r.recordedReconciledSandbox(ctx, agent)
	if err != nil {
		return err
	}
	if !recorded {
		return fmt.Errorf("common Sandbox reconciliation requires its recorded bootstrap ownership; existing namespaces are not adopted")
	}
	if r.nativeConnectionsRequested(agent) && agent.Status.Namespace != instanceNamespace(agent.Name) {
		// Namespace ownership is already read back from bootstrap. Publish its
		// locator before Ready so credential readback cannot cycle on readiness.
		agent.Status.Namespace = instanceNamespace(agent.Name)
		if err := r.updateStatus(ctx, agent); err != nil {
			return err
		}
	}
	return nil
}

func (r *AgentReconciler) prepareReconciledSandbox(ctx context.Context, agent *api.Agent,
	prepared *helm.PreparedRelease, values map[string]any,
) (*helm.PreparedRelease, error) {
	attempt, err := r.resolveSandboxInputs(ctx, agent, false, true)
	if err != nil {
		return nil, err
	}
	source, ok := r.SandboxPreparation.Prerequisites.(interface {
		ResolveCurrent(context.Context, *api.Agent, *api.Team) (SandboxPrerequisites, error)
	})
	if !ok {
		return nil, fmt.Errorf("Sandbox prerequisites do not support current desired-state reconciliation")
	}
	attempt.prerequisites, err = source.ResolveCurrent(ctx, attempt.agent, attempt.team)
	if err != nil {
		return nil, err
	}
	if err := r.sandboxPreparationInput(ctx, attempt); err != nil {
		return nil, err
	}
	if err := sandboxEgressSNIInputs(attempt); err != nil {
		return nil, err
	}
	if attempt.input.Terminal != nil && !attempt.agent.Spec.Suspended {
		if err := r.ensureCommonTerminalExecPolicy(ctx, attempt.agent); err != nil {
			return nil, err
		}
		if err := r.ensureTerminalReadRoles(ctx, attempt.agent); err != nil {
			return nil, err
		}
		if err := r.ensureCommonTerminalReaderBinding(ctx, attempt.agent); err != nil {
			return nil, err
		}
	}
	if attempt.input.CustomAgent != nil {
		agent = attempt.agent
	}
	if err := r.sandboxIngressMTLSInputs(ctx, attempt); err != nil {
		return nil, fmt.Errorf("%w: %w", errSandboxIngressMTLS, err)
	}
	connections, err := r.nativeConnectionsInputs(ctx, attempt)
	if err != nil {
		return nil, err
	}
	if agent.Spec.Agent.RegistrySecretRef != nil {
		name, err := registryauth.PullSecretName(agent.UID)
		if err != nil {
			return nil, err
		}
		secret := &corev1.Secret{}
		if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: attempt.input.Namespace.Name, Name: name}, secret); err != nil {
			return nil, fmt.Errorf("read Sandbox registry pull Secret: %w", err)
		}
		receipt := agent.Status.RegistryCredentials
		if receipt == nil || receipt.SourceUID == "" || receipt.SourceResourceVersion == "" ||
			!isConditionTrue(agent.Status.Conditions, api.ConditionRegistryCredentialsReady) ||
			secret.UID == "" || secret.ResourceVersion == "" || string(secret.UID) != receipt.PullSecretUID ||
			secret.Annotations[registryauth.SourceUID] != receipt.SourceUID ||
			secret.Annotations[registryauth.SourceVersion] != receipt.SourceResourceVersion ||
			registryauth.Validate(secret, agent) != nil {
			return nil, fmt.Errorf("Sandbox registry pull Secret does not match its synchronized approval")
		}
		attempt.input.ImagePullSecret = &w.ObjectIdentity{Name: secret.Name, UID: secret.UID}
		attempt.sources = append(attempt.sources, sandboxSource(secret, "v1", "Secret"))
	}
	platformValues, user, err := r.sandboxHelmValues(agent, attempt.team)
	if err != nil {
		return nil, err
	}
	if err := w.ValidateReconciledUserValues(user, attempt.input.AuthorizedSecrets); err != nil {
		return nil, err
	}
	if attempt.input.CustomAgent != nil {
		values = platformValues
		if len(user) != 0 {
			values = deepMerge(values, user).(map[string]any)
		}
		values["memoryOptions"] = r.memoryOptions()
		if agent.Spec.Suspended {
			values["replicaCount"] = 0
		}
	}
	configuration, ok := r.SandboxPreparation.Configuration.(*sandboxplatform.KubernetesSource)
	if !ok {
		return nil, fmt.Errorf("common Sandbox reconciliation requires the configured Kubernetes authority")
	}
	authority := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: configuration.Namespace, Name: configuration.Name}, authority); err != nil {
		return nil, err
	}
	attempt.sources = append(attempt.sources, sandboxSource(authority, "v1", "ConfigMap"))
	snapshot := attempt.configuration
	snapshot.ObservedAt = time.Time{}
	snapshotDigest, err := sandboxPreparationDigest(snapshot)
	if err != nil {
		return nil, err
	}
	platform := attempt.configuration.Platforms[attempt.resolution.Sandbox.Type]
	if err := sandboxpolicy.VerifyPolicyGrants(ctx, r.APIReader, attempt.selectedPolicy(), platform.Grants); err != nil {
		return nil, err
	}
	var customIngress *sandboxpolicy.CustomIngress
	if attempt.input.CustomAgent != nil {
		customIngress = r.customSandboxIngress(agent)
	}
	identityPin, err := r.sandboxIdentityPin(ctx, attempt)
	if err != nil {
		return nil, err
	}
	attempt.plan, err = sandboxpolicy.Compile(sandboxpolicy.CompileInput{
		ConnectionsMCP:  connections,
		NativeExecution: true, Resolution: *attempt.resolution, AgentName: agent.Name,
		Namespace: attempt.input.Namespace.Name, ServiceName: attempt.input.WorkloadName, Platform: platform,
		CustomIngress: customIngress, TerminalIngress: terminalSandboxIngress(attempt.input.Terminal),
		IdentityPin: identityPin,
	})
	if err != nil {
		return nil, err
	}
	roleSources, err := reconciledKubernetesRoleSources(attempt.plan, platform)
	if err != nil {
		return nil, err
	}
	attempt.sources = append(attempt.sources, roleSources...)
	if err := r.syncSandboxIdentity(ctx, attempt); err != nil {
		return nil, err
	}
	attempt.input.Controls, err = w.ControlsFromPlan(*attempt.plan)
	if err != nil {
		return nil, err
	}
	bundle, err := w.Build(attempt.input)
	if err != nil {
		return nil, err
	}
	if prepared == nil {
		prepared, err = r.SandboxPreparation.Installer.PrepareSandbox(attempt.input.Namespace.Name, values, nil, agent.Spec.Helm.Version, bundle)
		if err != nil {
			return nil, err
		}
	}
	if attempt.input.CustomAgent != nil {
		r.applyCopilotChartValues(values, agent, prepared)
	}
	sources := selectedSandboxExecutionSources(attempt.sources, attempt.resolution.Binding)
	agentSource, err := w.AgentSourceReference(agent)
	if err != nil {
		return nil, err
	}
	sources = append(sources, agentSource)
	digests := make(map[string]string, len(sources))
	for _, source := range sources {
		if !w.EffectiveSource(source) {
			continue
		}
		if source.APIVersion == api.GroupVersion.String() && source.Kind == "AgentPolicy" &&
			source.UID == attempt.resolution.Binding.AgentPolicyUID {
			digests[w.SourceKey(source)] = attempt.policyDigest
			continue
		}
		digest, err := r.nativeSourceDigest(ctx, source, agent.Spec.SandboxRef, false)
		if err != nil {
			return nil, err
		}
		digests[w.SourceKey(source)] = digest
	}
	var published *corev1.ConfigMap
	var reservation *sandboxcoordination.NativeTicket
	var migrationTemplate *corev1.PodSpec
	var desiredGrants []sandboxpolicy.BindingReference
	grantsRecorded := false
	guard := func(ctx context.Context) error {
		if err := r.checkMigrationApply(ctx, agent, migrationTemplate); err != nil {
			return err
		}
		if reservation != nil {
			if err := r.SandboxNativeCoordination.CheckNative(ctx, *reservation); err != nil {
				return err
			}
		}
		if published != nil {
			current := &corev1.ConfigMap{}
			if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(published), current); err != nil {
				return err
			}
			if _, err := w.ReadReconcileExpectation(current); err != nil {
				return err
			}
			if current.UID != published.UID || current.ResourceVersion != published.ResourceVersion ||
				!reflect.DeepEqual(current.Data, published.Data) ||
				current.Annotations[sandboxApplyStateAnnotation] == sandboxApplyCancelled {
				return fmt.Errorf("Sandbox desired authority changed before apply")
			}
		}
		if err := configurationSnapshotTime(attempt.configuration.ObservedAt, configurationNow(r.SandboxPreparation.Now), r.SandboxPreparation.MaxSnapshotAge); err != nil {
			return err
		}
		currentConfiguration, err := configurationSnapshot(ctx, r.SandboxPreparation.Configuration, r.SandboxPreparation.Now, r.SandboxPreparation.MaxSnapshotAge)
		if err != nil {
			return err
		}
		currentConfiguration.ObservedAt = time.Time{}
		digest, err := sandboxPreparationDigest(currentConfiguration)
		if err != nil || digest != snapshotDigest {
			return fmt.Errorf("Sandbox platform approvals changed before reconciliation")
		}
		if err := sandboxpolicy.VerifyPolicyGrants(ctx, r.APIReader, attempt.selectedPolicy(), platform.Grants); err != nil {
			return err
		}
		current, binding, err := sandboxbinding.Read(ctx, r.customReader(), client.ObjectKeyFromObject(agent))
		if err != nil {
			return err
		}
		if current.UID != agent.UID || current.Generation != agent.Generation ||
			current.Spec.Suspended != agent.Spec.Suspended || !current.DeletionTimestamp.IsZero() {
			return fmt.Errorf("%w before resource reconciliation", errSandboxIntentChanged)
		}
		if binding != attempt.resolution.Binding {
			return fmt.Errorf("Sandbox binding changed before resource reconciliation")
		}
		for _, source := range sources {
			if !w.EffectiveSource(source) {
				if err := r.verifySandboxSources(ctx, []w.SourceReference{source}); err != nil {
					return err
				}
				continue
			}
			digest, err := r.nativeSourceDigest(ctx, source, current.Spec.SandboxRef, false)
			if err != nil {
				return err
			}
			if digest != digests[w.SourceKey(source)] {
				return fmt.Errorf("%w: Sandbox input changed before resource reconciliation", errSandboxSourceChanged)
			}
		}
		if grantsRecorded {
			return r.sandboxGrantJournal().RefreshBindingIntents(ctx, agent.Name, agent.UID, desiredGrants)
		}
		return nil
	}
	renderGuard := func(ctx context.Context) error {
		if published == nil || published.Annotations[sandboxApplyStateAnnotation] == sandboxApplyInitializing {
			return fmt.Errorf("Sandbox apply requires an initialized expectation")
		}
		if err := guard(ctx); err != nil {
			return err
		}
		claimed := published.DeepCopy()
		// Fence competing renderers without declaring a workload submission.
		metav1.SetMetaDataAnnotation(&claimed.ObjectMeta, sandboxApplyAttemptAnnotation, rand.Text())
		if err := r.Patch(ctx, claimed, client.MergeFromWithOptions(published, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("claim Sandbox apply attempt: %w", err)
		}
		published = claimed
		return guard(ctx)
	}
	applyGuard := func(ctx context.Context) error {
		if err := renderGuard(ctx); err != nil {
			return err
		}
		if published.Annotations[sandboxApplyStateAnnotation] == sandboxApplyPending {
			submitted := published.DeepCopy()
			submitted.Annotations[sandboxApplyStateAnnotation] = sandboxApplySubmitted
			if err := r.Patch(ctx, submitted, client.MergeFromWithOptions(published, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("record Sandbox apply submission: %w", err)
			}
			published = submitted
		}
		return nil
	}
	bindingGuard := func(ctx context.Context, object unstructured.Unstructured) (func(context.Context) error, error) {
		if published == nil {
			return nil, fmt.Errorf("Kubernetes grant creation requires its published expectation")
		}
		index := slices.IndexFunc(desiredGrants, func(ref sandboxpolicy.BindingReference) bool {
			return ref.Kind == object.GetKind() && ref.Namespace == object.GetNamespace() && ref.Name == object.GetName()
		})
		if index < 0 {
			return nil, fmt.Errorf("Kubernetes grant creation has no recorded binding identity")
		}
		if err := guard(ctx); err != nil {
			return nil, err
		}
		claimed := published.DeepCopy()
		metav1.SetMetaDataAnnotation(&claimed.ObjectMeta, sandboxApplyAttemptAnnotation, rand.Text())
		if err := r.Patch(ctx, claimed, client.MergeFromWithOptions(published, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, fmt.Errorf("claim Kubernetes grant creation: %w", err)
		}
		published = claimed
		if err := guard(ctx); err != nil {
			return nil, err
		}
		submitted := claimed.DeepCopy()
		requested := desiredGrants[index]
		if err := r.recordReconciledGrantSubmission(ctx, agent, submitted, requested); err != nil {
			return nil, err
		}
		if err := r.Patch(ctx, submitted, client.MergeFromWithOptions(claimed, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, fmt.Errorf("record Kubernetes grant submission: %w", err)
		}
		published = submitted
		return func(ctx context.Context) error {
			return r.rejectReconciledGrantSubmission(ctx, submitted, requested)
		}, nil
	}
	workloadCreateGuard := func(ctx context.Context) (func(context.Context) error, error) {
		firstSubmission := published != nil && published.Annotations[sandboxApplyStateAnnotation] == sandboxApplyPending
		if err := applyGuard(ctx); err != nil {
			return nil, err
		}
		if !firstSubmission {
			return nil, nil
		}
		submitted := published.DeepCopy()
		return func(ctx context.Context) error {
			return r.rejectReconciledWorkloadSubmission(ctx, submitted)
		}, nil
	}
	result, err := prepared.ReconcileSandboxWithPreflight(values, bundle, guard, renderGuard, applyGuard, bindingGuard, workloadCreateGuard)
	if err != nil {
		return nil, err
	}
	expected, err := result.ReconcileExpectation()
	if err != nil {
		return nil, err
	}
	for _, workload := range expected.Workloads {
		if workload.Kind == "StatefulSet" {
			migrationTemplate = workload.Template.Spec.DeepCopy()
		}
	}
	for _, object := range expected.Controls {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(object.GroupVersionKind())
		err := r.customReader().Get(ctx, client.ObjectKeyFromObject(&object), current)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !sandboxBuiltinOwned(current, agent) || !current.GetDeletionTimestamp().IsZero() {
			return nil, fmt.Errorf("Sandbox refuses to adopt an unowned or terminating chart resource")
		}
	}
	var pods []corev1.PodSpec
	for _, workload := range expected.Workloads {
		pods = append(pods, workload.Template.Spec)
		for _, source := range w.PodSourceReferences(workload.Template.Spec, expected.Namespace.Name) {
			if source.Kind != "Secret" && source.Kind != "ConfigMap" && source.Kind != "ServiceAccount" {
				continue
			}
			if slices.ContainsFunc(sources, func(existing w.SourceReference) bool { return w.SourceKey(source) == w.SourceKey(existing) }) {
				continue
			}
			if slices.ContainsFunc(expected.Controls, func(object unstructured.Unstructured) bool {
				return object.GetKind() == source.Kind && object.GetNamespace() == source.Namespace && object.GetName() == source.Name
			}) {
				continue
			}
			object := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: source.Kind}}
			if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, object); err != nil {
				return nil, err
			}
			defaultAccount := source.Kind == "ServiceAccount" && source.Name == "default"
			kubernetesCA := source.Kind == "ConfigMap" && source.Name == "kube-root-ca.crt" &&
				attempt.resolution.AgentPolicy.KubernetesAccess != nil
			if kubernetesCA {
				if err := r.validateReconciledKubernetesCA(ctx, source); err != nil {
					return nil, err
				}
			}
			terminalCA := attempt.input.Terminal != nil && source.Kind == "ConfigMap" &&
				source.Name == "kube-root-ca.crt" && source.Namespace == expected.Namespace.Name
			if (!defaultAccount && !kubernetesCA && !terminalCA && !sandboxBuiltinOwned(object, agent)) || !object.DeletionTimestamp.IsZero() {
				return nil, fmt.Errorf("Sandbox component source is not owned by the current Agent")
			}
			source = sandboxSource(object, "v1", source.Kind)
			sources = append(sources, source)
			if source.Kind == "Secret" {
				attempt.input.AuthorizedSecrets = append(attempt.input.AuthorizedSecrets, source.Name)
			} else {
				digest, err := r.nativeSourceDigest(ctx, source, agent.Spec.SandboxRef, false)
				if err != nil {
					return nil, err
				}
				digests[w.SourceKey(source)] = digest
			}
		}
	}
	expected.Sources, expected.SourceDigests = sources, digests
	role, binding, access, err := r.sandboxSourceAccess(attempt, pods...)
	if err != nil {
		return nil, err
	}
	type sourceAccess struct {
		role    *rbacv1.Role
		binding *rbacv1.RoleBinding
		ref     sandboxpolicy.BindingReference
	}
	var tlsSources []sourceAccess
	if plan := attempt.input.Controls.IngressMTLS; plan != nil {
		for _, secret := range plan.ClientSecrets() {
			role, binding, ref, err := r.sandboxMTLSSourceAccess(attempt, secret)
			if err != nil {
				return nil, err
			}
			tlsSources = append(tlsSources, sourceAccess{role, binding, ref})
		}
	}
	journal := r.sandboxGrantJournal()
	previous, err := journal.Read(ctx, agent.Name, agent.UID)
	missingJournal := apierrors.IsNotFound(err)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	for index := range expected.Workloads {
		workload := &expected.Workloads[index]
		current := &unstructured.Unstructured{}
		current.SetAPIVersion("apps/v1")
		current.SetKind(workload.Kind)
		err := r.customReader().Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: workload.Identity.Name}, current)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !sandboxBuiltinOwned(current, agent) {
			return nil, fmt.Errorf("Sandbox refuses to adopt an unowned workload")
		}
		workload.Identity.UID = current.GetUID()
	}
	if err := guard(ctx); err != nil {
		return nil, err
	}
	desired, err := expected.Object(r.OperatorNamespace)
	if err != nil {
		return nil, err
	}
	unobserved := !slices.ContainsFunc(expected.Workloads, func(workload w.RenderedWorkload) bool {
		return workload.Identity.UID != ""
	})
	current := &corev1.ConfigMap{}
	err = r.customReader().Get(ctx, client.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		if err := r.confirmReconciledGrantOutcomes(ctx, agent, current, previous); err != nil {
			return nil, err
		}
		// Persist non-submission before the journal so either publication can
		// retry without losing the proof. Older or lost records stay uncertain.
		if missingJournal && unobserved {
			desired.Annotations[sandboxApplyStateAnnotation] = sandboxApplyInitializing
		}
		err = r.Create(ctx, desired)
		current = desired.DeepCopy()
	} else if err == nil {
		recorded, readErr := w.ReadReconcileExpectation(current)
		if readErr != nil || recorded.Agent != expected.Agent || recorded.Namespace != expected.Namespace || recorded.Home != expected.Home {
			return nil, fmt.Errorf("Sandbox expectation cannot replace a different recorded lifetime")
		}
		if missingJournal && current.Annotations[sandboxApplyStateAnnotation] != sandboxApplyInitializing {
			return nil, fmt.Errorf("Sandbox grant authority is missing for an existing execution")
		}
		if err := r.confirmReconciledGrantOutcomes(ctx, agent, current, previous); err != nil {
			return nil, err
		}
		switch state := current.Annotations[sandboxApplyStateAnnotation]; state {
		case sandboxApplyInitializing:
			if !unobserved || len(previous) != 0 {
				return nil, fmt.Errorf("initializing Sandbox expectation has recorded grants or an observed workload")
			}
			desired.Annotations[sandboxApplyStateAnnotation] = state
		case sandboxApplyPending, sandboxApplyCancelled:
			if reconciledPrimaryObserved(expected) {
				return nil, fmt.Errorf("unsubmitted Sandbox expectation has an observed primary workload")
			}
			desired.Annotations[sandboxApplyStateAnnotation] = sandboxApplyPending
		case sandboxApplySubmitted:
			desired.Annotations[sandboxApplyStateAnnotation] = state
		case "":
		default:
			return nil, fmt.Errorf("Sandbox apply checkpoint is invalid")
		}
		if attempt := current.Annotations[sandboxApplyAttemptAnnotation]; attempt != "" {
			metav1.SetMetaDataAnnotation(&desired.ObjectMeta, sandboxApplyAttemptAnnotation, attempt)
		}
		desired.Annotations[sandboxGrantSubmissionsAnnotation] = "[]"
		if !reflect.DeepEqual(current.Data, desired.Data) || !reflect.DeepEqual(current.Annotations, desired.Annotations) {
			base := current.DeepCopy()
			current.Data, current.Annotations = desired.Data, desired.Annotations
			err = r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		}
	}
	if err != nil {
		return nil, err
	}
	if _, present := current.Annotations[sandboxGrantSubmissionsAnnotation]; !present {
		before := current.DeepCopy()
		metav1.SetMetaDataAnnotation(&current.ObjectMeta, sandboxGrantSubmissionsAnnotation, "[]")
		if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, fmt.Errorf("initialize Kubernetes grant submission inventory: %w", err)
		}
	}
	initializing := current.Annotations[sandboxApplyStateAnnotation] == sandboxApplyInitializing
	if missingJournal || initializing {
		if err := journal.Record(ctx, agent.Name, agent.UID, nil); err != nil {
			return nil, err
		}
	}
	if initializing {
		if err := guard(ctx); err != nil {
			return nil, err
		}
		// The journal is confirmed before this CAS; no grant or workload write
		// is allowed while the expectation is still initializing.
		base := current.DeepCopy()
		current.Annotations[sandboxApplyStateAnnotation] = sandboxApplyPending
		if err := r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, fmt.Errorf("finish Sandbox expectation initialization: %w", err)
		}
	}
	accessGrants := []sandboxpolicy.BindingReference{access}
	for _, source := range tlsSources {
		accessGrants = append(accessGrants, source.ref)
	}
	desiredGrants = accessGrants
	if !agent.Spec.Suspended {
		desiredGrants = append(desiredGrants, attempt.plan.Identity.Bindings...)
	}
	if err := journal.Revoke(ctx, agent.Name, agent.UID, sandboxpolicy.ObsoleteBindings(previous, desiredGrants)); err != nil {
		return nil, err
	}
	if err := journal.Record(ctx, agent.Name, agent.UID, desiredGrants); err != nil {
		return nil, err
	}
	grantsRecorded = true
	if err := guard(ctx); err != nil {
		return nil, err
	}
	if err := r.applySandboxSourceAccess(ctx, agent, role, binding, access); err != nil {
		return nil, err
	}
	for _, source := range tlsSources {
		if err := r.applySandboxSourceAccess(ctx, agent, source.role, source.binding, source.ref); err != nil {
			return nil, err
		}
	}
	confirmed, err := r.sandboxBindingUIDs(ctx, accessGrants)
	if err != nil {
		return nil, err
	}
	if err := journal.Record(ctx, agent.Name, agent.UID, confirmed); err != nil {
		return nil, err
	}
	published = current.DeepCopy()
	if r.SandboxNativeCoordination != nil && !agent.Spec.Suspended {
		intent, err := sandboxcoordination.ReconciledIntentFor(expected)
		if err != nil {
			return nil, err
		}
		ticket, err := r.SandboxNativeCoordination.AcquireNative(ctx, intent)
		if err != nil {
			return nil, err
		}
		reservation = &ticket
	}
	return result, guard(ctx)
}

func (r *AgentReconciler) readReconciledSandboxExpectation(ctx context.Context, agent *api.Agent) (*corev1.ConfigMap, w.ReconcileExpectation, error) {
	var empty w.ReconcileExpectation
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
		return nil, empty, err
	}
	if !sandboxBuiltinOwned(namespace, agent) || !namespace.DeletionTimestamp.IsZero() {
		return nil, empty, fmt.Errorf("common Sandbox namespace is not current and owned")
	}
	state := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{
		Namespace: r.OperatorNamespace, Name: w.PodExpectationName(namespace.Name, namespace.UID),
	}, state); err != nil {
		return nil, empty, err
	}
	if state.Annotations[w.AgentUIDAnnotation] != string(agent.UID) ||
		!reflect.DeepEqual(state.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}) {
		return nil, empty, fmt.Errorf("common Sandbox expectation ownership changed")
	}
	expected, err := w.ReadReconcileExpectation(state)
	if err != nil {
		return nil, empty, err
	}
	if expected.Agent != (w.ObjectIdentity{Name: agent.Name, UID: agent.UID}) ||
		expected.AgentGeneration != agent.Generation ||
		expected.Namespace != (w.ObjectIdentity{Name: namespace.Name, UID: namespace.UID}) {
		return nil, empty, fmt.Errorf("common Sandbox expectation differs from the current Agent")
	}
	return state, expected, nil
}

func (r *AgentReconciler) observeReconciledSandbox(ctx context.Context, agent *api.Agent, prepared *helm.PreparedRelease) error {
	expected, err := prepared.ReconcileExpectation()
	if err != nil {
		return err
	}
	state := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{
		Namespace: r.OperatorNamespace, Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID),
	}, state); err != nil {
		return err
	}
	current, err := w.ReadReconcileExpectation(state)
	if err != nil || current.Agent != expected.Agent || current.PlanHash != expected.PlanHash ||
		current.Revision != expected.Revision || current.AgentGeneration != agent.Generation {
		return fmt.Errorf("Sandbox desired expectation changed during reconciliation")
	}
	if err := r.verifyReconciledIdentity(ctx, current); err != nil {
		return err
	}
	for _, desired := range current.Controls {
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(desired.GroupVersionKind())
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(&desired), actual); err != nil {
			return err
		}
		if !w.ReconciledObjectMatches(actual, &desired) {
			return fmt.Errorf("Sandbox enforced resource differs from the current desired controls")
		}
	}
	binding := current.Binding
	agent.Status.Sandbox = &api.SandboxStatus{
		TeamUID: binding.TeamUID, SandboxUID: binding.SandboxUID,
		AgentPolicyUID: binding.AgentPolicyUID, AppliedRevision: current.Revision,
	}
	setCondition(&agent.Status.Conditions, api.ConditionSandboxReady, metav1.ConditionTrue,
		"ReconciledSandboxReady", "Selected Sandbox configuration is applied through common reconciliation; physical failover fencing is not provided")
	for index := range agent.Status.Conditions {
		if agent.Status.Conditions[index].Type == api.ConditionSandboxReady {
			agent.Status.Conditions[index].ObservedGeneration = agent.Generation
		}
	}
	return nil
}

func (r *AgentReconciler) reconciledSandboxApplied(ctx context.Context, agent *api.Agent, prepared *helm.PreparedRelease) (bool, error) {
	expected, err := prepared.ReconcileExpectation()
	if err != nil {
		return false, err
	}
	if expected.IdentityServiceAccount != nil {
		state := &corev1.ConfigMap{}
		if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace,
			Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}, state); err != nil {
			return false, err
		}
		current, err := w.ReadReconcileExpectation(state)
		if err != nil || current.PlanHash != expected.PlanHash || current.Revision != expected.Revision {
			return false, fmt.Errorf("current identity expectation changed before apply observation")
		}
		if err := r.verifyReconciledIdentity(ctx, current); err != nil {
			return false, err
		}
	}
	for _, desired := range expected.Controls {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(desired.GroupVersionKind())
		err := r.customReader().Get(ctx, client.ObjectKeyFromObject(&desired), current)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !w.ReconciledObjectMatches(current, &desired) {
			return false, nil
		}
	}
	for _, desired := range expected.Workloads {
		key := client.ObjectKey{Namespace: expected.Namespace.Name, Name: desired.Identity.Name}
		var object client.Object
		var replicas *int32
		var template corev1.PodTemplateSpec
		if desired.Kind == "StatefulSet" {
			current := &appsv1.StatefulSet{}
			err = r.customReader().Get(ctx, key, current)
			object, replicas, template = current, current.Spec.Replicas, current.Spec.Template
		} else {
			current := &appsv1.Deployment{}
			err = r.customReader().Get(ctx, key, current)
			object, replicas, template = current, current.Spec.Replicas, current.Spec.Template
		}
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !sandboxBuiltinOwned(object, agent) || !object.GetDeletionTimestamp().IsZero() {
			return false, fmt.Errorf("Sandbox workload ownership changed during reconciliation")
		}
		if ptr.Deref(replicas, 1) != desired.Replicas || !w.SamePodTemplate(template, desired.Template) {
			return false, nil
		}
	}
	return true, nil
}

func (r *AgentReconciler) fenceReconciledGrantRevocation(ctx context.Context, state *corev1.ConfigMap) error {
	before := state.DeepCopy()
	metav1.SetMetaDataAnnotation(&state.ObjectMeta, sandboxApplyAttemptAnnotation, rand.Text())
	if err := r.Patch(ctx, state, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("fence unsubmitted Kubernetes grants: %w", err)
	}
	return nil
}

func (r *AgentReconciler) cancelUnsubmittedReconciledApply(ctx context.Context, state *corev1.ConfigMap, expected w.ReconcileExpectation) (bool, error) {
	applyState := state.Annotations[sandboxApplyStateAnnotation]
	if applyState != sandboxApplyPending && applyState != sandboxApplyCancelled {
		return false, nil
	}
	if reconciledPrimaryObserved(expected) {
		return false, fmt.Errorf("unsubmitted Sandbox expectation has a recorded primary workload")
	}
	if applyState == sandboxApplyPending {
		// Compete with submission on its resource version; absence alone cannot
		// cancel an outstanding grant or workload write.
		before := state.DeepCopy()
		state.Annotations[sandboxApplyStateAnnotation] = sandboxApplyCancelled
		if err := r.Patch(ctx, state, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, fmt.Errorf("cancel unsubmitted Sandbox apply: %w", err)
		}
	}
	return true, nil
}

func reconciledPrimaryObserved(expected w.ReconcileExpectation) bool {
	return slices.ContainsFunc(expected.Workloads, func(workload w.RenderedWorkload) bool {
		return workload.Kind == "StatefulSet" && workload.Identity.UID != ""
	})
}

func (r *AgentReconciler) rejectReconciledWorkloadSubmission(ctx context.Context, submitted *corev1.ConfigMap) error {
	expected, err := w.ReadReconcileExpectation(submitted)
	if err != nil || submitted.UID == "" || submitted.ResourceVersion == "" ||
		submitted.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted ||
		reconciledPrimaryObserved(expected) {
		return fmt.Errorf("workload rejection requires its first unobserved submission")
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": submitted.UID, "resourceVersion": submitted.ResourceVersion,
		"annotations": map[string]string{sandboxApplyStateAnnotation: sandboxApplyCancelled},
	}})
	if err != nil {
		return err
	}
	return r.Patch(ctx, submitted.DeepCopy(), client.RawPatch(types.MergePatchType, patch))
}

func (r *AgentReconciler) stopReconciledReservation(ctx context.Context, agent *api.Agent, ticket sandboxcoordination.NativeTicket) error {
	intent := ticket.Reservation.Intent
	state := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{
		Namespace: r.OperatorNamespace, Name: w.PodExpectationName(intent.Namespace.Name, intent.Namespace.UID),
	}, state); err != nil {
		return err
	}
	expected, err := w.ReadReconcileExpectationForCleanup(state)
	if err != nil || expected.Agent != intent.Agent || expected.Namespace != intent.Namespace ||
		expected.Home != intent.Home || expected.Binding != intent.Binding {
		return fmt.Errorf("common Sandbox cleanup differs from its source reservation")
	}
	if _, err := r.cancelUnsubmittedReconciledApply(ctx, state, expected); err != nil {
		return err
	}
	applyState := state.Annotations[sandboxApplyStateAnnotation]
	if applyState != sandboxApplyCancelled && applyState != "" && applyState != sandboxApplySubmitted {
		return fmt.Errorf("Sandbox apply checkpoint is invalid")
	}
	for _, desired := range expected.Workloads {
		key := client.ObjectKey{Namespace: expected.Namespace.Name, Name: desired.Identity.Name}
		var current client.Object
		stopped := false
		if desired.Kind == "StatefulSet" {
			workload := &appsv1.StatefulSet{}
			err = r.customReader().Get(ctx, key, workload)
			current = workload
			stopped = ptr.Deref(workload.Spec.Replicas, 1) == 0 &&
				workload.Status.ObservedGeneration >= workload.Generation && workload.Status.Replicas == 0
		} else {
			workload := &appsv1.Deployment{}
			err = r.customReader().Get(ctx, key, workload)
			current = workload
			stopped = ptr.Deref(workload.Spec.Replicas, 1) == 0 &&
				workload.Status.ObservedGeneration >= workload.Generation && workload.Status.Replicas == 0
		}
		if apierrors.IsNotFound(err) && desired.Kind == "StatefulSet" && applyState == sandboxApplyCancelled {
			continue
		}
		if apierrors.IsNotFound(err) && desired.Kind == "Deployment" {
			// A removed or never-created auxiliary has no HOME writer. Its
			// ReplicaSets must be gone; namespace Pods are checked below.
			var replicas appsv1.ReplicaSetList
			if err := r.customReader().List(ctx, &replicas, client.InNamespace(expected.Namespace.Name)); err != nil {
				return fmt.Errorf("observe removed Sandbox auxiliary children: %w", err)
			}
			for _, replica := range replicas.Items {
				if slices.ContainsFunc(replica.OwnerReferences, func(owner metav1.OwnerReference) bool {
					return owner.Kind == "Deployment" && (owner.Name == desired.Identity.Name ||
						(desired.Identity.UID != "" && owner.UID == desired.Identity.UID))
				}) {
					return fmt.Errorf("common Sandbox source reservation waits for removed auxiliary ReplicaSets")
				}
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("observe common controller shutdown: %w", err)
		}
		if !stopped || !sandboxBuiltinOwned(current, agent) ||
			(desired.Identity.UID != "" && desired.Identity.UID != current.GetUID()) {
			return fmt.Errorf("common Sandbox source reservation waits for acknowledged controller shutdown")
		}
	}
	quiescence := &KubernetesNativeQuiescence{APIReader: r.customReader(), Reconciled: true}
	if err := quiescence.CheckQuiesced(ctx, agent, SandboxPrerequisites{
		AgentUID: agent.UID, AgentGeneration: agent.Generation, NamespaceUID: expected.Namespace.UID, Home: expected.Home,
	}); err != nil {
		return err
	}
	if err := r.revokeSandboxPreparationGrants(ctx, agent); err != nil {
		return err
	}
	return r.SandboxNativeCoordination.ReleaseNative(ctx, ticket)
}

func (r *AgentReconciler) stopReconciledAuxiliaries(ctx context.Context, agent *api.Agent) error {
	var deployments appsv1.DeploymentList
	if err := r.customReader().List(ctx, &deployments, client.InNamespace(instanceNamespace(agent.Name))); err != nil {
		return err
	}
	var failures []error
	for index := range deployments.Items {
		object := &deployments.Items[index]
		if !sandboxBuiltinOwned(object, agent) {
			failures = append(failures, fmt.Errorf("Sandbox auxiliary workload ownership is unconfirmed"))
			continue
		}
		if ptr.Deref(object.Spec.Replicas, 0) != 0 {
			before := object.DeepCopy()
			object.Spec.Replicas = ptr.To[int32](0)
			failures = append(failures, r.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
		}
	}
	return errors.Join(failures...)
}

func (r *AgentReconciler) recordedReconciledSandbox(ctx context.Context, agent *api.Agent) (bool, error) {
	if !controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) ||
		controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) {
		return false, nil
	}
	_, receipt, err := r.readSandboxBootstrap(ctx, agent)
	if err != nil {
		return false, err
	}
	return receipt.Reconciled, nil
}

func (r *AgentReconciler) holdReconciledPolicyChange(ctx context.Context, agent *api.Agent) error {
	var holdErr error
	if !controllerutil.ContainsFinalizer(agent, sandbox.HoldFinalizer) {
		before := agent.DeepCopy()
		controllerutil.AddFinalizer(agent, sandbox.HoldFinalizer)
		holdErr = r.Patch(ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	blockErr := r.beginAIGatewayPolicyReprojection(ctx, agent)
	revokeErr := r.revokeSandboxPreparationGrants(ctx, agent)
	auxiliaryErr := r.stopReconciledAuxiliaries(ctx, agent)
	stopErr := r.stopSandboxBuiltin(ctx, agent)
	agent.Status.Phase = api.PhaseProvisioning
	agent.Status.AccessURL = ""
	for _, kind := range []string{api.ConditionReady, api.ConditionIngressReady, api.ConditionSandboxReady} {
		setCondition(&agent.Status.Conditions, kind, metav1.ConditionFalse,
			"PolicyReprojection", "Applying current policy through common reconciliation")
	}
	return errors.Join(holdErr, blockErr, revokeErr, stopErr, auxiliaryErr, r.updateStatus(ctx, agent))
}

func (r *AgentReconciler) reconciledSandboxFailure(ctx context.Context, agent *api.Agent, cause error) (ctrl.Result, error) {
	if errors.Is(cause, errMigrationIntentChanged) || errors.Is(cause, errSandboxIntentChanged) {
		return ctrl.Result{}, cause
	}
	// A later source read can observe a newer Agent after the intent guards pass.
	current, currentErr := r.agentIsCurrent(ctx, agent)
	if currentErr == nil && !current {
		return ctrl.Result{}, cause
	}
	result, holdErr := r.holdPolicyDependencyUnavailable(ctx, agent)
	return result, errors.Join(cause, currentErr, holdErr)
}

func (r *AgentReconciler) holdReconciledSandbox(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	return r.holdReconciledSandboxWithStatus(ctx, agent, reconciledSandboxHoldStatus{
		phase: api.PhasePending, reason: "SandboxHeld",
		message: "Current Sandbox authorization or prerequisites are unavailable",
	}, reconciledSandboxHoldStatus{
		phase: api.PhasePending, reason: "SandboxHeld",
		message: "Current Sandbox authorization or prerequisites are unavailable",
	})
}

type reconciledSandboxHoldStatus struct {
	phase              string
	reason             string
	message            string
	conditions         []string
	observedGeneration bool
	verifiedSuspension bool
}

// holdReconciledSandboxForDeniedSuspension preserves a completed suspension
// while the current policy remains denied. It is only called after the
// revocation request has completed successfully; ordinary missing-key and
// revocation failures continue through the generic pending hold.
func (r *AgentReconciler) holdReconciledSandboxForDeniedSuspension(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	return r.holdReconciledSandboxWithStatus(ctx, agent, reconciledSandboxHoldStatus{
		phase: api.PhaseSuspended, reason: "Suspended",
		message:            "Suspension is confirmed; policy remains denied until a valid configuration is supplied",
		conditions:         []string{api.ConditionReady, api.ConditionPodReady, api.ConditionSandboxReady, api.ConditionIngressReady},
		observedGeneration: true,
		verifiedSuspension: true,
	}, reconciledSandboxHoldStatus{
		phase: api.PhasePending, reason: "SuspensionPending",
		message:            "Suspension requested; waiting for shutdown and access revocation",
		conditions:         []string{api.ConditionReady, api.ConditionPodReady, api.ConditionSandboxReady, api.ConditionIngressReady},
		observedGeneration: true,
		verifiedSuspension: true,
	})
}

func (r *AgentReconciler) holdReconciledSandboxWithStatus(ctx context.Context, agent *api.Agent, complete, incomplete reconciledSandboxHoldStatus) (ctrl.Result, error) {
	var failures []error
	if !controllerutil.ContainsFinalizer(agent, sandbox.HoldFinalizer) {
		before := agent.DeepCopy()
		controllerutil.AddFinalizer(agent, sandbox.HoldFinalizer)
		failures = append(failures, r.Patch(ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
	}
	failures = append(failures, r.revokeSandboxPreparationGrants(ctx, agent))
	failures = append(failures, r.stopSandboxBuiltin(ctx, agent))
	if controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) {
		failures = append(failures, r.stopReconciledAuxiliaries(ctx, agent))
	}
	cleanupErr := errors.Join(failures...)
	status := complete
	if cleanupErr != nil {
		status = incomplete
	}
	agent.Status.Phase, agent.Status.AccessURL = status.phase, ""
	conditions := status.conditions
	if len(conditions) == 0 {
		conditions = []string{api.ConditionReady, api.ConditionSandboxReady, api.ConditionIngressReady}
	}
	for _, kind := range conditions {
		setCondition(&agent.Status.Conditions, kind, metav1.ConditionFalse,
			status.reason, status.message)
	}
	if status.verifiedSuspension {
		if agent.Status.AIGateway != nil && agent.Status.AIGateway.Phase == api.AIGatewayPhaseDeleting {
			setCondition(&agent.Status.Conditions, api.ConditionAIGatewayReady, metav1.ConditionFalse,
				status.reason, status.message)
			conditions = append(conditions, api.ConditionAIGatewayReady)
		}
		if agent.Spec.Agent.Type == api.AgentTypeCustom {
			for _, kind := range []string{api.ConditionDesktopConfigured, api.ConditionTerminalConfigured} {
				setCondition(&agent.Status.Conditions, kind, metav1.ConditionFalse,
					status.reason, status.message)
				conditions = append(conditions, kind)
			}
		}
	}
	if status.observedGeneration {
		agent.Status.ObservedGeneration = agent.Generation
		for index := range agent.Status.Conditions {
			if slices.Contains(conditions, agent.Status.Conditions[index].Type) {
				agent.Status.Conditions[index].ObservedGeneration = agent.Generation
			}
		}
	}
	failures = append(failures, r.updateStatus(ctx, agent))
	return ctrl.Result{RequeueAfter: 15 * time.Second}, errors.Join(failures...)
}

func (r *AgentReconciler) stopReconciledSandboxForSuspension(ctx context.Context, agent *api.Agent) error {
	return r.stopReconciledSandbox(ctx, agent, true)
}

func (r *AgentReconciler) stopReconciledSandboxForMigration(ctx context.Context, agent *api.Agent) error {
	// Migration retains terminal Pods until their acknowledgement is durable.
	// Releasing the reservation here would wait for those same Pods to vanish.
	return r.stopReconciledSandbox(ctx, agent, false)
}

func (r *AgentReconciler) stopReconciledSandbox(ctx context.Context, agent *api.Agent, finishReservation bool) error {
	if !finishReservation && !agent.DeletionTimestamp.IsZero() {
		if err := r.checkMigrationIntent(ctx, agent); err != nil {
			return err
		}
	} else {
		if err := r.checkRegistryIntent(ctx, agent); err != nil {
			return err
		}
	}
	var blockErr error
	if r.AIGateway.configured() {
		if agent.Status.AIGateway != nil && agent.Status.AIGateway.Phase == api.AIGatewayPhaseDeleting {
			// A denied policy revokes the key and records Deleting before the
			// external side effect. Retrying that revocation proves the old key
			// remains unavailable without recreating it. A Deleting phase alone
			// is only intent, so every revoke error still keeps suspension pending.
			blockErr = r.revokeAIGatewayKey(ctx, agent)
		} else {
			gateway, err := r.aiGatewayClient(ctx)
			blockErr = err
			if err == nil {
				blocked, err := r.blockAIGatewayKeyIfPresent(ctx, gateway, agent, aiGatewayReasonSuspended)
				blockErr = err
				if err == nil && !blocked && aiGatewayKeyWasIssued(agent) {
					blockErr = fmt.Errorf("the previously issued AI Gateway key is missing during suspension")
				}
				if blockErr == nil && agent.Status.AIGateway != nil {
					agent.Status.AIGateway.Blocked = blocked
				}
			}
		}
	} else if aiGatewayKeyWasIssued(agent) {
		blockErr = fmt.Errorf("AI Gateway configuration is required to block the existing key during suspension")
	}
	// Independent shutdown must precede chart/bootstrap reads and must still
	// run when Gateway revocation or one component's stop cannot complete.
	revokeErr := r.revokeSandboxPreparationGrants(ctx, agent)
	auxiliaryErr := r.stopReconciledAuxiliaries(ctx, agent)
	stopErr := r.stopSandboxBuiltinWorkload(ctx, agent)
	if stopErr == nil && finishReservation {
		stopErr = r.stopNativeSandboxReservation(ctx, agent)
	}
	agent.Status.AccessURL = ""
	if err := errors.Join(blockErr, revokeErr, auxiliaryErr, stopErr); err != nil {
		agent.Status.Phase = api.PhasePending
		for _, kind := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionIngressReady} {
			setCondition(&agent.Status.Conditions, kind, metav1.ConditionFalse,
				"SuspensionPending", "Suspension requested; waiting for shutdown and access revocation")
		}
		return errors.Join(err, r.updateStatus(ctx, agent))
	}
	return nil
}

func (r *AgentReconciler) releaseReconciledHold(ctx context.Context, agent *api.Agent) error {
	if agent.Spec.Suspended || !slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) {
		return nil
	}
	before := agent.DeepCopy()
	controllerutil.RemoveFinalizer(agent, sandbox.HoldFinalizer)
	return r.Patch(ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
