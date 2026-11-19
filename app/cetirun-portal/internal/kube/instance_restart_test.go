// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const firstRestartID = "request-00000001"
const secondRestartID = "request-00000002"

func restartableInstance() *api.Agent {
	instance := sampleInstance()
	instance.UID, instance.Generation = "agent-uid", 3
	instance.Spec.Agent.Type = api.AgentTypeOpenClaw
	instance.Status = api.AgentStatus{Phase: api.PhaseReady, ObservedGeneration: 3,
		Conditions: []metav1.Condition{{Type: api.ConditionReady, Status: metav1.ConditionTrue}}}
	return instance
}

func TestRetainedRestartPersistsAndRetriesWithoutTouchingData(t *testing.T) {
	ctx := context.Background()
	agent := restartableInstance()
	home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: "cetirun-alice", UID: "home-uid"}}
	key := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "model-key", Namespace: "cetirun-alice", UID: "key-uid"},
		Data: map[string][]byte{"key": []byte("synthetic-model-key")}}
	writes := 0
	lostResponse := errors.New("synthetic response lost after commit")
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().
		WithStatusSubresource(&api.Agent{}).WithObjects(agent, home, key).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes++
			if _, ok := obj.(*api.Agent); !ok {
				t.Fatalf("restart patched %T instead of only the Agent", obj)
			}
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if writes == 1 {
				return lostResponse
			}
			return nil
		}}).Build()
	if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	before := agent.DeepCopy()
	version := InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}
	if _, err := NewClient(fc).RestartInstanceByName(ctx, agent.Name, version, firstRestartID, nil); !errors.Is(err, lostResponse) {
		t.Fatalf("lost response was not reported: %v", err)
	}
	current, err := NewClient(fc).RestartInstanceByName(ctx, agent.Name, version, firstRestartID, nil)
	if err != nil || current.Spec.RestartRequestID != firstRestartID || writes != 1 {
		t.Fatalf("exact retry reapplied a restart: writes=%d err=%v", writes, err)
	}
	wantSpec := before.Spec.DeepCopy()
	wantSpec.RestartRequestID = firstRestartID
	if current.UID != before.UID || !reflect.DeepEqual(current.Spec, *wantSpec) || !reflect.DeepEqual(current.Status, before.Status) {
		t.Fatal("request changed Agent identity, unrelated desired state, or observed status")
	}
	if CanRestartInstance(current) {
		t.Fatal("a pending request allowed another restart")
	}
	for _, object := range []client.Object{home, key} {
		before := object.DeepCopyObject()
		if err := fc.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal(err)
		}
		// The fake API assigns resourceVersion during initial storage.
		before.(client.Object).SetResourceVersion(object.GetResourceVersion())
		if !reflect.DeepEqual(before, object) {
			t.Fatal("restart changed retained HOME or model credentials")
		}
	}
	current.Status.ObservedRestartRequestID = firstRestartID
	if err := fc.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	nextVersion := InstanceVersion{UID: current.UID, ResourceVersion: current.ResourceVersion}
	if _, err := NewClient(fc).RestartInstanceByName(ctx, current.Name, nextVersion, secondRestartID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(fc).RestartInstanceByName(ctx, current.Name, version, firstRestartID, nil); !errors.Is(err, ErrInstanceChanged) {
		t.Fatalf("late retry replaced a newer restart: %v", err)
	}
}

