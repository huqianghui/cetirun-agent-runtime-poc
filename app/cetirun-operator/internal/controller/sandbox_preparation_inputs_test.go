// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func preparedRevision(t *testing.T, f *preparationFixture) *appsv1.ControllerRevision {
	t.Helper()
	ctx := context.Background()
	expected := f.expectation(t)
	workload := &appsv1.StatefulSet{}
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}, workload); err != nil {
		t.Fatal(err)
	}
	template, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&workload.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	template["$patch"] = "replace"
	raw, err := json.Marshal(map[string]any{"spec": map[string]any{"template": template}})
	if err != nil {
		t.Fatal(err)
	}
	revision := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workload.Namespace, Name: workload.Name + "-revision",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))},
		},
		Data: runtime.RawExtension{Raw: raw}, Revision: 1,
	}
	if err := f.client.Create(ctx, revision); err != nil {
		t.Fatal(err)
	}
	workload.Status.UpdateRevision, workload.Status.ObservedGeneration = revision.Name, workload.Generation
	if err := f.client.Status().Update(ctx, workload); err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestSandboxPreparationStartInputPins(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit-image=%v", typ, explicit), func(t *testing.T) {
				f := newPreparationFixture(t, typ)
				f.reconciler.ChartRegistry = "registry.example.com"
				if explicit {
					agent := f.current(t)
					image := "registry.example.com/" + typ + "-gateway:" + f.installer.chart.Metadata.AppVersion
					agent.Spec.Agent.Image = &image
					agent.Generation++
					if err := f.client.Update(context.Background(), agent); err != nil {
						t.Fatal(err)
					}
					f.source.inputs.AgentGeneration = agent.Generation
				}
				if err := f.reconcile(); err != nil {
					t.Fatal(err)
				}
				assertSandboxPrepared(t, f)
				expected := f.expectation(t)
				journal := &corev1.ConfigMap{}
				if err := f.client.Get(context.Background(), client.ObjectKey{
					Namespace: "cetirun-system", Name: sandboxpolicy.GrantJournalName(f.fixture.Agent.UID),
				}, journal); err != nil {
					t.Fatal(err)
				}
				if expected.Version != "v1" || expected.ControllerRevision != "" || !slices.Contains(expected.Sources, w.SourceReference{
					APIVersion: "v1", Kind: "ConfigMap", Namespace: journal.Namespace, Name: journal.Name,
					UID: journal.UID, ResourceVersion: journal.ResourceVersion,
				}) {
					t.Fatal("held expectation did not retain its format and exact validated grant-journal identity")
				}
				state := &corev1.ConfigMap{}
				if err := f.client.Get(context.Background(), client.ObjectKey{
					Namespace: "cetirun-system", Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID),
				}, state); err != nil {
					t.Fatal(err)
				}
				expected.Sources = slices.DeleteFunc(expected.Sources, func(source w.SourceReference) bool {
					return source.Kind == "ConfigMap" && source.Namespace == journal.Namespace && source.Name == journal.Name
				})
				raw, err := json.Marshal(expected)
				if err != nil {
					t.Fatal(err)
				}
				state.Data[w.PodExpectationKey] = string(raw)
				if err := f.client.Update(context.Background(), state); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(); err != nil {
					t.Fatal("older held expectation without new pins was not accepted for guarded rebuilding:", err)
				}
				revision := preparedRevision(t, f)
				if err := f.reconcile(); err != nil {
					t.Fatal(err)
				}
				expected = f.expectation(t)
				if expected.ControllerRevision != revision.Name || !slices.Contains(expected.Sources, w.SourceReference{
					APIVersion: "apps/v1", Kind: "ControllerRevision", Namespace: revision.Namespace, Name: revision.Name,
					UID: revision.UID, ResourceVersion: revision.ResourceVersion,
				}) {
					t.Fatal("the current controller revision was not pinned after validating its actual template")
				}
				revision.Labels = map[string]string{"example.com/refreshed": "true"}
				if err := f.client.Update(context.Background(), revision); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(); err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(f.expectation(t).Sources, w.SourceReference{
					APIVersion: "apps/v1", Kind: "ControllerRevision", Namespace: revision.Namespace, Name: revision.Name,
					UID: revision.UID, ResourceVersion: revision.ResourceVersion,
				}) {
					t.Fatal("revision metadata change reused a stale source version")
				}
				assertSandboxPrepared(t, f)
			})
		}
	}
}

