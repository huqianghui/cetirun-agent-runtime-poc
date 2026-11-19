// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	helmkube "helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const maintenanceNamespace = "cetirun-example"
const maintenanceReleaseName = "openclaw-example"

const maintenanceTemplate = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: runtime
  namespace: {{ .Release.Namespace }}
spec:
  replicas: {{ .Values.replicaCount }}
  serviceName: runtime
  selector:
    matchLabels: {app: runtime}
  volumeClaimTemplates:
    - metadata: {name: data}
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: {{ .Values.claimSize | default "1Gi" }}
  template:
    metadata:
      labels: {app: runtime}
    spec:
      serviceAccountName: {{ .Values.account | default "agent" }}
      containers:
        - name: main
          image: registry.example.com/runtime:1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: runtime-config
  namespace: {{ .Release.Namespace }}
data:
  message: {{ .Values.message | quote }}
---
apiVersion: v1
kind: Service
metadata:
  name: runtime
  namespace: {{ .Release.Namespace }}
spec:
  type: ClusterIP
  selector: {app: runtime}
  ports: [{port: 8080}]
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: agent
  namespace: {{ .Release.Namespace }}
automountServiceAccountToken: false
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: readonly
  namespace: {{ .Release.Namespace }}
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [{{ .Values.permission | default "get" }}]
`

const maintenanceHookTemplate = `apiVersion: batch/v1
kind: Job
metadata:
  name: maintenance-hook
  namespace: {{ .Release.Namespace }}
  annotations:
    helm.sh/hook: pre-upgrade,post-upgrade
    helm.sh/hook-delete-policy: hook-succeeded
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: hook
          image: registry.example.com/hook:{{ .Values.hookVersion | default "1" }}
