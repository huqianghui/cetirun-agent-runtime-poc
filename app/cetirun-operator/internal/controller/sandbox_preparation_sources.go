// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errSandboxSourceChanged = sandboxplatform.ErrSourceChanged

func (r *AgentReconciler) sandboxPreparationInput(ctx context.Context, attempt *sandboxPreparationAttempt) error {
	inputs := attempt.prerequisites
	agent := attempt.agent
	config := r.SandboxPreparation
	if err := configurationSnapshotTime(inputs.ObservedAt, configurationNow(config.Now), config.MaxSnapshotAge); err != nil {
		return err
	}
	if inputs.AgentUID != agent.UID || inputs.AgentGeneration != agent.Generation || inputs.Revision == "" ||
		inputs.ChartVersion == "" || (agent.Spec.Helm.Version != "" && agent.Spec.Helm.Version != inputs.ChartVersion) ||
		inputs.NamespaceUID == "" || inputs.RuntimeSecret.Name == "" || inputs.RuntimeSecret.UID == "" ||
		sandboxRuntimeSecretName(agent) != inputs.RuntimeSecret.Name {
		return fmt.Errorf("Sandbox prerequisites must authorize this Agent generation, chart and runtime Secret")
	}
	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
		return err
	}
	owner := []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)}
	if namespace.UID != inputs.NamespaceUID || namespace.Name == r.OperatorNamespace ||
		!namespace.DeletionTimestamp.IsZero() ||
		namespace.Labels[sandboxworkload.ProtectedNamespaceLabel] != "true" ||
		namespace.Annotations[sandboxworkload.AgentNameAnnotation] != agent.Name ||
		namespace.Annotations[sandboxworkload.AgentUIDAnnotation] != string(agent.UID) ||
		!equality.Semantic.DeepEqual(namespace.OwnerReferences, owner) {
		return fmt.Errorf("Sandbox namespace protection or UID ownership is not established")
	}
	if err := inputs.SkillsMode.ValidateIdentity(inputs.Skills); err != nil {
		return err
	}
	if inputs.SkillsMode == sandboxworkload.SkillsModeEmpty &&
		(!config.NativeExecution || config.bootstrap == nil || len(inputs.PublicationSources) != 0 ||
			sandboxSkillsMode(config.NativeExecution, agent, attempt.team) != inputs.SkillsMode) {
		return fmt.Errorf("empty skills require native execution, an empty effective selection, no shared skill storage and no recorded publication")
	}
	home := &corev1.PersistentVolumeClaim{}
	var skills *corev1.PersistentVolumeClaim
	claims := []struct {
		expected sandboxworkload.ObjectIdentity
		object   *corev1.PersistentVolumeClaim
	}{{inputs.Home, home}}
	if inputs.SkillsMode != sandboxworkload.SkillsModeEmpty {
		skills = &corev1.PersistentVolumeClaim{}
		claims = append(claims, struct {
			expected sandboxworkload.ObjectIdentity
			object   *corev1.PersistentVolumeClaim
		}{inputs.Skills, skills})
	}
	for _, claim := range claims {
		if claim.expected.Name == "" || claim.expected.UID == "" {
			return fmt.Errorf("Sandbox HOME and skills require explicit persisted identities")
		}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: claim.expected.Name}, claim.object); err != nil {
			return err
		}
		if claim.object.UID != claim.expected.UID || !claim.object.DeletionTimestamp.IsZero() {
			return fmt.Errorf("Sandbox storage UID changed or storage is terminating")
		}
	}
	secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: inputs.RuntimeSecret.Name}, secret); err != nil {
		return err
	}
	if secret.UID != inputs.RuntimeSecret.UID || !secret.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Sandbox runtime Secret approval no longer matches")
	}
	runtimeClass := &nodev1.RuntimeClass{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: attempt.resolution.Capabilities.RuntimeClassName}, runtimeClass); err != nil {
		return err
	}
	registered := attempt.configuration.RuntimeClasses[runtimeClass.Name]
	if registered.UID != runtimeClass.UID || registered.Handler != runtimeClass.Handler || !runtimeClass.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Sandbox RuntimeClass approval changed")
	}
	storageClass := &storagev1.StorageClass{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: attempt.resolution.Sandbox.Storage.Home.StorageClassName}, storageClass); err != nil {
		return err
	}
	storage := attempt.configuration.StorageClasses[storageClass.Name]
	if storage.UID != storageClass.UID || storage.Provisioner != storageClass.Provisioner || !storageClass.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Sandbox StorageClass approval changed")
	}
	attempt.input = sandboxworkload.Input{
		Reconciled: attempt.reconciled, Suspended: attempt.reconciled && agent.Spec.Suspended,
		Resolution: attempt.resolution, AgentName: agent.Name, AgentGeneration: agent.Generation,
		AgentType: string(agent.Spec.Agent.Type), WorkloadName: instanceServiceName(agent.Name), ReleaseName: helmReleaseName(agent.Name),
		Namespace: sandboxworkload.ObjectIdentity{Name: namespace.Name, UID: namespace.UID},
		Home:      home, Skills: skills, SkillsMode: inputs.SkillsMode, RuntimeClass: runtimeClass,
		Helpers: inputs.Helpers, AuthorizedSecrets: []string{inputs.RuntimeSecret.Name},
	}
	if r.SkillMarketplace && attempt.reconciled && inputs.SkillsMode != sandboxworkload.SkillsModeEmpty {
		// The claim holds every Team's subtree. Select the bound Team's and
		// the release of its current selection; the Team is a pinned source.
		attempt.input.SkillMarketplace = &sandboxworkload.SkillMarketplaceInput{
			TeamPath:        api.SkillMarketplaceTeamPath(string(attempt.resolution.Binding.TeamUID)),
			SelectionDigest: api.SkillMarketplaceSelectionDigest(attempt.team.Spec.Skills),
		}
	}
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		if err := r.configureCustomSandboxInput(ctx, attempt); err != nil {
			return err
		}
	}
	if state := agent.Status.PlannedMigration; state.CanStart() {
		attempt.input.PlannedMigration = &state.PlannedMigrationRequest
	}
	attempt.sources = append(attempt.sources,
		sandboxSource(namespace, "v1", "Namespace"),
		sandboxSource(home, "v1", "PersistentVolumeClaim"),
	)
	if skills != nil {
		attempt.sources = append(attempt.sources, sandboxSource(skills, "v1", "PersistentVolumeClaim"))
	}
	attempt.sources = append(attempt.sources,
		sandboxSource(secret, "v1", "Secret"),
		sandboxSource(runtimeClass, "node.k8s.io/v1", "RuntimeClass"),
		sandboxSource(storageClass, "storage.k8s.io/v1", "StorageClass"),
	)
	attempt.sources = append(attempt.sources, inputs.PublicationSources...)
	if config.NativeExecution {
		if !sandboxGatewayProjectionReady(agent) && !(attempt.reconciled && sandboxGatewayProjectionReadyForLifecycle(agent)) {
			return fmt.Errorf("%w: current Gateway projection is required for native execution", sandboxpolicy.ErrUnavailable)
		}
		key := &corev1.Secret{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: aiGatewayKeySecretName}, key); err != nil {
			return fmt.Errorf("read Sandbox AI Gateway key projection: %w", err)
		}
		if key.UID == "" || key.ResourceVersion == "" || !key.DeletionTimestamp.IsZero() ||
			!hasAgentOwner(key, agent) || key.Labels[aiGatewayAgentUIDLabel] != string(agent.UID) ||
			!aigateway.ValidKeyValue(string(key.Data[aiGatewayKeySecretField])) {
			return fmt.Errorf("Sandbox AI Gateway key projection is not owned and ready")
		}
		attempt.input.AuthorizedSecrets = append(attempt.input.AuthorizedSecrets, key.Name)
		attempt.input.GatewayKey = &sandboxworkload.ObjectIdentity{Name: key.Name, UID: key.UID}
		attempt.sources = append(attempt.sources, sandboxSource(key, "v1", "Secret"))
	}
	// Recheck captured versions before applying grants/workloads or accepting
	// read-only reuse; durable bookkeeping can precede those checks.
	return nil
}

