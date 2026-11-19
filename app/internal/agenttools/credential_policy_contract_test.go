// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agenttools

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestCredentialPolicyWireContractContainsOnlyEnforcementEvidence(t *testing.T) {
	policy := credentialPolicyFixture()
	readback := CredentialPolicyReadback{
		Policy: policy, Revision: policy.Revision(), RecordUID: "record-uid", ResourceVersion: "7",
	}
	raw, err := json.Marshal(readback)
	if err != nil {
		t.Fatal(err)
	}
	fields := func(raw json.RawMessage, want []string) map[string]json.RawMessage {
		t.Helper()
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		if got := slices.Sorted(maps.Keys(object)); !slices.Equal(got, want) {
			t.Fatalf("credential-free contract fields = %v, want %v", got, want)
		}
		return object
	}
	record := fields(raw, []string{"policy", "recordUID", "resourceVersion", "revision"})
	projection := fields(record["policy"], []string{
		"agentUID", "clientID", "enabled", "identity", "ownerObjectID", "ownerTenantID",
		"policyUID", "principalID", "teamUID", "version",
	})
	identity := fields(projection["identity"], []string{"allowed", "credentialServices", "required"})
	var services []string
	if err := json.Unmarshal(identity["credentialServices"], &services); err != nil ||
		!slices.Equal(services, []string{CredentialServiceARM}) {
		t.Fatal("the projection did not retain its sole public ARM service", err)
	}
	var decoded CredentialPolicyReadback
	if err := json.Unmarshal(raw, &decoded); err != nil || !decoded.Matches(policy) {
		t.Fatal("the credential-free wire contract lost its enforcement identity", err)
	}
}

func TestCredentialPolicyRevisionCanonicalizesIdentityIDCase(t *testing.T) {
	policy := credentialPolicyFixture()
	policy.OwnerTenantID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	policy.OwnerObjectID = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	policy.ClientID = "cccccccc-dddd-eeee-ffff-aaaaaaaaaaaa"
	policy.PrincipalID = "dddddddd-eeee-ffff-aaaa-bbbbbbbbbbbb"
	upper := policy
	upper.OwnerTenantID, upper.OwnerObjectID = strings.ToUpper(policy.OwnerTenantID), strings.ToUpper(policy.OwnerObjectID)
	upper.ClientID, upper.PrincipalID = strings.ToUpper(policy.ClientID), strings.ToUpper(policy.PrincipalID)
	readback := CredentialPolicyReadback{
		Policy: upper, Revision: upper.Revision(), RecordUID: "record-uid", ResourceVersion: "7",
	}
	if !policy.ValidApplied() || !upper.ValidApplied() || policy.Revision() != upper.Revision() || !readback.Matches(policy) {
		t.Fatal("equivalent canonical identity IDs produced different enforcement revisions")
	}
}
