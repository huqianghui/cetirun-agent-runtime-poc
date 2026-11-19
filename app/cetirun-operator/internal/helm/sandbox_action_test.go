// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/resource"
	restfake "k8s.io/client-go/rest/fake"
	"k8s.io/utils/ptr"
)

var errSandboxActionTest = errors.New("synthetic held action failure")

func TestSandboxStageDigestIsReadOnlyAndDetectsReleaseDrift(t *testing.T) {
	prepared, _, _ := sandboxRender(t, "openclaw", true, false)
	cfg, kubeClient, store := sandboxActionConfig()
	input := prepared.sandboxBundle.Identity()
	ctx := context.Background()
	if _, err := sandboxStageDigest(ctx, cfg, input.Namespace.Name, input.ReleaseName); err == nil {
		t.Fatal("missing release returned a usable identity")
	}
	if err := prepared.stageSandbox(ctx, cfg, input.Namespace.Name, input.ReleaseName, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	writes, reads := store.writes, kubeClient.reads
	first, err := sandboxStageDigest(ctx, cfg, input.Namespace.Name, input.ReleaseName)
	if err != nil || len(first) != 64 {
		t.Fatalf("deployed release identity unavailable: %q %v", first, err)
	}
	second, err := sandboxStageDigest(ctx, cfg, input.Namespace.Name, input.ReleaseName)
	if err != nil || first != second || store.writes != writes || kubeClient.reads != reads {
		t.Fatal("read-only release inspection wrote state, read workload objects or returned an unstable identity")
	}
	current, err := cfg.Releases.Last(input.ReleaseName)
	if err != nil {
		t.Fatal(err)
	}
	current.Info.Description = "synthetic changed release metadata"
	if err := cfg.Releases.Update(current); err != nil {
		t.Fatal(err)
	}
	changed, err := sandboxStageDigest(ctx, cfg, input.Namespace.Name, input.ReleaseName)
	if err != nil || changed == first {
		t.Fatal("release-store drift was hidden")
	}
	for _, status := range []release.Status{release.StatusFailed, release.StatusPendingUpgrade, release.StatusUninstalled} {
		current.Info.Status = status
		if err := cfg.Releases.Update(current); err != nil {
			t.Fatal(err)
		}
		if _, err := sandboxStageDigest(ctx, cfg, input.Namespace.Name, input.ReleaseName); err == nil {
			t.Fatalf("release status %s was reusable", status)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := sandboxStageDigest(canceled, cfg, input.Namespace.Name, input.ReleaseName); !errors.Is(err, context.Canceled) {
		t.Fatal("release inspection ignored cancellation:", err)
	}
	if _, err := sandboxStageDigest(ctx, nil, input.Namespace.Name, input.ReleaseName); err == nil {
		t.Fatal("missing release store was accepted")
	}
	if _, err := (&Client{}).SandboxStageDigest(nil, "", ""); err == nil {
		t.Fatal("invalid release inspection reached configuration loading")
	}
}

// Unlike driver.Memory's pointer storage, API-backed Helm storage serializes
// releases. Copies also make final-status write failures observable in tests.
type sandboxActionStorage struct {
	driver.Driver
	writes        int
	queryError    error
	createError   error
	deployedError error
}

func sandboxReleaseCopy(rel *release.Release) *release.Release {
	if rel == nil {
		return nil
	}
	raw, err := json.Marshal(rel)
	if err != nil {
		panic(err)
	}
	var out release.Release
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return &out
}

func (s *sandboxActionStorage) Create(key string, rel *release.Release) error {
	s.writes++
	if s.createError != nil {
		return s.createError
	}
	return s.Driver.Create(key, sandboxReleaseCopy(rel))
}

func (s *sandboxActionStorage) Update(key string, rel *release.Release) error {
	s.writes++
	if rel.Info.Status == release.StatusDeployed && s.deployedError != nil {
		return s.deployedError
	}
	return s.Driver.Update(key, sandboxReleaseCopy(rel))
}

func (s *sandboxActionStorage) Get(key string) (*release.Release, error) {
	rel, err := s.Driver.Get(key)
	return sandboxReleaseCopy(rel), err
}

func (s *sandboxActionStorage) Query(labels map[string]string) ([]*release.Release, error) {
	if s.queryError != nil {
		return nil, s.queryError
	}
	records, err := s.Driver.Query(labels)
	out := make([]*release.Release, 0, len(records))
	for _, rel := range records {
		out = append(out, sandboxReleaseCopy(rel))
	}
	return out, err
}

type sandboxActionKube struct {
	kubefake.PrintingKubeClient
	mu         sync.Mutex
	objects    map[string]*unstructured.Unstructured
	writes     []*unstructured.Unstructured
	reads      int
	failAt     int
	readError  error
	afterWrite func()
	afterRead  func()
	skipWrites bool
	hooks      int
	waits      int
	deletes    int
}

func sandboxActionConfig() (*action.Configuration, *sandboxActionKube, *sandboxActionStorage) {
	cfg := customActionConfig()
	k := &sandboxActionKube{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
		objects:            map[string]*unstructured.Unstructured{},
	}
	mem := driver.NewMemory()
	mem.SetNamespace("cetirun-example")
	store := &sandboxActionStorage{Driver: mem}
	cfg.KubeClient, cfg.Releases = k, storage.Init(store)
	return cfg, k, store
}

func sandboxTestMapping(obj *unstructured.Unstructured) *meta.RESTMapping {
	gvk := obj.GroupVersionKind()
	gvr, _ := meta.UnsafeGuessKindToResource(gvk)
	scope := meta.RESTScopeNamespace
	if gvk.Kind == "ClusterRoleBinding" {
		scope = meta.RESTScopeRoot
	}
	return &meta.RESTMapping{Resource: gvr, GroupVersionKind: gvk, Scope: scope}
}

func sandboxTestPath(obj *unstructured.Unstructured) string {
	mapping := sandboxTestMapping(obj)
	base := "/apis/" + mapping.Resource.GroupVersion().String()
	if mapping.Resource.Group == "" {
		base = "/api/" + mapping.Resource.Version
	}
	if obj.GetNamespace() != "" {
		base += "/namespaces/" + obj.GetNamespace()
	}
	return base + "/" + mapping.Resource.Resource + "/" + obj.GetName()
}

func (k *sandboxActionKube) Build(reader io.Reader, _ bool) (kube.ResourceList, error) {
	decoder := kubeyaml.NewYAMLOrJSONDecoder(reader, 4096)
	var resources kube.ResourceList
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err == io.EOF {
			return resources, nil
		} else if err != nil {
			return nil, err
		}
		if len(obj.Object) == 0 {
			continue
		}
		mapping := sandboxTestMapping(&obj)
		apiPath := "/apis/" + mapping.Resource.GroupVersion().String()
		if mapping.Resource.Group == "" {
			apiPath = "/api/" + mapping.Resource.Version
		}
		client := &restfake.RESTClient{
			GroupVersion: mapping.Resource.GroupVersion(), VersionedAPIPath: apiPath,
			NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
			Client: restfake.CreateHTTPClient(func(request *http.Request) (*http.Response, error) {
				k.mu.Lock()
				defer k.mu.Unlock()
				if k.readError != nil {
					return nil, k.readError
				}
				if err := request.Context().Err(); err != nil {
					return nil, err
				}
				if request.Method == http.MethodPatch {
					current := k.objects[request.URL.Path]
					if current == nil {
						return sandboxSDKStatus(http.StatusNotFound, metav1.StatusReasonNotFound)
					}
					raw, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					updated, err := sandboxSDKMergePatch(current, raw, request.Header.Get("Content-Type"))
					if err != nil {
						return nil, err
					}
					if current.GetUID() != updated.GetUID() || current.GetResourceVersion() != updated.GetResourceVersion() {
						return sandboxSDKStatus(http.StatusConflict, metav1.StatusReasonConflict)
					}
					written, err := k.writeObject(updated)
					if err != nil {
						return nil, err
					}
					if k.afterWrite != nil {
						k.afterWrite()
					}
					return sandboxSDKResponse(http.StatusOK, written)
				}
				if request.Method != http.MethodGet {
					return nil, fmt.Errorf("unexpected fake API request %s", request.Method)
				}
				k.reads++
				status := http.StatusOK
				var body any = k.objects[request.URL.Path]
				if body.(*unstructured.Unstructured) == nil {
					status = http.StatusNotFound
					body = metav1.Status{
						TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
						Status:   metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound,
					}
				}
				raw, err := json.Marshal(body)
				if k.afterRead != nil {
					k.afterRead()
				}
				return &http.Response{
					StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(bytes.NewReader(raw)),
				}, err
			}),
		}
		resources = append(resources, &resource.Info{
			Client: client, Mapping: mapping, Namespace: obj.GetNamespace(), Name: obj.GetName(), Object: obj.DeepCopy(),
		})
	}
}

func (k *sandboxActionKube) apply(target kube.ResourceList) (*kube.Result, error) {
	result := &kube.Result{}
	for _, info := range target {
		k.mu.Lock()
		obj, err := k.writeObject(info.Object.(*unstructured.Unstructured))
		k.mu.Unlock()
		if err != nil {
			return result, err
		}
		if err := info.Refresh(obj, true); err != nil {
			return result, err
		}
		result.Created = append(result.Created, info)
		if k.afterWrite != nil {
			k.afterWrite()
		}
	}
	return result, nil
}

func (k *sandboxActionKube) writeObject(target *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if k.failAt > 0 {
		k.failAt--
		if k.failAt == 0 {
			return nil, errSandboxActionTest
		}
	}
	obj := target.DeepCopy()
	key := sandboxTestPath(obj)
	if current := k.objects[key]; current != nil {
		obj.SetUID(current.GetUID())
	} else {
		obj.SetUID(types.UID(fmt.Sprintf("synthetic-object-%d", len(k.writes)+1)))
	}
	obj.SetResourceVersion(strconv.Itoa(len(k.writes) + 1))
	k.writes = append(k.writes, obj.DeepCopy())
	if !k.skipWrites {
		k.objects[key] = obj
	}
	return obj, nil
}

func (k *sandboxActionKube) Create(target kube.ResourceList) (*kube.Result, error) {
	return k.apply(target)
}

func (k *sandboxActionKube) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	if force {
		return &kube.Result{}, fmt.Errorf("unexpected forced replacement")
	}
	result, err := k.apply(target)
	if err != nil {
		return result, err
	}
	for _, removed := range original.Difference(target) {
		k.mu.Lock()
		key := sandboxTestPath(removed.Object.(*unstructured.Unstructured))
		if k.objects[key] != nil {
			delete(k.objects, key)
			k.deletes++
		}
		k.mu.Unlock()
	}
	return result, nil
}

func (k *sandboxActionKube) WatchUntilReady(kube.ResourceList, time.Duration) error {
	k.hooks++
	return fmt.Errorf("Sandbox must not run hooks")
}

func (k *sandboxActionKube) Wait(kube.ResourceList, time.Duration) error {
	k.waits++
	return fmt.Errorf("Sandbox must not wait for execution")
}

func (k *sandboxActionKube) Delete(kube.ResourceList) (*kube.Result, []error) {
	k.deletes++
	return &kube.Result{}, []error{fmt.Errorf("Sandbox must not roll back or clean up")}
}

func sandboxStageCheck(context.Context) error { return nil }

func stageSandboxTest(p *PreparedRelease, cfg *action.Configuration, check func(context.Context) error) error {
	input := p.sandboxBundle.Identity()
	return p.stageSandbox(context.Background(), cfg, input.Namespace.Name, input.ReleaseName, check)
}

func driftSandboxTestConfig(k *sandboxActionKube) {
	for _, obj := range k.objects {
		if obj.GetKind() == "ConfigMap" {
			data := object(obj.Object["data"])
			for key := range data {
				data[key] = "synthetic live drift"
				return
			}
		}
	}
	panic("missing ConfigMap fixture")
}

func assertSandboxHeldWrites(t *testing.T, k *sandboxActionKube) {
	t.Helper()
	if k.hooks != 0 || k.waits != 0 || k.deletes != 0 {
		t.Fatalf("staging used hooks, readiness waits or cleanup: %d/%d/%d", k.hooks, k.waits, k.deletes)
	}
	seen := false
	for _, obj := range k.writes {
		switch obj.GetKind() {
		case "StatefulSet":
			seen = true
			var stateful appsv1.StatefulSet
			if err := decodeObject(obj, &stateful); err != nil || stateful.Spec.Replicas == nil ||
				*stateful.Spec.Replicas != 0 || len(stateful.Spec.VolumeClaimTemplates) != 0 {
				t.Fatalf("write lifted the hold or took over HOME: %v", err)
			}
		case "ConfigMap", "Service", "CiliumNetworkPolicy", "ServiceAccount", "RoleBinding", "ClusterRoleBinding":
		default:
			t.Fatalf("stage wrote execution, route or storage object %s", obj.GetKind())
		}
		owners := obj.GetOwnerReferences()
		if len(owners) != 1 || owners[0].UID != "agent-uid" || owners[0].Kind != "Agent" {
			t.Fatalf("staged object lacks its exact Agent owner: %s", obj.GetKind())
		}
	}
	if !seen {
		t.Fatal("no held StatefulSet was staged")
	}
}

func TestSandboxStageNativeFrozenInstallAndUpgrade(t *testing.T) {
	for _, typ := range []string{"openclaw"} {
		t.Run(typ, func(t *testing.T) {
			p, _, _ := sandboxRender(t, typ, true, true)
			sealed, err := p.SandboxManifest()
			if err != nil {
				t.Fatal(err)
			}
			// Late source changes, including hooks, CRDs, template delimiters
			// and action-dependent content, must never be executed.
			p.chart.Templates = append(p.chart.Templates, &chart.File{
				Name: "templates/late-hook.yaml", Data: []byte(`{{ fail "source template was re-executed" }}`),
			})
			p.chart.Files = append(p.chart.Files, &chart.File{Name: "crds/late.yaml", Data: []byte("kind: CustomResourceDefinition\n")})
			cfg, k, store := sandboxActionConfig()
			fixture := gatewayHeldFixture(typ)
			for _, claim := range []any{fixture.Input.Home, fixture.Input.Skills} {
				var obj unstructured.Unstructured
				raw, _ := json.Marshal(claim)
				if err := json.Unmarshal(raw, &obj.Object); err != nil {
					t.Fatal(err)
				}
				obj.SetAPIVersion("v1")
				obj.SetKind("PersistentVolumeClaim")
				k.objects[sandboxTestPath(&obj)] = obj.DeepCopy()
			}
			claimsBefore := len(k.objects)
			checks := 0
			check := func(context.Context) error { checks++; return nil }
			for revision := 1; revision <= 2; revision++ {
				if revision == 2 {
					driftSandboxTestConfig(k)
				}
				if err := stageSandboxTest(p, cfg, check); err != nil {
					t.Fatalf("stage revision %d: %v", revision, err)
				}
				last, err := cfg.Releases.Last(p.releaseName)
				if err != nil || last.Info.Status != release.StatusDeployed || last.Version != revision {
					t.Fatalf("held release not stored: %v", err)
				}
				if last.Manifest != string(sealed) || len(last.Hooks) != 0 || len(last.Chart.CRDObjects()) != 0 ||
					len(last.Chart.Dependencies()) != 0 || len(last.Chart.Templates) != 1 {
					t.Fatal("action altered the sealed bytes or retained source execution")
				}
				if _, err := sandboxReleaseSeal(last); err != nil {
					t.Fatalf("stage provenance was not durably stored: %v", err)
				}
			}
			if checks < 4 || k.reads == 0 || store.writes == 0 {
				t.Fatal("stage skipped authoritative guards, API reads or official storage")
			}
			objects, _ := sandboxStageObjects(sealed, sandboxSealIdentity(fixture.Input))
			if len(k.objects) != claimsBefore+len(objects) {
				t.Fatal("stage changed the pre-existing claim inventory")
			}
			for _, applied := range k.writes {
				approvedObject, exists := objects[sandboxObjectKey(*applied)]
				if !exists {
					t.Fatalf("unsealed object was applied: %s", applied.GetKind())
				}
				approved := approvedObject.DeepCopy()
				labels, annotations := approved.GetLabels(), approved.GetAnnotations()
				if labels == nil {
					labels = map[string]string{}
				}
				if annotations == nil {
					annotations = map[string]string{}
				}
				labels["app.kubernetes.io/managed-by"] = "Helm"
				annotations["meta.helm.sh/release-name"] = p.releaseName
				annotations["meta.helm.sh/release-namespace"] = fixture.Namespace.Name
				approved.SetLabels(labels)
				approved.SetAnnotations(annotations)
				if sandboxJSONHash(sandboxComparableObject(approved)) != sandboxJSONHash(sandboxComparableObject(applied)) {
					t.Fatalf("action altered sealed %s fields beyond Helm release tracking metadata", applied.GetKind())
				}
			}
			assertSandboxHeldWrites(t, k)
			if current, err := p.SandboxManifest(); err != nil || !bytes.Equal(current, sealed) {
				t.Fatal("staging changed the preparation")
			}
			if err := (&Client{}).InstallOrUpgrade(fixture.Namespace.Name, p.releaseName, p); !errors.Is(err, sandboxworkload.ErrExecutionHeld) {
				t.Fatalf("legacy installer stopped refusing Sandbox: %v", err)
			}
		})
	}
}

func TestSandboxStageRejectsOwnerlessSealedObjectsBeforeWrites(t *testing.T) {
	for _, kind := range []string{"ConfigMap", "Service"} {
		t.Run(kind, func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, false)
			objects, err := sandboxStageObjects(p.manifest, sandboxSealIdentity(p.sandboxBundle.Identity()))
			if err != nil {
				t.Fatal(err)
			}
			var manifest bytes.Buffer
			for _, obj := range objects {
				if obj.GetKind() == kind {
					obj.SetOwnerReferences(nil)
				}
				raw, err := json.Marshal(obj.Object)
				if err != nil {
					t.Fatal(err)
				}
				manifest.WriteString("---\n")
				manifest.Write(raw)
				manifest.WriteByte('\n')
			}
			// Even a self-consistent internal seal is insufficient when its
			// approved representation omitted protected Agent ownership.
			p.manifest = manifest.Bytes()
			p.manifestHash = sandboxBytesHash(p.manifest)
			p.WorkloadHash = p.manifestHash
			p.sealedInput = p.inputDigest()
			cfg, k, store := sandboxActionConfig()
			if err := stageSandboxTest(p, cfg, sandboxStageCheck); err == nil ||
				store.writes != 0 || len(k.writes) != 0 {
				t.Fatal("action repaired missing ownership outside the sealed manifest")
			}
		})
	}
}

