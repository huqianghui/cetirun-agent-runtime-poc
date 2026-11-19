// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"google.golang.org/protobuf/proto"
	istioapi "istio.io/api/networking/v1beta1"
	istio "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type accessFixture struct {
	*preparationFixture
	access      controller.SandboxExecutionAccess
	target      sandboxcoordination.EnrolledTarget
	intent      sandboxcoordination.Intent
	ticket      sandboxcoordination.Ticket
	observation sandbox.Observation
}

func newAccessFixture(t *testing.T, authenticated bool, ingressNamespaces ...string) *accessFixture {
	t.Helper()
	ctx := context.Background()
	if len(ingressNamespaces) == 0 {
		ingressNamespaces = []string{"gateway-system"}
	}
	f, target := executionIntentFixture(t, "openclaw", func(f *preparationFixture) {
		agent := f.current(t)
		agent.Spec.Agent.Port = ptr.To(int32(18789))
		if err := f.client.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		if authenticated {
			platform := f.config.Platforms[api.SandboxKata]
			original := platform.Topology.Ingress[0]
			platform.Topology.Ingress = nil
			for i, namespace := range ingressNamespaces {
				destination := original
				destination.Peer = sandboxpolicy.Peer{
					Name: fmt.Sprintf("ingress-%d", i), Namespace: namespace,
					UID: types.UID(fmt.Sprintf("ingress-uid-%d", i)), IPs: []string{fmt.Sprintf("10.96.1.%d", 60+i)},
				}
				platform.Topology.Ingress = append(platform.Topology.Ingress, destination)
			}
			f.config.Platforms[api.SandboxKata] = platform
			registration := f.config.Registrations[api.SandboxKata]
			registration.Capabilities.AuthenticatedIngress = true
			f.config.Registrations[api.SandboxKata] = registration
			policy := &api.AgentPolicy{}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Policy), policy); err != nil {
				t.Fatal(err)
			}
			policy.Spec.Network.Ingress.Mode = api.AgentPolicyIngressAuthenticated
			if err := f.client.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
		}
	})
	f.reconciler.IstioGateway = "gateway-system/native-gateway"
	for _, namespace := range ingressNamespaces {
		object := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		if err := f.client.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	gateway := &istio.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "native-gateway", Namespace: "gateway-system"},
		Spec: istioapi.Gateway{
			Selector: map[string]string{"app": "synthetic-gateway"},
			Servers: []*istioapi.Server{{
				Port:  &istioapi.Port{Name: "https", Number: 443, Protocol: "HTTPS"},
				Hosts: []string{"example.com"},
				Tls:   &istioapi.ServerTLSSettings{Mode: istioapi.ServerTLSSettings_SIMPLE, CredentialName: "synthetic-tls"},
			}},
		},
	}
	if err := f.client.Create(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	intent, err := f.reconciler.SandboxExecutionIntent(ctx, f.current(t), target)
	if err != nil {
		t.Fatal(err)
	}
	state, err := sandboxcoordination.NewState(f.reconciler.OperatorNamespace, "access-coordination")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.Create(ctx, state); err != nil {
		t.Fatal(err)
	}
	result := &accessFixture{preparationFixture: f, target: target, intent: intent}
	result.access = controller.SandboxExecutionAccess{
		Reconciler: f.reconciler,
		Coordination: sandboxcoordination.Store{Client: f.client, APIReader: f.client, Namespace: state.Namespace,
			Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID}},
		Now: func() time.Time { return f.now }, MaxAge: 15 * time.Second,
	}
	return result
}

func (f *accessFixture) enroll(t *testing.T) {
	t.Helper()
	before := f.current(t)
	intent, err := f.access.Enroll(context.Background(), f.intent)
	if err != nil {
		t.Fatal(err)
	}
	f.intent = intent
	if !reflect.DeepEqual(before, f.current(t)) || !controllerutil.ContainsFinalizer(f.current(t), sandbox.HoldFinalizer) {
		t.Fatal("access enrollment changed the Agent or lifted its execution hold")
	}
}

func (f *accessFixture) run(t *testing.T) {
	t.Helper()
	f.enroll(t)
	f.start(t)
}