`

func maintenanceFixture(t *testing.T, hooks bool) (*PreparedRelease, *action.Configuration, *sandboxActionKube, *sandboxActionStorage) {
	t.Helper()
	cfg, kube, store := sandboxActionConfig()
	ch := &chart.Chart{Metadata: &chart.Metadata{APIVersion: "v2", Name: "cetirun", Version: "1.0.0"},
		Values: map[string]any{}, Templates: []*chart.File{{Name: "templates/runtime.yaml", Data: []byte(maintenanceTemplate)}}}
	if hooks {
		ch.Templates = append(ch.Templates, &chart.File{Name: "templates/hook.yaml", Data: []byte(maintenanceHookTemplate)})
	}
	install := action.NewInstall(cfg)
	install.Namespace, install.ReleaseName, install.DisableHooks = maintenanceNamespace, maintenanceReleaseName, true
	if _, err := install.Run(ch, map[string]any{"replicaCount": 1, "message": "original"}); err != nil {
		t.Fatal(err)
	}
	p := &PreparedRelease{chart: ch, namespace: maintenanceNamespace, releaseName: maintenanceReleaseName,
		Agent: agents.ResolvedAgent{Type: "hermes"}, Values: map[string]any{"replicaCount": 0, "message": "desired"}}
	kube.writes, kube.hooks, kube.waits, kube.deletes, store.writes = nil, 0, 0, 0, 0
	return p, cfg, kube, store
}

func maintenanceApply(ctx context.Context, p *PreparedRelease, cfg *action.Configuration, revision int, guard func(context.Context) error) error {
	return (&Client{}).installOrUpgradeMaintenance(ctx, maintenanceNamespace, maintenanceReleaseName, p, revision, guard,
		func(context.Context, string, func(context.Context) error) (*action.Configuration, error) {
			return cfg, nil
		})
}

func allowMaintenance(context.Context) error { return nil }

func maintenanceSeedHistory(t *testing.T, cfg *action.Configuration, count int) {
	t.Helper()
	first, err := cfg.Releases.Last(maintenanceReleaseName)
	if err != nil {
		t.Fatal(err)
	}
	first.Info.Status = release.StatusSuperseded
	if err := cfg.Releases.Update(first); err != nil {
		t.Fatal(err)
	}
	for revision := 2; revision <= count; revision++ {
		next := sandboxReleaseCopy(first)
		next.Version = revision
		if revision == count {
			next.Info.Status = release.StatusDeployed
		}
		if err := cfg.Releases.Create(next); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMaintenanceSDKDisablesHooksKeepsHistoryAndOnlyUpdatesStoppedRuntime(t *testing.T) {
	p, cfg, kube, _ := maintenanceFixture(t, true)
	maintenanceSeedHistory(t, cfg, 15)
	checks := 0
	err := maintenanceApply(context.Background(), p, cfg, 15, func(context.Context) error { checks++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if checks < 4 || kube.hooks != 0 || kube.waits != 0 || kube.deletes != 0 {
		t.Fatal("maintenance skipped guards or ran hooks/waits/deletes")
	}
	if len(kube.writes) != 3 {
		t.Fatalf("expected only workload/config/service updates, got %d", len(kube.writes))
	}
	for _, object := range kube.writes {
		switch object.GetKind() {
		case "StatefulSet":
			replicas, _, _ := unstructured.NestedInt64(object.Object, "spec", "replicas")
			if replicas != 0 {
				t.Fatal("maintenance started a workload")
			}
		case "ConfigMap", "Service":
		default:
			t.Fatalf("protected resource reached update: %s", object.GetKind())
		}
	}
	history, err := readReleaseHistory(context.Background(), cfg, maintenanceNamespace, maintenanceReleaseName, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 16 || history[0].Version != 16 || history[0].Info.Status != release.StatusDeployed {
		t.Fatal("maintenance pruned history or lost the new revision")
	}
	if len(history[0].Hooks) != 1 || !history[0].Hooks[0].LastRun.StartedAt.IsZero() {
		t.Fatal("disabled hooks were run or lost from inspection history")
	}
	if p.maintenance != nil || len(p.manifest) != 0 || len(p.chart.Templates) != 2 {
		t.Fatal("maintenance mutated the caller's prepared release")
	}
}

func TestMaintenanceDoesNotChangeNormalHookOrHistoryPolicy(t *testing.T) {
	t.Run("normal hooks still execute with Wait false", func(t *testing.T) {
		p, cfg, kube, _ := maintenanceFixture(t, true)
		p.Values["maintenance"], p.Values["disableHooks"], p.Values["historyMax"] = true, true, 0
		err := (&Client{}).installOrUpgrade(maintenanceNamespace, maintenanceReleaseName, p, func(string) (*action.Configuration, error) { return cfg, nil })
		if err == nil || kube.hooks == 0 {
			t.Fatalf("ordinary hook behavior changed: hooks=%d, error=%v", kube.hooks, err)
		}
	})
	t.Run("normal history still retains ten", func(t *testing.T) {
		p, cfg, _, _ := maintenanceFixture(t, false)
		maintenanceSeedHistory(t, cfg, 15)
		if err := (&Client{}).installOrUpgrade(maintenanceNamespace, maintenanceReleaseName, p, func(string) (*action.Configuration, error) { return cfg, nil }); err != nil {
			t.Fatal(err)
		}
		history, err := cfg.Releases.History(maintenanceReleaseName)
		if err != nil || len(history) != 10 {
			t.Fatalf("ordinary history policy changed: count=%d error=%v", len(history), err)
		}
	})
}

func TestMaintenanceRejectsArgumentsBeforeConfiguration(t *testing.T) {
	for _, scenario := range []string{"nil context", "nil guard", "revision zero", "namespace", "release", "unbound release", "running", "missing replicas", "string replicas", "sandbox"} {
		t.Run(scenario, func(t *testing.T) {
			p, _, _, _ := maintenanceFixture(t, false)
			ctx, guard, revision := context.Background(), allowMaintenance, 1
			switch scenario {
			case "nil context":
				ctx = nil
			case "nil guard":
				guard = nil
			case "revision zero":
				revision = 0
			case "namespace":
				p.namespace = "another-namespace"
			case "release":
				p.releaseName = "another-release"
			case "unbound release":
				p.releaseName = ""
			case "running":
				p.Values["replicaCount"] = 1
			case "missing replicas":
				delete(p.Values, "replicaCount")
			case "string replicas":
				p.Values["replicaCount"] = "0"
			case "sandbox":
				p.Values["sandboxWorkload"] = map[string]any{}
			}
			configured := false
			err := (&Client{}).installOrUpgradeMaintenance(ctx, maintenanceNamespace, maintenanceReleaseName, p, revision, guard,
				func(context.Context, string, func(context.Context) error) (*action.Configuration, error) {
					configured = true
					return nil, nil
				})
			if err == nil || configured {
				t.Fatal("invalid maintenance input reached cluster configuration")
			}
		})
	}
}

func TestMaintenanceRejectsMissingPendingAndChangedRevisionsWithoutWrites(t *testing.T) {
	for _, scenario := range []string{"missing", "revision", "pending-install", "pending-upgrade", "pending-rollback", "uninstalled", "read failure"} {
		t.Run(scenario, func(t *testing.T) {
			p, cfg, kube, store := maintenanceFixture(t, true)
			last, _ := cfg.Releases.Last(maintenanceReleaseName)
			revision := 1
			switch scenario {
			case "missing":
				_, _ = cfg.Releases.Delete(maintenanceReleaseName, 1)
			case "revision":
				revision = 2
			case "read failure":
				store.queryError = errors.New("synthetic read failure")
			default:
				last.Info.Status = release.Status(scenario)
				if err := cfg.Releases.Update(last); err != nil {
					t.Fatal(err)
				}
			}
			before := store.writes
			if err := maintenanceApply(context.Background(), p, cfg, revision, allowMaintenance); err == nil {
				t.Fatal("release conflict became success")
			}
			if store.writes != before || len(kube.writes) != 0 || kube.hooks != 0 {
				t.Fatal("rejected maintenance reached writes")
			}
		})
	}
}

func TestMaintenanceGuardFailureAtPreflightAndWriteBoundaries(t *testing.T) {
	for _, failAt := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			p, cfg, kube, store := maintenanceFixture(t, true)
			failure := errors.New("synthetic withdrawn maintenance authority")
			calls := 0
			err := maintenanceApply(context.Background(), p, cfg, 1, func(context.Context) error {
				calls++
				if calls == failAt {
					return failure
				}
				return nil
			})
			if !errors.Is(err, failure) {
				t.Fatalf("guard failure was lost: %v", err)
			}
			if errors.Is(err, ErrMaintenanceNotSubmitted) != (failAt <= 3) {
				t.Fatalf("write submission outcome was not preserved: %v", err)
			}
			if failAt <= 3 && (store.writes != 0 || len(kube.writes) != 0) {
				t.Fatal("pre-submission rejection wrote resources")
			}
			if failAt == 4 && (store.writes != 1 || len(kube.writes) != 0) {
				t.Fatal("workload write was not fenced after the pending revision")
			}
			if kube.hooks != 0 || kube.deletes != 0 {
				t.Fatal("guard failure ran recovery hooks or deletion")
			}
		})
	}
}

func TestMaintenanceGuardCannotChangePreparedValues(t *testing.T) {
	p, cfg, kube, store := maintenanceFixture(t, false)
	err := maintenanceApply(context.Background(), p, cfg, 1, func(context.Context) error { p.Values["replicaCount"] = 1; return nil })
	if err == nil || store.writes != 0 || len(kube.writes) != 0 {
		t.Fatal("changed prepared input was submitted")
	}
}

func TestMaintenanceDoesNotRerenderSourceAfterPreflight(t *testing.T) {
	p, cfg, _, _ := maintenanceFixture(t, true)
	calls := 0
	err := maintenanceApply(context.Background(), p, cfg, 1, func(context.Context) error {
		calls++
		if calls == 2 {
			p.chart.Templates = append(p.chart.Templates, &chart.File{Name: "templates/late.yaml", Data: []byte(`{{ fail "late source template was executed" }}`)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	last, _ := cfg.Releases.Last(maintenanceReleaseName)
	if strings.Contains(last.Manifest, "late source") {
		t.Fatal("unvalidated source reached the release")
	}
}

func TestMaintenanceRejectsHookPermissionStorageAndObjectChanges(t *testing.T) {
	for _, scenario := range []string{"hook", "permission", "storage", "account", "container privilege", "container mount", "credential reference", "namespace", "new object", "removed object", "running template", "CRD"} {
		t.Run(scenario, func(t *testing.T) {
			p, cfg, kube, store := maintenanceFixture(t, true)
			switch scenario {
			case "hook":
				p.Values["hookVersion"] = "2"
			case "permission":
				p.Values["permission"] = "delete"
			case "storage":
				p.Values["claimSize"] = "2Gi"
			case "account":
				p.Values["account"] = "foreign"
			case "container privilege":
				p.chart.Templates[0].Data = []byte(strings.Replace(maintenanceTemplate, "image: registry.example.com/runtime:1", "image: registry.example.com/runtime:1\n          securityContext: {privileged: true}", 1))
			case "container mount":
				p.chart.Templates[0].Data = []byte(strings.Replace(maintenanceTemplate, "image: registry.example.com/runtime:1", "image: registry.example.com/runtime:1\n          volumeMounts: [{name: data, mountPath: /foreign-home}]", 1))
			case "credential reference":
				p.chart.Templates[0].Data = []byte(strings.Replace(maintenanceTemplate, "image: registry.example.com/runtime:1", "image: registry.example.com/runtime:1\n          env: [{name: PRIVATE, valueFrom: {secretKeyRef: {name: foreign, key: token}}}]", 1))
			case "namespace":
				p.chart.Templates[0].Data = []byte(strings.Replace(maintenanceTemplate, "namespace: {{ .Release.Namespace }}", "namespace: foreign", 1))
			case "new object":
				p.chart.Templates = append(p.chart.Templates, &chart.File{Name: "templates/new.yaml", Data: []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: new-secret\n")})
			case "removed object":
				p.chart.Templates[0].Data = []byte(strings.Split(maintenanceTemplate, "---\napiVersion: rbac.")[0])
			case "running template":
				p.chart.Templates[0].Data = []byte(strings.Replace(maintenanceTemplate, "replicas: {{ .Values.replicaCount }}", "replicas: 1", 1))
			case "CRD":
				p.chart.Files = []*chart.File{{Name: "crds/fixture.yaml", Data: []byte("kind: CustomResourceDefinition\n")}}
			}
			err := maintenanceApply(context.Background(), p, cfg, 1, allowMaintenance)
			if err == nil || store.writes != 0 || len(kube.writes) != 0 || kube.hooks != 0 {
				t.Fatalf("unsupported %s reached writes: %v", scenario, err)
			}
		})
	}
}

func TestMaintenanceRevisionChangingAfterPreflightDoesNotOverwrite(t *testing.T) {
	p, cfg, kube, store := maintenanceFixture(t, false)
	calls := 0
	err := maintenanceApply(context.Background(), p, cfg, 1, func(context.Context) error {
		calls++
		if calls == 2 {
			next, _ := cfg.Releases.Last(maintenanceReleaseName)
			next.Version, next.Info.Status = 2, release.StatusPendingUpgrade
			return cfg.Releases.Create(next)
		}
		return nil
	})
	if err == nil || store.writes != 1 || len(kube.writes) != 0 {
		t.Fatal("concurrent revision was overwritten")
	}
}

func TestMaintenanceHistoryReturnsBoundedExactInternalContent(t *testing.T) {
	_, cfg, _, store := maintenanceFixture(t, true)
	maintenanceSeedHistory(t, cfg, 12)
	history, err := readReleaseHistory(context.Background(), cfg, maintenanceNamespace, maintenanceReleaseName, 2)
	if err != nil || len(history) != 2 || history[0].Version != 12 || history[1].Version != 11 ||
		len(history[0].Hooks) != 1 || !strings.Contains(history[0].Manifest, "kind: StatefulSet") {
		t.Fatalf("incorrect release/history read: %v", err)
	}
	store.queryError = errors.New("synthetic storage unavailable, not found is not an absence proof")
	if _, err := readReleaseHistory(context.Background(), cfg, maintenanceNamespace, maintenanceReleaseName, 1); !errors.Is(err, store.queryError) {
		t.Fatal("read failure was hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readReleaseHistory(ctx, cfg, maintenanceNamespace, maintenanceReleaseName, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read continued")
	}
}

func TestMaintenanceFailedInstallRevisionCanBeReappliedWithoutDroppingHistory(t *testing.T) {
	p, cfg, kube, _ := maintenanceFixture(t, true)
	last, _ := cfg.Releases.Last(maintenanceReleaseName)
	last.Info.Status = release.StatusFailed
	if err := cfg.Releases.Update(last); err != nil {
		t.Fatal(err)
	}
	if err := maintenanceApply(context.Background(), p, cfg, 1, allowMaintenance); err != nil {
		t.Fatal(err)
	}
	history, err := cfg.Releases.History(maintenanceReleaseName)
	if err != nil || len(history) != 2 || kube.hooks != 0 {
		t.Fatalf("failed revision was not retained: %v", err)
	}
}

func TestMaintenanceRequiresReadbackCapacityBeforeWriting(t *testing.T) {
	p, cfg, kube, store := maintenanceFixture(t, false)
	maintenanceSeedHistory(t, cfg, maintenanceHistoryLimit)
	before := store.writes
	if err := maintenanceApply(context.Background(), p, cfg, maintenanceHistoryLimit, allowMaintenance); err == nil {
		t.Fatal("maintenance committed a revision outside its readable history bound")
	}
	if store.writes != before || len(kube.writes) != 0 {
		t.Fatal("history capacity was checked after writes")
	}
}

func TestMaintenanceCustomUsesItsExistingSealedActionPreparation(t *testing.T) {
	cfg, kube, store := sandboxActionConfig()
	cfg.KubeClient = &maintenanceNamespaceKube{sandboxActionKube: kube}
	input := customInput(8080)
	first, err := prepareChart(testChart(t), input, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.prepareCustomAction(cfg, maintenanceNamespace, maintenanceReleaseName); err != nil {
		t.Fatal(err)
	}
	if err := first.installOrUpgradeCustom(cfg, maintenanceNamespace, maintenanceReleaseName); err != nil {
		t.Fatal(err)
	}
	input["replicaCount"] = 0
	next, err := prepareChart(testChart(t), input, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.prepareCustomAction(cfg, maintenanceNamespace, maintenanceReleaseName); err != nil {
		t.Fatal(err)
	}
	// Admission/API validation is represented by the mock kube client in this test.
	next.namespace, next.apiValidated = maintenanceNamespace, true
	kube.writes, store.writes = nil, 0
	if err := maintenanceApply(context.Background(), next, cfg, 1, allowMaintenance); err != nil {
		t.Fatal(err)
	}
	if kube.hooks != 0 || kube.waits != 0 {
		t.Fatal("Custom maintenance ran hooks or waited for readiness")
	}
	for _, obj := range kube.writes {
		if obj.GetKind() == "StatefulSet" {
			replicas, _, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
			if replicas != 0 {
				t.Fatal("Custom maintenance started a workload")
			}
		}
	}
	current, _ := cfg.Releases.Last(maintenanceReleaseName)
	if current.Version != 2 || current.Info.Status != release.StatusDeployed {
		t.Fatal("Custom maintenance was not recorded")
	}
}

// Real Helm Build supplies the release namespace when chart metadata omits it.
type maintenanceNamespaceKube struct{ *sandboxActionKube }

func (k *maintenanceNamespaceKube) Build(reader io.Reader, validate bool) (helmkube.ResourceList, error) {
	resources, err := k.sandboxActionKube.Build(reader, validate)
	if err != nil {
		return nil, err
	}
	for _, info := range resources {
		if info.Mapping.Scope.Name() == meta.RESTScopeNameNamespace && info.Namespace == "" {
			info.Namespace = maintenanceNamespace
			info.Object.(*unstructured.Unstructured).SetNamespace(maintenanceNamespace)
		}
	}
	return resources, nil
}

func TestMaintenancePreparationRetainsTrustedNamesAcrossRefresh(t *testing.T) {
	input := sampleInput("hermes", "openai", false)
	input["agentRequest"].(map[string]any)["releaseName"] = maintenanceReleaseName
	prepared, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	prepared.namespace = maintenanceNamespace
	input["replicaCount"] = 0
	refreshed, err := prepared.Rebuild(input)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.namespace != maintenanceNamespace || refreshed.releaseName != maintenanceReleaseName || !sameJSON(refreshed.Values["replicaCount"], 0) {
		t.Fatal("refresh lost its namespace, trusted release name or suspended target")
	}
}

func TestMaintenanceHistoryRejectsExcessAndMalformedRecords(t *testing.T) {
	for _, scenario := range []string{"count", "size", "nil info", "mismatched identity"} {
		t.Run(scenario, func(t *testing.T) {
			_, cfg, _, _ := maintenanceFixture(t, false)
			if scenario == "count" {
				maintenanceSeedHistory(t, cfg, maintenanceHistoryLimit+1)
			} else {
				last, _ := cfg.Releases.Last(maintenanceReleaseName)
				switch scenario {
				case "size":
					last.Manifest = strings.Repeat("x", maintenanceManifestLimit+1)
				case "nil info":
					last.Info = nil
				case "mismatched identity":
					last.Namespace = "wrong-namespace"
				}
				// Get returns the memory driver's stored pointer, allowing a malformed fixture.
				stored, _ := cfg.Releases.Driver.(*sandboxActionStorage).Driver.Get(storage.HelmStorageType + "." + maintenanceReleaseName + ".v1")
				*stored = *last
			}
			if _, err := readReleaseHistory(context.Background(), cfg, maintenanceNamespace, maintenanceReleaseName, 1); err == nil {
				t.Fatal("incomplete inspection succeeded")
			}
		})
	}
}

func TestMaintenanceAPITransportRejectsCancellationAndDeletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transport := maintenanceTransport{ctx: ctx, guard: allowMaintenance, base: maintenanceTestTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled or deleting maintenance request reached the API")
		return nil, nil
	})}
	request, _ := http.NewRequest(http.MethodDelete, "https://api.example.invalid/fixture", nil)
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("maintenance deletion was allowed")
	}
	cancel()
	request.Method = http.MethodGet
	if _, err := transport.RoundTrip(request); !errors.Is(err, context.Canceled) {
		t.Fatal("read did not retain cancellation")
	}
}

func TestMaintenanceSecretQueriesAreBoundedAndCorruptionIsNotSkipped(t *testing.T) {
	for _, scenario := range []string{"valid", "paginated", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			api := kubefake.NewClientset()
			secrets := api.CoreV1().Secrets(maintenanceNamespace)
			sdk := driver.NewSecrets(secrets)
			var logs bytes.Buffer
			sdk.Log = func(format string, args ...any) { fmt.Fprintf(&logs, format, args...) }
			key := storage.HelmStorageType + "." + maintenanceReleaseName + ".v1"
			if err := sdk.Create(key, &release.Release{Name: maintenanceReleaseName, Namespace: maintenanceNamespace,
				Version: 1, Info: &release.Info{Status: release.StatusFailed}, Manifest: "synthetic-private-manifest"}); err != nil {
				t.Fatal(err)
			}
			if scenario == "corrupt" {
				secret, _ := secrets.Get(context.Background(), key, metav1.GetOptions{})
				secret.Data["release"] = []byte("synthetic-private-manifest-invalid-encoding")
				if _, err := secrets.Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			api.PrependReactor("list", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
				options := action.(ktesting.ListActionImpl).GetListOptions()
				if options.Limit != maintenanceHistoryLimit+1 || action.GetNamespace() != maintenanceNamespace ||
					options.LabelSelector != "name="+maintenanceReleaseName+",owner=helm" {
					t.Error("unbounded or unscoped Secret query")
				}
				if scenario == "paginated" {
					return true, &corev1.SecretList{ListMeta: metav1.ListMeta{Continue: "more"}}, nil
				}
				return false, nil, nil
			})
			bounded := &boundedReleaseSecrets{Driver: sdk, ctx: context.Background(), secrets: secrets}
			records, err := bounded.Query(map[string]string{"owner": "helm", "name": maintenanceReleaseName})
			if (err != nil) != (scenario != "valid") {
				t.Fatalf("incorrect bounded read result: %v", err)
			}
			if scenario == "valid" && (len(records) != 1 || records[0].Manifest != "synthetic-private-manifest") {
				t.Fatal("internal read lost manifest content")
			}
			if strings.Contains(logs.String(), "synthetic-private-manifest") {
				t.Fatal("Secret or manifest was logged")
			}
		})
	}
}

type maintenanceTestTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMaintenanceHTTPGuardRunsBeforeEachAPIWrite(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			calls, checks := 0, 0
			denied := errors.New("synthetic stale namespace binding")
			transport := maintenanceTransport{ctx: ctx, guard: func(context.Context) error { checks++; return denied },
				base: maintenanceTestTransport(func(request *http.Request) (*http.Response, error) {
					calls++
					if request.Context() != ctx {
						t.Error("SDK background request lost its deadline")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
				})}
			request, _ := http.NewRequest(method, "https://api.example.invalid/fixture", nil)
			response, err := transport.RoundTrip(request)
			if method == http.MethodGet {
				if err != nil || calls != 1 || checks != 0 {
					t.Fatal("read unexpectedly required a write grant")
				}
				response.Body.Close()
			} else if !errors.Is(err, denied) || calls != 0 || checks != 1 {
				t.Fatal("request reached API before guard")
			}
		})
	}
}
