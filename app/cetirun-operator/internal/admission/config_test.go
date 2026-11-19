// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"errors"
	"slices"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
)

func TestTeamAssignmentConfigurationRequiresExactUniqueIdentities(t *testing.T) {
	for _, assignment := range [][]TeamAssignment{
		{{Team: "engineering"}},
		{{Team: "Invalid", Users: []string{"member"}}},
		{{Team: "engineering", Users: []string{"*"}}},
		{{Team: "engineering", Users: []string{" member"}}},
		{{Team: "engineering", Users: []string{"system:anonymous"}}},
		{{Team: "engineering", Groups: []string{"system:authenticated"}}},
		{{Team: "engineering", Groups: []string{"system:unauthenticated"}}},
		{{Team: "engineering", Groups: []string{"system:serviceaccounts"}}},
		{{Team: "engineering", Groups: []string{"system:serviceaccounts:example"}}},
		{{Team: "engineering", Users: []string{"member", "member"}}},
		{{Team: "engineering", Users: []string{"member"}}, {Team: "other", Users: []string{"member"}}},
		{{Team: "engineering", Groups: []string{"members"}}, {Team: "other", Groups: []string{"members"}}},
		{{Team: "engineering", Users: []string{"member"}}, {Team: "engineering", Users: []string{"other"}}},
	} {
		config := testConfig()
		config.TeamAssignments = assignment
		if err := config.Validate(); err == nil {
			t.Fatalf("invalid assignment accepted: %#v", assignment)
		}
	}
	raw := `{"adminUsers":["administrator","system:serviceaccount:operator-test:portal"],"controllerUsername":"system:serviceaccount:operator-test:controller","teamAssignments":[{"team":"engineering","users":["member"],"groups":["engineering-members"]}]}`
	config, err := ParseConfig(raw)
	if err != nil || len(config.TeamAssignments) != 1 || config.TeamAssignments[0].Team != "engineering" ||
		!slices.Equal(config.TeamAssignments[0].Groups, []string{"engineering-members"}) {
		t.Fatalf("trusted assignment configuration did not roundtrip: %#v, %v", config, err)
	}
}

func TestAuthenticatedTeamAssignmentIsDeterministicAndFailClosed(t *testing.T) {
	config := testConfig()
	config.TeamAssignments = []TeamAssignment{
		{Team: "engineering", Users: []string{"member"}, Groups: []string{"engineering-members"}},
		{Team: "other", Groups: []string{"other-members"}},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("static configuration cannot determine authenticated group overlap: %v", err)
	}
	for _, test := range []struct {
		name string
		user authenticationv1.UserInfo
		team string
		err  bool
	}{
		{"user", authenticationv1.UserInfo{Username: "member"}, "engineering", false},
		{"group", authenticationv1.UserInfo{Username: "other-user", Groups: []string{"engineering-members"}}, "engineering", false},
		{"same Team twice", authenticationv1.UserInfo{Username: "member", Groups: []string{"engineering-members"}}, "engineering", false},
		{"unmapped", authenticationv1.UserInfo{Username: "outsider"}, "", false},
		{"case sensitive", authenticationv1.UserInfo{Username: "Member"}, "", false},
		{"group case sensitive", authenticationv1.UserInfo{Username: "outsider", Groups: []string{"Engineering-members"}}, "", false},
		{"anonymous", authenticationv1.UserInfo{Username: "system:anonymous", Groups: []string{"engineering-members"}}, "", false},
		{"unauthenticated", authenticationv1.UserInfo{Username: "member", Groups: []string{"system:unauthenticated", "engineering-members"}}, "", false},
		{"empty username", authenticationv1.UserInfo{Groups: []string{"engineering-members"}}, "", false},
		{"conflicting user and group", authenticationv1.UserInfo{Username: "member", Groups: []string{"other-members"}}, "", true},
		{"conflicting groups", authenticationv1.UserInfo{Username: "outsider", Groups: []string{"engineering-members", "other-members"}}, "", true},
		{"conflicting groups reversed", authenticationv1.UserInfo{Username: "outsider", Groups: []string{"other-members", "engineering-members"}}, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			team, err := config.assignedTeam(test.user)
			if (err != nil) != test.err || team != test.team {
				t.Fatalf("got Team=%q, err=%v", team, err)
			}
			if test.err && !errors.Is(err, errTeamAssignmentConflict) {
				t.Fatalf("conflict lacks the stable configuration reason: %v", err)
			}
		})
	}
}

