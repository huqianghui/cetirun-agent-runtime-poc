// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const sandboxStageConfigKey = "_cetirunSandboxStage"

// PrepareSandbox loads the native chart once and seals its protected, held
// output. Preparation grants no permission to start a workload.
func (c *Client) PrepareSandbox(namespace string, platform, user map[string]any, chartVersion string, bundle *sandboxworkload.Bundle) (*PreparedRelease, error) {
	if bundle == nil || namespace == "" || namespace != bundle.Identity().Namespace.Name {
		return nil, fmt.Errorf("Sandbox preparation requires the bound namespace and a protected bundle")
	}
	cfg, err := actionConfig(namespace)
	if err != nil {
		return nil, err
	}
	chrt, err := c.loadChart(cfg, namespace, chartVersion)
	if err != nil {
		return nil, err
	}
	return PrepareSandboxChart(chrt, platform, user, bundle)
}

// StageSandbox installs or upgrades only sealed zero-replica Sandbox resources.
// check must authoritatively verify the durable write-ahead grant inventory,
// Agent UID/generation/hold, current sources, approved namespace and absence of
// old Pods. It is called before writes and after the action, including failures.
// Retired controls must be revoked separately before changing the inventory.
// Failed stages permit exact-plan retries; pending operations require coordinated
// recovery. Identical deployed stages are a no-op only after fresh, default-aware
// API comparison and another hold check. Success is not an execution receipt.
func (c *Client) StageSandbox(ctx context.Context, namespace, name string, prepared *PreparedRelease, check func(context.Context) error) error {
	if err := sandboxStageArguments(ctx, namespace, name, prepared, check); err != nil {
		return err
	}
	cfg, err := sandboxActionConfiguration(ctx, namespace)
	if err != nil {
		return err
	}
	return prepared.stageSandbox(ctx, cfg, namespace, name, check)
}

