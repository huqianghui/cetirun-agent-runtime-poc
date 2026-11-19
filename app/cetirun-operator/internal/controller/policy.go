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
	"math/big"
	"reflect"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// holdPolicyDenied revokes a virtual key before using the existing
// Sandbox hold path to remove ingress and scale known workloads down. A revoke
// error never skips the compute stop: the next reconcile keeps the fail-closed
// hold and retries revocation.
func (r *AgentReconciler) holdPolicyDenied(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	var revokeErr error
	if r.AIGateway.configured() {
		revokeErr = r.revokeAIGatewayKey(ctx, agent)
	}
	// An absent Gateway cannot confirm revocation of a previously issued key.
	// In that case retain the normal pending hold instead of claiming suspension
	// completed.
	revocationConfirmed := revokeErr == nil && (r.AIGateway.configured() || !aiGatewayKeyWasIssued(agent))
	if revocationConfirmed && agent.Spec.Suspended {
		reconciled, reconcileErr := r.usesReconciledSandbox(ctx, agent)
		if reconciled && reconcileErr == nil {
			result, holdErr := r.holdReconciledSandboxForDeniedSuspension(ctx, agent)
			return result, holdErr
		}
	}
	result, holdErr := r.holdSandbox(ctx, agent)
	if revokeErr != nil || holdErr != nil {
		return result, errors.Join(revokeErr, holdErr)
	}
	return result, nil
}

