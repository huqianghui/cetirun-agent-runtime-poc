// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	strictjson "sigs.k8s.io/json"
)

const (
	sandboxBootstrapReceiptKey = "bootstrap.json"
	sandboxBootstrapAnnotation = "cetirun.microsoft.com/sandbox-bootstrap"
)

type sandboxBootstrapConfig struct {
	HomeClaimName              string
	ChartVersion               string
	SkillsClaimName            string
	SkillsCatalogConfigMapName string
}

type sandboxBootstrapObject struct {
	Kind string    `json:"kind"`
	Name string    `json:"name"`
	UID  types.UID `json:"uid,omitempty"`
}

// The annotation distinguishes newly selected empty storage from legacy v1/v3
// receipts whose externally supplied skills claim must remain authoritative.
const sandboxBootstrapSkillsModeAnnotation = "cetirun.microsoft.com/bootstrap-skills-mode"

type sandboxBootstrapReceipt struct {
	Reconciled     bool                      `json:"reconciled,omitempty"`
	Version        string                    `json:"version"`
	AgentName      string                    `json:"agentName"`
	AgentUID       types.UID                 `json:"agentUID"`
	AgentType      api.AgentType             `json:"agentType"`
	Namespace      string                    `json:"namespace"`
	Home           runtimebindings.HomeClaim `json:"home"`
	ChartVersion   string                    `json:"chartVersion"`
	Objects        []sandboxBootstrapObject  `json:"objects"`
	Skills         *sandboxBootstrapSkills   `json:"skills,omitempty"`
	ServiceAccount *sandboxBootstrapObject   `json:"serviceAccount,omitempty"`
}

func (receipt *sandboxBootstrapReceipt) inventory() []*sandboxBootstrapObject {
	items := make([]*sandboxBootstrapObject, 0, len(receipt.Objects)+1)
	for i := range receipt.Objects {
		items = append(items, &receipt.Objects[i])
	}
	if receipt.ServiceAccount != nil {
		items = append(items, receipt.ServiceAccount)
	}
	return items
}

func (r *AgentReconciler) sandboxBootstrapKey(agent *api.Agent) client.ObjectKey {
	sum := sha256.Sum256([]byte(agent.UID))
	return client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-bootstrap-" + hex.EncodeToString(sum[:20])}
}

