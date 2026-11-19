// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const migrationTestDriver = "csi.example.com"

func newMigrationFixture(t *testing.T, kind string, coordinated bool, setup ...func(*AgentReconciler, client.Client)) *nativeSandboxFixture {
	t.Helper()
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false, setup...)
	f.r.SandboxPreparation.PlannedMigration = true
	ctx := context.Background()
	workload := f.workload(t)
	workload.Generation = 1
	if err := f.kube.Update(ctx, workload); err != nil {
		t.Fatal(err)
	}
	home := &corev1.PersistentVolumeClaim{}
	expected := reconciledExpectationForTest(t, f)
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Home.Name}, home); err != nil {
		t.Fatal(err)
	}
	home.Spec.VolumeName = "synthetic-home-volume"
	if err := f.kube.Update(ctx, home); err != nil {
		t.Fatal(err)
	}
	home.Status.Phase = corev1.ClaimBound
	if err := f.kube.Status().Update(ctx, home); err != nil {
		t.Fatal(err)
	}
	volume := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: home.Spec.VolumeName, UID: "home-volume-uid"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Name: home.Name, Namespace: home.Namespace, UID: home.UID},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: migrationTestDriver, VolumeHandle: "synthetic-volume-handle"}},
			NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"example-zone"}}},
			}}}},
		}, Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound}}
	if err := f.kube.Create(ctx, volume); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Create(ctx, &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: migrationTestDriver, UID: "driver-uid"},
		Spec: storagev1.CSIDriverSpec{AttachRequired: ptr.To(true)}}); err != nil {
		t.Fatal(err)
	}
	runtimeClass := &nodev1.RuntimeClass{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: *workload.Spec.Template.Spec.RuntimeClassName}, runtimeClass); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"node-a", "node-b"} {
		labels := maps.Clone(workload.Spec.Template.Spec.NodeSelector)
		if labels == nil {
			labels = map[string]string{}
		}
		if runtimeClass.Scheduling != nil {
			maps.Copy(labels, runtimeClass.Scheduling.NodeSelector)
		}
		labels["topology.kubernetes.io/zone"] = "example-zone"
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Labels: labels},
			Spec: corev1.NodeSpec{ProviderID: "provider://example.test/" + name},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: name + "-boot"},
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: corev1.NamespaceNodeLease,
			UID: types.UID(name + "-lease"), OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: name, UID: node.UID}}},
			Spec: coordinationv1.LeaseSpec{LeaseDurationSeconds: ptr.To[int32](40), RenewTime: &metav1.MicroTime{Time: time.Now()}}}
		csi := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-csi")},
			Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: migrationTestDriver, NodeID: name + "-storage"}}}}
		for _, object := range []client.Object{node, lease, csi} {
			if err := f.kube.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
	}
	migrationReadyPod(t, f, "source-pod", "node-a")
	migrationAttachment(t, f, "source-attachment", "node-a")
	migrationWorkloadStatus(t, f, 1)
	for range 3 {
		f.reconcileOK(t)
		migrationWorkloadStatus(t, f, 1)
	}
	if f.current(t).Status.Phase != api.PhaseReady {
		t.Fatal("migration fixture is not ready")
	}
	if coordinated {
		withReconciledCoordination(t)(f.r, f.kube)
		if _, err := f.reconcile(); err != nil {
			t.Fatal("establish initial coordination:", err)
		}
	}
	for _, key := range f.gateway.keys {
		key.Spend = ptr.To(3.25)
	}
	return f
}

