// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
)

func (s SourceReference) IsAgent() bool {
	return s.APIVersion == api.GroupVersion.String() && s.Kind == "Agent" && s.Namespace == ""
}

func (s SourceReference) IsClusterRole() bool {
	return s.APIVersion == "rbac.authorization.k8s.io/v1" && s.Kind == "ClusterRole" && s.Namespace == ""
}

func (s SourceReference) ValidIdentity() bool {
	if s.Name == "" || s.UID == "" {
		return false
	}
	if !s.IsAgent() {
		return s.ResourceVersion != "" && s.Generation == 0 && s.IntentDigest == ""
	}
	raw, err := hex.DecodeString(s.IntentDigest)
	return s.ResourceVersion == "" && s.Generation > 0 && err == nil && len(raw) == sha256.Size &&
		hex.EncodeToString(raw) == s.IntentDigest
}

// AgentSourceReference excludes status and unrelated metadata, but includes
// explicit reauthorization and lifecycle holds even when generation is unchanged.
func AgentSourceReference(agent *api.Agent) (SourceReference, error) {
	if _, err := sandboxbinding.AuthorizedBinding(agent); err != nil {
		return SourceReference{}, err
	}
	if agent.Generation < 1 || agent.ResourceVersion == "" {
		return SourceReference{}, fmt.Errorf("Sandbox Agent intent requires a persisted generation and resource version")
	}
	digest, err := agent.SandboxExecutionIntentDigest()
	if err != nil {
		return SourceReference{}, err
	}
	return SourceReference{
		APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID,
		Generation: agent.Generation, IntentDigest: digest,
	}, nil
}
