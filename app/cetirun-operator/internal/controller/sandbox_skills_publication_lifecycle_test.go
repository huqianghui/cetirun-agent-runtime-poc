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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSandboxSkillsPublicationRejectsForeignBindings(t *testing.T) {
	for _, kind := range []string{"claim", "volume"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _ := newSkillsPublicationFixture(t, "hermes")
			var foreign client.Object
			f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
					_, isVolume := object.(*corev1.PersistentVolume)
					_, isClaim := object.(*corev1.PersistentVolumeClaim)
					if !((kind == "volume" && isVolume) || (kind == "claim" && isClaim && object.GetName() == f.fixture.Input.Skills.Name)) {
						return c.Create(ctx, object, options...)
					}
					foreign = object.DeepCopyObject().(client.Object)
					foreign.SetOwnerReferences(nil)
					foreign.SetAnnotations(nil)
					foreign.SetFinalizers(nil)
					if err := c.Create(ctx, foreign); err != nil {
						return err
					}
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: kind}, object.GetName())
				},
			})
			if err := f.reconcile(); !apierrors.IsAlreadyExists(err) || foreign == nil {
				t.Fatal("concurrent foreign binding was not exercised:", err)
			}
			f.reconciler.Client = f.client
			for range 2 {
				if err := f.reconcile(); err == nil || errors.Is(err, sandbox.ErrObservationUnavailable) {
					t.Fatal("foreign publication binding was adopted:", err)
				}
				current := foreign.DeepCopyObject().(client.Object)
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(foreign), current); err != nil ||
					!equality.Semantic.DeepEqual(foreign, current) {
					t.Fatal("foreign publication binding was mutated:", err)
				}
			}
			if err := f.client.Delete(ctx, f.current(t)); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); err == nil {
				t.Fatal("cleanup adopted a foreign binding outside its receipt")
			}
		})
	}
}

func TestSandboxSkillsPublicationRejectsReplacementAndUnsafeDrift(t *testing.T) {
	for _, change := range []string{"claim replacement", "volume replacement", "reclaim policy", "writable source", "claim UID", "source container"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f, _ := newSkillsPublicationFixture(t, "openclaw")
			if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal(err)
			}
			claim, volume := publicationObjects(t, f)
			var object client.Object = volume
			switch change {
			case "claim replacement":
				object = claim
			case "reclaim policy":
				volume.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
			case "writable source":
				volume.Spec.CSI.ReadOnly = false
			case "claim UID":
				volume.Spec.ClaimRef.UID = "foreign-claim"
			case "source container":
				volume.Spec.CSI.VolumeAttributes["containerName"] = "unapproved-container"
			}
			if strings.HasSuffix(change, "replacement") {
				object.SetFinalizers(nil)
				if err := f.client.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
				if err := f.client.Delete(ctx, object); err != nil {
					t.Fatal(err)
				}
				object.SetUID("")
				object.SetResourceVersion("")
				if err := f.client.Create(ctx, object); err != nil {
					t.Fatal(err)
				}
			} else if err := f.client.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			for _, deleting := range []bool{false, true} {
				if deleting {
					if err := f.client.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.reconcile(); err == nil || errors.Is(err, sandbox.ErrObservationUnavailable) {
					t.Fatal("unsafe drift passed preparation or cleanup:", err)
				}
				current := object.DeepCopyObject().(client.Object)
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil ||
					!equality.Semantic.DeepEqual(object, current) {
					t.Fatal("drifted resource was repaired or deleted without recovery:", err)
				}
			}
		})
	}
}