func TestTeamAssignmentConflictGuidanceDoesNotDiscloseMemberships(t *testing.T) {
	config := testConfig()
	config.TeamAssignments = []TeamAssignment{
		{Team: "first-team", Users: []string{"synthetic-caller"}, Groups: []string{"first-group"}},
		{Team: "second-team", Groups: []string{"second-group"}},
	}
	user := authenticationv1.UserInfo{Username: "synthetic-caller", Groups: []string{"first-group", "second-group"}}
	for range 2 {
		if err := config.Validate(); err != nil {
			t.Fatal(err)
		}
		team, err := config.assignedTeam(user)
		if team != "" || !errors.Is(err, errTeamAssignmentConflict) {
			t.Fatalf("ambiguous assignment did not fail closed: Team=%q, err=%v", team, err)
		}
		for _, guidance := range []string{"TeamAssignmentConflict:", "administrator", "sandboxAdmission.teamAssignments", "identity-provider", "one Team"} {
			if !strings.Contains(err.Error(), guidance) {
				t.Fatalf("conflict lacks actionable guidance %q: %v", guidance, err)
			}
		}
		for _, identity := range []string{user.Username, "first-group", "second-group", "first-team", "second-team"} {
			if strings.Contains(err.Error(), identity) {
				t.Fatalf("conflict diagnostic disclosed request-specific membership %q", identity)
			}
		}
		slices.Reverse(config.TeamAssignments)
		slices.Reverse(user.Groups)
	}
}

func TestHandlerOwnsImmutableCopyOfTrustedAssignments(t *testing.T) {
	f := newFixture(t)
	config := testConfig()
	config.TeamAssignments = []TeamAssignment{{Team: "engineering", Users: []string{"member"}, Groups: []string{"engineering-members"}}}
	handler, err := NewHandler(f.client, config)
	if err != nil {
		t.Fatal(err)
	}
	config.AdminUsers[0] = "attacker"
	config.AdminGroups[0] = "attacker-group"
	config.TeamAssignments[0].Team = "attacker-team"
	config.TeamAssignments[0].Users[0] = "attacker"
	config.TeamAssignments[0].Groups[0] = "attacker-group"
	for _, user := range []authenticationv1.UserInfo{
		{Username: "member"}, {Username: "group-member", Groups: []string{"engineering-members"}},
	} {
		team, err := handler.config.assignedTeam(user)
		if err != nil || team != "engineering" {
			t.Fatal("caller mutation changed the handler's trust configuration")
		}
	}

	if handler.config.administrator(authenticationv1.UserInfo{Username: "attacker", Groups: []string{"attacker-group"}}) {
		t.Fatal("caller mutation expanded administration")
	}
}

func TestPlatformSourceConfigurationRequiresControllerNamespace(t *testing.T) {
	for _, test := range []struct {
		namespace, name string
		valid           bool
	}{
		{"", "", true},
		{"operator-test", "sandbox-platform", true},
		{"", "sandbox-platform", false},
		{"operator-test", "", false},
		{"workload", "sandbox-platform", false},
		{"Operator-test", "sandbox-platform", false},
		{"operator-test", " platform", false},
		{"operator-test", "bad/name", false},
	} {
		config := testConfig()
		config.PlatformNamespace, config.PlatformConfigMapName = test.namespace, test.name
		if err := config.Validate(); (err == nil) != test.valid {
			t.Errorf("source %q/%q valid=%t: %v", test.namespace, test.name, test.valid, err)
		}
	}
	config, err := ParseConfig(`{"adminUsers":["administrator","system:serviceaccount:operator-test:portal"],"controllerUsername":"system:serviceaccount:operator-test:controller","platformNamespace":"operator-test","platformConfigMapName":"sandbox-platform"}`)
	if err != nil || config.PlatformNamespace != "operator-test" || config.PlatformConfigMapName != "sandbox-platform" {
		t.Fatalf("explicit source did not roundtrip: %+v, %v", config, err)
	}
}
