// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
)

var testGatewaySelector = map[string]string{"app.kubernetes.io/name": "cetirun-ai-gateway"}

// testGatewayService is the Service testAIGatewayURL names.
func testGatewayService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cetirun-ai-gateway", Namespace: "cetirun-system"},
		Spec:       corev1.ServiceSpec{Selector: testGatewaySelector},
	}
}

func testGatewayPod(name, template string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	labels := map[string]string{"pod-template-hash": template}
	for key, value := range testGatewaySelector {
		labels[key] = value
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cetirun-system", Labels: labels},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

// rolloutTestReconciler returns a reconciler with a routed shared-model Agent
// and the given Gateway objects, and a function that drains the Agents the
// catalog queued.
func rolloutTestReconciler(t *testing.T, objects ...client.Object) (*AgentReconciler, *fakeAIGateway, func() []string) {
	t.Helper()
	r, _, gateway := aiGatewayTestReconciler(t, append([]client.Object{routedAgent()}, objects...)...)
	r.aiGatewayEvents = make(chan event.GenericEvent, 8)
	queued := func() []string {
		var names []string
		for {
			select {
			case queuedEvent := <-r.aiGatewayEvents:
				names = append(names, queuedEvent.Object.GetName())
			default:
				slices.Sort(names)
				return names
			}
		}
	}
	return r, gateway, queued
}

func catalogHas(r *AgentReconciler, name string) bool {
	shared, _ := r.aiGatewayCatalog.shared()
	return slices.ContainsFunc(shared, func(model aiGatewayCatalogModel) bool { return model.Name == name })
}

func addSharedTestModel(gateway *fakeAIGateway) {
	gateway.shared = append(gateway.shared, aigateway.Deployment{ID: "cfg-3", ModelName: "new-shared", AccessGroups: []string{aiGatewaySharedAccessGroup}})
}

func TestAIGatewayCatalogWaitsForARolloutBeforeApplyingAChange(t *testing.T) {
	ctx := context.Background()
	r, gateway, queued := rolloutTestReconciler(t, testGatewayService(),
		testGatewayPod("gateway-old", "old", true), testGatewayPod("gateway-new", "new", true),
		testGatewayPod("gateway-starting", "newer", false))

	// At operator start there is no view to keep: the first listing is
	// adopted even during a rollout.
	r.publishAIGatewayCatalog(ctx, 0)
	if got := queued(); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("the first listing queued %v, want the shared-model Agent", got)
	}
	if !catalogHas(r, testSharedModelOne) {
		t.Fatal("the first listing was not adopted")
	}

	// Ready pods of two templates: the change waits, and no Agent hears of it.
	addSharedTestModel(gateway)
	r.publishAIGatewayCatalog(ctx, 0)
	if catalogHas(r, "new-shared") {
		t.Fatal("a listing taken during a rollout replaced the catalog")
	}
	if got := queued(); len(got) != 0 {
		t.Fatalf("a held change queued %v", got)
	}
	if !r.aiGatewayCatalog.rollout.waiting() {
		t.Fatal("the publisher is not asked to list again soon")
	}
	if err := r.refreshAIGatewayCatalogForGrant(ctx, gateway); err != nil {
		t.Fatalf("a grant during a rollout failed: %v", err)
	}
	if catalogHas(r, "new-shared") || len(queued()) != 0 {
		t.Fatal("a grant during a rollout adopted the changed listing")
	}

	// One pod template serves the Gateway again: the change is applied once.
	if err := r.Delete(ctx, testGatewayPod("gateway-old", "old", true)); err != nil {
		t.Fatal(err)
	}
	r.aiGatewayCatalog.rollout.checkedAt = time.Time{}
	r.publishAIGatewayCatalog(ctx, 0)
	if !catalogHas(r, "new-shared") {
		t.Fatal("the change was not applied after the rollout")
	}
	if got := queued(); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("the applied change queued %v, want the shared-model Agent", got)
	}
	if r.aiGatewayCatalog.rollout.waiting() {
		t.Fatal("the rollout is still followed after it ended")
	}
}