func sandboxSource(object client.Object, version, kind string) sandboxworkload.SourceReference {
	return sandboxworkload.SourceReference{
		APIVersion: version, Kind: kind, Namespace: object.GetNamespace(), Name: object.GetName(),
		UID: object.GetUID(), ResourceVersion: object.GetResourceVersion(),
	}
}

func sandboxSourceKey(source sandboxworkload.SourceReference) string {
	return source.APIVersion + "/" + source.Kind + "/" + source.Namespace + "/" + source.Name
}

func (r *AgentReconciler) readSandboxSource(ctx context.Context, source sandboxworkload.SourceReference) (sandboxworkload.SourceReference, error) {
	if source.IsAgent() {
		agent := &api.Agent{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: source.Name}, agent); err != nil {
			return source, err
		}
		current, err := sandboxworkload.AgentSourceReference(agent)
		if err != nil {
			return source, err
		}
		if source.ResourceVersion != "" || (source.UID != "" && source.UID != current.UID) ||
			(source.Generation != 0 && source.Generation != current.Generation) ||
			(source.IntentDigest != "" && source.IntentDigest != current.IntentDigest) {
			return source, fmt.Errorf("%w: %s", errSandboxSourceChanged, sandboxSourceKey(source))
		}
		return current, nil
	}
	object := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: source.APIVersion, Kind: source.Kind}}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, object); err != nil {
		return source, err
	}
	if object.UID == "" || object.ResourceVersion == "" || !object.DeletionTimestamp.IsZero() {
		return source, fmt.Errorf("%w: %s is not persisted or is terminating", errSandboxSourceChanged, sandboxSourceKey(source))
	}
	if (source.UID != "" && object.UID != source.UID) ||
		(source.ResourceVersion != "" && object.ResourceVersion != source.ResourceVersion) {
		return source, fmt.Errorf("%w: %s", errSandboxSourceChanged, sandboxSourceKey(source))
	}
	return sandboxSource(object, source.APIVersion, source.Kind), nil
}

