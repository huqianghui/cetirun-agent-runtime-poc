// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package egresscapture

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func runtimeRegistration() Registration {
	return Registration{
		EnvoyImage: "registry.example.com/envoy@sha256:" + strings.Repeat("a", 64),
		InitImage:  "registry.example.com/cetirun-base@sha256:" + strings.Repeat("b", 64),
	}
}

func TestRuntimePlanRequiresPinnedIndependentImages(t *testing.T) {
	for _, registration := range []Registration{
		{}, {EnvoyImage: runtimeRegistration().EnvoyImage},
		{EnvoyImage: "envoyproxy/envoy:v1.36.3", InitImage: runtimeRegistration().InitImage},
		{EnvoyImage: runtimeRegistration().EnvoyImage, InitImage: "registry.example.com/agent:latest"},
	} {
		if plan, err := registration.Resolve([]string{"blocked.example.com"}, true, false); err == nil || plan != nil {
			t.Fatal("unpinned or absent trusted image accepted")
		}
	}
	if _, err := runtimeRegistration().Resolve(nil, true, false); err == nil {
		t.Fatal("empty controls must not acquire a capture plan")
	}
	if _, err := runtimeRegistration().Resolve([]string{"blocked.example.com"}, false, false); err == nil {
		t.Fatal("capture requires explicit IP families")
	}
}

func TestRuntimePlanFamiliesAndScript(t *testing.T) {
	for _, family := range []struct{ v4, v6 bool }{{true, false}, {false, true}, {true, true}} {
		input := []string{"z.example.com", "blocked.example.com", "blocked.example.com"}
		before := slices.Clone(input)
		plan, err := runtimeRegistration().Resolve(input, family.v4, family.v6)
		if err != nil || !slices.Equal(input, before) ||
			!slices.Equal(plan.Hostnames, []string{"blocked.example.com", "z.example.com"}) {
			t.Fatal("runtime plan changed input or failed normalization:", err)
		}
		config, err := plan.Config()
		if err != nil {
			t.Fatal(err)
		}
		var resources struct {
			Listeners []object `json:"listeners"`
		}
		if err := json.Unmarshal(config.StaticResources, &resources); err != nil {
			t.Fatal(err)
		}
		if (len(config.IPv4Commands) != 0) != family.v4 || (len(config.IPv6Commands) != 0) != family.v6 {
			t.Fatal("unregistered family acquired capture commands")
		}
		for _, listener := range resources.Listeners {
			if listener["name"] == "cetirun_egress_ipv4" && !family.v4 ||
				listener["name"] == "cetirun_egress_ipv6" && !family.v6 {
				t.Fatal("unregistered family acquired a listener")
			}
		}
		script, err := plan.Script(15440)
		if err != nil || !strings.Contains(script, "--noproxy '*'") ||
			!strings.Contains(script, "http://127.0.0.1:15440/ready") ||
			strings.Contains(script, "blocked.example.com") ||
			strings.Contains(script, "PRIVATE KEY") ||
			strings.Contains(script, "iptables -F") {
			t.Fatal("capture script has an unsafe dependency or untrusted interpolation:", err)
		}
		if strings.Index(script, "/ready") > strings.Index(script, "reconcile_capture ip") {
			t.Fatal("capture was installed before the controlled proxy became ready")
		}
	}
}
