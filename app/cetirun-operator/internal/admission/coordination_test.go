// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func coordinatedFixture(t *testing.T) (fixture, sandboxcoordination.Store) {
	t.Helper()
	f := newFixture(t)
	if err := corev1.AddToScheme(f.client.Scheme()); err != nil {
		t.Fatal(err)
	}
	state, err := sandboxcoordination.NewState("operator-test", "coordination")
	if err != nil {
		t.Fatal(err)
	}
	state.UID = "coordination-uid"
	if err := f.client.Create(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	config := testConfig()
	config.CoordinationConfigMapName = state.Name
	config.CoordinationConfigMapUID = string(state.UID)
	handler, err := NewHandler(f.client, config)
	if err != nil {
		t.Fatal(err)
	}
	handler.registrations = f.handler.registrations
	f.handler = handler
	return f, sandboxcoordination.Store{
		Client: f.client, APIReader: f.client, Namespace: state.Namespace,
		Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID},
	}
}

func TestConfigurationCoordinationUsesExactSubmittedChange(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, store := coordinatedFixture(t)
			var before, after client.Object
			switch kind {
			case "Team":
				before, after = f.team, f.team.DeepCopy()
			case "Sandbox":
				before, after = f.box, f.box.DeepCopy()
			case "AgentPolicy":
				before, after = f.policy, f.policy.DeepCopy()
			}
			after.SetAnnotations(map[string]string{"example.com/description": "updated"})
			request := requestFor(t, admissionv1.Update, after, before, "administrator")
			requireResponse(t, f.handler.Validate(ctx, request), false, "CoordinationRequired")

			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			requireResponse(t, f.handler.Validate(ctx, request), false, "CoordinationRequired")
			ticket, err = store.BeginChange(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			requireResponse(t, f.handler.Validate(ctx, request), true, "")

			tampered := after.DeepCopyObject().(client.Object)
			tampered.SetAnnotations(map[string]string{"example.com/description": "different"})
			requireResponse(t, f.handler.Validate(ctx,
				requestFor(t, admissionv1.Update, tampered, before, "administrator")), false, "CoordinationRequired")
			if err := store.CheckChange(ctx, ticket); err != nil {
				t.Fatal("admission changed the pending reservation:", err)
			}
		})
	}
}

func TestConfigurationCoordinationRetainsNormalAuthorization(t *testing.T) {
	ctx := context.Background()
	f, store := coordinatedFixture(t)
	after := f.policy.DeepCopy()
	after.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
	ticket, err := store.AcquireConfigurationChange(ctx, f.policy, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginChange(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	for _, caller := range []string{"member", testController, "system:anonymous"} {
		requireResponse(t, f.handler.Validate(ctx,
			requestFor(t, admissionv1.Update, after, f.policy, caller)), false, "administrator")
	}
	requireResponse(t, f.handler.Validate(ctx,
		requestFor(t, admissionv1.Update, after, f.policy, "administrator")), true, "")
}

func TestConfigurationCoordinationAllowsLegacyTeamTransitions(t *testing.T) {
	for _, transition := range []struct {
		name                          string
		beforeGoverned, afterGoverned bool
	}{
		{"legacy update", false, false},
		{"add governance", false, true},
		{"remove governance", true, false},
	} {
		t.Run(transition.name, func(t *testing.T) {
			ctx := context.Background()
			f, store := coordinatedFixture(t)
			before := f.team.DeepCopy()
			governed := *before.Spec.DeepCopy()
			if !transition.beforeGoverned {
				before.Spec = api.TeamSpec{}
				if err := f.client.Update(ctx, before); err != nil {
					t.Fatal(err)
				}
			}
			after := before.DeepCopy()
			if transition.afterGoverned {
				after.Spec = governed
			} else {
				after.Spec = api.TeamSpec{}
			}
			after.Annotations = map[string]string{"example.com/change": "legacy-transition"}
			request := requestFor(t, admissionv1.Update, after, before, "administrator")
			requireResponse(t, f.handler.Validate(ctx, request), false, "CoordinationRequired")
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.BeginChange(ctx, ticket); err != nil {
				t.Fatal(err)
			}
			requireResponse(t, f.handler.Validate(ctx, request), true, "")
		})
	}
}

func TestConfigurationCoordinationIsReadOnlyForDryRun(t *testing.T) {
	ctx := context.Background()
	f, store := coordinatedFixture(t)
	after := f.policy.DeepCopy()
	after.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
	ticket, err := store.AcquireConfigurationChange(ctx, f.policy, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginChange(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
	beforeState := &corev1.ConfigMap{}
	if err := f.client.Get(ctx, key, beforeState); err != nil {
		t.Fatal(err)
	}
	if f.handler.coordination.Client != nil {
		t.Fatal("admission was configured with a coordination writer")
	}
	request := requestFor(t, admissionv1.Update, after, f.policy, "administrator")
	request.DryRun = ptr.To(true)
	requireResponse(t, f.handler.Validate(ctx, request), true, "")
	current := &corev1.ConfigMap{}
	if err := f.client.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeState, current) {
		t.Fatal("dry-run admission mutated authority")
	}
}

func TestConfigurationCoordinationMissingAuthorityFailsUpdatesNotCleanup(t *testing.T) {
	ctx := context.Background()
	f, store := coordinatedFixture(t)
	f.handler.coordination.Identity.UID = "replaced-authority"
	after := f.policy.DeepCopy()
	after.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
	requireResponse(t, f.handler.Validate(ctx,
		requestFor(t, admissionv1.Update, after, f.policy, "administrator")), false, "")
	status := requestFor(t, admissionv1.Update, f.agent, f.agent.DeepCopy(), testController)
	status.SubResource = "status"
	requireResponse(t, f.handler.Validate(ctx, status), true, "")
	requireResponse(t, f.handler.Validate(ctx,
		requestFor(t, admissionv1.Delete, f.agent, nil, "administrator")), true, "")
	unreferenced := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "unused-policy"}}
	requireResponse(t, f.handler.Validate(ctx,
		requestFor(t, admissionv1.Delete, unreferenced, nil, "administrator")), false, "CoordinationRequired")
	deleting := f.policy.DeepCopy()
	deleting.DeletionTimestamp = ptr.To(metav1.Now())
	deleting.Finalizers = []string{metav1.FinalizerDeleteDependents, "example.com/retain"}
	cleaned := deleting.DeepCopy()
	cleaned.Finalizers = []string{"example.com/retain"}
	requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Update, cleaned, deleting,
		"system:serviceaccount:kube-system:generic-garbage-collector")), true, "")
	cleaned.Spec.Network.BlockedCIDRs = []string{"198.51.100.0/24"}
	requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Update, cleaned, deleting,
		"system:serviceaccount:kube-system:generic-garbage-collector")), false, "")
	if _, err := store.ReadExecution(ctx, "not-enrolled"); err == nil {
		t.Fatal("fixture unexpectedly created execution state")
	}
}