// A change to a router's configuration alone, such as its tiers, keeps the
// shared models and their bindings; it is held during a rollout like any
// other catalog change, so keys follow it once, after the rollout.
func TestAIGatewayCatalogHoldsARouterChangeDuringARollout(t *testing.T) {
	ctx := context.Background()
	r, gateway, queued := rolloutTestReconciler(t, testGatewayService(),
		testGatewayPod("gateway-old", "old", true), testGatewayPod("gateway-new", "new", true))
	addSmallSharedModel(gateway)
	gateway.shared = append(gateway.shared, gatewayRouter(testGatewayRouter, tierModel("SIMPLE", testSmallModel)))
	r.publishAIGatewayCatalog(ctx, 0)
	queued()
	before := r.aiGatewayCatalog.routerFingerprint
	if before == "" {
		t.Fatal("the router was not taken into the catalog")
	}
	// The same listing again changes nothing, so it is not held: the
	// rollout is not followed for it.
	r.publishAIGatewayCatalog(ctx, 0)
	if r.aiGatewayCatalog.rollout.waiting() || len(queued()) != 0 {
		t.Fatal("an unchanged listing with a router was taken for a change")
	}

	setRouter(t, gateway, testGatewayRouter, aigateway.Router{Models: []aigateway.RouterModel{
		tierModel("SIMPLE", testSmallModel), tierModel("MEDIUM", testSharedModelOne)}})
	r.publishAIGatewayCatalog(ctx, 0)
	if r.aiGatewayCatalog.routerFingerprint != before {
		t.Fatal("a router change listed during a rollout replaced the catalog")
	}
	if got := queued(); len(got) != 0 {
		t.Fatalf("a held router change queued %v", got)
	}
	if !r.aiGatewayCatalog.rollout.waiting() {
		t.Fatal("the publisher is not asked to list again soon")
	}

	if err := r.Delete(ctx, testGatewayPod("gateway-old", "old", true)); err != nil {
		t.Fatal(err)
	}
	r.aiGatewayCatalog.rollout.checkedAt = time.Time{}
	r.publishAIGatewayCatalog(ctx, 0)
	if r.aiGatewayCatalog.routerFingerprint == before {
		t.Fatal("the router change was not applied after the rollout")
	}
	if got := queued(); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("the applied router change queued %v, want the shared-model Agent", got)
	}
}

func TestAIGatewayCatalogAppliesAChangeFromOnePodTemplate(t *testing.T) {
	ctx := context.Background()
	r, gateway, queued := rolloutTestReconciler(t, testGatewayService(),
		testGatewayPod("gateway-a", "same", true), testGatewayPod("gateway-b", "same", true),
		testGatewayPod("gateway-c", "next", false))
	r.publishAIGatewayCatalog(ctx, 0)
	queued()
	addSharedTestModel(gateway)
	r.publishAIGatewayCatalog(ctx, 0)
	if !catalogHas(r, "new-shared") {
		t.Fatal("a change served by one pod template was held")
	}
	if got := queued(); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("the change queued %v, want the shared-model Agent", got)
	}
}

func TestAIGatewayCatalogAppliesAChangeWhenThePodsCannotBeRead(t *testing.T) {
	ctx := context.Background()
	// No Service: as with a Gateway the operator cannot look into.
	r, gateway, queued := rolloutTestReconciler(t,
		testGatewayPod("gateway-old", "old", true), testGatewayPod("gateway-new", "new", true))
	r.publishAIGatewayCatalog(ctx, 0)
	queued()
	addSharedTestModel(gateway)
	r.publishAIGatewayCatalog(ctx, 0)
	if !catalogHas(r, "new-shared") {
		t.Fatal("a change was held although the Gateway's pods cannot be read")
	}
	if got := queued(); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("the change queued %v, want the shared-model Agent", got)
	}
	if r.aiGatewayCatalog.rollout.waiting() {
		t.Fatal("an unreadable Gateway must not be followed as a rollout")
	}
}

func TestAIGatewayCatalogStopsWaitingAtTheHoldLimit(t *testing.T) {
	ctx := context.Background()
	r, gateway, queued := rolloutTestReconciler(t, testGatewayService(),
		testGatewayPod("gateway-old", "old", true), testGatewayPod("gateway-new", "new", true))
	r.publishAIGatewayCatalog(ctx, 0)
	queued()
	addSharedTestModel(gateway)
	r.publishAIGatewayCatalog(ctx, 0)
	if catalogHas(r, "new-shared") {
		t.Fatal("the change was not held during the rollout")
	}
	r.aiGatewayCatalog.rollout.since = time.Now().Add(-aiGatewayRolloutHoldLimit - time.Second)
	r.publishAIGatewayCatalog(ctx, 0)
	if !catalogHas(r, "new-shared") {
		t.Fatal("the change was still held after the hold limit")
	}
	if got := queued(); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("the change queued %v, want the shared-model Agent", got)
	}
	if r.aiGatewayCatalog.rollout.waiting() {
		t.Fatal("the publisher still lists again soon after the hold limit")
	}
}

func TestAIGatewayServiceKeyAcceptsOnlyServiceNames(t *testing.T) {
	for raw, want := range map[string]string{
		"http://gateway.platform.svc.cluster.local:4000": "platform/gateway",
		"http://gateway.platform.svc:4000":               "platform/gateway",
	} {
		key, err := aiGatewayServiceKey(raw)
		if err != nil || key.String() != want {
			t.Fatalf("%s: got %v, %v; want %s", raw, key, err, want)
		}
	}
	for _, raw := range []string{
		"http://gateway.example.com:4000", "http://192.0.2.10:4000", "http://gateway:4000",
		"http://.platform.svc:4000", "http://gateway.platform.svc.example.com:4000",
	} {
		if _, err := aiGatewayServiceKey(raw); err == nil {
			t.Fatalf("%s was taken for a Service", raw)
		}
	}
}
