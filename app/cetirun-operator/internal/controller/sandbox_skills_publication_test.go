// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newSkillsPublicationFixture(t *testing.T, typ string) (*preparationFixture, *corev1.ConfigMap) {
	t.Helper()
	f := newSandboxBootstrapFixture(t, typ, true)
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
		ObjectMeta: metav1.ObjectMeta{Name: "skills-catalog", Namespace: "cetirun-system"},
		Data:       map[string]string{controller.SandboxSkillsCatalogKey: string(raw)},
	}
	if err := f.client.Create(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	config := controller.SandboxSourcesConfig{
		Enabled: true, BootstrapOwnedPrerequisites: true, PlatformConfigMapName: "sandbox-platform",
		HomeClaimName: f.fixture.Input.Home.Name, SkillsClaimName: f.fixture.Input.Skills.Name,
		SkillsCatalogConfigMapName: catalog.Name, AdmissionServiceAccount: "cetirun-sandbox-admission",
		Helpers: f.fixture.Input.Helpers,
	}
	preparation, err := controller.NewKubernetesSandboxPreparation(config, f.client, f.installer,
		"cetirun-system", f.installer.chart.Metadata.Version)
	if err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation = preparation
	return f, catalog
}

func TestSandboxNativeEmptySelectionPreservesPublishedSkills(t *testing.T) {
	for _, change := range []string{"unchanged", "missing catalog", "malformed catalog", "changed publication", "removed catalog", "missing claim"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f, catalog := newSkillsPublicationFixture(t, "openclaw")
			if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal("publication bootstrap did not reach held prerequisites:", err)
			}
			source := f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites)
			source.NativeExecution = true
			team := &api.Team{}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Team), team); err != nil {
				t.Fatal(err)
			}
			if len(team.Spec.Skills) != 0 {
				t.Fatal("catalog regression must exercise an empty effective selection")
			}
			switch change {
			case "missing catalog":
				if err := f.client.Delete(ctx, catalog); err != nil {
					t.Fatal(err)
				}
			case "malformed catalog":
				catalog.Data[controller.SandboxSkillsCatalogKey] = `{"version":"v1","teams":null}`
				if err := f.client.Update(ctx, catalog); err != nil {
					t.Fatal(err)
				}
			case "changed publication":
				catalog.Data[controller.SandboxSkillsCatalogKey] = strings.ReplaceAll(catalog.Data[controller.SandboxSkillsCatalogKey], "published-v1", "published-v2")
				if err := f.client.Update(ctx, catalog); err != nil {
					t.Fatal(err)
				}
			case "removed catalog":
				source.SkillsCatalogConfigMapName = ""
			case "missing claim":
				claim, _ := publicationObjects(t, f)
				if err := f.client.Delete(ctx, claim); err != nil {
					t.Fatal(err)
				}
			}
			got, err := source.Resolve(ctx, f.current(t), team)
			if change == "unchanged" {
				if err != nil || got.SkillsMode != "" || got.Skills.UID == "" || len(got.PublicationSources) != 3 {
					t.Fatal("configured catalog did not preserve its validated PVC/publication path:", err)
				}
			} else {
				assertPrerequisiteRejection(t, got, err)
			}
		})
	}
}

func publicationObjects(t *testing.T, f *preparationFixture) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	t.Helper()
	claim, volume := &corev1.PersistentVolumeClaim{}, &corev1.PersistentVolume{}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Input.Skills), claim); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Get(context.Background(), client.ObjectKey{Name: claim.Spec.VolumeName}, volume); err != nil {
		t.Fatal(err)
	}
	return claim, volume
}

func assertPublicationHeld(t *testing.T, f *preparationFixture) {
	t.Helper()
	agent := f.current(t)
	if agent.Status.Sandbox != nil || agent.Status.AccessURL != "" || f.installer.stages != 0 ||
		!slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) ||
		!slices.Contains(agent.Finalizers, sandbox.BootstrapFinalizer) ||
		slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) ||
		!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionReady) ||
		!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionAgentConfigReady) {
		t.Fatal("published skills were confused with qualified preparation or runnable enforcement")
	}
}