func TestConfigurationCoordinationDefaultsRemainDisabled(t *testing.T) {
	f := newFixture(t)
	after := f.policy.DeepCopy()
	after.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
	if f.handler.coordination != nil {
		t.Fatal("default configuration installed a coordination gate")
	}
	requireResponse(t, f.handler.Validate(context.Background(),
		requestFor(t, admissionv1.Update, after, f.policy, "administrator")), true, "")
}

func TestConfigurationCoordinationFinalizerCleanup(t *testing.T) {
	for _, scenario := range []string{"remove one", "remove all", "dry run", "unchanged", "add finalizer", "spec", "label", "member", "pending change", "missing authority"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f, store := coordinatedFixture(t)
			policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{
				Name: "cleanup-policy", UID: "cleanup-policy-uid",
				Finalizers: []string{"example.com/cleanup", "example.com/retain"},
			}, Spec: api.AgentPolicySpec{Harnesses: api.AgentPolicyHarnesses{
				Allowed: []api.AgentType{api.AgentTypeOpenClaw},
			}}}
			if err := f.client.Create(ctx, policy); err != nil {
				t.Fatal(err)
			}
			if scenario == "pending change" {
				proposed := policy.DeepCopy()
				proposed.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
				if _, err := store.AcquireConfigurationChange(ctx, policy, proposed); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.client.Delete(ctx, policy); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			after := policy.DeepCopy()
			after.Finalizers = []string{"example.com/retain"}
			caller := "administrator"
			switch scenario {
			case "remove all":
				after.Finalizers = nil
			case "unchanged":
				after.Finalizers = policy.Finalizers
			case "add finalizer":
				after.Finalizers = []string{"example.com/new"}
			case "spec":
				after.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
			case "label":
				after.Labels = map[string]string{"example.com/changed": "true"}
			case "member":
				caller = "member"
			case "missing authority":
				f.handler.coordination.Identity.UID = "different-authority"
			}
			request := requestFor(t, admissionv1.Update, after, policy, caller)
			request.DryRun = ptr.To(scenario == "dry run")
			key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
			beforeState := &corev1.ConfigMap{}
			if err := f.client.Get(ctx, key, beforeState); err != nil {
				t.Fatal(err)
			}
			allowed := scenario == "remove one" || scenario == "remove all" || scenario == "dry run"
			requireResponse(t, f.handler.Validate(ctx, request), allowed, "")
			currentState := &corev1.ConfigMap{}
			if err := f.client.Get(ctx, key, currentState); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeState, currentState) {
				t.Fatal("finalizer admission modified coordination state")
			}
		})
	}
}

func TestConfigurationCoordinationRequiresStrictPinnedAuthority(t *testing.T) {
	for _, test := range []struct {
		name, uid, mode string
		valid           bool
	}{
		{"", "", ValidationCompatible, true},
		{"coordination", "authority-uid", ValidationStrict, true},
		{"coordination", "", ValidationStrict, false},
		{"", "authority-uid", ValidationStrict, false},
		{"invalid/name", "authority-uid", ValidationStrict, false},
		{"coordination", "authority uid", ValidationStrict, false},
		{"coordination", "authority\nuid", ValidationStrict, false},
		{"coordination", strings.Repeat("x", 129), ValidationStrict, false},
		{"coordination", "authority-uid", ValidationCompatible, false},
		{"coordination", "authority-uid", "", false},
	} {
		config := testConfig()
		config.CoordinationConfigMapName, config.CoordinationConfigMapUID = test.name, test.uid
		config.ValidationMode = test.mode
		if err := config.Validate(); (err == nil) != test.valid {
			t.Errorf("coordination name=%q mode=%q valid=%v: %v", test.name, test.mode, test.valid, err)
		}
	}
	for _, source := range []string{"platform", "placement"} {
		config := testConfig()
		config.CoordinationConfigMapName, config.CoordinationConfigMapUID = "coordination", "authority-uid"
		if source == "platform" {
			config.PlatformNamespace, config.PlatformConfigMapName = "operator-test", "coordination"
		} else {
			config.PlacementConfigMapName = "coordination"
		}
		if err := config.Validate(); err == nil {
			t.Fatalf("%s source accepted as coordination authority", source)
		}
	}
}
