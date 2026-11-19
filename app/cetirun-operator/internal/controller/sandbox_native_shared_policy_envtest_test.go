// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNativeSandboxMetadataUpdatePreservesAuthorizationAPIServer(t *testing.T) {
	testNativeSandboxPolicyUpdatePreservesAuthorizationAPIServer(t, false)
}

func TestNativeSandboxRateLimitUpdatePreservesAuthorizationAPIServer(t *testing.T) {
	testNativeSandboxPolicyUpdatePreservesAuthorizationAPIServer(t, true)
}

func testNativeSandboxPolicyUpdatePreservesAuthorizationAPIServer(t *testing.T, rateLimits bool) {
	h := newNativeStartAPIHarness(t, true, true)
	store := h.r.SandboxNativeCoordination
	team := &api.Team{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: h.agent.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	policy := &api.AgentPolicy{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: team.Spec.AgentPolicyRef}, policy); err != nil {
		t.Fatal(err)
	}
	original := policy.DeepCopy()
	policy.Labels = map[string]string{"example.com/manifest": "minimal"}
	raw := &unstructured.Unstructured{}
	raw.SetAPIVersion(api.GroupVersion.String())
	raw.SetKind("AgentPolicy")
	if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(policy), raw); err != nil {
		t.Fatal(err)
	}
	if policy.Spec.AgentIdentity.Allowed || policy.Spec.AgentIdentity.Required || len(policy.Spec.AgentIdentity.CredentialServices) != 0 {
		t.Fatal("fixture must not remove an effective identity entitlement")
	}
	raw.SetLabels(policy.Labels)
	unstructured.RemoveNestedField(raw.Object, "spec", "agentIdentity")
	ticket, err := store.AcquireConfigurationChange(h.ctx, original, policy)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = store.BeginChange(h.ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	// Establish the minimal wire representation before any Agent starts.
	if err := h.admin.Update(h.ctx, raw, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
		t.Fatal(err)
	}
	adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(h.admin, h.admin, original, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileChange(h.ctx, ticket, adapter); err != nil {
		t.Fatal(err)
	}
	policies := &AgentPolicyReconciler{Client: h.kube, APIReader: h.kube,
		Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}
	if _, err := policies.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(policy), raw); err != nil {
		t.Fatal(err)
	}
	if _, found, err := unstructured.NestedFieldNoCopy(raw.Object, "spec", "agentIdentity"); err != nil || found {
		t.Fatal("fixture did not retain an omitted defaulted spec object:", err)
	}
	if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}

	otherTeam := team.DeepCopy()
	otherTeam.ObjectMeta = metav1.ObjectMeta{Name: "other-team"}
	otherTeam.Status = api.TeamStatus{}
	h.create(t, otherTeam)
	teams := &TeamReconciler{Client: h.kube, APIReader: h.kube}
	if _, err := teams.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(otherTeam)}); err != nil {
		t.Fatal(err)
	}
	agent := h.agent.DeepCopy()
	agent.ObjectMeta = metav1.ObjectMeta{Name: "other-agent"}
	agent.Status = api.AgentStatus{}
	agent.Spec.User.Alias, agent.Spec.Team = agent.Name, otherTeam.Name
	h.create(t, agent)
	peer := *h
	peer.nativeSandboxFixture = &nativeSandboxFixture{r: h.r, kube: h.kube, gateway: h.gateway, agent: agent}
	type running struct {
		harness  *nativeStartAPIHarness
		expected *w.Expectation
		route    *unstructured.Unstructured
	}
	runs := []running{{harness: h}, {harness: &peer}}
	for i := range runs {
		run := &runs[i]
		current := run.harness
		current.seedRetainedBootstrap(t)
		current.create(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Namespace: instanceNamespace(current.agent.Name), Name: "default",
		}})
		current.prepare(t)
		current.reconcileOK(t)
		_, expected := current.expectation(t)
		pod := testfixture.Pod(*expected)
		pod.Spec.Overhead = nil
		current.create(t, pod)
		current.observeReadyPod(t, pod)
		current.reconcileOK(t)
		current.assertReady(t)
		_, run.expected = current.expectation(t)
		run.route = &unstructured.Unstructured{}
		run.route.SetAPIVersion("networking.istio.io/v1beta1")
		run.route.SetKind("VirtualService")
		if err := h.admin.Get(h.ctx, client.ObjectKey{
			Namespace: run.expected.Namespace.Name, Name: virtualServiceName(current.agent.Name),
		}, run.route); err != nil {
			t.Fatal(err)
		}
	}
	if runs[0].expected.Binding.TeamUID == runs[1].expected.Binding.TeamUID ||
		runs[0].expected.Binding.AgentPolicyUID != runs[1].expected.Binding.AgentPolicyUID {
		t.Fatal("fixture must share a policy across distinct Teams")
	}
	for _, key := range h.gateway.keys {
		key.Spend = ptr.To(1.25)
	}
	gateway := &metadataContinuityGateway{fakeAIGateway: h.gateway}
	h.r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return gateway, nil }
	expectedKeys := make(map[string]*aigateway.Key, len(h.gateway.keys))
	for token, key := range h.gateway.keys {
		expected := *key
		if rateLimits {
			expected.Limits = exampleKeyLimits()
		}
		expectedKeys[token] = &expected
	}
	keysBefore, err := json.Marshal(expectedKeys)
	if err != nil {
		t.Fatal(err)
	}
	generatedBefore := len(h.gateway.generated)
	before := policy.DeepCopy()
	after := policy.DeepCopy()
	after.Labels["example.com/change"] = "metadata"
	if rateLimits {
		after.Spec.ModelAccess.RateLimits = exampleRateLimits()
	}
	ticket, err = store.AcquireConfigurationChange(h.ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err = sandboxcoordination.NewKubernetesChangeAdapter(h.admin, h.admin, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileChange(h.ctx, ticket, adapter); err != nil {
		t.Fatal(err)
	}
	// Reconcile both Agents before either configuration controller can refresh.
	for _, run := range runs {
		current := run.harness
		if _, err := current.reconcile(); err != nil {
			t.Error("execution-neutral policy update could not preserve Agent reconciliation:", err)
		}
		workload := current.workload(t)
		if workload.UID != run.expected.Workload.UID || ptr.Deref(workload.Spec.Replicas, -1) != 1 ||
			current.pod(t).UID != run.expected.PodUID {
			t.Error("execution-neutral policy update stopped or replaced a referencing Agent's compute")
		}
		route := run.route.DeepCopy()
		if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(route), route); err != nil || route.GetUID() != run.route.GetUID() {
			t.Error("execution-neutral policy update removed or replaced a referencing Agent's route:", err)
		}
		if current.current(t).Status.Phase != api.PhaseReady {
			t.Error("execution-neutral policy update interrupted a referencing Agent's readiness")
		}
	}
	keysAfter, err := json.Marshal(h.gateway.keys)
	if err != nil {
		t.Fatal(err)
	}
	if string(keysBefore) != string(keysAfter) || len(h.gateway.generated) != generatedBefore ||
		len(h.gateway.deletedTokens) != 0 || len(h.gateway.deletedAliases) != 0 || gateway.blocks != 0 {
		t.Error("execution-neutral policy update did not preserve key identity/spend with the expected limits")
	}
	currentPolicy := &api.AgentPolicy{}
	if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(policy), currentPolicy); err != nil {
		t.Fatal(err)
	}
	wantGeneration := before.Generation
	if rateLimits {
		wantGeneration++
	}
	if currentPolicy.Generation != wantGeneration || !reflect.DeepEqual(currentPolicy.Status, before.Status) {
		t.Error("policy update did not preserve the original status across the expected generation change")
	}
	if currentPolicy.ResourceVersion == before.ResourceVersion || currentPolicy.Labels["example.com/change"] != "metadata" {
		t.Fatal("metadata update was not persisted")
	}
	changed := currentPolicy.DeepCopy()
	changed.Spec.Security.ReadOnlyRootFilesystem = ptr.To(false)
	if _, err := store.AcquireConfigurationChange(h.ctx, currentPolicy, changed); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("actual policy change bypassed active native reservations:", err)
	}
}