func validateSandboxSourceInventory(sources []sandboxworkload.SourceReference) error {
	if len(sources) == 0 || len(sources) > sandboxworkload.MaxPreparationSources {
		return fmt.Errorf("Sandbox source inventory exceeds the supported bound")
	}
	seen := map[string]bool{}
	for _, source := range sources {
		key := sandboxSourceKey(source)
		if !source.ValidIdentity() || seen[key] {
			return fmt.Errorf("Sandbox source identities must be complete and unique")
		}
		seen[key] = true
	}
	return nil
}

func (r *AgentReconciler) verifySandboxSources(ctx context.Context, sources []sandboxworkload.SourceReference) error {
	if err := validateSandboxSourceInventory(sources); err != nil {
		return err
	}
	for _, source := range sources {
		if _, err := r.readSandboxSource(ctx, source); err != nil {
			return err
		}
	}
	return nil
}

func sameReconciledSandboxTeamInputs(expected, current *api.Team) bool {
	if expected == nil || current == nil || expected.UID == "" || expected.ResourceVersion == "" ||
		!expected.DeletionTimestamp.IsZero() || current.UID == "" || current.ResourceVersion == "" ||
		!current.DeletionTimestamp.IsZero() {
		return false
	}
	return equality.Semantic.DeepEqual(normalizedTeamRuntimeInputs(expected), normalizedTeamRuntimeInputs(current))
}

// verifyReconciledSandboxTeamSource re-reads the Team used by common native
// reconciliation. Compatibility is diagnostic status and changes its resource
// version, so this boundary compares the complete meaningful Team object rather
// than treating that bookkeeping update as a new authorization input.
func (r *AgentReconciler) verifyReconciledSandboxTeamSource(ctx context.Context, source sandboxworkload.SourceReference, expected *api.Team) error {
	if expected == nil || source.APIVersion != api.GroupVersion.String() || source.Kind != "Team" ||
		source != sandboxSource(expected, api.GroupVersion.String(), "Team") {
		return fmt.Errorf("%w: reconciled Sandbox Team source identity changed", errSandboxSourceChanged)
	}
	current := &api.Team{}
	key := client.ObjectKey{Namespace: source.Namespace, Name: source.Name}
	if err := r.APIReader.Get(ctx, key, current); err != nil {
		return err
	}
	if client.ObjectKeyFromObject(current) != key || !sameReconciledSandboxTeamInputs(expected, current) {
		return fmt.Errorf("%w: reconciled Sandbox Team source inputs changed", errSandboxSourceChanged)
	}
	return nil
}