func TestSandboxStageRejectsInvalidPreparationAndGuardBeforeWrites(t *testing.T) {
	for _, mode := range []string{
		"nil", "unsealed", "no bundle", "replicas", "hold", "agent", "manifest", "chart version",
		"namespace", "release", "nil guard", "guard fails", "guard mutates values", "forged stage values",
	} {
		t.Run(mode, func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, false)
			cfg, k, store := sandboxActionConfig()
			ns, name := p.sandboxBundle.Identity().Namespace.Name, p.releaseName
			check := sandboxStageCheck
			switch mode {
			case "nil":
				p = nil
			case "unsealed":
				p.sealedInput = ""
			case "no bundle":
				p.sandboxBundle = nil
			case "replicas":
				p.Values["replicaCount"] = 1
			case "hold":
				object(p.Values[sandboxworkload.ValuesKey])["executionHeld"] = false
			case "agent":
				p.Agent.Image = "example.com/changed:1"
			case "manifest":
				p.manifest = append(p.manifest, []byte("\n---\nkind: Pod\n")...)
			case "chart version":
				p.ChartVersion = "changed"
			case "namespace":
				ns = "other"
			case "release":
				name = "other"
			case "nil guard":
				check = nil
			case "guard fails":
				check = func(context.Context) error { return errSandboxActionTest }
			case "guard mutates values":
				check = func(context.Context) error { p.Values["replicaCount"] = 1; return nil }
			case "forged stage values":
				p.Values[sandboxStageConfigKey] = map[string]any{"version": "held-v1"}
				p.sealedInput = p.inputDigest()
			}
			if err := p.stageSandbox(context.Background(), cfg, ns, name, check); err == nil {
				t.Fatal("invalid preparation or guard was accepted")
			}
			if len(k.writes) != 0 || store.writes != 0 {
				t.Fatal("rejection happened after persistent writes")
			}
		})
	}
	p, _, _ := sandboxRender(t, "openclaw", true, false)
	if err := (&Client{}).StageSandbox(context.Background(), "cetirun-example", p.releaseName, p, nil); err == nil {
		t.Fatal("public staging accepted a nil guard")
	}
	if _, err := (&Client{}).PrepareSandbox("other", nil, nil, "", p.sandboxBundle); err == nil {
		t.Fatal("preparation loaded a chart for a mismatched namespace")
	}
}

