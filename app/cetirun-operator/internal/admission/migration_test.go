// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
)

func TestMigrationDestinationRequiresAdministratorAuthority(t *testing.T) {
	f := newFixture(t)
	stampFixtureAuthorization(t, f)
	next := f.agent.DeepCopy()
	next.Spec.PlannedMigration = &api.PlannedMigrationRequest{RequestID: "migration-00000001",
		Target: api.MigrationObject{Name: "node-b", UID: "node-b-uid"}}
	for _, user := range []string{"member", "portal", "administrator"} {
		response := f.handler.Validate(context.Background(), requestFor(t, admissionv1.Update, next, f.agent, user))
		if user == "administrator" {
			if !response.Allowed {
				t.Fatal("administrator migration was rejected:", response.Result)
			}
		} else if response.Allowed || !strings.Contains(response.Result.Message, "administrator") {
			t.Fatalf("%s could choose a migration destination: %+v", user, response.Result)
		}
	}
}
