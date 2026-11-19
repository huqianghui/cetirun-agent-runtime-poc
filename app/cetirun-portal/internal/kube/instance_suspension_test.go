// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func suspensionWorkload(agent *api.Agent) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: chataccess.ServiceName(agent.Name), Namespace: InstanceNamespace(agent.Name),
			UID: "workload-uid",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(),
				Kind: "Agent", Name: agent.Name, UID: agent.UID, Controller: ptr.To(true)}},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](1),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "cetirun",
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/home/vibe"}}}},
				Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "home"},
				}}},
			}},
		},
	}
}

func TestOwnerSuspensionRejectsEphemeralHome(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		t.Run(string(kind), func(t *testing.T) {
			agent := restartableInstance()
			agent.Spec.Agent.Type = kind
			workload := suspensionWorkload(agent)
			workload.Spec.Template.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(agent, workload).Build()
			if err := fc.Get(context.Background(), client.ObjectKeyFromObject(agent), agent); err != nil {
				t.Fatal(err)
			}
			before := agent.DeepCopy()
			_, err := NewClient(fc).SetInstanceSuspendedByName(context.Background(), agent.Name,
				InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}, true, nil)
			if err == nil {
				t.Fatal("retained suspension accepted emptyDir HOME")
			}
			if err := fc.Get(context.Background(), client.ObjectKeyFromObject(agent), agent); err != nil ||
				!reflect.DeepEqual(agent.Spec, before.Spec) {
				t.Fatal("rejected suspension changed intent:", err)
			}
		})
	}
}

