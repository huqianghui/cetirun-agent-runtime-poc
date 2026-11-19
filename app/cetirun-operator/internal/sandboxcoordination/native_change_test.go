// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"errors"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func nativeConfigurationIntent(t *testing.T, before client.Object) NativeIntent {
	t.Helper()
	intent := testNativeIntent("one")
	for i := range intent.Sources {
		reference := &intent.Sources[i]
		if reference.Name != before.GetName() || reference.UID != before.GetUID() {
			continue
		}
		reference.ResourceVersion = before.GetResourceVersion()
		value, err := w.ExecutionSourceDigest(before, intent.SelectedSandbox())
		if err != nil {
			t.Fatal(err)
		}
		intent.SourceDigests[w.SourceKey(*reference)] = value
	}
	return intent
}

func TestNativeConfigurationWriterAndDeletionRespectReservations(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, after := configurationFixture(t, kind)
			if team, ok := after.(*api.Team); ok {
				team.Spec.AgentPolicyRef = "replacement-policy"
			}
			ticket := acquireNative(t, store, nativeConfigurationIntent(t, before))
			for _, stopping := range []bool{false, true} {
				if stopping {
					var err error
					ticket, err = store.BeginNativeStop(ctx, ticket)
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, ErrConflict) {
					t.Fatal("native selection allowed a participating configuration update:", err)
				}
				if _, err := store.AcquireConfigurationDeletion(ctx, before); !errors.Is(err, ErrConflict) {
					t.Fatal("native selection allowed a participating configuration delete:", err)
				}
				readOnly := store
				readOnly.Client = nil
				if err := readOnly.AuthorizeConfigurationUpdate(ctx, before, after); !errors.Is(err, ErrConflict) {
					t.Fatal("configuration admission bypassed the native reservation:", err)
				}
			}
			if err := store.ReleaseNative(ctx, ticket); err != nil {
				t.Fatal(err)
			}
			change, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal("released native selection blocked the writer:", err)
			}
			if _, err := store.ReconcileChange(ctx, change, configurationAdapter(t, storage, storage, before, after)); err != nil {
				t.Fatal("ordinary participating update failed after release:", err)
			}
		})
	}
}

func TestNativeTeamChangesRespectSelectedSlots(t *testing.T) {
	for _, scenario := range []string{"creation default", "unused sandbox membership", "smart routing", "policy", "selected sandbox membership", "shared context"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before := teamChangeFixture(t)
			intent := nativeConfigurationIntent(t, before)
			acquireNative(t, store, intent)
			after := before.DeepCopy()
			allowed := true
			switch scenario {
			case "creation default":
				after.Spec.DefaultSandboxRef = "spare"
			case "unused sandbox membership":
				after.Spec.SandboxRefs = append(after.Spec.SandboxRefs, "another-spare")
			case "smart routing":
				after.Spec.ModelRouters = []api.TeamModelRouter{{Name: "auto-team",
					Classifier: api.TeamModelRouterClassifier{Type: api.ModelRouterClassifierHeuristic},
					Tiers:      api.TeamModelRouterTiers{Simple: "small", Medium: "large", Complex: "large"}}}
			case "policy":
				after.Spec.AgentPolicyRef, allowed = "replacement-policy", false
			case "selected sandbox membership":
				after.Spec.SandboxRefs, after.Spec.DefaultSandboxRef, allowed = []string{"spare"}, "spare", false
			case "shared context":
				after.Spec.MemberContext.Role, allowed = "changed-context", false
			}
			change, err := store.AcquireConfigurationChange(ctx, before, after)
			if !allowed {
				if !errors.Is(err, ErrConflict) {
					t.Fatal("affected Team change did not conflict:", err)
				}
				return
			}
			if err != nil {
				t.Fatal("unused Team input was blocked:", err)
			}
			second := nativeConfigurationIntent(t, before)
			other := testNativeIntent("other")
			second.Agent, second.Namespace, second.Home, second.Workload = other.Agent, other.Namespace, other.Home, other.Workload
			second.Binding.AgentUID = other.Agent.UID
			acquireNative(t, store, second)
			if _, err := store.ReconcileChange(ctx, change, configurationAdapter(t, storage, storage, before, after)); err != nil {
				t.Fatal(err)
			}
			if err := store.AuthorizeNativeStart(ctx, intent); err != nil {
				t.Fatal("unrelated Team mutation invalidated the native reservation:", err)
			}
		})
	}
}

func TestNativeMetadataOnlyConfigurationChangesRemainAllowed(t *testing.T) {
	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, _ := configurationFixture(t, kind)
			intent := nativeConfigurationIntent(t, before)
			acquireNative(t, store, intent)
			after := before.DeepCopyObject().(client.Object)
			after.SetAnnotations(map[string]string{"example.com/description": "metadata-only"})
			change, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal("unchanged effective native configuration was blocked:", err)
			}
			if _, err := store.ReconcileChange(ctx, change, configurationAdapter(t, storage, storage, before, after)); err != nil {
				t.Fatal(err)
			}
			if err := store.AuthorizeNativeStart(ctx, intent); err != nil {
				t.Fatal("metadata-only configuration invalidated a native reservation:", err)
			}
		})
	}
}

