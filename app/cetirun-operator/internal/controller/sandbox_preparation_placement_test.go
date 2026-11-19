// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplacement"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func placementPreparationFixture(t *testing.T, typ string) (*preparationFixture, *corev1.Node, *corev1.ConfigMap) {
	t.Helper()
	f := newPreparationFixture(t, typ)
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "reserved-worker"},
		Spec: corev1.NodeSpec{ProviderID: "example://reserved-worker"}}
	if err := f.client.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	registry := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-placement", Namespace: "cetirun-system"}}
	reader, err := sandboxplacement.NewHandler(f.client, sandboxplacement.Config{Namespace: registry.Namespace, ConfigMapName: registry.Name})
	if err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation.Placement = reader
	f.installer.after = func(ctx context.Context) error {
		if registry.UID != "" {
			return nil
		}
		var workloads appsv1.StatefulSetList
		if err := f.client.List(ctx, &workloads, client.InNamespace(f.fixture.Namespace.Name)); err != nil {
			return err
		}
		if len(workloads.Items) != 1 {
			return fmt.Errorf("expected one staged workload before placement enrollment")
		}
		workload := workloads.Items[0]
		raw, err := json.Marshal(map[string]any{
			"version": sandboxplacement.RegistryVersion,
			"nodes": []any{map[string]any{
				"name": node.Name, "uid": node.UID, "providerID": node.Spec.ProviderID,
				"reservation": map[string]any{
					"agent":       w.ObjectIdentity{Name: f.fixture.Agent.Name, UID: f.fixture.Agent.UID},
					"namespace":   w.ObjectIdentity{Name: f.fixture.Namespace.Name, UID: f.fixture.Namespace.UID},
					"statefulSet": w.ObjectIdentity{Name: workload.Name, UID: workload.UID},
				},
			}},
			"platformDaemonSets": []any{},
		})
		if err != nil {
			return err
		}
		registry.Data = map[string]string{sandboxplacement.ConfigMapKey: string(raw)}
		return f.client.Create(ctx, registry)
	}
	return f, node, registry
}

func preparationPlacementReceipt(t *testing.T, f *preparationFixture) (*corev1.ConfigMap, string, *w.PlacementBinding) {
	t.Helper()
	var objects corev1.ConfigMapList
	if err := f.client.List(context.Background(), &objects, client.InNamespace("cetirun-system")); err != nil {
		t.Fatal(err)
	}
	for i := range objects.Items {
		object := &objects.Items[i]
		if raw := object.Data["preparation.json"]; raw != "" {
			var receipt struct {
				Version       string                          `json:"version"`
				Prerequisites controller.SandboxPrerequisites `json:"prerequisites"`
			}
			if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
				t.Fatal(err)
			}
			return object, receipt.Version, receipt.Prerequisites.Placement
		}
	}
	t.Fatal("missing existing preparation receipt")
	return nil, "", nil
}

func TestSandboxPreparationPersistsPlacementBeforeHeldExpectation(t *testing.T) {
	for _, typ := range []string{"hermes", "openclaw"} {
		t.Run(typ, func(t *testing.T) {
			f, node, registry := placementPreparationFixture(t, typ)
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			assertSandboxPrepared(t, f)
			expected := f.expectation(t)
			receipt, version, binding := preparationPlacementReceipt(t, f)
			if version != "v2" || binding == nil || !equality.Semantic.DeepEqual(binding, expected.Placement) ||
				binding.Home != expected.Home || binding.Agent != expected.Agent || binding.Namespace != expected.Namespace ||
				binding.Workload != expected.Workload || binding.Registry != (w.ObjectIdentity{Name: registry.Name, UID: registry.UID}) ||
				len(binding.Nodes) != 1 || binding.Nodes[0].Name != node.Name || binding.Nodes[0].UID != node.UID ||
				binding.Nodes[0].ProviderID != node.Spec.ProviderID {
				t.Fatal("durable placement did not bind the exact prepared identities")
			}
			for _, object := range []*corev1.ConfigMap{receipt, registry} {
				if !slices.Contains(expected.Sources, w.SourceReference{
					APIVersion: "v1", Kind: "ConfigMap", Namespace: object.Namespace, Name: object.Name,
					UID: object.UID, ResourceVersion: object.ResourceVersion,
				}) {
					t.Fatal("held expectation did not pin the registry and durable preparation receipt")
				}
			}
			if !equality.Semantic.DeepEqual(f.source.lastFenced.Placement, binding) {
				t.Fatal("preparation fence did not receive the recorded placement")
			}
			stages, original := f.installer.stages, receipt.Data["preparation.json"]
			for update := range 72 {
				node.Labels = map[string]string{"example.com/heartbeat": fmt.Sprint(update)}
				if err := f.client.Update(context.Background(), node); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(); err != nil || f.installer.stages != stages {
					t.Fatalf("Node metadata churn restaged the held workload: %v", err)
				}
				current, _, _ := preparationPlacementReceipt(t, f)
				if current.Data["preparation.json"] != original || current.ResourceVersion != receipt.ResourceVersion {
					t.Fatal("Node metadata churn rewrote or appended placement history")
				}
			}
		})
	}
}

