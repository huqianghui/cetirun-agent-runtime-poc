// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	helmtime "helm.sh/helm/v3/pkg/time"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type maintenanceTestHelm struct {
	suspensionHelmClient
	f             *maintenanceFixture
	current       *release.Release
	prepares      int
	applies       int
	imageResolves int
	beforeApply   func()
	applyErr      error
	lostReply     bool
	readErr       error
	quiescenceErr error
	appliedValues map[string]any
	created       []cetiadmin.ManagedResource
}

func (h *maintenanceTestHelm) ResolveImageUpgrade(_, _, _, _, image, _ string) (string, error) {
	h.imageResolves++
	return image, nil
}

func (h *maintenanceTestHelm) Prepare(namespace string, values map[string]any, version string) (*helm.PreparedRelease, error) {
	h.prepares++
	chart, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		return nil, err
	}
	chart.Metadata.Version = version
	return helm.PrepareChart(chart, values)
}

func (h *maintenanceTestHelm) Refresh(prepared *helm.PreparedRelease, values map[string]any) (*helm.PreparedRelease, error) {
	return prepared.Rebuild(values)
}

func (h *maintenanceTestHelm) CurrentRelease(context.Context, string, string) (*release.Release, error) {
	if h.readErr != nil {
		return nil, h.readErr
	}
	return h.current, nil
}

func (h *maintenanceTestHelm) MaintenanceQuiescent(context.Context, string, string) error {
	return h.quiescenceErr
}

func (h *maintenanceTestHelm) render(prepared *helm.PreparedRelease, revision int) *release.Release {
	h.f.t.Helper()
	chart, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		h.f.t.Fatal(err)
	}
	chart.Metadata.Version = prepared.ChartVersion
	caps := chartutil.DefaultCapabilities.Copy()
	caps.APIVersions = append(caps.APIVersions, "networking.istio.io/v1/Sidecar")
	namespace, name := instanceNamespace(h.f.agentName), helmReleaseName(h.f.agentName)
	values, err := chartutil.ToRenderValues(chart, prepared.Values, chartutil.ReleaseOptions{Name: name, Namespace: namespace, Revision: revision, IsUpgrade: revision > 1, IsInstall: revision == 1}, caps)
	if err != nil {
		h.f.t.Fatal(err)
	}
	files, err := engine.Render(chart, values)
	if err != nil {
		h.f.t.Fatal(err)
	}
	for key := range files {
		if strings.HasSuffix(key, "/NOTES.txt") {
			delete(files, key)
		}
	}
	hooks, manifests, err := releaseutil.SortManifests(files, caps.APIVersions, releaseutil.InstallOrder)
	if err != nil {
		h.f.t.Fatal(err)
	}
	var contents []string
	for _, manifest := range manifests {
		contents = append(contents, manifest.Content)
	}
	return &release.Release{Name: name, Namespace: namespace, Version: revision, Chart: chart, Config: prepared.Values,
		Info: &release.Info{Status: release.StatusDeployed}, Manifest: strings.Join(contents, "\n---\n"), Hooks: hooks}
}

func (h *maintenanceTestHelm) InstallOrUpgradeMaintenance(ctx context.Context, namespace, name string, prepared *helm.PreparedRelease, revision int, guard func(context.Context) error) error {
	h.applies++
	if h.beforeApply != nil {
		h.beforeApply()
	}
	if guard == nil || h.current.Name != name || h.current.Namespace != namespace || h.current.Version != revision {
		return errors.New("unguarded or stale maintenance apply")
	}
	if err := guard(ctx); err != nil {
		return err
	}
	if h.applyErr != nil {
		return h.applyErr
	}
	next := h.render(prepared, revision+1)
	objects, err := maintenanceManifest(next.Manifest, namespace)
	if err != nil {
		return err
	}
	// Apply the real renderer's stable objects, with API-like UID/RV guards.
	// Hooks remain history only and are never created by this adapter.
	for i := range objects {
		want := &objects[i]
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(want.GroupVersionKind())
		if err := h.f.kube.Get(ctx, client.ObjectKeyFromObject(want), actual); apierrors.IsNotFound(err) {
			if err := guard(ctx); err != nil {
				return err
			}
			if want.GetAnnotations() == nil {
				want.SetAnnotations(map[string]string{})
			}
			annotations := want.GetAnnotations()
			annotations["meta.helm.sh/release-name"], annotations["meta.helm.sh/release-namespace"] = name, namespace
			want.SetAnnotations(annotations)
			labels := want.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels["app.kubernetes.io/managed-by"] = "Helm"
			want.SetLabels(labels)
			if err := h.f.kube.Create(ctx, want); err != nil {
				return err
			}
			h.created = append(h.created, cetiadmin.ManagedResource{APIVersion: want.GetAPIVersion(), Kind: want.GetKind(), ObjectIdentity: identityOf(want)})
			continue
		} else if err != nil {
			return err
		}
		if err := guard(ctx); err != nil {
			return err
		}
		before := actual.DeepCopy()
		for key, value := range want.Object {
			if key != "metadata" && key != "status" {
				actual.Object[key] = value
			}
		}
		labels, annotations := actual.GetLabels(), actual.GetAnnotations()
		for key, value := range want.GetLabels() {
			labels[key] = value
		}
		for key, value := range want.GetAnnotations() {
			annotations[key] = value
		}
		actual.SetLabels(labels)
		actual.SetAnnotations(annotations)
		if err := h.f.kube.Patch(ctx, actual, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	h.current, h.appliedValues = next, prepared.Values
	if h.lostReply {
		return errors.New("synthetic lost Helm acknowledgement")
	}
	return nil
}

func TestHelmMaintenanceRestoresPartialPendingInstall(t *testing.T) {
	for _, missing := range []string{"ConfigMap", "StatefulSet"} {
		t.Run(missing, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.r.Runtime.CetiAdmin = maintenanceRestorationTestAuthority{f.r}
			removed := f.removeRestorationTestObject(missing)
			f.h.current.Info.Status = release.StatusPendingInstall
			f.reach(api.HelmMaintenancePhaseHeld)
			f.h.current.Info.Status = release.StatusFailed // The status-only repair has finished.
			f.updateAgent(func(agent *api.Agent) { agent.Spec.Helm.Maintenance = nil })
			f.reach(api.HelmMaintenancePhaseReapplying)
			if handled, err := f.runMaintenance(); err != nil || !handled {
				t.Fatalf("repaired partial install could not reapply: handled=%v err=%v status=%+v", handled, err, f.agentCurrent().Status.HelmMaintenance)
			}
			if len(f.h.created) != 1 || f.h.created[0].Name != removed.Name || f.h.created[0].UID == "" || f.h.created[0].UID == removed.UID {
				t.Fatal("missing original object was not restored with a new actual UID")
			}
			f.observeStopped()
			f.reach(api.HelmMaintenancePhaseReleased)
			if f.h.applies != 1 || f.h.current.Version != 2 || !f.marked() || !f.agentCurrent().Spec.Suspended {
				t.Fatal("restoration replayed Helm, dropped the namespace hold, or resumed the user")
			}
		})
	}
}

func (f *maintenanceFixture) removeRestorationTestObject(kind string) cetiadmin.ManagedResource {
	f.t.Helper()
	var removed cetiadmin.ManagedResource
	for _, ref := range f.target.Resources {
		if ref.Kind == kind {
			removed = ref
			break
		}
	}
	if removed.UID == "" {
		f.t.Fatalf("real runtime chart did not contain %s", kind)
	}
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(removed.APIVersion)
	object.SetKind(removed.Kind)
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: removed.Name, Namespace: removed.Namespace}, object); err != nil {
		f.t.Fatal(err)
	}
	if err := f.kube.Delete(f.ctx, object, client.Preconditions{UID: ptr.To(object.GetUID()), ResourceVersion: ptr.To(object.GetResourceVersion())}); err != nil {
		f.t.Fatal(err)
	}
	return removed
}