// holdPolicyReprojection fences an already-running Agent while a changed
// expected projection is being applied. Unlike a denied binding it preserves
// the virtual-key identity and spend history, but blocks the old grant before
// removing workload ingress and compute. The Gateway records that this is an
// operator-owned temporary block; an administrator emergency block is never
// adopted or cleared by this path.
func (r *AgentReconciler) holdPolicyReprojection(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	var blockErr error
	if r.AIGateway.configured() {
		blockErr = r.beginAIGatewayPolicyReprojection(ctx, agent)
	}
	result, holdErr := r.fenceSandboxForPolicyReprojection(ctx, agent)
	if blockErr != nil || holdErr != nil {
		return result, errors.Join(blockErr, holdErr)
	}
	// The hold has removed ingress and stopped known compute. Reconcile the
	// replacement key while that fence is in place; this prevents a later
	// Sandbox preparation pass from deadlocking before the Gateway can observe
	// and clear its own PolicyReprojection block.
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	if agentIdentityRequested(agent) || !credentialPolicyVerified(agent, "") {
		if err := r.reconcileAgentIdentity(ctx, agent); err != nil {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, errors.Join(err, r.updateStatus(ctx, agent))
		}
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}
	if _, err := r.reconcilePolicy(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

// holdPolicyDependencyUnavailable preserves the virtual-key identity and
// spend history while a control-plane dependency cannot be read. It blocks an
// existing key and stops compute, but deliberately does not try to calculate
// or apply a replacement grant without current policy evidence.
func (r *AgentReconciler) holdPolicyDependencyUnavailable(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	var blockErr error
	if r.AIGateway.configured() {
		blockErr = r.beginAIGatewayPolicyReprojection(ctx, agent)
	}
	result, holdErr := r.holdSandbox(ctx, agent)
	return result, errors.Join(blockErr, holdErr)
}

// reconcilePolicy resolves opted-in Agents and preserves the legacy path for
// Agents and Teams with no policy intent or recorded governed lifetime. Workload,
// Gateway, and credential controllers remain the authority for their own
// applied evidence. Invalid selection is fail-closed before any workload
// recovery can happen.
func (r *AgentReconciler) reconcilePolicy(ctx context.Context, agent *api.Agent) (policyReconcileResult, error) {
	required, err := r.agentPolicyRequired(ctx, agent)
	if err != nil {
		return policyReconcileResult{dependencyUnavailable: true}, err
	}
	if !required {
		return policyReconcileResult{authorized: true, projectionsVerified: true}, nil
	}
	resolution, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		result := policyReconcileResult{
			dependencyUnavailable: isPolicyDependencyError(err),
			ingressMTLSPending:    errors.Is(err, ingressmtls.ErrApprovalPending),
			identityPending:       errors.Is(err, errSandboxIdentityPending),
		}
		return result, r.recordPolicy(ctx, agent, nil, err)
	}
	if err := r.reconcileCredentialPolicy(ctx, agent, resolution); err != nil {
		return policyReconcileResult{authorized: true, dependencyUnavailable: true},
			errors.Join(err, r.recordPolicy(ctx, agent, resolution, nil))
	}
	result := policyReconcileResult{
		authorized:          true,
		projectionsVerified: policyEnforcementVerified(agent, resolution),
		reprojectionHold:    policyReprojectionMustHold(agent, resolution),
	}
	err = r.recordPolicy(ctx, agent, resolution, nil)
	if err != nil {
		// A failed observation write does not turn valid authorization into a
		// denial that destroys the existing virtual key and spending identity.
		result.dependencyUnavailable = true
	}
	return result, err
}

func (r *AgentReconciler) agentPolicyRequired(ctx context.Context, agent *api.Agent) (bool, error) {
	if sandboxRequested(agent, nil) {
		return true, nil
	}
	if agent.Spec.Team == "" {
		return false, nil
	}
	reader := configurationReader(r.APIReader, r.Client)
	team := &api.Team{}
	if err := reader.Get(ctx, types.NamespacedName{Name: agent.Spec.Team}, team); err != nil {
		if apierrors.IsNotFound(err) {
			// The legacy reconcile path reports a missing Team without
			// inventing a policy binding or granting a runtime fallback.
			return false, nil
		}
		return false, policyDependency("read Team policy selection: %w", err)
	}
	return sandboxRequested(agent, team), nil
}

type policyResolution struct {
	team               *api.Team
	policy             *api.AgentPolicy
	gatewayRevision    string
	credentialRevision string
	sandboxRevision    string
}

// policyReconcileResult distinguishes a declaratively authorized Agent
// from one whose current projections have been independently verified. This
// lets an initial binding create its projections while preventing an already
// running Agent from retaining an old key or workload during a revision
// transition.
type policyReconcileResult struct {
	authorized            bool
	projectionsVerified   bool
	reprojectionHold      bool
	dependencyUnavailable bool
	ingressMTLSPending    bool
	identityPending       bool
}

// policyDependencyError distinguishes an unavailable policy/runtime dependency
// from a definitive authorization denial. The former must fence access while
// preserving the key so recovery can continue with the same spend record.
type policyDependencyError struct{ err error }

func (e *policyDependencyError) Error() string { return e.err.Error() }
func (e *policyDependencyError) Unwrap() error { return e.err }

func policyDependency(format string, args ...any) error {
	return &policyDependencyError{err: fmt.Errorf(format, args...)}
}

func isPolicyDependencyError(err error) bool {
	var dependency *policyDependencyError
	return errors.As(err, &dependency)
}

func (r *AgentReconciler) verifyTeamAcceptance(ctx context.Context, team *api.Team) error {
	accepted := meta.FindStatusCondition(team.Status.Conditions, api.ConditionTeamAccepted)
	if team.Status.ObservedGeneration != team.Generation {
		config := r.SandboxPreparation
		if config == nil || !config.NativeExecution || config.Configuration == nil ||
			accepted == nil || accepted.Status != metav1.ConditionTrue ||
			accepted.ObservedGeneration != team.Status.ObservedGeneration ||
			team.Status.ObservedGeneration <= 0 || team.Status.ObservedGeneration >= team.Generation ||
			!team.DeletionTimestamp.IsZero() {
			return policyDependency("Team observation is pending for generation %d", team.Generation)
		}
		// Router edits can precede their status observation. Recheck the same
		// bindings as the Team controller; execution digests still fence changes.
		assessor := &TeamReconciler{APIReader: configurationReader(r.APIReader, r.Client)}
		if err := assessor.validate(ctx, team); err != nil {
			return policyDependency("reassess pending Team: %w", err)
		}
		return nil
	}
	if accepted == nil {
		return policyDependency("Team acceptance observation is unavailable")
	}
	if accepted.ObservedGeneration != team.Generation || accepted.Status == metav1.ConditionUnknown {
		return policyDependency("Team acceptance observation is unavailable for generation %d", team.Generation)
	}
	if accepted.Status == metav1.ConditionFalse {
		return fmt.Errorf("Team policy assignment is not accepted")
	}
	if accepted.Status != metav1.ConditionTrue {
		return policyDependency("Team acceptance observation has an unrecognized status")
	}
	return nil
}

func (r *AgentReconciler) verifyPolicyAcceptance(ctx context.Context, policy *api.AgentPolicy) error {
	accepted := meta.FindStatusCondition(policy.Status.Conditions, api.ConditionAgentPolicyAccepted)
	if policy.Status.ObservedGeneration != policy.Generation {
		config := r.SandboxPreparation
		if config == nil || !config.NativeExecution || config.Configuration == nil ||
			accepted == nil || accepted.Status != metav1.ConditionTrue ||
			accepted.ObservedGeneration != policy.Status.ObservedGeneration ||
			policy.Status.ObservedGeneration <= 0 || policy.Status.ObservedGeneration >= policy.Generation {
			return policyDependency("AgentPolicy observation is pending for generation %d", policy.Generation)
		}
		// A generation bump alone is not an execution change. Assess the current
		// policy with the same trusted sources as its controller; the per-Agent
		// projections and execution digests still fence changed controls.
		assessor := &AgentPolicyReconciler{
			NativeExecution: true, Source: config.Configuration, Now: config.Now, MaxSnapshotAge: config.MaxSnapshotAge,
		}
		condition, observedAt, err := assessor.assess(ctx, configurationReader(r.APIReader, r.Client), policy)
		if err != nil {
			return policyDependency("reassess pending AgentPolicy: %w", err)
		}
		if condition.Status != metav1.ConditionTrue {
			return policyDependency("AgentPolicy observation is pending for generation %d: %s", policy.Generation, condition.Reason)
		}
		if err := configurationSnapshotTime(observedAt, configurationNow(config.Now), config.MaxSnapshotAge); err != nil {
			return policyDependency("reassess pending AgentPolicy freshness: %w", err)
		}
		return nil
	}
	if accepted == nil {
		return policyDependency("AgentPolicy acceptance observation is unavailable")
	}
	if accepted.ObservedGeneration != policy.Generation || accepted.Status == metav1.ConditionUnknown {
		return policyDependency("AgentPolicy acceptance observation is unavailable for generation %d", policy.Generation)
	}
	if accepted.Status == metav1.ConditionFalse {
		return fmt.Errorf("effective AgentPolicy is not accepted")
	}
	if accepted.Status != metav1.ConditionTrue {
		return policyDependency("AgentPolicy acceptance observation has an unrecognized status")
	}
	return nil
}

func (r *AgentReconciler) resolvePolicy(ctx context.Context, agent *api.Agent) (*policyResolution, error) {
	if agent.Spec.Team == "" || agent.Spec.SandboxRef == "" {
		return nil, fmt.Errorf("policy resolution requires spec.team and spec.sandboxRef")
	}
	team := &api.Team{}
	if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.Team}, team); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, policyDependency("read Team: %w", err)
		}
		return nil, fmt.Errorf("read Team: %w", err)
	}
	if team.Spec.DefaultSandboxRef == "" || !slices.Contains(team.Spec.SandboxRefs, team.Spec.DefaultSandboxRef) || !slices.Contains(team.Spec.SandboxRefs, agent.Spec.SandboxRef) {
		return nil, fmt.Errorf("Agent Sandbox is not permitted by the current Team")
	}
	if err := r.verifyTeamAcceptance(ctx, team); err != nil {
		return nil, err
	}
	policyRef := team.Spec.AgentPolicyRef
	if policyRef == "" {
		return nil, fmt.Errorf("Team has no AgentPolicy")
	}
	policy := &api.AgentPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: policyRef}, policy); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, policyDependency("read effective AgentPolicy: %w", err)
		}
		return nil, fmt.Errorf("read effective AgentPolicy: %w", err)
	}
	if err := r.verifyPolicyAcceptance(ctx, policy); err != nil {
		return nil, err
	}
	if !slices.Contains(policy.Spec.Harnesses.Allowed, agent.Spec.Agent.Type) {
		return nil, fmt.Errorf("Agent harness %q is not allowed by policy", agent.Spec.Agent.Type)
	}
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		if r.SandboxPreparation == nil || !r.SandboxPreparation.NativeExecution || !r.customGatewaySelected(agent) {
			return nil, fmt.Errorf("Custom policy execution requires the common executor and a Gateway-qualified image")
		}
		if agentIdentityRequested(agent) && !r.nativeConnectionsRequested(agent) {
			return nil, fmt.Errorf("Custom Agent Identity requires explicit governed Connections support")
		}
	}
	if !aiGatewayHarnessSupported(agent.Spec.Agent.Type) && !r.customGatewaySelected(agent) {
		return nil, fmt.Errorf("Agent harness %q has no typed AI Gateway enforcement adapter", agent.Spec.Agent.Type)
	}
	if !r.AIGateway.Enabled || !r.AIGateway.configured() {
		return nil, fmt.Errorf("AI Gateway is not enabled and fully configured; policy-controlled Agents cannot use a direct model provider path")
	}
	if err := validatePolicySkills(agent, team, policy); err != nil {
		return nil, err
	}
	budget, models, err := effectivePolicyModels(agent, policy)
	if err != nil {
		return nil, err
	}
	if err := validatePolicyIdentity(agent, policy); err != nil {
		return nil, err
	}
	// A display catalog is not authorization evidence. Read the current managed
	// model groups before computing the Gateway projection revision, so a
	// change in which shared model names are bound, or to which groups,
	// invalidates both the projection and any previously computed grant. The
	// fingerprint covers model names and their trusted groups only; see
	// aiGatewayCatalogFingerprint for why configuration edits that keep a
	// name leave the revision, and therefore running Agents, unchanged.
	catalogFingerprint := ""
	if agent.Spec.SharedModelsEnabled() {
		gateway, err := r.aiGatewayClient(ctx)
		if err != nil {
			return nil, policyDependency("create AI Gateway client for policy catalog: %w", err)
		}
		if err := r.refreshAIGatewayCatalogForGrant(ctx, gateway); err != nil {
			return nil, policyDependency("refresh live AI Gateway catalog: %w", err)
		}
		// Model router names are granted by their own checks
		// (ai_gateway_routers.go), not resolved as shared models.
		sharedModels, err := r.aiGatewayPolicySharedModels(ctx, models)
		if err != nil {
			return nil, policyDependency("list Team model routers: %w", err)
		}
		if _, complete := r.aiGatewayCatalog.resolvedGroups(sharedModels); !complete {
			return nil, policyDependency("the live AI Gateway catalog cannot resolve every authorized shared model")
		}
		// Only the bindings of the models this policy allows count: a shared
		// model added to or removed from the Gateway that the policy does not
		// name changes nothing for its Agents, and must not re-project them.
		var known bool
		catalogFingerprint, known = r.aiGatewayCatalog.policyFingerprint(sharedModels)
		if !known || catalogFingerprint == "" {
			return nil, policyDependency("live AI Gateway managed catalog is unavailable")
		}
	}
	gatewayRevision, err := policyHash(struct {
		Version            string    `json:"version"`
		PolicyUID          types.UID `json:"policyUID"`
		Models             []string  `json:"models"`
		Budget             string    `json:"budget"`
		RequestedShared    bool      `json:"requestedShared"`
		RequestedCopilot   bool      `json:"requestedCopilot"`
		CatalogFingerprint string    `json:"catalogFingerprint"`
	}{"gateway-policy-v2", policy.UID, models, budget, agent.Spec.SharedModelsEnabled(), agent.Spec.GitHubCopilotEnabled(), catalogFingerprint})
	if err != nil {
		return nil, err
	}
	credentialRevision, err := policyHash(struct {
		Version   string                       `json:"version"`
		PolicyUID types.UID                    `json:"policyUID"`
		Enabled   bool                         `json:"enabled"`
		Identity  api.AgentPolicyAgentIdentity `json:"identity"`
	}{"credential-policy-v2", policy.UID, agentIdentityRequested(agent), policy.Spec.AgentIdentity})
	if err != nil {
		return nil, err
	}
	// Only an active credential projection needs identity/owner-bound evidence.
	// Keep the existing inactive-domain input hash so enabling this executor
	// does not reproject unrelated identity-free workloads.
	if agentIdentityRequested(agent) {
		credentialRevision = credentialPolicyProjection(agent, team, policy).Revision()
	}
	sandboxRevision, err := r.desiredSandboxPolicyRevision(ctx, agent, team)
	if err != nil {
		if errors.Is(err, runtimebindings.ErrSkillsMaterializationPending) {
			return nil, policyDependency("%w", err)
		}
		return nil, err
	}
	return &policyResolution{team: team, policy: policy, gatewayRevision: gatewayRevision, credentialRevision: credentialRevision, sandboxRevision: sandboxRevision}, nil
}