func (r *AgentReconciler) readSandboxBootstrap(ctx context.Context, agent *api.Agent) (*corev1.ConfigMap, *sandboxBootstrapReceipt, error) {
	object := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, r.sandboxBootstrapKey(agent), object); err != nil {
		return nil, nil, err
	}
	raw := object.Data[sandboxBootstrapReceiptKey]
	if len(raw) > 16*1024 || len(object.Data) != 1 || len(object.BinaryData) != 0 {
		return nil, nil, fmt.Errorf("Sandbox bootstrap receipt is invalid")
	}
	var receipt sandboxBootstrapReceipt
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &receipt)
	owner := []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)}
	if err != nil || len(problems) != 0 || object.UID == "" || object.ResourceVersion == "" ||
		!equality.Semantic.DeepEqual(object.OwnerReferences, owner) ||
		object.Annotations[sandboxworkload.AgentUIDAnnotation] != string(agent.UID) ||
		receipt.AgentName != agent.Name || receipt.AgentUID != agent.UID ||
		(receipt.AgentType != api.AgentTypeOpenClaw && receipt.AgentType != api.AgentTypeHermes && receipt.AgentType != api.AgentTypeCustom) ||
		(receipt.AgentType == api.AgentTypeCustom && !receipt.Reconciled) ||
		receipt.Namespace != instanceNamespace(agent.Name) || receipt.Namespace == r.OperatorNamespace ||
		len(validation.IsDNS1123Label(receipt.Namespace)) != 0 || receipt.ChartVersion == "" ||
		!((receipt.Version == "v1" && receipt.ServiceAccount == nil && receipt.Skills == nil && len(receipt.Objects) == 5) ||
			(receipt.Version == "v2" && receipt.ServiceAccount == nil && receipt.Skills != nil && len(receipt.Objects) == 7) ||
			(receipt.Version == "v3" && receipt.Reconciled && receipt.ServiceAccount != nil && receipt.Skills == nil && len(receipt.Objects) == 5) ||
			(receipt.Version == "v4" && receipt.Reconciled && receipt.ServiceAccount != nil && receipt.Skills != nil && len(receipt.Objects) == 7)) {
		return nil, nil, fmt.Errorf("Sandbox bootstrap ownership receipt is invalid")
	}
	if mode := object.Annotations[sandboxBootstrapSkillsModeAnnotation]; mode != "" &&
		(mode != string(sandboxworkload.SkillsModeEmpty) || receipt.Skills != nil) {
		return nil, nil, fmt.Errorf("Sandbox bootstrap skills mode is invalid")
	}
	size, err := resource.ParseQuantity(receipt.Home.StorageSize)
	if err != nil || size.Sign() <= 0 || len(validation.IsDNS1123Subdomain(receipt.Home.StorageClass)) != 0 ||
		(receipt.Home.AccessMode != string(corev1.ReadWriteOnce) && receipt.Home.AccessMode != string(corev1.ReadWriteOncePod)) {
		return nil, nil, fmt.Errorf("Sandbox bootstrap HOME receipt is invalid")
	}
	kinds := []string{"Namespace", "Role", "RoleBinding", "PersistentVolumeClaim", "Secret"}
	if receipt.Skills != nil {
		selected := receipt.Skills
		if !selected.Catalog.ValidIdentity() || selected.Catalog.APIVersion != "v1" || selected.Catalog.Kind != "ConfigMap" ||
			selected.Catalog.Namespace != r.OperatorNamespace || len(validation.IsDNS1123Subdomain(selected.TeamName)) != 0 ||
			receipt.Objects[5].Name == receipt.Objects[3].Name || receipt.Objects[6].Name != sandboxSkillsVolumeName(agent.UID) {
			return nil, nil, fmt.Errorf("Sandbox skills publication receipt is invalid")
		}
		if err := validateSandboxSkillsPublication(selected.Publication); err != nil {
			return nil, nil, err
		}
		kinds = append(kinds, "PersistentVolumeClaim", "PersistentVolume")
	}
	for i, entry := range receipt.Objects {
		if entry.Kind != kinds[i] || len(validation.IsDNS1123Subdomain(entry.Name)) != 0 {
			return nil, nil, fmt.Errorf("Sandbox bootstrap resource inventory is invalid")
		}
		if account := receipt.ServiceAccount; account != nil && (account.Kind != "ServiceAccount" ||
			account.Name == "default" || len(validation.IsDNS1123Subdomain(account.Name)) != 0) {
			return nil, nil, fmt.Errorf("Sandbox bootstrap ServiceAccount inventory is invalid")
		}
	}
	role, binding := r.operatorNamespaceRBAC(receipt.Namespace)
	if receipt.Objects[0].Name != receipt.Namespace || receipt.Objects[1].Name != role.Name || receipt.Objects[2].Name != binding.Name {
		return nil, nil, fmt.Errorf("Sandbox bootstrap resource inventory is invalid")
	}
	return object, &receipt, nil
}

// validateSandboxBootstrapNamespace proves that a namespace is the exact
// protected object recorded for this Agent before any credential is projected
// into it. Names are not identities: the receipt and current namespace UID
// must both survive a read-back check.
func (r *AgentReconciler) validateSandboxBootstrapNamespace(ctx context.Context, agent *api.Agent, namespace *corev1.Namespace) error {
	object, receipt, err := r.readSandboxBootstrap(ctx, agent)
	if err != nil {
		return fmt.Errorf("read Sandbox bootstrap ownership receipt: %w", err)
	}
	if receipt.Objects[0].UID == "" {
		return fmt.Errorf("Sandbox bootstrap namespace identity is not yet recorded")
	}
	desired := r.sandboxBootstrapObjects(agent, object, receipt)[0]
	if err := validateSandboxBootstrapObject(namespace, desired, receipt.Objects[0].UID, false); err != nil {
		return fmt.Errorf("validate Sandbox bootstrap namespace: %w", err)
	}
	return nil
}

func (r *AgentReconciler) saveSandboxBootstrap(ctx context.Context, object *corev1.ConfigMap, receipt *sandboxBootstrapReceipt) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	object.Data[sandboxBootstrapReceiptKey] = string(raw)
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": object.UID, "resourceVersion": object.ResourceVersion},
		"data":     object.Data,
	})
	if err != nil {
		return err
	}
	return r.Patch(ctx, object, client.RawPatch(types.MergePatchType, patch))
}

