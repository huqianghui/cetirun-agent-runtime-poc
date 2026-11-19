// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/telemetry"
)

func TestParseDefaults(t *testing.T) {
	o, err := parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.events != telemetry.EventsWarningsAndCetiRun || o.snapshotInterval != time.Hour ||
		o.healthAddress != ":8081" || o.metricsAddress != "0" {
		t.Fatalf("defaults = %+v", o)
	}
}

func TestParseRejectsInvalidSettings(t *testing.T) {
	for _, args := range [][]string{
		{"--events=everything"},
		{"--snapshot-interval=30s"},
		{"--unknown"},
		{"extra"},
	} {
		if _, err := parse(args); err == nil {
			t.Errorf("accepted %s", strings.Join(args, " "))
		}
	}
}
