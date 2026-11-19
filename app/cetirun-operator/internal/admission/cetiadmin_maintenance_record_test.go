// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func heldWorkloadFixture(t *testing.T) (cetiAdminFixture, *appsv1.StatefulSet, *corev1.ConfigMap) {
	t.Helper()
	f := newCetiAdminFixture(t)
	f.updateTarget(t, func(agent *api.Agent) {
		agent.Spec.Suspended = true
		agent.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-snapshot-one", ExpectedRevision: 10}
		agent.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: agent.Spec.Helm.Maintenance.RequestID, Revision: 10, ObservedGeneration: agent.Generation, Phase: api.HelmMaintenancePhaseHeld}
	})
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "openclaw-example-cetirun", Namespace: f.target.Status.Namespace, UID: "workload-uid",
		Annotations: map[string]string{"meta.helm.sh/release-name": f.target.Status.HelmReleaseName, "meta.helm.sh/release-namespace": f.target.Status.Namespace}},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(0)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "cetirun", Image: "registry.example.com/hermes:old"}}}}}}
	if err := f.kube.Create(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	checkpoint := f.heldMaintenance(t, []cetiadmin.ManagedResource{{APIVersion: "apps/v1", Kind: "StatefulSet", ObjectIdentity: cetiadmin.ObjectIdentity{Name: workload.Name, Namespace: workload.Namespace, UID: workload.UID}}})
	return f, workload, checkpoint
}

func TestCetiAdminHelmRequiresCurrentOperationSnapshot(t *testing.T) {
	for _, change := range []string{"valid", "missing checkpoint", "checkpoint owner", "request ID", "revision", "generation", "phase", "resumed", "Agent UID", "namespace UID", "resource UID", "empty resources", "duplicate resource", "missing marker", "replacement resource", "unrecorded resource", "protected namespace", "protected Team"} {
		t.Run(change, func(t *testing.T) {
			f, old, checkpoint := heldWorkloadFixture(t)
			ctx := context.Background()
			var record cetiAdminMaintenanceRecord
			if err := decodeJSON([]byte(checkpoint.Data["maintenance.json"]), &record, false); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "checkpoint owner":
				checkpoint.OwnerReferences[0].UID = "another-owner-uid"
			case "request ID":
				record.Request.RequestID = "another-maintenance-request"
			case "revision":
				record.Request.ExpectedRevision++
			case "generation":
				record.RequestGeneration--
			case "phase":
				record.Phase = api.HelmMaintenancePhaseRequested
			case "resumed":
				record.Resumed = true
			case "Agent UID":
				record.Target.Agent.UID = "another-agent-uid"
			case "namespace UID":
				record.Target.Namespace.UID = "another-namespace-uid"
			case "resource UID":
				record.Target.Resources[0].UID = "another-workload-uid"
			case "empty resources":
				record.Target.Resources = nil
			case "duplicate resource":
				record.Target.Resources = append(record.Target.Resources, record.Target.Resources[0])
			case "missing marker", "protected namespace":
				namespace := &corev1.Namespace{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: old.Namespace}, namespace); err != nil {
					t.Fatal(err)
				}
				if change == "missing marker" {
					delete(namespace.Annotations, cetiadmin.MaintenanceRequestID)
				} else {
					namespace.Labels["cetirun.microsoft.com/sandbox-protected"] = "true"
				}
				if err := f.kube.Update(ctx, namespace); err != nil {
					t.Fatal(err)
				}
			case "replacement resource":
				old.UID = "replacement-workload-uid"
				if err := f.kube.Update(ctx, old); err != nil {
					t.Fatal(err)
				}
			case "unrecorded resource":
				old = old.DeepCopy()
				old.Name, old.UID, old.ResourceVersion = "new-workload", "new-workload-uid", ""
				if err := f.kube.Create(ctx, old); err != nil {
					t.Fatal(err)
				}
			case "protected Team":
				team := &api.Team{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: f.target.Spec.Team}, team); err != nil {
					t.Fatal(err)
				}
				team.Spec.AgentPolicyRef = "protected-policy"
				if err := f.kube.Update(ctx, team); err != nil {
					t.Fatal(err)
				}
			}
			checkpoint.Data["maintenance.json"] = string(adminJSON(t, record))
			if err := f.kube.Update(ctx, checkpoint); err != nil {
				t.Fatal(err)
			}
			if change == "missing checkpoint" {
				if err := f.kube.Delete(ctx, checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			next := old.DeepCopy()
			next.Spec.Template.Spec.Containers[0].Image = "registry.example.com/hermes:new"
			request := adminRequest(t, admissionv1.Update, "apps", "StatefulSet", "statefulsets", next, old, f.actor)
			if response := f.guard.Validate(ctx, request); response.Allowed != (change == "valid") {
				t.Fatalf("allowed=%v: %v", response.Allowed, response.Result)
			}
		})
	}
}

func TestCetiAdminHelmManifestDoesNotRefreshOperationSnapshot(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		name := "recorded-current-object"
		if replaced {
			name = "same-name-replacement"
		}
		t.Run(name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			ctx := context.Background()
			f.updateTarget(t, func(agent *api.Agent) {
				agent.Spec.Suspended = true
				agent.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-manifest-one", ExpectedRevision: 1}
				agent.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: agent.Spec.Helm.Maintenance.RequestID, Revision: 1, ObservedGeneration: agent.Generation, Phase: api.HelmMaintenancePhaseHeld}
			})
			config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "runtime-config", Namespace: f.target.Status.Namespace, UID: "runtime-config-uid"}}
			if err := f.kube.Create(ctx, config); err != nil {
				t.Fatal(err)
			}
			f.heldMaintenance(t, []cetiadmin.ManagedResource{{APIVersion: "v1", Kind: "ConfigMap", ObjectIdentity: cetiadmin.ObjectIdentity{Name: config.Name, Namespace: config.Namespace, UID: config.UID}}})
			previous := maintenanceSecret(t, f.target, 1, "deployed")
			previous.ResourceVersion = ""
			if err := f.kube.Create(ctx, previous); err != nil {
				t.Fatal(err)
			}
			if replaced {
				config.UID = "replacement-config-uid"
				if err := f.kube.Update(ctx, config); err != nil {
					t.Fatal(err)
				}
			}
			next := maintenanceSecret(t, f.target, 2, "pending-upgrade")
			next.UID, next.ResourceVersion = "", ""
			request := adminRequest(t, admissionv1.Create, "", "Secret", "secrets", next, nil, f.actor)
			if response := f.guard.Validate(ctx, request); response.Allowed == replaced {
				t.Fatalf("allowed=%v with replacement=%v: %v", response.Allowed, replaced, response.Result)
			}
		})
	}
}