func migrationReadyPod(t *testing.T, f *nativeSandboxFixture, uid, node string) {
	t.Helper()
	workload := f.workload(t)
	pod := &corev1.Pod{ObjectMeta: *workload.Spec.Template.ObjectMeta.DeepCopy(), Spec: *workload.Spec.Template.Spec.DeepCopy()}
	pod.Name, pod.Namespace, pod.UID = workload.Name+"-0", workload.Namespace, types.UID(uid)
	pod.Spec.NodeName = node
	pod.Spec.TerminationGracePeriodSeconds = ptr.To[int64](30)
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := f.kube.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func migrationAttachment(t *testing.T, f *nativeSandboxFixture, name, node string) {
	t.Helper()
	attachment := &storagev1.VolumeAttachment{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"),
		Finalizers: []string{migrationAttacherFinalizer(migrationTestDriver)}},
		Spec: storagev1.VolumeAttachmentSpec{Attacher: migrationTestDriver, NodeName: node,
			Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: ptr.To("synthetic-home-volume")}},
		Status: storagev1.VolumeAttachmentStatus{Attached: true}}
	if err := f.kube.Create(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
}

func migrationWorkloadStatus(t *testing.T, f *nativeSandboxFixture, replicas int32) {
	t.Helper()
	workload := f.workload(t)
	workload.Status = appsv1.StatefulSetStatus{ObservedGeneration: workload.Generation, Replicas: replicas,
		ReadyReplicas: replicas, UpdatedReplicas: replicas, CurrentReplicas: replicas}
	if err := f.kube.Status().Update(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
}

func requestMigration(t *testing.T, f *nativeSandboxFixture, id, target string) {
	t.Helper()
	agent := f.current(t)
	agent.Spec.PlannedMigration = &api.PlannedMigrationRequest{RequestID: id,
		Target: api.MigrationObject{Name: target, UID: types.UID(target + "-uid")}}
	agent.Generation++
	if err := f.kube.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
}

func migrationPhase(t *testing.T, f *nativeSandboxFixture, want string) {
	t.Helper()
	state := f.current(t).Status.PlannedMigration
	if state == nil || state.Phase != want {
		t.Fatalf("migration phase = %+v, want %s", state, want)
	}
}

func acknowledgeMigrationSource(t *testing.T, f *nativeSandboxFixture) {
	t.Helper()
	ctx := context.Background()
	state := f.current(t).Status.PlannedMigration
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: state.Namespace.Name, Name: state.SourcePod.Name}
	if err := f.kube.Get(ctx, key, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Get(ctx, key, pod); err != nil {
		t.Fatal("source protection did not retain its termination observation:", err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.Conditions = nil
	terminated := func(containers []corev1.Container) []corev1.ContainerStatus {
		result := make([]corev1.ContainerStatus, 0, len(containers))
		for _, container := range containers {
			result = append(result, corev1.ContainerStatus{Name: container.Name,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Now()}}})
		}
		return result
	}
	pod.Status.ContainerStatuses = terminated(pod.Spec.Containers)
	pod.Status.InitContainerStatuses = terminated(pod.Spec.InitContainers)
	if err := f.kube.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	migrationWorkloadStatus(t, f, 0)
}

func acknowledgeMigrationDetach(t *testing.T, f *nativeSandboxFixture) {
	t.Helper()
	attachment := acknowledgeMigrationDetachFinalizer(t, f)
	attachment.Status.Attached = false
	attachment.Status.DetachError = nil
	if err := f.kube.Status().Update(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
}

func acknowledgeMigrationDetachFinalizer(t *testing.T, f *nativeSandboxFixture) *storagev1.VolumeAttachment {
	t.Helper()
	ctx := context.Background()
	state := f.current(t).Status.PlannedMigration
	attachment := &storagev1.VolumeAttachment{}
	key := client.ObjectKey{Name: state.Attachment.Name}
	if err := f.kube.Get(ctx, key, attachment); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, attachment); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Get(ctx, key, attachment); err != nil {
		t.Fatal("attachment protection did not retain its CSI observation:", err)
	}
	attachment.Finalizers = slices.DeleteFunc(attachment.Finalizers, func(s string) bool { return s == migrationAttacherFinalizer(state.Driver) })
	if err := f.kube.Update(ctx, attachment); err != nil {
		t.Fatal(err)
	}
	return attachment
}

func completeMigration(t *testing.T, f *nativeSandboxFixture, id, target string) {
	t.Helper()
	requestMigration(t, f, id, target)
	f.reconcileOK(t)
	migrationPhase(t, f, api.MigrationProtecting)
	finishMigration(t, f)
}

func finishMigration(t *testing.T, f *nativeSandboxFixture) {
	t.Helper()
	request := f.current(t).Spec.PlannedMigration
	id, target := request.RequestID, request.Target.Name
	advance := func() {
		t.Helper()
		before := f.current(t).Status.PlannedMigration
		if _, err := f.reconcile(); err != nil {
			workload := f.workload(t)
			t.Fatalf("advance migration %s from %+v: %v; replicas=%d status=%+v", id, before, err,
				ptr.Deref(workload.Spec.Replicas, -1), workload.Status)
		}
	}
	stages := f.installer.stages
	if f.current(t).Status.PlannedMigration.Phase == api.MigrationProtecting {
		advance()
	}
	migrationPhase(t, f, api.MigrationStopping)
	advance()
	if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || f.installer.stages != stages {
		t.Fatal("migration did not stop before replacement")
	}
	for _, key := range f.gateway.keys {
		if !key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
			t.Fatal("migration did not retain and block the original spend identity")
		}
	}
	acknowledgeMigrationSource(t, f)
	advance()
	migrationPhase(t, f, api.MigrationSourceTerminated)
	advance()
	acknowledgeMigrationDetach(t, f)
	advance()
	migrationPhase(t, f, api.MigrationDetached)
	advance()
	advance()
	migrationPhase(t, f, api.MigrationStarting)
	for range 3 {
		advance()
	}
	migrationPhase(t, f, api.MigrationStarting)
	if f.installer.stages != stages+1 {
		t.Fatalf("replacement applications = %d, want one", f.installer.stages-stages)
	}
	migrationReadyPod(t, f, id+"-pod", target)
	migrationAttachment(t, f, id+"-attachment", target)
	migrationWorkloadStatus(t, f, 1)
	advance()
	migrationPhase(t, f, api.MigrationCompleted)
	if f.current(t).Status.Phase != api.PhaseReady {
		t.Fatal("migration completed without current Ready")
	}
	for range 2 {
		advance()
	}
	if f.installer.stages != stages+1 {
		t.Fatal("unchanged migration repeated its workload apply")
	}
}

