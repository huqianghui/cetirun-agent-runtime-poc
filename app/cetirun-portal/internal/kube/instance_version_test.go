// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestInstanceUpdateDoesNotReplayAgainstChangedOrReplacedAgent(t *testing.T) {
	for _, operation := range []string{"update", "upgrade"} {
		for _, replacement := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replacement=%v", operation, replacement), func(t *testing.T) {
				instance := sampleInstance()
				instance.UID = "agent-uid"
				updates := 0
				fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().WithObjects(instance).
					WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						updates++
						current := &api.Agent{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
							return err
						}
						current.Spec.User.DisplayName = "Concurrent value"
						if replacement {
							if err := c.Delete(ctx, current); err != nil {
								return err
							}
							current.UID, current.ResourceVersion = "replacement-uid", ""
							if err := c.Create(ctx, current); err != nil {
								return err
							}
						} else if err := c.Update(ctx, current); err != nil {
							return err
						}
						return c.Patch(ctx, obj, patch, opts...)
					}}).Build()
				if err := fc.Get(context.Background(), client.ObjectKeyFromObject(instance), instance); err != nil {
					t.Fatal(err)
				}
				portal := NewClient(fc)
				portal.chartVersionVal, portal.chartVersionExp = "2.0.0", time.Now().Add(time.Minute)
				name := "Stale value"
				var err error
				if operation == "upgrade" {
					_, err = portal.UpgradeInstanceByName(context.Background(), instance.Name, InstanceVersion{UID: instance.UID, ResourceVersion: instance.ResourceVersion})
				} else {
					_, err = portal.UpdateInstanceByName(context.Background(), instance.Name, UpdateInstanceOpts{
						ExpectedUID: instance.UID, ExpectedResourceVersion: instance.ResourceVersion, DisplayName: &name,
					})
				}
				if !errors.Is(err, ErrInstanceChanged) || updates != 1 {
					t.Fatalf("update was replayed or conflict hidden: updates=%d err=%v", updates, err)
				}
				current := &api.Agent{}
				if err := fc.Get(context.Background(), client.ObjectKeyFromObject(instance), current); err != nil || current.Spec.User.DisplayName != "Concurrent value" || current.Spec.Helm.Version != instance.Spec.Helm.Version {
					t.Fatalf("concurrent value was overwritten: %v", err)
				}
			})
		}
	}
}

func TestRegistryIntentPatchDoesNotReplayAgainstChangedOrReplacedAgent(t *testing.T) {
	for _, remove := range []bool{false, true} {
		for _, replacement := range []bool{false, true} {
			t.Run(fmt.Sprintf("remove=%v/replacement=%v", remove, replacement), func(t *testing.T) {
				instance := sampleInstance()
				instance.UID = "agent-uid"
				instance.Annotations = map[string]string{registryauth.Operation: "original-operation"}
				instance.Spec.Agent.Image = ptr.To("registry.example.com/agent:1")
				instance.Spec.Agent.RegistrySecretRef = &api.RegistrySecretRef{Name: "registry-source-existing"}
				patches := 0
				fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().WithObjects(instance).
					WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						patches++
						current := &api.Agent{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
							return err
						}
						current.Annotations[registryauth.Operation] = "concurrent-operation"
						if replacement {
							if err := c.Delete(ctx, current); err != nil {
								return err
							}
							current.UID, current.ResourceVersion = "replacement-uid", ""
							if err := c.Create(ctx, current); err != nil {
								return err
							}
						} else if err := c.Update(ctx, current); err != nil {
							return err
						}
						return c.Patch(ctx, obj, patch, opts...)
					}}).Build()
				if err := fc.Get(context.Background(), client.ObjectKeyFromObject(instance), instance); err != nil {
					t.Fatal(err)
				}
				_, err := NewClient(fc).BeginRegistryUpdate(context.Background(), instance, remove)
				if !errors.Is(err, ErrRegistryConflict) || patches != 1 {
					t.Fatalf("registry update was replayed or conflict hidden: patches=%d err=%v", patches, err)
				}
				current := &api.Agent{}
				if err := fc.Get(context.Background(), client.ObjectKeyFromObject(instance), current); err != nil {
					t.Fatal(err)
				}
				if current.Annotations[registryauth.Operation] != "concurrent-operation" ||
					current.Spec.Agent.RegistrySecretRef == nil || current.Spec.Agent.RegistrySecretRef.Name != "registry-source-existing" {
					t.Fatal("stale registry intent overwrote the current operation or reference")
				}
			})
		}
	}
}

func TestDeleteChecksTheDisplayedAgentVersion(t *testing.T) {
	instance := sampleInstance()
	instance.UID, instance.ResourceVersion = "new-uid", "2"
	c := NewClient(newFakeClient(instance))
	err := c.DeleteInstanceByName(context.Background(), instance.Name, InstanceVersion{UID: "old-uid", ResourceVersion: "1"})
	if !errors.Is(err, ErrInstanceChanged) {
		t.Fatalf("stale delete was accepted: %v", err)
	}
	if _, err := c.GetInstance(context.Background(), instance.Name); err != nil {
		t.Fatal("replacement Agent was deleted")
	}
}

func TestUpdateRejectsTerminatingAgent(t *testing.T) {
	instance := sampleInstance()
	now := metav1.Now()
	instance.DeletionTimestamp = &now
	instance.Finalizers = []string{api.Finalizer}
	name := "Changed"
	_, err := NewClient(newFakeClient(instance)).UpdateInstanceByName(context.Background(), instance.Name, UpdateInstanceOpts{DisplayName: &name})
	if !errors.Is(err, ErrInstanceDeleting) {
		t.Fatalf("terminating Agent update was accepted: %v", err)
	}
}
