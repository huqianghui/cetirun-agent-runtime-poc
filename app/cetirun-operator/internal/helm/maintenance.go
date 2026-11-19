// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

const (
	maintenanceTimeout       = 30 * time.Second
	maintenanceHistoryLimit  = 100
	maintenanceManifestLimit = 4 << 20
	maintenanceObjectLimit   = 256
)

var ErrMaintenanceConflict = errors.New("Helm maintenance release precondition changed")

var ErrMaintenanceNotSubmitted = errors.New("Helm maintenance failed before submitting any write")

// CurrentRelease returns the latest stored revision, including its manifest and
// hooks. Its contents are internal recovery input, not log or Event payloads.
func (c *Client) CurrentRelease(ctx context.Context, namespace, name string) (*release.Release, error) {
	history, err := c.ReleaseHistory(ctx, namespace, name, 1)
	if err != nil {
		return nil, err
	}
	return history[0], nil
}

// ReleaseHistory returns newest first, with at most limit entries. Inspection
// fails rather than truncating an unknown inventory beyond 100 stored revisions.
func (c *Client) ReleaseHistory(ctx context.Context, namespace, name string, limit int) ([]*release.Release, error) {
	if err := maintenanceReadArguments(ctx, namespace, name, limit); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, maintenanceTimeout)
	defer cancel()
	cfg, err := maintenanceActionConfig(ctx, namespace, nil)
	if err != nil {
		return nil, err
	}
	return readReleaseHistory(ctx, cfg, namespace, name, limit)
}

func maintenanceReadArguments(ctx context.Context, namespace, name string, limit int) error {
	if ctx == nil || len(validation.IsDNS1123Label(namespace)) != 0 || chartutil.ValidateReleaseName(name) != nil ||
		limit < 1 || limit > maintenanceHistoryLimit {
		return fmt.Errorf("release inspection requires a context, namespace, release and history limit from 1 to 100")
	}
	return ctx.Err()
}

