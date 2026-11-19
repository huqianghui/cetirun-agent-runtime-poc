// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPlannedMigrationUncapturedSourceRecoversStaleReadiness(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "return"}[previous], func(t *testing.T) {
			ctx := context.Background()
			f := newMigrationFixture(t, "openclaw", true)
			source, target := "node-a", "node-b"
			if previous {
				completeMigration(t, f, "migration-00000001", target)
				source, target = target, source
			}

			recorded := f.current(t).Status.PlannedMigration.DeepCopy()
			before := reconciledExpectationForTest(t, f)
			keyID := f.current(t).Status.AIGateway.KeyID
			requestMigration(t, f, "migration-00000002", target)
			workload := f.workload(t)
			workload.Spec.Replicas = ptr.To[int32](0)
			if err := f.kube.Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Delete(ctx, f.pod(t)); err != nil {
				t.Fatal(err)
			}
			attachments, err := f.r.migrationAttachments(ctx, "synthetic-home-volume")
			if err != nil {
				t.Fatal(err)
			}
			for index := range attachments {
				attachment := &attachments[index]
				if err := f.kube.Delete(ctx, attachment); err != nil {
					t.Fatal(err)
				}
				if err := f.kube.Get(ctx, client.ObjectKeyFromObject(attachment), attachment); err != nil {
					t.Fatal(err)
				}
				attachment.Finalizers = nil
				if err := f.kube.Update(ctx, attachment); err != nil {
					t.Fatal(err)
				}
			}
			migrationWorkloadStatus(t, f, 0)
			if f.current(t).Status.Phase != api.PhaseReady {
				t.Fatal("fixture must retain the stale Ready observation")
			}
			allowed, _, err := f.r.reconcileMigration(ctx, f.current(t), true)
			if err != nil || !allowed {
				t.Fatalf("uncaptured source cannot reach ordinary recovery: allowed=%t err=%v", allowed, err)
			}
			if !reflect.DeepEqual(recorded, f.current(t).Status.PlannedMigration) {
				t.Fatal("unready source was captured or its previous checkpoint changed")
			}
			for range 6 {
				f.reconcileOK(t)
				if ptr.Deref(f.workload(t).Spec.Replicas, -1) == 1 {
					break
				}
			}
			after := reconciledExpectationForTest(t, f)
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 ||
				f.current(t).Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 ||
				after.Home != before.Home || after.Namespace != before.Namespace {
				t.Fatal("ordinary source recovery failed or changed a retained identity")
			}
			if !reflect.DeepEqual(recorded, f.current(t).Status.PlannedMigration) {
				t.Fatal("migration advanced before the source regained stable readiness")
			}
			migrationReadyPod(t, f, "recovered-source-pod", source)
			migrationAttachment(t, f, "recovered-source-attachment", source)
			migrationWorkloadStatus(t, f, 1)
			f.reconcileOK(t)
			if f.current(t).Status.Phase != api.PhaseReady {
				t.Fatal("ordinary source recovery did not restore readiness")
			}
			f.reconcileOK(t)
			migrationPhase(t, f, api.MigrationProtecting)
			if f.current(t).Status.PlannedMigration.SourcePod.UID != "recovered-source-pod" {
				t.Fatal("migration captured stale source evidence")
			}
			finishMigration(t, f)
			final := reconciledExpectationForTest(t, f)
			if f.current(t).Status.PlannedMigration.RequestID != "migration-00000002" ||
				f.current(t).Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 ||
				final.Home != before.Home || final.Namespace != before.Namespace {
				t.Fatal("recovered migration changed the request or retained lifetime")
			}
		})
	}
}

func TestPlannedMigrationUnreadySourceRecoveryKeepsGuards(t *testing.T) {
	for _, fault := range []string{"ownership", "extra-replicas", "suspended"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			f := newMigrationFixture(t, "openclaw", true)
			requestMigration(t, f, "migration-00000001", "node-b")
			workload := f.workload(t)
			workload.Spec.Replicas = ptr.To[int32](0)
			switch fault {
			case "ownership":
				workload.OwnerReferences = nil
			case "extra-replicas":
				workload.Spec.Replicas = ptr.To[int32](2)
			case "suspended":
				agent := f.current(t)
				agent.Spec.Suspended = true
				agent.Generation++
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.kube.Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			migrationWorkloadStatus(t, f, 0)
			stages := f.installer.stages
			allowed, _, _ := f.r.reconcileMigration(ctx, f.current(t), true)
			if allowed || f.current(t).Status.PlannedMigration != nil || f.installer.stages != stages {
				t.Fatal("invalid or suspended source entered recovery or migration")
			}
		})
	}
}
