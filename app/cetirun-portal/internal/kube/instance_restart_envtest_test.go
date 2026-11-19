// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package kube

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestRetainedRestartThroughServedAPI(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, name))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatal("set KUBEBUILDER_ASSETS to preinstalled isolated test binaries")
		}
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../../cetirun-operator/config/crd/bases"}, ErrorIfCRDPathMissing: true,
		ControlPlaneStartTimeout: 30 * time.Second, ControlPlaneStopTimeout: 30 * time.Second}
	config, err := env.Start()
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	kc, err := client.New(config, client.Options{Scheme: newScheme()})
	if err != nil {
		t.Fatal(err)
	}
	watchClient, err := client.NewWithWatch(config, client.Options{Scheme: newScheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := restartableInstance()
	agent.UID, agent.ResourceVersion, agent.Generation, agent.Status = "", "", 0, api.AgentStatus{}
	if err := kc.Create(ctx, agent, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
		t.Fatal(err)
	}
	agent.Status = api.AgentStatus{Phase: api.PhaseReady, ObservedGeneration: agent.Generation,
		Conditions: []metav1.Condition{{Type: api.ConditionReady, Status: metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(), Reason: "TestReady", Message: "Synthetic readiness for API validation"}}}
	if err := kc.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	workload := suspensionWorkload(agent)
	workload.UID = ""
	workload.Spec.ServiceName = workload.Name
	workload.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "suspension-fixture"}}
	workload.Spec.Template.Labels = workload.Spec.Selector.MatchLabels
	workload.Spec.Template.Spec.Containers[0].Image = "registry.example.com/fixture:1"
	if err := kc.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workload.Namespace}}); err != nil {
		t.Fatal(err)
	}
	if err := kc.Create(ctx, workload); err != nil {
		t.Fatal(err)
	}
	version := InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}
	startGeneration := agent.Generation
	current, err := NewClient(kc).RestartInstanceByName(ctx, agent.Name, version, firstRestartID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if current.UID != agent.UID || current.Generation != startGeneration+1 || current.Spec.RestartRequestID != firstRestartID ||
		current.Status.ObservedRestartRequestID != "" {
		t.Fatal("served API did not persist a pending restart on the existing Agent")
	}
	retried, err := NewClient(kc).RestartInstanceByName(ctx, agent.Name, version, firstRestartID, nil)
	if err != nil || retried.ResourceVersion != current.ResourceVersion || retried.Generation != current.Generation {
		t.Fatalf("Portal restart retry wrote a second request: %v", err)
	}
	current.Spec.Suspended = true
	if err := kc.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(kc).RestartInstanceByName(ctx, agent.Name, version, firstRestartID, nil); !errors.Is(err, ErrInstanceSuspended) {
		t.Fatalf("duplicate bypassed suspension: %v", err)
	}
	for _, id := range []string{secondRestartID, "", "invalid"} {
		change := current.DeepCopy()
		change.Spec.RestartRequestID = id
		if err := kc.Update(ctx, change, client.FieldValidation(metav1.FieldValidationStrict)); !apierrors.IsInvalid(err) {
			t.Fatalf("served validation accepted restart ID %q during suspension: %v", id, err)
		}
	}
	t.Run("owner suspension uses conditional existing intent", func(t *testing.T) {
		for _, suspended := range []bool{false, true} {
			version := InstanceVersion{UID: current.UID, ResourceVersion: current.ResourceVersion}
			before := current.DeepCopy()
			updated, err := NewClient(kc).SetInstanceSuspendedByName(ctx, current.Name, version, suspended, nil)
			if err != nil {
				t.Fatal(err)
			}
			current = updated
			if current.Spec.Suspended != suspended || current.Generation != before.Generation+1 ||
				current.Spec.RestartRequestID != firstRestartID || current.Status.Phase != before.Status.Phase ||
				current.Status.ObservedGeneration != before.Status.ObservedGeneration {
				t.Fatal("conditional suspension changed the restart or claimed controller completion")
			}
			retry, err := NewClient(kc).SetInstanceSuspendedByName(ctx, current.Name, version, suspended, nil)
			if err != nil || retry.ResourceVersion != current.ResourceVersion || retry.Generation != current.Generation {
				t.Fatalf("retry created another write: %v", err)
			}
			if _, err := NewClient(kc).SetInstanceSuspendedByName(ctx, current.Name, version, !suspended, nil); !errors.Is(err, ErrInstanceChanged) {
				t.Fatalf("stale version reversed intent: %v", err)
			}
			replaced := version
			replaced.UID = "replaced-agent"
			if _, err := NewClient(kc).SetInstanceSuspendedByName(ctx, current.Name, replaced, suspended, nil); !errors.Is(err, ErrInstanceChanged) {
				t.Fatalf("same-name replacement accepted: %v", err)
			}
		}
	})
	t.Run("proven pre-bootstrap suspension uses only the Agent intent", func(t *testing.T) {
		preBootstrap := sampleInstance()
		preBootstrap.Name, preBootstrap.UID, preBootstrap.ResourceVersion, preBootstrap.Generation = "prebootstrap-suspension", "", "", 0
		preBootstrap.Spec.SandboxRef = "sample-sandbox"
		preBootstrap.Finalizers = []string{api.Finalizer, api.SandboxHoldFinalizer}
		preBootstrap.Status = api.AgentStatus{}
		if err := kc.Create(ctx, preBootstrap, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
			t.Fatal(err)
		}
		preBootstrap.Status = api.AgentStatus{Phase: api.PhaseFailed, Policy: &api.PolicyStatus{}}
		if err := kc.Status().Update(ctx, preBootstrap); err != nil {
			t.Fatal(err)
		}
		version := InstanceVersion{UID: preBootstrap.UID, ResourceVersion: preBootstrap.ResourceVersion}
		updated, err := NewClient(kc).SetInstanceSuspendedByName(ctx, preBootstrap.Name, version, true, nil)
		if err != nil || !updated.Spec.Suspended || updated.Generation != preBootstrap.Generation+1 ||
			updated.Status.Phase != api.PhaseFailed || updated.Status.Policy == nil {
			t.Fatalf("served pre-bootstrap suspension did not preserve the failed diagnostic: %#v, %v", updated, err)
		}
		if err := kc.Get(ctx, client.ObjectKey{Name: InstanceNamespace(preBootstrap.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
			t.Fatalf("Portal pre-bootstrap suspension wrote a namespace: %v", err)
		}
		retry, err := NewClient(kc).SetInstanceSuspendedByName(ctx, preBootstrap.Name, version, true, nil)
		if err != nil || retry.ResourceVersion != updated.ResourceVersion || retry.Generation != updated.Generation {
			t.Fatalf("pre-bootstrap suspend retry wrote a second intent: %v", err)
		}
		if _, err := NewClient(kc).SetInstanceSuspendedByName(ctx, preBootstrap.Name, version, false, nil); !errors.Is(err, ErrInstanceChanged) {
			t.Fatalf("stale pre-bootstrap request reversed the intent: %v", err)
		}
	})
	t.Run("owner suspension tolerates a status-only version change", func(t *testing.T) {
		statusOnly := sampleInstance()
		statusOnly.Name, statusOnly.UID, statusOnly.ResourceVersion, statusOnly.Generation = "status-only-suspension", "", "", 0
		statusOnly.Spec.SandboxRef = "sample-sandbox"
		statusOnly.Finalizers = []string{api.Finalizer, api.SandboxHoldFinalizer}
		if err := kc.Create(ctx, statusOnly, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
			t.Fatal(err)
		}
		statusOnly.Status = api.AgentStatus{Phase: api.PhaseFailed, Policy: &api.PolicyStatus{}}
		if err := kc.Status().Update(ctx, statusOnly); err != nil {
			t.Fatal(err)
		}
		version := InstanceVersion{UID: statusOnly.UID, ResourceVersion: statusOnly.ResourceVersion, Generation: statusOnly.Generation}
		statusOnly.Status.Phase = api.PhasePending
		if err := kc.Status().Update(ctx, statusOnly); err != nil {
			t.Fatal(err)
		}
		if statusOnly.ResourceVersion == version.ResourceVersion || statusOnly.Generation != version.Generation {
			t.Fatal("served status update did not change only resourceVersion")
		}
		updated, err := NewClient(kc).SetInstanceSuspendedByName(ctx, statusOnly.Name, version, true, nil)
		if err != nil || !updated.Spec.Suspended || updated.Status.Phase != api.PhasePending || updated.Generation != version.Generation+1 {
			t.Fatalf("served status-only update blocked suspension: %#v, %v", updated, err)
		}
	})
	t.Run("owner suspension retries one served status-only patch conflict", func(t *testing.T) {
		duringPatch := sampleInstance()
		duringPatch.Name, duringPatch.UID, duringPatch.ResourceVersion, duringPatch.Generation = "status-conflict-suspension", "", "", 0
		duringPatch.Spec.SandboxRef = "sample-sandbox"
		duringPatch.Finalizers = []string{api.Finalizer, api.SandboxHoldFinalizer}
		if err := kc.Create(ctx, duringPatch, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
			t.Fatal(err)
		}
		duringPatch.Status = api.AgentStatus{Phase: api.PhaseFailed, Policy: &api.PolicyStatus{}}
		if err := kc.Status().Update(ctx, duringPatch); err != nil {
			t.Fatal(err)
		}
		version := InstanceVersion{UID: duringPatch.UID, ResourceVersion: duringPatch.ResourceVersion, Generation: duringPatch.Generation}
		patches := 0
		wrapped := interceptor.NewClient(watchClient, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			if patches == 1 {
				current := &api.Agent{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
					return err
				}
				current.Status.Phase = api.PhasePending
				if err := c.Status().Update(ctx, current); err != nil {
					return err
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		}})
		updated, err := NewClient(wrapped).SetInstanceSuspendedByName(ctx, duringPatch.Name, version, true, nil)
		if err != nil || !updated.Spec.Suspended || updated.Generation != version.Generation+1 || patches != 2 {
			t.Fatalf("served status conflict did not perform one safe retry: patches=%d updated=%#v err=%v", patches, updated, err)
		}
		retry, err := NewClient(wrapped).SetInstanceSuspendedByName(ctx, duringPatch.Name, version, true, nil)
		if err != nil || retry.Generation != updated.Generation || retry.ResourceVersion != updated.ResourceVersion || patches != 2 {
			t.Fatalf("same-intent retry wrote after served suspension: patches=%d retry=%#v err=%v", patches, retry, err)
		}
		if _, err := NewClient(wrapped).SetInstanceSuspendedByName(ctx, duringPatch.Name, version, false, nil); !errors.Is(err, ErrInstanceChanged) || patches != 2 {
			t.Fatalf("old generation reversed the accepted intent: patches=%d err=%v", patches, err)
		}
	})
	t.Run("owner suspension rejects a served specification change", func(t *testing.T) {
		specChanged := sampleInstance()
		specChanged.Name, specChanged.UID, specChanged.ResourceVersion, specChanged.Generation = "spec-change-suspension", "", "", 0
		specChanged.Spec.SandboxRef = "sample-sandbox"
		specChanged.Finalizers = []string{api.Finalizer, api.SandboxHoldFinalizer}
		if err := kc.Create(ctx, specChanged, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
			t.Fatal(err)
		}
		specChanged.Status = api.AgentStatus{Phase: api.PhaseFailed, Policy: &api.PolicyStatus{}}
		if err := kc.Status().Update(ctx, specChanged); err != nil {
			t.Fatal(err)
		}
		version := InstanceVersion{UID: specChanged.UID, ResourceVersion: specChanged.ResourceVersion, Generation: specChanged.Generation}
		specChanged.Spec.User.DisplayName = "Concurrent update"
		if err := kc.Update(ctx, specChanged, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
			t.Fatal(err)
		}
		if specChanged.Generation != version.Generation+1 {
			t.Fatal("served specification update did not increment generation")
		}
		if _, err := NewClient(kc).SetInstanceSuspendedByName(ctx, specChanged.Name, version, true, nil); !errors.Is(err, ErrInstanceChanged) {
			t.Fatalf("served specification change was accepted: %v", err)
		}
	})
	if err := kc.Delete(ctx, current, client.Preconditions{UID: &current.UID, ResourceVersion: &current.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
}
