// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// SandboxExecutionIntentDigest binds the complete spec and execution-affecting
// metadata, excluding status churn. It does not authorize the captured intent.
func (a *Agent) SandboxExecutionIntentDigest() (string, error) {
	if a == nil {
		return "", fmt.Errorf("Sandbox Agent intent requires an Agent")
	}
	annotations := make(map[string]string, 2)
	for _, key := range []string{AnnotationSandboxAuthorization, AnnotationSandboxReauthorize} {
		if value, found := a.Annotations[key]; found {
			annotations[key] = value
		}
	}
	intent := struct {
		Spec         AgentSpec
		Annotations  map[string]string
		Held         bool
		Prepared     bool
		Bootstrapped bool `json:",omitempty"`
	}{
		Spec: a.Spec, Annotations: annotations,
		Held:         slices.Contains(a.Finalizers, SandboxHoldFinalizer),
		Prepared:     slices.Contains(a.Finalizers, SandboxPreparationFinalizer),
		Bootstrapped: slices.Contains(a.Finalizers, SandboxBootstrapFinalizer),
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		return "", fmt.Errorf("encode Sandbox Agent intent: %w", err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
