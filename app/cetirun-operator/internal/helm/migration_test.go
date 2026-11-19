// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
)

func TestMigrationUsesThePinnedChartWithoutChangingHOME(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	chrt := testChart(t)
	before, err := PrepareSandboxChart(chrt, values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	input := bundle.Identity()
	input.PlannedMigration = &api.PlannedMigrationRequest{RequestID: "migration-00000001",
		Target: api.MigrationObject{Name: "node-b", UID: "node-b-uid"}}
	desired, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	after, err := PrepareSandboxChart(chrt, values, nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := PrepareSandboxChart(chrt, values, nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	if after.WorkloadHash == before.WorkloadHash || after.WorkloadHash != retry.WorkloadHash ||
		after.ChartVersion != before.ChartVersion {
		t.Fatal("migration did not produce a deterministic workload on the existing chart pin")
	}
	original, err := before.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := after.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Annotations[api.AnnotationMigrationRequest] != input.PlannedMigration.RequestID {
		t.Fatal("migration request is not bound to the rendered template")
	}
	delete(migrated.Annotations, api.AnnotationMigrationRequest)
	if err := runtimebindings.UnpinMigrationNode(&migrated.Spec, "node-b"); err != nil {
		t.Fatal(err)
	}
	if original.Spec.Affinity == nil && migrated.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		migrated.Spec.Affinity = nil
	}
	if !reflect.DeepEqual(original, migrated) {
		t.Fatal("migration changed HOME, credentials, runtime or resources instead of only placement and request identity")
	}
}