// verifySuspendedReconciledBootstrap reads every recorded object without
// repairing it. A suspended Agent must never adopt a same-name replacement or
// recreate a partial bootstrap lifetime before the normal stop path completes.
func (r *AgentReconciler) verifySuspendedReconciledBootstrap(ctx context.Context, agent *api.Agent) error {
	object, receipt, err := r.readSandboxBootstrap(ctx, agent)
	if err != nil {
		return fmt.Errorf("read suspended Sandbox bootstrap ownership receipt: %w", err)
	}
	desired, inventory := r.sandboxBootstrapObjects(agent, object, receipt), receipt.inventory()
	if len(desired) != len(inventory) || slices.ContainsFunc(inventory, func(entry *sandboxBootstrapObject) bool { return entry.UID == "" }) {
		return fmt.Errorf("suspended Sandbox bootstrap ownership receipt is incomplete")
	}
	for index, expected := range desired {
		current := expected.DeepCopyObject().(client.Object)
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
			return fmt.Errorf("read suspended Sandbox bootstrap %s: %w", inventory[index].Kind, err)
		}
		if err := validateSandboxBootstrapObject(current, expected, inventory[index].UID, false); err != nil {
			return fmt.Errorf("validate suspended Sandbox bootstrap %s: %w", inventory[index].Kind, err)
		}
	}
	return nil
}

// verifySuspendedReconciledLifetime admits the independent suspension path
// only when the protected runtime namespace is the exact object recorded for
// this Agent.  It intentionally does not require the full bootstrap inventory
// to match: stopSandboxBuiltinWorkload separately proves workload ownership
// before changing replicas, while a Role or other auxiliary drift must not
// keep an already-owned workload or Gateway key live.  The normal suspended
// bootstrap verification still requires the complete inventory before it can
// report suspension as fully verified.
func (r *AgentReconciler) verifySuspendedReconciledLifetime(ctx context.Context, agent *api.Agent) error {
	_, receipt, err := r.readSandboxBootstrap(ctx, agent)
	if err != nil {
		return fmt.Errorf("read suspended Sandbox bootstrap ownership receipt: %w", err)
	}
	if !receipt.Reconciled || receipt.Objects[0].UID == "" {
		return fmt.Errorf("suspended Sandbox namespace identity is incomplete")
	}
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: receipt.Namespace}, namespace); err != nil {
		return fmt.Errorf("read suspended Sandbox namespace: %w", err)
	}
	if err := r.validateSandboxBootstrapNamespace(ctx, agent, namespace); err != nil {
		return err
	}
	return nil
}