func TestRetainedRestartRejectsInvalidOrSupersededIntent(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*api.Agent, *InstanceVersion, *string)
		want   error
	}{
		{"suspended", func(a *api.Agent, _ *InstanceVersion, _ *string) { a.Spec.Suspended = true }, ErrInstanceSuspended},
		{"migration pending", func(a *api.Agent, _ *InstanceVersion, _ *string) {
			a.Spec.PlannedMigration = &api.PlannedMigrationRequest{RequestID: "migration-00000001",
				Target: api.MigrationObject{Name: "node-b", UID: "node-b-uid"}}
		}, ErrMigrationPending},
		{"suspended duplicate", func(a *api.Agent, _ *InstanceVersion, _ *string) {
			a.Spec.Suspended, a.Spec.RestartRequestID = true, firstRestartID
		}, ErrInstanceSuspended},
		{"deleting duplicate", func(a *api.Agent, _ *InstanceVersion, _ *string) {
			now := metav1.Now()
			a.Finalizers, a.DeletionTimestamp, a.Spec.RestartRequestID = []string{api.Finalizer}, &now, firstRestartID
		}, ErrInstanceDeleting},
		{"single-start", func(a *api.Agent, _ *InstanceVersion, _ *string) {
			a.Finalizers = []string{api.SandboxPreparationFinalizer}
		}, ErrRestartUnsupported},
		{"not ready", func(a *api.Agent, _ *InstanceVersion, _ *string) { a.Status.Phase = api.PhaseProvisioning }, ErrRestartNotReady},
		{"unready condition", func(a *api.Agent, _ *InstanceVersion, _ *string) {
			a.Status.Conditions[0].Status = metav1.ConditionFalse
		}, ErrRestartNotReady},
		{"missing condition", func(a *api.Agent, _ *InstanceVersion, _ *string) { a.Status.Conditions = nil }, ErrRestartNotReady},
		{"unobserved generation", func(a *api.Agent, _ *InstanceVersion, _ *string) { a.Generation++ }, ErrRestartNotReady},
		{"pending restart", func(a *api.Agent, _ *InstanceVersion, _ *string) { a.Spec.RestartRequestID = secondRestartID }, ErrRestartNotReady},
		{"replaced Agent", func(_ *api.Agent, v *InstanceVersion, _ *string) { v.UID = "previous-agent-uid" }, ErrInstanceChanged},
		{"stale version", func(_ *api.Agent, v *InstanceVersion, _ *string) { v.ResourceVersion = "stale" }, ErrInstanceChanged},
		{"no version", func(_ *api.Agent, v *InstanceVersion, _ *string) { v.ResourceVersion = "" }, ErrInvalidRestartRequest},
		{"no UID", func(_ *api.Agent, v *InstanceVersion, _ *string) { v.UID = "" }, ErrInvalidRestartRequest},
		{"short request", func(_ *api.Agent, _ *InstanceVersion, id *string) { *id = "short" }, ErrInvalidRestartRequest},
		{"long request", func(_ *api.Agent, _ *InstanceVersion, id *string) { *id = strings.Repeat("a", 129) }, ErrInvalidRestartRequest},
		{"invalid request", func(_ *api.Agent, _ *InstanceVersion, id *string) { *id = "request/00000001" }, ErrInvalidRestartRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := restartableInstance()
			agent.ResourceVersion = "7"
			version, id := InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}, firstRestartID
			test.change(agent, &version, &id)
			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(agent).Build()
			_, err := NewClient(fc).RestartInstanceByName(context.Background(), agent.Name, version, id, nil)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			current := &api.Agent{}
			if err := fc.Get(context.Background(), client.ObjectKeyFromObject(agent), current); err != nil || !reflect.DeepEqual(current.Spec, agent.Spec) {
				t.Fatalf("rejected request changed desired state: %v", err)
			}
		})
	}
}

func TestRetainedRestartDoesNotRetryAcrossConcurrentSuspension(t *testing.T) {
	ctx := context.Background()
	agent := restartableInstance()
	writes := 0
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().WithObjects(agent).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes++
			current := &api.Agent{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
				return err
			}
			current.Spec.Suspended = true
			if err := c.Update(ctx, current); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		}}).Build()
	if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	_, err := NewClient(fc).RestartInstanceByName(ctx, agent.Name, InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}, firstRestartID, nil)
	if !apierrors.IsConflict(err) || writes != 1 {
		t.Fatalf("concurrent suspension was replayed or hidden: writes=%d err=%v", writes, err)
	}
}
