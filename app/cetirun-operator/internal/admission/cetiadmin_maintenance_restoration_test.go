// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func restorationAdmissionFixture(t *testing.T, kind string) (cetiAdminFixture, *corev1.ConfigMap, map[string]any, admission.Request) {
	t.Helper()
	f := newCetiAdminFixture(t)
	f.updateTarget(t, func(a *api.Agent) {
		a.Spec.Suspended = true
		a.Spec.Helm.Maintenance = nil
		a.Status.HelmMaintenance = &api.HelmMaintenanceStatus{Phase: api.HelmMaintenancePhaseReapplying, RequestID: "maintenance-restore-one", Revision: 1, ObservedGeneration: a.Generation}
	})
	namespace := &corev1.Namespace{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.target.Status.Namespace}, namespace); err != nil {
		t.Fatal(err)
	}
	namespace.Labels = map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}
	namespace.Annotations = map[string]string{cetiadmin.MaintenanceAgentName: f.target.Name, cetiadmin.MaintenanceAgentUID: string(f.target.UID), cetiadmin.MaintenanceRequestID: "maintenance-restore-one"}
	if err := f.kube.Update(context.Background(), namespace); err != nil {
		t.Fatal(err)
	}
	group, version, resource := "apps", "apps/v1", "statefulsets"
	if kind == "ConfigMap" {
		group, version, resource = "", "v1", "configmaps"
	} else if kind == "Deployment" {
		resource = "deployments"
	}
	record := map[string]any{"version": 1, "target": f.targetIdentity, "requestGeneration": f.target.Generation - 1, "intentHash": "synthetic-intent-hash",
		"phase": "Reapplying", "applyRevision": 1, "preparedHash": "prepared-hash",
		"request":     map[string]any{"requestID": "maintenance-restore-one", "expectedRevision": 1},
		"restoration": map[string]any{"revision": 1, "manifestHash": strings.Repeat("a", 64), "objects": []any{map[string]any{"apiVersion": version, "kind": kind, "name": "original-runtime", "namespace": namespace.Name}}}}
	hash := sha256.Sum256([]byte(f.target.UID))
	checkpoint := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin-maintenance-" + hex.EncodeToString(hash[:16]), Namespace: f.source.Namespace, UID: "maintenance-checkpoint-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(f.target, api.GroupVersion.WithKind("Agent"))}}, Data: map[string]string{"maintenance.json": string(adminJSON(t, record))}}
	if err := f.kube.Create(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": version, "kind": kind,
		"metadata": map[string]any{"name": "original-runtime", "namespace": namespace.Name}}}
	if kind == "ConfigMap" {
		object.Object["data"] = map[string]any{"configuration": "synthetic"}
	} else {
		object.Object["spec"] = map[string]any{"replicas": int64(0)}
	}
	caller := authenticationv1.UserInfo{Username: f.guard.config.ControllerUsername, Groups: []string{"system:authenticated"}}
	return f, checkpoint, record, adminRequest(t, admissionv1.Create, group, kind, resource, object, nil, caller)
}