func TestHelmMaintenanceRestorationRejectsUnrestorableObjects(t *testing.T) {
	for _, kind := range []string{"Service", "ServiceAccount", "NetworkPolicy", "ValidatingAdmissionPolicy", "PersistentVolumeClaim"} {
		t.Run(kind, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.r.Runtime.CetiAdmin = maintenanceRestorationTestAuthority{f.r}
			if kind == "ServiceAccount" {
				// The native fixture uses a controller-created SA rather than a
				// chart-owned one. Model a stored manifest that did declare its SA.
				f.h.current.Manifest += "\n---\napiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: missing-runtime-account\n  namespace: " + f.target.Namespace.Name + "\n"
			} else {
				if kind == "PersistentVolumeClaim" {
					f.reach(api.HelmMaintenancePhaseHeld)
				}
				f.removeRestorationTestObject(kind)
			}
			f.h.current.Info.Status = release.StatusPendingInstall
			if kind != "PersistentVolumeClaim" {
				f.reach(api.HelmMaintenancePhaseHeld)
				f.h.current.Info.Status = release.StatusFailed
				f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
				f.reach(api.HelmMaintenancePhaseReapplying)
			}
			if _, err := f.runMaintenance(); err == nil || f.agentCurrent().Status.HelmMaintenance.Phase != api.HelmMaintenancePhaseBlocked {
				t.Fatalf("missing %s was not explicitly rejected: %v", kind, err)
			}
			if f.h.applies != 0 || len(f.h.created) != 0 || !f.marked() {
				t.Fatal("unsupported missing object reached a new Helm revision or lost its hold")
			}
		})
	}
}

func TestHelmMaintenanceRestorationConflictAndUnknownOutcomes(t *testing.T) {
	for _, change := range []string{"foreign name", "changed manifest", "changed revision", "missing admission", "unknown SDK result", "lost success", "lost UID checkpoint"} {
		t.Run(change, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.r.Runtime.CetiAdmin = maintenanceRestorationTestAuthority{f.r}
			ref := f.removeRestorationTestObject("ConfigMap")
			f.h.current.Info.Status = release.StatusPendingInstall
			f.reach(api.HelmMaintenancePhaseHeld)
			_, record, err := f.r.Runtime.readMaintenance(f.ctx, f.agentCurrent())
			if err != nil || record.Restoration == nil || record.Restoration.Objects[0].UID != "" || len(record.Restoration.ManifestHash) != 64 {
				t.Fatal("absence inventory lacks an original manifest/revision anchor", err)
			}
			f.h.current.Info.Status = release.StatusFailed
			f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
			f.reach(api.HelmMaintenancePhaseReapplying)
			var foreign *corev1.ConfigMap
			switch change {
			case "foreign name":
				foreign = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace,
					Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": f.target.Release, "meta.helm.sh/release-namespace": ref.Namespace}}, Data: map[string]string{"unrelated": "preserve"}}
				f.seed(foreign) // Even copied Helm labels cannot adopt a preexisting object.
			case "changed manifest":
				f.h.current.Manifest += "\n# changed after the handoff\n"
			case "changed revision":
				f.h.current.Version++
			case "missing admission":
				f.r.Runtime.CetiAdmin = f.r // No real webhook was installed in this unit fixture.
			case "unknown SDK result":
				f.h.applyErr = errors.New("synthetic unknown apply")
			case "lost success":
				f.h.lostReply = true
			case "lost UID checkpoint":
				f.before = func(operation string, object client.Object) error {
					if operation == "patch" && object.GetName() == f.r.Runtime.maintenanceKey(f.agentCurrent()).Name {
						cm := object.(*corev1.ConfigMap)
						var proposed helmMaintenanceRecord
						if json.Unmarshal([]byte(cm.Data[maintenanceRecordKey]), &proposed) == nil && proposed.Restoration != nil && proposed.Restoration.Objects[0].UID != "" {
							return errors.New("synthetic UID checkpoint failure")
						}
					}
					return nil
				}
			}
			_, err = f.runMaintenance()
			if err == nil || f.agentCurrent().Status.HelmMaintenance.Phase != api.HelmMaintenancePhaseBlocked {
				t.Fatalf("unconfirmed restoration was not Blocked: %v", err)
			}
			if change == "lost success" || change == "lost UID checkpoint" {
				f.h.lostReply, f.before = false, nil
				f.observeStopped()
				f.reach(api.HelmMaintenancePhaseReleased)
				if f.h.applies != 1 || f.h.current.Version != 2 {
					t.Fatal("readback recovery created another revision")
				}
			} else {
				f.h.applyErr = nil
				for range 3 {
					_, _ = f.runMaintenance()
				}
				allowedAttempts := 0
				if change == "unknown SDK result" {
					allowedAttempts = 1
				}
				if f.h.applies != allowedAttempts || len(f.h.created) != 0 || f.agentCurrent().Status.HelmMaintenance.Phase != api.HelmMaintenancePhaseBlocked {
					t.Fatal("conflict or unknown result caused a restoration retry")
				}
			}
			if foreign != nil {
				current := &corev1.ConfigMap{}
				if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(foreign), current); err != nil || current.UID != foreign.UID || !reflect.DeepEqual(current.Data, foreign.Data) {
					t.Fatal("foreign ConfigMap was overwritten or replaced", err)
				}
			}
			if !f.marked() || !f.agentCurrent().Spec.Suspended {
				t.Fatal("restoration removed the user or namespace suspension")
			}
		})
	}
}

// Unit tests synthesize admission readiness; the restoration API-server test
// separately exercises the production webhook and exact checkpoint-reader RBAC.
type maintenanceRestorationTestAuthority struct{ *CetiAdminReconciler }

func (maintenanceRestorationTestAuthority) MaintenanceRestorationReady(context.Context, *api.Agent) error {
	return nil
}

type maintenanceFixture struct {
	*cetiAdminFixture
	agentName string
	h         *maintenanceTestHelm
	gateway   *fakeAIGateway
	token     string
	target    cetiadmin.TargetIdentity
}