func TestSandboxSkillsPublicationCatalogChangeDuringCreate(t *testing.T) {
	f, catalog := newSkillsPublicationFixture(t, "openclaw")
	f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if err := c.Create(ctx, object, options...); err != nil {
				return err
			}
			if _, ok := object.(*corev1.PersistentVolumeClaim); ok && object.GetName() == f.fixture.Input.Skills.Name {
				catalog.Data[controller.SandboxSkillsCatalogKey] = strings.ReplaceAll(catalog.Data[controller.SandboxSkillsCatalogKey], "published-v1", "published-v2")
				return c.Update(ctx, catalog)
			}
			return nil
		},
	})
	if err := f.reconcile(); err == nil {
		t.Fatal("concurrent catalog change was not guarded")
	}
	var volumes corev1.PersistentVolumeList
	if err := f.client.List(context.Background(), &volumes); err != nil || len(volumes.Items) != 0 {
		t.Fatal("a PV was created after publication authority changed:", err)
	}
	f.reconciler.Client = f.client
	if err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "recovery") {
		t.Fatal("retry replaced the recorded publication with a different approval:", err)
	}
}

func TestSandboxSkillsPublicationReceiptConfirmationFailures(t *testing.T) {
	for _, index := range []int{5, 6} {
		for _, responseLost := range []bool{false, true} {
			t.Run(fmt.Sprintf("object=%d/response-lost=%t", index, responseLost), func(t *testing.T) {
				f, _ := newSkillsPublicationFixture(t, "hermes")
				failure := errors.New("synthetic UID confirmation failure")
				f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
						match := false
						if cm, ok := object.(*corev1.ConfigMap); ok && cm.Data["bootstrap.json"] != "" {
							var receipt struct{ Objects []struct{ UID string } }
							if err := json.Unmarshal([]byte(cm.Data["bootstrap.json"]), &receipt); err != nil {
								t.Fatal(err)
							}
							match = len(receipt.Objects) == 7 && receipt.Objects[index].UID != ""
						}
						if !match {
							return c.Patch(ctx, object, patch, options...)
						}
						if responseLost {
							if err := c.Patch(ctx, object, patch, options...); err != nil {
								return err
							}
						}
						return failure
					},
				})
				if err := f.reconcile(); !errors.Is(err, failure) {
					t.Fatal("UID confirmation failure was hidden:", err)
				}
				f.reconciler.Client = f.client
				if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
					t.Fatal("UID confirmation retry did not recover:", err)
				}
				publicationObjects(t, f)
				assertPublicationHeld(t, f)
			})
		}
	}
}

func TestSandboxSkillsPublicationCleanupRetainsSourceAndOtherFinalizers(t *testing.T) {
	for _, otherFinalizer := range []bool{false, true} {
		t.Run(fmt.Sprintf("other-finalizer=%t", otherFinalizer), func(t *testing.T) {
			ctx := context.Background()
			f, catalog := newSkillsPublicationFixture(t, "hermes")
			if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal(err)
			}
			claim, volume := publicationObjects(t, f)
			unrelated := volume.DeepCopy()
			unrelated.Name, unrelated.UID, unrelated.ResourceVersion = "unrelated-published-source", "", ""
			unrelated.OwnerReferences, unrelated.Finalizers, unrelated.Annotations = nil, nil, nil
			if err := f.client.Create(ctx, unrelated); err != nil {
				t.Fatal(err)
			}
			if otherFinalizer {
				volume.Finalizers = append(volume.Finalizers, "example.com/retain")
				if err := f.client.Update(ctx, volume); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.client.Delete(ctx, catalog); err != nil {
				t.Fatal(err)
			}
			agent, receipt := f.current(t), bootstrapReceipt(t, f)
			if err := f.client.Delete(ctx, agent); err != nil {
				t.Fatal(err)
			}
			f.reconciler.SandboxPreparation = nil
			if err := f.reconcile(); err != nil {
				t.Fatal("bootstrap-only cleanup depended on the deleted catalog:", err)
			}
			// The fake client has no namespace controller. Emulate its PVC
			// deletion only after the real cleanup path has released protection.
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil ||
				slices.Contains(claim.Finalizers, sandbox.BootstrapFinalizer) {
				t.Fatal("namespace cleanup did not release the owned claim:", err)
			}
			if err := f.client.Delete(ctx, claim); err != nil {
				t.Fatal(err)
			}
			err := f.reconcile()
			if otherFinalizer {
				if err == nil {
					t.Fatal("cleanup discarded a still-retained PV obligation")
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(volume), volume); err != nil ||
					!slices.Equal(volume.Finalizers, []string{"example.com/retain"}) ||
					volume.DeletionTimestamp.IsZero() || volume.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
					t.Fatal("cleanup changed backing-data retention or another controller's finalizer:", err)
				}
				if !slices.Contains(f.current(t).Finalizers, sandbox.BootstrapFinalizer) {
					t.Fatal("cleanup discarded unresolved PV ownership")
				}
				volume.Finalizers = nil // Simulate completion by the synthetic finalizer owner.
				if err := f.client.Update(ctx, volume); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(); err != nil {
					t.Fatal("cleanup did not resume:", err)
				}
			} else if err != nil {
				t.Fatal("static binding cleanup failed:", err)
			}
			for _, object := range []client.Object{agent, receipt, volume} {
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
					t.Fatalf("cleanup retained %T after confirmed PV absence: %v", object, err)
				}
			}
			actual := &corev1.PersistentVolume{}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(unrelated), actual); err != nil ||
				!equality.Semantic.DeepEqual(unrelated, actual) {
				t.Fatal("cleanup mutated an unrelated shared-source binding:", err)
			}
		})
	}
}