// SandboxStageDigest reads the current deployed Helm record without changing it.
// It detects release-store drift for held preparation reuse, not enforcement.
func (c *Client) SandboxStageDigest(ctx context.Context, namespace, name string) (string, error) {
	if ctx == nil || namespace == "" || name == "" {
		return "", fmt.Errorf("Sandbox release inspection requires a context, namespace and name")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cfg, err := sandboxActionConfiguration(ctx, namespace)
	if err != nil {
		return "", err
	}
	return sandboxStageDigest(ctx, cfg, namespace, name)
}

func sandboxStageDigest(ctx context.Context, cfg *action.Configuration, namespace, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if cfg == nil || cfg.Releases == nil {
		return "", fmt.Errorf("Sandbox release inspection requires release storage")
	}
	current, err := cfg.Releases.Last(name)
	if err != nil {
		return "", fmt.Errorf("read staged Sandbox release: %w", err)
	}
	if current == nil || current.Info == nil || current.Name != name || current.Namespace != namespace ||
		current.Info.Status != release.StatusDeployed {
		return "", fmt.Errorf("Sandbox release is not deployed; guarded recovery is required")
	}
	raw, err := json.Marshal(current)
	if err != nil {
		return "", fmt.Errorf("encode staged Sandbox release identity: %w", err)
	}
	return sandboxBytesHash(raw), ctx.Err()
}

func sandboxActionConfiguration(ctx context.Context, namespace string) (*action.Configuration, error) {
	cfg, err := actionConfig(namespace)
	if err != nil {
		return nil, err
	}
	// Helm's resource helpers and release driver use background contexts, and
	// RunWithContext alone permits writes after cancellation. Bind their HTTP
	// requests as well, without changing the client's configuration globally.
	config, err := cfg.RESTClientGetter.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	config = rest.CopyConfig(config)
	config.Wrap(func(base http.RoundTripper) http.RoundTripper {
		return sandboxContextTransport{ctx: ctx, base: base}
	})
	if err := cfg.Init(restClientGetter(config, namespace), namespace, "secret", cfg.Log); err != nil {
		return nil, err
	}
	return cfg, nil
}

func sandboxStageArguments(ctx context.Context, namespace, name string, p *PreparedRelease, check func(context.Context) error) error {
	if ctx == nil || check == nil {
		return fmt.Errorf("Sandbox staging requires a context and authoritative hold check")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.validateSandboxStage(namespace, name)
}

func (p *PreparedRelease) validateSandboxStage(namespace, name string) error {
	if p == nil || p.custom || p.chart == nil || p.chart.Metadata == nil ||
		p.chart.Metadata.Name != "cetirun" || p.ChartVersion != p.chart.Metadata.Version ||
		p.sandboxBundle == nil || p.sandboxTemplate == nil || p.sealedInput == "" ||
		p.sealedInput != p.inputDigest() || p.manifestHash == "" ||
		p.manifestHash != sandboxBytesHash(p.manifest) || p.WorkloadHash != p.manifestHash {
		return fmt.Errorf("Sandbox staging requires an unchanged sealed protected preparation")
	}
	input := p.sandboxBundle.Identity()
	if namespace == "" || name == "" || namespace != input.Namespace.Name ||
		name != input.ReleaseName || name != p.releaseName || p.Agent.Type != input.AgentType ||
		p.PlanHash != input.Controls.Revision {
		return fmt.Errorf("Sandbox namespace, release or protected identity mismatch")
	}
	protected := object(p.Values[sandboxworkload.ValuesKey])
	if protected["contractVersion"] != sandboxworkload.ContractVersion || protected["executionHeld"] != true {
		return fmt.Errorf("Sandbox execution must remain held")
	}
	_, err := sandboxStageObjects(p.manifest, sandboxSealIdentity(input))
	return err
}

// This record distinguishes held native stages from legacy or foreign Helm
// releases. It binds release storage to the same namespace, Agent and claims
// across revisions, including failed partial applications.
type sandboxStageSeal struct {
	Version         string                         `json:"version"`
	Agent           sandboxworkload.ObjectIdentity `json:"agent"`
	AgentGeneration int64                          `json:"agentGeneration"`
	Namespace       sandboxworkload.ObjectIdentity `json:"namespace"`
	Home            sandboxworkload.ObjectIdentity `json:"home"`
	Skills          sandboxworkload.ObjectIdentity `json:"skills"`
	SkillsMode      sandboxworkload.SkillsMode     `json:"skillsMode,omitempty"`
	ReleaseName     string                         `json:"releaseName"`
	WorkloadName    string                         `json:"workloadName"`
	Runtime         agents.ResolvedAgent           `json:"runtime"`
	PlanHash        string                         `json:"planHash"`
	InputHash       string                         `json:"inputHash"`
	ManifestHash    string                         `json:"manifestHash"`
	ChartHash       string                         `json:"chartHash"`
	Digest          string                         `json:"digest"`
}

func sandboxSealIdentity(input sandboxworkload.Input) sandboxStageSeal {
	var skills sandboxworkload.ObjectIdentity
	if input.Skills != nil {
		skills = sandboxworkload.ObjectIdentity{Name: input.Skills.Name, UID: input.Skills.UID}
	}
	return sandboxStageSeal{
		Version: "held-v1", Agent: sandboxworkload.ObjectIdentity{Name: input.AgentName, UID: input.Resolution.Binding.AgentUID},
		AgentGeneration: input.AgentGeneration, Namespace: input.Namespace,
		Home:   sandboxworkload.ObjectIdentity{Name: input.Home.Name, UID: input.Home.UID},
		Skills: skills, SkillsMode: input.SkillsMode,
		ReleaseName: input.ReleaseName, WorkloadName: input.WorkloadName,
	}
}

func (s sandboxStageSeal) digest() string {
	s.Digest = ""
	return sandboxJSONHash(s)
}

func (s sandboxStageSeal) sameOwner(other sandboxStageSeal) bool {
	return s.Agent == other.Agent && s.Namespace == other.Namespace && s.Home == other.Home &&
		s.Skills == other.Skills && s.SkillsMode == other.SkillsMode && s.ReleaseName == other.ReleaseName &&
		s.WorkloadName == other.WorkloadName && s.Runtime.Type == other.Runtime.Type &&
		s.AgentGeneration <= other.AgentGeneration
}

func sandboxBytesHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func sandboxJSONHash(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return sandboxBytesHash(raw)
}

func (p *PreparedRelease) stageSandbox(ctx context.Context, cfg *action.Configuration, namespace, name string, check func(context.Context) error) error {
	if err := sandboxStageArguments(ctx, namespace, name, p, check); err != nil {
		return err
	}
	if cfg == nil || cfg.KubeClient == nil || cfg.Releases == nil {
		return fmt.Errorf("Sandbox staging requires Helm action configuration")
	}
	// Snapshot all action inputs before calling application code. No source
	// templates, dependencies, hooks, CRDs or mutable caller values reach Helm.
	frozen := p.frozenChart()
	raw, err := json.Marshal(frozen)
	if err != nil {
		return err
	}
	var chrt chart.Chart
	if err := json.Unmarshal(raw, &chrt); err != nil {
		return err
	}
	raw, err = json.Marshal(p.Values)
	if err != nil {
		return err
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	if _, exists := values[sandboxStageConfigKey]; exists {
		return fmt.Errorf("Sandbox stage provenance cannot be supplied through Helm values")
	}
	seal := sandboxSealIdentity(p.sandboxBundle.Identity())
	seal.Runtime, seal.PlanHash, seal.InputHash = p.Agent, p.PlanHash, p.sealedInput
	seal.ManifestHash, seal.ChartHash = p.manifestHash, sandboxJSONHash(&chrt)
	seal.Digest = seal.digest()
	raw, err = json.Marshal(seal)
	if err != nil {
		return err
	}
	var protected map[string]any
	if err := json.Unmarshal(raw, &protected); err != nil {
		return err
	}
	values[sandboxStageConfigKey] = protected
	run := &sandboxStageRun{
		ctx: ctx, cfg: cfg, prepared: p, namespace: namespace, name: name, check: check,
		seal: seal, manifest: bytes.Clone(p.manifest), revision: 1, previousHashes: map[int]string{},
	}
	if err := run.guard(); err != nil {
		return err
	}
	last, err := cfg.Releases.Last(name)
	if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		return err
	}
	if err == nil {
		if err := run.acceptPrevious(last); err != nil {
			return err
		}
		run.lastHash, run.revision = sandboxJSONHash(last), last.Version+1
		run.previous = append(run.previous, last)
		run.previousHashes[last.Version] = run.lastHash
		// Helm may use an older deployed baseline when the latest revision
		// failed. Verify that baseline too, not just the last release.
		if last.Info.Status == release.StatusFailed {
			base, err := cfg.Releases.Deployed(name)
			if err != nil && !errors.Is(err, driver.ErrNoDeployedReleases) {
				return err
			}
			if err == nil {
				if err := run.acceptPrevious(base); err != nil {
					return err
				}
				run.previous = append(run.previous, base)
				run.previousHashes[base.Version] = sandboxJSONHash(base)
			}
		}
	}
	if err := run.preflight(false, false); err != nil {
		return err
	}
	if unchanged, err := run.unchangedLiveStage(last); err != nil || unchanged {
		return err
	}
	guardedDriver := &sandboxStageDriver{Driver: cfg.Releases.Driver, run: run}
	guardedConfig := *cfg
	guardedConfig.Releases = storage.Init(guardedDriver)
	guardedConfig.Releases.Log = cfg.Releases.Log
	guardedConfig.KubeClient = &sandboxStageKube{Interface: cfg.KubeClient, run: run}
	renderer := sandboxStagePostRenderer{manifest: run.manifest, seal: seal}
	if run.revision == 1 {
		install := action.NewInstall(&guardedConfig)
		install.Namespace, install.ReleaseName = namespace, name
		install.DisableHooks, install.SkipCRDs = true, true
		install.CreateNamespace, install.Wait, install.Atomic = false, false, false
		install.PostRenderer = renderer
		_, err = install.RunWithContext(ctx, &chrt, values)
	} else {
		upgrade := action.NewUpgrade(&guardedConfig)
		upgrade.Namespace, upgrade.DisableHooks, upgrade.ResetValues = namespace, true, true
		upgrade.Wait, upgrade.Atomic, upgrade.CleanupOnFail, upgrade.Recreate = false, false, false, false
		// Do not prune history or silently invoke an unrelated delete path.
		upgrade.MaxHistory, upgrade.PostRenderer = 0, renderer
		_, err = upgrade.RunWithContext(ctx, name, &chrt, values)
	}
	err = errors.Join(err, guardedDriver.err(), run.guard())
	if err != nil {
		return fmt.Errorf("Sandbox held Helm stage failed: %w", err)
	}
	if err := run.preflight(true, true); err != nil {
		return err
	}
	// Helm install can swallow its final storage-update error. A returned
	// release or an existing-name conflict is not durable success evidence.
	applied, err := cfg.Releases.Last(name)
	if err != nil {
		return err
	}
	if err := run.exactRelease(applied, release.StatusDeployed); err != nil {
		return fmt.Errorf("Sandbox held stage was not durably recorded: %w", err)
	}
	return run.guard()
}

type sandboxStageRun struct {
	ctx            context.Context
	cfg            *action.Configuration
	prepared       *PreparedRelease
	namespace      string
	name           string
	check          func(context.Context) error
	seal           sandboxStageSeal
	manifest       []byte
	revision       int
	lastHash       string
	previous       []*release.Release
	previousHashes map[int]string
	objects        map[string]*unstructured.Unstructured
}

func (r *sandboxStageRun) guard() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if err := r.unchangedPreparation(); err != nil {
		return err
	}
	if err := r.check(r.ctx); err != nil {
		return fmt.Errorf("Sandbox authoritative hold check failed: %w", err)
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	return r.unchangedPreparation()
}

func (r *sandboxStageRun) unchangedPreparation() error {
	if err := r.prepared.validateSandboxStage(r.namespace, r.name); err != nil {
		return err
	}
	current := sandboxSealIdentity(r.prepared.sandboxBundle.Identity())
	current.Runtime = r.prepared.Agent
	if r.prepared.sealedInput != r.seal.InputHash || r.prepared.manifestHash != r.seal.ManifestHash ||
		!current.sameOwner(r.seal) || current.AgentGeneration != r.seal.AgentGeneration {
		return fmt.Errorf("Sandbox preparation changed during staging")
	}
	return nil
}

func (r *sandboxStageRun) acceptPrevious(rel *release.Release) error {
	seal, err := sandboxReleaseSeal(rel)
	if err != nil || !seal.sameOwner(r.seal) || rel.Name != r.name || rel.Namespace != r.namespace ||
		rel.Info == nil || rel.Version < 1 {
		return fmt.Errorf("Sandbox prior release is foreign, legacy or changed: %w", errors.Join(err, sandboxworkload.ErrExecutionHeld))
	}
	if rel.Info.Status.IsPending() {
		return fmt.Errorf("Sandbox release operation is pending; coordinated recovery is required")
	}
	if rel.Info.Status != release.StatusDeployed && rel.Info.Status != release.StatusFailed {
		return fmt.Errorf("Sandbox prior release has an unexpected lifecycle state")
	}
	if rel.Info.Status == release.StatusFailed && seal.Digest != r.seal.Digest {
		return fmt.Errorf("Sandbox failed stage requires recovery of its exact held plan before transition")
	}
	return nil
}

func (r *sandboxStageRun) exactRelease(rel *release.Release, status release.Status) error {
	seal, err := sandboxReleaseSeal(rel)
	if err != nil {
		return err
	}
	if rel.Name != r.name || rel.Namespace != r.namespace || rel.Version != r.revision ||
		rel.Info == nil || rel.Info.Status != status || seal.Digest != r.seal.Digest ||
		rel.Manifest != string(r.manifest) {
		return fmt.Errorf("Sandbox release changed during staging")
	}
	return nil
}

func (r *sandboxStageRun) unchangedPrevious() error {
	last, err := r.cfg.Releases.Last(r.name)
	if r.lastHash == "" && errors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.lastHash == "" || sandboxJSONHash(last) != r.lastHash {
		return fmt.Errorf("Sandbox release changed before the Helm write")
	}
	for revision, expected := range r.previousHashes {
		current, err := r.cfg.Releases.Get(r.name, revision)
		if err != nil {
			return err
		}
		if sandboxJSONHash(current) != expected {
			return fmt.Errorf("Sandbox release baseline changed before the Helm write")
		}
	}
	return nil
}

func (r *sandboxStageRun) unchangedLiveStage(last *release.Release) (bool, error) {
	if last == nil || last.Info == nil || last.Info.Status != release.StatusDeployed {
		return false, nil
	}
	seal, err := sandboxReleaseSeal(last)
	if err != nil {
		return false, err
	}
	if seal.Digest != r.seal.Digest || last.Manifest != string(r.manifest) {
		return false, nil
	}
	if err := r.guard(); err != nil {
		return false, err
	}
	client := &sandboxStageKube{Interface: r.cfg.KubeClient, run: r}
	resources, err := client.Build(bytes.NewReader(r.manifest), false)
	if err != nil {
		return false, err
	}
	if len(resources) != len(r.objects) {
		return false, fmt.Errorf("Sandbox no-op check has an incomplete resource inventory")
	}
	seen := map[string]bool{}
	versions := make([]string, len(resources))
	for index, info := range resources {
		if info == nil || info.Client == nil || info.Mapping == nil || info.Mapping.Scope == nil {
			return false, fmt.Errorf("Sandbox no-op check requires authoritative resource clients")
		}
		key := sandboxResourceKey(info)
		observed, exists := r.objects[key]
		if !exists || seen[key] {
			return false, fmt.Errorf("Sandbox no-op resource inventory changed after preflight")
		}
		seen[key] = true
		currentObject, err := info.Client.Get().
			NamespaceIfScoped(info.Namespace, info.Mapping.Scope.Name() == meta.RESTScopeNameNamespace).
			Resource(info.Mapping.Resource.Resource).Name(info.Name).Do(r.ctx).Get()
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		current, err := sandboxUnstructured(currentObject)
		if err != nil {
			return false, err
		}
		if !sandboxSameObjectVersion(observed, current) {
			return false, fmt.Errorf("Sandbox object changed during the no-op check")
		}
		versions[index] = current.GetResourceVersion()
		desired, err := r.desiredObject(info.Object)
		if err != nil {
			return false, err
		}
		if !sandboxDesiredObjectAgrees(desired, current) {
			return false, nil
		}
	}
	if err := r.unchangedPrevious(); err != nil {
		return false, err
	}
	// A match at different times is insufficient if an earlier resource was
	// replaced or modified while later resources were being compared.
	for index, info := range resources {
		current, err := info.Client.Get().
			NamespaceIfScoped(info.Namespace, info.Mapping.Scope.Name() == meta.RESTScopeNameNamespace).
			Resource(info.Mapping.Resource.Resource).Name(info.Name).Do(r.ctx).Get()
		if err != nil {
			return false, err
		}
		metadata, err := meta.Accessor(current)
		if err != nil {
			return false, err
		}
		key := sandboxResourceKey(info)
		if metadata.GetUID() != r.objects[key].GetUID() || metadata.GetResourceVersion() != versions[index] {
			return false, fmt.Errorf("Sandbox object changed during the no-op check")
		}
	}
	return true, r.guard()
}

func sandboxResourceKey(info *resource.Info) string {
	gvk := info.Mapping.GroupVersionKind
	return gvk.GroupVersion().String() + "/" + gvk.Kind + "/" + info.Namespace + "/" + info.Name
}

func sandboxSameObjectVersion(a, b *unstructured.Unstructured) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.GetUID() == b.GetUID() && a.GetResourceVersion() == b.GetResourceVersion()
}

func (r *sandboxStageRun) desiredObject(value any) (*unstructured.Unstructured, error) {
	desired, err := sandboxUnstructured(value)
	if err != nil {
		return nil, err
	}
	labels, annotations := desired.GetLabels(), desired.GetAnnotations()
	if labels == nil {
		labels = map[string]string{}
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	labels["app.kubernetes.io/managed-by"] = "Helm"
	annotations["meta.helm.sh/release-name"], annotations["meta.helm.sh/release-namespace"] = r.name, r.namespace
	desired.SetLabels(labels)
	desired.SetAnnotations(annotations)
	return desired, nil
}

func sandboxUnstructured(value any) (*unstructured.Unstructured, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var obj unstructured.Unstructured
	if err := obj.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return &obj, nil
}

func sandboxComparableObject(object *unstructured.Unstructured) map[string]any {
	out := object.DeepCopy()
	delete(out.Object, "status")
	for _, field := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "selfLink"} {
		unstructured.RemoveNestedField(out.Object, "metadata", field)
	}
	return out.Object
}

func sandboxDesiredObjectAgrees(desired, current *unstructured.Unstructured) bool {
	want, got := sandboxComparableObject(desired), sandboxComparableObject(current)
	switch desired.GetAPIVersion() + "/" + desired.GetKind() {
	case "apps/v1/StatefulSet":
		var a, b appsv1.StatefulSet
		if decodeObject(desired, &a) != nil || decodeObject(current, &b) != nil ||
			!sandboxworkload.SamePodTemplate(b.Spec.Template, a.Spec.Template) ||
			sandboxJSONHash(a.Spec.Template.ObjectMeta) != sandboxJSONHash(b.Spec.Template.ObjectMeta) {
			return false
		}
		a.Spec.Template, b.Spec.Template = corev1.PodTemplateSpec{}, corev1.PodTemplateSpec{}
		sandboxDefaultStatefulSet(&a.Spec)
		sandboxDefaultStatefulSet(&b.Spec)
		if sandboxJSONHash(a.Spec) != sandboxJSONHash(b.Spec) {
			return false
		}
		delete(want, "spec")
		delete(got, "spec")
	case "v1/Service":
		var a, b corev1.Service
		if decodeObject(desired, &a) != nil || decodeObject(current, &b) != nil {
			return false
		}
		// Ignore only the API's single-stack address allocation when the
		// chart requested automatic allocation, not headless/external or
		// otherwise changed service behavior.
		if a.Spec.ClusterIP == "" && len(a.Spec.ClusterIPs) == 0 {
			if b.Spec.ClusterIP == corev1.ClusterIPNone || len(b.Spec.ClusterIPs) > 1 {
				return false
			}
			b.Spec.ClusterIP, b.Spec.ClusterIPs = "", nil
		}
		if len(a.Spec.IPFamilies) == 0 {
			if len(b.Spec.IPFamilies) > 1 {
				return false
			}
			b.Spec.IPFamilies = nil
		}
		if a.Spec.IPFamilyPolicy == nil && b.Spec.IPFamilyPolicy != nil {
			if *b.Spec.IPFamilyPolicy != corev1.IPFamilyPolicySingleStack {
				return false
			}
			b.Spec.IPFamilyPolicy = nil
		}
		for _, spec := range []*corev1.ServiceSpec{&a.Spec, &b.Spec} {
			if spec.SessionAffinity == "" {
				spec.SessionAffinity = corev1.ServiceAffinityNone
			}
			if spec.InternalTrafficPolicy == nil {
				spec.InternalTrafficPolicy = ptr.To(corev1.ServiceInternalTrafficPolicyCluster)
			}
		}
		if sandboxJSONHash(a.Spec) != sandboxJSONHash(b.Spec) {
			return false
		}
		delete(want, "spec")
		delete(got, "spec")
	}
	return reflect.DeepEqual(want, got)
}

func sandboxDefaultStatefulSet(spec *appsv1.StatefulSetSpec) {
	if spec.PodManagementPolicy == "" {
		spec.PodManagementPolicy = appsv1.OrderedReadyPodManagement
	}
	if spec.RevisionHistoryLimit == nil {
		spec.RevisionHistoryLimit = ptr.To(int32(10))
	}
	if spec.UpdateStrategy.Type == "" {
		spec.UpdateStrategy.Type = appsv1.RollingUpdateStatefulSetStrategyType
	}
	if spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType {
		if spec.UpdateStrategy.RollingUpdate == nil {
			spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{}
		}
		if spec.UpdateStrategy.RollingUpdate.Partition == nil {
			spec.UpdateStrategy.RollingUpdate.Partition = ptr.To(int32(0))
		}
		if spec.UpdateStrategy.RollingUpdate.MaxUnavailable == nil {
			spec.UpdateStrategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt32(1))
		}
	}
	if spec.PersistentVolumeClaimRetentionPolicy == nil {
		spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{}
	}
	if spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted == "" {
		spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted = appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	}
	if spec.PersistentVolumeClaimRetentionPolicy.WhenScaled == "" {
		spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	}
	if spec.Ordinals == nil {
		spec.Ordinals = &appsv1.StatefulSetOrdinals{}
	}
}