// desiredSandboxPolicyRevision compiles the current desired Sandbox controls
// without applying them. Runtime availability is intentionally ignored here:
// an unavailable runtime still has a well-defined desired revision, while
// SandboxReady remains false until the enforcement adapter supplies fresh
// read-back evidence for that exact revision.
func (r *AgentReconciler) desiredSandboxPolicyRevision(ctx context.Context, agent *api.Agent, team *api.Team) (string, error) {
	if r.SandboxPreparation == nil || r.SandboxPreparation.Configuration == nil {
		// Standalone Gateway-only deployments have no Sandbox compiler. They can
		// preserve existing Sandbox history, but cannot turn that historical
		// evidence into a desired revision or make a new projection ready.
		return "", nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if reader == nil {
		return "", policyDependency("Sandbox policy reader is unavailable")
	}
	snapshot, err := configurationSnapshot(ctx, r.SandboxPreparation.Configuration, r.SandboxPreparation.Now, r.SandboxPreparation.MaxSnapshotAge)
	if err != nil {
		return "", policyDependency("read Sandbox platform configuration: %w", err)
	}
	if len(team.Spec.SandboxRefs) > 128 {
		return "", fmt.Errorf("Sandbox dependency inventory exceeds its bound")
	}
	resources := sandbox.Resources{Sandboxes: map[string]*api.Sandbox{}, AgentPolicies: map[string]*api.AgentPolicy{}}
	for _, name := range uniqueReferences(append(slices.Clone(team.Spec.SandboxRefs), team.Spec.DefaultSandboxRef, agent.Spec.SandboxRef)) {
		object := &api.Sandbox{}
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, object); err != nil {
			if apierrors.IsNotFound(err) {
				return "", fmt.Errorf("read Sandbox %q: %w", name, err)
			}
			return "", policyDependency("read Sandbox %q: %w", name, err)
		}
		resources.Sandboxes[name] = object
	}
	for _, name := range uniqueReferences([]string{team.Spec.AgentPolicyRef}) {
		object := &api.AgentPolicy{}
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, object); err != nil {
			if apierrors.IsNotFound(err) {
				return "", fmt.Errorf("read AgentPolicy %q: %w", name, err)
			}
			return "", policyDependency("read AgentPolicy %q: %w", name, err)
		}
		resources.AgentPolicies[name] = object
	}
	authorized, err := sandboxbinding.AuthorizedBinding(agent)
	if err != nil {
		return "", err
	}
	selected := resources.Sandboxes[agent.Spec.SandboxRef]
	registration, exists := snapshot.Registrations[selected.Spec.Type]
	if !exists {
		return "", fmt.Errorf("Sandbox runtime %q has no registered policy compiler", selected.Spec.Type)
	}
	registration.Available = true
	registrations := make(map[api.SandboxType]sandbox.Registration, len(snapshot.Registrations))
	for key, value := range snapshot.Registrations {
		registrations[key] = value
	}
	registrations[selected.Spec.Type] = registration
	resolution, err := sandbox.Resolve(sandbox.ResolveInput{
		NativeExecution: r.SandboxPreparation.NativeExecution,
		Agent:           agent, Team: team, Resources: resources, Registrations: registrations, AuthorizedBinding: authorized,
	})
	if err != nil {
		return "", err
	}
	platform, exists := snapshot.Platforms[resolution.Sandbox.Type]
	if !exists {
		return "", policyDependency("Sandbox platform configuration for %q is unavailable", resolution.Sandbox.Type)
	}
	selectedPolicy := resources.AgentPolicies[team.Spec.AgentPolicyRef]
	if err := sandboxpolicy.VerifyPolicyGrants(ctx, reader, selectedPolicy, platform.Grants); err != nil {
		if errors.Is(err, sandboxpolicy.ErrUnavailable) {
			return "", policyDependency("verify Sandbox policy grants: %w", err)
		}
		return "", err
	}
	var identityPin *sandboxpolicy.IdentityPin
	if (resolution.AgentPolicy.EntraID.Enabled || resolution.AgentPolicy.KubernetesAccess != nil) && r.SandboxPreparation.NativeExecution {
		common, err := r.usesReconciledSandbox(ctx, agent)
		if err != nil {
			return "", policyDependency("read identity lifetime: %w", err)
		}
		attempt := &sandboxPreparationAttempt{reconciled: common, agent: agent, team: team,
			resolution: resolution, configuration: snapshot}
		identityPin, err = r.sandboxIdentityPin(ctx, attempt)
		if err != nil {
			if errors.Is(err, sandboxpolicy.ErrUnapprovedGrant) {
				return "", err
			}
			return "", policyDependency("%w: %w", errSandboxIdentityPending, err)
		}
	}
	connections, _, err := r.nativeConnectionsDestination(ctx, agent, platform)
	if err != nil {
		return "", policyDependency("resolve native Connections destination: %w", err)
	}
	var customIngress *sandboxpolicy.CustomIngress
	var terminalIngress *sandboxpolicy.TerminalIngress
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		values, user, err := r.customSandboxBaseValues(agent, team)
		if err != nil {
			return "", err
		}
		terminal, err := customSandboxTerminalInput(values, user, instanceServiceName(agent.Name))
		if err != nil {
			return "", err
		}
		customIngress, terminalIngress = r.customSandboxIngress(agent), terminalSandboxIngress(terminal)
	}
	plan, err := sandboxpolicy.Compile(sandboxpolicy.CompileInput{
		ConnectionsMCP:  connections,
		NativeExecution: r.SandboxPreparation.NativeExecution,
		Resolution:      *resolution, AgentName: agent.Name, Namespace: instanceNamespace(agent.Name),
		ServiceName: instanceServiceName(agent.Name), Platform: platform,
		CustomIngress: customIngress, TerminalIngress: terminalIngress,
		IdentityPin: identityPin,
	})
	if err != nil {
		if errors.Is(err, sandboxpolicy.ErrUnavailable) {
			return "", policyDependency("compile Sandbox policy: %w", err)
		}
		return "", err
	}
	return plan.Revision, nil
}