func TestOwnerSuspensionAllowsProvenPreBootstrapSandbox(t *testing.T) {
	ctx := context.Background()
	agent := restartableInstance()
	agent.Spec.SandboxRef = "sample-sandbox"
	// This mirrors the failed governed bootstrap: policy diagnostics and its
	// hold finalizer may exist, but no runtime lifetime was created.
	agent.Status = api.AgentStatus{Phase: api.PhaseFailed, Policy: &api.PolicyStatus{}}
	agent.Finalizers = []string{api.Finalizer, api.SandboxHoldFinalizer}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(agent).Build()
	if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	version := InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}
	updated, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name, version, true, nil)
	if err != nil || !updated.Spec.Suspended {
		t.Fatalf("proven pre-bootstrap suspension failed: %v", err)
	}
	for index, makeState := range []func(*api.Agent) client.Object{
		func(agent *api.Agent) client.Object {
			return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: InstanceNamespace(agent.Name)}}
		},
		func(agent *api.Agent) client.Object {
			return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-system", Name: sandboxBootstrapReceiptName(agent.UID)}}
		},
		func(agent *api.Agent) client.Object {
			return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-system", Name: sandboxPreparationReceiptName(agent.UID)}}
		},
		func(agent *api.Agent) client.Object {
			return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-system", Name: sandboxGrantJournalName(agent.UID)}}
		},
	} {
		fresh := restartableInstance()
		fresh.Name = fmt.Sprintf("alice-%d", index)
		fresh.UID = types.UID(fmt.Sprintf("agent-uid-%d", index))
		fresh.Spec.SandboxRef = "sample-sandbox"
		fresh.Status = api.AgentStatus{Phase: api.PhaseFailed, Policy: &api.PolicyStatus{}}
		fresh.Finalizers = []string{api.Finalizer, api.SandboxHoldFinalizer}
		state := makeState(fresh)
		if err := fc.Create(ctx, state); err != nil {
			t.Fatal(err)
		}
		if err := fc.Create(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		if err := fc.Get(ctx, client.ObjectKeyFromObject(fresh), fresh); err != nil {
			t.Fatal(err)
		}
		_, err := NewClient(fc).SetInstanceSuspendedByName(ctx, fresh.Name, InstanceVersion{UID: fresh.UID, ResourceVersion: fresh.ResourceVersion}, true, nil)
		if !errors.Is(err, ErrSuspensionHomeUnverified) {
			t.Fatalf("state %T allowed pre-bootstrap suspension: %v", state, err)
		}
		if err := fc.Delete(ctx, state); err != nil {
			t.Fatal(err)
		}
		if err := fc.Delete(ctx, fresh); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOwnerSuspensionRejectsPreBootstrapHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		observe func(*api.Agent)
	}{
		{"enforced Sandbox", func(agent *api.Agent) { agent.Status.Sandbox = &api.SandboxStatus{} }},
		{"Helm handoff", func(agent *api.Agent) { agent.Status.HelmMaintenance = &api.HelmMaintenanceStatus{} }},
		{"registry credentials", func(agent *api.Agent) { agent.Status.RegistryCredentials = &api.RegistryCredentialsStatus{} }},
		{"ready phase", func(agent *api.Agent) { agent.Status.Phase = api.PhaseReady }},
		{"ready condition", func(agent *api.Agent) {
			agent.Status.Conditions = []metav1.Condition{{Type: api.ConditionSandboxReady, Status: metav1.ConditionTrue}}
		}},
		{"deployed chart", func(agent *api.Agent) {
			agent.Status.Conditions = []metav1.Condition{{Type: api.ConditionHelmDeployed, Status: metav1.ConditionTrue}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := restartableInstance()
			agent.Spec.SandboxRef = "sample-sandbox"
			agent.Status = api.AgentStatus{Policy: &api.PolicyStatus{}}
			tc.observe(agent)
			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(agent).Build()
			if err := NewClient(fc).CheckSuspension(context.Background(), agent); !errors.Is(err, ErrSuspensionHomeUnverified) {
				t.Fatalf("historical execution evidence allowed pre-bootstrap suspension: %v", err)
			}
		})
	}
}

func TestSuspensionHomeRequiresTheOwnedEffectiveMount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*appsv1.StatefulSet)
		want   error
	}{
		{"existing claim", func(_ *appsv1.StatefulSet) {}, nil},
		{"claim template", func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.Volumes = nil
			s.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}}}
		}, nil},
		{"empty claim", func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = ""
		}, ErrSuspensionHomeRequired},
		{"unmounted persistent data", func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name = "ephemeral"
			s.Spec.Template.Spec.Volumes = append(s.Spec.Template.Spec.Volumes, corev1.Volume{Name: "ephemeral",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}, ErrSuspensionHomeRequired},
		{"unknown HOME mount", func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.Containers[0].VolumeMounts = nil
		}, ErrSuspensionHomeUnverified},
		{"replaced Agent", func(s *appsv1.StatefulSet) {
			s.OwnerReferences[0].UID = "previous-agent"
		}, ErrSuspensionHomeUnverified},
		{"unowned workload", func(s *appsv1.StatefulSet) { s.OwnerReferences = nil }, ErrSuspensionHomeUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := restartableInstance()
			workload := suspensionWorkload(agent)
			tc.change(workload)
			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(agent, workload).Build()
			err := NewClient(fc).CheckSuspensionHome(context.Background(), agent)
			if !errors.Is(err, tc.want) {
				t.Fatalf("effective HOME check=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestOwnerSuspensionPreservesPendingOperationsAndRetainedResources(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes, api.AgentTypeCustom} {
		for _, suspended := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/suspended=%t", kind, suspended), func(t *testing.T) {
				ctx := context.Background()
				agent := restartableInstance()
				agent.Spec.Agent.Type, agent.Spec.Suspended = kind, !suspended
				agent.Spec.RestartRequestID = firstRestartID
				agent.Spec.PlannedMigration = &api.PlannedMigrationRequest{RequestID: "migration-00000001",
					Target: api.MigrationObject{Name: "destination", UID: "destination-uid"}}
				agent.Status.PlannedMigration = &api.PlannedMigrationStatus{
					PlannedMigrationRequest: *agent.Spec.PlannedMigration, Phase: api.MigrationStarting}
				home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: "cetirun-alice", UID: "home-uid"}}
				key := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "model-key", Namespace: "cetirun-alice", UID: "key-uid"},
					Data: map[string][]byte{"key": []byte("synthetic-model-key")}}
				writes := 0
				lost := errors.New("synthetic response lost after persistence")
				fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().
					WithStatusSubresource(&api.Agent{}).WithObjects(agent, home, key, suspensionWorkload(agent)).
					WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						writes++
						if _, ok := obj.(*api.Agent); !ok {
							t.Fatalf("suspension wrote %T instead of only the Agent", obj)
						}
						if err := c.Patch(ctx, obj, patch, opts...); err != nil {
							return err
						}
						return lost
					}}).Build()
				for _, obj := range []client.Object{agent, home, key} {
					if err := fc.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
						t.Fatal(err)
					}
				}
				before := agent.DeepCopy()
				version := InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}
				if _, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name, version, suspended, nil); !errors.Is(err, lost) {
					t.Fatalf("uncertain write was not surfaced: %v", err)
				}
				current, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name, version, suspended, nil)
				if err != nil || writes != 1 {
					t.Fatalf("retry wrote another operation: writes=%d err=%v", writes, err)
				}
				wantSpec := before.Spec.DeepCopy()
				wantSpec.Suspended = suspended
				if current.UID != before.UID || !reflect.DeepEqual(current.Spec, *wantSpec) ||
					!reflect.DeepEqual(current.Status, before.Status) || !reflect.DeepEqual(current.Annotations, before.Annotations) {
					t.Fatal("changed retained identity, unrelated intent or observed status")
				}
				if CanSuspendInstance(current) == suspended || CanResumeInstance(current) != suspended || CanRestartInstance(current) {
					t.Fatal("capabilities must permit reversing suspension, not another pending restart")
				}
				for _, obj := range []client.Object{home, key} {
					want := obj.DeepCopyObject()
					if err := fc.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil || !reflect.DeepEqual(want, obj) {
						t.Fatalf("retained HOME/key changed: %v", err)
					}
				}
				current.Spec.Suspended = !suspended
				if err := fc.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
				if _, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name, version, suspended, nil); !errors.Is(err, ErrInstanceChanged) || writes != 1 {
					t.Fatalf("late retry reversed newer intent: writes=%d err=%v", writes, err)
				}
			})
		}
	}
}