func readReleaseHistory(ctx context.Context, cfg *action.Configuration, namespace, name string, limit int) ([]*release.Release, error) {
	if err := maintenanceReadArguments(ctx, namespace, name, limit); err != nil {
		return nil, err
	}
	history, err := action.NewHistory(cfg).Run(name)
	if err != nil {
		return nil, fmt.Errorf("read Helm release history: %w", err)
	}
	if len(history) == 0 || len(history) > maintenanceHistoryLimit {
		return nil, fmt.Errorf("Helm release history is empty or exceeds the inspection bound")
	}
	seen := make(map[int]bool, len(history))
	for _, rel := range history {
		if rel == nil || rel.Info == nil || rel.Info.Status == "" || rel.Name != name || rel.Namespace != namespace || rel.Version < 1 || seen[rel.Version] {
			return nil, fmt.Errorf("Helm history contains an invalid or mismatched release identity")
		}
		seen[rel.Version] = true
		if err := maintenanceContentBound(rel); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(history, func(a, b *release.Release) int { return b.Version - a.Version })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return history[:min(limit, len(history))], nil
}

func maintenanceContentBound(rel *release.Release) error {
	size := len(rel.Manifest)
	for _, hook := range rel.Hooks {
		if hook == nil {
			return fmt.Errorf("Helm history contains an invalid hook")
		}
		size += len(hook.Manifest)
	}
	if size > maintenanceManifestLimit || len(rel.Hooks) > maintenanceObjectLimit {
		return fmt.Errorf("Helm manifest or hooks exceed the inspection bound")
	}
	return nil
}

// No registry client or registry credential lookup is needed for stored releases
// or an already-loaded PreparedRelease. Bind SDK background requests to ctx.
func maintenanceActionConfig(ctx context.Context, namespace string, guard func(context.Context) error) (*action.Configuration, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	config.Timeout = maintenanceTimeout
	config.Wrap(func(base http.RoundTripper) http.RoundTripper {
		return maintenanceTransport{ctx: ctx, base: base, guard: guard}
	})
	cfg := new(action.Configuration)
	if err := cfg.Init(restClientGetter(config, namespace), namespace, "secret", func(string, ...any) {}); err != nil {
		return nil, err
	}
	client, err := cfg.KubernetesClientSet()
	if err != nil {
		return nil, err
	}
	cfg.Releases = storage.Init(&boundedReleaseSecrets{Driver: cfg.Releases.Driver, ctx: ctx, secrets: client.CoreV1().Secrets(namespace)})
	return cfg, nil
}

type maintenanceTransport struct {
	ctx   context.Context
	base  http.RoundTripper
	guard func(context.Context) error
}

func (t maintenanceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	if t.guard != nil && (request.Method == http.MethodPost || request.Method == http.MethodPut || request.Method == http.MethodPatch || request.Method == http.MethodDelete) {
		if err := t.guard(t.ctx); err != nil {
			return nil, err
		}
		if request.Method == http.MethodDelete {
			return nil, fmt.Errorf("maintenance cannot delete Kubernetes resources")
		}
	}
	return t.base.RoundTrip(request.WithContext(t.ctx))
}

type boundedReleaseSecrets struct {
	driver.Driver
	ctx     context.Context
	secrets typedcore.SecretInterface
}

func (d *boundedReleaseSecrets) Query(selector map[string]string) ([]*release.Release, error) {
	if selector["owner"] != "helm" || chartutil.ValidateReleaseName(selector["name"]) != nil {
		return nil, fmt.Errorf("Helm inspection requires one named release")
	}
	list, err := d.secrets.List(d.ctx, metav1.ListOptions{LabelSelector: labels.Set(selector).String(), Limit: maintenanceHistoryLimit + 1})
	if err != nil {
		return nil, err
	}
	if list.Continue != "" || len(list.Items) > maintenanceHistoryLimit {
		return nil, fmt.Errorf("Helm history exceeds the inspection bound")
	}
	if len(list.Items) == 0 {
		return nil, driver.ErrReleaseNotFound
	}
	result := make([]*release.Release, 0, len(list.Items))
	for _, secret := range list.Items {
		if secret.Type != corev1.SecretType("helm.sh/release.v1") {
			return nil, fmt.Errorf("Helm history contains a non-release Secret")
		}
		// SDK Query silently skips decode errors. Exact Get propagates them.
		rel, err := exactStoredRelease(d.Driver, secret.Name)
		if err != nil {
			return nil, fmt.Errorf("read exact Helm revision: %w", err)
		}
		if rel == nil || rel.Info == nil || rel.Name != selector["name"] || rel.Namespace != secret.Namespace ||
			secret.Name != fmt.Sprintf("%s.%s.v%d", storage.HelmStorageType, rel.Name, rel.Version) ||
			secret.Labels["version"] != strconv.Itoa(rel.Version) || secret.Labels["status"] != string(rel.Info.Status) {
			return nil, fmt.Errorf("Helm revision metadata does not match its payload")
		}
		result = append(result, rel)
	}
	return result, nil
}

// Helm 3.22.1 Secrets.Get dereferences the decode result even on error.
// Keep a malformed stored release from panicking the controller's read path.
func exactStoredRelease(store driver.Driver, key string) (rel *release.Release, err error) {
	defer func() {
		if recover() != nil {
			rel, err = nil, fmt.Errorf("Helm release decoder rejected malformed storage")
		}
	}()
	return store.Get(key)
}

// InstallOrUpgradeMaintenance reapplies an existing ordinary release while the
// caller holds its current Agent/namespace startup fence. It never installs a
// missing release, runs hooks, prunes history, or resumes the Agent.
func (c *Client) InstallOrUpgradeMaintenance(ctx context.Context, namespace, name string, prepared *PreparedRelease, expectedRevision int, guard func(context.Context) error) error {
	return c.installOrUpgradeMaintenance(ctx, namespace, name, prepared, expectedRevision, guard,
		func(ctx context.Context, namespace string, beforeWrite func(context.Context) error) (*action.Configuration, error) {
			return maintenanceActionConfig(ctx, namespace, beforeWrite)
		})
}

func (c *Client) installOrUpgradeMaintenance(ctx context.Context, namespace, name string, prepared *PreparedRelease, expectedRevision int,
	guard func(context.Context) error, configure func(context.Context, string, func(context.Context) error) (*action.Configuration, error),
) error {
	if err := maintenanceReadArguments(ctx, namespace, name, 1); err != nil {
		return errors.Join(ErrMaintenanceNotSubmitted, err)
	}
	if guard == nil || expectedRevision < 1 || prepared == nil || prepared.chart == nil || prepared.namespace != namespace ||
		prepared.releaseName != name || !sameJSON(prepared.Values["replicaCount"], 0) ||
		prepared.sandboxBundle != nil || prepared.sandboxReconcile != nil || prepared.Values["sandboxWorkload"] != nil {
		return errors.Join(ErrMaintenanceNotSubmitted, configurationError(fmt.Errorf("maintenance requires a bound ordinary zero-replica preparation, a positive revision and a guard")))
	}
	ctx, cancel := context.WithTimeout(ctx, maintenanceTimeout)
	defer cancel()
	copy := *prepared
	mode := &maintenanceMode{ctx: ctx, guard: guard, source: prepared, input: prepared.inputDigest(), namespace: namespace, name: name, revision: expectedRevision}
	copy.maintenance = mode
	err := c.installOrUpgrade(namespace, name, &copy, func(namespace string) (*action.Configuration, error) {
		return configure(ctx, namespace, func(context.Context) error { return mode.check() })
	})
	return mode.result(err)
}

type maintenanceMode struct {
	ctx                    context.Context
	guard                  func(context.Context) error
	source                 *PreparedRelease
	input, namespace, name string
	revision               int
	latest, base           *release.Release
	objects                map[string]*unstructured.Unstructured
	mutable                map[string]bool
	hooks                  []*release.Hook
	mu                     sync.Mutex
	err                    error
	writeAttempted         bool
}

func (m *maintenanceMode) result(err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err = errors.Join(err, m.err)
	if err != nil {
		m.err = err
		if !m.writeAttempted {
			return errors.Join(ErrMaintenanceNotSubmitted, err)
		}
	}
	return err
}

func (m *maintenanceMode) startWrite() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.writeAttempted = true
	return nil
}