func (r *AgentReconciler) publishSandboxExpectation(ctx context.Context, attempt *sandboxPreparationAttempt,
	bundle *sandboxworkload.Bundle, template *corev1.PodTemplateSpec, role *rbacv1.Role, binding *rbacv1.RoleBinding,
	bindings []sandboxpolicy.BindingReference, stageDigest string,
	check func(context.Context) error,
) error {
	workload := &appsv1.StatefulSet{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: attempt.input.Namespace.Name, Name: attempt.input.WorkloadName}, workload); err != nil {
		return err
	}
	owner := []metav1.OwnerReference{sandboxworkload.AgentOwner(attempt.agent.Name, attempt.agent.UID)}
	if !workload.DeletionTimestamp.IsZero() || !equality.Semantic.DeepEqual(workload.OwnerReferences, owner) ||
		ptr.Deref(workload.Spec.Replicas, -1) != 0 || len(workload.Spec.VolumeClaimTemplates) != 0 ||
		workload.Annotations[sandboxworkload.AgentUIDAnnotation] != string(attempt.agent.UID) ||
		workload.Annotations[sandboxworkload.RevisionAnnotation] != attempt.plan.Revision ||
		!sandboxworkload.SamePodTemplate(workload.Spec.Template, *template) {
		return fmt.Errorf("Sandbox staged workload is not the exact owned, held template")
	}
	var placementSources []sandboxworkload.SourceReference
	if r.SandboxPreparation.Placement != nil {
		if err := check(ctx); err != nil {
			return err
		}
		var err error
		placementSources, err = r.captureSandboxPlacement(ctx, attempt, workload, template)
		if err != nil {
			return err
		}
		// Initial enrollment changes durable prerequisites, not the execution
		// hold. Subsequent write guards must cover the newly recorded tuple.
		check = r.sandboxPreparationGuard(attempt, bindings)
	}
	candidates := slices.Clone(attempt.sources)
	candidates = append(candidates, placementSources...)
	journal, err := r.sandboxGrantJournal().ReadSnapshot(ctx, attempt.agent.Name, attempt.agent.UID)
	if err != nil {
		return err
	}
	recorded, err := sandboxConfirmedBindingInventory(journal.Bindings)
	if err != nil {
		return err
	}
	confirmed, err := sandboxConfirmedBindingInventory(bindings)
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(recorded, confirmed) {
		return fmt.Errorf("Sandbox grant inventory changed before expectation publication")
	}
	candidates = append(candidates, sandboxworkload.SourceReference{
		APIVersion: "v1", Kind: "ConfigMap", Namespace: r.OperatorNamespace,
		Name: sandboxpolicy.GrantJournalName(attempt.agent.UID), UID: journal.UID, ResourceVersion: journal.ResourceVersion,
	})
	controllerRevision := ""
	if workload.Status.UpdateRevision != "" && workload.Status.ObservedGeneration == workload.Generation {
		revision, err := r.readSandboxPreparedRevision(ctx, workload, *template)
		if err != nil {
			return err
		}
		controllerRevision = revision.Name
		candidates = append(candidates, sandboxSource(revision, appsv1.SchemeGroupVersion.String(), "ControllerRevision"))
	}
	agentSource, err := sandboxworkload.AgentSourceReference(attempt.agent)
	if err != nil {
		return err
	}
	candidates = append(candidates,
		agentSource,
		sandboxSource(workload, "apps/v1", "StatefulSet"),
		sandboxworkload.SourceReference{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role", Namespace: role.Namespace, Name: role.Name},
		sandboxworkload.SourceReference{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding", Namespace: binding.Namespace, Name: binding.Name},
	)
	candidates = append(candidates, sandboxworkload.PodSourceReferences(workload.Spec.Template.Spec, workload.Namespace)...)
	for _, object := range attempt.plan.Resources {
		candidates = append(candidates, sandboxworkload.SourceReference{
			APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName(),
		})
	}
	for _, grant := range attempt.configuration.Platforms[attempt.resolution.Sandbox.Type].Grants {
		if grant.PolicyUID == attempt.resolution.Binding.AgentPolicyUID {
			candidates = append(candidates, sandboxworkload.SourceReference{
				APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole", Name: grant.RoleRef.Name,
				UID: grant.RoleUID, ResourceVersion: grant.RoleResourceVersion,
			})
		}
	}
	sources := make([]sandboxworkload.SourceReference, 0, len(candidates))
	seen := map[string]sandboxworkload.SourceReference{}
	for _, candidate := range candidates {
		key := sandboxSourceKey(candidate)
		if old, exists := seen[key]; exists {
			if (candidate.UID != "" && candidate.UID != old.UID) ||
				(candidate.ResourceVersion != "" && candidate.ResourceVersion != old.ResourceVersion) ||
				(candidate.Generation != 0 && candidate.Generation != old.Generation) ||
				(candidate.IntentDigest != "" && candidate.IntentDigest != old.IntentDigest) {
				return fmt.Errorf("Sandbox source inventory contains conflicting snapshots")
			}
			continue
		}
		source, err := r.readSandboxSource(ctx, candidate)
		if err != nil {
			return err
		}
		seen[key] = source
		sources = append(sources, source)
	}
	if len(sources) < 9 || len(sources) > sandboxworkload.MaxPreparationSources {
		return fmt.Errorf("Sandbox Pod source inventory must contain 9 to %d identities", sandboxworkload.MaxPreparationSources)
	}
	slices.SortFunc(sources, func(a, b sandboxworkload.SourceReference) int {
		if sandboxSourceKey(a) < sandboxSourceKey(b) {
			return -1
		}
		if sandboxSourceKey(a) > sandboxSourceKey(b) {
			return 1
		}
		return 0
	})
	// The live template was compared with supported API defaults above. Keep
	// the sealed template as the protected contract, not API-added defaults.
	expected, err := bundle.Expect(*template, workload, sources)
	if err != nil {
		return err
	}
	expected.ControllerRevision = controllerRevision
	expected.Placement = attempt.prerequisites.Placement.DeepCopy()
	object, err := sandboxworkload.PodExpectationObject(r.OperatorNamespace, expected)
	if err != nil {
		return err
	}
	if len(object.Data[sandboxworkload.PodExpectationKey]) > 512*1024 {
		return fmt.Errorf("Sandbox Pod expectation exceeds the admission size bound")
	}
	digest, err := r.sandboxPreparationReuseDigest(ctx, attempt, expected, bindings, stageDigest)
	if err != nil {
		return err
	}
	object.Annotations[sandboxPreparationDigestAnnotation] = digest
	if err := check(ctx); err != nil {
		return err
	}
	if err := r.verifySandboxSources(ctx, sources); err != nil {
		return err
	}
	current := &corev1.ConfigMap{}
	err = r.APIReader.Get(ctx, client.ObjectKeyFromObject(object), current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, object); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if !current.DeletionTimestamp.IsZero() || !equality.Semantic.DeepEqual(current.OwnerReferences, owner) ||
			current.Annotations[sandboxworkload.AgentUIDAnnotation] != string(attempt.agent.UID) {
			return fmt.Errorf("Sandbox Pod expectation has foreign or terminating ownership")
		}
		if !equality.Semantic.DeepEqual(current.Data, object.Data) ||
			current.Annotations[sandboxworkload.RevisionAnnotation] != expected.Revision ||
			current.Annotations[sandboxPreparationDigestAnnotation] != digest || len(current.BinaryData) != 0 {
			base := current.DeepCopy()
			current.Data, current.BinaryData = object.Data, nil
			current.Annotations[sandboxworkload.RevisionAnnotation] = expected.Revision
			current.Annotations[sandboxPreparationDigestAnnotation] = digest
			if err := r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	}
	// An expectation remains held even if a source races this last check.
	// Report the race and retry rather than claiming preparation succeeded.
	if err := check(ctx); err != nil {
		return err
	}
	return r.verifySandboxSources(ctx, sources)
}

func (r *AgentReconciler) readSandboxPreparedRevision(ctx context.Context, workload *appsv1.StatefulSet,
	template corev1.PodTemplateSpec,
) (*appsv1.ControllerRevision, error) {
	revision := &appsv1.ControllerRevision{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Status.UpdateRevision}, revision); err != nil {
		return nil, fmt.Errorf("read held Sandbox ControllerRevision: %w", err)
	}
	owner := metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	if revision.UID == "" || revision.ResourceVersion == "" || !revision.DeletionTimestamp.IsZero() ||
		!equality.Semantic.DeepEqual(revision.OwnerReferences, []metav1.OwnerReference{*owner}) ||
		revision.Revision < 1 || len(revision.Data.Raw) == 0 || len(revision.Data.Raw) > 512*1024 {
		return nil, fmt.Errorf("held Sandbox ControllerRevision ownership or data is unavailable")
	}
	// Preserve the controller's template-level $patch: replace directive
	// without inheriting fields from the live workload.
	raw, err := strategicpatch.StrategicMergePatch([]byte(`{"spec":{"template":{}}}`), revision.Data.Raw, appsv1.StatefulSet{})
	if err != nil {
		return nil, fmt.Errorf("decode held Sandbox ControllerRevision patch: %w", err)
	}
	var restored appsv1.StatefulSet
	if err := json.Unmarshal(raw, &restored); err != nil {
		return nil, fmt.Errorf("decode held Sandbox ControllerRevision workload: %w", err)
	}
	if !sandboxworkload.SamePodTemplate(restored.Spec.Template, template) {
		return nil, fmt.Errorf("held Sandbox ControllerRevision does not describe the prepared template")
	}
	return revision, nil
}