func TestSandboxSkillsPublicationCleanupMutationPreconditions(t *testing.T) {
	for _, operation := range []string{"delete", "release"} {
		for _, responseLost := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/response-lost=%t", operation, responseLost), func(t *testing.T) {
				ctx := context.Background()
				f, _ := newSkillsPublicationFixture(t, "openclaw")
				if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
					t.Fatal(err)
				}
				claim, volume := publicationObjects(t, f)
				if err := f.client.Delete(ctx, f.current(t)); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(); err != nil {
					t.Fatal(err)
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
					t.Fatal(err)
				}
				if err := f.client.Delete(ctx, claim); err != nil {
					t.Fatal(err)
				}
				failure := errors.New("synthetic PV cleanup response failure")
				f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
						if _, ok := object.(*corev1.PersistentVolume); !ok {
							return c.Delete(ctx, object, options...)
						}
						preconditions := (&client.DeleteOptions{}).ApplyOptions(options).Preconditions
						if preconditions == nil || preconditions.UID == nil || *preconditions.UID != volume.UID ||
							preconditions.ResourceVersion == nil || *preconditions.ResourceVersion != object.GetResourceVersion() {
							t.Fatal("PV delete lacked exact UID/resourceVersion preconditions")
						}
						if operation == "release" || responseLost {
							if err := c.Delete(ctx, object, options...); err != nil {
								return err
							}
						}
						if operation == "delete" {
							return failure
						}
						return nil
					},
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
						if _, ok := object.(*corev1.PersistentVolume); !ok {
							return c.Patch(ctx, object, patch, options...)
						}
						raw, err := patch.Data(object)
						if err != nil {
							t.Fatal(err)
						}
						var data struct{ Metadata metav1.ObjectMeta }
						if err := json.Unmarshal(raw, &data); err != nil || data.Metadata.UID != volume.UID ||
							data.Metadata.ResourceVersion != object.GetResourceVersion() {
							t.Fatal("PV finalizer release lacked exact UID/resourceVersion preconditions:", err)
						}
						if responseLost {
							if err := c.Patch(ctx, object, patch, options...); err != nil {
								return err
							}
						}
						return failure
					},
				})
				if err := f.reconcile(); !errors.Is(err, failure) {
					t.Fatal("cleanup failure was hidden:", err)
				}
				if !slices.Contains(f.current(t).Finalizers, sandbox.BootstrapFinalizer) {
					t.Fatal("interrupted cleanup abandoned the PV receipt")
				}
				f.reconciler.Client = f.client
				if err := f.reconcile(); err != nil {
					t.Fatal("interrupted PV cleanup did not recover:", err)
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(volume), &corev1.PersistentVolume{}); !apierrors.IsNotFound(err) {
					t.Fatal("recovered cleanup retained the PV:", err)
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Agent), &api.Agent{}); !apierrors.IsNotFound(err) {
					t.Fatal("recovered cleanup retained the Agent:", err)
				}
			})
		}
	}

}

