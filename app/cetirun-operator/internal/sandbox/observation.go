// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

var (
	ErrObservationUnavailable = errors.New("current sandbox enforcement cannot be established")
	ErrEnforcementPending     = errors.New("sandbox enforcement verification is incomplete")
)

type ExecutionIdentity struct {
	Binding          Binding
	Revision         string
	PodUID           types.UID
	NodeUID          types.UID
	EnforcementEpoch string
}

// Observation must come from a trusted guest-external adapter. Validation of
// this structure does not produce evidence or qualify a network dataplane.
type Observation struct {
	ExecutionIdentity
	ObservedAt       time.Time
	RuntimeVerified  bool
	PodVerified      bool
	ControlsVerified bool
}

func VerifyObservation(expected ExecutionIdentity, observation *Observation, now time.Time, maxAge time.Duration) error {
	if now.IsZero() || maxAge <= 0 {
		return fmt.Errorf("%w: a clock and positive freshness bound are required", ErrObservationUnavailable)
	}
	if expected.Binding.AgentUID == "" || expected.Binding.TeamUID == "" ||
		expected.Binding.SandboxUID == "" || expected.Binding.AgentPolicyUID == "" ||
		expected.Revision == "" || expected.PodUID == "" || expected.NodeUID == "" || expected.EnforcementEpoch == "" {
		return fmt.Errorf("%w: expected binding, revision and workload identities must be complete", ErrObservationUnavailable)
	}
	if observation == nil || observation.ObservedAt.IsZero() {
		return fmt.Errorf("%w: no trusted observation is available", ErrObservationUnavailable)
	}
	if observation.ExecutionIdentity != expected {
		return fmt.Errorf("%w: observation does not match the currently authorized execution", ErrObservationUnavailable)
	}
	if observation.ObservedAt.After(now) || now.Sub(observation.ObservedAt) > maxAge {
		return fmt.Errorf("%w: observation is stale or in the future", ErrObservationUnavailable)
	}
	if !observation.RuntimeVerified || !observation.PodVerified || !observation.ControlsVerified {
		return ErrEnforcementPending
	}
	return nil
}