func (f *accessFixture) start(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	ticket, err := f.access.Coordination.AcquireExecution(ctx, f.intent)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = f.access.Coordination.BeginStart(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	completeRunningFixture(t, f.preparationFixture, f.target, f.intent)
	ticket, err = f.access.Coordination.MarkRunning(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	f.ticket = ticket
	f.observation = sandbox.Observation{ExecutionIdentity: f.intent.Expected, ObservedAt: f.now,
		RuntimeVerified: true, PodVerified: true, ControlsVerified: true}
}

func (f *accessFixture) route(t *testing.T) *istio.VirtualService {
	t.Helper()
	route := &istio.VirtualService{}
	if err := f.client.Get(context.Background(), client.ObjectKey{
		Namespace: f.intent.Namespace.Name, Name: "cetirun-" + f.intent.Agent.Name,
	}, route); err != nil {
		t.Fatal(err)
	}
	return route
}

func (f *accessFixture) fillPreparedSources(t *testing.T) {
	t.Helper()
	expected := fillPreparedSourceInventory(t, f.preparationFixture)
	f.target.Binding, f.target.Revision = expected.Binding, expected.Revision
	intent, err := f.reconciler.SandboxExecutionIntent(context.Background(), f.current(t), f.target)
	if err != nil || len(intent.Sources) != sandboxcoordination.MaxPreparedExecutionSources {
		t.Fatalf("maximum preparation did not yield the full base execution inventory: count=%d err=%v", len(intent.Sources), err)
	}
	f.intent = intent
}

func TestSandboxAccessEnrolledAtMaximumPreparationSize(t *testing.T) {
	maximumConsumers := make([]string, sandboxcoordination.MaxIngressNamespaces)
	for i := range maximumConsumers {
		maximumConsumers[i] = fmt.Sprintf("ingress-%03d", i)
	}
	for _, consumers := range [][]string{
		{"gateway-workloads"},
		{"ingress-west", "ingress-east", "ingress-west"},
		maximumConsumers,
		append(slices.Clone(maximumConsumers), maximumConsumers[0]),
	} {
		t.Run(fmt.Sprintf("consumers=%d", len(consumers)), func(t *testing.T) {
			ctx := context.Background()
			f := newAccessFixture(t, true, consumers...)
			f.fillPreparedSources(t)
			before := f.intent.Sources
			f.run(t)
			if len(f.intent.Sources) != len(before)+2+len(f.intent.Access.IngressNamespaces) {
				t.Fatal("access did not append exactly the distinct Service, Gateway and ingress namespace dependencies")
			}
			for _, source := range before {
				if !slices.Contains(f.intent.Sources, source) {
					t.Fatal("access enrollment discarded a preparation source")
				}
			}
			current, err := f.access.Coordination.ReadExecution(ctx, f.intent.Agent.UID)
			if err != nil || !reflect.DeepEqual(current, f.ticket) {
				t.Fatal("coordination did not persist the complete access inventory:", err)
			}
			if err := f.access.Publish(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
				t.Fatal("maximum prepared inputs could not publish enrolled access:", err)
			}
			stopping, err := f.access.Coordination.BeginStop(ctx, f.ticket)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil {
				t.Fatal("maximum prepared inputs could not withdraw enrolled access:", err)
			}
		})
	}
}

func TestSandboxAccessRejectsExcessIngressNamespacesBeforeEnrollment(t *testing.T) {
	names := make([]string, sandboxcoordination.MaxIngressNamespaces+1)
	for i := range names {
		names[i] = fmt.Sprintf("ingress-%03d", i)
	}
	f := newAccessFixture(t, true, names...)
	before := f.current(t)
	key := client.ObjectKey{Namespace: f.access.Coordination.Namespace, Name: f.access.Coordination.Identity.Name}
	authority := &corev1.ConfigMap{}
	if err := f.client.Get(context.Background(), key, authority); err != nil {
		t.Fatal(err)
	}
	counts := &preparationRequestCounts{Client: f.client}
	f.reconciler.Client = counts
	consumerReads := 0
	f.reconciler.APIReader = &prerequisitesAPIReader{
		Reader: f.client, reads: map[client.ObjectKey]int{},
		afterGet: func(key client.ObjectKey, _ client.Object) error {
			if key.Namespace == "" && slices.Contains(names, key.Name) {
				consumerReads++
			}
			return nil
		},
	}
	if _, err := f.access.Enroll(context.Background(), f.intent); err == nil ||
		!strings.Contains(err.Error(), "ingress namespace inventory exceeds its bound") {
		t.Fatal("unbounded ingress inventory reached enrollment:", err)
	}
	currentAuthority := &corev1.ConfigMap{}
	if err := f.client.Get(context.Background(), key, currentAuthority); err != nil {
		t.Fatal(err)
	}
	if consumerReads != 0 || counts.writes != 0 || !reflect.DeepEqual(before, f.current(t)) ||
		!reflect.DeepEqual(authority, currentAuthority) {
		t.Fatal("excess ingress inventory read consumer namespaces or changed workload/coordination state")
	}
	if err := f.client.Get(context.Background(), client.ObjectKey{
		Namespace: f.intent.Namespace.Name, Name: "cetirun-" + f.intent.Agent.Name,
	}, &istio.VirtualService{}); !apierrors.IsNotFound(err) {
		t.Fatal("oversized ingress inventory created a route slot:", err)
	}
}

func TestSandboxAccessPublishesOnlyNativeUIAfterEvidence(t *testing.T) {
	ctx := context.Background()
	f := newAccessFixture(t, true)
	f.run(t)
	slot := f.route(t)
	if f.intent.Access == nil || slot.UID != f.intent.Access.Route.UID || len(slot.Spec.Http) != 1 ||
		slot.Spec.Http[0].DirectResponse.GetStatus() != 404 || len(slot.Spec.Http[0].Route) != 0 {
		t.Fatal("enrollment did not retain a nonforwarding exact-UID slot")
	}
	before := f.current(t)
	if err := f.access.Publish(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	current := f.route(t)
	if current.UID != slot.UID || current.ResourceVersion == slot.ResourceVersion || len(current.Spec.Http) != 3 {
		t.Fatal("publication did not conditionally update the enrolled slot")
	}
	path := "/ui/" + f.intent.Agent.Name
	for _, route := range current.Spec.Http {
		for _, match := range route.Match {
			if match.Uri.GetExact() != path && match.Uri.GetPrefix() != path+"/" {
				t.Fatal("native route exposed another path, helper or anonymous webhook")
			}
		}
		for _, destination := range route.Route {
			if destination.Destination.Port.Number != uint32(f.intent.Access.Port) ||
				destination.Destination.Host != f.intent.Workload.Name+"."+f.intent.Namespace.Name+".svc.cluster.local" {
				t.Fatal("native route escaped the pinned backend")
			}
		}
	}
	if !reflect.DeepEqual(before, f.current(t)) {
		t.Fatal("route publication changed Agent status or access readiness")
	}
	if err := f.access.Publish(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	if got := f.route(t); got.ResourceVersion != current.ResourceVersion {
		t.Fatal("unchanged access publication rewrote the route")
	}
}

func TestSandboxAccessExportsToIngressWorkloadNamespaces(t *testing.T) {
	for _, test := range []struct {
		name     string
		workload []string
		exported []string
	}{
		{"same namespace", []string{"gateway-system"}, []string{"gateway-system"}},
		{"separate namespace", []string{"gateway-workloads"}, []string{"gateway-workloads"}},
		{"multiple consumers", []string{"ingress-west", "ingress-east", "ingress-west"}, []string{"ingress-east", "ingress-west"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAccessFixture(t, true, test.workload...)
			f.run(t)
			if len(f.intent.Access.IngressNamespaces) != len(test.exported) {
				t.Fatal("ingress consumers were not deduplicated and persisted")
			}
			for i, source := range f.intent.Access.IngressNamespaces {
				if source.Name != test.exported[i] || source.Kind != "Namespace" || source.APIVersion != "v1" ||
					source.UID == "" || source.ResourceVersion == "" || f.intent.SourceDigests[w.SourceKey(source)] == "" {
					t.Fatal("ingress consumer namespace was not pinned as an effective source")
				}
			}
			check := func() {
				t.Helper()
				route := f.route(t)
				if !reflect.DeepEqual(route.Spec.Gateways, []string{"gateway-system/native-gateway"}) ||
					!reflect.DeepEqual(route.Spec.ExportTo, test.exported) {
					t.Fatalf("Gateway reference and consumer visibility differ: gateways=%v exportTo=%v",
						route.Spec.Gateways, route.Spec.ExportTo)
				}
			}
			check()
			if err := f.access.Publish(context.Background(), f.ticket, f.observation, statusTestGuard); err != nil {
				t.Fatal(err)
			}
			check()
		})
	}
}

func TestSandboxAccessRequiresCurrentIngressNamespace(t *testing.T) {
	for _, scenario := range []string{"deleted", "replaced", "terminating", "control labels", "topology", "metadata only"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := newAccessFixture(t, true, "gateway-workloads")
			f.run(t)
			before := f.route(t)
			namespace := &corev1.Namespace{}
			if err := f.client.Get(ctx, client.ObjectKey{Name: "gateway-workloads"}, namespace); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "deleted", "replaced":
				if err := f.client.Delete(ctx, namespace); err != nil {
					t.Fatal(err)
				}
				if scenario == "replaced" {
					namespace.UID, namespace.ResourceVersion = "replacement-namespace", ""
					if err := f.client.Create(ctx, namespace); err != nil {
						t.Fatal(err)
					}
				}
			case "terminating":
				namespace.Finalizers = []string{"example.com/hold"}
				if err := f.client.Update(ctx, namespace); err != nil {
					t.Fatal(err)
				}
				if err := f.client.Delete(ctx, namespace); err != nil {
					t.Fatal(err)
				}
			case "control labels":
				namespace.Labels = map[string]string{"istio.io/rev": "changed"}
				if err := f.client.Update(ctx, namespace); err != nil {
					t.Fatal(err)
				}
			case "topology":
				platform := f.config.Platforms[api.SandboxKata]
				platform.Topology.Ingress[0].Peer.Namespace = "other-ingress"
				f.config.Platforms[api.SandboxKata] = platform
			case "metadata only":
				namespace.Annotations = map[string]string{"example.com/description": "updated"}
				if err := f.client.Update(ctx, namespace); err != nil {
					t.Fatal(err)
				}
			}
			err := f.access.Publish(ctx, f.ticket, f.observation, statusTestGuard)
			if scenario == "metadata only" {
				if err != nil {
					t.Fatal("harmless namespace metadata invalidated enrolled access:", err)
				}
			} else if err == nil || !reflect.DeepEqual(before, f.route(t)) {
				t.Fatal("namespace drift did not preserve the nonforwarding slot:", err)
			}
		})
	}
}

func TestSandboxAccessRejectsMissingIngressNamespaceBeforeEnrollment(t *testing.T) {
	f := newAccessFixture(t, true, "gateway-workloads")
	ctx := context.Background()
	if err := f.client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gateway-workloads"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.Enroll(ctx, f.intent); !apierrors.IsNotFound(err) {
		t.Fatal("enrollment did not require the resolved consumer namespace:", err)
	}
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: f.intent.Namespace.Name,
		Name: "cetirun-" + f.intent.Agent.Name}, &istio.VirtualService{}); !apierrors.IsNotFound(err) {
		t.Fatal("failed ingress namespace resolution created an access slot:", err)
	}
}