func validatePolicySkills(agent *api.Agent, team *api.Team, policy *api.AgentPolicy) error {
	requested := append(append([]string{}, team.Spec.Skills...), agent.Spec.SkillOverrides.Add...)
	for _, skill := range requested {
		if !slices.Contains(policy.Spec.Skills.Allowed, skill) {
			return fmt.Errorf("skill %q is not allowed by policy", skill)
		}
	}
	return nil
}

func effectivePolicyModels(agent *api.Agent, policy *api.AgentPolicy) (string, []string, error) {
	shared := agent.Spec.SharedModelsEnabled()
	copilot := agent.Spec.GitHubCopilotEnabled()
	if shared && len(policy.Spec.ModelAccess.SharedModels.Allowed) == 0 {
		return "", nil, fmt.Errorf("shared models were requested but policy grants none")
	}
	if copilot && !policy.Spec.ModelAccess.Copilot.Allowed {
		return "", nil, fmt.Errorf("GitHub Copilot was requested but policy does not allow it")
	}
	models := []string{}
	if shared {
		models = append(models, policy.Spec.ModelAccess.SharedModels.Allowed...)
	}
	if agent.Spec.Model != "" && !(shared && slices.Contains(models, agent.Spec.Model)) && !(copilot && copilotDefaultModelAllowed(agent.Spec.Model)) {
		return "", nil, fmt.Errorf("requested default model %q is not authorized", agent.Spec.Model)
	}
	if !shared && !copilot {
		if agent.Spec.ModelAccess != nil && agent.Spec.ModelAccess.MonthlyBudgetUSD != "" {
			return "", nil, fmt.Errorf("an Agent without model entitlement must omit monthlyBudgetUSD")
		}
		return "", models, nil
	}
	if policy.Spec.ModelAccess.GatewayBudget == nil {
		return "", nil, fmt.Errorf("policy must define a Gateway budget for model access")
	}
	budget := policy.Spec.ModelAccess.GatewayBudget.DefaultMonthlyUSD
	if agent.Spec.ModelAccess != nil && agent.Spec.ModelAccess.MonthlyBudgetUSD != "" {
		budget = agent.Spec.ModelAccess.MonthlyBudgetUSD
	}
	if err := validateBudget(budget, policy.Spec.ModelAccess.GatewayBudget.MaxMonthlyUSD); err != nil {
		return "", nil, err
	}
	return budget, models, nil
}

