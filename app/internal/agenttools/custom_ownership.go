// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agenttools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// CustomOwnershipRecordName is the immutable operator-namespace receipt for
// one ordinary Custom Agent. The Agent UID is part of the name so a recreated
// Agent cannot inherit its predecessor's namespace authority.
func CustomOwnershipRecordName(agentUID string) string {
	sum := sha256.Sum256([]byte(agentUID))
	return "custom-ownership-" + hex.EncodeToString(sum[:16])
}

// CustomOwnershipNamespace is the small, credential-free part of the Custom
// ownership receipt that a Broker needs to validate an Agent record namespace.
// The Operator retains the complete receipt, including HOME policy.
type CustomOwnershipNamespace struct {
	AgentName    string `json:"agentName"`
	AgentUID     string `json:"agentUID"`
	Namespace    string `json:"namespace"`
	NamespaceUID string `json:"namespaceUID"`
}

func ParseCustomOwnershipNamespace(raw string) (CustomOwnershipNamespace, error) {
	var receipt CustomOwnershipNamespace
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil || receipt.AgentName == "" || receipt.AgentUID == "" ||
		receipt.Namespace == "" || receipt.NamespaceUID == "" {
		return CustomOwnershipNamespace{}, errors.New("Custom ownership namespace receipt is invalid")
	}
	return receipt, nil
}

func (r CustomOwnershipNamespace) Matches(agentName, agentUID, namespace, namespaceUID string) bool {
	return agentName != "" && agentUID != "" && namespace != "" && namespaceUID != "" &&
		r.AgentName == agentName && r.AgentUID == agentUID && r.Namespace == namespace && r.NamespaceUID == namespaceUID
}