func TestSandboxStageOwnedFailuresRetryWithoutExecution(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("upgrade=%v", upgrade), func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, true)
			cfg, k, _ := sandboxActionConfig()
			if upgrade {
				if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
					t.Fatal(err)
				}
				driftSandboxTestConfig(k)
			}
			k.failAt = 2
			if err := stageSandboxTest(p, cfg, sandboxStageCheck); !errors.Is(err, errSandboxActionTest) {
				t.Fatalf("official action failure was swallowed: %v", err)
			}
			last, _ := cfg.Releases.Last(p.releaseName)
			if last.Info.Status != release.StatusFailed {
				t.Fatalf("failed partial action was not recorded as failed: %s", last.Info.Status)
			}
			k.failAt = 0
			if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
				t.Fatalf("exact held failure could not be safely retried: %v", err)
			}
			assertSandboxHeldWrites(t, k)
		})
	}
}

func TestSandboxStageDoesNotHideStorageConflictsOrLostStatus(t *testing.T) {
	for _, mode := range []string{"name conflict", "lost final status", "missing objects"} {
		t.Run(mode, func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, false)
			cfg, k, store := sandboxActionConfig()
			switch mode {
			case "name conflict":
				store.createError = driver.ErrReleaseExists
			case "lost final status":
				store.deployedError = errSandboxActionTest
			case "missing objects":
				k.skipWrites = true
			}
			err := stageSandboxTest(p, cfg, sandboxStageCheck)
			if err == nil {
				t.Fatal("unsuccessful stage looked like success")
			}
			if mode == "name conflict" && (!errors.Is(err, driver.ErrReleaseExists) || len(k.writes) != 0) {
				t.Fatalf("name conflict was not returned before resource writes: %v", err)
			}
			if mode == "lost final status" {
				if !errors.Is(err, errSandboxActionTest) {
					t.Fatalf("Helm's swallowed storage error was lost: %v", err)
				}
				last, _ := cfg.Releases.Last(p.releaseName)
				if last.Info.Status != release.StatusPendingInstall {
					t.Fatal("test did not preserve the durable pending state")
				}
				store.deployedError = nil
				before := store.writes
				if err := stageSandboxTest(p, cfg, sandboxStageCheck); err == nil || store.writes != before {
					t.Fatal("pending install was silently adopted or retried")
				}
			}
		})
	}
}

