// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
)

func withEgressSNI(input CompileInput) CompileInput {
	input.Resolution.Capabilities.BlockedFQDNs = true
	input.Platform.EgressSNI = &egresscapture.Registration{
		EnvoyImage: "registry.example.com/envoy@sha256:" + strings.Repeat("a", 64),
		InitImage:  "registry.example.com/cetirun-base@sha256:" + strings.Repeat("b", 64),
	}
	input.Resolution.AgentPolicy.Network.BlockedFQDNs = []string{"blocked.example.com"}
	return input
}

func TestEgressSNICompilerModes(t *testing.T) {
	for _, harness := range []string{"openclaw", "hermes"} {
		for _, ingress := range []bool{false, true} {
			t.Run(harness+"/"+map[bool]string{false: "fqdn-only", true: "both"}[ingress], func(t *testing.T) {
				input := withEgressSNI(ingressMTLSFixture(t, harness))
				input.Resolution.AgentPolicy.Network.Ingress.RequireMTLS = ingress
				if !ingress {
					input.Platform.IngressMTLS = nil
					input.Resolution.Capabilities.MutualTLS = false
				}
				plan := mustCompile(t, input)
				if plan.EgressSNI == nil || plan.ProxyDigest == "" || (plan.IngressMTLS != nil) != ingress {
					t.Fatal("missing or conflated ingress/egress controls")
				}
				expectedObjects := 2
				if ingress {
					expectedObjects++
				}
				if len(plan.Resources) != expectedObjects || plan.Resources[1].GetKind() != "ConfigMap" {
					t.Fatal("shared Envoy requires one ConfigMap, with a DestinationRule only for mTLS")
				}
				data := plan.Resources[1].Object["data"].(map[string]any)
				bootstrap := data["envoy.json"].(string)
				if !strings.Contains(bootstrap, "cetirun_egress_") || !strings.Contains(bootstrap, "cetirun_proxy_health") ||
					!strings.Contains(data["capture.sh"].(string), "--uid-owner 1337") {
					t.Fatal("compiled resource lacks actual capture/Envoy wiring")
				}
				if strings.Contains(bootstrap, "DownstreamTlsContext") != ingress ||
					strings.Contains(bootstrap, "/ingress-tls/") != ingress {
					t.Fatal("FQDN-only acquired certificate dependencies or both lost TLS")
				}
				if !strings.Contains(bootstrap, `"pipe"`) || !strings.Contains(bootstrap, `"exact":"GET"`) ||
					strings.Contains(bootstrap, "quitquitquit") {
					t.Fatal("agent-facing health must not expose mutating admin handlers")
				}
				input.Resolution.AgentPolicy.Network.BlockedFQDNs = nil
				withoutEgress := mustCompile(t, input)
				if withoutEgress.EgressSNI != nil || withoutEgress.ProxyDigest != "" {
					t.Fatal("empty list retained capture")
				}
				if !reflect.DeepEqual(plan.Network.Policy.Spec, withoutEgress.Network.Policy.Spec) {
					t.Fatal("observed-SNI capture changed Cilium CIDR/port enforcement")
				}
				if plan.Revision == withoutEgress.Revision {
					t.Fatal("capture removal did not change the execution revision")
				}
				if ingress {
					before, _ := json.Marshal(plan.Resources[2].Object)
					after, _ := json.Marshal(withoutEgress.Resources[2].Object)
					var a, b map[string]any
					_ = json.Unmarshal(before, &a)
					_ = json.Unmarshal(after, &b)
					if !reflect.DeepEqual(a["spec"], b["spec"]) {
						t.Fatal("egress composition changed mTLS routing")
					}
				} else if len(withoutEgress.Resources) != 1 {
					t.Fatal("neither control retained proxy resources")
				}
			})
		}
	}
}

func TestEgressSNICompilerRequiresCompleteRegistration(t *testing.T) {
	for name, mutate := range map[string]func(*CompileInput){
		"no capability":   func(i *CompileInput) { i.Resolution.Capabilities.BlockedFQDNs = false },
		"no registration": func(i *CompileInput) { i.Platform.EgressSNI = nil },
		"tag-only init":   func(i *CompileInput) { i.Platform.EgressSNI.InitImage = "registry.example.com/init:latest" },
		"tag-only Envoy":  func(i *CompileInput) { i.Platform.EgressSNI.EnvoyImage = "envoyproxy/envoy:v1.36.3" },
		"two proxy images": func(i *CompileInput) {
			i.Platform.EgressSNI.EnvoyImage = "registry.example.com/envoy@sha256:" + strings.Repeat("c", 64)
		},
		"legacy execution": func(i *CompileInput) { i.NativeExecution = false },
	} {
		t.Run(name, func(t *testing.T) {
			input := withEgressSNI(ingressMTLSFixture(t, "openclaw"))
			mutate(&input)
			if plan, err := Compile(input); err == nil || plan != nil {
				t.Fatal("incomplete capture registration produced executable controls")
			}
		})
	}
}

func TestEgressSNIRevisionBindsPolicyAndImages(t *testing.T) {
	input := withEgressSNI(ingressMTLSFixture(t, "openclaw"))
	first := mustCompile(t, input)
	input.Resolution.AgentPolicy.Network.BlockedFQDNs = []string{"other.example.com"}
	second := mustCompile(t, input)
	input.Platform.EgressSNI.InitImage = "registry.example.com/cetirun-base@sha256:" + strings.Repeat("d", 64)
	third := mustCompile(t, input)
	if first.Revision == second.Revision || second.Revision == third.Revision ||
		first.ProxyDigest == second.ProxyDigest || second.ProxyDigest == third.ProxyDigest {
		t.Fatal("policy and trusted init image changes must invalidate active execution")
	}
}
