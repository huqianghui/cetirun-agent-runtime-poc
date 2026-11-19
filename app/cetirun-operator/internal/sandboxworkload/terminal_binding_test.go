// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestTerminalBindingsAreNotAgentKubernetesGrants(t *testing.T) {
	for _, change := range []string{"current", "subject", "role", "owner", "namespace", "workload", "missing-label"} {
		t.Run(change, func(t *testing.T) {
			const namespace, workload = "cetirun-example", "openclaw-example-cetirun"
			account := terminalpolicy.WorkloadName(workload)
			expected := ReconcileExpectation{
				Agent: ObjectIdentity{Name: "example", UID: "agent-uid"}, Namespace: ObjectIdentity{Name: namespace, UID: "namespace-uid"},
				Workloads: []RenderedWorkload{
					{Kind: "StatefulSet", Identity: ObjectIdentity{Name: workload}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "default"}}},
					{Kind: "Deployment", Identity: ObjectIdentity{Name: account}, Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{terminalpolicy.WorkloadLabel: "true"}},
						Spec:       corev1.PodSpec{ServiceAccountName: account},
					}},
				},
			}
			binding := &rbacv1.RoleBinding{
				TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
				ObjectMeta: metav1.ObjectMeta{Name: account + "-exec", Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{AgentOwner(expected.Agent.Name, expected.Agent.UID)},
					Labels:          map[string]string{terminalpolicy.WorkloadLabel: "true", sandboxpolicy.AgentUIDLabel: string(expected.Agent.UID)}},
				Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: namespace, Name: account}},
				RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: terminalpolicy.ExecRole},
			}
			switch change {
			case "subject":
				binding.Subjects[0].Name = "foreign"
			case "role":
				binding.RoleRef.Name = "cluster-admin"
			case "owner":
				binding.OwnerReferences[0].UID = "foreign"
			case "namespace":
				binding.Namespace = "foreign"
			case "workload":
				expected.Workloads = expected.Workloads[:1]
			case "missing-label":
				delete(binding.Labels, terminalpolicy.WorkloadLabel)
			}
			raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(binding)
			if err != nil {
				t.Fatal(err)
			}
			expected.Controls = []unstructured.Unstructured{{Object: raw}}
			refs, err := expected.KubernetesBindings()
			if (err == nil) != (change == "current") || len(refs) != 0 {
				t.Fatalf("terminal/Agent grant classification: refs=%v err=%v", refs, err)
			}
			if change == "current" && KubernetesBindingResource(expected.Controls[0]) {
				t.Fatal("terminal binding entered the Agent grant writer")
			}
		})
	}
}