func TestPlannedMigrationRetainsHOMEAcrossRepeatedMoves(t *testing.T) {
	testPlannedMigrationRetainsHOME(t)
}

func testPlannedMigrationRetainsHOME(t *testing.T, setup ...func(*AgentReconciler, client.Client)) {
	testPlannedMigrationRetainsHOMEForHarnesses(t, []string{"openclaw", "hermes"}, setup...)
}

func testPlannedMigrationRetainsHOMEForHarnesses(t *testing.T, harnesses []string, setup ...func(*AgentReconciler, client.Client)) {
	for _, kind := range harnesses {
		for _, coordinated := range []bool{false, true} {
			for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
				t.Run(fmt.Sprintf("%s/coordination=%t/%s", kind, coordinated, mode), func(t *testing.T) {
					f := newMigrationFixture(t, kind, coordinated,
						append([]func(*AgentReconciler, client.Client){withNativeHomeAccessMode(t, mode)}, setup...)...)
					assertNativeHomeAccessMode(t, f, mode)
					initial := reconciledExpectationForTest(t, f)
					key := f.current(t).Status.AIGateway.KeyID
					completeMigration(t, f, "migration-00000001", "node-b")
					completeMigration(t, f, "migration-00000002", "node-a")
					current := reconciledExpectationForTest(t, f)
					if current.Home != initial.Home || current.Namespace != initial.Namespace || current.Agent != initial.Agent ||
						f.current(t).Status.AIGateway.KeyID != key || len(f.gateway.generated) != 1 {
						t.Fatal("migration replaced a retained identity")
					}
					for _, item := range f.gateway.keys {
						if item.Blocked || ptr.Deref(item.Spend, 0) != 3.25 {
							t.Fatal("migration reset spend or failed to release its own block")
						}
					}
					assertNativeHomeAccessMode(t, f, mode)
				})
			}
		}
	}
}