func validateBudget(value, maximum string) error {
	v, valid := new(big.Rat).SetString(value)
	if !valid || v.Sign() <= 0 {
		return fmt.Errorf("gateway budget must be a positive decimal")
	}
	m, valid := new(big.Rat).SetString(maximum)
	if !valid || m.Sign() <= 0 || v.Cmp(m) > 0 {
		return fmt.Errorf("gateway budget exceeds the policy maximum")
	}
	return nil
}

func validatePolicyIdentity(agent *api.Agent, policy *api.AgentPolicy) error {
	requested := agent.Spec.AgentIdentity != nil && agent.Spec.AgentIdentity.Enabled
	identity := policy.Spec.AgentIdentity
	if identity.Required && !requested {
		return fmt.Errorf("policy requires Agent Identity")
	}
	if requested && (!identity.Allowed || !slices.Contains(identity.CredentialServices, "azure-resource-manager")) {
		return fmt.Errorf("Agent Identity is not permitted by policy")
	}
	return nil
}

func policyHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (r *AgentReconciler) recordPolicy(ctx context.Context, agent *api.Agent, resolution *policyResolution, resolutionErr error) error {
	before := agent.Status.DeepCopy()
	status := &api.PolicyStatus{ObservedGeneration: agent.Generation}
	if resolutionErr != nil {
		if agent.Status.AgentIdentity != nil {
			agent.Status.AgentIdentity.CredentialPolicyRevision = ""
			setCredentialPolicyCondition(agent, metav1.ConditionFalse, "PolicyUnresolved")
		}
		if isPolicyDependencyError(resolutionErr) {
			if agent.Status.Policy != nil {
				status = agent.Status.Policy.DeepCopy()
				status.ObservedGeneration = agent.Generation
			}
			status.Phase = "Pending"
			setCondition(&agent.Status.Conditions, api.ConditionPolicyResolved, metav1.ConditionFalse, "DependencyUnavailable", resolutionErr.Error())
			setCondition(&agent.Status.Conditions, api.ConditionPolicyApplied, metav1.ConditionFalse, "DependencyUnavailable", "Policy enforcement is blocked until current assignment evidence can be read")
		} else {
			status.Phase = "Denied"
			setCondition(&agent.Status.Conditions, api.ConditionPolicyResolved, metav1.ConditionFalse, "PolicyDenied", resolutionErr.Error())
			setCondition(&agent.Status.Conditions, api.ConditionPolicyApplied, metav1.ConditionFalse, "PolicyDenied", "Policy enforcement is withheld until the assignment and requests are valid")
		}
	} else {
		status.EffectivePolicyRef = &api.EffectivePolicyReference{PolicyObjectReference: api.PolicyObjectReference{Name: resolution.policy.Name, UID: resolution.policy.UID, Generation: resolution.policy.Generation}}
		status.Enforcement = api.PolicyEnforcementStatus{Sandbox: api.PolicyRevisionExpectation{DesiredRevision: resolution.sandboxRevision}, Gateway: api.PolicyRevisionExpectation{DesiredRevision: resolution.gatewayRevision}, Credential: api.PolicyRevisionExpectation{DesiredRevision: resolution.credentialRevision}}
		status.Phase = "Pending"
		setCondition(&agent.Status.Conditions, api.ConditionPolicyResolved, metav1.ConditionTrue, "EffectivePolicyResolved", "The Team selection, requests, Sandbox, and effective AgentPolicy are valid")
		verified := policyEnforcementVerified(agent, resolution)
		if verified {
			status.Phase = "Applied"
			setCondition(&agent.Status.Conditions, api.ConditionPolicyApplied, metav1.ConditionTrue, "RequiredEnforcementVerified", "Every required policy revision matches component evidence")
		} else {
			setCondition(&agent.Status.Conditions, api.ConditionPolicyApplied, metav1.ConditionFalse, "EnforcementPending", "Waiting for Sandbox, Gateway, or credential policy enforcement")
		}
	}
	agent.Status.Policy = status
	if reflect.DeepEqual(*before, agent.Status) {
		return nil
	}
	return r.updateStatus(ctx, agent)
}