func TestSandboxSkillsPublicationColdCreateAndHeldHandoff(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			ctx := context.Background()
			f, catalog := newSkillsPublicationFixture(t, typ)
			team := f.fixture.Team.DeepCopy()
			team.Spec.SkillStorage = &api.SkillStorageSpec{
				ResourceGroup: "caller-rg", StorageAccount: "calleraccount", ContainerName: "caller-container",
			}
			if err := f.client.Update(ctx, team); err != nil {
				t.Fatal(err)
			}
			before := f.current(t).Spec.DeepCopy()
			if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal("catalog cold-create did not reach the missing-fence boundary:", err)
			}
			assertPublicationHeld(t, f)
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), f.fixture.Namespace); err != nil {
				t.Fatal(err)
			}
			claim, volume := publicationObjects(t, f)
			if claim.Name == f.fixture.Input.Home.Name || claim.Spec.DataSource != nil || claim.Spec.DataSourceRef != nil ||
				claim.Spec.Selector != nil || claim.Spec.VolumeName != volume.Name ||
				volume.Spec.ClaimRef == nil || volume.Spec.ClaimRef.UID != claim.UID ||
				volume.Spec.ClaimRef.Name != claim.Name || volume.Spec.ClaimRef.Namespace != claim.Namespace ||
				volume.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain ||
				volume.Spec.CSI == nil || !volume.Spec.CSI.ReadOnly || volume.Spec.CSI.Driver != "blob.csi.azure.com" ||
				!slices.Equal(volume.Spec.MountOptions, []string{"ro"}) ||
				volume.Spec.CSI.VolumeAttributes["storageAccount"] != "publishedskills" ||
				volume.Spec.CSI.VolumeAttributes["protocol"] != "nfs" || volume.Spec.CSI.NodeStageSecretRef != nil {
				t.Fatal("skills did not use the static UID-bound read-only catalog source")
			}
			receipt := bootstrapReceipt(t, f)
			var inventory struct {
				Version string
				Objects []struct {
					Kind, Name string
					UID        types.UID
				}
			}
			if err := json.Unmarshal([]byte(receipt.Data["bootstrap.json"]), &inventory); err != nil {
				t.Fatal(err)
			}
			if inventory.Version != "v2" || len(inventory.Objects) != 7 ||
				inventory.Objects[5].UID != claim.UID || inventory.Objects[6].UID != volume.UID {
				t.Fatal("published bindings were not durably inventoried")
			}
			for _, object := range []client.Object{claim, volume} {
				if !slices.Contains(object.GetFinalizers(), sandbox.BootstrapFinalizer) ||
					object.GetAnnotations()["cetirun.microsoft.com/sandbox-bootstrap"] != string(receipt.UID) ||
					!equality.Semantic.DeepEqual(object.GetOwnerReferences(),
						[]metav1.OwnerReference{sandboxworkload.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)}) {
					t.Fatal("published binding lost its exact Agent/receipt ownership")
				}
			}
			secret := &corev1.Secret{}
			if err := f.client.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: f.fixture.Agent.Spec.GatewayTokenSecretRef}, secret); err != nil {
				t.Fatal(err)
			}
			wantKeys := []string{"OPENCLAW_GATEWAY_TOKEN", "CETIRUN_AUTH_KEY"}
			if typ == "hermes" {
				wantKeys = append(wantKeys, "API_SERVER_KEY", "hermes.env")
			}
			if len(secret.Data) != len(wantKeys) {
				t.Fatal("native bootstrap added non-authentication or provider credentials")
			}
			for _, key := range wantKeys {
				if len(secret.Data[key]) == 0 {
					t.Fatal("native authentication initialization is incomplete")
				}
			}
			if !equality.Semantic.DeepEqual(f.current(t).Spec, *before) {
				t.Fatal("catalog bootstrap defaulted Agent launch inputs")
			}
			if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal("retry did not remain held:", err)
			}
			afterClaim, afterVolume := publicationObjects(t, f)
			if !equality.Semantic.DeepEqual(claim, afterClaim) || !equality.Semantic.DeepEqual(volume, afterVolume) ||
				bootstrapReceipt(t, f).ResourceVersion != receipt.ResourceVersion {
				t.Fatal("retry changed immutable bindings or their receipt")
			}
			account := f.fixture.ServiceAccount.DeepCopy()
			account.UID, account.ResourceVersion = "", ""
			if err := f.client.Create(ctx, account); err != nil {
				t.Fatal(err)
			}
			// This test-only dependency exercises held rendering, not a qualified fence.
			f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites).WriterExclusion = &testSandboxWriterExclusion{}
			if err := f.reconcile(); err != nil {
				t.Fatal("published skills could not hand off to held rendering:", err)
			}
			assertSandboxPrepared(t, f)
			expected := f.expectation(t)
			for _, object := range []client.Object{catalog, receipt, volume} {
				if !slices.ContainsFunc(expected.Sources, func(source sandboxworkload.SourceReference) bool {
					return source.Namespace == object.GetNamespace() && source.Name == object.GetName() &&
						source.UID == object.GetUID() && source.ResourceVersion == object.GetResourceVersion()
				}) {
					t.Fatalf("held admission omitted the %T publication dependency", object)
				}
			}
			var volumeName string
			for _, volume := range expected.Template.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim.Name {
					volumeName = volume.Name
					if !volume.PersistentVolumeClaim.ReadOnly {
						t.Fatal("skills claim is writable in the held template")
					}
				}
			}
			if volumeName == "" {
				t.Fatal("held template did not consume the published skills claim")
			}
			for _, container := range append(expected.Template.Spec.Containers, expected.Template.Spec.InitContainers...) {
				for _, mount := range container.VolumeMounts {
					if mount.Name == volumeName && !mount.ReadOnly {
						t.Fatal("a native container has writable published skills")
					}
				}
			}
			stages := f.installer.stages
			catalog.Data[controller.SandboxSkillsCatalogKey] = strings.ReplaceAll(catalog.Data[controller.SandboxSkillsCatalogKey], "published-v1", "published-v2")
			if err := f.client.Update(ctx, catalog); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); !errors.Is(err, sandboxworkload.ErrStorageTransitionRequired) {
				t.Fatal("changed publication was silently rebound:", err)
			}
			if f.installer.stages != stages || !apimeta.IsStatusConditionFalse(f.current(t).Status.Conditions, api.ConditionAgentConfigReady) {
				t.Fatal("changed publication retained preparation or staged another release")
			}
		})
	}
}