func TestPlannedMigrationWaitsForPositiveSourceEvidence(t *testing.T) {
	for _, fault := range []string{"lease", "node identity", "pod missing", "container running", "detach missing", "detach error"} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
			t.Run(fmt.Sprintf("%s/%s", fault, mode), func(t *testing.T) {
				f := newMigrationFixture(t, "openclaw", false, withNativeHomeAccessMode(t, mode))
				ctx := context.Background()
				requestMigration(t, f, "migration-00000001", "node-b")
				f.reconcileOK(t)
				f.reconcileOK(t)
				state := f.current(t).Status.PlannedMigration
				stages := f.installer.stages
				switch fault {
				case "lease":
					lease := &coordinationv1.Lease{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: state.Source.Name}, lease); err != nil {
						t.Fatal(err)
					}
					lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now().Add(-time.Hour)}
					if err := f.kube.Update(ctx, lease); err != nil {
						t.Fatal(err)
					}
				case "node identity":
					node := &corev1.Node{}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: state.Source.Name}, node); err != nil {
						t.Fatal(err)
					}
					node.Status.NodeInfo.BootID = "replacement-boot"
					if err := f.kube.Status().Update(ctx, node); err != nil {
						t.Fatal(err)
					}
				case "pod missing":
					pod := &corev1.Pod{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: state.Namespace.Name, Name: state.SourcePod.Name}, pod); err != nil {
						t.Fatal(err)
					}
					pod.Finalizers = nil
					if err := f.kube.Update(ctx, pod); err != nil {
						t.Fatal(err)
					}
					if err := f.kube.Delete(ctx, pod); err != nil {
						t.Fatal(err)
					}
				case "container running":
					acknowledgeMigrationSource(t, f)
					pod := &corev1.Pod{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: state.Namespace.Name, Name: state.SourcePod.Name}, pod); err != nil {
						t.Fatal(err)
					}
					pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
					if err := f.kube.Status().Update(ctx, pod); err != nil {
						t.Fatal(err)
					}
				default:
					acknowledgeMigrationSource(t, f)
					f.reconcileOK(t)
					f.reconcileOK(t)
					attachment := &storagev1.VolumeAttachment{}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: state.Attachment.Name}, attachment); err != nil {
						t.Fatal(err)
					}
					if fault == "detach missing" {
						attachment.Finalizers = nil
						if err := f.kube.Update(ctx, attachment); err != nil {
							t.Fatal(err)
						}
						if err := f.kube.Delete(ctx, attachment); err != nil {
							t.Fatal(err)
						}
					} else {
						attachment.Status.DetachError = &storagev1.VolumeError{Message: "synthetic detach rejection"}
						if err := f.kube.Status().Update(ctx, attachment); err != nil {
							t.Fatal(err)
						}
					}
				}
				_, err := f.reconcile()
				if fault != "container running" && err == nil {
					t.Fatal("unknown source evidence was not surfaced")
				}
				if f.current(t).Status.PlannedMigration.CanStart() || f.installer.stages != stages || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
					t.Fatal("uncertain source state permitted a replacement")
				}
			})
		}
	}
}

func TestPlannedMigrationCheckpointLossCannotSkipProtection(t *testing.T) {
	f := newMigrationFixture(t, "openclaw", false)
	requestMigration(t, f, "migration-00000001", "node-b")
	lost := errors.New("synthetic status acknowledgement lost")
	original := f.r.Client
	f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if err := c.SubResource(sub).Patch(ctx, object, patch, opts...); err != nil {
				return err
			}
			return lost
		},
	})
	if _, err := f.reconcile(); !errors.Is(err, lost) {
		t.Fatal("lost checkpoint response was hidden:", err)
	}
	migrationPhase(t, f, api.MigrationProtecting)
	state := f.current(t).Status.PlannedMigration
	pod := &corev1.Pod{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: state.Namespace.Name, Name: state.SourcePod.Name}, pod); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pod.Finalizers, api.MigrationPodFinalizer) || ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
		t.Fatal("an unacknowledged checkpoint performed later effects")
	}
	f.r.Client = original
	f.reconcileOK(t)
	migrationPhase(t, f, api.MigrationStopping)
	attachment := &storagev1.VolumeAttachment{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: state.Attachment.Name}, attachment); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(attachment.Finalizers, api.MigrationAttachmentFinalizer) {
		t.Fatal("recovery skipped attachment protection")
	}
}

