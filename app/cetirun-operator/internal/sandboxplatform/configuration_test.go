// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplatform_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

func TestConfigurationJSON(t *testing.T) {
	document := sandboxplatform.Configuration{
		Version:        "v1",
		Registrations:  map[api.SandboxType]sandbox.Registration{},
		Platforms:      map[api.SandboxType]sandboxpolicy.Platform{},
		RuntimeClasses: map[string]sandboxplatform.RuntimePrerequisite{"example-runtime": {UID: "example-runtime-uid", Handler: "kata"}},
		StorageClasses: map[string]sandboxplatform.StoragePrerequisite{"example-storage": {UID: "example-storage-uid", Provisioner: "csi.example.com"}},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":"v1","registrations":{},"platforms":{},"runtimeClasses":{"example-runtime":{"UID":"example-runtime-uid","Handler":"kata"}},"storageClasses":{"example-storage":{"UID":"example-storage-uid","Provisioner":"csi.example.com"}}}`
	if string(raw) != want {
		t.Fatalf("configuration JSON contract changed: %s", raw)
	}
	if sandboxplatform.ConfigurationKey != "platform.json" || sandboxplatform.MaxConfigurationBytes != 512*1024 {
		t.Fatal("configuration key or byte limit changed")
	}
}

func TestCheckFreshness(t *testing.T) {
	observed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		observedAt  time.Time
		now         time.Time
		maxAge      time.Duration
		unavailable bool
	}{
		{"zero observation", time.Time{}, observed, 0, true},
		{"zero clock", observed, time.Time{}, 0, true},
		{"both zero", time.Time{}, time.Time{}, 0, true},
		{"future observation", observed, observed.Add(-time.Nanosecond), 0, true},
		{"just captured", observed, observed, 0, false},
		{"default before expiry", observed, observed.Add(30*time.Second - time.Nanosecond), 0, false},
		{"default at expiry", observed, observed.Add(30 * time.Second), 0, true},
		{"negative default before expiry", observed, observed.Add(30*time.Second - time.Nanosecond), -time.Second, false},
		{"negative default at expiry", observed, observed.Add(30 * time.Second), -time.Second, true},
		{"explicit before expiry", observed, observed.Add(time.Minute - time.Nanosecond), time.Minute, false},
		{"explicit at expiry", observed, observed.Add(time.Minute), time.Minute, true},
		{"explicit after expiry", observed, observed.Add(2 * time.Minute), time.Minute, true},
		{"minimum just captured", observed, observed, time.Nanosecond, false},
		{"minimum at expiry", observed, observed.Add(time.Nanosecond), time.Nanosecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sandboxplatform.CheckFreshness(tc.observedAt, tc.now, tc.maxAge)
			if tc.unavailable {
				if !errors.Is(err, sandboxpolicy.ErrUnavailable) {
					t.Fatalf("expected unavailable snapshot, got %v", err)
				}
				if want := sandboxpolicy.ErrUnavailable.Error() + ": platform snapshot is stale or incomplete"; err.Error() != want {
					t.Fatalf("freshness diagnostic changed: %v", err)
				}
			} else if err != nil {
				t.Fatalf("fresh snapshot was rejected: %v", err)
			}
		})
	}
}