func sandboxReleaseSeal(rel *release.Release) (sandboxStageSeal, error) {
	var seal sandboxStageSeal
	if rel == nil || decodeObject(rel.Config[sandboxStageConfigKey], &seal) != nil ||
		seal.Version != "held-v1" || seal.Agent.Name == "" || seal.Agent.UID == "" ||
		seal.AgentGeneration < 1 || seal.Namespace.Name == "" || seal.Namespace.UID == "" ||
		seal.Home.Name == "" || seal.Home.UID == "" || seal.SkillsMode.ValidateIdentity(seal.Skills) != nil ||
		seal.ReleaseName != rel.Name || seal.Namespace.Name != rel.Namespace ||
		(seal.Runtime.Type != "openclaw" && seal.Runtime.Type != "hermes") ||
		(seal.SkillsMode == sandboxworkload.SkillsModeEmpty && seal.Runtime.Type != "openclaw") ||
		seal.Digest == "" || seal.Digest != seal.digest() ||
		seal.ManifestHash != sandboxBytesHash([]byte(rel.Manifest)) || seal.ChartHash != sandboxJSONHash(rel.Chart) ||
		len(rel.Hooks) != 0 || rel.Chart == nil || rel.Chart.Metadata == nil || rel.Chart.Metadata.Name != "cetirun" ||
		len(rel.Chart.Dependencies()) != 0 || len(rel.Chart.CRDObjects()) != 0 ||
		len(rel.Chart.Files) != 1 || rel.Chart.Files[0].Name != "approved.yaml" ||
		!bytes.Equal(rel.Chart.Files[0].Data, []byte(rel.Manifest)) ||
		len(rel.Chart.Templates) != 1 || rel.Chart.Templates[0].Name != "templates/approved.yaml" ||
		string(rel.Chart.Templates[0].Data) != `{{ .Files.Get "approved.yaml" }}` {
		return seal, fmt.Errorf("Sandbox release lacks valid sealed held provenance")
	}
	values := make(map[string]any, len(rel.Config)-1)
	for key, value := range rel.Config {
		if key != sandboxStageConfigKey {
			values[key] = value
		}
	}
	p := &PreparedRelease{Values: values, Agent: seal.Runtime, PlanHash: seal.PlanHash, WorkloadHash: seal.ManifestHash}
	if seal.InputHash == "" || p.inputDigest() != seal.InputHash {
		return seal, fmt.Errorf("Sandbox stored configuration changed after sealing")
	}
	if _, err := sandboxStageObjects([]byte(rel.Manifest), seal); err != nil {
		return seal, err
	}
	return seal, nil
}