func TestCetiAdminMaintenanceRestorationAdmission(t *testing.T) {
	for _, kind := range []string{"ConfigMap", "StatefulSet", "Deployment"} {
		t.Run("allow-exact-Operator-"+kind, func(t *testing.T) {
			f, _, _, request := restorationAdmissionFixture(t, kind)
			if response := f.guard.Validate(context.Background(), request); !response.Allowed {
				t.Fatalf("exact stopped restoration denied: %v", response.Result)
			}
		})
	}
	for _, change := range []string{"machine", "human administrator", "held", "blocked", "released", "request", "generation", "revision", "checkpoint owner", "namespace UID", "new name", "new GVK", "nonzero replicas", "implicit replicas", "hook", "recorded UID", "missing checkpoint", "duplicate grant"} {
		t.Run("reject-"+change, func(t *testing.T) {
			f, checkpoint, record, request := restorationAdmissionFixture(t, "StatefulSet")
			object := &unstructured.Unstructured{}
			if err := object.UnmarshalJSON(request.Object.Raw); err != nil {
				t.Fatal(err)
			}
			restoration := record["restoration"].(map[string]any)
			switch change {
			case "machine":
				request.UserInfo = f.actor
			case "human administrator":
				request.UserInfo = authenticationv1.UserInfo{Username: "administrator", Groups: []string{"system:authenticated"}}
			case "held", "blocked", "released":
				record["phase"] = map[string]string{"held": "Held", "blocked": "Blocked", "released": "Released"}[change]
			case "request":
				record["request"] = map[string]any{"requestID": "another-maintenance", "expectedRevision": 1}
			case "generation":
				record["requestGeneration"] = f.target.Generation
			case "revision":
				record["applyRevision"] = 2
			case "checkpoint owner":
				checkpoint.OwnerReferences[0].UID = "foreign-agent-uid"
			case "namespace UID":
				target := f.targetIdentity
				target.Namespace.UID = "replacement-namespace"
				record["target"] = target
			case "new name":
				object.SetName("not-in-original-manifest")
				request.Name = object.GetName()
			case "new GVK":
				object.SetKind("Deployment")
				request.Kind.Kind, request.Resource.Resource = "Deployment", "deployments"
			case "nonzero replicas":
				_ = unstructured.SetNestedField(object.Object, int64(1), "spec", "replicas")
			case "implicit replicas":
				unstructured.RemoveNestedField(object.Object, "spec", "replicas")
			case "hook":
				object.SetAnnotations(map[string]string{"helm.sh/hook": "pre-upgrade"})
			case "recorded UID":
				restoration["objects"].([]any)[0].(map[string]any)["uid"] = "already-created"
			case "duplicate grant":
				objects := restoration["objects"].([]any)
				restoration["objects"] = append(objects, objects[0])
			}
			checkpoint.Data["maintenance.json"] = string(adminJSON(t, record))
			if err := f.kube.Update(context.Background(), checkpoint); err != nil {
				t.Fatal(err)
			}
			if change == "missing checkpoint" {
				if err := f.kube.Delete(context.Background(), checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			request.Object.Raw = adminJSON(t, object)
			if response := f.guard.Validate(context.Background(), request); response.Allowed {
				t.Fatal("invalid restoration grant admitted a CREATE")
			}
		})
	}
	t.Run("reject-immutable-ConfigMap", func(t *testing.T) {
		f, _, _, request := restorationAdmissionFixture(t, "ConfigMap")
		object := &unstructured.Unstructured{}
		_ = object.UnmarshalJSON(request.Object.Raw)
		object.Object["immutable"] = true
		request.Object.Raw = adminJSON(t, object)
		if f.guard.Validate(context.Background(), request).Allowed {
			t.Fatal("immutable ConfigMap restoration was allowed")
		}
	})
}

func TestCetiAdminRestorationDoesNotReleasePodOrJobStartup(t *testing.T) {
	for _, kind := range []string{"Pod", "Job"} {
		for _, caller := range []string{"controller", "administrator", "machine"} {
			t.Run(kind+"/"+caller, func(t *testing.T) {
				f, _, _, _ := restorationAdmissionFixture(t, "StatefulSet")
				group, version, resource := "", "v1", "pods"
				if kind == "Job" {
					group, version, resource = "batch", "batch/v1", "jobs"
				}
				actor := authenticationv1.UserInfo{Username: f.guard.config.ControllerUsername, Groups: []string{"system:authenticated"}}
				if caller == "administrator" {
					actor.Username = "administrator"
				} else if caller == "machine" {
					actor = f.actor
				}
				object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": "forbidden-start", "namespace": f.target.Status.Namespace}}}
				request := adminRequest(t, admissionv1.Create, group, kind, resource, object, nil, actor)
				if f.guard.Validate(context.Background(), request).Allowed {
					t.Fatal("restoration admitted a new Pod or hook Job")
				}
			})
		}
	}
}