func newMaintenanceFixture(t *testing.T, kind api.AgentType) *maintenanceFixture {
	t.Helper()
	f := &maintenanceFixture{cetiAdminFixture: newCetiAdminFixture(t), agentName: "maintenance-target", gateway: newFakeAIGateway()}
	f.converge()
	agent, namespace := f.targetAgent(kind)
	r := f.r.Runtime
	r.CetiAdmin, r.CetiRunHelmValues = f.r, nil
	r.AIGateway.CopilotProxyImage = ""
	r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return f.gateway, nil }
	f.h = &maintenanceTestHelm{f: f}
	r.HelmClient = f.h
	value := "sk-" + strings.Repeat("b", 43)
	f.token = aigateway.Token(value)
	model := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: aiGatewayKeySecretName, Namespace: namespace.Name}, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{aiGatewayKeySecretField: []byte(value)}}
	labelAgentSecret(model, agent)
	f.seed(model)
	f.gateway.keys[f.token] = &aigateway.Key{Alias: aiGatewayKeyAlias(agent), Models: []string{aiGatewaySharedAccessGroup},
		TeamID: aiGatewayTeamID(agent.Spec.Team), UserID: agent.Spec.User.Alias, AllowedRoutes: slices.Clone(aigateway.KeyRoutes)}
	agent.Status = api.AgentStatus{Phase: api.PhaseReady, Namespace: namespace.Name, HelmReleaseName: helmReleaseName(agent.Name),
		AIGateway: &api.AIGatewayStatus{Phase: api.AIGatewayPhaseReady, Routed: true, KeyID: f.token[:aiGatewayKeyIDLength], KeySecretName: aiGatewayKeySecretName,
			AccessGroups: []string{aiGatewaySharedAccessGroup}, Models: []string{testSharedModelOne}, DefaultModel: testSharedModelOne}}
	if err := f.kube.Status().Update(f.ctx, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.GatewayTokenSecretRef != "" {
		f.seed(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.GatewayTokenSecretRef, Namespace: namespace.Name,
			OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}}, Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"CETIRUN_AUTH_KEY": []byte("synthetic-native-auth"), "API_SERVER_KEY": []byte("synthetic-native-auth")}})
	}
	team := &api.Team{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	var prepared *helm.PreparedRelease
	var err error
	if kind == api.AgentTypeCustom {
		prepared = f.prepareInitialCustom(agent, namespace, team, model)
	} else {
		prepared, err = r.prepareAgent(agent, team)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.h.current = f.h.render(prepared, 1)
	objects, err := maintenanceManifest(f.h.current.Manifest, namespace.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.target = cetiadmin.TargetIdentity{Agent: identityOf(agent), Namespace: identityOf(namespace), Team: agent.Spec.Team,
		Runtime: string(kind), Release: helmReleaseName(agent.Name), AuthSecret: agent.Spec.GatewayTokenSecretRef}
	for i := range objects {
		object := &objects[i]
		labels, annotations := object.GetLabels(), object.GetAnnotations()
		if labels == nil {
			labels = map[string]string{}
		}
		if annotations == nil {
			annotations = map[string]string{}
		}
		labels["app.kubernetes.io/managed-by"] = "Helm"
		annotations["meta.helm.sh/release-name"], annotations["meta.helm.sh/release-namespace"] = f.target.Release, namespace.Name
		object.SetLabels(labels)
		object.SetAnnotations(annotations)
		f.seed(object)
		f.target.Resources = append(f.target.Resources, cetiadmin.ManagedResource{APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), ObjectIdentity: identityOf(object)})
	}
	workload := &appsv1.StatefulSet{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: instanceServiceName(agent.Name), Namespace: namespace.Name}, workload); err != nil || len(workload.Spec.VolumeClaimTemplates) != 1 {
		t.Fatal("runtime fixture has no HOME claim template", err)
	}
	home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-" + workload.Name + "-0", Namespace: namespace.Name},
		Spec: *workload.Spec.VolumeClaimTemplates[0].Spec.DeepCopy()}
	if kind == api.AgentTypeCustom {
		home.Annotations = map[string]string{customAgentUID: string(agent.UID)}
	}
	f.seed(home)
	f.target.Resources = append(f.target.Resources, cetiadmin.ManagedResource{APIVersion: "v1", Kind: "PersistentVolumeClaim", ObjectIdentity: identityOf(home)})
	f.updateAgent(func(a *api.Agent) {
		a.Spec.Suspended = true
		a.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-request-one", ExpectedRevision: 1}
	})
	return f
}

func TestHelmMaintenanceDynamicTargetDoesNotRequireRegistration(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw, api.AgentTypeCustom} {
		t.Run(string(kind), func(t *testing.T) {
			f := newMaintenanceFixture(t, kind)
			f.h.readErr = errors.New("target lookup must not read Helm")
			target, err := f.r.MaintenanceTargetForAgent(f.ctx, f.agentCurrent())
			if err != nil {
				t.Fatalf("current Agent requires a global target registration: %v", err)
			}
			if target.Agent != f.target.Agent || target.Namespace != f.target.Namespace || target.Release != f.target.Release || len(target.Resources) != 0 {
				t.Fatalf("new request did not resolve ownership independently of Helm: %+v", target)
			}
			f.h.readErr = nil
			f.reach(api.HelmMaintenancePhaseHeld)
			_, record, err := f.r.Runtime.readMaintenance(f.ctx, f.agentCurrent())
			if err != nil || record == nil || len(record.Target.Resources) == 0 {
				t.Fatalf("new operation did not persist its resource snapshot: %v", err)
			}
		})
	}
}

func TestHelmMaintenanceExistingSnapshotNeverReadsHelm(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	f.reach(api.HelmMaintenancePhaseHeld)
	_, record, err := f.r.Runtime.readMaintenance(f.ctx, f.agentCurrent())
	if err != nil {
		t.Fatal(err)
	}
	f.h.readErr = errors.New("recursive Helm release read")
	target, err := f.r.MaintenanceTargetForAgent(f.ctx, f.agentCurrent())
	if err != nil || !reflect.DeepEqual(target, record.Target) {
		t.Fatalf("existing operation did not retain its snapshot without Helm: %v", err)
	}
	if err := f.r.Runtime.maintenanceGuard(f.ctx, f.agentCurrent(), record); err != nil {
		t.Fatalf("apply guard requires a recursive Helm read: %v", err)
	}
	f.updateAgent(func(agent *api.Agent) { agent.Spec.Helm.Maintenance.RequestID = "different-operation" })
	if target, err := f.r.MaintenanceTargetForAgent(f.ctx, f.agentCurrent()); err == nil || len(target.Resources) != 0 {
		t.Fatal("an unfinished operation supplied resources to a different request")
	}
}

func TestHelmMaintenanceGuardRequiresPersistedSnapshot(t *testing.T) {
	for _, change := range []string{"missing", "request", "resource UID"} {
		t.Run(change, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.reach(api.HelmMaintenancePhaseHeld)
			object, record, err := f.r.Runtime.readMaintenance(f.ctx, f.agentCurrent())
			if err != nil {
				t.Fatal(err)
			}
			saved := *record
			saved.Target.Resources = slices.Clone(record.Target.Resources)
			if change == "missing" {
				if err := f.kube.Delete(f.ctx, object, client.Preconditions{UID: ptr.To(object.UID), ResourceVersion: ptr.To(object.ResourceVersion)}); err != nil {
					t.Fatal(err)
				}
			} else {
				if change == "request" {
					saved.Request.RequestID = "replacement-operation"
				} else {
					saved.Target.Resources[0].UID = "replacement-resource-uid"
				}
				object.Data[maintenanceRecordKey] = string(mustMaintenanceJSON(t, saved))
				if err := f.kube.Update(f.ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			f.h.readErr = errors.New("guard must not rebuild its snapshot from Helm")
			if err := f.r.Runtime.maintenanceGuard(f.ctx, f.agentCurrent(), record); err == nil {
				t.Fatal("cached operation remained writable after its checkpoint changed")
			}
		})
	}
}

func (f *maintenanceFixture) targetAgent(kind api.AgentType) (*api.Agent, *corev1.Namespace) {
	agent, namespace := f.cetiAdminFixture.target(f.agentName, "ordinary-team", kind, kind != api.AgentTypeCustom)
	agent.Spec.Helm.Version = f.r.ChartVersion
	chart, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		f.t.Fatal(err)
	}
	agent.Spec.Agent.Image = ptr.To("registry.example.com/" + string(kind) + "-gateway:" + chart.Metadata.AppVersion)
	agent.Spec.Agent.Port = ptr.To[int32](8642)
	agent.Spec.ModelAccess = &api.ModelAccessSpec{GitHubCopilot: ptr.To(false)}
	agent.Spec.Model = testSharedModelOne
	if kind == api.AgentTypeOpenClaw {
		agent.Spec.Agent.Port = ptr.To[int32](18789)
	}
	if kind == api.AgentTypeCustom {
		agent.Spec.GatewayTokenSecretRef = ""
		agent.Spec.Agent.Username = ptr.To("vibe")
	}
	if err := f.kube.Update(f.ctx, agent); err != nil {
		f.t.Fatal(err)
	}
	return agent, namespace
}

