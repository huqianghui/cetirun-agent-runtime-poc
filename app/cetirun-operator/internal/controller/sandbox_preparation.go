// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplacement"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// SandboxPrerequisites authorizes one native Agent generation and its existing
// platform-owned storage and native authentication Secret. It contains identities, never Secret
// contents. Revision also covers the approved chart, helpers and native inputs.
type SandboxPrerequisites = sandboxworkload.PreparationPrerequisites

// SandboxPreparationSource is a trusted platform integration, not CR status or
// tenant Helm values. Resolve must authorize the whole Agent generation without
// stopping or requiring quiescence of its writer; execution rechecks it too.
// The default path requires CheckQuiesced to confirm physical writer exclusion;
// API-level Pod absence alone is insufficient. The explicit native profile uses
// ordinary Kubernetes quiescence instead and cannot qualify failover or reuse.
// Neither method grants a start.
type SandboxPreparationSource interface {
	Resolve(context.Context, *api.Agent, *api.Team) (SandboxPrerequisites, error)
	CheckQuiesced(context.Context, *api.Agent, SandboxPrerequisites) error
}

type SandboxHelmInstaller interface {
	PrepareSandbox(string, map[string]any, map[string]any, string, *sandboxworkload.Bundle) (*helm.PreparedRelease, error)
	StageSandbox(context.Context, string, string, *helm.PreparedRelease, func(context.Context) error) error
	SandboxStageDigest(context.Context, string, string) (string, error)
}

// SandboxPreparation wires protected provisioning. NativeExecution is a trusted
// deployment opt-in, not tenant input or an independent enforcement receipt.
type SandboxPreparation struct {
	NativeExecution         bool
	PlannedMigration        bool
	Configuration           SandboxConfigurationSource
	Prerequisites           SandboxPreparationSource
	Installer               SandboxHelmInstaller
	AdmissionServiceAccount string
	Now                     func() time.Time
	MaxSnapshotAge          time.Duration
	Placement               *sandboxplacement.Handler
	bootstrap               *sandboxBootstrapConfig
}

type sandboxPreparationAttempt struct {
	reconciled     bool
	agent          *api.Agent
	team           *api.Team
	resolution     *sandbox.Resolution
	configuration  SandboxConfigurationSnapshot
	prerequisites  SandboxPrerequisites
	input          sandboxworkload.Input
	plan           *sandboxpolicy.Plan
	sources        []sandboxworkload.SourceReference
	policyDigest   string
	platformValues map[string]any
	userValues     map[string]any
}

func sandboxPreparationSupported(agent *api.Agent) bool {
	return (agent.Spec.Agent.Type == api.AgentTypeOpenClaw || agent.Spec.Agent.Type == api.AgentTypeHermes) &&
		agent.Spec.Tailscale == nil && !agentIdentityRequested(agent)
}