func TestSandboxSkillsCatalogRejectsInvalidPublicationBeforeEffects(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*preparationFixture, *corev1.ConfigMap)
	}{
		{"missing document", func(_ *preparationFixture, c *corev1.ConfigMap) { c.Data = nil }},
		{"extra document", func(_ *preparationFixture, c *corev1.ConfigMap) { c.Data["extra"] = "unused" }},
		{"binary document", func(_ *preparationFixture, c *corev1.ConfigMap) {
			c.BinaryData = map[string][]byte{"extra": []byte("unused")}
		}},
		{"malformed", func(_ *preparationFixture, c *corev1.ConfigMap) { c.Data[controller.SandboxSkillsCatalogKey] = "{" }},
		{"null", func(_ *preparationFixture, c *corev1.ConfigMap) {
			c.Data[controller.SandboxSkillsCatalogKey] = `{"version":"v1","teams":null}`
		}},
		{"empty catalog", func(_ *preparationFixture, c *corev1.ConfigMap) {
			c.Data[controller.SandboxSkillsCatalogKey] = `{"version":"v1","teams":{}}`
		}},
		{"wrong Team name", func(f *preparationFixture, c *corev1.ConfigMap) {
			c.Data[controller.SandboxSkillsCatalogKey] = strings.ReplaceAll(c.Data[controller.SandboxSkillsCatalogKey], f.fixture.Team.Name, "other-team")
		}},
		{"wrong Team UID", func(f *preparationFixture, c *corev1.ConfigMap) {
			c.Data[controller.SandboxSkillsCatalogKey] = strings.ReplaceAll(c.Data[controller.SandboxSkillsCatalogKey], string(f.fixture.Team.UID), "different-team-uid")
		}},
		{"oversized", func(_ *preparationFixture, c *corev1.ConfigMap) {
			raw := c.Data[controller.SandboxSkillsCatalogKey]
			c.Data[controller.SandboxSkillsCatalogKey] = raw + strings.Repeat(" ", controller.MaxSandboxSkillsCatalogBytes-len(raw)+1)
		}},
		{"excessive nesting", func(_ *preparationFixture, c *corev1.ConfigMap) {
			c.Data[controller.SandboxSkillsCatalogKey] = strings.Repeat("[", 6) + "0" + strings.Repeat("]", 6)
		}},
		{"excessive tokens", func(_ *preparationFixture, c *corev1.ConfigMap) {
			c.Data[controller.SandboxSkillsCatalogKey] = "[" + strings.Repeat("0,", 8192) + "0]"
		}},
		{"too many Teams", func(_ *preparationFixture, c *corev1.ConfigMap) {
			var document struct {
				Version string
				Teams   map[string]json.RawMessage
			}
			if err := json.Unmarshal([]byte(c.Data[controller.SandboxSkillsCatalogKey]), &document); err != nil {
				t.Fatal(err)
			}
			var publication json.RawMessage
			for _, value := range document.Teams {
				publication = value
			}
			for i := range 128 {
				document.Teams[fmt.Sprintf("other-team-%d", i)] = publication
			}
			raw, err := json.Marshal(map[string]any{"version": "v1", "teams": document.Teams})
			if err != nil {
				t.Fatal(err)
			}
			c.Data[controller.SandboxSkillsCatalogKey] = string(raw)
		}},
		{"invalid UTF-8", func(_ *preparationFixture, c *corev1.ConfigMap) {
			c.Data[controller.SandboxSkillsCatalogKey] = string([]byte{0xff})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, catalog := newSkillsPublicationFixture(t, "hermes")
			test.edit(f, catalog)
			assertCatalogRejectedBeforeEffects(t, f, catalog)
		})
	}
	for _, test := range []struct{ name, old, replacement string }{
		{"wrong version", `"version":"v1"`, `"version":"v2"`},
		{"duplicate version", `"version":"v1"`, `"version":"v1","version":"v1"`},
		{"unknown field", `"revision":`, `"unapproved":true,"revision":`},
		{"wrong field case", `"revision":`, `"Revision":`},
		{"null source", `"storageAccount":"publishedskills"`, `"storageAccount":null`},
		{"missing revision", `"revision":"published-v1",`, ""},
		{"invalid resource group", `"skills-example"`, `"https://example.com/source"`},
		{"invalid account", `"publishedskills"`, `"Caller/Account"`},
		{"invalid container", `"skills-v1"`, `"invalid--container"`},
		{"invalid class", `"azureblob-nfs-premium"`, `"Bad/Class"`},
		{"invalid size", `"1Gi"`, `"-1Gi"`},
		{"credentials forbidden", `"storageAccount":`, `"secretName":"caller-secret","storageAccount":`},
		{"mount options forbidden", `"storageAccount":`, `"mountOptions":["rw"],"storageAccount":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, catalog := newSkillsPublicationFixture(t, "openclaw")
			raw := catalog.Data[controller.SandboxSkillsCatalogKey]
			if !strings.Contains(raw, test.old) {
				t.Fatal("negative catalog fixture did not exercise its intended field")
			}
			catalog.Data[controller.SandboxSkillsCatalogKey] = strings.Replace(raw, test.old, test.replacement, 1)
			assertCatalogRejectedBeforeEffects(t, f, catalog)
		})
	}
	t.Run("exact byte bound", func(t *testing.T) {
		f, catalog := newSkillsPublicationFixture(t, "openclaw")
		raw := catalog.Data[controller.SandboxSkillsCatalogKey]
		catalog.Data[controller.SandboxSkillsCatalogKey] = raw + strings.Repeat(" ", controller.MaxSandboxSkillsCatalogBytes-len(raw))
		if err := f.client.Update(context.Background(), catalog); err != nil {
			t.Fatal(err)
		}
		if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
			t.Fatal("valid catalog at its exact byte bound was rejected:", err)
		}
	})
}

func assertCatalogRejectedBeforeEffects(t *testing.T, f *preparationFixture, catalog *corev1.ConfigMap) {
	t.Helper()
	ctx := context.Background()
	if err := f.client.Update(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("invalid catalog was accepted")
	}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatal("invalid catalog created a namespace:", err)
	}
	var volumes corev1.PersistentVolumeList
	if err := f.client.List(ctx, &volumes); err != nil || len(volumes.Items) != 0 {
		t.Fatal("invalid catalog created storage:", err)
	}
	var state corev1.ConfigMapList
	if err := f.client.List(ctx, &state, client.InNamespace(catalog.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 2 || slices.Contains(f.current(t).Finalizers, sandbox.BootstrapFinalizer) || f.installer.stages != 0 {
		t.Fatal("invalid catalog persisted bootstrap intent or a workload")
	}
}

func TestSandboxSkillsPublicationInterruptedCreates(t *testing.T) {
	for _, target := range []string{"claim", "volume"} {
		for _, responseLost := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/response-lost=%t", target, responseLost), func(t *testing.T) {
				f, catalog := newSkillsPublicationFixture(t, "hermes")
				failure := errors.New("synthetic published-skills create failure")
				var created client.Object
				f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
						_, volume := object.(*corev1.PersistentVolume)
						claim, isClaim := object.(*corev1.PersistentVolumeClaim)
						match := (target == "volume" && volume) || (target == "claim" && isClaim && claim.Name == f.fixture.Input.Skills.Name)
						if !match {
							return c.Create(ctx, object, options...)
						}
						var receipt struct {
							Skills struct {
								Catalog sandboxworkload.SourceReference
							}
							Objects []struct{ UID types.UID }
						}
						if err := json.Unmarshal([]byte(bootstrapReceipt(t, f).Data["bootstrap.json"]), &receipt); err != nil {
							t.Fatal(err)
						}
						if receipt.Skills.Catalog.UID != catalog.UID || len(receipt.Objects) != 7 {
							t.Fatal("storage effect preceded durable publication selection")
						}
						if volume && (receipt.Objects[5].UID == "" || object.(*corev1.PersistentVolume).Spec.ClaimRef.UID != receipt.Objects[5].UID) {
							t.Fatal("PV create preceded durable exact PVC identity")
						}
						if responseLost {
							if err := c.Create(ctx, object, options...); err != nil {
								return err
							}
							created = object.DeepCopyObject().(client.Object)
						}
						return failure
					},
				})
				if err := f.reconcile(); !errors.Is(err, failure) {
					t.Fatal("interrupted create failure was hidden:", err)
				}
				f.reconciler.Client = f.client
				if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
					t.Fatal("interrupted create did not recover to the held prerequisite boundary:", err)
				}
				if created != nil {
					current := created.DeepCopyObject().(client.Object)
					if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(current), current); err != nil ||
						!equality.Semantic.DeepEqual(created, current) {
						t.Fatal("lost create response caused storage replacement or mutation:", err)
					}
				}
				assertPublicationHeld(t, f)
			})
		}
	}
}

func TestSandboxSkillsPublicationCatalogIdentityAndSelection(t *testing.T) {
	for _, change := range []string{"unrelated metadata", "removed", "replaced", "unapproved Team", "source changed", "catalog disabled"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f, catalog := newSkillsPublicationFixture(t, "openclaw")
			if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal(err)
			}
			claim, volume := publicationObjects(t, f)
			receipt := bootstrapReceipt(t, f)
			switch change {
			case "unrelated metadata":
				catalog.Labels = map[string]string{"example.com/reviewed": "true"}
			case "removed", "replaced":
				if err := f.client.Delete(ctx, catalog); err != nil {
					t.Fatal(err)
				}
				if change == "replaced" {
					catalog.UID, catalog.ResourceVersion = "", ""
					if err := f.client.Create(ctx, catalog); err != nil {
						t.Fatal(err)
					}
				}
			case "unapproved Team":
				catalog.Data[controller.SandboxSkillsCatalogKey] = `{"version":"v1","teams":{}}`
			case "source changed":
				catalog.Data[controller.SandboxSkillsCatalogKey] = strings.ReplaceAll(catalog.Data[controller.SandboxSkillsCatalogKey], "skills-v1", "skills-v2")
			case "catalog disabled":
				// Both consumers must reject loss of the selected startup authority.
				f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites).SkillsCatalogConfigMapName = ""
			}
			if change != "removed" && change != "replaced" && change != "catalog disabled" {
				if err := f.client.Update(ctx, catalog); err != nil {
					t.Fatal(err)
				}
			}
			err := f.reconcile()
			if change == "unrelated metadata" {
				if !errors.Is(err, sandbox.ErrObservationUnavailable) {
					t.Fatal("unrelated catalog metadata invalidated an unchanged publication:", err)
				}
			} else if err == nil || errors.Is(err, sandbox.ErrObservationUnavailable) || !strings.Contains(err.Error(), "recovery") {
				t.Fatal("changed catalog selection bypassed recovery:", err)
			}
			nextClaim, nextVolume := publicationObjects(t, f)
			if !equality.Semantic.DeepEqual(claim, nextClaim) || !equality.Semantic.DeepEqual(volume, nextVolume) ||
				receipt.ResourceVersion != bootstrapReceipt(t, f).ResourceVersion {
				t.Fatal("publication drift rewrote owned storage or its receipt")
			}
			assertPublicationHeld(t, f)
		})
	}
}