func TestSandboxAccessUnpinnedLegacyConsumersPermitOnlyWithdrawal(t *testing.T) {
	ctx := context.Background()
	f := newAccessFixture(t, true)
	f.enroll(t)
	f.intent.Access.IngressNamespaces = nil
	f.start(t)
	if err := f.access.Publish(ctx, f.ticket, f.observation, statusTestGuard); err == nil {
		t.Fatal("a legacy binding inferred ingress consumers for new publication")
	}
	stopping, err := f.access.Coordination.BeginStop(ctx, f.ticket)
	if err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation = nil
	if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil {
		t.Fatal("an unpinned legacy binding prevented restrictive cleanup:", err)
	}
}

func TestSandboxAccessFailsClosedOnEvidenceOrSourceChanges(t *testing.T) {
	for _, scenario := range []string{
		"stale", "mismatched", "incomplete", "missing guard", "changed authority", "wrong phase",
		"service", "gateway", "domain", "wrong UID", "missing slot", "held", "missing access",
	} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := newAccessFixture(t, true)
			f.run(t)
			before := f.route(t)
			guard := statusTestGuard
			observation := f.observation
			ticket := f.ticket.DeepCopy()
			switch scenario {
			case "stale":
				f.now = f.now.Add(f.access.MaxAge + time.Nanosecond)
			case "mismatched":
				observation.PodUID = "other-pod"
			case "incomplete":
				observation.ControlsVerified = false
			case "missing guard":
				guard = nil
			case "changed authority":
				ticket.AuthorityUID = "other-authority"
			case "wrong phase":
				ticket.Execution.Phase = sandboxcoordination.Starting
			case "service":
				service := &corev1.Service{}
				if err := f.client.Get(ctx, client.ObjectKey{Namespace: f.intent.Namespace.Name, Name: f.intent.Workload.Name}, service); err != nil {
					t.Fatal(err)
				}
				service.Spec.Selector = map[string]string{"app": "another"}
				if err := f.client.Update(ctx, service); err != nil {
					t.Fatal(err)
				}
			case "gateway":
				gateway := &istio.Gateway{}
				if err := f.client.Get(ctx, client.ObjectKey{Namespace: "gateway-system", Name: "native-gateway"}, gateway); err != nil {
					t.Fatal(err)
				}
				gateway.Spec.Servers[0].Tls = nil
				if err := f.client.Update(ctx, gateway); err != nil {
					t.Fatal(err)
				}
			case "domain":
				f.reconciler.DomainSuffix = "changed.example.com"
			case "wrong UID", "missing slot":
				if err := f.client.Delete(ctx, before); err != nil {
					t.Fatal(err)
				}
				if scenario == "wrong UID" {
					replacement := before.DeepCopy()
					replacement.UID, replacement.ResourceVersion = "replacement-route", ""
					if err := f.client.Create(ctx, replacement); err != nil {
						t.Fatal(err)
					}
				}
			case "held":
				agent := f.current(t)
				controllerutil.AddFinalizer(agent, sandbox.HoldFinalizer)
				if err := f.client.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
			case "missing access":
				ticket.Execution.Intent.Access = nil
			}
			if err := f.access.Publish(ctx, ticket, observation, guard); err == nil {
				t.Fatal("invalid access publication succeeded")
			}
			if scenario != "missing slot" && scenario != "wrong UID" {
				if after := f.route(t); !reflect.DeepEqual(before, after) {
					t.Fatal("rejected publication changed the slot")
				}
			}
		})
	}
}