func TestOwnerSuspensionRejectsUnsafeRequests(t *testing.T) {
	denied := errors.New("synthetic owner denial")
	for _, tc := range []struct {
		name      string
		change    func(*api.Agent, *InstanceVersion)
		authorize func(*api.Agent) error
		want      error
	}{
		{"missing UID", func(_ *api.Agent, v *InstanceVersion) { v.UID = "" }, nil, ErrInstanceVersionRequired},
		{"missing version", func(_ *api.Agent, v *InstanceVersion) { v.ResourceVersion = "" }, nil, ErrInstanceVersionRequired},
		{"negative generation", func(_ *api.Agent, v *InstanceVersion) { v.Generation = -1 }, nil, ErrInstanceVersionRequired},
		{"replaced Agent", func(_ *api.Agent, v *InstanceVersion) { v.UID = "old-uid" }, nil, ErrInstanceChanged},
		{"stale version", func(_ *api.Agent, v *InstanceVersion) { v.ResourceVersion = "old-version" }, nil, ErrInstanceChanged},
		{"foreign owner", nil, func(*api.Agent) error { return denied }, denied},
		{"foreign duplicate", func(a *api.Agent, _ *InstanceVersion) { a.Spec.Suspended = true }, func(*api.Agent) error { return denied }, denied},
		{"deleting duplicate", func(a *api.Agent, _ *InstanceVersion) {
			now := metav1.Now()
			a.Spec.Suspended, a.Finalizers, a.DeletionTimestamp = true, []string{api.Finalizer}, &now
		}, nil, ErrInstanceDeleting},
		{"legacy single-start", func(a *api.Agent, _ *InstanceVersion) {
			a.Finalizers = []string{api.SandboxPreparationFinalizer}
		}, nil, ErrSuspensionUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := restartableInstance()
			agent.ResourceVersion = "7"
			version := InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}
			if tc.change != nil {
				tc.change(agent, &version)
			}
			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(agent).Build()
			_, err := NewClient(fc).SetInstanceSuspendedByName(context.Background(), agent.Name, version, true, tc.authorize)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			current := &api.Agent{}
			if err := fc.Get(context.Background(), client.ObjectKeyFromObject(agent), current); err != nil ||
				!reflect.DeepEqual(current.Spec, agent.Spec) || current.ResourceVersion != agent.ResourceVersion {
				t.Fatalf("rejected request wrote the Agent: %v", err)
			}
			if errors.Is(tc.want, ErrInstanceDeleting) || errors.Is(tc.want, ErrSuspensionUnsupported) {
				if CanSuspendInstance(current) || CanResumeInstance(current) {
					t.Fatal("unavailable lifecycle action was advertised")
				}
			}
		})
	}
}

