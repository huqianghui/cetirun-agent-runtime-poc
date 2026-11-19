// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"errors"
	"testing"
	"time"
)

func TestObservationBindsIdentityAndFreshness(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	expected := ExecutionIdentity{
		Binding: resolverFixture().AuthorizedBinding, Revision: "effective-revision",
		PodUID: "pod-uid", NodeUID: "node-uid", EnforcementEpoch: "epoch-1",
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Observation)
		want   error
	}{
		{"valid", func(o *Observation) {}, nil},
		{"inclusive freshness limit", func(o *Observation) { o.ObservedAt = now.Add(-15 * time.Second) }, nil},
		{"stale", func(o *Observation) { o.ObservedAt = now.Add(-15*time.Second - time.Nanosecond) }, ErrObservationUnavailable},
		{"future", func(o *Observation) { o.ObservedAt = now.Add(time.Nanosecond) }, ErrObservationUnavailable},
		{"zero timestamp", func(o *Observation) { o.ObservedAt = time.Time{} }, ErrObservationUnavailable},
		{"different Agent", func(o *Observation) { o.Binding.AgentUID = "other" }, ErrObservationUnavailable},
		{"different Team", func(o *Observation) { o.Binding.TeamUID = "other" }, ErrObservationUnavailable},
		{"different Sandbox", func(o *Observation) { o.Binding.SandboxUID = "other" }, ErrObservationUnavailable},
		{"different policy", func(o *Observation) { o.Binding.AgentPolicyUID = "other" }, ErrObservationUnavailable},
		{"different revision", func(o *Observation) { o.Revision = "other" }, ErrObservationUnavailable},
		{"different Pod", func(o *Observation) { o.PodUID = "other" }, ErrObservationUnavailable},
		{"different node", func(o *Observation) { o.NodeUID = "other" }, ErrObservationUnavailable},
		{"different epoch", func(o *Observation) { o.EnforcementEpoch = "other" }, ErrObservationUnavailable},
		{"runtime incomplete", func(o *Observation) { o.RuntimeVerified = false }, ErrEnforcementPending},
		{"Pod incomplete", func(o *Observation) { o.PodVerified = false }, ErrEnforcementPending},
		{"controls incomplete", func(o *Observation) { o.ControlsVerified = false }, ErrEnforcementPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation := &Observation{
				ExecutionIdentity: expected, ObservedAt: now,
				RuntimeVerified: true, PodVerified: true, ControlsVerified: true,
			}
			tc.mutate(observation)
			if err := VerifyObservation(expected, observation, now, 15*time.Second); !errors.Is(err, tc.want) {
				t.Fatalf("VerifyObservation = %v, want %v", err, tc.want)
			}
		})
	}
	if err := VerifyObservation(expected, nil, now, time.Second); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("nil observation accepted: %v", err)
	}
	for _, duration := range []time.Duration{0, -time.Second} {
		if err := VerifyObservation(expected, &Observation{}, now, duration); !errors.Is(err, ErrObservationUnavailable) {
			t.Fatalf("invalid observation lifetime accepted: %v", err)
		}
	}
	if err := VerifyObservation(ExecutionIdentity{}, &Observation{}, now, time.Second); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("empty identities accepted: %v", err)
	}
	if err := VerifyObservation(expected, &Observation{}, time.Time{}, time.Second); !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("zero clock accepted: %v", err)
	}
}
