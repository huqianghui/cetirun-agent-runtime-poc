// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
package agenttools

import (
	"net/url"
	"testing"
)

func TestServiceProfilesHaveFixedDistinctOriginsAndAudiences(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Services() {
		u, err := url.Parse(p.Origin)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || seen[p.ID] {
			t.Fatalf("invalid profile: %#v", p)
		}
		seen[p.ID] = true
		profile, ok := LookupService(p.ID)
		if !ok || profile != p {
			t.Fatal("profile lookup mismatch")
		}
	}
	arm, _ := LookupService("azure_arm")
	if arm.Audience != "https://management.azure.com/" {
		t.Fatal("ARM audience changed")
	}
	graph, _ := LookupService("microsoft_graph")
	if graph.Audience != "https://graph.microsoft.com/" {
		t.Fatal("Graph audience changed")
	}
	if _, ok := LookupService("https://example.com"); ok {
		t.Fatal("accepted arbitrary service")
	}
	profiles := Services()
	profiles[0].Origin = "https://example.com"
	arm, _ = LookupService("azure_arm")
	if arm.Origin == profiles[0].Origin {
		t.Fatal("caller mutated service registry")
	}
}