func (m *maintenanceMode) failure() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *maintenanceMode) stop(err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err == nil {
		m.err = err
	}
	return err
}

func (m *maintenanceMode) check() error {
	if err := m.failure(); err != nil {
		return err
	}
	if err := m.ctx.Err(); err != nil {
		return m.stop(err)
	}
	if err := m.guard(m.ctx); err != nil {
		return m.stop(err)
	}
	if m.input == "" || m.input != m.source.inputDigest() {
		return m.stop(fmt.Errorf("maintenance preparation changed after validation"))
	}
	return m.ctx.Err()
}

func (m *maintenanceMode) prepare(cfg *action.Configuration, p *PreparedRelease) (*action.Configuration, error) {
	guarded := *cfg
	guarded.Log = func(string, ...any) {}
	guarded.Releases = storage.Init(&maintenanceDriver{Driver: cfg.Releases.Driver, mode: m})
	cfg = &guarded
	history, err := readReleaseHistory(m.ctx, cfg, m.namespace, m.name, maintenanceHistoryLimit)
	if err != nil {
		return nil, err
	}
	if len(history) == maintenanceHistoryLimit {
		return nil, configurationError(fmt.Errorf("maintenance history has no room for a bounded next-revision readback"))
	}
	m.latest = history[0]
	if m.latest.Version != m.revision || (m.latest.Info.Status != release.StatusDeployed && m.latest.Info.Status != release.StatusFailed) {
		return nil, ErrMaintenanceConflict
	}
	m.base = m.latest
	if m.latest.Info.Status == release.StatusFailed {
		for _, rel := range history {
			if rel.Info.Status == release.StatusDeployed {
				m.base = rel
				break
			}
		}
	}
	if len(p.chart.CRDObjects()) != 0 {
		return nil, configurationError(fmt.Errorf("maintenance cannot apply CRDs"))
	}
	chrt := p.chart
	if p.custom {
		if p.actionRevision != m.revision+1 || !p.ownsRelease(m.latest, m.namespace, m.name) {
			return nil, ErrMaintenanceConflict
		}
		chrt = p.frozenChart()
	}
	upgrade := action.NewUpgrade(cfg)
	upgrade.Namespace, upgrade.DisableHooks, upgrade.MaxHistory = m.namespace, true, 0
	upgrade.DryRun, upgrade.DryRunOption = true, "client"
	planned, err := upgrade.RunWithContext(m.ctx, m.name, chrt, p.Values)
	if err != nil {
		return nil, fmt.Errorf("maintenance Helm preflight failed: %w", err)
	}
	if planned.Version != m.revision+1 || planned.Namespace != m.namespace || planned.Name != m.name {
		return nil, ErrMaintenanceConflict
	}
	if err := maintenanceContentBound(planned); err != nil {
		return nil, configurationError(err)
	}
	m.objects, err = maintenanceObjects(planned.Manifest, m.namespace)
	if err != nil {
		return nil, configurationError(err)
	}
	for _, previous := range []*release.Release{m.latest, m.base} {
		if err := maintenanceCompatible(previous, planned, m.objects, m.namespace); err != nil {
			return nil, configurationError(err)
		}
	}
	if err := m.inspectRestoration(cfg.KubeClient, planned.Manifest); err != nil {
		return nil, err
	}
	m.mutable = make(map[string]bool)
	for key, obj := range m.objects {
		m.mutable[key] = maintenanceMutable(obj)
	}
	m.hooks = planned.Hooks
	var frozen bytes.Buffer
	frozen.WriteString(planned.Manifest)
	for _, hook := range planned.Hooks {
		frozen.WriteString("\n---\n")
		frozen.WriteString(hook.Manifest)
	}
	p.manifest = frozen.Bytes()
	p.chart = p.frozenChart()
	if err := m.check(); err != nil {
		return nil, err
	}
	cfg.KubeClient = &maintenanceKube{Interface: cfg.KubeClient, mode: m}
	return cfg, nil
}

