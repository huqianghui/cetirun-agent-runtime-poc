// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func KubernetesBindingResource(object unstructured.Unstructured) bool {
	return object.GetAPIVersion() == rbacv1.SchemeGroupVersion.String() &&
		(object.GetKind() == "ClusterRoleBinding" ||
			object.GetKind() == "RoleBinding" && object.GetLabels()[terminalpolicy.WorkloadLabel] != "true")
}

// KubernetesBindings derives journal identities from the already-authorized
// compiler output. It never discovers or approves a Role or a new subject.
func (e ReconcileExpectation) KubernetesBindings() ([]sandboxpolicy.BindingReference, error) {
	var account, workloadName string
	for _, workload := range e.Workloads {
		if workload.Kind == "StatefulSet" {
			account = workload.Template.Spec.ServiceAccountName
			workloadName = workload.Identity.Name
		}
	}
	binding, err := json.Marshal(e.Binding)
	if err != nil {
		return nil, err
	}
	var refs []sandboxpolicy.BindingReference
	for _, object := range e.Controls {
		if object.GetAPIVersion() == rbacv1.SchemeGroupVersion.String() && object.GetKind() == "RoleBinding" &&
			!KubernetesBindingResource(object) {
			terminalAccount := terminalpolicy.WorkloadName(workloadName)
			if workloadName == "" || !slices.ContainsFunc(e.Workloads, func(workload RenderedWorkload) bool {
				return workload.Kind == "Deployment" && workload.Identity.Name == terminalAccount &&
					workload.Template.Spec.ServiceAccountName == terminalAccount &&
					workload.Template.Labels[terminalpolicy.WorkloadLabel] == "true"
			}) ||
				!equality.Semantic.DeepEqual(object.GetOwnerReferences(), []metav1.OwnerReference{AgentOwner(e.Agent.Name, e.Agent.UID)}) ||
				object.GetLabels()[sandboxpolicy.AgentUIDLabel] != string(e.Agent.UID) {
				return nil, fmt.Errorf("terminal role requires the current owned terminal expectation")
			}
			if err := terminalpolicy.ValidateRoleBinding(object, e.Namespace.Name, terminalAccount); err != nil {
				return nil, err
			}
			continue
		}
		var metadata metav1.ObjectMeta
		var role rbacv1.RoleRef
		var subjects []rbacv1.Subject
		switch object.GetAPIVersion() + "/" + object.GetKind() {
		case "rbac.authorization.k8s.io/v1/RoleBinding":
			var value rbacv1.RoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(object.Object, &value, true); err != nil {
				return nil, err
			}
			if value.Namespace == "" {
				return nil, fmt.Errorf("Kubernetes RoleBinding requires an explicit namespace")
			}
			metadata, role, subjects = value.ObjectMeta, value.RoleRef, value.Subjects
		case "rbac.authorization.k8s.io/v1/ClusterRoleBinding":
			var value rbacv1.ClusterRoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(object.Object, &value, true); err != nil {
				return nil, err
			}
			if value.Namespace != "" {
				return nil, fmt.Errorf("Kubernetes ClusterRoleBinding must be cluster-scoped")
			}
			metadata, role, subjects = value.ObjectMeta, value.RoleRef, value.Subjects
		default:
			continue
		}
		if account == "" || account == "default" ||
			!equality.Semantic.DeepEqual(subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: e.Namespace.Name}}) ||
			role.APIGroup != rbacv1.GroupName || role.Kind != "ClusterRole" || role.Name == "" ||
			!equality.Semantic.DeepEqual(metadata.OwnerReferences, []metav1.OwnerReference{AgentOwner(e.Agent.Name, e.Agent.UID)}) ||
			metadata.Labels[sandboxpolicy.AgentUIDLabel] != string(e.Agent.UID) ||
			metadata.Annotations[sandboxpolicy.BindingAnnotation] != string(binding) ||
			metadata.Annotations[sandboxpolicy.RevisionAnnotation] != e.Revision ||
			!ValidInputDigest(metadata.Annotations[sandboxpolicy.GrantRevisionAnnotation]) {
			return nil, fmt.Errorf("Kubernetes binding differs from its exact dedicated subject, ownership or approval")
		}
		if !slices.ContainsFunc(e.Sources, func(source SourceReference) bool {
			return source.IsClusterRole() && source.Name == role.Name && source.ValidIdentity()
		}) {
			return nil, fmt.Errorf("Kubernetes binding has no approved ClusterRole source pin")
		}
		refs = append(refs, sandboxpolicy.BindingReference{
			Kind: object.GetKind(), Name: metadata.Name, Namespace: metadata.Namespace,
			AgentName: e.Agent.Name, AgentUID: e.Agent.UID, RoleRef: role,
			ServiceAccountName: account, ServiceAccountNamespace: e.Namespace.Name,
			GrantRevision: metadata.Annotations[sandboxpolicy.GrantRevisionAnnotation],
		})
	}
	return refs, nil
}

func (b *Bundle) validateReconciledIdentity(spec corev1.PodSpec) error {
	controls := b.input.Controls
	for _, desired := range controls.Volumes {
		if !slices.ContainsFunc(spec.Volumes, func(volume corev1.Volume) bool {
			return equality.Semantic.DeepEqual(volume, desired)
		}) {
			return fmt.Errorf("reconciled Sandbox omitted or changed an approved identity volume")
		}
	}
	main := spec.Containers[0]
	for _, desired := range controls.VolumeMounts {
		if !slices.ContainsFunc(main.VolumeMounts, func(mount corev1.VolumeMount) bool {
			return equality.Semantic.DeepEqual(mount, desired)
		}) {
			return fmt.Errorf("reconciled Sandbox omitted or changed an approved identity mount")
		}
	}
	for _, desired := range controls.Environment {
		if !slices.ContainsFunc(main.Env, func(env corev1.EnvVar) bool {
			return equality.Semantic.DeepEqual(env, desired)
		}) {
			return fmt.Errorf("reconciled Sandbox omitted or changed approved identity environment")
		}
	}
	for _, group := range [][]corev1.Container{spec.InitContainers, spec.Containers[1:]} {
		for _, container := range group {
			for _, mount := range container.VolumeMounts {
				if slices.ContainsFunc(controls.Volumes, func(volume corev1.Volume) bool { return volume.Name == mount.Name }) {
					return fmt.Errorf("reconciled Sandbox helpers cannot receive the Agent identity")
				}
			}
		}
	}
	return nil
}