func TestSandboxPreparationRejectsUntrustedRevision(t *testing.T) {
	for _, scenario := range []string{"foreign-owner", "different-template", "missing", "unobserved-generation"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPreparationFixture(t, "hermes")
			ctx := context.Background()
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			revision := preparedRevision(t, f)
			switch scenario {
			case "foreign-owner":
				revision.OwnerReferences[0].UID = "foreign-workload"
			case "different-template":
				revision.Data.Raw = []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"cetirun","image":"registry.example.com/foreign:latest"}]}}}}`)
			case "missing":
				if err := f.client.Delete(ctx, revision); err != nil {
					t.Fatal(err)
				}
			case "unobserved-generation":
				expected := f.expectation(t)
				workload := &appsv1.StatefulSet{}
				if err := f.client.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}, workload); err != nil {
					t.Fatal(err)
				}
				workload.Status.ObservedGeneration = workload.Generation - 1
				if err := f.client.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "foreign-owner" || scenario == "different-template" {
				if err := f.client.Update(ctx, revision); err != nil {
					t.Fatal(err)
				}
			}
			err := f.reconcile()
			if scenario == "unobserved-generation" {
				if err != nil || f.expectation(t).ControllerRevision != "" {
					t.Fatal("an unobserved workload generation acquired a current revision pin:", err)
				}
				assertSandboxPrepared(t, f)
			} else if err == nil || apimeta.IsStatusConditionTrue(f.current(t).Status.Conditions, api.ConditionAgentConfigReady) {
				t.Fatal("untrusted revision looked like successfully prepared inputs:", err)
			}
			if !f.expectation(t).ExecutionHeld || !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
				t.Fatal("revision validation lifted execution holds")
			}
		})
	}
}

func TestSandboxPreparationMetadataChurnKeepsBoundedStateAndGrants(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			f := newPreparationFixture(t, typ)
			ctx := context.Background()
			store, grants := prepareWorkloadGrants(t, f)
			revision := preparedRevision(t, f)
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			expected := f.expectation(t)
			stateKey := client.ObjectKey{Namespace: "cetirun-system", Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}
			state := &corev1.ConfigMap{}
			if err := f.client.Get(ctx, stateKey, state); err != nil {
				t.Fatal(err)
			}
			stateUID, previousRV := state.UID, state.ResourceVersion
			journal, err := store.ReadSnapshot(ctx, f.fixture.Agent.Name, f.fixture.Agent.UID)
			if err != nil {
				t.Fatal(err)
			}
			var before corev1.ConfigMapList
			if err := f.client.List(ctx, &before, client.InNamespace("cetirun-system")); err != nil {
				t.Fatal(err)
			}
			revocations := 0
			f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					switch object.(type) {
					case *rbacv1.RoleBinding, *rbacv1.ClusterRoleBinding:
						revocations++
					}
					return c.Delete(ctx, object, opts...)
				},
			})
			for update := range 72 {
				revision.Labels = map[string]string{"example.com/metadata-observation": fmt.Sprint(update)}
				if err := f.client.Update(ctx, revision); err != nil {
					t.Fatal(err)
				}
				workload := &appsv1.StatefulSet{}
				if err := f.client.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}, workload); err != nil {
					t.Fatal(err)
				}
				workload.Status.Conditions = []appsv1.StatefulSetCondition{{
					Type: "SyntheticObservation", Status: corev1.ConditionUnknown,
					Reason: "TestObservation", Message: fmt.Sprint(update),
				}}
				if err := f.client.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(); err != nil {
					t.Fatalf("metadata/status update %d failed held preparation: %v", update+1, err)
				}
				current := f.expectation(t)
				if !current.ExecutionHeld || current.Workload != expected.Workload || current.Binding != expected.Binding ||
					current.Revision != expected.Revision || current.ControllerRevision != revision.Name ||
					len(current.Sources) != len(expected.Sources) ||
					!slices.Contains(current.Sources, w.SourceReference{
						APIVersion: "apps/v1", Kind: "ControllerRevision", Namespace: revision.Namespace, Name: revision.Name,
						UID: revision.UID, ResourceVersion: revision.ResourceVersion,
					}) || !apimeta.IsStatusConditionTrue(f.current(t).Status.Conditions, api.ConditionAgentConfigReady) {
					t.Fatalf("metadata/status update %d lost logical inputs or retained stale observation versions", update+1)
				}
				if err := f.client.Get(ctx, stateKey, state); err != nil {
					t.Fatal(err)
				}
				if state.UID != stateUID || state.ResourceVersion == previousRV || len(state.Data) != 1 ||
					len(state.Data[w.PodExpectationKey]) > 512*1024 {
					t.Fatal("metadata refresh replaced or grew the single held expectation instead of CAS-refreshing it")
				}
				previousRV = state.ResourceVersion
				currentJournal, err := store.ReadSnapshot(ctx, f.fixture.Agent.Name, f.fixture.Agent.UID)
				if err != nil || currentJournal.UID != journal.UID || currentJournal.ResourceVersion != journal.ResourceVersion ||
					!slices.Equal(currentJournal.Bindings, grants) || revocations != 0 {
					t.Fatalf("metadata/status update %d revoked or changed prior grant obligations: %v", update+1, err)
				}
			}
			var after corev1.ConfigMapList
			if err := f.client.List(ctx, &after, client.InNamespace("cetirun-system")); err != nil {
				t.Fatal(err)
			}
			if len(after.Items) != len(before.Items) {
				t.Fatal("metadata refresh created per-observation state objects")
			}
			for _, object := range after.Items {
				if raw := object.Data["preparation.json"]; raw != "" {
					var receipt map[string]any
					if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
						t.Fatal(err)
					}
					if receipt["version"] != "v1" || len(receipt) != 4 || receipt["executions"] != nil || len(raw) > 64*1024 {
						t.Fatal("metadata refresh appended permanent history or changed the cleanup receipt")
					}
				}
			}
		})
	}
}

func TestSandboxPreparationKeepsV1ReceiptAndRejectsUnsupportedFormats(t *testing.T) {
	f := newPreparationFixture(t, "hermes")
	ctx := context.Background()
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	var objects corev1.ConfigMapList
	if err := f.client.List(ctx, &objects, client.InNamespace("cetirun-system")); err != nil {
		t.Fatal(err)
	}
	var receipt *corev1.ConfigMap
	for i := range objects.Items {
		if objects.Items[i].Data["preparation.json"] != "" {
			receipt = &objects.Items[i]
		}
	}
	if receipt == nil {
		t.Fatal("missing existing cleanup receipt")
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(receipt.Data["preparation.json"]), &value); err != nil {
		t.Fatal(err)
	}
	if value["version"] != "v1" || len(value) != 4 || value["executions"] != nil {
		t.Fatal("pre-start input pins changed the cleanup format or added attempt history")
	}
	value["version"] = "v2"
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Data["preparation.json"] = string(raw)
	if err := f.client.Update(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("unsupported receipt version was silently adopted")
	}
	if err := f.client.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("cleanup ignored an unsupported receipt")
	}
	namespace := &corev1.Namespace{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil || !namespace.DeletionTimestamp.IsZero() {
		t.Fatal("unsupported receipt bypassed existing cleanup obligations:", err)
	}
}