func migrationPodRequest(t *testing.T, pod, old *corev1.Pod) admission.Request {
	t.Helper()
	pod, old = pod.DeepCopy(), old.DeepCopy()
	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	old.TypeMeta = pod.TypeMeta
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "migration-request", Name: pod.Name, Namespace: pod.Namespace, Operation: admissionv1.Update,
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Kind:     metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
		Object:   runtime.RawExtension{Raw: raw}, OldObject: runtime.RawExtension{Raw: previous},
	}}
}

func TestPlannedMigrationFinalizerAdmissionUsesTheCheckpoint(t *testing.T) {
	f := newMigrationFixture(t, "openclaw", false)
	requestMigration(t, f, "migration-00000001", "node-b")
	f.reconcileOK(t)
	handler, err := sandboxadmission.NewPodHandler(f.kube, sandboxadmission.PodConfig{StateNamespace: f.r.OperatorNamespace})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: f.workload(t).Namespace, Name: f.workload(t).Name + "-0"}
	if err := f.kube.Get(ctx, key, pod); err != nil {
		t.Fatal(err)
	}
	for _, changeWorkload := range []bool{false, true} {
		next := pod.DeepCopy()
		next.Finalizers = append(next.Finalizers, api.MigrationPodFinalizer)
		if changeWorkload {
			next.Spec.Containers[0].Image = "example.com/unapproved:1"
		}
		response := handler.Validate(ctx, migrationPodRequest(t, next, pod))
		if response.Allowed == changeWorkload {
			t.Fatalf("finalizer addition allowed=%v with workload change=%v: %+v", response.Allowed, changeWorkload, response.Result)
		}
	}
	f.reconcileOK(t)
	if err := f.kube.Get(ctx, key, pod); err != nil {
		t.Fatal(err)
	}
	next := pod.DeepCopy()
	next.Finalizers = slices.DeleteFunc(next.Finalizers, func(s string) bool { return s == api.MigrationPodFinalizer })
	if response := handler.Validate(ctx, migrationPodRequest(t, next, pod)); response.Allowed {
		t.Fatal("source protection was removable before termination acknowledgement")
	}
	acknowledgeMigrationSource(t, f)
	f.reconcileOK(t)
	if err := f.kube.Get(ctx, key, pod); err != nil {
		t.Fatal(err)
	}
	next = pod.DeepCopy()
	next.Finalizers = slices.DeleteFunc(next.Finalizers, func(s string) bool { return s == api.MigrationPodFinalizer })
	if response := handler.Validate(ctx, migrationPodRequest(t, next, pod)); !response.Allowed {
		t.Fatal("durably acknowledged source finalization was rejected:", response.Result)
	}
}