func TestSandboxAccessWithdrawalUsesHistoricalIdentity(t *testing.T) {
	ctx := context.Background()
	f := newAccessFixture(t, true, "gateway-workloads")
	f.run(t)
	if err := f.access.Publish(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	stopping, err := f.access.Coordination.BeginStop(ctx, f.ticket)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, f.fixture.Policy); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gateway-workloads"}}); err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation = nil
	f.reconciler.DomainSuffix, f.reconciler.IstioGateway = "", ""
	var withdrawn []string
	f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if route, ok := object.(*istio.VirtualService); ok {
				withdrawn = append([]string{}, route.Spec.ExportTo...)
			}
			return c.Update(ctx, object, options...)
		},
	})
	if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil {
		t.Fatal("withdrawal depended on current policy/configuration:", err)
	}
	if !reflect.DeepEqual(withdrawn, []string{"gateway-workloads"}) {
		t.Fatalf("withdrawal lost the enrolled ingress consumer: %v", withdrawn)
	}
	err = f.client.Get(ctx, client.ObjectKey{Namespace: f.intent.Namespace.Name, Name: f.intent.Access.Route.Name}, &istio.VirtualService{})
	if !apierrors.IsNotFound(err) {
		t.Fatal("withdrawn route did not reach observed absence:", err)
	}
	if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil {
		t.Fatal("repeated withdrawal was not idempotent:", err)
	}
	if err := f.access.Coordination.Check(ctx, stopping); err != nil {
		t.Fatal("route withdrawal released the execution reservation:", err)
	}
}