func (f *maintenanceFixture) prepareInitialCustom(agent *api.Agent, namespace *corev1.Namespace, team *api.Team, model *corev1.Secret) *helm.PreparedRelease {
	f.t.Helper()
	r := f.r.Runtime
	r.CetiRunHelmValues = map[string]any{"ttyd": map[string]any{"enabled": false}, "desktop": map[string]any{"enabled": false}}
	namespace.Annotations = map[string]string{customAgentUID: string(agent.UID)}
	if err := f.kube.Update(f.ctx, namespace); err != nil {
		f.t.Fatal(err)
	}
	config := CustomRuntimeConfig{Enabled: true, ApprovedImages: []string{*agent.Spec.Agent.Image}, StorageClass: "default", StorageSize: "10Gi",
		AccessMode: "ReadWriteOnce", FSGroup: 1024, Context: map[string]string{"AGENTS.md": "synthetic operator context", "skills/cetirun-memory/SKILL.md": "synthetic memory instructions"}}
	provider := NewKubernetesCustomBindings(r, config)
	r.CustomBindings = provider
	var bindings runtimebindings.Custom
	var err error
	for range 2 {
		bindings, err = provider.Resolve(f.ctx, agent, team)
		if err == nil {
			break
		}
		agent = f.agentCurrent()
	}
	if err != nil {
		f.t.Fatal(err)
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: f.r.Source.Namespace}, Spec: corev1.ServiceSpec{
		Type: corev1.ServiceTypeClusterIP, ClusterIP: "192.0.2.23", Ports: []corev1.ServicePort{{Port: 4000, Protocol: corev1.ProtocolTCP}},
		Selector: map[string]string{"app.kubernetes.io/name": "cetirun-ai-gateway", "app.kubernetes.io/component": "gateway"}}}
	f.seed(service)
	bindings.Model = runtimebindings.ModelAccess{ObjectReference: objectReference(model), BaseURL: r.AIGateway.URL + "/v1", Key: aiGatewayKeySecretField}
	bindings.Network.Gateway = runtimebindings.Peer{Namespace: service.Namespace, PodLabels: service.Spec.Selector}
	bindings.Network.GatewayPort = 4000
	bindings.Network.Revision += ":gateway:" + string(service.UID)
	values, err := r.customOverrides(agent, team)
	if err != nil {
		f.t.Fatal(err)
	}
	values["agentType"], values["agentRequest"] = "custom", runtimeRequest(agent, "")
	values["agentRequest"].(map[string]any)["releaseName"] = helmReleaseName(agent.Name)
	values["customBindings"], values["memoryOptions"] = bindings, r.memoryOptions()
	values["fullnameOverride"], values["replicaCount"] = instanceServiceName(agent.Name), 1
	values["desktop"] = deepMerge(values["desktop"], map[string]any{"novncPathPrefix": agent.Name + "/desktop/", "image": map[string]any{"repository": r.ChartRegistry + "/cetirun-desktop"}})
	values["ttyd"] = deepMerge(values["ttyd"], map[string]any{"image": map[string]any{"repository": r.ChartRegistry + "/cetirun-terminal"}})
	prepared, err := f.h.Prepare(namespace.Name, values, agent.Spec.Helm.Version)
	if err != nil {
		f.t.Fatal(err)
	}
	return prepared
}

func TestHelmMaintenanceCustomLifecycle(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeCustom)
	f.reach(api.HelmMaintenancePhaseHeld)
	before, err := f.r.Runtime.readCustomOwnership(f.ctx, f.agentCurrent())
	if err != nil {
		t.Fatal(err)
	}
	f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReleased)
	if f.h.applies != 1 || f.h.installCalled || !f.marked() {
		t.Fatal("Custom maintenance did not use the stopped no-hooks apply")
	}
	namespace := &corev1.Namespace{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: f.target.Namespace.Name}, namespace); err != nil {
		t.Fatal(err)
	}
	if len(namespace.OwnerReferences) != 0 {
		t.Fatal("Custom maintenance adopted namespace ownership")
	}
	after, err := f.r.Runtime.readCustomOwnership(f.ctx, f.agentCurrent())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("Custom HOME ownership changed", err)
	}
	f.updateAgent(func(a *api.Agent) { a.Spec.Suspended = false })
	if handled, err := f.runMaintenance(); err != nil || handled || f.marked() {
		t.Fatal("Custom separate resume failed", handled, err)
	}
}

func (f *maintenanceFixture) agentCurrent() *api.Agent {
	f.t.Helper()
	agent := &api.Agent{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: f.agentName}, agent); err != nil {
		f.t.Fatal(err)
	}
	return agent
}

func (f *maintenanceFixture) updateAgent(mutate func(*api.Agent)) {
	f.t.Helper()
	agent := f.agentCurrent()
	mutate(agent)
	agent.Generation++
	if err := f.kube.Update(f.ctx, agent); err != nil {
		f.t.Fatal(err)
	}
}

func (f *maintenanceFixture) runMaintenance() (bool, error) {
	f.t.Helper()
	_, handled, err := f.r.Runtime.reconcileHelmMaintenance(f.ctx, f.agentCurrent())
	return handled, err
}

func (f *maintenanceFixture) observeStopped() {
	f.t.Helper()
	sets := &appsv1.StatefulSetList{}
	deployments := &appsv1.DeploymentList{}
	if err := f.kube.List(f.ctx, sets, client.InNamespace(f.target.Namespace.Name)); err != nil {
		f.t.Fatal(err)
	}
	if err := f.kube.List(f.ctx, deployments, client.InNamespace(f.target.Namespace.Name)); err != nil {
		f.t.Fatal(err)
	}
	for i := range sets.Items {
		set := &sets.Items[i]
		set.Status = appsv1.StatefulSetStatus{ObservedGeneration: set.Generation}
		if err := f.kube.Status().Update(f.ctx, set); err != nil {
			f.t.Fatal(err)
		}
	}
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: deployment.Generation}
		if err := f.kube.Status().Update(f.ctx, deployment); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *maintenanceFixture) reach(phase api.HelmMaintenancePhase) {
	f.t.Helper()
	for range 6 {
		if handled, err := f.runMaintenance(); err != nil || !handled {
			f.t.Fatalf("maintenance: handled=%v error=%v status=%+v", handled, err, f.agentCurrent().Status.HelmMaintenance)
		}
		if status := f.agentCurrent().Status.HelmMaintenance; status != nil && status.Phase == phase {
			return
		}
		f.observeStopped()
	}
	f.t.Fatalf("did not reach %s: %+v", phase, f.agentCurrent().Status.HelmMaintenance)
}

func (f *maintenanceFixture) marked() bool {
	f.t.Helper()
	namespace := &corev1.Namespace{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: f.target.Namespace.Name}, namespace); err != nil {
		f.t.Fatal(err)
	}
	return namespace.Annotations[cetiadmin.MaintenanceAgentUID] == string(f.target.Agent.UID) && namespace.Annotations[cetiadmin.MaintenanceRequestID] != ""
}

func TestHelmMaintenanceFencesDefaultImageUpgrade(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw} {
		for _, phase := range []api.HelmMaintenancePhase{api.HelmMaintenancePhaseHeld, api.HelmMaintenancePhaseReapplying} {
			t.Run(string(kind)+"/"+string(phase), func(t *testing.T) {
				f := newMaintenanceFixture(t, kind)
				f.reach(api.HelmMaintenancePhaseHeld)
				if phase == api.HelmMaintenancePhaseReapplying {
					f.updateAgent(func(agent *api.Agent) { agent.Spec.Helm.Maintenance = nil })
					f.reach(phase)
				}
				before := f.agentCurrent()
				if _, err := f.r.Runtime.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(before)}); err != nil {
					t.Fatal(err)
				}
				after := f.agentCurrent()
				if f.h.imageResolves != 0 || f.h.installCalled || before.UID != after.UID || !reflect.DeepEqual(before.Spec, after.Spec) {
					t.Fatalf("maintenance entered normal image/release reconciliation: resolves=%d installed=%t before=%+v after=%+v",
						f.h.imageResolves, f.h.installCalled, before.Spec, after.Spec)
				}
			})
		}
	}
}