// Check the sealed representation, not another render. The only native
// workload allowed here is the held StatefulSet; neither claims nor routes are
// eligible for Helm creation, adoption, modification or deletion.
func sandboxStageObjects(manifest []byte, seal sandboxStageSeal) (map[string]unstructured.Unstructured, error) {
	reader := kubeyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifest)))
	objects := map[string]unstructured.Unstructured{}
	native := map[string]bool{}
	owner := []metav1.OwnerReference{sandboxworkload.AgentOwner(seal.Agent.Name, seal.Agent.UID)}
	for {
		document, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || validateYAMLKeys(document) != nil {
			return nil, fmt.Errorf("invalid Sandbox stage manifest")
		}
		raw, err := yaml.YAMLToJSONStrict(document)
		if err != nil {
			return nil, err
		}
		var obj unstructured.Unstructured
		if err := json.Unmarshal(raw, &obj.Object); err != nil {
			return nil, err
		}
		if len(obj.Object) == 0 {
			continue
		}
		key := sandboxObjectKey(obj)
		if _, exists := objects[key]; exists || obj.GetName() == "" ||
			!equality.Semantic.DeepEqual(obj.GetOwnerReferences(), owner) {
			return nil, fmt.Errorf("Sandbox stage object identity or Agent ownership is invalid")
		}
		for key := range obj.GetAnnotations() {
			if strings.HasPrefix(key, "helm.sh/") {
				return nil, fmt.Errorf("Sandbox stage cannot contain Helm lifecycle annotations")
			}
		}
		switch obj.GetAPIVersion() + "/" + obj.GetKind() {
		case "apps/v1/StatefulSet":
			var stateful appsv1.StatefulSet
			if err := decodeObject(obj.Object, &stateful); err != nil || stateful.Name != seal.WorkloadName ||
				stateful.Namespace != seal.Namespace.Name || ptr.Deref(stateful.Spec.Replicas, -1) != 0 ||
				len(stateful.Spec.VolumeClaimTemplates) != 0 || stateful.Spec.ServiceName != seal.WorkloadName {
				return nil, fmt.Errorf("Sandbox stage StatefulSet must remain held without managed claims")
			}
			native["StatefulSet"] = true
		case "v1/ConfigMap":
			if obj.GetName() != seal.WorkloadName+"-config" || obj.GetNamespace() != seal.Namespace.Name {
				return nil, fmt.Errorf("Sandbox stage has a foreign ConfigMap")
			}
			native["ConfigMap"] = true
		case "v1/Service":
			var service corev1.Service
			if err := decodeObject(obj.Object, &service); err != nil || service.Name != seal.WorkloadName ||
				service.Namespace != seal.Namespace.Name || service.Spec.Type != corev1.ServiceTypeClusterIP ||
				service.Spec.ExternalName != "" || len(service.Spec.ExternalIPs) != 0 ||
				service.Spec.LoadBalancerIP != "" || service.Spec.LoadBalancerClass != nil {
				return nil, fmt.Errorf("Sandbox stage Service must remain private")
			}
			for _, port := range service.Spec.Ports {
				if port.NodePort != 0 {
					return nil, fmt.Errorf("Sandbox stage cannot expose a NodePort")
				}
			}
			native["Service"] = true
		case "cilium.io/v2/CiliumNetworkPolicy", "v1/ServiceAccount":
			if obj.GetNamespace() != seal.Namespace.Name {
				return nil, fmt.Errorf("Sandbox stage control has a foreign namespace")
			}
		case "rbac.authorization.k8s.io/v1/RoleBinding":
			if obj.GetNamespace() == "" {
				return nil, fmt.Errorf("Sandbox stage RoleBinding requires an explicit namespace")
			}
		case "rbac.authorization.k8s.io/v1/ClusterRoleBinding":
			if obj.GetNamespace() != "" {
				return nil, fmt.Errorf("Sandbox stage ClusterRoleBinding must be cluster-scoped")
			}
		default:
			return nil, fmt.Errorf("Sandbox stage cannot manage %s", obj.GetKind())
		}
		objects[key] = obj
	}
	if !native["StatefulSet"] || !native["ConfigMap"] || !native["Service"] {
		return nil, fmt.Errorf("Sandbox stage is missing its held native workload")
	}
	return objects, nil
}

