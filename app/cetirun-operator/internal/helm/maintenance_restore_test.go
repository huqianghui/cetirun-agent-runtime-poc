// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/resource"
	restfake "k8s.io/client-go/rest/fake"
)

// Build and HTTP are local doubles; Update is the pinned Helm SDK, including
// its GET/NotFound/POST path. No listener or Kubernetes credentials are used.
type restorationSDKKube struct {
	*sandboxActionKube
	created     []string
	missing     []string
	notFound    atomic.Int64
	beforeWrite func(context.Context) error
}

func (k *restorationSDKKube) Build(reader io.Reader, validate bool) (kube.ResourceList, error) {
	list, err := k.sandboxActionKube.Build(reader, validate)
	if err != nil {
		return nil, err
	}
	for _, info := range list {
		mapping := info.Mapping
		apiPath := "/apis/" + mapping.Resource.GroupVersion().String()
		if mapping.Resource.Group == "" {
			apiPath = "/api/" + mapping.Resource.Version
		}
		info.Client = &restfake.RESTClient{GroupVersion: mapping.Resource.GroupVersion(), VersionedAPIPath: apiPath,
			NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
			Client:               restfake.CreateHTTPClient(k.request)}
	}
	return list, nil
}

func (k *restorationSDKKube) request(request *http.Request) (*http.Response, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.readError != nil {
		return nil, k.readError
	}
	if request.Method != http.MethodGet && k.beforeWrite != nil {
		if err := k.beforeWrite(request.Context()); err != nil {
			return nil, err
		}
	}
	if !strings.Contains(request.URL.Path, "/namespaces/"+maintenanceNamespace+"/") {
		return nil, fmt.Errorf("unexpected non-target API path")
	}
	current := k.objects[request.URL.Path]
	if request.Method == http.MethodGet {
		k.reads++
		if current == nil {
			k.missing = append(k.missing, request.URL.Path)
			k.notFound.Add(1)
			return sandboxSDKStatus(http.StatusNotFound, metav1.StatusReasonNotFound)
		}
		return sandboxSDKResponse(http.StatusOK, current)
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, maintenanceManifestLimit+1))
	if err != nil || len(raw) > maintenanceManifestLimit {
		return nil, fmt.Errorf("invalid local request body")
	}
	switch request.Method {
	case http.MethodPost:
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(raw); err != nil {
			return nil, err
		}
		if obj.GetName() == "" || obj.GetNamespace() != maintenanceNamespace || obj.GetUID() != "" || obj.GetResourceVersion() != "" || sandboxTestPath(obj) != request.URL.Path+"/"+obj.GetName() {
			return nil, fmt.Errorf("unexpected recreation identity")
		}
		if k.objects[sandboxTestPath(obj)] != nil {
			return sandboxSDKStatus(http.StatusConflict, metav1.StatusReasonAlreadyExists)
		}
		written, err := k.writeObject(obj)
		if err != nil {
			return nil, err
		}
		k.created = append(k.created, obj.GetKind()+"/"+obj.GetName())
		return sandboxSDKResponse(http.StatusCreated, written)
	case http.MethodPatch:
		if current == nil {
			return sandboxSDKStatus(http.StatusNotFound, metav1.StatusReasonNotFound)
		}
		obj, err := sandboxSDKMergePatch(current, raw, request.Header.Get("Content-Type"))
		if err != nil {
			return nil, err
		}
		written, err := k.writeObject(obj)
		if err != nil {
			return nil, err
		}
		return sandboxSDKResponse(http.StatusOK, written)
	default:
		return nil, fmt.Errorf("unexpected maintenance method %s", request.Method)
	}
}

func (k *restorationSDKKube) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	sdk := &kube.Client{Log: func(string, ...any) {}}
	return sdk.Update(original, target, force)
}

