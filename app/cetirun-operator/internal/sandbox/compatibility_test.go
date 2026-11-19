// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func TestTeamCompatibilityReportUsesSavedReferencesAndSafeIssues(t *testing.T) {
	input := resolverFixture()
	team := input.Team.DeepCopy()
	team.Generation = 7
	input.Resources.AgentPolicies["normal"].Generation = 5
	input.Resources.Sandboxes["standard"].Generation = 3
	report := TeamCompatibilityReport(team, input.Resources, input.Registrations, false)
	if report.State != api.CompatibilityCompatible || report.Team.Generation != 7 || report.Policy.Generation != 5 || len(report.Sandboxes) != 1 || report.Sandboxes[0].Sandbox.Generation != 3 {
		t.Fatalf("compatible report = %+v", report)
	}
	input.Resources.Sandboxes["standard"].Spec.Storage.Home.AccessMode = corev1.ReadWriteOnce
	report = TeamCompatibilityReport(team, input.Resources, input.Registrations, false)
	if report.State != api.CompatibilityIncompatible || len(report.Sandboxes) != 1 || report.Sandboxes[0].Reason != "SandboxIncompatible" {
		t.Fatalf("incompatible report = %+v", report)
	}
	var found bool
	for _, issue := range report.Issues {
		if issue.Code == "home_access_mode_unsupported" {
			found = true
			if !issue.Valid() || issue.Field != "spec.storage.home.accessMode" || issue.Sandbox == nil || issue.Sandbox.Name != "standard" {
				t.Fatalf("unsafe or incomplete issue = %+v", issue)
			}
		}
	}
	if !found {
		t.Fatalf("issues = %+v, want home access mode issue", report.Issues)
	}
}

func TestTeamCompatibilityReportKeepsLegacyTeamsUnreported(t *testing.T) {
	team := &api.Team{}
	report := TeamCompatibilityReport(team, Resources{}, nil, false)
	if report.State != api.CompatibilityCompatible || len(report.Issues) != 0 || len(report.Sandboxes) != 0 {
		t.Fatalf("legacy report = %+v", report)
	}
}
