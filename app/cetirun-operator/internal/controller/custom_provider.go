// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	strictjson "sigs.k8s.io/json"
)

// This is operator configuration, never a tenant Agent/Helm override.
type CustomRuntimeConfig struct {
	Enabled        bool                       `json:"enabled"`
	ApprovedImages []string                   `json:"approvedImages"`
	ImageUsers     map[string]CustomImageUser `json:"imageUsers,omitempty"`
	StorageClass   string                     `json:"storageClass"`
	StorageSize    string                     `json:"storageSize"`
	AccessMode     string                     `json:"accessMode"`
	FSGroup        int64                      `json:"fsGroup"`
	Context        map[string]string          `json:"context"`
}

type CustomImageUser struct {
	UID int64 `json:"uid"`
	GID int64 `json:"gid"`
}

func ParseCustomRuntimeConfig(raw string) (CustomRuntimeConfig, error) {
	var cfg CustomRuntimeConfig
	if raw == "" {
		return cfg, nil
	}
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &cfg)
	if err != nil || len(problems) != 0 {
		return cfg, fmt.Errorf("invalid CETIRUN_CUSTOM_RUNTIME configuration")
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	if len(cfg.ApprovedImages) > 64 {
		return cfg, fmt.Errorf("Custom provisioning supports at most 64 approved images")
	}
	digest := regexp.MustCompile(`^[^[:space:]]+@sha256:[a-f0-9]{64}$`)
	for _, image := range cfg.ApprovedImages {
		if !digest.MatchString(image) {
			return cfg, fmt.Errorf("Custom approvedImages must use immutable sha256 references")
		}
	}
	if len(cfg.ImageUsers) > len(cfg.ApprovedImages) {
		return cfg, fmt.Errorf("Custom image users require existing image approvals")
	}
	for image, user := range cfg.ImageUsers {
		if !slices.Contains(cfg.ApprovedImages, image) || user.UID < 1 || user.GID < 1 ||
			user.UID == egresscapture.ProxyUID || user.UID > 2147483647 || user.GID > 2147483647 {
			return cfg, fmt.Errorf("Custom image users require approved images and non-root numeric identities distinct from the proxy")
		}
	}
	if cfg.FSGroup < 1 || cfg.FSGroup > 2147483647 {
		return cfg, fmt.Errorf("Custom provisioning requires explicit qualified storage and a non-root fsGroup")
	}
	storage := runtimebindings.HomeClaim{StorageClass: cfg.StorageClass, StorageSize: cfg.StorageSize, AccessMode: cfg.AccessMode}
	if err := storage.Validate(); err != nil {
		return cfg, err
	}
	if cfg.Context["AGENTS.md"] == "" || cfg.Context["skills/cetirun-memory/SKILL.md"] == "" || len(raw) > 64*1024 {
		return cfg, fmt.Errorf("Custom provisioning requires bounded canonical context and cetirun-memory")
	}
	for _, content := range cfg.Context {
		if content == "" {
			return cfg, fmt.Errorf("Custom context files must not be empty")
		}
	}
	if err := runtimebindings.ValidateContextFiles(cfg.contextFiles()); err != nil {
		return cfg, fmt.Errorf("invalid Custom provisioning context: %w", err)
	}
	return cfg, nil
}

func (cfg CustomRuntimeConfig) contextFiles() map[string]string {
	files := map[string]string{}
	for path := range cfg.Context {
		hash := sha256.Sum256([]byte(path))
		files["file-"+hex.EncodeToString(hash[:16])] = path
	}
	return files
}

type KubernetesCustomBindings struct {
	reconciler *AgentReconciler
	config     CustomRuntimeConfig
	revision   string
}

func NewKubernetesCustomBindings(r *AgentReconciler, cfg CustomRuntimeConfig) *KubernetesCustomBindings {
	raw, _ := json.Marshal(cfg)
	sum := sha256.Sum256(raw)
	return &KubernetesCustomBindings{reconciler: r, config: cfg, revision: hex.EncodeToString(sum[:])}
}

func (p *KubernetesCustomBindings) authorize(instance *api.Agent) error {
	if !p.config.Enabled || instance.Spec.Agent.Type != api.AgentTypeCustom ||
		instance.Spec.Agent.Image == nil || !slices.Contains(p.config.ApprovedImages, *instance.Spec.Agent.Image) {
		return fmt.Errorf("Custom image is not approved")
	}
	return nil
}

