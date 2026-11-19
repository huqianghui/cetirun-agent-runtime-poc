// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Uses the actual installation controller, CRDs, RBAC and served admission.
// Gateway and Helm history are synthetic. Kubernetes mutations use the pinned
// Helm kube.Client.Update, including its real GET/NotFound/POST path.
func TestHelmMaintenanceRestorationAPIServer(t *testing.T) {
	for _, missing := range []string{"ConfigMap", "StatefulSet", "Deployment"} {
		t.Run(missing, func(t *testing.T) {
			h := newCetiAdminInstallationAPI(t, true)
			agent, namespace := h.target(t, "restore-target", "engineering", api.AgentTypeHermes, true)
			agent.Spec.Agent.Image, agent.Spec.Agent.Port = ptr.To("registry.example.com/hermes:1"), ptr.To[int32](8642)
			agent.Spec.Helm.Version = h.installer.ChartVersion
			agent.Spec.Model = testSharedModelOne
			if err := h.operator.Update(h.ctx, agent); err != nil {
				t.Fatal(err)
			}
			original := restorationAPIObjects(agent, namespace.Name)
			var missingName string
			for i := range original {
				if original[i].GetKind() == missing {
					missingName = original[i].GetName()
					continue
				}
				h.create(t, original[i].DeepCopy())
			}
			if missingName == "" {
				t.Fatal("missing-object fixture was not selected")
			}
			name := helmReleaseName(agent.Name)
			h.helm.releases[namespace.Name+"/"+name] = &release.Release{Name: name, Namespace: namespace.Name, Version: 1,
				Info: &release.Info{Status: release.StatusPendingInstall}, Manifest: restorationAPIManifest(t, original)}
			h.converge(t) // No handcrafted Active installation or target binding.
			r := h.installer.Runtime
			checkpoint := r.maintenanceKey(agent)
			h.assertPermission(t, h.admission, "get", "", "configmaps", "", checkpoint.Namespace, checkpoint.Name, false)
			gateway := newFakeAIGateway()
			keyValue := "sk-" + strings.Repeat("c", 43)
			token := aigateway.Token(keyValue)
			model := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: aiGatewayKeySecretName, Namespace: namespace.Name}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{aiGatewayKeySecretField: []byte(keyValue)}}
			labelAgentSecret(model, agent)
			h.create(t, model)
			h.create(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.GatewayTokenSecretRef, Namespace: namespace.Name, OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}},
				Data: map[string][]byte{"API_SERVER_KEY": []byte("synthetic-native-auth"), "CETIRUN_AUTH_KEY": []byte("synthetic-native-auth")}})
			gateway.keys[token] = &aigateway.Key{Alias: aiGatewayKeyAlias(agent), Models: []string{aiGatewaySharedAccessGroup}, TeamID: aiGatewayTeamID(agent.Spec.Team), UserID: agent.Spec.User.Alias, AllowedRoutes: slices.Clone(aigateway.KeyRoutes)}
			r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return gateway, nil }
			agent.Status.AIGateway = &api.AIGatewayStatus{Phase: api.AIGatewayPhaseReady, Routed: true, KeyID: token[:aiGatewayKeyIDLength], KeySecretName: aiGatewayKeySecretName,
				AccessGroups: []string{aiGatewaySharedAccessGroup}, Models: []string{testSharedModelOne}, DefaultModel: testSharedModelOne}
			if err := h.operator.Status().Update(h.ctx, agent); err != nil {
				t.Fatal(err)
			}
			backend := &restorationAPIHelm{cetiAdminInstallationHelm: h.helm, namespace: namespace.Name, original: original, t: t,
				config: h.accountConfig(t, h.source.Namespace, "controller", nil), lostReply: missing == "Deployment"}
			r.HelmClient = backend
			current := func() *api.Agent {
				t.Helper()
				result := &api.Agent{}
				if err := h.operator.Get(h.ctx, client.ObjectKeyFromObject(agent), result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			update := func(edit func(*api.Agent)) {
				t.Helper()
				latest := current()
				before := latest.DeepCopy()
				edit(latest)
				if err := h.admin.Patch(h.ctx, latest, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					t.Fatal(err)
				}
			}
			observe := func() {
				t.Helper()
				for _, list := range []client.ObjectList{&appsv1.StatefulSetList{}, &appsv1.DeploymentList{}} {
					if err := h.operator.List(h.ctx, list, client.InNamespace(namespace.Name)); err != nil {
						t.Fatal(err)
					}
					switch list := list.(type) {
					case *appsv1.StatefulSetList:
						for i := range list.Items {
							list.Items[i].Status = appsv1.StatefulSetStatus{ObservedGeneration: list.Items[i].Generation}
							if err := h.admin.Status().Update(h.ctx, &list.Items[i]); err != nil {
								t.Fatal(err)
							}
						}
					case *appsv1.DeploymentList:
						for i := range list.Items {
							list.Items[i].Status = appsv1.DeploymentStatus{ObservedGeneration: list.Items[i].Generation}
							if err := h.admin.Status().Update(h.ctx, &list.Items[i]); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
			}
			reconcile := func() error {
				_, err := r.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
				return err
			}
			runUntil := func(phase api.HelmMaintenancePhase) {
				t.Helper()
				for range 8 {
					if err := reconcile(); err != nil {
						t.Fatalf("Agent reconciliation: err=%v maintenance=%+v", err, current().Status.HelmMaintenance)
					}
					if status := current().Status.HelmMaintenance; status != nil && status.Phase == phase {
						return
					}
					observe()
				}
				t.Fatalf("maintenance never reached %s: %+v", phase, current().Status.HelmMaintenance)
			}
			update(func(a *api.Agent) {
				a.Spec.Suspended = true
				a.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "restoration-api-request", ExpectedRevision: 1}
			})
			runUntil(api.HelmMaintenancePhaseHeld)
			_, held, err := r.readMaintenance(h.ctx, current())
			if err != nil || held.Restoration == nil || len(held.Restoration.Objects) != 1 || held.Restoration.Objects[0].UID != "" {
				t.Fatalf("Held did not record actual absence without inventing a UID: %v", err)
			}
			h.assertPermission(t, h.admission, "get", "", "configmaps", "", checkpoint.Namespace, checkpoint.Name, true)
			h.assertPermission(t, h.admission, "get", "", "configmaps", "", checkpoint.Namespace, "unrelated-maintenance-record", false)
			if err := h.installer.MaintenanceRestorationReady(h.ctx, current()); err != nil {
				t.Fatalf("production Chart and scoped reader do not permit restoration: %v", err)
			}
			backend.releases[namespace.Name+"/"+name].Info.Status = release.StatusFailed // Synthetic successful status-only repair.
			update(func(a *api.Agent) { a.Spec.Helm.Maintenance = nil })
			runUntil(api.HelmMaintenancePhaseReapplying)
			applyErr := reconcile()
			if backend.lostReply {
				if applyErr == nil {
					t.Fatal("lost response was not exercised")
				}
				backend.lostReply = false
			} else if applyErr != nil {
				t.Fatal(applyErr)
			}
			observe()
			if err := reconcile(); err != nil {
				t.Fatal(err)
			}
			_, restored, err := r.readMaintenance(h.ctx, current())
			if err != nil || restored.Restoration.Objects[0].UID == "" {
				t.Fatalf("actual restored UID was not persisted: %v", err)
			}
			runUntil(api.HelmMaintenancePhaseReleased)
			if backend.applies != 1 || backend.releases[namespace.Name+"/"+name].Version != 2 || !current().Spec.Suspended {
				t.Fatal("restoration replayed a revision or resumed compute")
			}
			if err := h.operator.Get(h.ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil || namespace.Annotations[cetiadmin.MaintenanceAgentUID] != string(agent.UID) {
				t.Fatalf("Released removed the namespace startup hold: %v", err)
			}
			object := &unstructured.Unstructured{}
			object.SetAPIVersion(restored.Restoration.Objects[0].APIVersion)
			object.SetKind(missing)
			if err := h.operator.Get(h.ctx, client.ObjectKey{Name: missingName, Namespace: namespace.Name}, object); err != nil || object.GetUID() != restored.Restoration.Objects[0].UID {
				t.Fatalf("recorded restored UID is not the served object: %v", err)
			}
		})
	}
}

func restorationAPIObjects(agent *api.Agent, namespace string) []unstructured.Unstructured {
	metadata := func(name string) map[string]any {
		return map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{"app.kubernetes.io/managed-by": "Helm"},
			"annotations": map[string]any{"meta.helm.sh/release-name": helmReleaseName(agent.Name), "meta.helm.sh/release-namespace": namespace}}
	}
	objects := []unstructured.Unstructured{
		{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata("runtime-config"), "data": map[string]any{"message": "original"}}},
		{Object: map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": metadata("runtime-account"), "automountServiceAccountToken": false}},
		{Object: map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": metadata("runtime-service"), "spec": map[string]any{"ports": []any{map[string]any{"port": int64(8642), "targetPort": int64(8642), "protocol": "TCP"}}, "selector": map[string]any{"runtime": "main"}}}},
	}
	for _, kind := range []string{"StatefulSet", "Deployment"} {
		name := instanceServiceName(agent.Name)
		if kind == "Deployment" {
			name = "runtime-companion"
		}
		spec := map[string]any{"replicas": int64(1), "selector": map[string]any{"matchLabels": map[string]any{"runtime": name}},
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"runtime": name}}, "spec": map[string]any{
				"serviceAccountName": "runtime-account", "automountServiceAccountToken": false, "containers": []any{map[string]any{"name": "main", "image": *agent.Spec.Agent.Image}}}}}
		if kind == "StatefulSet" {
			spec["serviceName"] = "runtime-service"
		}
		objects = append(objects, unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": kind, "metadata": metadata(name), "spec": spec}})
	}
	return objects
}