type metadataContinuityGateway struct {
	*fakeAIGateway
	blocks int
}

func (g *metadataContinuityGateway) SetKeyBlocked(ctx context.Context, token string, blocked bool) error {
	if blocked {
		g.blocks++
	}
	return g.fakeAIGateway.SetKeyBlocked(ctx, token, blocked)
}

func TestNativeSandboxSharedPolicyAPIServer(t *testing.T) {
	h := newNativeStartAPIHarness(t, true, true)
	agent := h.agent.DeepCopy()
	agent.ObjectMeta = metav1.ObjectMeta{Name: "second"}
	agent.Status = api.AgentStatus{}
	agent.Spec.User.Alias, agent.Spec.Team, agent.Spec.SandboxRef = agent.Name, "", ""
	if err := h.member.Create(h.ctx, agent); err != nil {
		t.Fatalf("create another member Agent through current single-policy admission: %v", err)
	}
	peer := *h
	peer.nativeSandboxFixture = &nativeSandboxFixture{
		r: h.r, kube: h.kube, gateway: h.gateway, agent: agent,
	}
	type lifetime struct {
		harness        *nativeStartAPIHarness
		state, receipt *corev1.ConfigMap
		expected       *w.Expectation
		pod            *corev1.Pod
	}
	lifetimes := []lifetime{{harness: h}, {harness: &peer}}
	for i := range lifetimes {
		run := &lifetimes[i]
		current := run.harness
		current.seedRetainedBootstrap(t)
		current.create(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Namespace: instanceNamespace(current.agent.Name), Name: "default",
		}})
		current.prepare(t)
		current.reconcileOK(t)
		run.state, run.expected = current.expectation(t)
		current.assertHoldAdmission(t, run.expected.Revision, true)
		var err error
		run.receipt, _, err = current.r.readSandboxPreparationReceipt(h.ctx, current.current(t))
		if err != nil {
			t.Fatal(err)
		}
		run.pod = testfixture.Pod(*run.expected)
		run.pod.Spec.Overhead = nil
		current.create(t, run.pod)
		current.observeReadyPod(t, run.pod)
		current.reconcileOK(t)
		current.assertReady(t)
	}
	first, second := lifetimes[0], lifetimes[1]
	if first.expected.Version != "v1" || second.expected.Version != "v1" ||
		first.expected.Binding.TeamUID != second.expected.Binding.TeamUID ||
		first.expected.Binding.AgentPolicyUID != second.expected.Binding.AgentPolicyUID ||
		first.expected.Namespace.UID == second.expected.Namespace.UID || first.expected.Home.UID == second.expected.Home.UID {
		t.Fatal("single-policy Agents did not retain shared configuration with independent private lifetimes")
	}
	_, binding, err := sandboxbinding.Read(h.ctx, h.member, client.ObjectKeyFromObject(agent))
	if err != nil || binding != second.expected.Binding {
		t.Fatal("served member authorization differs from the started single-policy binding:", err)
	}
	team := &api.Team{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: h.agent.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	before := &api.AgentPolicy{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: team.Spec.AgentPolicyRef}, before); err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Spec.Security.ReadOnlyRootFilesystem = ptr.To(false)
	store := h.r.SandboxNativeCoordination
	if store == nil {
		t.Fatal("rendered configuration did not install native coordination")
	}
	assertChangeBlocked := func() {
		t.Helper()
		if _, err := store.AcquireConfigurationChange(h.ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
			t.Fatalf("active native lifetime did not reserve its selected policy: %v", err)
		}
		if err := h.admin.Update(h.ctx, after.DeepCopy()); err == nil || !strings.Contains(err.Error(), "CoordinationRequired") {
			t.Fatalf("served policy admission accepted an unreserved shared-policy change: %v", err)
		}
	}
	assertChangeBlocked()
	for _, run := range lifetimes {
		ticket, found, err := store.ReadNative(h.ctx, run.expected.Agent.UID)
		if err != nil || !found || ticket.Reservation.Intent.Binding != run.expected.Binding {
			t.Fatal("current native lifetime has no matching durable reservation:", err)
		}
	}
	generatedKeys := len(h.gateway.generated)
	if len(h.gateway.keys) != 2 {
		t.Fatal("shared-policy Agents did not receive independent model keys")
	}
	h.cleanupAgent(t, first.pod, first.state, first.receipt)
	if _, found, err := store.ReadNative(h.ctx, first.expected.Agent.UID); err != nil || found {
		t.Fatal("first Agent cleanup retained its native reservation:", err)
	}
	assertChangeBlocked()
	peer.reconcileOK(t)
	peer.assertReady(t)
	if peer.workload(t).UID != second.expected.Workload.UID ||
		ptr.Deref(peer.workload(t).Spec.Replicas, -1) != 1 || len(h.gateway.keys) != 1 ||
		len(h.gateway.generated) != generatedKeys {
		t.Fatal("first Agent cleanup stopped or recreated the remaining Agent or its model key")
	}

	// Reconstruct the production factories with new starts disabled; cleanup
	// must use the persisted single-policy lifetime, not the old process.
	config := h.sources
	config.NativeExecution = false
	h.r.SandboxPreparation, err = NewKubernetesSandboxPreparation(config, h.kube, h.localHelm, h.r.OperatorNamespace, h.r.ChartVersion)
	if err != nil {
		t.Fatal(err)
	}
	h.r.SandboxNativeCoordination, err = NewNativeSandboxCoordination(config, h.kube, h.kube, h.r.OperatorNamespace)
	if err != nil {
		t.Fatal(err)
	}
	peer.cleanupAgent(t, second.pod, second.state, second.receipt)
	if _, found, err := store.ReadNative(h.ctx, second.expected.Agent.UID); err != nil || found || len(h.gateway.keys) != 0 {
		t.Fatal("restart with native starts disabled lost cleanup of the second lifetime:", err)
	}
	change, err := store.AcquireConfigurationChange(h.ctx, before, after)
	if err != nil {
		t.Fatal("shared policy remained locked after both lifetimes completed:", err)
	}
	adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(h.admin, h.admin, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileChange(h.ctx, change, adapter); err != nil {
		t.Fatal("coordinated policy update did not pass served admission after cleanup:", err)
	}
	current := &api.AgentPolicy{}
	if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(before), current); err != nil ||
		current.UID != before.UID || current.ResourceVersion == before.ResourceVersion ||
		ptr.Deref(current.Spec.Security.ReadOnlyRootFilesystem, true) {
		t.Fatal("completed policy update lost identity or failed to apply its exact change:", err)
	}
}
