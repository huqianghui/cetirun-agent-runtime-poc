// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplacement"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func migrationIdentity(object client.Object) api.MigrationObject {
	return api.MigrationObject{Name: object.GetName(), UID: object.GetUID()}
}

func (r *AgentReconciler) migrationNow() time.Time {
	if r.SandboxPreparation != nil && r.SandboxPreparation.Now != nil {
		return r.SandboxPreparation.Now()
	}
	return time.Now()
}

func (r *AgentReconciler) migrationGet(ctx context.Context, namespace string, identity api.MigrationObject, object client.Object) error {
	if identity.Name == "" || identity.UID == "" {
		return fmt.Errorf("migration requires complete object identities")
	}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: identity.Name}, object); err != nil {
		return err
	}
	if object.GetUID() != identity.UID {
		return fmt.Errorf("migration object identity changed")
	}
	return nil
}

func (r *AgentReconciler) migrationSourcePod(ctx context.Context, state *api.PlannedMigrationStatus) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	if err := r.migrationGet(ctx, state.Namespace.Name, state.SourcePod, pod); err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOf(pod)
	if len(pod.OwnerReferences) != 1 || owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "StatefulSet" ||
		owner.Name != state.Workload.Name || owner.UID != state.Workload.UID || pod.Spec.NodeName != state.Source.Name {
		return nil, fmt.Errorf("migration source Pod ownership or Node changed")
	}
	return pod, nil
}

func (r *AgentReconciler) migrationNode(ctx context.Context, identity api.MigrationObject, driver string) (*corev1.Node, api.MigrationNode, error) {
	node := &corev1.Node{}
	var result api.MigrationNode
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: identity.Name}, node); err != nil {
		return nil, result, err
	}
	if node.UID == "" || (identity.UID != "" && identity.UID != node.UID) ||
		!node.DeletionTimestamp.IsZero() || node.Spec.ProviderID == "" || node.Status.NodeInfo.BootID == "" {
		return nil, result, fmt.Errorf("migration requires the current physical Node identity")
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			ready = condition.Status == corev1.ConditionTrue
		}
	}
	for _, taint := range node.Spec.Taints {
		if taint.Key == corev1.TaintNodeNotReady || taint.Key == corev1.TaintNodeUnreachable {
			ready = false
		}
	}
	lease := &coordinationv1.Lease{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: node.Name}, lease); err != nil {
		return nil, result, err
	}
	duration := time.Duration(ptr.Deref(lease.Spec.LeaseDurationSeconds, 0)) * time.Second
	if duration > time.Minute {
		duration = time.Minute
	}
	if !ready || duration <= 0 || lease.Spec.RenewTime == nil || !lease.DeletionTimestamp.IsZero() ||
		len(lease.OwnerReferences) != 1 || lease.OwnerReferences[0].APIVersion != "v1" ||
		lease.OwnerReferences[0].Kind != "Node" || lease.OwnerReferences[0].Name != node.Name || lease.OwnerReferences[0].UID != node.UID {
		return nil, result, fmt.Errorf("migration requires a healthy Node and its current lease")
	}
	age := r.migrationNow().Sub(lease.Spec.RenewTime.Time)
	if age < -5*time.Second || age > duration {
		return nil, result, fmt.Errorf("migration Node lease is stale or from the future")
	}
	csi := &storagev1.CSINode{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: node.Name}, csi); err != nil {
		return nil, result, err
	}
	if csi.UID == "" || !csi.DeletionTimestamp.IsZero() {
		return nil, result, fmt.Errorf("migration requires a current CSINode")
	}
	result = api.MigrationNode{MigrationObject: migrationIdentity(node), ProviderID: node.Spec.ProviderID,
		BootID: node.Status.NodeInfo.BootID, CSINode: migrationIdentity(csi)}
	for _, item := range csi.Spec.Drivers {
		if item.Name == driver {
			result.CSINodeID = item.NodeID
		}
	}
	if result.CSINodeID == "" {
		return nil, result, fmt.Errorf("migration destination does not register the HOME CSI driver")
	}
	return node, result, nil
}

func (r *AgentReconciler) checkMigrationNode(ctx context.Context, recorded api.MigrationNode, driver string, source bool) (*corev1.Node, error) {
	node, current, err := r.migrationNode(ctx, recorded.MigrationObject, driver)
	if err != nil {
		return nil, err
	}
	if current.ProviderID != recorded.ProviderID || current.CSINode != recorded.CSINode ||
		current.CSINodeID != recorded.CSINodeID || (source && current.BootID != recorded.BootID) {
		return nil, fmt.Errorf("migration Node or CSI identity changed; unqualified recovery is forbidden")
	}
	return node, nil
}