func (m *maintenanceMode) verify(cfg *action.Configuration) error {
	if err := m.check(); err != nil {
		return err
	}
	history, err := readReleaseHistory(m.ctx, cfg, m.namespace, m.name, maintenanceHistoryLimit)
	if err != nil {
		return err
	}
	last := history[0]
	actual, err := maintenanceObjects(last.Manifest, m.namespace)
	if err != nil {
		return err
	}
	expectedHooks, err := maintenanceHooks(m.hooks, m.namespace)
	if err != nil {
		return err
	}
	actualHooks, err := maintenanceHooks(last.Hooks, m.namespace)
	if err != nil {
		return err
	}
	if last.Version != m.revision+1 || last.Info.Status != release.StatusDeployed ||
		!sameJSON(actual, m.objects) || !sameJSON(last.Config, m.source.Values) || !sameJSON(expectedHooks, actualHooks) {
		return fmt.Errorf("maintenance result did not match the prepared zero-replica release")
	}
	return nil
}

func maintenanceObjects(manifest, namespace string) (map[string]*unstructured.Unstructured, error) {
	if len(manifest) > maintenanceManifestLimit {
		return nil, fmt.Errorf("maintenance manifest exceeds its size bound")
	}
	reader := kubeyaml.NewYAMLReader(bufio.NewReader(bytes.NewBufferString(manifest)))
	objects := make(map[string]*unstructured.Unstructured)
	for {
		document, err := reader.Read()
		if err == io.EOF {
			return objects, nil
		}
		if err != nil || validateYAMLKeys(document) != nil {
			return nil, fmt.Errorf("invalid maintenance manifest")
		}
		data, err := yaml.YAMLToJSONStrict(document)
		if err != nil {
			return nil, fmt.Errorf("invalid maintenance manifest")
		}
		obj := new(unstructured.Unstructured)
		if err := obj.UnmarshalJSON(data); err != nil {
			return nil, fmt.Errorf("invalid maintenance object")
		}
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetName() == "" || obj.GetGenerateName() != "" || obj.GetAPIVersion() == "" || obj.GetKind() == "" ||
			(obj.GetNamespace() != "" && obj.GetNamespace() != namespace) || obj.GetKind() == "List" {
			return nil, fmt.Errorf("maintenance object lacks an exact release-local identity")
		}
		key := maintenanceObjectKey(obj, namespace)
		if objects[key] != nil || len(objects) == maintenanceObjectLimit {
			return nil, fmt.Errorf("duplicate or excessive maintenance objects")
		}
		objects[key] = obj
	}
}

func maintenanceObjectKey(obj *unstructured.Unstructured, namespace string) string {
	if obj.GetNamespace() != "" {
		namespace = obj.GetNamespace()
	}
	return obj.GetAPIVersion() + "/" + obj.GetKind() + "/" + namespace + "/" + obj.GetName()
}

func maintenanceMutable(obj *unstructured.Unstructured) bool {
	switch obj.GetAPIVersion() + "/" + obj.GetKind() {
	case "apps/v1/StatefulSet", "apps/v1/Deployment", "v1/ConfigMap", "v1/Service":
		return true
	default:
		return false
	}
}