func TestSandboxStageGuardRecheckedAfterAction(t *testing.T) {
	p, _, _ := sandboxRender(t, "openclaw", true, true)
	cfg, k, _ := sandboxActionConfig()
	changed, checksAfter := false, 0
	k.afterWrite = func() { changed = true }
	check := func(context.Context) error {
		if changed {
			checksAfter++
			return errSandboxActionTest
		}
		return nil
	}
	if err := stageSandboxTest(p, cfg, check); !errors.Is(err, errSandboxActionTest) || checksAfter == 0 {
		t.Fatalf("post-action authority change was ignored: %v", err)
	}
	last, _ := cfg.Releases.Last(p.releaseName)
	if last.Info.Status != release.StatusFailed {
		t.Fatal("guard failure was not retained as a failed held stage")
	}
	k.afterWrite, changed = nil, false
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
		t.Fatalf("failed held action was not recoverable: %v", err)
	}
	assertSandboxHeldWrites(t, k)
}

func rewriteSandboxTestSeal(t *testing.T, rel *release.Release, mutate func(*sandboxStageSeal)) {
	t.Helper()
	var seal sandboxStageSeal
	if err := decodeObject(rel.Config[sandboxStageConfigKey], &seal); err != nil {
		t.Fatal(err)
	}
	mutate(&seal)
	seal.Digest = seal.digest()
	raw, _ := json.Marshal(seal)
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	rel.Config[sandboxStageConfigKey] = value
}