func (r *AgentReconciler) reconcileSandboxPreparation(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	prepareContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := r.prepareSandboxHeld(prepareContext, agent)
	cancel()
	if err != nil {
		// Revocation does not depend on the failed configuration source. Keep
		// every journal entry and both finalizers until cleanup is observed.
		cleanupContext, cancelCleanup := context.WithTimeout(ctx, 10*time.Second)
		defer cancelCleanup()
		cleanupErr := r.revokeSandboxPreparationGrants(cleanupContext, agent)
		statusErr := r.sandboxPreparationStatus(cleanupContext, agent, false, err)
		if sandboxPreparationUnsupportedError(err) && cleanupErr == nil && statusErr == nil {
			log.FromContext(ctx).Info("Unsupported Sandbox preparation remains held", "agent", agent.Name)
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		err = errors.Join(err, cleanupErr, statusErr)
		log.FromContext(ctx).Error(err, "Sandbox preparation remains held", "agent", agent.Name)
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func sandboxPreparationUnsupportedError(err error) bool {
	// A joined outage must not disappear into a clean unsupported hold.
	switch cause := err.(type) {
	case interface{ Unwrap() []error }:
		causes := cause.Unwrap()
		for _, nested := range causes {
			if !sandboxPreparationUnsupportedError(nested) {
				return false
			}
		}
		return len(causes) != 0
	case interface{ Unwrap() error }:
		return sandboxPreparationUnsupportedError(cause.Unwrap())
	default:
		return errors.Is(err, sandboxpolicy.ErrUnsupportedControl)
	}
}

func (r *AgentReconciler) prepareSandboxHeld(ctx context.Context, requested *api.Agent) error {
	config := r.SandboxPreparation
	if config == nil || config.Configuration == nil || config.Prerequisites == nil || config.Installer == nil ||
		r.APIReader == nil || r.OperatorNamespace == "" || config.AdmissionServiceAccount == "" {
		return fmt.Errorf("%w: Sandbox preparation requires explicit trusted sources, installer, uncached reader and state identities", sandboxpolicy.ErrUnavailable)
	}
	if requested.Spec.Agent.Type != api.AgentTypeOpenClaw && requested.Spec.Agent.Type != api.AgentTypeHermes {
		return fmt.Errorf("%w: only native OpenClaw and Hermes preparation is supported", sandboxpolicy.ErrUnsupportedControl)
	}
	if requested.Spec.Tailscale != nil || agentIdentityRequested(requested) {
		return fmt.Errorf("%w: native cloud and sidecar integrations require separate Sandbox lifecycle support", sandboxpolicy.ErrUnsupportedControl)
	}
	if config.bootstrap == nil && controllerutil.ContainsFinalizer(requested, sandbox.BootstrapFinalizer) {
		return fmt.Errorf("%w: recorded Sandbox bootstrap requires its configured ownership checks; execution remains held", sandboxpolicy.ErrUnavailable)
	}
	attempt, err := r.resolveSandboxPreparation(ctx, requested)
	if err != nil {
		return err
	}
	if config.NativeExecution {
		if !r.AIGateway.Enabled {
			return fmt.Errorf("%w: native Sandbox execution requires the AI Gateway", sandboxpolicy.ErrUnsupportedControl)
		}
		if config.Placement != nil {
			return fmt.Errorf("%w: native Sandbox execution does not support a placement registry", sandboxpolicy.ErrUnsupportedControl)
		}
		if err := validateNativeSandboxProfile(attempt); err != nil {
			return err
		}
		if err := r.checkNativeSandboxUnused(ctx, requested); err != nil {
			return err
		}
	}
	if config.bootstrap != nil {
		if err := r.bootstrapSandboxOwned(ctx, attempt); err != nil {
			return fmt.Errorf("bootstrap Sandbox prerequisites: %w", err)
		}
		attempt, err = r.resolveSandboxPreparation(ctx, requested)
		if err != nil {
			return err
		}
	}
	// Bootstrap first: the Gateway must validate the recorded namespace UID
	// before projecting its key. Only the opt-in native path renders that key.
	if requested.Status.Policy != nil && requested.Status.Policy.EffectivePolicyRef != nil &&
		(!config.NativeExecution || !sandboxGatewayProjectionReady(attempt.agent)) {
		return fmt.Errorf("%w: typed AI Gateway Sandbox projection is unavailable; execution remains held", sandboxpolicy.ErrUnsupportedControl)
	}
	prerequisites, err := config.Prerequisites.Resolve(ctx, attempt.agent.DeepCopy(), attempt.team.DeepCopy())
	if err != nil {
		return fmt.Errorf("resolve Sandbox workload prerequisites: %w", err)
	}
	if err := r.inheritSandboxPlacement(ctx, attempt.agent, &prerequisites); err != nil {
		return err
	}
	attempt.prerequisites = prerequisites
	if err := r.sandboxPreparationInput(ctx, attempt); err != nil {
		return err
	}
	platform := attempt.configuration.Platforms[attempt.resolution.Sandbox.Type]
	selected := attempt.selectedPolicy()
	if err := sandboxpolicy.VerifyPolicyGrants(ctx, r.APIReader, selected, platform.Grants); err != nil {
		return err
	}
	attempt.platformValues, attempt.userValues, err = r.sandboxHelmValues(attempt.agent, attempt.team)
	if err != nil {
		return err
	}
	if reused, err := r.reuseSandboxPreparation(ctx, attempt); err != nil || reused {
		return err
	}
	// Do not persist cleanup obligations until their fence is available.
	if err := r.checkSandboxPreparationQuiesced(ctx, attempt); err != nil {
		return err
	}
	attempt.plan, err = sandboxpolicy.Compile(sandboxpolicy.CompileInput{
		NativeExecution: config.NativeExecution,
		Resolution:      *attempt.resolution, AgentName: attempt.agent.Name,
		Namespace: attempt.input.Namespace.Name, Platform: platform,
	})
	if err != nil {
		return err
	}
	attempt.input.Controls, err = sandboxworkload.ControlsFromPlan(*attempt.plan)
	if err != nil {
		return err
	}
	bundle, err := sandboxworkload.Build(attempt.input)
	if err != nil {
		return err
	}
	prepared, err := config.Installer.PrepareSandbox(attempt.input.Namespace.Name, attempt.platformValues,
		attempt.userValues, prerequisites.ChartVersion, bundle)
	if err != nil {
		return err
	}
	if prepared == nil || prepared.ChartVersion != prerequisites.ChartVersion {
		return fmt.Errorf("Sandbox renderer returned a different chart version")
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		return err
	}
	role, binding, access, err := r.sandboxSourceAccess(attempt, template.Spec)
	if err != nil {
		return err
	}
	desired := append(slices.Clone(attempt.plan.Identity.Bindings), access)
	if err := r.recordSandboxPreparationReceipt(ctx, attempt); err != nil {
		return err
	}
	journal := r.sandboxGrantJournal()
	previous, err := journal.Read(ctx, attempt.agent.Name, attempt.agent.UID)
	if apierrors.IsNotFound(err) && !controllerutil.ContainsFinalizer(attempt.agent, sandbox.PreparationFinalizer) {
		if err := journal.Record(ctx, attempt.agent.Name, attempt.agent.UID, nil); err != nil {
			return err
		}
		previous = nil
	} else if err != nil {
		return fmt.Errorf("read Sandbox grant journal (recovery is required if missing): %w", err)
	}
	if !controllerutil.ContainsFinalizer(attempt.agent, sandbox.PreparationFinalizer) {
		base := attempt.agent.DeepCopy()
		controllerutil.AddFinalizer(attempt.agent, sandbox.PreparationFinalizer)
		if err := r.Patch(ctx, attempt.agent, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("persist Sandbox preparation finalizer: %w", err)
		}
	}
	if err := journal.Revoke(ctx, attempt.agent.Name, attempt.agent.UID,
		sandboxpolicy.ObsoleteBindings(previous, desired)); err != nil {
		return err
	}
	if err := journal.Record(ctx, attempt.agent.Name, attempt.agent.UID, desired); err != nil {
		return err
	}
	check := r.sandboxPreparationGuard(attempt, desired)
	if err := check(ctx); err != nil {
		return err
	}
	if err := r.applySandboxSourceAccess(ctx, attempt.agent, role, binding, access); err != nil {
		return err
	}
	if err := config.Installer.StageSandbox(ctx, attempt.input.Namespace.Name, attempt.input.ReleaseName, prepared, check); err != nil {
		return fmt.Errorf("stage held Sandbox release: %w", err)
	}
	if err := check(ctx); err != nil {
		return err
	}
	confirmed, err := r.sandboxBindingUIDs(ctx, desired)
	if err != nil {
		return err
	}
	if err := journal.Record(ctx, attempt.agent.Name, attempt.agent.UID, confirmed); err != nil {
		return err
	}
	stageDigest, err := config.Installer.SandboxStageDigest(ctx, attempt.input.Namespace.Name, attempt.input.ReleaseName)
	if err != nil {
		return err
	}
	if err := r.publishSandboxExpectation(ctx, attempt, bundle, template, role, binding, confirmed, stageDigest, check); err != nil {
		return err
	}
	return r.sandboxPreparationStatus(ctx, attempt.agent, true, nil)
}

func (r *AgentReconciler) resolveSandboxPreparation(ctx context.Context, requested *api.Agent) (*sandboxPreparationAttempt, error) {
	return r.resolveSandboxExecutionInputs(ctx, requested, true)
}

func (r *AgentReconciler) resolveSandboxExecutionInputs(ctx context.Context, requested *api.Agent, held bool) (*sandboxPreparationAttempt, error) {
	return r.resolveSandboxInputs(ctx, requested, held, false)
}

func (r *AgentReconciler) resolveSandboxInputs(ctx context.Context, requested *api.Agent, held, reconciled bool) (*sandboxPreparationAttempt, error) {
	config := r.SandboxPreparation
	agent, authorized, err := sandboxbinding.Read(ctx, r.APIReader, client.ObjectKeyFromObject(requested))
	if err != nil {
		return nil, err
	}
	if agent.UID != requested.UID || agent.Generation != requested.Generation ||
		(!reconciled && (agent.Spec.Suspended || controllerutil.ContainsFinalizer(agent, sandbox.HoldFinalizer) != held)) ||
		!agent.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("Sandbox Agent identity, generation or hold changed")
	}
	snapshot, err := configurationSnapshot(ctx, config.Configuration, config.Now, config.MaxSnapshotAge)
	if err != nil {
		return nil, err
	}
	team := &api.Team{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
		return nil, err
	}
	if len(team.Spec.SandboxRefs) > 128 {
		return nil, fmt.Errorf("Sandbox preparation dependency inventory exceeds its bound")
	}
	resources := sandbox.Resources{Sandboxes: map[string]*api.Sandbox{}, AgentPolicies: map[string]*api.AgentPolicy{}}
	sources := []sandboxworkload.SourceReference{sandboxSource(team, api.GroupVersion.String(), "Team")}
	for _, name := range uniqueReferences(append(slices.Clone(team.Spec.SandboxRefs), team.Spec.DefaultSandboxRef, agent.Spec.SandboxRef)) {
		object := &api.Sandbox{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, object); err != nil {
			return nil, err
		}
		resources.Sandboxes[name] = object
		sources = append(sources, sandboxSource(object, api.GroupVersion.String(), "Sandbox"))
	}
	var policyDigest string
	for _, name := range uniqueReferences([]string{team.Spec.AgentPolicyRef}) {
		object := &api.AgentPolicy{}
		if reconciled && config.NativeExecution {
			object, policyDigest, err = r.readSandboxExecutionPolicy(ctx, name)
		} else {
			err = r.APIReader.Get(ctx, client.ObjectKey{Name: name}, object)
		}
		if err != nil {
			return nil, err
		}
		resources.AgentPolicies[name] = object
		sources = append(sources, sandboxSource(object, api.GroupVersion.String(), "AgentPolicy"))
	}
	resolution, err := sandbox.Resolve(sandbox.ResolveInput{
		NativeExecution: config.NativeExecution,
		Agent:           agent, Team: team, Resources: resources, Registrations: snapshot.Registrations, AuthorizedBinding: authorized,
	})
	if err != nil {
		return nil, err
	}
	condition, _, err := assessSandboxPrerequisites(ctx, r.APIReader, resources.Sandboxes[agent.Spec.SandboxRef],
		SandboxConfigurationSourceFunc(func(context.Context) (SandboxConfigurationSnapshot, error) { return snapshot, nil }),
		config.MaxSnapshotAge, config.Now, config.NativeExecution)
	if err != nil {
		return nil, err
	}
	if condition.Status != metav1.ConditionTrue {
		cause := sandboxpolicy.ErrUnavailable
		if condition.Status == metav1.ConditionFalse && condition.Reason == "UnsupportedControl" {
			cause = sandboxpolicy.ErrUnsupportedControl
		}
		return nil, fmt.Errorf("%w: %s: %s", cause, condition.Reason, condition.Message)
	}
	return &sandboxPreparationAttempt{
		reconciled: reconciled, agent: agent, team: team, resolution: resolution,
		configuration: snapshot, sources: sources, policyDigest: policyDigest,
	}, nil
}

func (a *sandboxPreparationAttempt) selectedPolicy() *api.AgentPolicy {
	name := a.team.Spec.AgentPolicyRef
	return &api.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: a.resolution.Binding.AgentPolicyUID},
		Spec:       a.resolution.AgentPolicy,
	}
}