func (r *AgentReconciler) bootstrapSandboxOwned(ctx context.Context, attempt *sandboxPreparationAttempt) error {
	agent, config := attempt.agent, r.SandboxPreparation.bootstrap
	accountName, err := r.bootstrapIdentityAccountName(attempt)
	if err != nil {
		return err
	}
	object, receipt, err := r.readSandboxBootstrap(ctx, agent)
	if apierrors.IsNotFound(err) {
		if controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) {
			return fmt.Errorf("Sandbox bootstrap receipt is missing; recovery is required")
		}
		ns := &corev1.Namespace{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, ns); err == nil {
			if ns.Annotations[sandboxBootstrapAnnotation] != "" {
				return fmt.Errorf("Sandbox namespace has bootstrap intent but its receipt is missing; recovery is required")
			}
			// Pre-existing inputs stay on the read-only path; this is not adoption.
			return nil
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		if controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) || agent.Status.Sandbox != nil {
			return fmt.Errorf("%w: a previously prepared namespace cannot be bootstrapped again", sandboxworkload.ErrStorageTransitionRequired)
		}
	} else if err != nil {
		return err
	}
	if (!attempt.reconciled && agent.Spec.Helm.Version != config.ChartVersion) || agent.Spec.Helm.Version == "" || agent.Spec.Agent.Image == nil || *agent.Spec.Agent.Image == "" ||
		(agent.Spec.Agent.Type != api.AgentTypeCustom && agent.Spec.Agent.Port == nil) ||
		(agent.Spec.Agent.Port != nil && (*agent.Spec.Agent.Port < 1 || *agent.Spec.Agent.Port > 65535)) {
		return fmt.Errorf("Sandbox bootstrap requires explicit approved chart version, native image and port; controller spec defaulting is not authorized")
	}
	if len(validation.IsDNS1123Label(instanceNamespace(agent.Name))) != 0 || instanceNamespace(agent.Name) == r.OperatorNamespace ||
		len(validation.IsDNS1123Subdomain(sandboxRuntimeSecretName(agent))) != 0 ||
		len(validation.IsDNS1123Subdomain(r.OperatorSAName)) != 0 {
		return fmt.Errorf("Sandbox bootstrap requires valid isolated namespace, runtime Secret and operator identities")
	}
	home := attempt.resolution.Sandbox.Storage.Home
	profile := runtimebindings.HomeClaim{StorageClass: home.StorageClassName, StorageSize: home.Size.String(), AccessMode: string(home.AccessMode)}
	if receipt == nil {
		role, binding := r.operatorNamespaceRBAC(instanceNamespace(agent.Name))
		receipt = &sandboxBootstrapReceipt{
			Reconciled: attempt.reconciled,
			Version:    "v1", AgentName: agent.Name, AgentUID: agent.UID, AgentType: agent.Spec.Agent.Type, Namespace: instanceNamespace(agent.Name),
			Home: profile, ChartVersion: agent.Spec.Helm.Version,
			Objects: []sandboxBootstrapObject{
				{Kind: "Namespace", Name: instanceNamespace(agent.Name)},
				{Kind: "Role", Name: role.Name}, {Kind: "RoleBinding", Name: binding.Name},
				{Kind: "PersistentVolumeClaim", Name: config.HomeClaimName},
				{Kind: "Secret", Name: sandboxRuntimeSecretName(agent)},
			},
		}
		if config.SkillsCatalogConfigMapName != "" &&
			sandboxSkillsMode(r.SandboxPreparation.NativeExecution, agent, attempt.team) != sandboxworkload.SkillsModeEmpty {
			publication, err := r.readSandboxSkillsPublication(ctx, config.SkillsCatalogConfigMapName, attempt.team)
			if err != nil {
				return err
			}
			receipt.Version, receipt.Skills = "v2", publication
			receipt.Objects = append(receipt.Objects,
				sandboxBootstrapObject{Kind: "PersistentVolumeClaim", Name: config.SkillsClaimName},
				sandboxBootstrapObject{Kind: "PersistentVolume", Name: sandboxSkillsVolumeName(agent.UID)})
			attempt.sources = append(attempt.sources, publication.Catalog)
		}
		if accountName != "" {
			receipt.ServiceAccount = &sandboxBootstrapObject{Kind: "ServiceAccount", Name: accountName}
			receipt.Version = "v3"
			if receipt.Skills != nil {
				receipt.Version = "v4"
			}
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		key := r.sandboxBootstrapKey(agent)
		object = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
				OwnerReferences: []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)},
				Annotations:     map[string]string{sandboxworkload.AgentUIDAnnotation: string(agent.UID)},
				Finalizers:      []string{sandbox.BootstrapFinalizer},
			},
			Data: map[string]string{sandboxBootstrapReceiptKey: string(raw)},
		}
		if receipt.Skills == nil && sandboxSkillsMode(r.SandboxPreparation.NativeExecution, agent, attempt.team) == sandboxworkload.SkillsModeEmpty {
			object.Annotations[sandboxBootstrapSkillsModeAnnotation] = string(sandboxworkload.SkillsModeEmpty)
		}
		if err := r.sandboxBootstrapGuard(ctx, attempt); err != nil {
			return err
		}
		if err := r.Create(ctx, object); err != nil {
			return err
		}
	}
	if !object.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(object, sandbox.BootstrapFinalizer) {
		return fmt.Errorf("Sandbox bootstrap receipt is no longer protected for writes")
	}
	if receipt.Home != profile || receipt.Objects[3].Name != config.HomeClaimName ||
		receipt.Objects[4].Name != sandboxRuntimeSecretName(agent) || (!receipt.Reconciled && receipt.ChartVersion != config.ChartVersion) ||
		receipt.AgentType != agent.Spec.Agent.Type || receipt.Reconciled != attempt.reconciled {
		return fmt.Errorf("%w: changing bootstrap storage, native type, Secret or chart requires an explicit handoff", sandboxworkload.ErrStorageTransitionRequired)
	}
	if accountName != "" && receipt.ServiceAccount != nil && receipt.ServiceAccount.Name != accountName {
		return fmt.Errorf("%w: the recorded ServiceAccount name is immutable for this Agent", sandboxworkload.ErrStorageTransitionRequired)
	}
	if accountName != "" && receipt.ServiceAccount == nil &&
		!slices.ContainsFunc(receipt.Objects, func(entry sandboxBootstrapObject) bool { return entry.UID == "" }) {
		quiescence := &KubernetesNativeQuiescence{APIReader: r.customReader(), Reconciled: true}
		if err := quiescence.CheckQuiesced(ctx, agent, SandboxPrerequisites{
			AgentUID: agent.UID, AgentGeneration: agent.Generation, NamespaceUID: receipt.Objects[0].UID,
			Home: sandboxworkload.ObjectIdentity{Name: receipt.Objects[3].Name, UID: receipt.Objects[3].UID},
		}); err != nil {
			return err
		}
		if err := r.sandboxBootstrapGuard(ctx, attempt); err != nil {
			return err
		}
		receipt.ServiceAccount = &sandboxBootstrapObject{Kind: "ServiceAccount", Name: accountName}
		receipt.Version = "v3"
		if receipt.Skills != nil {
			receipt.Version = "v4"
		}
		if err := r.saveSandboxBootstrap(ctx, object, receipt); err != nil {
			return err
		}
	}
	if receipt.Skills != nil {
		if config.SkillsCatalogConfigMapName != receipt.Skills.Catalog.Name || config.SkillsClaimName != receipt.Objects[5].Name {
			return fmt.Errorf("%w: recorded skills catalog or claim selection changed; recovery is required", sandboxworkload.ErrStorageTransitionRequired)
		}
		publication, err := r.readSandboxSkillsPublication(ctx, config.SkillsCatalogConfigMapName, attempt.team)
		if err != nil {
			return fmt.Errorf("recorded skills publication is unavailable; recovery is required: %w", err)
		}
		if !sameSandboxSkillsPublication(*publication, *receipt.Skills) {
			return fmt.Errorf("%w: recorded skills publication changed; recovery is required", sandboxworkload.ErrStorageTransitionRequired)
		}
		// New receipts already added the source used before their first write.
		if !slices.ContainsFunc(attempt.sources, func(source sandboxworkload.SourceReference) bool {
			return sandboxSourceKey(source) == sandboxSourceKey(publication.Catalog)
		}) {
			attempt.sources = append(attempt.sources, publication.Catalog)
		}
	}
	if !controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) {
		if err := r.sandboxBootstrapGuard(ctx, attempt); err != nil {
			return err
		}
		base := agent.DeepCopy()
		controllerutil.AddFinalizer(agent, sandbox.BootstrapFinalizer)
		if err := r.Patch(ctx, agent, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	for i, entry := range receipt.inventory() {
		// The static skills PV is created only after its PVC UID is durably recorded.
		desired := r.sandboxBootstrapObjects(agent, object, receipt)
		resource := desired[i]
		if err := r.sandboxBootstrapGuard(ctx, attempt); err != nil {
			return err
		}
		// Once recorded, namespace replacement cannot redirect a subsequent create.
		if i > 0 {
			ns := &corev1.Namespace{}
			if err := r.APIReader.Get(ctx, client.ObjectKey{Name: receipt.Namespace}, ns); err != nil {
				return err
			}
			if err := validateSandboxBootstrapObject(ns, desired[0], receipt.Objects[0].UID, false); err != nil {
				return err
			}
		}
		current := resource.DeepCopyObject().(client.Object)
		err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(resource), current)
		if apierrors.IsNotFound(err) {
			if entry.UID != "" {
				return fmt.Errorf("recorded Sandbox bootstrap %s is missing; recovery is required", entry.Kind)
			}
			if entry.Kind == "Secret" {
				secret, err := r.newGatewayTokenSecret(agent)
				if err != nil {
					return err
				}
				secret.ObjectMeta = *resource.(*metav1.PartialObjectMetadata).ObjectMeta.DeepCopy()
				current = secret
			}
			if err := r.Create(ctx, current); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if role, ok := current.(*rbacv1.Role); ok {
			current, err = r.upgradeSandboxBootstrapRole(ctx, attempt, receipt, desired, role)
			if err != nil {
				return err
			}
		}
		if err := validateSandboxBootstrapObject(current, resource, entry.UID, false); err != nil {
			return err
		}
		if entry.UID == "" {
			if err := r.sandboxBootstrapGuard(ctx, attempt); err != nil {
				return err
			}
			entry.UID = current.GetUID()
			if err := r.saveSandboxBootstrap(ctx, object, receipt); err != nil {
				return err
			}
		}
	}
	return r.sandboxBootstrapGuard(ctx, attempt)
}

func (r *AgentReconciler) upgradeSandboxBootstrapRole(ctx context.Context, attempt *sandboxPreparationAttempt,
	receipt *sandboxBootstrapReceipt, desired []client.Object, current *rbacv1.Role,
) (*rbacv1.Role, error) {
	if len(receipt.Objects) < 3 || len(desired) < 3 ||
		receipt.Objects[0].UID == "" || receipt.Objects[1].UID == "" || receipt.Objects[2].UID == "" {
		return current, nil
	}
	target, ok := desired[1].(*rbacv1.Role)
	if !ok || equality.Semantic.DeepEqual(current.Rules, target.Rules) {
		return current, nil
	}
	legacyRules, upgradedRules := sandboxBootstrapMTLSRoleRules()
	if !equality.Semantic.DeepEqual(current.Rules, legacyRules) || !equality.Semantic.DeepEqual(target.Rules, upgradedRules) {
		return current, nil
	}
	legacy := target.DeepCopy()
	legacy.Rules = legacyRules
	if err := validateSandboxBootstrapObject(current, legacy, receipt.Objects[1].UID, false); err != nil {
		return nil, err
	}
	if err := r.validateBootstrapRoleDependencies(ctx, receipt, desired); err != nil {
		return nil, err
	}
	if err := r.sandboxBootstrapGuard(ctx, attempt); err != nil {
		return nil, err
	}
	before := current.DeepCopy()
	current.Rules = upgradedRules
	if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, fmt.Errorf("upgrade recorded Sandbox bootstrap Role: %w", err)
	}
	observed := &rbacv1.Role{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(target), observed); err != nil {
		return nil, err
	}
	if err := validateSandboxBootstrapObject(observed, target, receipt.Objects[1].UID, false); err != nil {
		return nil, err
	}
	if err := r.validateBootstrapRoleDependencies(ctx, receipt, desired); err != nil {
		return nil, err
	}
	if err := r.sandboxBootstrapGuard(ctx, attempt); err != nil {
		return nil, err
	}
	return observed, nil
}