func TestSandboxStageRejectsForeignAndUnexpectedReleaseStates(t *testing.T) {
	mutations := map[string]func(*testing.T, *release.Release){
		"legacy":         func(_ *testing.T, r *release.Release) { delete(r.Config, sandboxStageConfigKey) },
		"foreign config": func(_ *testing.T, r *release.Release) { r.Config["replicaCount"] = 1 },
		"unsealed chart": func(_ *testing.T, r *release.Release) {
			r.Chart.Templates = append(r.Chart.Templates, &chart.File{Name: "templates/foreign.yaml"})
		},
		"hooks":            func(_ *testing.T, r *release.Release) { r.Hooks = []*release.Hook{{Name: "foreign"}} },
		"changed manifest": func(_ *testing.T, r *release.Release) { r.Manifest += "\n---\nkind: Pod\n" },
	}
	for _, status := range []release.Status{
		release.StatusUnknown, release.StatusUninstalled, release.StatusUninstalling, release.StatusSuperseded,
		release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback,
	} {
		mutations[string(status)] = func(_ *testing.T, r *release.Release) { r.Info.Status = status }
	}
	for _, identity := range []string{"namespace UID", "Agent UID", "HOME UID", "skills UID", "generation", "workload"} {
		mutations[identity] = func(t *testing.T, r *release.Release) {
			rewriteSandboxTestSeal(t, r, func(s *sandboxStageSeal) {
				switch identity {
				case "namespace UID":
					s.Namespace.UID = "replaced-namespace"
				case "Agent UID":
					s.Agent.UID = "replaced-agent"
				case "HOME UID":
					s.Home.UID = "replaced-home"
				case "skills UID":
					s.Skills.UID = "replaced-skills"
				case "generation":
					s.AgentGeneration++
				case "workload":
					s.WorkloadName = "foreign"
				}
			})
		}
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, false)
			cfg, k, store := sandboxActionConfig()
			if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
				t.Fatal(err)
			}
			rel, _ := cfg.Releases.Last(p.releaseName)
			mutate(t, rel)
			if err := cfg.Releases.Update(rel); err != nil {
				t.Fatal(err)
			}
			before, resourceWrites := store.writes, len(k.writes)
			if err := stageSandboxTest(p, cfg, sandboxStageCheck); err == nil {
				t.Fatal("foreign or unexpected release was upgraded")
			}
			if store.writes != before || len(k.writes) != resourceWrites {
				t.Fatal("rejection changed release or resource state")
			}
		})
	}
}