type sandboxStagePostRenderer struct {
	manifest []byte
	seal     sandboxStageSeal
}

func (r sandboxStagePostRenderer) Run(buffer *bytes.Buffer) (*bytes.Buffer, error) {
	expected, err := sandboxStageObjects(r.manifest, r.seal)
	if err != nil {
		return nil, err
	}
	actual, err := sandboxStageObjects(buffer.Bytes(), r.seal)
	if err != nil {
		return nil, fmt.Errorf("Helm changed the sealed Sandbox manifest: %w", err)
	}
	if !reflect.DeepEqual(expected, actual) {
		return nil, fmt.Errorf("Helm changed the sealed Sandbox manifest")
	}
	return bytes.NewBuffer(bytes.Clone(r.manifest)), nil
}

func (r *sandboxStageRun) preflight(compare, applied bool) error {
	if err := r.guard(); err != nil {
		return err
	}
	manifests := []string{string(r.manifest)}
	for _, previous := range r.previous {
		manifests = append(manifests, previous.Manifest)
	}
	seen := map[string]bool{}
	live := map[string]*unstructured.Unstructured{}
	owner := []metav1.OwnerReference{sandboxworkload.AgentOwner(r.seal.Agent.Name, r.seal.Agent.UID)}
	desired, err := sandboxStageObjects(r.manifest, r.seal)
	if err != nil {
		return err
	}
	for index, manifest := range manifests {
		objects, err := sandboxStageObjects([]byte(manifest), r.seal)
		if err != nil {
			return err
		}
		resources, err := r.cfg.KubeClient.Build(strings.NewReader(manifest), false)
		if err != nil {
			return err
		}
		if len(resources) != len(objects) {
			return fmt.Errorf("Sandbox API preflight returned an incomplete resource inventory")
		}
		for _, info := range resources {
			if info == nil || info.Client == nil || info.Mapping == nil || info.Mapping.Scope == nil {
				return fmt.Errorf("Sandbox API preflight requires authoritative resource clients")
			}
			gvk := info.Mapping.GroupVersionKind
			key := sandboxResourceKey(info)
			if _, ok := objects[key]; !ok {
				return fmt.Errorf("Sandbox API resource mapping differs from the sealed inventory")
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			current, err := info.Client.Get().
				NamespaceIfScoped(info.Namespace, info.Mapping.Scope.Name() == meta.RESTScopeNameNamespace).
				Resource(info.Mapping.Resource.Resource).Name(info.Name).Do(r.ctx).Get()
			if apierrors.IsNotFound(err) {
				if applied && index == 0 {
					return fmt.Errorf("Sandbox stage did not create its complete resource inventory")
				}
				live[key] = nil
			} else if err != nil {
				return fmt.Errorf("Sandbox object preflight failed for %s: %w", info.Mapping.GroupVersionKind.Kind, err)
			} else {
				object, err := sandboxUnstructured(current)
				if err != nil {
					return err
				}
				if (!applied && r.revision == 1) || object.GetName() != info.Name ||
					object.GetNamespace() != info.Namespace || object.GetUID() == "" ||
					object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil ||
					!equality.Semantic.DeepEqual(object.GetOwnerReferences(), owner) ||
					object.GetLabels()["app.kubernetes.io/managed-by"] != "Helm" ||
					object.GetAnnotations()["meta.helm.sh/release-name"] != r.name ||
					object.GetAnnotations()["meta.helm.sh/release-namespace"] != r.namespace {
					return fmt.Errorf("Sandbox existing %s is not owned by this exact Agent and release", info.Mapping.GroupVersionKind.Kind)
				}
				if _, retained := desired[key]; !retained {
					// Helm Update can log and ignore deletion failures. Do
					// not delegate grant retirement to that success path.
					return fmt.Errorf("Sandbox retired %s requires journalled revocation before staging", gvk.Kind)
				}
				if info.Mapping.GroupVersionKind.Kind == "StatefulSet" {
					var stateful appsv1.StatefulSet
					if err := decodeObject(current, &stateful); err != nil {
						return err
					}
					if ptr.Deref(stateful.Spec.Replicas, -1) != 0 || stateful.Status.Replicas != 0 {
						return fmt.Errorf("Sandbox existing StatefulSet is not held and drained")
					}
					if err := r.prepared.sandboxBundle.CheckStatefulSetTransition(&stateful); err != nil {
						return err
					}
				}
				if applied {
					expected, err := r.desiredObject(info.Object)
					if err != nil {
						return err
					}
					if !sandboxDesiredObjectAgrees(expected, object) {
						return fmt.Errorf("Sandbox staged %s does not match its sealed content", gvk.Kind)
					}
				}
				live[key] = object
			}
			if compare {
				expected, exists := r.objects[key]
				if !exists || !sandboxSameObjectVersion(expected, live[key]) {
					return fmt.Errorf("Sandbox object was replaced or changed during staging")
				}
			}
		}
	}
	if !compare {
		r.objects = live
	}
	return r.ctx.Err()
}

type sandboxStageKube struct {
	kube.Interface
	run *sandboxStageRun
}

func (k *sandboxStageKube) beforeWrite() error {
	if err := k.run.preflight(true, false); err != nil {
		return err
	}
	last, err := k.run.cfg.Releases.Last(k.run.name)
	if err != nil {
		return err
	}
	status := release.StatusPendingInstall
	if k.run.revision != 1 {
		status = release.StatusPendingUpgrade
	}
	return k.run.exactRelease(last, status)
}

func (k *sandboxStageKube) Build(reader io.Reader, validate bool) (kube.ResourceList, error) {
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if _, err := sandboxStageObjects(raw, k.run.seal); err != nil {
		return nil, err
	}
	resources, err := k.Interface.Build(bytes.NewReader(raw), validate)
	if err != nil {
		return nil, err
	}
	for _, info := range resources {
		if info == nil || info.Mapping == nil {
			return nil, fmt.Errorf("Sandbox stage has an invalid API resource mapping")
		}
	}
	return resources, nil
}

func (k *sandboxStageKube) Create(resources kube.ResourceList) (*kube.Result, error) {
	if err := k.beforeWrite(); err != nil {
		return &kube.Result{}, err
	}
	result, err := k.Interface.Create(resources)
	if err == nil {
		err = k.recordApplied(resources)
	}
	return result, err
}

func (k *sandboxStageKube) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	if force {
		return &kube.Result{}, fmt.Errorf("Sandbox staging cannot force replacement")
	}
	if err := k.beforeWrite(); err != nil {
		return &kube.Result{}, err
	}
	result := &kube.Result{}
	// Helm Update prunes original.Difference(target) with name-only DELETEs.
	// Reconcile only the target; retirement belongs to the guarded journal.
	for _, info := range target {
		current, exists := k.run.objects[sandboxResourceKey(info)]
		if !exists {
			return result, fmt.Errorf("Sandbox update has an unobserved resource")
		}
		if current == nil {
			result.Created = append(result.Created, info)
			if _, err := k.Interface.Create(kube.ResourceList{info}); err != nil {
				return result, err
			}
			continue
		}
		patch, patchType, err := sandboxStagePatch(original.Get(info), info, current)
		if err != nil {
			return result, err
		}
		updated, err := info.Client.Patch(patchType).
			NamespaceIfScoped(info.Namespace, info.Mapping.Scope.Name() == meta.RESTScopeNameNamespace).
			Resource(info.Mapping.Resource.Resource).Name(info.Name).Body(patch).Do(k.run.ctx).Get()
		if err != nil {
			return result, fmt.Errorf("Sandbox conditional update of %s failed: %w", info.Mapping.GroupVersionKind.Kind, err)
		}
		if err := info.Refresh(updated, true); err != nil {
			return result, err
		}
		result.Updated = append(result.Updated, info)
	}
	return result, k.recordApplied(target)
}