func TestSandboxAccessRejectsDelayedPublicationAfterWithdrawal(t *testing.T) {
	ctx := context.Background()
	f := newAccessFixture(t, true)
	f.run(t)
	original := f.route(t)
	calls := 0
	f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			route, ok := object.(*istio.VirtualService)
			if !ok {
				t.Fatal("publication attempted a non-route write")
			}
			calls++
			stopping, err := f.access.Coordination.BeginStop(ctx, f.ticket)
			if err != nil {
				return err
			}
			f.reconciler.Client = c
			if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil {
				return err
			}
			err = c.Update(ctx, route, options...)
			if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				t.Fatal("delayed publication could recreate the retired route:", err)
			}
			return err
		},
	})
	if err := f.access.Publish(ctx, f.ticket, f.observation, statusTestGuard); err == nil || calls != 1 {
		t.Fatal("delayed publication did not remain failed after stop:", err)
	}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(original), &istio.VirtualService{}); !apierrors.IsNotFound(err) {
		t.Fatal("old route was recreated:", err)
	}
}

func TestSandboxAccessNeverDeletesReplacementOrClaimsUnobservedWrites(t *testing.T) {
	for _, scenario := range []string{"replacement", "withdrawal error", "lost publication", "expired publication"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := newAccessFixture(t, true)
			f.run(t)
			before := f.route(t)
			failure := errors.New("synthetic write outcome unavailable")
			if scenario == "replacement" {
				if err := f.client.Delete(ctx, before); err != nil {
					t.Fatal(err)
				}
				before.UID, before.ResourceVersion = "other-route", ""
				if err := f.client.Create(ctx, before); err != nil {
					t.Fatal(err)
				}
			} else {
				f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
						if scenario == "withdrawal error" {
							return failure
						}
						if err := c.Update(ctx, object, options...); err != nil {
							return err
						}
						if scenario == "expired publication" {
							f.now = f.now.Add(f.access.MaxAge + time.Nanosecond)
							return nil
						}
						return failure
					},
				})
			}
			if scenario == "replacement" || scenario == "withdrawal error" {
				stopping, err := f.access.Coordination.BeginStop(ctx, f.ticket)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err == nil {
					t.Fatal("unverified withdrawal succeeded")
				}
				if !proto.Equal(&before.Spec, &f.route(t).Spec) {
					t.Fatal("failed withdrawal changed a foreign or protected route")
				}
			} else if err := f.access.Publish(ctx, f.ticket, f.observation, statusTestGuard); err == nil {
				t.Fatal("uncertain or expired publication was reported as verified")
			}
		})
	}
}