func TestHelmMaintenanceNativeLifecycle(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw} {
		t.Run(string(kind), func(t *testing.T) {
			f := newMaintenanceFixture(t, kind)
			f.reach(api.HelmMaintenancePhaseHeld)
			if !cetiadmin.MaintenanceHeld(f.agentCurrent()) || !f.marked() || f.h.installCalled || f.h.applies != 0 {
				t.Fatal("Held did not fence the current request without normal Helm writes")
			}
			prepared := f.h.prepares
			for range 3 {
				if handled, err := f.runMaintenance(); err != nil || !handled {
					t.Fatal(handled, err)
				}
			}
			if f.h.prepares != prepared || f.h.applies != 0 {
				t.Fatal("Held entered a configuration or Helm write path")
			}
			f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
			f.reach(api.HelmMaintenancePhaseReleased)
			if f.h.applies != 1 || !f.marked() || !f.agentCurrent().Spec.Suspended || f.h.installCalled {
				t.Fatal("release did not retain suspension/fence or used ordinary Helm")
			}
			gateway := f.h.appliedValues["aiGateway"].(map[string]any)
			if gateway["keySecretName"] != aiGatewayKeySecretName || strings.Contains(fmt.Sprint(gateway), "cetirun-preflight") {
				t.Fatal("applied an inert Gateway preflight")
			}
			for range 2 {
				f.reach(api.HelmMaintenancePhaseReleased)
			}
			if f.h.applies != 1 {
				t.Fatal("Released loop resubmitted Helm")
			}
			f.updateAgent(func(a *api.Agent) { a.Spec.Suspended = false })
			resumed := f.agentCurrent()
			if _, handled, err := f.r.Runtime.reconcileHelmMaintenance(f.ctx, resumed); err != nil || handled || f.marked() {
				t.Fatalf("separate resume failed: handled=%v error=%v status=%+v", handled, err, f.agentCurrent().Status.HelmMaintenance)
			}
			if !reflect.DeepEqual(resumed, f.agentCurrent()) {
				t.Fatal("ordinary reconciliation retained the pre-completion Agent snapshot")
			}
			if resumed.Status.HelmMaintenance != nil || slices.ContainsFunc(resumed.Status.Conditions, func(c metav1.Condition) bool {
				return c.Type == api.ConditionHelmMaintenanceReady
			}) {
				t.Fatal("completed maintenance left a stale resume gate")
			}
			if f.h.applies != 1 || !f.gateway.keys[f.token].Blocked {
				t.Fatal("maintenance took over runtime start or unblocked a key")
			}
			if handled, err := f.runMaintenance(); err != nil || handled {
				t.Fatal("resumed handoff continued to own lifecycle", handled, err)
			}
			for _, suspended := range []bool{true, false} {
				f.updateAgent(func(a *api.Agent) { a.Spec.Suspended = suspended })
				if handled, err := f.runMaintenance(); err != nil || handled || f.marked() {
					t.Fatal("completed handoff blocked a later ordinary suspend/resume", handled, err)
				}
			}
		})
	}
}

func TestHelmMaintenancePodTerminationEvidence(t *testing.T) {
	terminated := func(name string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Now()}}}
	}
	base := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "writer", UID: types.UID("71c63201-63d7-4e4f-b9f2-91679eb91010")},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}, InitContainers: []corev1.Container{{Name: "init"}},
			EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{terminated("agent")},
			InitContainerStatuses: []corev1.ContainerStatus{terminated("init")}, EphemeralContainerStatuses: []corev1.ContainerStatus{terminated("debug")}}}
	if !maintenancePodStopped(base) {
		t.Fatal("complete terminated observation was rejected")
	}
	for name, change := range map[string]func(*corev1.Pod){
		"NodeLost":                 func(p *corev1.Pod) { p.Status.Reason = "NodeLost" },
		"missing normal":           func(p *corev1.Pod) { p.Status.ContainerStatuses = nil },
		"missing init":             func(p *corev1.Pod) { p.Status.InitContainerStatuses = nil },
		"missing ephemeral":        func(p *corev1.Pod) { p.Status.EphemeralContainerStatuses = nil },
		"unknown container":        func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Name = "someone-else" },
		"unknown termination":      func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Reason = "ContainerStatusUnknown" },
		"missing termination time": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{} },
		"still running": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		},
		"deleting": func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now },
		"pending":  func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending },
	} {
		t.Run(name, func(t *testing.T) {
			pod := base.DeepCopy()
			change(pod)
			if maintenancePodStopped(pod) {
				t.Fatal("unconfirmed writer accepted")
			}
		})
	}
}

func TestHelmMaintenanceWaitsForObservedCompute(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-home-writer", Namespace: f.target.Namespace.Name},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}, Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "NodeLost"}}
	f.seed(pod)
	for range 3 {
		_, _ = f.runMaintenance()
		f.observeStopped()
		if status := f.agentCurrent().Status.HelmMaintenance; status.Phase != api.HelmMaintenancePhaseBlocked || cetiadmin.MaintenanceHeld(f.agentCurrent()) || !f.marked() {
			t.Fatal("unknown writer acquired Held", status)
		}
	}
	pod.Status.Reason = ""
	if err := f.kube.Status().Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	_, _ = f.runMaintenance()
	if cetiadmin.MaintenanceHeld(f.agentCurrent()) {
		t.Fatal("Pod phase alone proved writer shutdown")
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: metav1.Now()}}}}
	if err := f.kube.Status().Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.reach(api.HelmMaintenancePhaseHeld)
}

func TestHelmMaintenanceHookObservations(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	var hook *release.Hook
	for _, candidate := range f.h.current.Hooks {
		if candidate.Kind == "Job" {
			hook = candidate
		}
	}
	if hook == nil {
		t.Fatal("real chart no longer supplies the hook under test")
	}
	hook.LastRun = release.HookExecution{StartedAt: helmtime.Now(), Phase: release.HookPhaseRunning}
	objects, err := maintenanceManifest(hook.Manifest, f.target.Namespace.Name)
	if err != nil || len(objects) != 1 {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := json.Unmarshal(mustMaintenanceJSON(t, objects[0].Object), job); err != nil {
		t.Fatal(err)
	}
	job.Status.Active = 1
	f.seed(job)
	_, _ = f.runMaintenance()
	f.observeStopped()
	_, _ = f.runMaintenance()
	if cetiadmin.MaintenanceHeld(f.agentCurrent()) || f.h.applies != 0 || !f.marked() {
		t.Fatal("running old hook did not block the handoff")
	}
	hook.LastRun.CompletedAt, hook.LastRun.Phase = helmtime.Now(), release.HookPhaseSucceeded
	_, _ = f.runMaintenance()
	if cetiadmin.MaintenanceHeld(f.agentCurrent()) {
		t.Fatal("Helm's success flag overrode a live Job")
	}
	job.Status.Active = 0
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := f.kube.Status().Update(f.ctx, job); err != nil {
		t.Fatal(err)
	}
	f.reach(api.HelmMaintenancePhaseHeld)
	_, record, err := f.r.Runtime.readMaintenance(f.ctx, f.agentCurrent())
	if err != nil || record == nil || len(f.target.Resources) != len(record.Target.Resources) {
		t.Fatal("transient hook identity changed the operation snapshot", err)
	}
}

func mustMaintenanceJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestHelmMaintenanceUnknownOperationNeverTimesOutToHeld(t *testing.T) {
	for _, phase := range []release.Status{release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback, release.StatusUnknown, release.StatusUninstalling} {
		t.Run(string(phase), func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeOpenClaw)
			f.h.current.Info.Status = phase
			if phase.IsPending() {
				f.h.quiescenceErr = errors.New("pending operation has no completion proof")
			}
			for range 3 {
				_, _ = f.runMaintenance()
				f.observeStopped()
			}
			if status := f.agentCurrent().Status.HelmMaintenance; status.Phase != api.HelmMaintenancePhaseBlocked || !f.marked() || f.h.applies != 0 {
				t.Fatal("unknown operation was treated as finished", status)
			}
		})
	}
}

