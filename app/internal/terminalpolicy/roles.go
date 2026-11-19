// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminalpolicy

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// ReadRoles are established by the Operator before the read-only API preflight.
// A dry-run RoleBinding cannot resolve a Role that only exists in another
// dry-run request. These definitions grant nothing until the Chart binds them.
func ReadRoles(namespace, workload, agentName, agentUID string) (*rbacv1.Role, *rbacv1.ClusterRole) {
	meta := func(name, namespace string) metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"},
			Annotations: map[string]string{"cetirun.microsoft.com/agent-uid": agentUID},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: agentName, UID: types.UID(agentUID),
				Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
			}},
		}
	}
	role := &rbacv1.Role{ObjectMeta: meta(WorkloadName(workload), namespace), Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods"}, ResourceNames: []string{workload + "-0"}, Verbs: []string{"get"}},
		{APIGroups: []string{"apps"}, Resources: []string{"statefulsets"}, ResourceNames: []string{workload}, Verbs: []string{"get"}},
	}}
	reader := &rbacv1.ClusterRole{ObjectMeta: meta(AgentReaderName(agentUID), ""), Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{"cetirun.microsoft.com"}, Resources: []string{"agents"}, ResourceNames: []string{agentName}, Verbs: []string{"get"},
	}}}
	return role, reader
}