func restorationFixture(t *testing.T, template string) (*PreparedRelease, *action.Configuration, *restorationSDKKube, *sandboxActionStorage) {
	t.Helper()
	cfg, base, store := sandboxActionConfig()
	ch := &chart.Chart{Metadata: &chart.Metadata{APIVersion: "v2", Name: "cetirun", Version: "1.0.0"}, Values: map[string]any{},
		Templates: []*chart.File{{Name: "templates/runtime.yaml", Data: []byte(template)}, {Name: "templates/hook.yaml", Data: []byte(maintenanceHookTemplate)}}}
	install := action.NewInstall(cfg)
	install.Namespace, install.ReleaseName, install.DisableHooks = maintenanceNamespace, maintenanceReleaseName, true
	if _, err := install.Run(ch, map[string]any{"replicaCount": 1, "message": "original"}); err != nil {
		t.Fatal(err)
	}
	last, _ := cfg.Releases.Last(maintenanceReleaseName)
	last.Info.Status = release.StatusFailed // The pending-install helper repaired only its status.
	if err := cfg.Releases.Update(last); err != nil {
		t.Fatal(err)
	}
	p := &PreparedRelease{chart: ch, namespace: maintenanceNamespace, releaseName: maintenanceReleaseName,
		Agent: agents.ResolvedAgent{Type: "hermes"}, Values: map[string]any{"replicaCount": 0, "message": "desired"}}
	k := &restorationSDKKube{sandboxActionKube: base}
	cfg.KubeClient = k
	base.writes, base.hooks, base.waits, base.deletes, store.writes = nil, 0, 0, 0, 0
	return p, cfg, k, store
}

func applyRestoration(p *PreparedRelease, cfg *action.Configuration, k *restorationSDKKube, guard func(context.Context) error) error {
	return (&Client{}).installOrUpgradeMaintenance(context.Background(), maintenanceNamespace, maintenanceReleaseName, p, 1, guard,
		func(_ context.Context, _ string, beforeWrite func(context.Context) error) (*action.Configuration, error) {
			k.beforeWrite = beforeWrite
			return cfg, nil
		})
}

func TestMaintenanceRestoresOnlyMissingOriginalRuntimeObjects(t *testing.T) {
	for _, kind := range []string{"ConfigMap", "StatefulSet", "Deployment"} {
		t.Run(kind, func(t *testing.T) {
			template := maintenanceTemplate
			if kind == "Deployment" {
				template = strings.Replace(template, "kind: StatefulSet", "kind: Deployment", 1)
				start, end := strings.Index(template, "  serviceName:"), strings.Index(template, "  template:")
				template = template[:start] + "  selector:\n    matchLabels: {app: runtime}\n" + template[end:]
			}
			p, cfg, k, _ := restorationFixture(t, template)
			original, _ := cfg.Releases.Last(maintenanceReleaseName)
			var removed *unstructured.Unstructured
			for key, obj := range k.objects {
				if obj.GetKind() == kind {
					removed = obj.DeepCopy()
					delete(k.objects, key)
				}
			}
			if removed == nil {
				t.Fatal("fixture did not remove a real current object")
			}
			if err := applyRestoration(p, cfg, k, allowMaintenance); err != nil {
				t.Fatal(err)
			}
			if len(k.created) != 1 || k.created[0] != kind+"/"+removed.GetName() || len(k.missing) == 0 {
				t.Fatalf("missing object did not take the SDK creation path: %v", k.created)
			}
			restored := k.objects[sandboxTestPath(removed)]
			if restored == nil || restored.GetUID() == removed.GetUID() {
				t.Fatal("missing object was not newly created")
			}
			if kind != "ConfigMap" {
				if replicas, _, _ := unstructured.NestedInt64(restored.Object, "spec", "replicas"); replicas != 0 {
					t.Fatal("restoration started a workload")
				}
			}
			if k.hooks != 0 || k.waits != 0 || k.deletes != 0 {
				t.Fatal("restoration ran hooks, waited for Pods or deleted resources")
			}
			history, err := cfg.Releases.History(maintenanceReleaseName)
			if err != nil || len(history) != 2 {
				t.Fatal("repair history was not retained", err)
			}
			retained, _ := cfg.Releases.Get(maintenanceReleaseName, 1)
			if retained.Manifest != original.Manifest || !sameJSON(retained.Config, original.Config) || retained.Info.Status != release.StatusSuperseded {
				t.Fatal("original repair evidence changed")
			}
		})
	}
}