func (p *KubernetesCustomBindings) UsesAIGateway(instance *api.Agent) bool {
	return p.config.Enabled && instance.Spec.Agent.Type == api.AgentTypeCustom && instance.Spec.Agent.Image != nil &&
		slices.Contains(p.config.ApprovedImages, *instance.Spec.Agent.Image)
}

func (p *KubernetesCustomBindings) ImageUser(instance *api.Agent) (CustomImageUser, error) {
	if err := p.authorize(instance); err != nil {
		return CustomImageUser{}, err
	}
	user, present := p.config.ImageUsers[*instance.Spec.Agent.Image]
	if !present || user.UID < 1 || user.GID < 1 {
		return CustomImageUser{}, fmt.Errorf("Custom FQDN capture requires the approved image's numeric UID/GID")
	}
	return user, nil
}

func objectReference(obj client.Object) runtimebindings.ObjectReference {
	return runtimebindings.ObjectReference{Name: obj.GetName(), UID: string(obj.GetUID()), Revision: obj.GetResourceVersion()}
}

func (p *KubernetesCustomBindings) Resolve(ctx context.Context, instance *api.Agent, team *api.Team) (runtimebindings.Custom, error) {
	r := p.reconciler
	var b runtimebindings.Custom
	if err := p.authorize(instance); err != nil {
		return b, err
	}
	if err := r.checkAgentGeneration(ctx, instance); err != nil {
		return b, err
	}
	ownership, err := r.readCustomOwnership(ctx, instance)
	if err != nil && !apierrors.IsNotFound(err) {
		return b, err
	}
	if apierrors.IsNotFound(err) && controllerutil.ContainsFinalizer(instance, customOwnershipFinalizer) {
		return b, fmt.Errorf("Custom ownership receipt is missing; recovery is required")
	}
	ns := &corev1.Namespace{}
	key := types.NamespacedName{Name: instanceNamespace(instance.Name)}
	if err := r.customReader().Get(ctx, key, ns); apierrors.IsNotFound(err) {
		if ownership != nil {
			return b, fmt.Errorf("retained Custom namespace is missing; automatic replacement is forbidden")
		}
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: key.Name,
			Annotations: map[string]string{customAgentUID: string(instance.UID)}}}
		if err := r.Create(ctx, ns); err != nil {
			return b, err
		}
	} else if err != nil {
		return b, err
	}
	if ns.Annotations[customAgentUID] != string(instance.UID) || !ns.DeletionTimestamp.IsZero() {
		return b, fmt.Errorf("Custom namespace has foreign ownership or is deleting")
	}
	if ownership != nil && ns.UID != ownership.NamespaceUID {
		return b, fmt.Errorf("Custom namespace UID changed")
	}
	if err := r.ensureNamespace(ctx, ns.Name, instance.Spec.User.Alias); err != nil {
		return b, err
	}
	if err := r.ensureNamespacedRBAC(ctx, ns.Name); err != nil {
		return b, err
	}
	if err := r.customReader().Get(ctx, key, ns); err != nil {
		return b, err
	}
	if revision := r.resolveIstioRevision(ctx); revision != "" && ns.Labels["istio.io/rev"] != revision {
		return b, fmt.Errorf("Custom namespace mesh preparation is pending")
	}
	home := &corev1.PersistentVolumeClaim{}
	homeKey := types.NamespacedName{Namespace: ns.Name, Name: "data-" + instanceServiceName(instance.Name) + "-0"}
	claim := &runtimebindings.HomeClaim{StorageClass: p.config.StorageClass, StorageSize: p.config.StorageSize, AccessMode: p.config.AccessMode}
	homeBinding := runtimebindings.Home{Name: homeKey.Name, FSGroup: p.config.FSGroup, Claim: claim}
	if ownership != nil {
		if ownership.Home != homeKey.Name {
			return b, fmt.Errorf("Custom HOME name changed")
		}
		if !claim.Matches(&corev1.PersistentVolumeClaim{Spec: ownership.HomeClaim.PVCSpec()}) {
			return b, fmt.Errorf("Custom HOME migration or resize requires separate storage approval")
		}
		homeBinding.Claim = ownership.HomeClaim
	}
	if err := r.customReader().Get(ctx, homeKey, home); apierrors.IsNotFound(err) {
		// The StatefulSet controller creates HOME from the approved template.
	} else if err != nil {
		return b, err
	} else {
		if ownership == nil || home.Annotations[customAgentUID] != string(instance.UID) ||
			!home.DeletionTimestamp.IsZero() || len(home.OwnerReferences) != 0 || home.Status.Phase == corev1.ClaimLost {
			return b, fmt.Errorf("Custom HOME is unowned, deleting or lost")
		}
		if !homeBinding.Claim.Matches(home) {
			return b, fmt.Errorf("Custom HOME migration or resize requires separate storage approval")
		}
	}
	b = runtimebindings.Custom{AgentName: instance.Name, AgentUID: string(instance.UID), Generation: instance.Generation,
		Namespace: objectReference(ns), ApprovedType: "custom", ApprovedImage: *instance.Spec.Agent.Image,
		PolicyRevision: p.revision,
		Home:           homeBinding,
		Network: runtimebindings.Network{Revision: p.revision,
			DNS: runtimebindings.Peer{Namespace: "kube-system", PodLabels: map[string]string{"k8s-app": "kube-dns"}}}}
	// Record ownership before Helm so failed installations can still be cleaned up.
	hadOwnership := controllerutil.ContainsFinalizer(instance, customOwnershipFinalizer)
	if err := r.ensureCustomOwnership(ctx, instance, b, nil); err != nil {
		return b, err
	}
	if !hadOwnership {
		return b, fmt.Errorf("Custom ownership recorded; waiting for the next observation")
	}
	files := p.config.contextFiles()
	data := map[string]string{}
	for key, path := range files {
		data[key] = p.config.Context[path]
		if path == "AGENTS.md" && team != nil {
			data[key] += "\n\n## Team context\n" + team.Spec.MemberContext.Role + "\n" +
				strings.Join(team.Spec.MemberContext.Description, "\n")
		}
	}
	raw, _ := json.Marshal(data)
	sum := sha256.Sum256(raw)
	name := "custom-context-" + hex.EncodeToString(sum[:16])
	snapshot := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: name}, snapshot); apierrors.IsNotFound(err) {
		snapshot = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(instance, api.GroupVersion.WithKind("Agent"))}},
			Immutable: ptr.To(true), Data: data}
		if err := r.Create(ctx, snapshot); err != nil {
			return b, err
		}
	} else if err != nil {
		return b, err
	}
	existing, _ := json.Marshal(snapshot.Data)
	if !hasAgentOwner(snapshot, instance) || !ptr.Deref(snapshot.Immutable, false) ||
		string(existing) != string(raw) || !snapshot.DeletionTimestamp.IsZero() {
		return b, fmt.Errorf("Custom context snapshot has changed or has foreign ownership")
	}
	b.Context = runtimebindings.Context{ObjectReference: objectReference(snapshot), Files: files}
	return b, nil
}