func maintenanceCompatible(previous, planned *release.Release, target map[string]*unstructured.Unstructured, namespace string) error {
	original, err := maintenanceObjects(previous.Manifest, namespace)
	if err != nil {
		return err
	}
	if len(original) != len(target) {
		return fmt.Errorf("maintenance cannot add or remove release objects")
	}
	oldHooks, err := maintenanceHooks(previous.Hooks, namespace)
	if err != nil {
		return err
	}
	newHooks, err := maintenanceHooks(planned.Hooks, namespace)
	if err != nil {
		return err
	}
	if !sameJSON(oldHooks, newHooks) {
		return fmt.Errorf("maintenance requires unchanged hook definitions")
	}
	workloads := 0
	for key, next := range target {
		old := original[key]
		if old == nil {
			return fmt.Errorf("maintenance cannot change release object identities")
		}
		left, right := old.DeepCopy(), next.DeepCopy()
		switch next.GetAPIVersion() + "/" + next.GetKind() {
		case "apps/v1/StatefulSet", "apps/v1/Deployment":
			workloads++
			replicas, found, err := unstructured.NestedInt64(next.Object, "spec", "replicas")
			if err != nil || !found || replicas != 0 {
				return fmt.Errorf("maintenance workloads must explicitly have zero replicas")
			}
			oldAccess, err := maintenanceContainerAccess(old)
			if err != nil {
				return err
			}
			newAccess, err := maintenanceContainerAccess(next)
			if err != nil {
				return err
			}
			if !sameJSON(oldAccess, newAccess) {
				return fmt.Errorf("maintenance cannot change container privileges, mounts or credential references")
			}
			for _, field := range []string{"serviceAccountName", "automountServiceAccountToken", "volumes", "imagePullSecrets", "securityContext", "hostNetwork", "hostPID", "hostIPC", "shareProcessNamespace"} {
				a, _, _ := unstructured.NestedFieldNoCopy(old.Object, "spec", "template", "spec", field)
				b, _, _ := unstructured.NestedFieldNoCopy(next.Object, "spec", "template", "spec", field)
				if !sameJSON(a, b) {
					return fmt.Errorf("maintenance cannot change workload identity or storage")
				}
			}
			for _, field := range []string{"replicas", "template", "updateStrategy", "strategy", "revisionHistoryLimit", "minReadySeconds", "progressDeadlineSeconds", "paused"} {
				unstructured.RemoveNestedField(left.Object, "spec", field)
				unstructured.RemoveNestedField(right.Object, "spec", field)
			}
			// Labels and rollout checksums may follow the authoritative runtime.
			for _, field := range []string{"labels", "annotations"} {
				unstructured.RemoveNestedField(left.Object, "metadata", field)
				unstructured.RemoveNestedField(right.Object, "metadata", field)
			}
		case "v1/ConfigMap":
			if old.Object["immutable"] == true || next.Object["immutable"] == true {
				if !sameJSON(old, next) {
					return fmt.Errorf("maintenance cannot change immutable ConfigMap data")
				}
			}
			for _, field := range []string{"data", "binaryData"} {
				delete(left.Object, field)
				delete(right.Object, field)
			}
		case "v1/Service":
			if t, _, _ := unstructured.NestedString(next.Object, "spec", "type"); t != "" && t != "ClusterIP" {
				return fmt.Errorf("maintenance requires an existing private Service")
			}
			if external, _, _ := unstructured.NestedSlice(next.Object, "spec", "externalIPs"); len(external) != 0 {
				return fmt.Errorf("maintenance cannot expose an external Service address")
			}
			unstructured.RemoveNestedField(left.Object, "spec", "ports")
			unstructured.RemoveNestedField(right.Object, "spec", "ports")
		case "v1/Pod", "apps/v1/ReplicaSet", "apps/v1/DaemonSet", "batch/v1/Job", "batch/v1/CronJob":
			return fmt.Errorf("maintenance cannot manage an independently starting workload")
		}
		if !sameJSON(left, right) {
			return fmt.Errorf("maintenance cannot change protected resources or immutable workload/storage fields")
		}
	}
	if workloads == 0 {
		return fmt.Errorf("maintenance release has no supported zero-replica workload")
	}
	return nil
}

func maintenanceContainerAccess(obj *unstructured.Unstructured) (map[string]any, error) {
	access := make(map[string]any)
	for _, group := range []string{"containers", "initContainers", "ephemeralContainers"} {
		containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", group)
		if err != nil {
			return nil, fmt.Errorf("invalid maintenance container list")
		}
		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			name, named := container["name"].(string)
			key := group + "/" + name
			if !ok || !named || name == "" || access[key] != nil {
				return nil, fmt.Errorf("invalid maintenance container identity")
			}
			refs := make(map[string]any)
			if env, exists := container["env"]; exists {
				entries, ok := env.([]any)
				if !ok {
					return nil, fmt.Errorf("invalid maintenance container environment")
				}
				for _, entry := range entries {
					variable, ok := entry.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("invalid maintenance container environment")
					}
					if variable["valueFrom"] != nil {
						refs[text(variable["name"])] = variable["valueFrom"]
					}
				}
			}
			access[key] = []any{container["securityContext"], container["volumeMounts"], container["volumeDevices"], container["envFrom"], refs}
		}
	}
	return access, nil
}