func TestSandboxStageRejectsForeignAndReplacedObjects(t *testing.T) {
	for _, kind := range []string{"ConfigMap", "Service", "StatefulSet", "CiliumNetworkPolicy"} {
		for _, mode := range []string{"copied labels", "foreign Agent", "deleting", "replaced between reads"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				p, _, _ := sandboxRender(t, "openclaw", true, false)
				cfg, k, store := sandboxActionConfig()
				if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
					t.Fatal(err)
				}
				var selected *unstructured.Unstructured
				for _, obj := range k.objects {
					if obj.GetKind() == kind {
						selected = obj
					}
				}
				if selected == nil {
					t.Fatal("missing test resource")
				}
				switch mode {
				case "copied labels":
					selected.SetOwnerReferences(nil)
				case "foreign Agent":
					owners := selected.GetOwnerReferences()
					owners[0].UID = "other-agent"
					selected.SetOwnerReferences(owners)
				case "deleting":
					now := metav1.Now()
					selected.SetDeletionTimestamp(&now)
				}
				checks := 0
				check := func(context.Context) error {
					checks++
					if mode == "replaced between reads" && checks == 3 {
						selected.SetUID("replaced-object")
					}
					return nil
				}
				before, resourceWrites := store.writes, len(k.writes)
				if err := stageSandboxTest(p, cfg, check); err == nil {
					t.Fatal("existing desired object was adopted or changed without exact ownership")
				}
				if store.writes != before || len(k.writes) != resourceWrites {
					t.Fatal("foreign/replaced object rejection happened after writes")
				}
			})
		}
	}
}

func TestSandboxStageDoesNotAdoptObjectsWithoutOwnedRelease(t *testing.T) {
	p, _, _ := sandboxRender(t, "openclaw", true, false)
	cfg, k, store := sandboxActionConfig()
	objects, _ := sandboxStageObjects(p.manifest, sandboxSealIdentity(p.sandboxBundle.Identity()))
	for _, obj := range objects {
		obj.SetUID("pre-existing")
		obj.SetOwnerReferences([]metav1.OwnerReference{sandboxworkload.AgentOwner("example", "agent-uid")})
		obj.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})
		obj.SetAnnotations(map[string]string{"meta.helm.sh/release-name": p.releaseName, "meta.helm.sh/release-namespace": "cetirun-example"})
		k.objects[sandboxTestPath(&obj)] = obj.DeepCopy()
		break
	}
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); err == nil || store.writes != 0 || len(k.writes) != 0 {
		t.Fatal("first stage adopted a copied-label object without owned release history")
	}
}

func TestSandboxStageRejectsReleaseChangeDuringGuard(t *testing.T) {
	p, _, _ := sandboxRender(t, "openclaw", true, false)
	cfg, k, store := sandboxActionConfig()
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
		t.Fatal(err)
	}
	last, _ := cfg.Releases.Last(p.releaseName)
	checks := 0
	check := func(context.Context) error {
		checks++
		if checks == 3 {
			last.Info.Status = release.StatusPendingUpgrade
			return cfg.Releases.Update(last)
		}
		return nil
	}
	before, resourceWrites := store.writes, len(k.writes)
	if err := stageSandboxTest(p, cfg, check); err == nil ||
		store.writes != before+1 || len(k.writes) != resourceWrites {
		t.Fatal("changed release escaped the last-record check")
	}
}

func TestSandboxStageValidatesFailedUpgradeBaseline(t *testing.T) {
	p, _, _ := sandboxRender(t, "openclaw", true, false)
	cfg, k, store := sandboxActionConfig()
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
		t.Fatal(err)
	}
	driftSandboxTestConfig(k)
	k.failAt = 2
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); !errors.Is(err, errSandboxActionTest) {
		t.Fatal(err)
	}
	k.failAt = 0
	base, _ := cfg.Releases.Get(p.releaseName, 1)
	delete(base.Config, sandboxStageConfigKey)
	if err := cfg.Releases.Update(base); err != nil {
		t.Fatal(err)
	}
	before, resourceWrites := store.writes, len(k.writes)
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); err == nil ||
		store.writes != before || len(k.writes) != resourceWrites {
		t.Fatal("foreign older deployed baseline was used for a failed-stage retry")
	}
}