type providerOperation struct {
	Operation CustomApplyOperation `json:"operation"`
	Image     string               `json:"image"`
	Policy    string               `json:"policy"`
	Expires   time.Time            `json:"expires"`
	Finished  bool                 `json:"finished"`
	Outcome   CustomApplyOutcome   `json:"outcome"`
}

type kubernetesCustomLease struct {
	provider  *KubernetesCustomBindings
	agent     *api.Agent
	operation CustomApplyOperation
}

func (p *KubernetesCustomBindings) readOperation(ctx context.Context, instance *api.Agent) (*corev1.ConfigMap, providerOperation, error) {
	record := &corev1.ConfigMap{}
	err := p.reconciler.customReader().Get(ctx, p.reconciler.customRecordKey(instance, "lease"), record)
	var state providerOperation
	if err != nil {
		return nil, state, err
	}
	if !hasCustomRecordOwner(record, instance) || json.Unmarshal([]byte(record.Data["operation"]), &state) != nil ||
		state.Operation.ID == "" || state.Operation.PlanKey == "" || state.Operation.Generation < 1 ||
		state.Image == "" || state.Policy == "" || state.Expires.IsZero() {
		return nil, state, fmt.Errorf("Custom provider operation is invalid")
	}
	return record, state, nil
}

func (p *KubernetesCustomBindings) ApproveApply(ctx context.Context, instance *api.Agent, current *appsv1.StatefulSet, operation CustomApplyOperation) (CustomApplyLease, error) {
	r := p.reconciler
	if err := p.authorize(instance); err != nil {
		return nil, err
	}
	if err := r.checkAgentGeneration(ctx, instance); err != nil {
		return nil, err
	}
	if operation.ID == "" || operation.PlanKey == "" || operation.Generation != instance.Generation {
		return nil, fmt.Errorf("invalid Custom apply operation")
	}
	if current != nil && len(current.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("Custom workload has no Agent container")
	}
	record, state, err := p.readOperation(ctx, instance)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil && !state.Finished && state.Operation != operation {
		return nil, fmt.Errorf("another Custom operation still owns storage coordination")
	}
	if err == nil && state.Finished && state.Operation == operation {
		return nil, fmt.Errorf("completed Custom operation cannot grant another lease")
	}
	state = providerOperation{Operation: operation, Image: *instance.Spec.Agent.Image, Policy: p.revision,
		Expires: time.Now().Add(5 * time.Minute)}
	raw, _ := json.Marshal(state)
	if record == nil {
		key := r.customRecordKey(instance, "lease")
		record = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(instance, api.GroupVersion.WithKind("Agent"))}},
			Data: map[string]string{"operation": string(raw)}}
		err = r.Create(ctx, record)
	} else {
		record.Data["operation"] = string(raw)
		err = r.Update(ctx, record)
	}
	if err != nil {
		return nil, err
	}
	if current != nil {
		stopped := &appsv1.StatefulSet{}
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(current), stopped); err != nil {
			return nil, err
		}
		if stopped.UID != current.UID || !hasAgentOwner(stopped, instance) ||
			stopped.Annotations[customAgentUID] != string(instance.UID) || !stopped.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("Custom old writer identity changed")
		}
		stopped.Spec.Replicas = ptr.To[int32](0)
		if err := r.Update(ctx, stopped); err != nil {
			return nil, err
		}
	}
	ownership, err := r.readCustomOwnership(ctx, instance)
	if err != nil {
		return nil, err
	}
	if err := r.noCustomWriters(ctx, runtimebindings.Custom{
		Namespace: runtimebindings.ObjectReference{Name: ownership.Namespace},
		Home:      runtimebindings.Home{Name: ownership.Home},
	}); err != nil {
		return nil, err
	}
	return &kubernetesCustomLease{provider: p, agent: instance.DeepCopy(), operation: operation}, nil
}