func policyEnforcementVerified(agent *api.Agent, resolution *policyResolution) bool {
	if agent == nil || resolution == nil || resolution.team == nil || resolution.policy == nil {
		return false
	}
	sandboxReady := meta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
	if agent.Status.Sandbox == nil || agent.Status.Sandbox.AppliedRevision == "" ||
		agent.Status.Sandbox.AppliedRevision != resolution.sandboxRevision ||
		agent.Status.Sandbox.TeamUID != resolution.team.UID ||
		agent.Status.Sandbox.AgentPolicyUID != resolution.policy.UID ||
		sandboxReady == nil ||
		sandboxReady.Status != metav1.ConditionTrue || sandboxReady.ObservedGeneration != agent.Generation {
		return false
	}
	needsModels := agent.Spec.SharedModelsEnabled() || agent.Spec.GitHubCopilotEnabled()
	if needsModels && (agent.Status.AIGateway == nil || agent.Status.AIGateway.Phase != api.AIGatewayPhaseReady || !agent.Status.AIGateway.Routed || agent.Status.AIGateway.PolicyRevision != resolution.gatewayRevision) {
		return false
	}
	if agentIdentityRequested(agent) {
		projection := credentialPolicyProjection(agent, resolution.team, resolution.policy)
		if !projection.ValidApplied() || projection.Revision() != resolution.credentialRevision {
			return false
		}
	}
	return credentialPolicyVerified(agent, resolution.credentialRevision)
}