func (r *AgentReconciler) sandboxPreparationGuard(attempt *sandboxPreparationAttempt, desired []sandboxpolicy.BindingReference) func(context.Context) error {
	config := r.SandboxPreparation
	snapshot := attempt.configuration
	snapshot.ObservedAt = time.Time{}
	prerequisites := attempt.prerequisites
	prerequisites.ObservedAt = time.Time{}
	snapshotKey, snapshotErr := sandboxPreparationDigest(snapshot)
	prerequisitesKey, prerequisitesErr := sandboxPreparationDigest(prerequisites)
	agentSource, agentErr := sandboxworkload.AgentSourceReference(attempt.agent)
	return func(ctx context.Context) error {
		if err := errors.Join(snapshotErr, prerequisitesErr, agentErr); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		now := configurationNow(config.Now)
		if err := configurationSnapshotTime(attempt.configuration.ObservedAt, now, config.MaxSnapshotAge); err != nil {
			return err
		}
		if err := configurationSnapshotTime(attempt.prerequisites.ObservedAt, now, config.MaxSnapshotAge); err != nil {
			return err
		}
		agent, binding, err := sandboxbinding.Read(ctx, r.APIReader, client.ObjectKey{Name: attempt.agent.Name})
		if err != nil {
			return err
		}
		if agent.UID != attempt.agent.UID || agent.Generation != attempt.agent.Generation ||
			!equality.Semantic.DeepEqual(agent.Spec, attempt.agent.Spec) || binding != attempt.resolution.Binding ||
			agent.Spec.Suspended || !controllerutil.ContainsFinalizer(agent, sandbox.HoldFinalizer) ||
			!controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) {
			return fmt.Errorf("Sandbox preparation intent changed")
		}
		currentAgentSource, err := sandboxworkload.AgentSourceReference(agent)
		if err != nil || currentAgentSource != agentSource {
			return fmt.Errorf("Sandbox preparation authorization or lifecycle intent changed")
		}
		current, err := configurationSnapshot(ctx, config.Configuration, config.Now, config.MaxSnapshotAge)
		if err != nil {
			return err
		}
		current.ObservedAt = time.Time{}
		currentKey, err := sandboxPreparationDigest(current)
		if err != nil {
			return err
		}
		if currentKey != snapshotKey {
			return fmt.Errorf("Sandbox platform support or approval inventory changed")
		}
		inputs, err := config.Prerequisites.Resolve(ctx, agent.DeepCopy(), attempt.team.DeepCopy())
		if err != nil {
			return err
		}
		if err := r.inheritSandboxPlacement(ctx, agent, &inputs); err != nil {
			return err
		}
		if err := configurationSnapshotTime(inputs.ObservedAt, configurationNow(config.Now), config.MaxSnapshotAge); err != nil {
			return err
		}
		inputs.ObservedAt = time.Time{}
		inputKey, err := sandboxPreparationDigest(inputs)
		if err != nil {
			return err
		}
		if inputKey != prerequisitesKey {
			return fmt.Errorf("Sandbox workload prerequisite approval changed")
		}
		if err := r.verifySandboxSources(ctx, attempt.sources); err != nil {
			return err
		}
		if err := sandboxpolicy.VerifyPolicyGrants(ctx, r.APIReader, attempt.selectedPolicy(),
			attempt.configuration.Platforms[attempt.resolution.Sandbox.Type].Grants); err != nil {
			return err
		}
		if err := r.sandboxGrantJournal().RefreshBindingIntents(ctx, agent.Name, agent.UID, desired); err != nil {
			return err
		}
		return r.checkSandboxPreparationQuiesced(ctx, attempt)
	}
}