func (p *KubernetesCustomBindings) ResumeApply(_ context.Context, instance *api.Agent, operation CustomApplyOperation) (CustomApplyLease, error) {
	return &kubernetesCustomLease{provider: p, agent: instance.DeepCopy(), operation: operation}, nil
}

func (l *kubernetesCustomLease) Check(ctx context.Context) error {
	if err := l.provider.authorize(l.agent); err != nil {
		return err
	}
	if err := l.provider.reconciler.checkAgentGeneration(ctx, l.agent); err != nil {
		return err
	}
	_, state, err := l.provider.readOperation(ctx, l.agent)
	if err != nil {
		return err
	}
	if state.Operation != l.operation || state.Finished || state.Policy != l.provider.revision ||
		state.Image != *l.agent.Spec.Agent.Image || time.Now().After(state.Expires) {
		return fmt.Errorf("Custom storage coordination is stale or expired")
	}
	return nil
}

func (l *kubernetesCustomLease) Complete(ctx context.Context, outcome CustomApplyOutcome) error {
	if outcome != CustomApplyNotStarted && outcome != CustomApplySucceeded && outcome != CustomApplyUnknown {
		return fmt.Errorf("invalid Custom apply outcome")
	}
	record, state, err := l.provider.readOperation(ctx, l.agent)
	if apierrors.IsNotFound(err) && outcome == CustomApplyNotStarted {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Operation != l.operation {
		// A different completed operation cannot have granted this attempt.
		if state.Finished && outcome == CustomApplyNotStarted {
			return nil
		}
		return fmt.Errorf("Custom provider completion identity changed")
	}
	if outcome == CustomApplyUnknown {
		return fmt.Errorf("Custom apply outcome is unknown; preserve the outbox for recovery")
	}
	if state.Finished {
		if state.Outcome != outcome {
			return fmt.Errorf("Custom provider completion outcome changed")
		}
		return nil
	}
	state.Finished, state.Outcome = true, outcome
	raw, _ := json.Marshal(state)
	record.Data["operation"] = string(raw)
	return l.provider.reconciler.Update(ctx, record)
}
