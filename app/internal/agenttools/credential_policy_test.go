// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agenttools

import "testing"

func credentialPolicyFixture() CredentialPolicy {
	return CredentialPolicy{
		Version: CredentialPolicyVersion, AgentUID: "11111111-1111-1111-1111-111111111111",
		TeamUID: "team-uid", PolicyUID: "policy-uid",
		OwnerTenantID: "22222222-2222-2222-2222-222222222222", OwnerObjectID: "33333333-3333-3333-3333-333333333333",
		ClientID: "44444444-4444-4444-4444-444444444444", PrincipalID: "55555555-5555-5555-5555-555555555555",
		Enabled: true, Identity: CredentialPolicyIdentity{Allowed: true, Required: true, CredentialServices: []string{CredentialServiceARM}},
	}
}

func TestCredentialPolicyReadbackBindsEveryAuthority(t *testing.T) {
	policy := credentialPolicyFixture()
	readback := CredentialPolicyReadback{Policy: policy, Revision: policy.Revision(), RecordUID: "record-uid", ResourceVersion: "12"}
	if !readback.Matches(policy) {
		t.Fatal("current typed readback did not match")
	}
	for name, mutate := range map[string]func(*CredentialPolicy){
		"agent":     func(p *CredentialPolicy) { p.AgentUID = "99999999-9999-9999-9999-999999999999" },
		"team":      func(p *CredentialPolicy) { p.TeamUID = "replacement-team" },
		"policy":    func(p *CredentialPolicy) { p.PolicyUID = "replacement-policy" },
		"tenant":    func(p *CredentialPolicy) { p.OwnerTenantID = "99999999-9999-9999-9999-999999999999" },
		"owner":     func(p *CredentialPolicy) { p.OwnerObjectID = "99999999-9999-9999-9999-999999999999" },
		"client":    func(p *CredentialPolicy) { p.ClientID = "99999999-9999-9999-9999-999999999999" },
		"principal": func(p *CredentialPolicy) { p.PrincipalID = "99999999-9999-9999-9999-999999999999" },
		"disabled":  func(p *CredentialPolicy) { p.Enabled = false },
		"denied":    func(p *CredentialPolicy) { p.Identity.Allowed = false },
		"required":  func(p *CredentialPolicy) { p.Identity.Required = false },
		"service":   func(p *CredentialPolicy) { p.Identity.CredentialServices = []string{"microsoft_graph"} },
		"version":   func(p *CredentialPolicy) { p.Version = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := policy
			mutate(&changed)
			if readback.Matches(changed) || changed.Revision() == policy.Revision() {
				t.Fatal("changed authority reused old evidence")
			}
		})
	}
	for name, mutate := range map[string]func(*CredentialPolicyReadback){
		"missing UID":      func(r *CredentialPolicyReadback) { r.RecordUID = "" },
		"missing version":  func(r *CredentialPolicyReadback) { r.ResourceVersion = "" },
		"desired only":     func(r *CredentialPolicyReadback) { r.Policy = CredentialPolicy{} },
		"changed revision": func(r *CredentialPolicyReadback) { r.Revision = "requested" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := readback
			mutate(&changed)
			if changed.Matches(policy) {
				t.Fatal("incomplete readback was accepted")
			}
		})
	}
}

func TestCredentialPolicyRequiresCanonicalIdentityAndOnlyARM(t *testing.T) {
	for name, mutate := range map[string]func(*CredentialPolicy){
		"unprovisioned": func(p *CredentialPolicy) { p.ClientID, p.PrincipalID = "", "" },
		"bad owner":     func(p *CredentialPolicy) { p.OwnerObjectID = "requested-owner" },
		"no service":    func(p *CredentialPolicy) { p.Identity.CredentialServices = nil },
		"wildcard":      func(p *CredentialPolicy) { p.Identity.CredentialServices = []string{"*"} },
		"extra":         func(p *CredentialPolicy) { p.Identity.CredentialServices = []string{CredentialServiceARM, "azure_arm"} },
		"duplicate": func(p *CredentialPolicy) {
			p.Identity.CredentialServices = []string{CredentialServiceARM, CredentialServiceARM}
		},
	} {
		t.Run(name, func(t *testing.T) {
			policy := credentialPolicyFixture()
			mutate(&policy)
			if policy.ValidApplied() || !ValidCredentialPolicyRevision(policy.Revision()) {
				t.Fatal("desired policy was confused with applied enforcement")
			}
		})
	}
}
