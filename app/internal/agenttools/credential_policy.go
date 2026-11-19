// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agenttools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

const (
	CredentialPolicyVersion = "credential-policy-v3"
	CredentialServiceARM    = "azure-resource-manager"
)

// CredentialPolicy is the Broker's credential-free enforcement input. Identity
// and owner references are part of the revision, not caller-selectable token
// destinations. Applying it never creates a Connection or a ConnectionBinding.
type CredentialPolicy struct {
	Version       string                   `json:"version"`
	AgentUID      string                   `json:"agentUID"`
	TeamUID       string                   `json:"teamUID"`
	PolicyUID     string                   `json:"policyUID"`
	OwnerTenantID string                   `json:"ownerTenantID"`
	OwnerObjectID string                   `json:"ownerObjectID"`
	ClientID      string                   `json:"clientID,omitempty"`
	PrincipalID   string                   `json:"principalID,omitempty"`
	Enabled       bool                     `json:"enabled"`
	Identity      CredentialPolicyIdentity `json:"identity"`
}

type CredentialPolicyIdentity struct {
	Allowed            bool     `json:"allowed"`
	Required           bool     `json:"required"`
	CredentialServices []string `json:"credentialServices,omitempty"`
}

// CredentialPolicyReadback identifies an actual owned enforcement record.
// It contains neither a provider credential nor evidence of provider access.
type CredentialPolicyReadback struct {
	Policy          CredentialPolicy `json:"policy"`
	Revision        string           `json:"revision"`
	RecordUID       string           `json:"recordUID"`
	ResourceVersion string           `json:"resourceVersion"`
}

// CredentialPolicyReference pins one observed record across check and issuance,
// including deletion/recreation of an otherwise identical policy projection.
type CredentialPolicyReference struct {
	Revision        string `json:"revision"`
	RecordUID       string `json:"recordUID"`
	ResourceVersion string `json:"resourceVersion"`
}

func (r CredentialPolicyReadback) Reference() *CredentialPolicyReference {
	return &CredentialPolicyReference{Revision: r.Revision, RecordUID: r.RecordUID, ResourceVersion: r.ResourceVersion}
}

func (r *CredentialPolicyReference) Valid() bool {
	return r != nil && ValidCredentialPolicyRevision(r.Revision) && r.RecordUID != "" && r.ResourceVersion != ""
}

var credentialIdentityID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var credentialRevision = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ValidCredentialPolicyRevision(revision string) bool {
	return credentialRevision.MatchString(revision)
}

// Revision is also usable while identity provisioning is pending. Such an input
// has a desired revision but fails ValidApplied and cannot produce readback.
// Unrelated Team/Policy generations deliberately do not change this projection.
func (p CredentialPolicy) Revision() string {
	p.Identity.CredentialServices = slices.Clone(p.Identity.CredentialServices)
	slices.Sort(p.Identity.CredentialServices)
	p.OwnerTenantID = strings.ToLower(p.OwnerTenantID)
	p.OwnerObjectID = strings.ToLower(p.OwnerObjectID)
	p.ClientID = strings.ToLower(p.ClientID)
	p.PrincipalID = strings.ToLower(p.PrincipalID)
	data, _ := json.Marshal(p)
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func (p CredentialPolicy) ValidApplied() bool {
	return p.Version == CredentialPolicyVersion &&
		credentialIdentityID.MatchString(p.AgentUID) && p.TeamUID != "" && p.PolicyUID != "" &&
		credentialIdentityID.MatchString(p.OwnerTenantID) && credentialIdentityID.MatchString(p.OwnerObjectID) &&
		credentialIdentityID.MatchString(p.ClientID) && credentialIdentityID.MatchString(p.PrincipalID) &&
		p.Enabled && p.Identity.Allowed &&
		len(p.Identity.CredentialServices) == 1 && p.Identity.CredentialServices[0] == CredentialServiceARM
}

func (r CredentialPolicyReadback) Matches(policy CredentialPolicy) bool {
	return policy.ValidApplied() && r.Policy.ValidApplied() && r.Reference().Valid() &&
		r.Revision == policy.Revision() && r.Revision == r.Policy.Revision()
}

// UsesAgentIdentity names the real Broker paths which acquire a credential using
// the canonical Entra Agent Identity, including delegated Azure/Graph OBO.
func UsesAgentIdentity(provider string) bool {
	return provider == "azure" || provider == "microsoft"
}
