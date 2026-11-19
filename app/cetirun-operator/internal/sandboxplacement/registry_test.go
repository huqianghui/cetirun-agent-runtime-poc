// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/apimachinery/pkg/types"
)

func TestStrictRegistryJSON(t *testing.T) {
	const empty = `{"version":"v1","nodes":[],"platformDaemonSets":[]}`
	if _, err := decodeRegistry(empty, "operator-state"); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"empty":                "",
		"null":                 "null",
		"array root":           "[]",
		"unknown version":      `{"version":"v2","nodes":[],"platformDaemonSets":[]}`,
		"missing nodes":        `{"version":"v1","platformDaemonSets":[]}`,
		"missing daemonsets":   `{"version":"v1","nodes":[]}`,
		"null nodes":           `{"version":"v1","nodes":null,"platformDaemonSets":[]}`,
		"null entry":           `{"version":"v1","nodes":[null],"platformDaemonSets":[]}`,
		"null daemonsets":      `{"version":"v1","nodes":[],"platformDaemonSets":null}`,
		"unknown field":        `{"version":"v1","nodes":[],"platformDaemonSets":[],"qualified":true}`,
		"duplicate version":    `{"version":"v1","version":"v1","nodes":[],"platformDaemonSets":[]}`,
		"duplicate nodes":      `{"version":"v1","nodes":[],"nodes":[],"platformDaemonSets":[]}`,
		"wrong array shape":    `{"version":"v1","nodes":{},"platformDaemonSets":[]}`,
		"case insensitive key": `{"Version":"v1","nodes":[],"platformDaemonSets":[]}`,
		"trailing document":    empty + empty,
		"trailing garbage":     empty + "?",
		"invalid utf8":         empty + string([]byte{255}),
		"depth budget":         strings.Repeat("[", maxRegistryDepth+1) + strings.Repeat("]", maxRegistryDepth+1),
		"token budget":         "[" + strings.Repeat("0,", maxRegistryTokens) + "0]",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRegistry(raw, "operator-state"); err == nil {
				t.Fatal("non-canonical registry shape accepted")
			}
		})
	}
	f := newFixture(t)
	valid := string(marshal(t, f.registry))
	for name, raw := range map[string]string{
		"duplicate nested UID": strings.Replace(valid, `"uid":"reserved-node-uid"`, `"uid":"reserved-node-uid","uid":"reserved-node-uid"`, 1),
		"unknown nested":       strings.Replace(valid, `"providerID":"example://reserved-worker"`, `"providerID":"example://reserved-worker","poolLabel":"not-authority"`, 1),
		"null reservation":     strings.Replace(valid, `"providerID":"example://ordinary-worker"`, `"providerID":"example://ordinary-worker","reservation":null`, 1),
		"null UID":             strings.Replace(valid, `"uid":"reserved-node-uid"`, `"uid":null`, 1),
		"null enrollment":      strings.Replace(valid, `"bootstrapPods":[`, `"bootstrapPods":[null,`, 1),
		"duplicate identity":   strings.Replace(valid, `"agent":{"name":"example"`, `"agent":{"name":"example","name":"example"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if raw == valid {
				t.Fatal("fixture mutation did not exercise the intended shape")
			}
			if _, err := decodeRegistry(raw, "operator-state"); err == nil {
				t.Fatal("ambiguous nested registry accepted")
			}
		})
	}
}

func TestRegistryBoundsAreInclusive(t *testing.T) {
	document := registry{Version: RegistryVersion, Nodes: []registeredNode{}, PlatformDaemonSets: []registeredDaemonSet{}}
	for i := range MaxNodes {
		document.Nodes = append(document.Nodes, registeredNode{
			Name: fmt.Sprintf("node-%d", i), UID: types.UID(fmt.Sprintf("node-uid-%d", i)), ProviderID: fmt.Sprintf("example://node-%d", i),
		})
	}
	for i := range MaxDaemonSets {
		document.PlatformDaemonSets = append(document.PlatformDaemonSets, registeredDaemonSet{
			Name: fmt.Sprintf("platform-%d", i), UID: types.UID(fmt.Sprintf("daemon-uid-%d", i)),
			Namespace:      sandboxworkload.ObjectIdentity{Name: "platform-system", UID: "namespace-uid"},
			TemplateDigest: strings.Repeat("0", 64), TrustedCreator: "system:kube-controller-manager",
			BootstrapPods: []sandboxworkload.ObjectIdentity{},
		})
	}
	if _, err := decodeRegistry(string(marshal(t, document)), "operator-state"); err != nil {
		t.Fatalf("exact inventory limits rejected: %v", err)
	}
	extraNodes := document
	extraNodes.Nodes = append(extraNodes.Nodes, registeredNode{Name: "extra-node", UID: "extra-node-uid", ProviderID: "example://extra-node"})
	if _, err := decodeRegistry(string(marshal(t, extraNodes)), "operator-state"); err == nil {
		t.Fatal("257 Nodes accepted")
	}
	extraDaemons := document
	extraDaemons.PlatformDaemonSets = append(extraDaemons.PlatformDaemonSets, registeredDaemonSet{
		Name: "extra-daemon", UID: "extra-daemon-uid", Namespace: sandboxworkload.ObjectIdentity{Name: "platform-system", UID: "namespace-uid"},
		TemplateDigest: strings.Repeat("0", 64), TrustedCreator: "system:kube-controller-manager", BootstrapPods: []sandboxworkload.ObjectIdentity{},
	})
	if _, err := decodeRegistry(string(marshal(t, extraDaemons)), "operator-state"); err == nil {
		t.Fatal("65 DaemonSets accepted")
	}
	raw := string(marshal(t, document))
	raw += strings.Repeat(" ", MaxRegistryBytes-len(raw))
	if _, err := decodeRegistry(raw, "operator-state"); err != nil {
		t.Fatalf("exact 256 KiB size limit rejected: %v", err)
	}
	if _, err := decodeRegistry(raw+" ", "operator-state"); err == nil {
		t.Fatal("oversized registry accepted")
	}
}

func TestRegistryIdentityValidation(t *testing.T) {
	for name, mutate := range map[string]func(*registry){
		"missing Node name":   func(r *registry) { r.Nodes[0].Name = "" },
		"invalid Node name":   func(r *registry) { r.Nodes[0].Name = "not/a/node" },
		"missing Node UID":    func(r *registry) { r.Nodes[0].UID = "" },
		"UID whitespace":      func(r *registry) { r.Nodes[0].UID = "node uid" },
		"missing provider":    func(r *registry) { r.Nodes[0].ProviderID = "" },
		"duplicate node name": func(r *registry) { r.Nodes[1].Name = r.Nodes[0].Name },
		"duplicate node UID":  func(r *registry) { r.Nodes[1].UID = r.Nodes[0].UID },
		"duplicate provider":  func(r *registry) { r.Nodes[1].ProviderID = r.Nodes[0].ProviderID },
		"missing Agent UID":   func(r *registry) { r.Nodes[0].Reservation.Agent.UID = "" },
		"missing STS UID":     func(r *registry) { r.Nodes[0].Reservation.StatefulSet.UID = "" },
		"missing NS UID":      func(r *registry) { r.Nodes[0].Reservation.Namespace.UID = "" },
		"state namespace":     func(r *registry) { r.Nodes[0].Reservation.Namespace.Name = "operator-state" },
		"invalid namespace":   func(r *registry) { r.PlatformDaemonSets[0].Namespace.Name = "not.a.namespace" },
		"conflicting NS UID": func(r *registry) {
			r.PlatformDaemonSets[0].Namespace.Name = r.Nodes[0].Reservation.Namespace.Name
			r.PlatformDaemonSets[0].Namespace.UID = "different-namespace-uid"
		},
		"missing digest":        func(r *registry) { r.PlatformDaemonSets[0].TemplateDigest = "" },
		"nonhex digest":         func(r *registry) { r.PlatformDaemonSets[0].TemplateDigest = strings.Repeat("g", 64) },
		"uppercase digest":      func(r *registry) { r.PlatformDaemonSets[0].TemplateDigest = strings.Repeat("A", 64) },
		"missing creator":       func(r *registry) { r.PlatformDaemonSets[0].TrustedCreator = "" },
		"wildcard creator":      func(r *registry) { r.PlatformDaemonSets[0].TrustedCreator = "system:*" },
		"anonymous creator":     func(r *registry) { r.PlatformDaemonSets[0].TrustedCreator = "system:anonymous" },
		"missing Pod inventory": func(r *registry) { r.PlatformDaemonSets[0].BootstrapPods = nil },
		"missing Pod UID":       func(r *registry) { r.PlatformDaemonSets[0].BootstrapPods[0].UID = "" },
		"duplicate DaemonSet":   func(r *registry) { r.PlatformDaemonSets = append(r.PlatformDaemonSets, r.PlatformDaemonSets[0]) },
		"duplicate Pod inventory": func(r *registry) {
			r.PlatformDaemonSets[0].BootstrapPods = append(r.PlatformDaemonSets[0].BootstrapPods, r.PlatformDaemonSets[0].BootstrapPods[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(&f.registry)
			if _, err := decodeRegistry(string(marshal(t, f.registry)), f.state.Namespace); err == nil {
				t.Fatal("incomplete or duplicate exact identity accepted")
			}
		})
	}
}