func TestSandboxStageNoOpRequiresIdenticalLiveObjects(t *testing.T) {
	for _, typ := range []string{"openclaw"} {
		for _, defaulted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/defaulted=%v", typ, defaulted), func(t *testing.T) {
				p, _, _ := sandboxRender(t, typ, true, false)
				cfg, k, store := sandboxActionConfig()
				if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
					t.Fatal(err)
				}
				if defaulted {
					addSandboxTestAPIDefaults(t, k)
				}
				before, writes, reads, checks := store.writes, len(k.writes), k.reads, 0
				for refresh := 0; refresh < 3; refresh++ {
					if err := stageSandboxTest(p, cfg, func(context.Context) error { checks++; return nil }); err != nil {
						t.Fatalf("unchanged stage refresh: %v", err)
					}
				}
				last, err := cfg.Releases.Last(p.releaseName)
				if err != nil || last.Version != 1 || store.writes != before || len(k.writes) != writes {
					t.Fatalf("periodic refresh wrote an unnecessary Helm revision: %v", err)
				}
				if k.reads <= reads || checks < 6 {
					t.Fatal("no-op trusted storage alone or skipped authoritative guard rechecks")
				}
			})
		}
	}
}

func addSandboxTestAPIDefaults(t *testing.T, k *sandboxActionKube) {
	t.Helper()
	for path, obj := range k.objects {
		switch obj.GetKind() {
		case "StatefulSet":
			var stateful appsv1.StatefulSet
			if err := decodeObject(obj, &stateful); err != nil {
				t.Fatal(err)
			}
			stateful.Spec.PodManagementPolicy = appsv1.OrderedReadyPodManagement
			stateful.Spec.RevisionHistoryLimit = ptr.To(int32(10))
			stateful.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
					Partition: ptr.To(int32(0)), MaxUnavailable: ptr.To(intstr.FromInt32(1)),
				},
			}
			stateful.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			}
			stateful.Spec.Ordinals = &appsv1.StatefulSetOrdinals{}
			stateful.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
			stateful.Spec.Template.Spec.SchedulerName = corev1.DefaultSchedulerName
			stateful.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(30))
			stateful.Spec.Template.Spec.DeprecatedServiceAccount = stateful.Spec.Template.Spec.ServiceAccountName
			stateful.Status.ObservedGeneration = 7
			current, err := sandboxUnstructured(&stateful)
			if err != nil {
				t.Fatal(err)
			}
			k.objects[path] = current
		case "Service":
			var service corev1.Service
			if err := decodeObject(obj, &service); err != nil {
				t.Fatal(err)
			}
			service.Spec.ClusterIP, service.Spec.ClusterIPs = "10.96.0.20", []string{"10.96.0.20"}
			service.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
			service.Spec.IPFamilyPolicy = ptr.To(corev1.IPFamilyPolicySingleStack)
			service.Spec.SessionAffinity = corev1.ServiceAffinityNone
			service.Spec.InternalTrafficPolicy = ptr.To(corev1.ServiceInternalTrafficPolicyCluster)
			current, err := sandboxUnstructured(&service)
			if err != nil {
				t.Fatal(err)
			}
			k.objects[path] = current
		}
		k.objects[path].SetGeneration(7)
		k.objects[path].SetManagedFields([]metav1.ManagedFieldsEntry{
			{Manager: "synthetic-controller", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: obj.GetAPIVersion()},
		})
	}
}

func TestSandboxStageNoOpDoesNotHideDriftOrMissingObjects(t *testing.T) {
	for _, kind := range []string{"ConfigMap", "Service", "StatefulSet", "CiliumNetworkPolicy", "missing ConfigMap", "template metadata"} {
		t.Run(kind, func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, false)
			cfg, k, store := sandboxActionConfig()
			if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
				t.Fatal(err)
			}
			for path, obj := range k.objects {
				if kind == "missing ConfigMap" && obj.GetKind() == "ConfigMap" {
					delete(k.objects, path)
					break
				}
				if kind == "template metadata" && obj.GetKind() == "StatefulSet" {
					template := object(object(obj.Object["spec"])["template"])
					object(template["metadata"])["name"] = "unexpected"
					break
				}
				if obj.GetKind() != kind {
					continue
				}
				switch kind {
				case "ConfigMap":
					driftSandboxTestConfig(k)
				case "Service":
					ports := object(obj.Object["spec"])["ports"].([]any)
					object(ports[0])["port"] = int64(43210)
				case "StatefulSet":
					pod := object(object(object(obj.Object["spec"])["template"])["spec"])
					containers := pod["containers"].([]any)
					object(containers[0])["image"] = "example.com/drift:1"
				case "CiliumNetworkPolicy":
					object(object(obj.Object["spec"])["enableDefaultDeny"])["egress"] = false
				}
				break
			}
			before, writes := store.writes, len(k.writes)
			err := stageSandboxTest(p, cfg, sandboxStageCheck)
			preservedField := kind == "Service" || kind == "template metadata"
			// Three-way merging preserves additions outside the sealed
			// baseline, including a changed Service port's merge key.
			// Refuse success instead of silently removing unowned fields.
			if preservedField {
				if err == nil || !strings.Contains(err.Error(), "sealed content") {
					t.Fatalf("unowned live field was not rejected by postconditions: %v", err)
				}
				for _, obj := range k.objects {
					if kind == "Service" && obj.GetKind() == "Service" {
						ports := object(obj.Object["spec"])["ports"].([]any)
						found := false
						for _, port := range ports {
							found = found || object(port)["port"] == int64(43210)
						}
						if !found {
							t.Fatal("unowned Service port was blindly removed")
						}
					}
					if kind == "template metadata" && obj.GetKind() == "StatefulSet" {
						template := object(object(obj.Object["spec"])["template"])
						if object(template["metadata"])["name"] != "unexpected" {
							t.Fatal("unowned template metadata was blindly removed")
						}
					}
				}
			} else if err != nil {
				t.Fatal(err)
			}
			last, _ := cfg.Releases.Last(p.releaseName)
			status := release.StatusDeployed
			if preservedField {
				status = release.StatusFailed
			}
			if last.Info.Status != status {
				t.Fatalf("drift reconciliation recorded status %s instead of %s", last.Info.Status, status)
			}
			if last.Version != 2 || store.writes == before || len(k.writes) == writes {
				t.Fatal("stored release was mistaken for proof of live convergence")
			}
			assertSandboxHeldWrites(t, k)
		})
	}
}