func TestMaintenanceRejectsMissingProtectedObjectsAndReadUncertainty(t *testing.T) {
	for _, kind := range []string{"Service", "ServiceAccount", "Role", "Secret", "PersistentVolumeClaim", "immutable ConfigMap", "read error"} {
		t.Run(kind, func(t *testing.T) {
			template := maintenanceTemplate
			if kind == "Secret" || kind == "PersistentVolumeClaim" {
				template += "\n---\napiVersion: v1\nkind: " + kind + "\nmetadata:\n  name: protected\n  namespace: {{ .Release.Namespace }}\n"
			}
			if kind == "immutable ConfigMap" {
				template = strings.Replace(template, "data:\n  message:", "immutable: true\ndata:\n  message:", 1)
			}
			p, cfg, k, store := restorationFixture(t, template)
			if kind == "immutable ConfigMap" {
				p.Values["message"] = "original"
			}
			removed := false
			for key, obj := range k.objects {
				if obj.GetKind() == strings.TrimPrefix(kind, "immutable ") {
					delete(k.objects, key)
					removed = true
				}
			}
			if kind == "read error" {
				k.readError = errors.New("synthetic unavailable API")
			} else if !removed {
				t.Fatal("missing-object fixture was not exercised")
			}
			if err := applyRestoration(p, cfg, k, allowMaintenance); err == nil {
				t.Fatal("unapproved creation or unknown read became a successful recovery")
			}
			if store.writes != 0 || len(k.writes) != 0 || len(k.created) != 0 {
				t.Fatal("rejected restoration reached a write")
			}
		})
	}
}

func TestMaintenanceRestoreCannotExpandOriginalNamesOrPrivileges(t *testing.T) {
	for _, scenario := range []string{"name", "namespace", "identity", "storage", "RBAC", "running", "hook"} {
		t.Run(scenario, func(t *testing.T) {
			p, cfg, k, store := restorationFixture(t, maintenanceTemplate)
			for key, obj := range k.objects {
				if obj.GetKind() == "ConfigMap" || obj.GetKind() == "StatefulSet" {
					delete(k.objects, key)
				}
			}
			switch scenario {
			case "name":
				p.chart.Templates[0].Data = []byte(strings.Replace(maintenanceTemplate, "name: runtime-config", "name: different-config", 1))
			case "namespace":
				p.chart.Templates[0].Data = []byte(strings.ReplaceAll(maintenanceTemplate, "namespace: {{ .Release.Namespace }}", "namespace: foreign"))
			case "identity":
				p.Values["account"] = "another-account"
			case "storage":
				p.Values["claimSize"] = "20Gi"
			case "RBAC":
				p.Values["permission"] = "delete"
			case "running":
				p.Values["replicaCount"] = 1
			case "hook":
				p.Values["hookVersion"] = "2"
			}
			if err := applyRestoration(p, cfg, k, allowMaintenance); err == nil || store.writes != 0 || len(k.created) != 0 {
				t.Fatal("restoration widened the original release boundary", err)
			}
		})
	}
}

func TestMaintenanceRestoreGuardFencesActualSDKPost(t *testing.T) {
	p, cfg, k, _ := restorationFixture(t, maintenanceTemplate)
	for key, obj := range k.objects {
		if obj.GetKind() == "ConfigMap" {
			delete(k.objects, key)
		}
	}
	denied := errors.New("synthetic missing-object authorization withdrawn")
	err := applyRestoration(p, cfg, k, func(context.Context) error {
		if k.notFound.Load() >= 2 {
			return denied
		}
		return nil
	})
	if !errors.Is(err, denied) || len(k.created) != 0 {
		t.Fatal("actual POST escaped the caller's maintenance guard", err)
	}
}