// Use Helm's native/CR merge strategies, but with the observed live state and
// explicit identity preconditions. Helm's CR two-way diff misses live drift,
// and even its three-way diff omits unchanged UID/resourceVersion fields.
func sandboxStagePatch(original, target *resource.Info, current *unstructured.Unstructured) ([]byte, types.PatchType, error) {
	old := []byte("{}")
	if original != nil {
		previous, err := sandboxUnstructured(original.Object)
		if err != nil {
			return nil, "", err
		}
		// An adoption check can append a live object to Helm's baseline.
		// Its extra fields are not part of the sealed managed inventory.
		if previous.GetUID() == "" && previous.GetResourceVersion() == "" {
			old, err = previous.MarshalJSON()
			if err != nil {
				return nil, "", err
			}
		}
	}
	desired, err := json.Marshal(target.Object)
	if err != nil {
		return nil, "", err
	}
	live, err := current.MarshalJSON()
	if err != nil {
		return nil, "", err
	}
	var patch []byte
	patchType := types.MergePatchType
	versioned := kube.AsVersioned(target)
	if _, unstructured := versioned.(runtime.Unstructured); unstructured {
		patch, err = jsonmergepatch.CreateThreeWayJSONMergePatch(old, desired, live)
	} else {
		patchType = types.StrategicMergePatchType
		var patchMeta strategicpatch.PatchMetaFromStruct
		patchMeta, err = strategicpatch.NewPatchMetaFromStruct(versioned)
		if err == nil {
			patch, err = strategicpatch.CreateThreeWayMergePatch(old, desired, live, patchMeta, true)
		}
	}
	if err != nil {
		return nil, "", err
	}
	var conditional unstructured.Unstructured
	if err := utiljson.Unmarshal(patch, &conditional.Object); err != nil {
		return nil, "", err
	}
	conditional.SetUID(current.GetUID())
	conditional.SetResourceVersion(current.GetResourceVersion())
	patch, err = conditional.MarshalJSON()
	return patch, patchType, err
}