func migrationNodeCompatible(node *corev1.Node, spec *corev1.PodSpec, volume *corev1.PersistentVolume) error {
	if node.Spec.Unschedulable || spec.NodeName != "" || !sandboxplacement.NodeMatchesSpec(node, spec) {
		return fmt.Errorf("migration destination does not satisfy current workload scheduling")
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if !slices.ContainsFunc(spec.Tolerations, func(t corev1.Toleration) bool {
			return t.ToleratesTaint(log.Log.WithName("planned-migration"), &taint, false)
		}) {
			return fmt.Errorf("migration destination has an untolerated scheduling taint")
		}
	}
	if volume.Spec.NodeAffinity != nil && !sandboxplacement.NodeMatchesSpec(node, &corev1.PodSpec{
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: volume.Spec.NodeAffinity.Required,
		}},
	}) {
		return fmt.Errorf("migration destination is outside the bound HOME volume topology")
	}
	return nil
}

func (r *AgentReconciler) migrationScheduling(ctx context.Context, node *corev1.Node, spec *corev1.PodSpec, volume *corev1.PersistentVolume) error {
	copy := spec.DeepCopy()
	if name := ptr.Deref(copy.RuntimeClassName, ""); name != "" {
		runtime := &nodev1.RuntimeClass{}
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: name}, runtime); err != nil {
			return err
		}
		if !runtime.DeletionTimestamp.IsZero() {
			return fmt.Errorf("migration RuntimeClass is terminating")
		}
		if runtime.Scheduling != nil {
			if copy.NodeSelector == nil {
				copy.NodeSelector = map[string]string{}
			}
			for key, value := range runtime.Scheduling.NodeSelector {
				if existing, found := copy.NodeSelector[key]; found && existing != value {
					return fmt.Errorf("migration workload conflicts with RuntimeClass scheduling")
				}
				copy.NodeSelector[key] = value
			}
			copy.Tolerations = append(copy.Tolerations, runtime.Scheduling.Tolerations...)
		}
	}
	return migrationNodeCompatible(node, copy, volume)
}

func (r *AgentReconciler) migrationHome(ctx context.Context, agent *api.Agent, state *api.PlannedMigrationStatus) (*corev1.PersistentVolume, error) {
	namespace := &corev1.Namespace{}
	if err := r.migrationGet(ctx, "", state.Namespace, namespace); err != nil {
		return nil, err
	}
	if namespace.Name != instanceNamespace(agent.Name) || !sandboxBuiltinOwned(namespace, agent) ||
		!namespace.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("migration namespace ownership changed")
	}
	home := &corev1.PersistentVolumeClaim{}
	if err := r.migrationGet(ctx, namespace.Name, state.Home, home); err != nil {
		return nil, err
	}
	if !sandboxBuiltinOwned(home, agent) || !home.DeletionTimestamp.IsZero() || home.Status.Phase != corev1.ClaimBound ||
		home.Spec.VolumeName != state.Volume.Name || len(home.Spec.AccessModes) != 1 ||
		(home.Spec.AccessModes[0] != corev1.ReadWriteOncePod && home.Spec.AccessModes[0] != corev1.ReadWriteOnce) ||
		ptr.Deref(home.Spec.VolumeMode, corev1.PersistentVolumeFilesystem) != corev1.PersistentVolumeFilesystem {
		return nil, fmt.Errorf("migration requires the same bound owned filesystem RWO or RWOP HOME")
	}
	volume := &corev1.PersistentVolume{}
	if err := r.migrationGet(ctx, "", state.Volume, volume); err != nil {
		return nil, err
	}
	claim := volume.Spec.ClaimRef
	if !volume.DeletionTimestamp.IsZero() || volume.Status.Phase != corev1.VolumeBound || claim == nil ||
		claim.Name != home.Name || claim.Namespace != home.Namespace || claim.UID != home.UID ||
		volume.Spec.CSI == nil || volume.Spec.CSI.Driver != state.Driver || volume.Spec.CSI.VolumeHandle != state.VolumeHandle {
		return nil, fmt.Errorf("migration HOME volume binding or CSI identity changed")
	}
	return volume, nil
}

func (r *AgentReconciler) migrationAttachments(ctx context.Context, volume string) ([]storagev1.VolumeAttachment, error) {
	list := &storagev1.VolumeAttachmentList{}
	if err := r.customReader().List(ctx, list); err != nil {
		return nil, err
	}
	var result []storagev1.VolumeAttachment
	for _, attachment := range list.Items {
		if ptr.Deref(attachment.Spec.Source.PersistentVolumeName, "") == volume {
			result = append(result, attachment)
		}
	}
	return result, nil
}