func TestSandboxStageNoOpFailsOnConcurrentChangeOrGuardFailure(t *testing.T) {
	for _, mode := range []string{"object version", "object UID", "post-check"} {
		t.Run(mode, func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, false)
			cfg, k, store := sandboxActionConfig()
			if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
				t.Fatal(err)
			}
			var config *unstructured.Unstructured
			for _, obj := range k.objects {
				if obj.GetKind() == "ConfigMap" {
					config = obj
				}
			}
			k.reads = 0
			k.afterRead = func() {
				if k.reads == 2*len(k.objects) {
					switch mode {
					case "object version":
						config.SetResourceVersion("changed-version")
					case "object UID":
						config.SetUID("replaced-object")
					}
				}
			}
			check := func(context.Context) error {
				if mode == "post-check" && k.reads >= 3*len(k.objects) {
					return errSandboxActionTest
				}
				return nil
			}
			before, writes := store.writes, len(k.writes)
			err := stageSandboxTest(p, cfg, check)
			if err == nil || store.writes != before || len(k.writes) != writes {
				t.Fatalf("concurrent change was accepted as an unchanged stage: %v", err)
			}
			if mode == "post-check" && !errors.Is(err, errSandboxActionTest) {
				t.Fatalf("no-op lost the final guard failure: %v", err)
			}
		})
	}
}

func TestSandboxStageRequiresJournalledRetirementBeforeInventoryChange(t *testing.T) {
	p, _, _ := sandboxRender(t, "openclaw", true, false)
	cfg, k, store := sandboxActionConfig()
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
		t.Fatal(err)
	}
	fixture := gatewayHeldFixture("openclaw")
	fixture.Input.Helpers.Desktop = false
	oldPolicy := fixture.Input.Controls.Objects[0].DeepCopy()
	fixture.Input.Controls.Objects[0].SetName("sandbox-replacement")
	bundle, err := sandboxworkload.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput("openclaw", "openai", false)
	object(input["secrets"])["existingSecret"] = "runtime-secret"
	next, err := PrepareSandboxChart(testChart(t), input, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	before, writes := store.writes, len(k.writes)
	if err := stageSandboxTest(next, cfg, sandboxStageCheck); err == nil || store.writes != before ||
		len(k.writes) != writes {
		t.Fatal("inventory change delegated grant retirement to Helm's best-effort deletion")
	}
	// The caller's separately journalled revocation has removed the old
	// control. Helm may now update its record without owning the retirement.
	delete(k.objects, sandboxTestPath(oldPolicy))
	if err := stageSandboxTest(next, cfg, sandboxStageCheck); err != nil {
		t.Fatalf("held stage could not follow confirmed retirement: %v", err)
	}
	assertSandboxHeldWrites(t, k)
}

func TestSandboxStageRespectsCancellationBeforeWrites(t *testing.T) {
	for _, mode := range []string{"already canceled", "deadline in guard", "API unavailable"} {
		t.Run(mode, func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, false)
			cfg, k, store := sandboxActionConfig()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			check := sandboxStageCheck
			switch mode {
			case "already canceled":
				cancel()
			case "deadline in guard":
				check = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
			case "API unavailable":
				k.readError = errSandboxActionTest
			}
			err := p.stageSandbox(ctx, cfg, "cetirun-example", p.releaseName, check)
			if err == nil || store.writes != 0 || len(k.writes) != 0 {
				t.Fatalf("failed context/API read permitted writes: %v", err)
			}
			if mode == "already canceled" && !errors.Is(err, context.Canceled) ||
				mode == "deadline in guard" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("context error was not preserved: %v", err)
			}
		})
	}
}

func TestSandboxContextTransportCancelsHelmBackgroundRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	base := restfake.CreateHTTPClient(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}).Transport
	transport := sandboxContextTransport{ctx: ctx, base: base}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.example.com", nil)
	done := make(chan error, 1)
	go func() {
		_, err := transport.RoundTrip(request)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("background request ignored stage cancellation: %v", err)
	}
	request, _ = http.NewRequest(http.MethodPost, "https://api.example.com", strings.NewReader("{}"))
	if _, err := transport.RoundTrip(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("request started after stage cancellation: %v", err)
	}
}