func (k *sandboxStageKube) recordApplied(resources kube.ResourceList) error {
	for _, info := range resources {
		current, err := sandboxUnstructured(info.Object)
		if err != nil {
			return err
		}
		key := sandboxResourceKey(info)
		previous, exists := k.run.objects[key]
		if !exists || current.GetUID() == "" || current.GetResourceVersion() == "" ||
			current.GetName() != info.Name || current.GetNamespace() != info.Namespace ||
			(previous != nil && current.GetUID() != previous.GetUID()) {
			return fmt.Errorf("Sandbox write returned an unexpected resource identity")
		}
		k.run.objects[key] = current
	}
	return k.run.preflight(true, true)
}

func (k *sandboxStageKube) Delete(kube.ResourceList) (*kube.Result, []error) {
	return &kube.Result{}, []error{fmt.Errorf("Sandbox staging cannot invoke lifecycle cleanup")}
}

type sandboxStageDriver struct {
	driver.Driver
	run      *sandboxStageRun
	mu       sync.Mutex
	writeErr error
}

func (d *sandboxStageDriver) record(err error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeErr = errors.Join(d.writeErr, err)
	return err
}

func (d *sandboxStageDriver) err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeErr
}

func (d *sandboxStageDriver) Create(key string, rel *release.Release) error {
	status := release.StatusPendingInstall
	if d.run.revision != 1 {
		status = release.StatusPendingUpgrade
	}
	if err := d.run.exactRelease(rel, status); err != nil {
		return d.record(err)
	}
	if err := d.run.preflight(true, false); err != nil {
		return d.record(err)
	}
	if err := d.run.unchangedPrevious(); err != nil {
		return d.record(err)
	}
	return d.record(d.Driver.Create(key, rel))
}

