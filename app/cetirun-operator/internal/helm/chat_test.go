// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	"helm.sh/helm/v3/pkg/chartutil"
	networkv1 "k8s.io/api/networking/v1"
)

func TestPortalChatPrivatePeerRendering(t *testing.T) {
	input := customInput(8080)
	before, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	b := input["customBindings"].(runtimebindings.Custom)
	caller := chataccess.Caller{Namespace: "test-platform", PodLabels: map[string]string{"app": "portal"}}
	b.Network.Callers = append(b.Network.Callers, runtimebindings.Peer{Namespace: caller.Namespace, PodLabels: caller.PodLabels})
	input["customBindings"] = b
	after, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	if !after.SupportsPortalChat() || before.WorkloadHash != after.WorkloadHash || before.PlanHash == after.PlanHash {
		t.Fatal("Chat access must change controls, not the Agent workload")
	}
	objects := platformRender(t, after, chartutil.DefaultCapabilities)
	var policy networkv1.NetworkPolicy
	if err := decodeObject(objects["NetworkPolicy"], &policy); err != nil {
		t.Fatal(err)
	}
	if !chataccess.Allows(&policy, caller, 8080) {
		t.Fatal("private caller rule missing")
	}
	if _, present := objects["VirtualService"]; present {
		t.Fatal("runtime Chart published a public Chat route")
	}
}