func TestSandboxPreparationPlacementCannotBeReassignedOrDisabled(t *testing.T) {
	for _, scenario := range []string{"missing registry", "registry replacement", "node replacement", "HOME change", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			f, node, registry := placementPreparationFixture(t, "hermes")
			ctx := context.Background()
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			_, _, original := preparationPlacementReceipt(t, f)
			stages := f.installer.stages
			switch scenario {
			case "missing registry":
				if err := f.client.Delete(ctx, registry); err != nil {
					t.Fatal(err)
				}
			case "registry replacement":
				if err := f.client.Delete(ctx, registry); err != nil {
					t.Fatal(err)
				}
				registry.UID, registry.ResourceVersion = "", ""
				if err := f.client.Create(ctx, registry); err != nil {
					t.Fatal(err)
				}
			case "node replacement":
				if err := f.client.Delete(ctx, node); err != nil {
					t.Fatal(err)
				}
				node.UID, node.ResourceVersion = "", ""
				if err := f.client.Create(ctx, node); err != nil {
					t.Fatal(err)
				}
			case "HOME change":
				f.source.inputs.Home.UID = "another-home-uid"
			case "disabled":
				f.reconciler.SandboxPreparation.Placement = nil
			}
			if err := f.reconcile(); err == nil || f.installer.stages != stages {
				t.Fatal("placement drift or loss silently changed the held preparation")
			}
			_, _, retained := preparationPlacementReceipt(t, f)
			if !equality.Semantic.DeepEqual(original, retained) || !f.expectation(t).ExecutionHeld ||
				!slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
				t.Fatal("placement failure lost history or released execution")
			}
		})
	}
}

func TestSandboxPlacementRegistryRefreshPreservesLifetime(t *testing.T) {
	f, _, registry := placementPreparationFixture(t, "hermes")
	ctx := context.Background()
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	receipt, _, original := preparationPlacementReceipt(t, f)
	registry.Labels = map[string]string{"example.com/metadata": "refreshed"}
	if err := f.client.Update(ctx, registry); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatal("unchanged registry assignment could not refresh its source version:", err)
	}
	current, _, binding := preparationPlacementReceipt(t, f)
	if current.UID != receipt.UID || !equality.Semantic.DeepEqual(binding, original) {
		t.Fatal("registry metadata refresh replaced lifetime ownership")
	}
	if !slices.Contains(f.expectation(t).Sources, w.SourceReference{
		APIVersion: "v1", Kind: "ConfigMap", Namespace: registry.Namespace, Name: registry.Name,
		UID: registry.UID, ResourceVersion: registry.ResourceVersion,
	}) {
		t.Fatal("expectation retained the previous registry source version")
	}
}

func TestSandboxPlacementStillRequiresQualifiedWriterExclusion(t *testing.T) {
	f, _, _ := placementPreparationFixture(t, "hermes")
	installKubernetesPrerequisites(t, f, nil)
	if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) || f.installer.stages != 0 {
		t.Fatal("selecting placement was mistaken for a qualified writer-exclusion source:", err)
	}
	var state corev1.ConfigMapList
	if err := f.client.List(context.Background(), &state, client.InNamespace("cetirun-system")); err != nil {
		t.Fatal(err)
	}
	for _, object := range state.Items {
		if object.Data["preparation.json"] != "" || object.Data[w.PodExpectationKey] != "" {
			t.Fatal("unqualified placement selection created durable execution prerequisites")
		}
	}
}