func TestOwnerSuspensionAcceptsStatusOnlyVersionChanges(t *testing.T) {
	ctx := context.Background()
	agent := restartableInstance()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().
		WithStatusSubresource(&api.Agent{}).WithObjects(agent, suspensionWorkload(agent)).Build()
	if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	version := InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion, Generation: agent.Generation}
	statusOnly := agent.DeepCopy()
	statusOnly.Status.Phase = api.PhasePending
	if err := fc.Status().Update(ctx, statusOnly); err != nil {
		t.Fatal(err)
	}
	if statusOnly.ResourceVersion == version.ResourceVersion {
		t.Fatal("status update did not advance resourceVersion")
	}
	updated, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name, version, true, nil)
	if err != nil || !updated.Spec.Suspended || updated.Generation != version.Generation {
		t.Fatalf("status-only update rejected suspension: %#v, %v", updated, err)
	}
}

func TestOwnerSuspensionRetriesOneStatusOnlyPatchConflict(t *testing.T) {
	ctx := context.Background()
	agent := restartableInstance()
	patches := 0
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().
		WithStatusSubresource(&api.Agent{}).WithObjects(agent, suspensionWorkload(agent)).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
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
		}}).Build()
	if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	updated, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name,
		InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion, Generation: agent.Generation}, true, nil)
	if err != nil || !updated.Spec.Suspended || patches != 2 {
		t.Fatalf("status-only patch conflict was not retried once: patches=%d updated=%#v err=%v", patches, updated, err)
	}
}

func TestOwnerSuspensionGenerationRejectsConcurrentSpecificationChange(t *testing.T) {
	ctx := context.Background()
	agent := restartableInstance()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().
		WithObjects(agent, suspensionWorkload(agent)).Build()
	if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	version := InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion, Generation: agent.Generation}
	concurrent := agent.DeepCopy()
	concurrent.Spec.User.DisplayName = "Concurrent update"
	concurrent.Generation++
	if err := fc.Update(ctx, concurrent); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name, version, true, nil); !errors.Is(err, ErrInstanceChanged) {
		t.Fatalf("concurrent specification change was accepted: %v", err)
	}
	current := &api.Agent{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil || current.Spec.Suspended || current.Spec.User.DisplayName != "Concurrent update" {
		t.Fatalf("rejected request overwrote concurrent specification: %#v, %v", current, err)
	}
}