func (d *sandboxStageDriver) Update(key string, rel *release.Release) error {
	if err := d.run.ctx.Err(); err != nil {
		return d.record(err)
	}
	current, err := d.Driver.Get(key)
	if err != nil {
		return d.record(err)
	}
	expected, err := sandboxReleaseSeal(current)
	if err != nil {
		return d.record(err)
	}
	actual, err := sandboxReleaseSeal(rel)
	if err != nil || expected.Digest != actual.Digest || !actual.sameOwner(d.run.seal) ||
		rel.Version != current.Version || rel.Name != current.Name || rel.Namespace != current.Namespace ||
		rel.Info == nil {
		return d.record(fmt.Errorf("Sandbox release changed before status recording"))
	}
	if rel.Version == d.run.revision {
		status := release.StatusPendingInstall
		if d.run.revision != 1 {
			status = release.StatusPendingUpgrade
		}
		if actual.Digest != d.run.seal.Digest ||
			current.Info == nil || current.Info.Status != status ||
			(rel.Info.Status != release.StatusDeployed && rel.Info.Status != release.StatusFailed) {
			return d.record(fmt.Errorf("unexpected Sandbox stage status transition"))
		}
		if rel.Info.Status == release.StatusDeployed {
			if err := d.run.preflight(true, true); err != nil {
				// Recording a failed held stage permits an exact-plan retry.
				// It never rolls back resources or grants execution.
				failed := *rel
				info := *rel.Info
				info.Status, info.Description = release.StatusFailed, "Sandbox stage postconditions or hold check failed"
				failed.Info = &info
				return d.record(errors.Join(err, d.Driver.Update(key, &failed)))
			}
		}
	} else {
		expected, ok := d.run.previousHashes[rel.Version]
		if !ok || sandboxJSONHash(current) != expected || current.Info == nil ||
			(rel.Info.Status != release.StatusSuperseded && rel.Info.Status != current.Info.Status) {
			return d.record(fmt.Errorf("unexpected Sandbox baseline status transition"))
		}
	}
	return d.record(d.Driver.Update(key, rel))
}

func (d *sandboxStageDriver) Delete(string) (*release.Release, error) {
	return nil, d.record(fmt.Errorf("Sandbox staging cannot remove release history"))
}

type sandboxContextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t sandboxContextTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	response, err := t.base.RoundTrip(request.Clone(ctx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	response.Body = &sandboxContextBody{ReadCloser: response.Body, stop: stop, cancel: cancel}
	return response, nil
}

type sandboxContextBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
}

func (b *sandboxContextBody) Close() error {
	defer b.cancel()
	defer b.stop()
	return b.ReadCloser.Close()
}
