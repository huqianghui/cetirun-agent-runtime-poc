// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SandboxWriterExclusionSource confirms termination or durable guest-external
// fencing of every old writer for the supplied Agent/namespace/HOME identities.
// Implementations must support historical tuples during cleanup; Kubernetes
// Pod absence, replicas, PVC access modes and node readiness are not evidence.
type SandboxWriterExclusionSource interface {
	CheckQuiesced(context.Context, *api.Agent, SandboxPrerequisites) error
}

// KubernetesSandboxPrerequisites selects existing resources with authoritative
// reads only. Names, chart version and helpers are immutable trusted startup
// configuration, never Agent Helm values. It neither provisions nor adopts
// resources, and no production WriterExclusion source is installed by default.
type KubernetesSandboxPrerequisites struct {
	NativeExecution            bool
	APIReader                  client.Reader
	HomeClaimName              string
	SkillsClaimName            string
	OperatorNamespace          string
	SkillsCatalogConfigMapName string
	ChartVersion               string
	Helpers                    sandboxworkload.Helpers
	Now                        func() time.Time
	WriterExclusion            SandboxWriterExclusionSource
	NativeQuiescence           *KubernetesNativeQuiescence
	// SkillMarketplace reports a missing skills claim of a common-executor
	// Agent as errSkillsClaimPending: the operator's Skill Marketplace claim
	// controller creates that claim after the Agent's namespace exists.
	SkillMarketplace bool
}

var _ SandboxPreparationSource = (*KubernetesSandboxPrerequisites)(nil)

func (p *KubernetesSandboxPrerequisites) Resolve(ctx context.Context, requested *api.Agent, requestedTeam *api.Team) (SandboxPrerequisites, error) {
	return p.resolve(ctx, requested, requestedTeam, true)
}

// ResolveRunning refreshes the same prerequisites after a separately authorized
// hold transition. It neither supplies writer exclusion nor authorizes a start.
func (p *KubernetesSandboxPrerequisites) ResolveRunning(ctx context.Context, requested *api.Agent, requestedTeam *api.Team) (SandboxPrerequisites, error) {
	return p.resolve(ctx, requested, requestedTeam, false)
}

func (p *KubernetesSandboxPrerequisites) ResolveCurrent(ctx context.Context, requested *api.Agent, requestedTeam *api.Team) (SandboxPrerequisites, error) {
	return p.resolveCurrent(ctx, requested, requestedTeam, false, true)
}

func (p *KubernetesSandboxPrerequisites) resolve(ctx context.Context, requested *api.Agent, requestedTeam *api.Team, held bool) (SandboxPrerequisites, error) {
	return p.resolveCurrent(ctx, requested, requestedTeam, held, false)
}