// policyReprojectionMustHold detects a change away from a projection that
// may still be in use. It intentionally does not hold a brand-new Pending
// Agent: its component controllers need an opportunity to create and verify
// the first Sandbox and Gateway projections. A persisted Ready/routed Agent,
// however, is fail-closed before it can retain the previous projection.
func policyReprojectionMustHold(agent *api.Agent, resolution *policyResolution) bool {
	if resolution == nil || !policyWorkloadMayBeActive(agent) {
		return false
	}
	existing := agent.Status.Policy
	if existing == nil {
		return true
	}
	if existing.Enforcement.Sandbox.DesiredRevision != resolution.sandboxRevision ||
		existing.Enforcement.Gateway.DesiredRevision != resolution.gatewayRevision ||
		existing.Enforcement.Credential.DesiredRevision != resolution.credentialRevision {
		return true
	}
	return !policyEnforcementVerified(agent, resolution)
}

func policyWorkloadMayBeActive(agent *api.Agent) bool {
	if agent == nil {
		return false
	}
	ingress := meta.FindStatusCondition(agent.Status.Conditions, api.ConditionIngressReady)
	// IngressReady=True also represents an intentionally absent route. That is
	// verified enforcement, not evidence that an old workload may still be
	// reachable; treating it as activity would re-enter the reprojection hold
	// immediately after the workload and Gateway were fenced.
	ingressMayBeActive := ingress != nil && ingress.Status == metav1.ConditionTrue && ingress.Reason != "IngressDisabled"
	if agent.Status.Phase == api.PhaseReady || meta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionReady) || ingressMayBeActive {
		return true
	}
	return false
}