func TestSandboxSkillsPublicationPreservesExistingExternalClaimMode(t *testing.T) {
	ctx := context.Background()
	f := bootstrappedFixture(t, "hermes")
	receipt := bootstrapReceipt(t, f)
	config := controller.SandboxSourcesConfig{
		Enabled: true, BootstrapOwnedPrerequisites: true, PlatformConfigMapName: "sandbox-platform",
		HomeClaimName: f.fixture.Input.Home.Name, SkillsClaimName: f.fixture.Input.Skills.Name,
		SkillsCatalogConfigMapName: "not-used-for-existing-bootstrap", Helpers: f.fixture.Input.Helpers,
		AdmissionServiceAccount: "cetirun-sandbox-admission",
	}
	preparation, err := controller.NewKubernetesSandboxPreparation(config, f.client, f.installer, "cetirun-system", f.installer.chart.Metadata.Version)
	if err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation = preparation
	skills := f.fixture.Input.Skills.DeepCopy()
	skills.UID, skills.ResourceVersion = "", ""
	if err := f.client.Create(ctx, skills); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
		t.Fatal("enabling a catalog migrated an existing external-claim receipt:", err)
	}
	var volumes corev1.PersistentVolumeList
	if err := f.client.List(ctx, &volumes); err != nil || len(volumes.Items) != 0 ||
		bootstrapReceipt(t, f).ResourceVersion != receipt.ResourceVersion {
		t.Fatal("legacy bootstrap was rebound to a catalog:", err)
	}
}

func TestSandboxSkillsPublicationRetriesKeepCredentialAndStorageReadsBounded(t *testing.T) {
	f, _ := newSkillsPublicationFixture(t, "hermes")
	if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
		t.Fatal(err)
	}
	var secretReads, writes int
	spy := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, ok := object.(*corev1.Secret); ok {
				secretReads++
				return errors.New("publication attempted to read native or provider credentials")
			}
			return c.Get(ctx, key, object, options...)
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			writes++
			return errors.New("unchanged publication attempted a create")
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			writes++
			return errors.New("unchanged publication attempted a patch")
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			writes++
			return errors.New("unchanged publication attempted an update")
		},
	})
	f.reconciler.Client, f.reconciler.APIReader = spy, spy
	f.reconciler.SandboxPreparation.Configuration.(*controller.KubernetesSandboxConfigurationSource).APIReader = spy
	source := f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites)
	source.APIReader = spy
	for range 2 {
		if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
			t.Fatal("unchanged publication retry did not remain at the fence boundary:", err)
		}
	}
	if writes != 0 || secretReads != 0 {
		t.Fatal("unchanged publication rewrote its objects or read credentials")
	}
	tuple, err := source.Resolve(context.Background(), f.current(t), f.fixture.Team)
	if err != nil || len(tuple.PublicationSources) != 3 {
		t.Fatal("publication dependencies were not captured:", err)
	}
	original := slices.Clone(tuple.PublicationSources)
	source.WriterExclusion = &testSandboxWriterExclusion{check: func(_ context.Context, _ *api.Agent, input controller.SandboxPrerequisites) {
		input.PublicationSources[0].Name = "provider-must-not-mutate-caller"
	}}
	if err := source.CheckQuiesced(context.Background(), f.current(t), tuple); err != nil || !slices.Equal(original, tuple.PublicationSources) {
		t.Fatal("writer-exclusion handoff changed the caller's publication snapshot:", err)
	}
}

func TestSandboxSkillsPublicationPrerequisitesRejectMissingNamespaceMarker(t *testing.T) {
	ctx := context.Background()
	f, _ := newSkillsPublicationFixture(t, "openclaw")
	if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), namespace); err != nil {
		t.Fatal(err)
	}
	delete(namespace.Annotations, "cetirun.microsoft.com/sandbox-bootstrap")
	if err := f.client.Update(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	source := f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites)
	if _, err := source.Resolve(ctx, f.current(t), f.fixture.Team); err == nil {
		t.Fatal("marker removal downgraded recorded publication to an external claim")
	}
}