func (p *KubernetesSandboxPrerequisites) resolveCurrent(ctx context.Context, requested *api.Agent, requestedTeam *api.Team, held, reconciled bool) (SandboxPrerequisites, error) {
	var empty SandboxPrerequisites
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if p == nil || p.APIReader == nil {
		return empty, fmt.Errorf("%w: Sandbox prerequisites require an authoritative API reader", sandboxpolicy.ErrUnavailable)
	}
	source := *p
	for i, name := range []string{source.HomeClaimName, source.SkillsClaimName} {
		if i == 1 && name == "" && source.NativeExecution && source.SkillsCatalogConfigMapName == "" {
			continue
		}
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return empty, fmt.Errorf("%w: Sandbox HOME and skills require literal configured PVC names", sandboxpolicy.ErrUnavailable)
		}
	}
	if source.HomeClaimName == source.SkillsClaimName {
		return empty, fmt.Errorf("%w: Sandbox HOME and skills must be distinct claims", sandboxpolicy.ErrUnavailable)
	}
	if _, err := semver.StrictNewVersion(source.ChartVersion); err != nil {
		return empty, fmt.Errorf("%w: Sandbox prerequisites require an explicitly approved chart version", sandboxpolicy.ErrUnavailable)
	}
	if requested == nil || requestedTeam == nil {
		return empty, fmt.Errorf("Sandbox prerequisites require the requested Agent and Team")
	}
	expectedAgent, err := sandboxworkload.AgentSourceReference(requested)
	if err != nil {
		return empty, err
	}
	observedAt := configurationNow(source.Now)
	if observedAt.IsZero() {
		return empty, fmt.Errorf("%w: Sandbox prerequisite observation requires a clock", sandboxpolicy.ErrUnavailable)
	}
	agent, binding, err := sandboxbinding.Read(ctx, source.APIReader, client.ObjectKeyFromObject(requested))
	if err != nil {
		return empty, fmt.Errorf("read Sandbox prerequisite Agent: %w", err)
	}
	agentSource, err := sandboxworkload.AgentSourceReference(agent)
	if err != nil {
		return empty, err
	}
	if agentSource != expectedAgent || (!reconciled && (agent.Spec.Suspended || slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) != held)) {
		return empty, fmt.Errorf("%w: Sandbox prerequisites require the exact current Agent intent and lifecycle state", errSandboxSourceChanged)
	}
	if !reconciled && (!sandboxPreparationSupported(agent) || (agent.Spec.Agent.Type != api.AgentTypeOpenClaw && source.Helpers.Doctor)) {
		return empty, fmt.Errorf("%w: Sandbox prerequisites support only the existing native held path and its helpers", sandboxpolicy.ErrUnsupportedControl)
	}
	if agent.Spec.Agent.Type != api.AgentTypeOpenClaw {
		source.Helpers.Doctor = false
	}
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		source.Helpers = sandboxworkload.Helpers{}
	}
	if reconciled {
		if _, err := semver.StrictNewVersion(agent.Spec.Helm.Version); err != nil {
			return empty, fmt.Errorf("Sandbox reconciliation requires a pinned native chart version")
		}
		source.ChartVersion = agent.Spec.Helm.Version
	}
	if agent.Spec.Helm.Version != "" && agent.Spec.Helm.Version != source.ChartVersion {
		return empty, fmt.Errorf("Sandbox Agent chart version differs from the configured approved version")
	}
	namespaceName := instanceNamespace(agent.Name)
	if len(validation.IsDNS1123Label(namespaceName)) != 0 ||
		len(validation.IsDNS1123Subdomain(sandboxRuntimeSecretName(agent))) != 0 {
		return empty, fmt.Errorf("Sandbox prerequisites require a valid derived namespace and explicit runtime Secret name")
	}

	team := &api.Team{}
	if err := source.readPrerequisite(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
		return empty, err
	}
	teamMatchesRequest := requestedTeam.DeletionTimestamp.IsZero() && team.Generation == requestedTeam.Generation &&
		sandboxSource(team, api.GroupVersion.String(), "Team") == sandboxSource(requestedTeam, api.GroupVersion.String(), "Team") &&
		equality.Semantic.DeepEqual(team.Spec, requestedTeam.Spec)
	if reconciled && source.NativeExecution {
		teamMatchesRequest = sameReconciledSandboxTeamInputs(requestedTeam, team)
	}
	if team.UID != binding.TeamUID || !teamMatchesRequest ||
		!slices.Contains(team.Spec.SandboxRefs, agent.Spec.SandboxRef) {
		return empty, fmt.Errorf("%w: Sandbox prerequisite Team identity or selection changed", errSandboxSourceChanged)
	}
	box := &api.Sandbox{}
	if err := source.readPrerequisite(ctx, client.ObjectKey{Name: agent.Spec.SandboxRef}, box); err != nil {
		return empty, err
	}
	if box.UID != binding.SandboxUID {
		return empty, fmt.Errorf("%w: Sandbox prerequisite storage profile identity changed", errSandboxSourceChanged)
	}
	profile, problems := sandbox.NormalizeSandbox(box.Spec)
	if len(problems) != 0 {
		return empty, fmt.Errorf("Sandbox prerequisite storage profile is invalid: %w", problems.ToAggregate())
	}
	namespace := &corev1.Namespace{}
	if err := source.readPrerequisite(ctx, client.ObjectKey{Name: namespaceName}, namespace); err != nil {
		return empty, err
	}
	if !sandboxBuiltinOwned(namespace, agent) ||
		!equality.Semantic.DeepEqual(namespace.OwnerReferences, []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)}) ||
		namespace.Labels[sandboxworkload.ProtectedNamespaceLabel] != "true" ||
		namespace.Annotations[sandboxworkload.AgentNameAnnotation] != agent.Name ||
		namespace.Annotations[sandboxworkload.AgentUIDAnnotation] != string(agent.UID) {
		return empty, fmt.Errorf("Sandbox prerequisite namespace protection and exact Agent ownership are required")
	}
	mode := sandboxSkillsMode(source.NativeExecution, agent, team)
	// A recorded publication is a retained storage contract, even when the Team
	// subsequently clears its selection. Never replace that contract with empty skills.
	if mode == sandboxworkload.SkillsModeEmpty && (namespace.Annotations[sandboxBootstrapAnnotation] != "" ||
		slices.Contains(agent.Finalizers, sandbox.BootstrapFinalizer)) {
		reader := &AgentReconciler{APIReader: source.APIReader, OperatorNamespace: source.OperatorNamespace}
		record, receipt, err := reader.readSandboxBootstrap(ctx, agent)
		if err != nil {
			return empty, err
		}
		if receipt.Skills != nil || (source.SkillsCatalogConfigMapName != "" && record.Annotations[sandboxBootstrapSkillsModeAnnotation] == "") {
			mode = ""
		}
	}
	home := &corev1.PersistentVolumeClaim{}
	var skills *corev1.PersistentVolumeClaim
	claims := []struct {
		name   string
		object *corev1.PersistentVolumeClaim
	}{{source.HomeClaimName, home}}
	if mode != sandboxworkload.SkillsModeEmpty {
		if source.SkillsClaimName == "" {
			return empty, fmt.Errorf("%w: selected skills require a configured skills PVC or published skills catalog", sandboxpolicy.ErrUnavailable)
		}
		skills = &corev1.PersistentVolumeClaim{}
		claims = append(claims, struct {
			name   string
			object *corev1.PersistentVolumeClaim
		}{source.SkillsClaimName, skills})
	}
	for _, claim := range claims {
		if err := source.readPrerequisite(ctx, client.ObjectKey{Namespace: namespace.Name, Name: claim.name}, claim.object); err != nil {
			if claim.object == skills {
				if source.skillsClaimPending(err, reconciled) {
					return empty, fmt.Errorf("%w: %w", errSkillsClaimPending, err)
				}
				return empty, fmt.Errorf("Sandbox skills require the configured skills PVC or validated publication: %w", err)
			}
			return empty, err
		}
		if claim.object.Status.Phase == corev1.ClaimLost {
			return empty, fmt.Errorf("Sandbox prerequisite PVC %s is lost", claim.name)
		}
	}
	if home.Annotations[sandboxworkload.AgentUIDAnnotation] != string(agent.UID) ||
		!sandboxPrerequisiteOwnersMatch(home, agent, nil) {
		return empty, fmt.Errorf("Sandbox HOME requires confirmed Agent UID ownership without a foreign or workload-lifetime owner")
	}
	homeProfile := runtimebindings.HomeClaim{
		StorageClass: profile.Storage.Home.StorageClassName,
		StorageSize:  profile.Storage.Home.Size.String(),
		AccessMode:   string(profile.Storage.Home.AccessMode),
	}
	if !homeProfile.Matches(home) ||
		ptr.Deref(home.Spec.VolumeMode, corev1.PersistentVolumeFilesystem) != corev1.PersistentVolumeFilesystem {
		return empty, fmt.Errorf("%w: existing Sandbox HOME does not match its authorized storage profile", sandboxworkload.ErrStorageTransitionRequired)
	}
	// Shared skills are read-only inputs, not HOME. They may be retained without
	// owner references or owned by the current Team, and may use ReadWriteMany.
	if skills != nil && (skills.UID == home.UID || !sandboxPrerequisiteOwnersMatch(skills, agent, team) ||
		ptr.Deref(skills.Spec.VolumeMode, corev1.PersistentVolumeFilesystem) != corev1.PersistentVolumeFilesystem) {
		return empty, fmt.Errorf("Sandbox skills require a distinct current filesystem PVC without foreign ownership")
	}
	secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
	if err := source.readPrerequisite(ctx, client.ObjectKey{Namespace: namespace.Name, Name: sandboxRuntimeSecretName(agent)}, secret); err != nil {
		return empty, err
	}
	if !sandboxBuiltinOwned(secret, agent) || !sandboxPrerequisiteOwnersMatch(secret, agent, nil) {
		return empty, fmt.Errorf("Sandbox runtime Secret requires confirmed Agent UID ownership")
	}
	publicationSources, err := source.publishedSkillsSources(ctx, agent, team, namespace, home, skills, secret)
	if err != nil {
		return empty, err
	}
	sources := []sandboxworkload.SourceReference{
		agentSource, sandboxSource(team, api.GroupVersion.String(), "Team"),
		sandboxSource(box, api.GroupVersion.String(), "Sandbox"), sandboxSource(namespace, "v1", "Namespace"),
		sandboxSource(home, "v1", "PersistentVolumeClaim"),
	}
	objects := []client.Object{team, box, namespace, home}
	var skillsID sandboxworkload.ObjectIdentity
	if skills != nil {
		sources = append(sources, sandboxSource(skills, "v1", "PersistentVolumeClaim"))
		objects = append(objects, skills)
		skillsID = sandboxworkload.ObjectIdentity{Name: skills.Name, UID: skills.UID}
	}
	sources = append(sources, sandboxSource(secret, "v1", "Secret"))
	sources = append(sources, publicationSources...)
	revisionSources := slices.Clone(sources)
	revisionAgent := agent.DeepCopy()
	// Preparation records its cleanup finalizer after Resolve, before its first
	// guard. That bookkeeping must not invalidate the approved input revision;
	// the exact lifecycle intent is still checked within this read interval.
	revisionAgent.Finalizers = slices.DeleteFunc(revisionAgent.Finalizers, func(value string) bool { return value == sandbox.PreparationFinalizer })
	if !held {
		revisionAgent.Finalizers = append(revisionAgent.Finalizers, sandbox.HoldFinalizer)
	}
	revisionSources[0], err = sandboxworkload.AgentSourceReference(revisionAgent)
	if err != nil {
		return empty, err
	}
	type revisionInput struct {
		Source sandboxworkload.SourceReference
		Digest string
	}
	revisionInputs := make([]revisionInput, 0, len(revisionSources))
	known := map[string]client.Object{}
	for i, object := range objects {
		known[sandboxworkload.SourceKey(sources[i+1])] = object
	}
	verifier := &AgentReconciler{APIReader: source.APIReader}
	for _, reference := range revisionSources {
		input := revisionInput{Source: reference}
		if sandboxworkload.EffectiveSource(reference) {
			object := known[sandboxworkload.SourceKey(reference)]
			switch {
			case reference.Kind == "Team" && reference.APIVersion == api.GroupVersion.String():
				teamInputs, err := sandboxworkload.ExecutionTeamInputs(team)
				if err != nil {
					return empty, err
				}
				input.Digest, err = teamInputs.DigestFor(agent.Spec.SandboxRef)
			case object != nil:
				input.Digest, err = sandboxworkload.ExecutionObjectDigest(object)
			default:
				input.Digest, err = verifier.executionSourceDigest(ctx, reference, agent.Spec.SandboxRef, true)
			}
			if err != nil {
				return empty, err
			}
			input.Source.ResourceVersion = ""
		}
		revisionInputs = append(revisionInputs, input)
	}
	revision, err := sandboxPreparationDigest(struct {
		Version      string
		Inputs       []revisionInput
		ChartVersion string
		Helpers      sandboxworkload.Helpers
		SkillsMode   sandboxworkload.SkillsMode `json:",omitempty"`
	}{"kubernetes-prerequisites-v2", revisionInputs, source.ChartVersion, source.Helpers, mode})
	if err != nil {
		return empty, err
	}
	recheckedSources := sources
	if source.NativeExecution && !held {
		if err := validateSandboxSourceInventory(sources); err != nil {
			return empty, err
		}
		recheckedSources = slices.Clone(sources)
		for _, claim := range claims {
			reference := sandboxSource(claim.object, "v1", "PersistentVolumeClaim")
			before, err := sandboxworkload.NativeClaimDigest(claim.object)
			if err != nil {
				return empty, err
			}
			after, err := verifier.nativeSourceDigest(ctx, reference, agent.Spec.SandboxRef, false)
			if err != nil {
				return empty, fmt.Errorf("recheck native Sandbox claim: %w", err)
			}
			if before != after {
				return empty, fmt.Errorf("%w: native Sandbox claim storage or ownership changed: %s",
					errSandboxSourceChanged, sandboxworkload.SourceKey(reference))
			}
			// Native admission already permits ordinary binding of these exact
			// claims. Do not turn a binding-only read race into a one-shot hold.
			recheckedSources = slices.DeleteFunc(recheckedSources, func(candidate sandboxworkload.SourceReference) bool {
				return candidate == reference
			})
		}
	}
	if reconciled && source.NativeExecution {
		if err := validateSandboxSourceInventory(recheckedSources); err != nil {
			return empty, err
		}
		for _, reference := range recheckedSources {
			if reference.APIVersion == api.GroupVersion.String() && reference.Kind == "Team" {
				if err := verifier.verifyReconciledSandboxTeamSource(ctx, reference, team); err != nil {
					return empty, fmt.Errorf("recheck Sandbox prerequisites: %w", err)
				}
				continue
			}
			if _, err := verifier.readSandboxSource(ctx, reference); err != nil {
				return empty, fmt.Errorf("recheck Sandbox prerequisites: %w", err)
			}
		}
	} else if err := verifier.verifySandboxSources(ctx, recheckedSources); err != nil {
		return empty, fmt.Errorf("recheck Sandbox prerequisites: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return SandboxPrerequisites{
		AgentUID: agent.UID, AgentGeneration: agent.Generation, Revision: revision, ObservedAt: observedAt,
		NamespaceUID: namespace.UID,
		Home:         sandboxworkload.ObjectIdentity{Name: home.Name, UID: home.UID},
		Skills:       skillsID, SkillsMode: mode,
		RuntimeSecret: sandboxworkload.ObjectIdentity{Name: secret.Name, UID: secret.UID},
		ChartVersion:  source.ChartVersion, Helpers: source.Helpers,
		PublicationSources: publicationSources,
	}, nil
}

