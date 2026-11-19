// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMigrationObservationNodeIdentityAndFreshness(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		duration int32
		age      time.Duration
		mutate   func(*corev1.Node, *coordinationv1.Lease, *storagev1.CSINode)
		allowed  bool
	}{
		{"current", 40, 0, nil, true},
		{"lease boundary", 40, 40 * time.Second, nil, true},
		{"expired lease", 40, 40*time.Second + time.Microsecond, nil, false},
		{"capped lease boundary", 120, time.Minute, nil, true},
		{"capped lease expired", 120, time.Minute + time.Microsecond, nil, false},
		{"clock boundary", 40, -5 * time.Second, nil, true},
		{"future lease", 40, -5*time.Second - time.Microsecond, nil, false},
		{"missing duration", 0, 0, nil, false},
		{"replaced node", 40, 0, func(n *corev1.Node, _ *coordinationv1.Lease, _ *storagev1.CSINode) { n.UID = "replacement" }, false},
		{"missing boot identity", 40, 0, func(n *corev1.Node, _ *coordinationv1.Lease, _ *storagev1.CSINode) { n.Status.NodeInfo.BootID = "" }, false},
		{"unhealthy node", 40, 0, func(n *corev1.Node, _ *coordinationv1.Lease, _ *storagev1.CSINode) {
			n.Status.Conditions[0].Status = corev1.ConditionFalse
		}, false},
		{"unreachable taint", 40, 0, func(n *corev1.Node, _ *coordinationv1.Lease, _ *storagev1.CSINode) {
			n.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute}}
		}, false},
		{"foreign lease", 40, 0, func(_ *corev1.Node, l *coordinationv1.Lease, _ *storagev1.CSINode) {
			l.OwnerReferences[0].UID = "other"
		}, false},
		{"missing CSI driver", 40, 0, func(_ *corev1.Node, _ *coordinationv1.Lease, c *storagev1.CSINode) { c.Spec.Drivers = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "source-node", UID: "node-uid"},
				Spec: corev1.NodeSpec{ProviderID: "cloud.example.com/source-node"},
				Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot-identity"},
					Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
			lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: corev1.NamespaceNodeLease,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}}},
				Spec: coordinationv1.LeaseSpec{LeaseDurationSeconds: ptr.To(tc.duration), RenewTime: &metav1.MicroTime{Time: now.Add(-tc.age)}}}
			csi := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: node.Name, UID: "csi-node-uid"},
				Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: "storage.example.com", NodeID: "csi-node-identity"}}}}
			if tc.mutate != nil {
				tc.mutate(node, lease, csi)
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, lease, csi).Build()
			r := &AgentReconciler{Client: kube, APIReader: kube, SandboxPreparation: &SandboxPreparation{Now: func() time.Time { return now }}}
			_, observed, err := r.migrationNode(context.Background(), api.MigrationObject{Name: node.Name, UID: "node-uid"}, "storage.example.com")
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%t err=%v", tc.allowed, err)
			}
			if tc.allowed && (observed.UID != node.UID || observed.ProviderID != node.Spec.ProviderID ||
				observed.BootID != node.Status.NodeInfo.BootID || observed.CSINode.UID != csi.UID || observed.CSINodeID != "csi-node-identity") {
				t.Fatal("current observation lost an immutable node or CSI identity")
			}
		})
	}
}

func TestMigrationObservationRequiresAcknowledgedContainers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*corev1.Pod)
		allowed bool
	}{
		{"complete", nil, true},
		{"not deleting", func(p *corev1.Pod) { p.DeletionTimestamp = nil }, false},
		{"node lost", func(p *corev1.Pod) { p.Status.Reason = "NodeLost" }, false},
		{"not terminal", func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }, false},
		{"missing status", func(p *corev1.Pod) { p.Status.ContainerStatuses = p.Status.ContainerStatuses[:1] }, false},
		{"duplicate name", func(p *corev1.Pod) { p.Status.ContainerStatuses[1].Name = "agent" }, false},
		{"unacknowledged exit", func(p *corev1.Pod) { p.Status.ContainerStatuses[1].State.Terminated.FinishedAt = metav1.Time{} }, false},
		{"contradictory running", func(p *corev1.Pod) { p.Status.ContainerStatuses[1].State.Running = &corev1.ContainerStateRunning{} }, false},
		{"missing init acknowledgement", func(p *corev1.Pod) { p.Spec.InitContainers = []corev1.Container{{Name: "init"}} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: ptr.To(metav1.Now())},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}, {Name: "helper"}}},
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{
					{Name: "agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Now()}}},
					{Name: "helper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Now()}}},
				}}}
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			if migrationPodTerminated(pod) != tc.allowed {
				t.Fatalf("container acknowledgement allowed=%t", tc.allowed)
			}
		})
	}
}

func TestMigrationObservationDetachFinalizerPrecedesStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deleting  bool
		finalizer bool
		attached  bool
		allowed   bool
	}{
		{"live detached status is insufficient", false, false, false, false},
		{"attacher still owns detach", true, true, true, false},
		{"status false before release", true, true, false, false},
		{"released with stale attached status", true, false, true, true},
		{"released with final status", true, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attachment := &storagev1.VolumeAttachment{Status: storagev1.VolumeAttachmentStatus{Attached: tc.attached}}
			if tc.deleting {
				attachment.DeletionTimestamp = ptr.To(metav1.Now())
			}
			if tc.finalizer {
				attachment.Finalizers = []string{migrationAttacherFinalizer("storage.example.com")}
			}
			if migrationDetachAcknowledged(attachment, "storage.example.com") != tc.allowed {
				t.Fatalf("detach acknowledgement allowed=%t", tc.allowed)
			}
		})
	}
}