func TestHelmMaintenanceKnownPendingRepairHandoff(t *testing.T) {
	for _, phase := range []release.Status{release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback} {
		t.Run(string(phase), func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.h.current.Info.Status = phase
			f.reach(api.HelmMaintenancePhaseHeld)
			if f.h.applies != 0 || f.h.current.Info.Status != phase || !f.marked() {
				t.Fatal("pending repair handoff modified history or invoked Helm")
			}
		})
	}
}

func TestHelmMaintenanceFirstPendingInstallUsesOnlyActualObjects(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.h.current.Info.Status = release.StatusPendingInstall
			if missing {
				for _, ref := range f.target.Resources {
					object := &unstructured.Unstructured{}
					object.SetAPIVersion(ref.APIVersion)
					object.SetKind(ref.Kind)
					if err := f.kube.Get(f.ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, object); err != nil {
						t.Fatal(err)
					}
					if err := f.kube.Delete(f.ctx, object, client.Preconditions{UID: ptr.To(object.GetUID()), ResourceVersion: ptr.To(object.GetResourceVersion())}); err != nil {
						t.Fatal(err)
					}
				}
			}
			agent := f.agentCurrent()
			agent.Status.Namespace, agent.Status.HelmReleaseName = "", ""
			if err := f.kube.Status().Update(f.ctx, agent); err != nil {
				t.Fatal(err)
			}
			f.reach(api.HelmMaintenancePhaseHeld)
			agent = f.agentCurrent()
			if agent.Status.Namespace != f.target.Namespace.Name || agent.Status.HelmReleaseName != f.target.Release || f.h.applies != 0 {
				t.Fatal("first-install status was not bound from current namespace ownership")
			}
			_, record, err := f.r.Runtime.readMaintenance(f.ctx, agent)
			if err != nil || record == nil || (len(record.Target.Resources) == 0) != missing {
				t.Fatal("first operation did not snapshot exactly the currently existing runtime objects", err)
			}
		})
	}
}

func TestHelmMaintenanceHeldKeepsAnchorDuringNativeRollback(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	f.reach(api.HelmMaintenancePhaseHeld)
	set := &appsv1.StatefulSet{}
	key := client.ObjectKey{Name: instanceServiceName(f.agentName), Namespace: f.target.Namespace.Name}
	if err := f.kube.Get(f.ctx, key, set); err != nil {
		t.Fatal(err)
	}
	set.Spec.Replicas = ptr.To[int32](1)
	if err := f.kube.Update(f.ctx, set); err != nil {
		t.Fatal(err)
	}
	f.h.current.Version, f.h.current.Info.Status = 2, release.StatusPendingRollback
	prepares, writes := f.h.prepares, len(f.writes)
	if handled, err := f.runMaintenance(); !handled || err != nil {
		t.Fatal(handled, err)
	}
	status := f.agentCurrent().Status.HelmMaintenance
	if !cetiadmin.MaintenanceHeld(f.agentCurrent()) || status.Revision != 1 || f.h.prepares != prepares || len(f.writes) != writes || f.h.applies != 0 {
		t.Fatal("native rollback lost its anchor or resumed Operator writes", status)
	}
	f.h.current.Info.Status = release.StatusDeployed
	f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReleased)
	if f.agentCurrent().Status.HelmMaintenance.Revision != 3 || !f.marked() {
		t.Fatal("reapplication did not follow the native revision")
	}
}

func TestHelmMaintenanceCheckpointAndPatchFailures(t *testing.T) {
	for _, failure := range []string{"failed checkpoint create", "lost checkpoint create", "namespace conflict"} {
		t.Run(failure, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.before = func(operation string, object client.Object) error {
				if failure == "failed checkpoint create" && operation == "create" && strings.HasPrefix(object.GetName(), "cetiadmin-maintenance-") {
					return errors.New("synthetic checkpoint failure")
				}
				if failure == "namespace conflict" && operation == "patch" && object.GetName() == f.target.Namespace.Name {
					return apierrors.NewConflict(corev1.Resource("namespaces"), object.GetName(), errors.New("synthetic conflict"))
				}
				return nil
			}
			f.after = func(operation string, object client.Object) error {
				if failure == "lost checkpoint create" && operation == "create" && strings.HasPrefix(object.GetName(), "cetiadmin-maintenance-") {
					return errors.New("synthetic lost create reply")
				}
				return nil
			}
			if handled, err := f.runMaintenance(); !handled || err == nil || cetiadmin.MaintenanceHeld(f.agentCurrent()) || f.h.applies != 0 {
				t.Fatal("failed persistence was treated as a handoff", handled, err)
			}
			if failure != "namespace conflict" {
				set := &appsv1.StatefulSet{}
				if err := f.kube.Get(f.ctx, client.ObjectKey{Name: instanceServiceName(f.agentName), Namespace: f.target.Namespace.Name}, set); err != nil || ptr.Deref(set.Spec.Replicas, 1) != 0 || !f.marked() {
					t.Fatal("failed checkpoint left compute without a stop request", err)
				}
			}
			f.before, f.after = nil, nil
			f.observeStopped()
			f.reach(api.HelmMaintenancePhaseHeld)
		})
	}
}

func TestHelmMaintenanceLostApplyReplyUsesReadbackNotRetry(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	f.reach(api.HelmMaintenancePhaseHeld)
	f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReapplying)
	f.h.lostReply = true
	if handled, err := f.runMaintenance(); !handled || err == nil || f.h.applies != 1 || f.h.current.Version != 2 || !f.marked() {
		t.Fatal("did not exercise a lost successful apply reply", handled, err)
	}
	f.observeStopped()
	f.h.lostReply = false
	f.reach(api.HelmMaintenancePhaseReleased)
	if f.h.applies != 1 {
		t.Fatal("unknown response caused a second Helm operation")
	}
}

func TestHelmMaintenanceUnknownApplyDoesNotRetry(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	f.reach(api.HelmMaintenancePhaseHeld)
	f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReapplying)
	f.h.applyErr = errors.New("unknown SDK failure")
	_, _ = f.runMaintenance()
	f.h.applyErr = nil
	for range 3 {
		_, _ = f.runMaintenance()
	}
	if f.h.applies != 1 || f.agentCurrent().Status.HelmMaintenance.Phase != api.HelmMaintenancePhaseBlocked || !f.marked() {
		t.Fatal("unknown apply was resubmitted or released")
	}
}

func TestHelmMaintenanceUnsubmittedApplyCanRetry(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	f.reach(api.HelmMaintenancePhaseHeld)
	f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReapplying)
	f.h.applyErr = errors.Join(helm.ErrMaintenanceNotSubmitted, errors.New("synthetic preflight read failure"))
	if handled, err := f.runMaintenance(); !handled || err == nil || f.h.applies != 1 || f.h.current.Version != 1 || !f.marked() {
		t.Fatal("preflight failure did not retain the stopped target", handled, err)
	}
	f.h.applyErr = nil
	f.reach(api.HelmMaintenancePhaseReleased)
	if f.h.applies != 2 || f.h.current.Version != 2 || !f.agentCurrent().Spec.Suspended || !f.marked() {
		t.Fatal("known unsubmitted failure did not retry exactly one stopped revision")
	}
}

func TestHelmMaintenanceExternalKeyBlockSurvivesRelease(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	key := f.gateway.keys[f.token]
	key.Blocked, key.MaxBudget, key.Spend = true, ptr.To(19.0), ptr.To(3.0)
	reset := time.Now().Add(time.Hour).UTC()
	key.BudgetResetAt = &reset
	agent := f.agentCurrent()
	agent.Status.AIGateway.Reason = "EmergencyAdministratorBlock"
	if err := f.kube.Status().Update(f.ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.reach(api.HelmMaintenancePhaseHeld)
	f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReleased)
	if f.agentCurrent().Status.AIGateway.Reason != "EmergencyAdministratorBlock" || !key.Blocked || *key.MaxBudget != 19 || *key.Spend != 3 || !key.BudgetResetAt.Equal(reset) {
		t.Fatal("maintenance took ownership of the external key block or reset spend")
	}
	f.updateAgent(func(a *api.Agent) { a.Spec.Suspended = false })
	if handled, err := f.runMaintenance(); !handled || err == nil || !f.marked() || !key.Blocked {
		t.Fatal("external block was bypassed during resume", handled, err)
	}
}