func TestPlannedMigrationBindingRechecksTheDestinationUID(t *testing.T) {
	f := newMigrationFixture(t, "openclaw", false)
	completeMigration(t, f, "migration-00000001", "node-b")
	handler, err := sandboxadmission.NewPodHandler(f.kube, sandboxadmission.PodConfig{StateNamespace: f.r.OperatorNamespace})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, legacy := range []bool{false, true} {
		request := migrationBindingRequest(t, f, "node-b", legacy)
		if response := handler.Validate(ctx, request); !response.Allowed {
			t.Fatal("the current destination was rejected:", response.Result)
		}
		node := &corev1.Node{}
		if err := f.kube.Get(ctx, client.ObjectKey{Name: "node-b"}, node); err != nil {
			t.Fatal(err)
		}
		if err := f.kube.Delete(ctx, node); err != nil {
			t.Fatal(err)
		}
		replacement := node.DeepCopy()
		replacement.UID, replacement.ResourceVersion = "replacement-node", ""
		if err := f.kube.Create(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		if response := handler.Validate(ctx, request); response.Allowed {
			t.Fatal("a same-name replacement Node bypassed the destination pin")
		}
		if err := f.kube.Delete(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		node.ResourceVersion = ""
		if err := f.kube.Create(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
}

func migrationBindingRequest(t *testing.T, f *nativeSandboxFixture, target string, legacy bool) admission.Request {
	t.Helper()
	workload := f.workload(t)
	binding := &corev1.Binding{ObjectMeta: metav1.ObjectMeta{Name: workload.Name + "-0", Namespace: workload.Namespace},
		Target: corev1.ObjectReference{Kind: "Node", Name: target}}
	raw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "binding-request", Name: binding.Name, Namespace: binding.Namespace, Operation: admissionv1.Create,
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"}, SubResource: "binding",
		Kind: metav1.GroupVersionKind{Version: "v1", Kind: "Binding"}, Object: runtime.RawExtension{Raw: raw},
	}}
	if legacy {
		request.Resource.Resource, request.SubResource, request.Name = "bindings", "", ""
	}
	return request
}

func TestPlannedMigrationUncapturedSourceBinding(t *testing.T) {
	for _, previous := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("previous=%t/legacy=%t", previous, legacy), func(t *testing.T) {
				ctx := context.Background()
				f := newMigrationFixture(t, "openclaw", false)
				source, target, id := "node-a", "node-b", "migration-00000001"
				if previous {
					completeMigration(t, f, id, target)
					source, target, id = target, source, "migration-00000002"
				}
				requestMigration(t, f, id, target)
				handler, err := sandboxadmission.NewPodHandler(f.kube, sandboxadmission.PodConfig{StateNamespace: f.r.OperatorNamespace})
				if err != nil {
					t.Fatal(err)
				}
				request := migrationBindingRequest(t, f, source, legacy)
				if response := handler.Validate(ctx, request); !response.Allowed {
					t.Fatal("an uncaptured request blocked ordinary source recovery:", response.Result)
				}
				if previous {
					if response := handler.Validate(ctx, migrationBindingRequest(t, f, target, legacy)); response.Allowed {
						t.Fatal("an uncaptured request replaced the previous completed destination pin")
					}
				}
				agent := f.current(t)
				agent.Finalizers = append(agent.Finalizers, sandbox.HoldFinalizer)
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				if response := handler.Validate(ctx, request); response.Allowed {
					t.Fatal("an execution hold allowed source recovery binding")
				}
				agent.Finalizers = slices.DeleteFunc(agent.Finalizers, func(value string) bool { return value == sandbox.HoldFinalizer })
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				workload := f.workload(t)
				pod := &corev1.Pod{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name + "-0"}, pod); err != nil {
					t.Fatal(err)
				}
				annotation := pod.Annotations[api.AnnotationMigrationRequest]
				pod.Annotations[api.AnnotationMigrationRequest] = id
				if err := f.kube.Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
				if response := handler.Validate(ctx, request); response.Allowed {
					t.Fatal("an uncaptured destination template bypassed binding admission")
				}
				if annotation == "" {
					delete(pod.Annotations, api.AnnotationMigrationRequest)
				} else {
					pod.Annotations[api.AnnotationMigrationRequest] = annotation
				}
				if err := f.kube.Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
				stale := f.current(t)
				f.reconcileOK(t)
				migrationPhase(t, f, api.MigrationProtecting)
				if response := handler.Validate(ctx, request); response.Allowed {
					t.Fatal("source recovery binding bypassed an accepted migration")
				}
				if err := f.r.checkMigrationApply(ctx, stale, &workload.Spec.Template.Spec); err == nil {
					t.Fatal("a stale recovery apply bypassed a same-generation capture checkpoint")
				}
			})
		}
	}
}