func TestOwnerSuspensionGenerationDoesNotRetryUnsafePatchConflicts(t *testing.T) {
	denied := errors.New("synthetic owner denial")
	for _, tc := range []struct {
		name      string
		prepare   func(*api.Agent)
		change    func(context.Context, client.WithWatch, *api.Agent, *appsv1.StatefulSet) error
		authorize func(*api.Agent) error
		want      error
	}{
		{
			name: "specification", want: ErrInstanceChanged,
			change: func(ctx context.Context, c client.WithWatch, current *api.Agent, _ *appsv1.StatefulSet) error {
				current.Spec.User.DisplayName = "Concurrent update"
				current.Generation++
				return c.Update(ctx, current)
			},
		},
		{
			name: "deletion", want: ErrInstanceDeleting,
			prepare: func(agent *api.Agent) { agent.Finalizers = []string{api.Finalizer} },
			change: func(ctx context.Context, c client.WithWatch, current *api.Agent, _ *appsv1.StatefulSet) error {
				return c.Delete(ctx, current)
			},
		},
		{
			name: "replacement", want: ErrInstanceChanged,
			change: func(ctx context.Context, c client.WithWatch, current *api.Agent, _ *appsv1.StatefulSet) error {
				if err := c.Delete(ctx, current); err != nil {
					return err
				}
				replacement := current.DeepCopy()
				replacement.UID, replacement.ResourceVersion = "replacement-uid", ""
				return c.Create(ctx, replacement)
			},
		},
		{
			name: "owner authorization", want: denied,
			change: func(ctx context.Context, c client.WithWatch, current *api.Agent, _ *appsv1.StatefulSet) error {
				if current.Annotations == nil {
					current.Annotations = map[string]string{}
				}
				current.Annotations["test.cetirun.microsoft.com/owner"] = "denied"
				return c.Update(ctx, current)
			},
			authorize: func(agent *api.Agent) error {
				if agent.Annotations["test.cetirun.microsoft.com/owner"] == "denied" {
					return denied
				}
				return nil
			},
		},
		{
			name: "HOME eligibility", want: ErrSuspensionHomeRequired,
			change: func(ctx context.Context, c client.WithWatch, _ *api.Agent, workload *appsv1.StatefulSet) error {
				workload.Spec.Template.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
				if err := c.Update(ctx, workload); err != nil {
					return err
				}
				return apierrors.NewConflict(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "agents"}, workload.Name,
					errors.New("synthetic status conflict"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			agent := restartableInstance()
			if tc.prepare != nil {
				tc.prepare(agent)
			}
			workload := suspensionWorkload(agent)
			patches := 0
			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().
				WithObjects(agent, workload).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				current := &api.Agent{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
					return err
				}
				storedWorkload := &appsv1.StatefulSet{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(workload), storedWorkload); err != nil {
					return err
				}
				if err := tc.change(ctx, c, current, storedWorkload); err != nil {
					return err
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}).Build()
			if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
				t.Fatal(err)
			}
			_, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name,
				InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion, Generation: agent.Generation}, true, tc.authorize)
			if !errors.Is(err, tc.want) || patches != 1 {
				t.Fatalf("unsafe conflict retried or accepted: patches=%d err=%v", patches, err)
			}
		})
	}
}

func TestOwnerSuspensionGenerationStopsAfterTwoStatusConflicts(t *testing.T) {
	ctx := context.Background()
	agent := restartableInstance()
	patches := 0
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().
		WithStatusSubresource(&api.Agent{}).WithObjects(agent, suspensionWorkload(agent)).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			current := &api.Agent{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
				return err
			}
			if patches%2 == 0 {
				current.Status.Phase = api.PhaseReady
			} else {
				current.Status.Phase = api.PhasePending
			}
			if err := c.Status().Update(ctx, current); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		}}).Build()
	if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	_, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name,
		InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion, Generation: agent.Generation}, true, nil)
	if !apierrors.IsConflict(err) || patches != 2 {
		t.Fatalf("sustained status conflicts retried more than once: patches=%d err=%v", patches, err)
	}
}

