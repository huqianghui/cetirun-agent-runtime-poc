// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func accessIntent() Intent {
	intent := testIntent("access")
	service := source("v1", "Service", intent.Namespace.Name, intent.Workload.Name, "service-uid")
	gateway := source("networking.istio.io/v1beta1", "Gateway", "gateway-system", "gateway", "gateway-uid")
	ingressNamespace := source("v1", "Namespace", "", "gateway-workloads", "ingress-namespace-uid")
	intent.Access = &AccessBinding{
		Route:   w.ObjectIdentity{Name: "cetirun-" + intent.Agent.Name, UID: "route-uid"},
		Service: service, Gateway: gateway, Host: "agents.example.com", Port: 18789,
		IngressNamespaces: []w.SourceReference{ingressNamespace},
	}
	intent.Sources = append(intent.Sources, service, gateway, ingressNamespace)
	intent.SourceDigests[w.SourceKey(service)] = strings.Repeat("b", 64)
	intent.SourceDigests[w.SourceKey(gateway)] = strings.Repeat("c", 64)
	intent.SourceDigests[w.SourceKey(ingressNamespace)] = strings.Repeat("d", 64)
	return intent
}

func TestAccessIntentRequiresExactSourcePins(t *testing.T) {
	for name, mutate := range map[string]func(*Intent){
		"disabled ingress": func(i *Intent) { i.IngressMode = api.AgentPolicyIngressDisabled },
		"route UID":        func(i *Intent) { i.Access.Route.UID = "" },
		"route name":       func(i *Intent) { i.Access.Route.Name = "another" },
		"host":             func(i *Intent) { i.Access.Host = "*.example.com" },
		"port":             func(i *Intent) { i.Access.Port = 65536 },
		"service key":      func(i *Intent) { i.Access.Service.Namespace = "another" },
		"service UID":      func(i *Intent) { i.Access.Service.UID = "another" },
		"gateway key":      func(i *Intent) { i.Access.Gateway.Name = "another" },
		"gateway type":     func(i *Intent) { i.Access.Gateway.Kind = "Secret" },
		"gateway digest":   func(i *Intent) { delete(i.SourceDigests, w.SourceKey(i.Access.Gateway)) },
		"ingress UID":      func(i *Intent) { i.Access.IngressNamespaces[0].UID = "another" },
		"ingress name":     func(i *Intent) { i.Access.IngressNamespaces[0].Name = "*" },
		"ingress kind":     func(i *Intent) { i.Access.IngressNamespaces[0].Kind = "ConfigMap" },
		"ingress scope":    func(i *Intent) { i.Access.IngressNamespaces[0].Namespace = "another" },
		"ingress digest":   func(i *Intent) { delete(i.SourceDigests, w.SourceKey(i.Access.IngressNamespaces[0])) },
		"duplicate ingress": func(i *Intent) {
			i.Access.IngressNamespaces = append(i.Access.IngressNamespaces, i.Access.IngressNamespaces[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			intent := accessIntent()
			if err := intent.Validate(); err != nil {
				t.Fatal("valid access fixture was rejected:", err)
			}
			mutate(&intent)
			if err := intent.Validate(); err == nil {
				t.Fatal("invalid access binding was accepted")
			}
		})
	}
}

func TestAccessIntentPersistsAnIndependentBinding(t *testing.T) {
	ctx := context.Background()
	store, _ := fixture(t)
	intent := accessIntent()
	ticket, err := store.AcquireExecution(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.Access.Host = "caller-mutation.example.com"
	intent.Access.IngressNamespaces[0].Name = "caller-mutation"
	original := ticket.DeepCopy()
	ticket.Execution.Intent.Access.Host = "returned-mutation.example.com"
	ticket.Execution.Intent.Access.IngressNamespaces[0].Name = "returned-mutation"
	stored, err := store.ReadExecution(ctx, original.Execution.Intent.Agent.UID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Execution.Intent.Access.Host != "agents.example.com" || original.Execution.Intent.Access.Host != "agents.example.com" {
		t.Fatal("caller mutation changed a persisted or copied binding")
	}
	if stored.Execution.Intent.Access.IngressNamespaces[0].Name != "gateway-workloads" ||
		original.Execution.Intent.Access.IngressNamespaces[0].Name != "gateway-workloads" {
		t.Fatal("caller mutation changed the persisted ingress consumer namespace")
	}
	if err := store.Check(ctx, ticket); err == nil {
		t.Fatal("mutated access target retained execution authority")
	}
	if err := store.Check(ctx, original); err != nil {
		t.Fatal("immutable original ticket changed:", err)
	}
}

func TestAccessInventoryBoundaries(t *testing.T) {
	if w.MaxPreparationSources != 128 || MaxPreparedExecutionSources != 129 ||
		MaxIngressNamespaces != 128 || MaxAccessSources != 130 || MaxSources != 259 {
		t.Fatal("preparation and bounded access inventory limits changed")
	}
	maximum := accessIntent()
	maximum.Access.IngressNamespaces = nil
	for i := range MaxIngressNamespaces {
		namespace := source("v1", "Namespace", "", fmt.Sprintf("consumer-%03d", i), fmt.Sprintf("consumer-uid-%03d", i))
		maximum.Access.IngressNamespaces = append(maximum.Access.IngressNamespaces, namespace)
		maximum.Sources = append(maximum.Sources, namespace)
		maximum.SourceDigests[w.SourceKey(namespace)] = strings.Repeat("d", 64)
	}
	for len(maximum.Sources) < MaxSources {
		index := len(maximum.Sources)
		extra := source("v1", "ConfigMap", "operator-state", fmt.Sprintf("input-%03d", index), fmt.Sprintf("input-uid-%03d", index))
		maximum.Sources = append(maximum.Sources, extra)
		maximum.SourceDigests[w.SourceKey(extra)] = strings.Repeat("e", 64)
	}
	if MaxSources != w.MaxPreparationSources+1+2+MaxIngressNamespaces || len(maximum.Sources) != MaxSources {
		t.Fatal("execution limit does not reserve the full bounded access dependency inventory")
	}
	for _, scenario := range []string{"maximum", "too many sources", "too many namespaces"} {
		t.Run(scenario, func(t *testing.T) {
			intent := (Ticket{Execution: Execution{Intent: maximum}}).DeepCopy().Execution.Intent
			switch scenario {
			case "too many sources":
				extra := source("v1", "ConfigMap", "operator-state", "overflow", "overflow-uid")
				intent.Sources = append(intent.Sources, extra)
				intent.SourceDigests[w.SourceKey(extra)] = strings.Repeat("f", 64)
			case "too many namespaces":
				// Keep the total below the combined bound, isolating the
				// independent ingress-namespace limit.
				intent = accessIntent()
				intent.Access.IngressNamespaces = nil
				for i := 0; i <= MaxIngressNamespaces; i++ {
					namespace := source("v1", "Namespace", "", fmt.Sprintf("consumer-%03d", i), fmt.Sprintf("consumer-uid-%03d", i))
					intent.Access.IngressNamespaces = append(intent.Access.IngressNamespaces, namespace)
					intent.Sources = append(intent.Sources, namespace)
					intent.SourceDigests[w.SourceKey(namespace)] = strings.Repeat("d", 64)
				}
				if len(intent.Sources) > MaxSources {
					t.Fatal("namespace overflow fixture must fit the combined source budget")
				}
			}
			store, storage := fixture(t)
			ctx := context.Background()
			key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
			before := &corev1.ConfigMap{}
			if err := storage.Get(ctx, key, before); err != nil {
				t.Fatal(err)
			}
			ticket, err := store.AcquireExecution(ctx, intent)
			if scenario == "maximum" {
				if err != nil {
					t.Fatal("maximum access inventory could not be persisted:", err)
				}
				restarted := Store{Client: storage, APIReader: storage, Namespace: store.Namespace, Identity: store.Identity}
				read, err := restarted.ReadExecution(ctx, intent.Agent.UID)
				if err != nil || !reflect.DeepEqual(read, ticket) {
					t.Fatal("maximum inventory was lost or truncated on readback:", err)
				}
				return
			}
			if err == nil {
				t.Fatal("oversized inventory acquired execution authority")
			}
			after := &corev1.ConfigMap{}
			if err := storage.Get(ctx, key, after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("oversized inventory changed coordination state")
			}
		})
	}
}