func (r *AgentReconciler) validateBootstrapRoleDependencies(ctx context.Context, receipt *sandboxBootstrapReceipt, desired []client.Object) error {
	for _, index := range []int{0, 2} {
		current := desired[index].DeepCopyObject().(client.Object)
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(desired[index]), current); err != nil {
			return err
		}
		if err := validateSandboxBootstrapObject(current, desired[index], receipt.Objects[index].UID, false); err != nil {
			return err
		}
	}
	return nil
}

// Pin both sides of the pre-mTLS upgrade; future Role changes need their own migration.
func sandboxBootstrapMTLSRoleRules() ([]rbacv1.PolicyRule, []rbacv1.PolicyRule) {
	legacy := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps", "services", "persistentvolumeclaims", "serviceaccounts"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{"apps"}, Resources: []string{"statefulsets", "deployments"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"roles", "rolebindings"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{"networking.istio.io"}, Resources: []string{"virtualservices", "sidecars"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{"security.istio.io"}, Resources: []string{"authorizationpolicies", "peerauthentications"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	}
	upgraded := (&rbacv1.Role{Rules: legacy}).DeepCopy().Rules
	upgraded[5].Resources = append(upgraded[5].Resources, "destinationrules")
	return legacy, upgraded
}

func (r *AgentReconciler) sandboxBootstrapGuard(ctx context.Context, attempt *sandboxPreparationAttempt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, binding, err := sandboxbinding.Read(ctx, r.APIReader, client.ObjectKeyFromObject(attempt.agent))
	if err != nil {
		return err
	}
	before, err := sandboxworkload.AgentSourceReference(attempt.agent)
	if err != nil {
		return err
	}
	after, err := sandboxworkload.AgentSourceReference(current)
	if err != nil || before != after || binding != attempt.resolution.Binding ||
		!current.DeletionTimestamp.IsZero() ||
		(!attempt.reconciled && (current.Spec.Suspended || !controllerutil.ContainsFinalizer(current, sandbox.HoldFinalizer))) {
		return fmt.Errorf("%w: Sandbox bootstrap intent changed", errSandboxSourceChanged)
	}
	config := r.SandboxPreparation
	if err := configurationSnapshotTime(attempt.configuration.ObservedAt, configurationNow(config.Now), config.MaxSnapshotAge); err != nil {
		return err
	}
	actual, err := configurationSnapshot(ctx, config.Configuration, config.Now, config.MaxSnapshotAge)
	if err != nil {
		return err
	}
	expected := attempt.configuration
	actual.ObservedAt, expected.ObservedAt = time.Time{}, time.Time{}
	actualDigest, err := sandboxPreparationDigest(actual)
	if err != nil {
		return err
	}
	expectedDigest, err := sandboxPreparationDigest(expected)
	if err != nil {
		return err
	}
	if actualDigest != expectedDigest {
		return fmt.Errorf("%w: Sandbox bootstrap platform changed", errSandboxSourceChanged)
	}
	if attempt.reconciled && config.NativeExecution {
		if err := validateSandboxSourceInventory(attempt.sources); err != nil {
			return err
		}
		for _, source := range attempt.sources {
			if source.APIVersion == api.GroupVersion.String() && source.Kind == "AgentPolicy" &&
				source.UID == attempt.resolution.Binding.AgentPolicyUID {
				digest, err := r.executionSourceDigest(ctx, source, current.Spec.SandboxRef, false)
				if err != nil {
					return err
				}
				if !sandboxworkload.ValidInputDigest(attempt.policyDigest) || digest != attempt.policyDigest {
					return fmt.Errorf("%w: Sandbox bootstrap policy inputs changed", errSandboxSourceChanged)
				}
			} else if source.APIVersion == api.GroupVersion.String() && source.Kind == "Team" &&
				source.UID == attempt.resolution.Binding.TeamUID {
				expected, err := sandboxworkload.ExecutionSourceDigest(attempt.team, current.Spec.SandboxRef)
				if err != nil {
					return err
				}
				digest, err := r.executionSourceDigest(ctx, source, current.Spec.SandboxRef, false)
				if err != nil {
					return err
				}
				if digest != expected {
					return fmt.Errorf("%w: Sandbox bootstrap Team inputs changed", errSandboxSourceChanged)
				}
			} else if _, err := r.readSandboxSource(ctx, source); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	return r.verifySandboxSources(ctx, attempt.sources)
}

func (r *AgentReconciler) sandboxBootstrapObjects(agent *api.Agent, receiptObject *corev1.ConfigMap, receipt *sandboxBootstrapReceipt) []client.Object {
	role, binding := r.operatorNamespaceRBAC(receipt.Namespace)
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: receipt.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "cetirun", "app.kubernetes.io/instance": agent.Spec.User.Alias,
				"app.kubernetes.io/managed-by": "cetirun-operator", sandboxworkload.ProtectedNamespaceLabel: "true",
			}}},
		role, binding,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: receipt.Objects[3].Name, Namespace: receipt.Namespace}, Spec: receipt.Home.PVCSpec()},
		&metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: receipt.Objects[4].Name, Namespace: receipt.Namespace}},
	}
	if receipt.Skills != nil {
		objects = append(objects, sandboxSkillsBootstrapObjects(receipt)...)
	}
	if receipt.ServiceAccount != nil {
		objects = append(objects, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: receipt.ServiceAccount.Name, Namespace: receipt.Namespace,
				Labels: map[string]string{sandboxpolicy.AgentUIDLabel: string(agent.UID)}},
			AutomountServiceAccountToken: ptr.To(false),
		})
	}
	for _, object := range objects {
		object.SetFinalizers([]string{sandbox.BootstrapFinalizer})
		object.SetOwnerReferences([]metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)})
		object.SetAnnotations(map[string]string{
			sandboxworkload.AgentUIDAnnotation: string(agent.UID), sandboxworkload.AgentNameAnnotation: agent.Name,
			sandboxBootstrapAnnotation: string(receiptObject.UID),
		})
	}
	return objects
}