func restorationAPIManifest(t *testing.T, objects []unstructured.Unstructured) string {
	t.Helper()
	var result strings.Builder
	for _, object := range objects {
		body, err := json.Marshal(object.Object)
		if err != nil {
			t.Fatal(err)
		}
		result.WriteString("---\n")
		result.Write(body)
		result.WriteByte('\n')
	}
	return result.String()
}

type restorationAPIHelm struct {
	*cetiAdminInstallationHelm
	t         *testing.T
	config    *rest.Config
	namespace string
	original  []unstructured.Unstructured
	applies   int
	lostReply bool
}

func (h *restorationAPIHelm) Prepare(namespace string, values map[string]any, version string) (*helm.PreparedRelease, error) {
	if namespace != h.namespace {
		return h.cetiAdminInstallationHelm.Prepare(namespace, values, version)
	}
	return &helm.PreparedRelease{Values: values, ChartVersion: version}, nil
}

func (h *restorationAPIHelm) MaintenanceQuiescent(context.Context, string, string) error { return nil }

func (h *restorationAPIHelm) InstallOrUpgradeMaintenance(ctx context.Context, namespace, name string, prepared *helm.PreparedRelease, revision int, guard func(context.Context) error) error {
	h.applies++
	old := h.releases[namespace+"/"+name]
	if old == nil || old.Version != revision || guard == nil {
		return errors.New("unexpected maintenance revision or guard")
	}
	if err := guard(ctx); err != nil {
		return err
	}
	wanted := make([]unstructured.Unstructured, len(h.original))
	for i := range h.original {
		wanted[i] = *h.original[i].DeepCopy()
		if wanted[i].GetKind() == "StatefulSet" || wanted[i].GetKind() == "Deployment" {
			_ = unstructured.SetNestedField(wanted[i].Object, int64(0), "spec", "replicas")
		}
		if wanted[i].GetKind() == "ConfigMap" {
			wanted[i].Object["data"] = map[string]any{"message": "authoritative"}
		}
	}
	manifest := restorationAPIManifest(h.t, wanted)
	config := rest.CopyConfig(h.config)
	config.Wrap(func(base http.RoundTripper) http.RoundTripper {
		return nativeStartGuardedTransport{base: base, guard: guard}
	})
	sdk := kube.New(&nativeStartRESTGetter{config: config, namespace: namespace})
	sdk.Namespace = namespace
	original, err := sdk.Build(strings.NewReader(old.Manifest), false)
	if err != nil {
		return err
	}
	objects, err := sdk.Build(strings.NewReader(manifest), false)
	if err != nil {
		return err
	}
	filter := func(input kube.ResourceList) kube.ResourceList {
		var result kube.ResourceList
		for _, info := range input {
			if object, ok := info.Object.(*unstructured.Unstructured); ok && maintenanceRestorableKind(object.GetAPIVersion(), object.GetKind()) {
				result = append(result, info)
			}
		}
		return result
	}
	if _, err := sdk.Update(filter(original), filter(objects), false); err != nil {
		return err
	}
	h.releases[namespace+"/"+name] = &release.Release{Name: name, Namespace: namespace, Version: revision + 1, Manifest: manifest,
		Info: &release.Info{Status: release.StatusDeployed}, Chart: &chart.Chart{Metadata: &chart.Metadata{Version: prepared.ChartVersion}}, Config: prepared.Values}
	if h.lostReply {
		return errors.New("synthetic lost successful restoration acknowledgement")
	}
	return nil
}