func TestSandboxAccessDisabledIngressCreatesNothing(t *testing.T) {
	ctx := context.Background()
	f := newAccessFixture(t, false)
	f.run(t)
	if f.intent.Access != nil {
		t.Fatal("disabled ingress enrolled an access route")
	}
	if err := f.access.Publish(ctx, f.ticket, f.observation, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	stopping, err := f.access.Coordination.BeginStop(ctx, f.ticket)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil {
		t.Fatal(err)
	}
	if f.current(t).Status.AccessURL != "" {
		t.Fatal("disabled ingress published a URL")
	}
	route := &istio.VirtualService{ObjectMeta: metav1.ObjectMeta{
		Namespace: f.intent.Namespace.Name, Name: "cetirun-" + f.intent.Agent.Name,
	}}
	if err := f.client.Create(ctx, route); err != nil {
		t.Fatal(err)
	}
	if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err == nil {
		t.Fatal("disabled ingress treated a foreign route as absent")
	}
}

func TestSandboxAccessEnrollmentRejectsMisconfiguredInputs(t *testing.T) {
	for _, scenario := range []string{"host", "gateway", "foreign slot", "wrong TLS"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAccessFixture(t, true)
			switch scenario {
			case "host":
				f.reconciler.DomainSuffix = "*.example.com"
			case "gateway":
				f.reconciler.IstioGateway = "implicit-default"
			case "foreign slot":
				if err := f.client.Create(context.Background(), &istio.VirtualService{
					ObjectMeta: metav1.ObjectMeta{Name: "cetirun-" + f.intent.Agent.Name, Namespace: f.intent.Namespace.Name},
				}); err != nil {
					t.Fatal(err)
				}
			case "wrong TLS":
				gateway := &istio.Gateway{}
				if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: "gateway-system", Name: "native-gateway"}, gateway); err != nil {
					t.Fatal(err)
				}
				gateway.Spec.Servers[0].Port.Protocol = "HTTP"
				if err := f.client.Update(context.Background(), gateway); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.access.Enroll(context.Background(), f.intent); err == nil {
				t.Fatalf("invalid enrollment %s succeeded", scenario)
			}
		})
	}
}