func validateSandboxBootstrapObject(current, desired client.Object, uid types.UID, cleanup bool) error {
	if current.GetUID() == "" || current.GetResourceVersion() == "" || (uid != "" && current.GetUID() != uid) ||
		(!cleanup && !current.GetDeletionTimestamp().IsZero()) ||
		!equality.Semantic.DeepEqual(current.GetOwnerReferences(), desired.GetOwnerReferences()) {
		return fmt.Errorf("Sandbox bootstrap resource ownership or UID changed; recovery is required")
	}
	for key, value := range desired.GetAnnotations() {
		if current.GetAnnotations()[key] != value {
			return fmt.Errorf("Sandbox bootstrap resource differs from its recorded creation intent")
		}
		if _, ok := desired.(*corev1.ServiceAccount); ok {
			account := current.(*corev1.ServiceAccount)
			if account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken ||
				len(account.Secrets)+len(account.ImagePullSecrets) != 0 ||
				account.Labels[sandboxpolicy.AgentUIDLabel] != desired.GetLabels()[sandboxpolicy.AgentUIDLabel] {
				return fmt.Errorf("bootstrap ServiceAccount must retain its dedicated unprivileged identity")
			}
		}
	}
	if expected, ok := desired.(*corev1.PersistentVolume); ok {
		if err := validateSandboxSkillsVolume(current.(*corev1.PersistentVolume), expected); err != nil {
			return err
		}
	}
	if expected, ok := desired.(*corev1.PersistentVolumeClaim); ok && expected.Spec.VolumeName != "" {
		if !equality.Semantic.DeepEqual(current.(*corev1.PersistentVolumeClaim).Spec, expected.Spec) {
			return fmt.Errorf("%w: published skills claim differs from its recorded static binding; recovery is required", sandboxworkload.ErrStorageTransitionRequired)
		}
	}
	if cleanup {
		return nil
	}
	if !controllerutil.ContainsFinalizer(current, sandbox.BootstrapFinalizer) {
		return fmt.Errorf("Sandbox bootstrap resource lifecycle protection is missing; recovery is required")
	}
	switch expected := desired.(type) {
	case *corev1.Namespace:
		if current.GetLabels()[sandboxworkload.ProtectedNamespaceLabel] != "true" {
			return fmt.Errorf("Sandbox bootstrap namespace is no longer protected against replacement")
		}
	case *corev1.PersistentVolumeClaim:
		actual := current.(*corev1.PersistentVolumeClaim)
		if actual.Status.Phase == corev1.ClaimLost ||
			ptr.Deref(actual.Spec.VolumeMode, corev1.PersistentVolumeFilesystem) != corev1.PersistentVolumeFilesystem ||
			!equality.Semantic.DeepEqual(actual.Spec.StorageClassName, expected.Spec.StorageClassName) ||
			!equality.Semantic.DeepEqual(actual.Spec.Resources, expected.Spec.Resources) ||
			!slices.Equal(actual.Spec.AccessModes, expected.Spec.AccessModes) ||
			actual.Spec.DataSource != nil || actual.Spec.DataSourceRef != nil || actual.Spec.Selector != nil {
			return fmt.Errorf("%w: Sandbox bootstrap HOME no longer matches its profile", sandboxworkload.ErrStorageTransitionRequired)
		}
	case *rbacv1.Role:
		if !equality.Semantic.DeepEqual(current.(*rbacv1.Role).Rules, expected.Rules) {
			return fmt.Errorf("Sandbox bootstrap operator Role changed")
		}
	case *rbacv1.RoleBinding:
		actual := current.(*rbacv1.RoleBinding)
		if actual.RoleRef != expected.RoleRef || !equality.Semantic.DeepEqual(actual.Subjects, expected.Subjects) {
			return fmt.Errorf("Sandbox bootstrap operator RoleBinding changed")
		}
	}
	return nil
}
