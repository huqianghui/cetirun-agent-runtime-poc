// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

func TestConfigurationRefreshJitterAndRemainingFreshness(t *testing.T) {
	observed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		maxAge   time.Duration
		age      time.Duration
		lifetime time.Duration
	}{
		{"default", 0, 0, 30 * time.Second},
		{"explicit", time.Minute, 0, time.Minute},
		{"near expiry", 30 * time.Second, 29 * time.Second, time.Second},
		{"minimum", time.Nanosecond, 0, time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := make(map[time.Duration]bool)
			for range 32 {
				delay := configurationRefresh(tc.maxAge, observed.Add(tc.age), observed)
				lower, upper := max(time.Nanosecond, tc.lifetime/2), max(time.Nanosecond, tc.lifetime/2+tc.lifetime/8)
				if delay < lower || delay > upper || delay > tc.lifetime {
					t.Fatalf("refresh %s outside [%s,%s], lifetime %s", delay, lower, upper, tc.lifetime)
				}
				values[delay] = true
			}
			if tc.lifetime > time.Second && len(values) == 1 {
				t.Fatal("refresh intervals remained synchronized")
			}
		})
	}
}

func TestConfigurationSnapshotFreshnessBoundaries(t *testing.T) {
	observed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{-time.Nanosecond, 0, 30*time.Second - time.Nanosecond, 30 * time.Second, time.Minute} {
		err := configurationSnapshotTime(observed, observed.Add(age), 30*time.Second)
		wantUnavailable := age < 0 || age >= 30*time.Second
		if errors.Is(err, sandboxpolicy.ErrUnavailable) != wantUnavailable || !wantUnavailable && err != nil {
			t.Fatalf("freshness age %s: %v", age, err)
		}
	}
	if err := configurationSnapshotTime(time.Time{}, observed, 0); !errors.Is(err, sandboxpolicy.ErrUnavailable) {
		t.Fatal("zero observation must not become fresh", err)
	}
}

func TestConfigurationSnapshotValidatesAfterCapture(t *testing.T) {
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		observedAfter   time.Duration
		captureDuration time.Duration
		unavailable     bool
	}{
		{"captured before request", -time.Second, time.Second, false},
		{"captured during request", time.Second, time.Second, false},
		{"fresh just before expiry", 0, 30*time.Second - time.Nanosecond, false},
		{"expires during request", 0, 30 * time.Second, true},
		{"future after request completes", 2 * time.Second, time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := started
			observed := started.Add(tc.observedAfter)
			source := SandboxConfigurationSourceFunc(func(context.Context) (SandboxConfigurationSnapshot, error) {
				now = started.Add(tc.captureDuration)
				return SandboxConfigurationSnapshot{ObservedAt: observed}, nil
			})
			snapshot, err := configurationSnapshot(context.Background(), source, func() time.Time { return now }, 30*time.Second)
			if tc.unavailable {
				if !errors.Is(err, sandboxpolicy.ErrUnavailable) || !snapshot.ObservedAt.IsZero() {
					t.Fatalf("invalid observation was not rejected: snapshot=%+v, err=%v", snapshot, err)
				}
			} else if err != nil || !snapshot.ObservedAt.Equal(observed) {
				t.Fatalf("fresh observation was not preserved: snapshot=%+v, err=%v", snapshot, err)
			}
		})
	}
}
