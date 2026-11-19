// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"fmt"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateIdentityApprovals checks the explicit per-Agent inventory. It does not
// provision identities or establish snapshot freshness; consumers must retain
// the current platform source revision through their execution/lifecycle gates.
func ValidateIdentityApprovals(inventory []ApprovedIdentity) error {
	agents := map[types.UID]bool{}
	accounts := map[struct{ namespace, name string }]bool{}
	for _, identity := range inventory {
		account := struct{ namespace, name string }{identity.Namespace, identity.ServiceAccountName}
		if identity.AgentUID == "" || len(validation.IsValidLabelValue(string(identity.AgentUID))) != 0 ||
			agents[identity.AgentUID] || accounts[account] ||
			len(validation.IsDNS1123Label(identity.Namespace)) != 0 ||
			identity.ServiceAccountName == "default" || len(validation.IsDNS1123Subdomain(identity.ServiceAccountName)) != 0 ||
			!identityValue(identity.Revision) || (!identity.Kubernetes && identity.Entra == nil) {
			return fmt.Errorf("%w: approvals require a complete Agent binding and an unshared dedicated ServiceAccount", ErrUnapprovedGrant)
		}
		if identity.Entra != nil && (!identityValue(identity.Entra.ClientID) || !identityValue(identity.Entra.TenantID)) {
			return fmt.Errorf("%w: Entra approval requires explicit client and tenant identifiers", ErrUnapprovedGrant)
		}
		agents[identity.AgentUID], accounts[account] = true, true
	}
	return nil
}

func identityValue(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) == -1
}

func IdentityApproval(agentUID types.UID, namespace string, inventory []ApprovedIdentity) (*ApprovedIdentity, error) {
	if err := ValidateIdentityApprovals(inventory); err != nil {
		return nil, err
	}
	for _, approval := range inventory {
		if approval.AgentUID == agentUID && approval.Namespace == namespace {
			if approval.Entra != nil {
				entra := *approval.Entra
				approval.Entra = &entra
			}
			return &approval, nil
		}
	}
	return nil, fmt.Errorf("%w: a dedicated per-Agent ServiceAccount registration is required", ErrUnapprovedGrant)
}