func TestOwnerSuspensionDoesNotRetryConcurrentChanges(t *testing.T) {
	for _, suspended := range []bool{true, false} {
		for _, deleting := range []bool{false, true} {
			t.Run(fmt.Sprintf("suspended=%t/deleting=%t", suspended, deleting), func(t *testing.T) {
				ctx := context.Background()
				agent := restartableInstance()
				agent.Spec.Suspended, agent.Finalizers = !suspended, []string{api.Finalizer}
				writes := 0
				fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().WithObjects(agent, suspensionWorkload(agent)).
					WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						writes++
						current := &api.Agent{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
							return err
						}
						if deleting {
							if err := c.Delete(ctx, current); err != nil {
								return err
							}
						} else {
							current.Spec.User.DisplayName = "Concurrent update"
							if err := c.Update(ctx, current); err != nil {
								return err
							}
						}
						return c.Patch(ctx, obj, patch, opts...)
					}}).Build()
				if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
					t.Fatal(err)
				}
				_, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name,
					InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}, suspended, nil)
				if !apierrors.IsConflict(err) || writes != 1 {
					t.Fatalf("concurrent change was retried or overwritten: writes=%d err=%v", writes, err)
				}
			})
		}
	}
}

func TestOwnerResumeDoesNotRequireMigrationCanStartOrObservedGeneration(t *testing.T) {
	for _, phase := range []string{"", api.MigrationProtecting, api.MigrationStopping, api.MigrationSourceTerminated, api.MigrationDetached, api.MigrationStarting, api.MigrationCompleted} {
		t.Run(phase, func(t *testing.T) {
			agent := restartableInstance()
			agent.Spec.Suspended, agent.ResourceVersion = true, "7"
			agent.Spec.PlannedMigration = &api.PlannedMigrationRequest{RequestID: "migration-00000001"}
			if phase != "" {
				agent.Status.PlannedMigration = &api.PlannedMigrationStatus{
					PlannedMigrationRequest: *agent.Spec.PlannedMigration, Phase: phase}
			}
			agent.Status.Phase, agent.Status.ObservedGeneration = api.PhaseSuspended, 0
			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(agent).Build()
			current, err := NewClient(fc).SetInstanceSuspendedByName(context.Background(), agent.Name,
				InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}, false, nil)
			if err != nil || current.Spec.Suspended || !reflect.DeepEqual(current.Spec.PlannedMigration, agent.Spec.PlannedMigration) ||
				!reflect.DeepEqual(current.Status, agent.Status) || CanRestartInstance(current) {
				t.Fatalf("resume stranded or advanced the original operation: %v", err)
			}
		})
	}
}

func TestOwnerResumeAdmissionFailureKeepsSuspensionAndFailedStatus(t *testing.T) {
	ctx := context.Background()
	agent := restartableInstance()
	agent.Spec.Suspended, agent.ResourceVersion = true, "7"
	agent.Spec.RestartRequestID = firstRestartID
	agent.Status.Phase, agent.Status.ObservedGeneration = api.PhaseFailed, 0
	agent.Status.Conditions[0].Status = metav1.ConditionFalse
	writes := 0
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(agent).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			writes++
			return apierrors.NewForbidden(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "agents"},
				agent.Name, errors.New("synthetic current authorization denial"))
		}}).Build()
	if !CanResumeInstance(agent) {
		t.Fatal("failed status prevented a versioned resume attempt")
	}
	_, err := NewClient(fc).SetInstanceSuspendedByName(ctx, agent.Name,
		InstanceVersion{UID: agent.UID, ResourceVersion: agent.ResourceVersion}, false, nil)
	if !apierrors.IsForbidden(err) || writes != 1 {
		t.Fatalf("admission rejection was hidden or retried: writes=%d err=%v", writes, err)
	}
	current := &api.Agent{}
	if err := fc.Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil ||
		!reflect.DeepEqual(current.Spec, agent.Spec) || !reflect.DeepEqual(current.Status, agent.Status) {
		t.Fatalf("rejected resume changed intent or claimed recovery: %v", err)
	}
}