func migrationAttacherFinalizer(driver string) string {
	return "external-attacher/" + strings.ReplaceAll(driver, ".", "-")
}

func migrationDetachAcknowledged(attachment *storagev1.VolumeAttachment, driver string) bool {
	// Capture verified the CSI finalizer on this pinned attachment. The attacher
	// removes it after successful detach, before status writes it may never retry.
	return !attachment.DeletionTimestamp.IsZero() &&
		!slices.Contains(attachment.Finalizers, migrationAttacherFinalizer(driver))
}

func (r *AgentReconciler) captureMigration(ctx context.Context, agent *api.Agent) (*api.PlannedMigrationStatus, error) {
	request := agent.Spec.PlannedMigration
	// Suspension can overtake the request before capture. Protect a still-ready
	// source before stopping it, without treating suspension intent as shutdown.
	if request == nil || !api.ValidRestartRequestID(request.RequestID) || request.Target.UID == "" ||
		agent.Status.Phase != api.PhaseReady ||
		!isConditionTrue(agent.Status.Conditions, api.ConditionReady) ||
		agent.Spec.RestartRequestID != agent.Status.ObservedRestartRequestID {
		return nil, fmt.Errorf("migration requires a ready Agent with no pending restart")
	}
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
		return nil, err
	}
	object := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{
		Namespace: r.OperatorNamespace, Name: w.PodExpectationName(namespace.Name, namespace.UID),
	}, object); err != nil {
		return nil, err
	}
	expected, err := w.ReadReconcileExpectation(object)
	if err != nil {
		return nil, err
	}
	if expected.Agent != (w.ObjectIdentity{Name: agent.Name, UID: agent.UID}) ||
		expected.AgentGeneration != agent.Status.ObservedGeneration || expected.Namespace.UID != namespace.UID {
		return nil, fmt.Errorf("migration requires the currently reconciled Agent lifetime")
	}
	workload := &appsv1.StatefulSet{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: instanceServiceName(agent.Name)}, workload); err != nil {
		return nil, err
	}
	if workload.UID == "" || !sandboxBuiltinOwned(workload, agent) || !workload.DeletionTimestamp.IsZero() ||
		workload.Spec.Replicas == nil || *workload.Spec.Replicas < 0 || *workload.Spec.Replicas > 1 {
		return nil, fmt.Errorf("migration requires a stable single-Pod owned workload")
	}
	if *workload.Spec.Replicas == 0 || workload.Status.ObservedGeneration < workload.Generation ||
		workload.Status.ReadyReplicas != 1 || workload.Status.UpdatedReplicas != 1 {
		return nil, errMigrationSourceNotReady
	}
	if !slices.ContainsFunc(expected.Workloads, func(item w.RenderedWorkload) bool {
		return item.Kind == "StatefulSet" && item.Identity.Name == workload.Name &&
			item.Replicas == 1 && w.SamePodTemplate(item.Template, workload.Spec.Template)
	}) {
		return nil, fmt.Errorf("migration workload differs from its current authorized template")
	}
	pods := &corev1.PodList{}
	if err := r.customReader().List(ctx, pods, client.InNamespace(namespace.Name)); err != nil {
		return nil, err
	}
	var source *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner != nil && owner.Kind == "StatefulSet" && owner.Name == workload.Name && owner.UID == workload.UID {
			if source != nil {
				return nil, fmt.Errorf("migration found multiple source Pods")
			}
			source = pod
		}
	}
	if source == nil || source.UID == "" || source.Spec.NodeName == "" || !source.DeletionTimestamp.IsZero() ||
		source.Status.Phase != corev1.PodRunning || ptr.Deref(source.Spec.TerminationGracePeriodSeconds, 0) <= 0 ||
		!slices.ContainsFunc(source.Status.Conditions, func(c corev1.PodCondition) bool {
			return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
		}) {
		return nil, fmt.Errorf("migration requires one ready source Pod with ordinary graceful termination")
	}
	for _, volume := range source.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != expected.Home.Name {
			return nil, fmt.Errorf("planned migration currently supports one persistent HOME volume")
		}
	}
	if source.Spec.NodeName == request.Target.Name {
		return nil, fmt.Errorf("migration destination must differ from the source")
	}
	home := &corev1.PersistentVolumeClaim{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: expected.Home.Name}, home); err != nil {
		return nil, err
	}
	volume := &corev1.PersistentVolume{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: home.Spec.VolumeName}, volume); err != nil {
		return nil, err
	}
	if home.UID != expected.Home.UID || volume.Spec.CSI == nil || volume.Spec.CSI.Driver == "" || volume.Spec.CSI.VolumeHandle == "" {
		return nil, fmt.Errorf("migration requires the original HOME and an attachable CSI volume")
	}
	driver := &storagev1.CSIDriver{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: volume.Spec.CSI.Driver}, driver); err != nil {
		return nil, err
	}
	if !driver.DeletionTimestamp.IsZero() || !ptr.Deref(driver.Spec.AttachRequired, true) {
		return nil, fmt.Errorf("migration requires CSI attachment acknowledgements")
	}
	_, from, err := r.migrationNode(ctx, api.MigrationObject{Name: source.Spec.NodeName}, driver.Name)
	if err != nil {
		return nil, err
	}
	node, destination, err := r.migrationNode(ctx, request.Target, driver.Name)
	if err != nil {
		return nil, err
	}
	spec := workload.Spec.Template.Spec.DeepCopy()
	if previous := agent.Status.PlannedMigration; previous != nil {
		if previous.Phase != api.MigrationCompleted || workload.Spec.Template.Annotations[api.AnnotationMigrationRequest] != previous.RequestID {
			return nil, fmt.Errorf("the previous migration has not completed on the current workload")
		}
		if err := runtimebindings.UnpinMigrationNode(spec, previous.Target.Name); err != nil {
			return nil, err
		}
	}
	if err := r.migrationScheduling(ctx, node, spec, volume); err != nil {
		return nil, err
	}
	attachments, err := r.migrationAttachments(ctx, volume.Name)
	if err != nil {
		return nil, err
	}
	if len(attachments) != 1 {
		return nil, fmt.Errorf("migration requires one unambiguous source VolumeAttachment")
	}
	attachment := &attachments[0]
	if attachment.UID == "" || attachment.Spec.NodeName != source.Spec.NodeName || attachment.Spec.Attacher != driver.Name ||
		!attachment.DeletionTimestamp.IsZero() || !attachment.Status.Attached || attachment.Status.AttachError != nil ||
		attachment.Status.DetachError != nil || !slices.Contains(attachment.Finalizers, migrationAttacherFinalizer(driver.Name)) {
		return nil, fmt.Errorf("migration source attachment is not acknowledged by its CSI attacher")
	}
	templateHash, err := sandboxPreparationDigest(workload.Spec.Template)
	if err != nil {
		return nil, err
	}
	state := &api.PlannedMigrationStatus{PlannedMigrationRequest: *request, Phase: api.MigrationProtecting,
		Namespace: migrationIdentity(namespace), Workload: migrationIdentity(workload), WorkloadTemplateHash: templateHash,
		SourcePod: migrationIdentity(source), Source: from, Destination: destination, Home: migrationIdentity(home),
		Volume: migrationIdentity(volume), Attachment: migrationIdentity(attachment), Driver: driver.Name,
		VolumeHandle: volume.Spec.CSI.VolumeHandle, StartedAt: metav1.NewTime(r.migrationNow())}
	if previous := agent.Status.PlannedMigration; previous != nil {
		state.PreviousRequestID = previous.RequestID
	}
	if _, err := r.migrationHome(ctx, agent, state); err != nil {
		return nil, err
	}
	return state, nil
}