func (p *KubernetesSandboxPrerequisites) readPrerequisite(ctx context.Context, key client.ObjectKey, object client.Object) error {
	if err := p.APIReader.Get(ctx, key, object); err != nil {
		return fmt.Errorf("read Sandbox prerequisite %s: %w", key, err)
	}
	if client.ObjectKeyFromObject(object) != key || object.GetUID() == "" ||
		object.GetResourceVersion() == "" || !object.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("%w: Sandbox prerequisite %s is not the persisted, non-terminating object", errSandboxSourceChanged, key)
	}
	return nil
}

func sandboxPrerequisiteOwnersMatch(object client.Object, agent *api.Agent, team *api.Team) bool {
	for key, expected := range map[string]string{
		sandboxworkload.AgentUIDAnnotation:  string(agent.UID),
		sandboxworkload.AgentNameAnnotation: agent.Name,
	} {
		if value := object.GetAnnotations()[key]; value != "" && value != expected {
			return false
		}
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == api.GroupVersion.String() &&
			((owner.Kind == "Agent" && owner.Name == agent.Name && owner.UID == agent.UID) ||
				(team != nil && owner.Kind == "Team" && owner.Name == team.Name && owner.UID == team.UID)) {
			continue
		}
		return false
	}
	return true
}

func (p *KubernetesSandboxPrerequisites) CheckQuiesced(ctx context.Context, agent *api.Agent, prerequisites SandboxPrerequisites) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || (p.WriterExclusion == nil && p.NativeQuiescence == nil) {
		return fmt.Errorf("%w: no qualified guest-external Sandbox writer-exclusion source is installed", sandbox.ErrObservationUnavailable)
	}
	if agent == nil || agent.Namespace != "" || len(validation.IsDNS1123Subdomain(agent.Name)) != 0 ||
		len(validation.IsDNS1123Label(instanceNamespace(agent.Name))) != 0 ||
		agent.UID == "" || agent.Generation < 1 || prerequisites.AgentUID != agent.UID ||
		prerequisites.AgentGeneration < 1 || prerequisites.AgentGeneration > agent.Generation ||
		prerequisites.NamespaceUID == "" || prerequisites.Home.UID == "" ||
		len(validation.IsDNS1123Subdomain(prerequisites.Home.Name)) != 0 {
		return fmt.Errorf("%w: Sandbox writer exclusion requires the exact Agent/namespace/HOME identity tuple", sandbox.ErrObservationUnavailable)
	}
	// Cleanup supplies a durable prior tuple, even after generation, policy,
	// chart, storage configuration or lifecycle changes. Do not Resolve again,
	// compare it with current settings, or require those resources to survive.
	if err := validateSandboxPlacementTuple(agent, prerequisites); err != nil {
		return err
	}
	prerequisites = copySandboxPrerequisites(prerequisites)
	if p.NativeQuiescence != nil {
		return p.NativeQuiescence.CheckQuiesced(ctx, agent.DeepCopy(), prerequisites)
	}
	if err := p.WriterExclusion.CheckQuiesced(ctx, agent.DeepCopy(), prerequisites); err != nil {
		return err
	}
	return ctx.Err()
}