func maintenanceHooks(hooks []*release.Hook, namespace string) (map[string]any, error) {
	result := make(map[string]any, len(hooks))
	for _, hook := range hooks {
		if hook == nil {
			return nil, fmt.Errorf("invalid maintenance hook")
		}
		objects, err := maintenanceObjects(hook.Manifest, namespace)
		if err != nil {
			return nil, err
		}
		copy := *hook
		copy.Path, copy.Manifest, copy.LastRun = "", "", release.HookExecution{}
		copy.DeletePolicies = slices.Clone(hook.DeletePolicies)
		if len(copy.DeletePolicies) == 0 {
			copy.DeletePolicies = []release.HookDeletePolicy{release.HookBeforeHookCreation}
		}
		slices.Sort(copy.DeletePolicies)
		key := hook.Kind + "/" + hook.Name
		if result[key] != nil {
			return nil, fmt.Errorf("duplicate maintenance hooks")
		}
		result[key] = []any{copy, objects}
	}
	return result, nil
}

type maintenanceDriver struct {
	driver.Driver
	mode *maintenanceMode
}

func (d *maintenanceDriver) Query(selector map[string]string) ([]*release.Release, error) {
	records, err := d.Driver.Query(selector)
	if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		d.mode.stop(err)
	}
	return records, err
}

func (d *maintenanceDriver) Create(key string, rel *release.Release) error {
	if err := d.mode.check(); err != nil {
		return err
	}
	history, err := d.Driver.Query(map[string]string{"name": d.mode.name, "owner": "helm"})
	if err != nil {
		return d.mode.stop(err)
	}
	slices.SortFunc(history, func(a, b *release.Release) int { return b.Version - a.Version })
	if len(history) == 0 || !sameJSON(history[0], d.mode.latest) || rel.Name != d.mode.name || rel.Namespace != d.mode.namespace || rel.Version != d.mode.revision+1 {
		return d.mode.stop(ErrMaintenanceConflict)
	}
	if err := d.mode.startWrite(); err != nil {
		return err
	}
	return d.mode.stop(d.Driver.Create(key, rel))
}

func (d *maintenanceDriver) Update(key string, rel *release.Release) error {
	if err := d.mode.check(); err != nil {
		return err
	}
	if rel.Name != d.mode.name || rel.Namespace != d.mode.namespace || (rel.Version != d.mode.base.Version && rel.Version != d.mode.revision+1) {
		return d.mode.stop(ErrMaintenanceConflict)
	}
	if err := d.mode.startWrite(); err != nil {
		return err
	}
	return d.mode.stop(d.Driver.Update(key, rel))
}

func (d *maintenanceDriver) Delete(string) (*release.Release, error) {
	return nil, d.mode.stop(fmt.Errorf("maintenance cannot delete Helm history"))
}

type maintenanceKube struct {
	kube.Interface
	mode *maintenanceMode
}

func (k *maintenanceKube) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	if err := k.mode.check(); err != nil {
		return &kube.Result{}, err
	}
	if force {
		return &kube.Result{}, fmt.Errorf("maintenance cannot force resource replacement")
	}
	filter := func(resources kube.ResourceList) kube.ResourceList {
		var selected kube.ResourceList
		for _, info := range resources {
			obj, ok := info.Object.(*unstructured.Unstructured)
			if ok && k.mode.mutable[maintenanceObjectKey(obj, k.mode.namespace)] {
				selected = append(selected, info)
			}
		}
		return selected
	}
	// Unchanged RBAC, identities, policies, Secrets and storage remain in Helm's
	// inventory but are never sent through its three-way update/recreation path.
	if err := k.mode.startWrite(); err != nil {
		return &kube.Result{}, err
	}
	return k.Interface.Update(filter(original), filter(target), false)
}

func (k *maintenanceKube) Create(kube.ResourceList) (*kube.Result, error) {
	return &kube.Result{}, k.mode.stop(fmt.Errorf("maintenance cannot create hooks or a new release"))
}

func (k *maintenanceKube) Delete(kube.ResourceList) (*kube.Result, []error) {
	return &kube.Result{}, []error{k.mode.stop(fmt.Errorf("maintenance cannot delete release resources"))}
}
