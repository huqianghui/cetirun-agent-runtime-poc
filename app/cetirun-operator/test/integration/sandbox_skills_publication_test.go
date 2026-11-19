// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package integration_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Catalog publication is Gateway-independent. It creates a read-only, retained
// skills binding, then stops before the now-unsupported held direct-provider
// workload preparation path.
func TestSandboxBootstrapSkillsPublication(t *testing.T) {
	h := newPreparationHarness(t)
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			f := h.newCaseWithPrerequisites(t, typ, false)
			raw, err := json.Marshal(map[string]any{
				"version": "v1", "teams": map[string]any{f.fixture.Team.Name: map[string]any{
					"teamUID": f.fixture.Team.UID, "revision": "published-v1",
					"source": map[string]any{
						"resourceGroup": "skills-example", "storageAccount": "publishedskills",
						"containerName": "skills-v1", "storageClassName": "azureblob-nfs-premium", "storageSize": "1Gi",
					},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			catalog := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: f.fixture.Agent.Name + "-skills", Namespace: preparationStateNamespace},
				Data:       map[string]string{controller.SandboxSkillsCatalogKey: string(raw)},
			}
			h.create(t, catalog)
			configureBootstrapSkillsCatalog(t, f, catalog.Name)
			if _, err := f.reconciler.Reconcile(h.ctx, f.request()); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal("catalog bootstrap did not stop at unavailable qualified writer evidence:", err)
			}
			agent := f.agent(t)
			if f.installer.stageCalls != 0 || agent.Status.Sandbox != nil || agent.Status.AccessURL != "" ||
				!slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) ||
				slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) ||
				!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionAgentConfigReady) {
				t.Fatal("catalog bootstrap persisted runnable direct-provider state")
			}
			h.get(t, client.ObjectKeyFromObject(f.fixture.Namespace), f.fixture.Namespace)
			h.get(t, client.ObjectKeyFromObject(f.fixture.Input.Home), f.fixture.Input.Home)
			h.get(t, client.ObjectKeyFromObject(f.fixture.Input.Skills), f.fixture.Input.Skills)
			claim := f.fixture.Input.Skills
			volume := &corev1.PersistentVolume{}
			h.get(t, client.ObjectKey{Name: claim.Spec.VolumeName}, volume)
			if volume.Spec.ClaimRef.UID != claim.UID || volume.Spec.ClaimRef.Namespace != claim.Namespace ||
				volume.Spec.CSI == nil || !volume.Spec.CSI.ReadOnly ||
				volume.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain ||
				!slices.Contains(claim.Finalizers, sandbox.BootstrapFinalizer) ||
				!slices.Contains(volume.Finalizers, sandbox.BootstrapFinalizer) ||
				claim.Name == f.fixture.Input.Home.Name {
				t.Fatal("API-defaulted skills lost their exact static binding or lifecycle protection")
			}
			originalClaim, originalVolume := claim.DeepCopy(), volume.DeepCopy()
			if _, err := f.reconciler.Reconcile(h.ctx, f.request()); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal("API-defaulted bindings failed immutable retry:", err)
			}
			h.get(t, client.ObjectKeyFromObject(claim), claim)
			h.get(t, client.ObjectKeyFromObject(volume), volume)
			if claim.ResourceVersion != originalClaim.ResourceVersion || volume.ResourceVersion != originalVolume.ResourceVersion {
				t.Fatal("immutable retry rewrote the publication bindings")
			}
		})
	}
}