func TestHelmMaintenanceRejectsReplacedNamespaceAndWorkload(t *testing.T) {
	for _, resource := range []string{"namespace", "workload"} {
		t.Run(resource, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.reach(api.HelmMaintenancePhaseHeld)
			var object client.Object = &corev1.Namespace{}
			key := client.ObjectKey{Name: f.target.Namespace.Name}
			if resource == "workload" {
				object = &appsv1.StatefulSet{}
				key = client.ObjectKey{Name: instanceServiceName(f.agentName), Namespace: f.target.Namespace.Name}
			}
			if err := f.kube.Get(f.ctx, key, object); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Delete(f.ctx, object, client.Preconditions{UID: ptr.To(object.GetUID()), ResourceVersion: ptr.To(object.GetResourceVersion())}); err != nil {
				t.Fatal(err)
			}
			object.SetUID("")
			object.SetResourceVersion("")
			object.SetAnnotations(map[string]string{"unrelated": "preserve"})
			if set, ok := object.(*appsv1.StatefulSet); ok {
				set.Spec.Replicas = ptr.To[int32](1)
			}
			f.seed(object)
			writes := len(f.writes)
			_, _ = f.runMaintenance()
			if cetiadmin.MaintenanceHeld(f.agentCurrent()) || f.h.applies != 0 || len(f.writes) != writes {
				t.Fatal("replaced object was adopted or mutated")
			}
		})
	}
}

func TestHelmMaintenanceCurrentOwnershipAndDependencies(t *testing.T) {
	for _, change := range []string{"nil admission", "nil provider", "nil Helm adapter", "platform", "registry", "broker", "policy Team", "migration", "foreign namespace", "self"} {
		t.Run(change, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			switch change {
			case "nil admission":
				f.r.AdmissionReady = nil
			case "nil provider":
				f.r.Runtime.CetiAdmin = nil
			case "nil Helm adapter":
				f.r.Runtime.HelmClient = &suspensionHelmClient{}
			case "platform":
				f.r.Runtime.OperatorNamespace = f.target.Namespace.Name
			case "registry":
				f.r.Runtime.RegistryCredentialNamespace = f.target.Namespace.Name
			case "broker":
				f.r.Runtime.ConnectionsMCPBrokerNamespace = f.target.Namespace.Name
			case "policy Team":
				team := &api.Team{}
				if err := f.kube.Get(f.ctx, client.ObjectKey{Name: f.target.Team}, team); err != nil {
					t.Fatal(err)
				}
				team.Spec.AgentPolicyRef = "restricted"
				if err := f.kube.Update(f.ctx, team); err != nil {
					t.Fatal(err)
				}
			case "migration":
				f.config.MigrationSource = &cetiadmin.MigrationSource{Agent: f.target.Agent, Namespace: f.target.Namespace}
				f.updateConfig()
			case "foreign namespace":
				namespace := &corev1.Namespace{}
				if err := f.kube.Get(f.ctx, client.ObjectKey{Name: f.target.Namespace.Name}, namespace); err != nil {
					t.Fatal(err)
				}
				namespace.OwnerReferences = nil
				if err := f.kube.Update(f.ctx, namespace); err != nil {
					t.Fatal(err)
				}
			case "self":
				f.agentName = f.config.InstanceName
				f.updateAgent(func(a *api.Agent) {
					a.Spec.Suspended = true
					a.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-self-reject", ExpectedRevision: 1}
				})
			}
			_, _ = f.runMaintenance()
			if status := f.agentCurrent().Status.HelmMaintenance; status == nil || status.Phase != api.HelmMaintenancePhaseBlocked || f.h.applies != 0 || (change != "nil Helm adapter" && f.marked()) {
				t.Fatal("out-of-scope or unavailable authority became a handoff", status)
			}
		})
	}
}

func TestHelmMaintenanceNewRequestCapturesCurrentResourceUIDs(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	f.reach(api.HelmMaintenancePhaseHeld)
	f.updateAgent(func(agent *api.Agent) { agent.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReleased)
	f.updateAgent(func(agent *api.Agent) { agent.Spec.Suspended = false })
	if handled, err := f.runMaintenance(); err != nil || handled {
		t.Fatal("previous operation did not resume", handled, err)
	}
	var previous cetiadmin.ManagedResource
	for _, ref := range f.target.Resources {
		if ref.Kind == "ConfigMap" {
			previous = ref
			break
		}
	}
	if previous.UID == "" {
		t.Fatal("fixture has no runtime ConfigMap")
	}
	object := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: previous.Name, Namespace: previous.Namespace}, object); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(f.ctx, object, client.Preconditions{UID: ptr.To(object.UID), ResourceVersion: ptr.To(object.ResourceVersion)}); err != nil {
		t.Fatal(err)
	}
	object.UID, object.ResourceVersion = "", ""
	f.seed(object)
	f.updateAgent(func(agent *api.Agent) {
		agent.Spec.Suspended = true
		agent.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "fresh-snapshot-operation", ExpectedRevision: 2}
	})
	target, err := f.r.MaintenanceTargetForAgent(f.ctx, f.agentCurrent())
	if err != nil || len(target.Resources) != 0 {
		t.Fatal("new request inherited the previous operation's resources", err)
	}
	f.reach(api.HelmMaintenancePhaseHeld)
	_, record, err := f.r.Runtime.readMaintenance(f.ctx, f.agentCurrent())
	if err != nil || record == nil || record.Request.RequestID != "fresh-snapshot-operation" {
		t.Fatal("new operation snapshot was not persisted", err)
	}
	ref, found := maintenanceResource(record.Target, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": object.Name, "namespace": object.Namespace}}})
	if !found || ref.UID != object.UID || ref.UID == previous.UID {
		t.Fatal("new operation reused an old resource UID")
	}
}

func TestHelmMaintenanceDisabledSourceCanFinishExistingHandoff(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	f.reach(api.HelmMaintenancePhaseHeld)
	f.config.Enabled = false
	f.updateConfig()
	f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReleased)
	if !f.marked() || f.h.applies != 1 {
		t.Fatal("component disable deadlocked target recovery")
	}
}

type maintenanceStorageOnly struct {
	HelmInstaller
	backend helmMaintenanceClient
}

func (h maintenanceStorageOnly) CurrentRelease(ctx context.Context, namespace, name string) (*release.Release, error) {
	return h.backend.CurrentRelease(ctx, namespace, name)
}

func (h maintenanceStorageOnly) InstallOrUpgradeMaintenance(ctx context.Context, namespace, name string, prepared *helm.PreparedRelease, revision int, guard func(context.Context) error) error {
	return h.backend.InstallOrUpgradeMaintenance(ctx, namespace, name, prepared, revision, guard)
}

func TestHelmMaintenanceRequiresPositiveQuiescence(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(fmt.Sprint(absent), func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.h.quiescenceErr = errors.New("previous leader write completion is unknown")
			if absent {
				f.r.Runtime.HelmClient = maintenanceStorageOnly{HelmInstaller: f.h, backend: f.h}
			}
			for range 3 {
				_, _ = f.runMaintenance()
				f.observeStopped()
			}
			if cetiadmin.MaintenanceHeld(f.agentCurrent()) || !f.marked() || f.h.applies != 0 {
				t.Fatal("stored deployed status substituted for old-writer completion")
			}
			f.r.Runtime.HelmClient, f.h.quiescenceErr = f.h, nil
			f.reach(api.HelmMaintenancePhaseHeld)
		})
	}
}

