// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload_test

import (
	"slices"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
)

func TestControlsPreserveCompilerDNSAndDistinctRevisions(t *testing.T) {
	fixture := testfixture.New("hermes")
	foundation := fixture.Input.Resolution.Revision
	plan := sandboxpolicy.Plan{
		Binding: fixture.Input.Resolution.Binding, ResolutionRevision: foundation,
		Revision: "effective-topology-and-grants-revision",
		Network: sandboxpolicy.NetworkPlan{
			DNSIPs: []string{"10.96.0.11", "10.96.0.10"}, IngressMode: fixture.Input.Resolution.AgentPolicy.Network.Ingress.Mode,
		},
		Identity:  sandboxpolicy.IdentityPlan{ServiceAccountName: "default"},
		Resources: fixture.Input.Controls.Objects,
	}

	for i := range plan.Resources {
		annotations := plan.Resources[i].GetAnnotations()
		annotations[sandboxpolicy.RevisionAnnotation] = plan.Revision
		plan.Resources[i].SetAnnotations(annotations)
	}
	controls, err := w.ControlsFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if controls.Binding != plan.Binding || controls.ResolutionRevision != foundation ||
		controls.Revision != plan.Revision || controls.IngressMode != plan.Network.IngressMode ||
		!slices.Equal(controls.DNSIPs, plan.Network.DNSIPs) {
		t.Fatal("compiler DNS, ingress or distinct revision bindings were lost")
	}
	plan.Network.DNSIPs[0] = "192.0.2.53"
	if controls.DNSIPs[0] != "10.96.0.11" {
		t.Fatal("carrier aliases compiler DNS output")
	}
	fixture.Input.Controls = controls
	bundle, err := w.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bundle.DNSConfig().Nameservers, []string{"10.96.0.11", "10.96.0.10"}) ||
		fixture.Input.Resolution.Revision != foundation || bundle.Identity().Resolution.Revision != foundation ||
		bundle.Identity().Controls.Revision != controls.Revision {
		t.Fatal("bundle rewrote compiler DNS order or the foundation revision")
	}
	plan.Identity.AutomountServiceAccountToken = true
	if _, err := w.ControlsFromPlan(plan); err == nil {
		t.Fatal("compiler automount override was accepted")
	}
}

func TestControlsPreserveNativeProxyIPsWithoutAliasing(t *testing.T) {
	plan := sandboxpolicy.Plan{Network: sandboxpolicy.NetworkPlan{TrustedProxyIPs: []string{"192.0.2.10", "2001:db8::10"}}}
	controls, err := w.ControlsFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(controls.TrustedProxyIPs, plan.Network.TrustedProxyIPs) {
		t.Fatal("native proxy IPs were not carried from the compiler")
	}
	plan.Network.TrustedProxyIPs[0] = "192.0.2.11"
	if controls.TrustedProxyIPs[0] != "192.0.2.10" {
		t.Fatal("proxy carrier aliases compiler output")
	}
}