func (r *AgentReconciler) checkSandboxPreparationQuiesced(ctx context.Context, attempt *sandboxPreparationAttempt) error {
	config := r.SandboxPreparation
	if err := r.verifySandboxPlacement(ctx, attempt); err != nil {
		return err
	}
	if err := config.Prerequisites.CheckQuiesced(ctx, attempt.agent.DeepCopy(), copySandboxPrerequisites(attempt.prerequisites)); err != nil {
		return fmt.Errorf("Sandbox old-writer fence is unconfirmed: %w", err)
	}
	var pods corev1.PodList
	if err := r.APIReader.List(ctx, &pods, client.InNamespace(attempt.input.Namespace.Name), client.Limit(1)); err != nil {
		return err
	}
	if len(pods.Items) != 0 {
		return fmt.Errorf("Sandbox preparation waits for all old Pods, including terminating Pods, to disappear")
	}
	now := configurationNow(config.Now)
	if err := configurationSnapshotTime(attempt.configuration.ObservedAt, now, config.MaxSnapshotAge); err != nil {
		return err
	}
	if err := configurationSnapshotTime(attempt.prerequisites.ObservedAt, now, config.MaxSnapshotAge); err != nil {
		return err
	}
	return ctx.Err()
}

func sandboxPreparationDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode Sandbox preparation identity: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// customSandboxBaseValues resolves configuration before a Gateway key exists.
// Policy compilation needs terminal settings before it can authorize that key;
// workload rendering adds the verified Gateway projection in sandboxHelmValues.
func (r *AgentReconciler) customSandboxBaseValues(agent *api.Agent, team *api.Team) (map[string]any, map[string]any, error) {
	if !r.customGatewaySelected(agent) {
		return nil, nil, fmt.Errorf("Custom Sandbox execution requires an explicitly Gateway-qualified image")
	}
	if agentIdentityRequested(agent) && !r.nativeConnectionsRequested(agent) {
		return nil, nil, fmt.Errorf("Custom Agent Identity requires the governed Connections executor")
	}
	if _, err := r.customRuntimeValues(agent, team); err != nil {
		return nil, nil, err
	}
	base := agent.DeepCopy()
	base.Spec.Helm.Values = nil
	platform, err := r.customRuntimeValues(base, team)
	if err != nil {
		return nil, nil, err
	}
	platform["secrets"] = deepMerge(platform["secrets"], map[string]any{"existingSecret": sandboxRuntimeSecretName(agent)})
	platform["agentType"] = "custom"
	platform["agentRequest"] = runtimeRequest(agent, "")
	platform["agentRequest"].(map[string]any)["releaseName"] = helmReleaseName(agent.Name)
	platform["desktop"] = map[string]any{"enabled": false}
	if agent.Spec.RestartRequestID != "" {
		platform["podAnnotations"] = deepMerge(platform["podAnnotations"], map[string]any{
			api.AnnotationRestartRequestID: agent.Spec.RestartRequestID,
		})
	}
	user := map[string]any{}
	for name, raw := range agent.Spec.Helm.Values {
		var value any
		if err := json.Unmarshal(raw.Raw, &value); err != nil {
			return nil, nil, fmt.Errorf("invalid Custom Sandbox Helm override %q", name)
		}
		user[name] = value
	}
	if desktop, ok := user["desktop"].(map[string]any); ok && desktop["enabled"] == true {
		return nil, nil, fmt.Errorf("common Custom desktop requires a qualified helper; use its Web UI, private Chat or terminal")
	}
	return platform, user, nil
}