func migrationPodTerminated(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp.IsZero() || pod.Status.Reason == "NodeLost" ||
		(pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed) {
		return false
	}
	if len(pod.Spec.Containers) == 0 || len(pod.Spec.Containers) != len(pod.Status.ContainerStatuses) ||
		len(pod.Spec.InitContainers) != len(pod.Status.InitContainerStatuses) ||
		len(pod.Spec.EphemeralContainers) != len(pod.Status.EphemeralContainerStatuses) {
		return false
	}
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses,
	} {
		for _, status := range statuses {
			if status.State.Terminated == nil || status.State.Terminated.FinishedAt.IsZero() ||
				status.State.Running != nil || status.State.Waiting != nil {
				return false
			}
		}
	}
	matches := func(name string, statuses []corev1.ContainerStatus) bool {
		count := 0
		for _, status := range statuses {
			if status.Name == name {
				count++
			}
		}
		return count == 1
	}
	for _, container := range pod.Spec.Containers {
		if !matches(container.Name, pod.Status.ContainerStatuses) {
			return false
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if !matches(container.Name, pod.Status.InitContainerStatuses) {
			return false
		}
	}
	for _, container := range pod.Spec.EphemeralContainers {
		if !matches(container.Name, pod.Status.EphemeralContainerStatuses) {
			return false
		}
	}
	return true
}