func TestHelmMaintenanceRequiresCurrentAdmissionCoverage(t *testing.T) {
	for _, scenario := range []string{"add missing label", "foreign label", "lost while Held", "lost marker write readback"} {
		t.Run(scenario, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			if scenario == "lost while Held" {
				f.reach(api.HelmMaintenancePhaseHeld)
			}
			namespace := &corev1.Namespace{}
			if err := f.kube.Get(f.ctx, client.ObjectKey{Name: f.target.Namespace.Name}, namespace); err != nil {
				t.Fatal(err)
			}
			namespace.Labels["unrelated.example.com/preserved"] = "yes"
			delete(namespace.Labels, maintenanceCoverageLabel)
			if scenario == "foreign label" {
				namespace.Labels[maintenanceCoverageLabel] = "different-controller"
			}
			if err := f.kube.Update(f.ctx, namespace); err != nil {
				t.Fatal(err)
			}
			if scenario == "lost marker write readback" {
				f.after = func(operation string, object client.Object) error {
					if operation == "patch" && object.GetName() == namespace.Name {
						actual := &corev1.Namespace{}
						if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(object), actual); err != nil {
							return err
						}
						delete(actual.Labels, maintenanceCoverageLabel)
						return f.kube.Update(f.ctx, actual)
					}
					return nil
				}
			}
			if scenario == "add missing label" {
				f.reach(api.HelmMaintenancePhaseHeld)
			} else {
				_, _ = f.runMaintenance()
				if cetiadmin.MaintenanceHeld(f.agentCurrent()) || f.h.applies != 0 {
					t.Fatal("namespace outside the admission selector acquired Held")
				}
			}
			if err := f.kube.Get(f.ctx, client.ObjectKey{Name: namespace.Name}, namespace); err != nil || namespace.Labels["unrelated.example.com/preserved"] != "yes" {
				t.Fatal("unrelated namespace metadata was cleared", err)
			}
		})
	}
}

func TestHelmMaintenanceNewRequestWhileUserRemainsSuspended(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	f.reach(api.HelmMaintenancePhaseHeld)
	f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReleased)
	f.updateAgent(func(a *api.Agent) {
		a.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-request-two", ExpectedRevision: 2}
	})
	f.reach(api.HelmMaintenancePhaseHeld)
	if !f.marked() || f.h.applies != 1 || !f.agentCurrent().Spec.Suspended || f.agentCurrent().Status.HelmMaintenance.RequestID != "maintenance-request-two" {
		t.Fatal("a fresh handoff lost the user's pause or resumed between requests")
	}
}

func TestHelmMaintenanceRejectsRequestAndConfigurationChanges(t *testing.T) {
	for _, mutation := range []string{"request id", "revision", "spec", "metadata", "early resume"} {
		t.Run(mutation, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.reach(api.HelmMaintenancePhaseHeld)
			f.updateAgent(func(a *api.Agent) {
				switch mutation {
				case "request id":
					a.Spec.Helm.Maintenance.RequestID = "different-request-identity"
				case "revision":
					a.Spec.Helm.Maintenance.ExpectedRevision++
				case "spec":
					a.Spec.Model = "different-model"
				case "metadata":
					if a.Annotations == nil {
						a.Annotations = map[string]string{}
					}
					a.Annotations[api.AnnotationCopilotModels] = "[]"
				case "early resume":
					a.Spec.Suspended = false
					a.Spec.Helm.Maintenance = nil
				}
			})
			_, _ = f.runMaintenance()
			if cetiadmin.MaintenanceHeld(f.agentCurrent()) || f.agentCurrent().Status.HelmMaintenance.Phase != api.HelmMaintenancePhaseBlocked || !f.marked() || f.h.applies != 0 {
				t.Fatal("changed handoff intent was accepted")
			}
		})
	}
}

func TestHelmMaintenanceFinalApplyGuardAndResumeReadback(t *testing.T) {
	for _, boundary := range []string{"before apply", "before resume"} {
		t.Run(boundary, func(t *testing.T) {
			f := newMaintenanceFixture(t, api.AgentTypeHermes)
			f.reach(api.HelmMaintenancePhaseHeld)
			f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
			if boundary == "before apply" {
				f.reach(api.HelmMaintenancePhaseReapplying)
				f.h.beforeApply = func() { f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Version = "0.1.1-changed" }) }
				_, _ = f.runMaintenance()
				if f.h.current.Version != 1 || !f.marked() {
					t.Fatal("changed final intent reached Helm writes")
				}
			} else {
				f.reach(api.HelmMaintenancePhaseReleased)
				set := &appsv1.StatefulSet{}
				if err := f.kube.Get(f.ctx, client.ObjectKey{Name: instanceServiceName(f.agentName), Namespace: f.target.Namespace.Name}, set); err != nil {
					t.Fatal(err)
				}
				set.Spec.Template.Spec.Containers[0].Image = "registry.example.com/foreign:1"
				if err := f.kube.Update(f.ctx, set); err != nil {
					t.Fatal(err)
				}
				f.updateAgent(func(a *api.Agent) { a.Spec.Suspended = false })
				if handled, _ := f.runMaintenance(); !handled || !f.marked() || f.h.applies != 1 {
					t.Fatal("Released status bypassed current API configuration readback")
				}
			}
		})
	}
}

func TestHelmMaintenanceCustomUnknownCompletionIsBlocked(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeCustom)
	agent := f.agentCurrent()
	completion := customCompletion{Version: "v1", AgentName: agent.Name, AgentUID: agent.UID,
		Operation: CustomApplyOperation{ID: "unknown-custom-operation", PlanKey: "unknown-plan", Generation: agent.Generation}, Outcome: CustomApplyUnknown}
	key := f.r.Runtime.completionKey(agent)
	f.seed(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}},
		Data: map[string]string{"completion": string(mustMaintenanceJSON(t, completion))}})
	for range 2 {
		_, _ = f.runMaintenance()
		f.observeStopped()
	}
	if cetiadmin.MaintenanceHeld(f.agentCurrent()) || !f.marked() || f.h.applies != 0 {
		t.Fatal("unknown Custom lifecycle completion was treated as released storage ownership")
	}
	if _, pending, err := f.r.Runtime.readCustomCompletion(f.ctx, agent); err != nil || pending == nil || pending.Outcome != CustomApplyUnknown {
		t.Fatal("maintenance discarded the unknown Custom completion", err)
	}
}

func TestHelmMaintenanceCheckpointIsNonSecretAndPreservesHOME(t *testing.T) {
	f := newMaintenanceFixture(t, api.AgentTypeHermes)
	f.reach(api.HelmMaintenancePhaseHeld)
	f.updateAgent(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
	f.reach(api.HelmMaintenancePhaseReleased)
	object, record, err := f.r.Runtime.readMaintenance(f.ctx, f.agentCurrent())
	if err != nil {
		t.Fatal(err)
	}
	raw := object.Data[maintenanceRecordKey]
	for _, forbidden := range []string{"sk-" + strings.Repeat("b", 43), f.token, "synthetic-native-auth", "master-key", "manifest", "command", "history"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("checkpoint contains %s", forbidden)
		}
	}
	if record.Target.Agent.UID != f.target.Agent.UID || record.Target.Namespace.UID != f.target.Namespace.UID || record.PreparedHash == "" || record.ValuesHash == "" {
		t.Fatal("checkpoint lacks identity and configuration evidence")
	}
	for _, ref := range record.Target.Resources {
		if ref.Kind == "PersistentVolumeClaim" {
			home := &corev1.PersistentVolumeClaim{}
			if err := f.kube.Get(f.ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, home); err != nil || home.UID != ref.UID || home.DeletionTimestamp != nil {
				t.Fatal("maintenance did not retain the HOME identity", err)
			}
		}
	}
}