func (r *AgentReconciler) sandboxHelmValues(agent *api.Agent, team *api.Team) (map[string]any, map[string]any, error) {
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		platform, user, err := r.customSandboxBaseValues(agent, team)
		if err != nil {
			return nil, nil, err
		}
		if err := r.applyAIGatewayValues(platform, agent); err != nil {
			return nil, nil, err
		}
		return platform, user, nil
	}
	copy := agent.DeepCopy()
	copy.Spec.Helm.Values = nil
	platform, err := r.builtinHelmValues(copy, team, AgentIdentityHelmConfig{})
	if err != nil {
		return nil, nil, err
	}
	if r.SandboxPreparation != nil && r.SandboxPreparation.NativeExecution {
		if err := r.applyAIGatewayValues(platform, agent); err != nil {
			return nil, nil, fmt.Errorf("apply typed AI Gateway Sandbox projection: %w", err)
		}
	}
	user := map[string]any{}
	for name, raw := range agent.Spec.Helm.Values {
		var value any
		if err := json.Unmarshal(raw.Raw, &value); err != nil {
			return nil, nil, fmt.Errorf("invalid Sandbox Helm override %q", name)
		}
		user[name] = value
	}
	return platform, user, nil
}

func (r *AgentReconciler) sandboxPreparationStatus(ctx context.Context, expected *api.Agent, prepared bool, cause error) error {
	agent := &api.Agent{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(expected), agent); err != nil {
		return err
	}
	if agent.UID != expected.UID || agent.Generation != expected.Generation || !agent.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Sandbox Agent changed before preparation status")
	}
	base := agent.DeepCopy()
	agent.Status.Phase = api.PhasePending
	agent.Status.AccessURL = ""
	reason, message := "SandboxPreparationPending", "Sandbox prerequisites or held provisioning are incomplete; execution remains held."
	configStatus := metav1.ConditionFalse
	sandboxStatus := metav1.ConditionFalse
	if cause != nil {
		sandboxStatus, reason, message = sandboxStatusFailure(cause)
		if sandboxPreparationUnsupportedError(cause) {
			agent.Status.Phase = api.PhaseFailed
			message = "A requested control is not supported by native Sandbox preparation; execution remains held."
		}
	} else if prepared {
		reason, message = "SandboxPrepared", "Native resources and controls are prepared at zero replicas; qualified start and enforcement observation are still required."
		if r.SandboxPreparation != nil && r.SandboxPreparation.NativeExecution {
			message = "Native resources are prepared at zero replicas for the supported single-start profile."
		}
		configStatus = metav1.ConditionTrue
	}
	for _, kind := range []string{api.ConditionReady, api.ConditionSandboxReady, api.ConditionAgentConfigReady} {
		status := metav1.ConditionFalse
		if kind == api.ConditionAgentConfigReady {
			status = configStatus
		} else if kind == api.ConditionSandboxReady {
			status = sandboxStatus
		}
		apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
			Type: kind, Status: status, ObservedGeneration: agent.Generation, Reason: reason, Message: message,
		})
	}
	if equality.Semantic.DeepEqual(base.Status, agent.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, agent, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	emitSandboxAgentTransition(ctx, base, agent)
	return nil
}