func TestNativePolicyRateLimitChangesPreserveReservations(t *testing.T) {
	for _, reconciled := range []bool{false, true} {
		name := "single-start"
		if reconciled {
			name = "reconciled"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, storage, object, _ := configurationFixture(t, "AgentPolicy")
			before := object.(*api.AgentPolicy)
			intent := nativeConfigurationIntent(t, before)
			if reconciled {
				intent.Reconciled, intent.Workload.UID = true, ""
			}
			ticket := acquireNative(t, store, intent)
			for _, limits := range []*api.AgentPolicyRateLimits{
				{MaxParallelRequests: ptr.To[int64](4), RPM: ptr.To[int64](60), TPM: ptr.To[int64](200000)},
				{RPM: ptr.To[int64](30), PerModel: map[string]api.AgentPolicyModelRateLimit{
					"example-model": {RPM: ptr.To[int64](10), TPM: ptr.To[int64](8192)},
				}},
				{PerModel: map[string]api.AgentPolicyModelRateLimit{"auto": {RPM: ptr.To[int64](20)}}},
				nil,
			} {
				after := before.DeepCopy()
				after.Spec.ModelAccess.RateLimits = limits
				change, err := store.AcquireConfigurationChange(ctx, before, after)
				if err != nil {
					t.Fatal("Gateway-only policy update was blocked by a native reservation:", err)
				}
				tampered := after.DeepCopy()
				tampered.Spec.ModelAccess.RateLimits = &api.AgentPolicyRateLimits{RPM: ptr.To[int64](999)}
				if _, err := store.ReadConfigurationChange(ctx, before, tampered); !errors.Is(err, ErrConflict) {
					t.Fatal("execution-neutral limits were not pinned by the exact mutation reservation:", err)
				}
				if _, err := store.ReconcileChange(ctx, change, configurationAdapter(t, storage, storage, before, after)); err != nil {
					t.Fatal("coordinated rate-limit update failed:", err)
				}
				if err := storage.Get(ctx, client.ObjectKeyFromObject(before), before); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before.Spec.ModelAccess.RateLimits, limits) {
					t.Fatal("coordinated update did not persist the requested limits")
				}
				if err := store.AuthorizeNativeStart(ctx, intent); err != nil {
					t.Fatal("rate-limit update invalidated the native reservation:", err)
				}
				current, found, err := store.ReadNative(ctx, intent.Agent.UID)
				if err != nil || !found || !reflect.DeepEqual(current, ticket) {
					t.Fatal("rate-limit update replaced or changed the running reservation:", err)
				}
				digest, err := w.ExecutionSourceDigest(before, intent.SelectedSandbox())
				if err != nil || digest != intent.SourceDigests[w.SourceKey(intent.Sources[2])] {
					t.Fatal("persisted rate limits invalidated native execution inputs:", err)
				}
			}
			after := before.DeepCopy()
			after.Spec.ModelAccess.RateLimits = &api.AgentPolicyRateLimits{RPM: ptr.To[int64](60)}
			after.Spec.ModelAccess.GatewayBudget = &api.AgentPolicyGatewayBudget{DefaultMonthlyUSD: "10", MaxMonthlyUSD: "20"}
			if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, ErrConflict) {
				t.Fatal("a rate-limit edit bypassed the reservation for a simultaneous budget change:", err)
			}
		})
	}
}

func TestNativeReservationBlocksSelectedConfigurationFinalization(t *testing.T) {
	ctx := context.Background()
	store, storage, before, _ := configurationFixture(t, "Sandbox")
	before.SetFinalizers([]string{"example.com/protect"})
	if err := storage.Update(ctx, before); err != nil {
		t.Fatal(err)
	}
	ticket := acquireNative(t, store, nativeConfigurationIntent(t, before))
	// Model an already-terminating source. Finalization must still not discard
	// a source selected by a native lifetime, even when it is stopping.
	if err := storage.Delete(ctx, before); err != nil {
		t.Fatal(err)
	}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), before); err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopyObject().(client.Object)
	after.SetFinalizers(nil)
	readOnly := store
	readOnly.Client = nil
	if err := readOnly.AuthorizeConfigurationFinalization(ctx, before, after); !errors.Is(err, ErrConflict) {
		t.Fatal("native reservation did not block source finalization:", err)
	}
	ticket, err := store.BeginNativeStop(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseNative(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if err := readOnly.AuthorizeConfigurationFinalization(ctx, before, after); err != nil {
		t.Fatal("released native reservation prevented independent finalization:", err)
	}
}