func TestSandboxPlacementReceiptCommitPrecedesExpectationAndRecovers(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost-response=%v", committed), func(t *testing.T) {
			f, _, _ := placementPreparationFixture(t, "hermes")
			intercepted := false
			f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					cm, ok := object.(*corev1.ConfigMap)
					if !ok || intercepted || cm.Data["preparation.json"] == "" {
						return c.Patch(ctx, object, patch, options...)
					}
					var receipt struct {
						Version string `json:"version"`
					}
					if err := json.Unmarshal([]byte(cm.Data["preparation.json"]), &receipt); err != nil {
						return err
					}
					if receipt.Version != "v2" {
						return c.Patch(ctx, object, patch, options...)
					}
					intercepted = true
					if committed {
						if err := c.Patch(ctx, object, patch, options...); err != nil {
							return err
						}
					}
					return fmt.Errorf("synthetic placement receipt write failure")
				},
			})
			if err := f.reconcile(); err == nil || !intercepted {
				t.Fatal("receipt write uncertainty was ignored")
			}
			key := client.ObjectKey{Namespace: "cetirun-system", Name: w.PodExpectationName(f.fixture.Namespace.Name, f.fixture.Namespace.UID)}
			if err := f.client.Get(context.Background(), key, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
				t.Fatal("expectation was published before placement durability:", err)
			}
			_, version, recorded := preparationPlacementReceipt(t, f)
			if committed != (version == "v2" && recorded != nil) {
				t.Fatal("receipt fixture did not model the intended commit outcome")
			}
			f.reconciler.Client = f.client
			reader, err := sandboxplacement.NewHandler(f.client, sandboxplacement.Config{Namespace: "cetirun-system", ConfigMapName: "sandbox-placement"})
			if err != nil {
				t.Fatal(err)
			}
			f.reconciler.SandboxPreparation.Placement = reader
			if err := f.reconcile(); err != nil {
				t.Fatal("restart could not recover recorded placement:", err)
			}
			assertSandboxPrepared(t, f)
			if committed && !equality.Semantic.DeepEqual(recorded, f.expectation(t).Placement) {
				t.Fatal("recovery replaced the durably committed placement")
			}
		})
	}
}

func TestSandboxPlacementIncompleteV2ReceiptDoesNotBecomeUnassigned(t *testing.T) {
	f, _, _ := placementPreparationFixture(t, "hermes")
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	receipt, _, _ := preparationPlacementReceipt(t, f)
	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(receipt.Data["preparation.json"]), &value); err != nil {
		t.Fatal(err)
	}
	var prerequisites controller.SandboxPrerequisites
	if err := json.Unmarshal(value["prerequisites"], &prerequisites); err != nil {
		t.Fatal(err)
	}
	prerequisites.Placement = nil
	raw, err := json.Marshal(prerequisites)
	if err != nil {
		t.Fatal(err)
	}
	value["prerequisites"] = raw
	raw, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Data["preparation.json"] = string(raw)
	if err := f.client.Update(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	fences := f.source.fences
	if err := f.reconcile(); err == nil || f.source.fences != fences {
		t.Fatal("lost placement was treated as an unassigned lifetime")
	}
}

func TestSandboxPlacementCleanupUsesHistoryWithoutCurrentRegistry(t *testing.T) {
	f, node, registry := placementPreparationFixture(t, "hermes")
	ctx := context.Background()
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	_, _, original := preparationPlacementReceipt(t, f)
	for _, object := range []client.Object{registry, node} {
		if err := f.client.Delete(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	f.source.resolveErr = fmt.Errorf("current configuration is unavailable")
	resolves := f.source.resolves
	f.reconciler.SandboxPreparation.Placement = nil
	agent := f.current(t)
	if err := f.client.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.source.fenceErr = fmt.Errorf("synthetic fence remains unconfirmed")
	if err := f.reconcile(); err == nil || !equality.Semantic.DeepEqual(original, f.source.lastFenced.Placement) ||
		f.source.resolves != resolves {
		t.Fatal("cleanup lost the historical tuple or depended on current placement discovery")
	}
	if err := f.client.Get(ctx, client.ObjectKey{Name: f.fixture.Namespace.Name}, &corev1.Namespace{}); err != nil {
		t.Fatal("namespace removed before the historical fence was confirmed:", err)
	}
	f.source.fenceErr = nil
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Get(ctx, client.ObjectKey{Name: agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatal("qualified synthetic cleanup did not finish:", err)
	}
}
