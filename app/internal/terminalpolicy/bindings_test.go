// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminalpolicy

import (
	"context"
	"testing"

	"github.com/Azure/CetiRun/app/internal/chataccess"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestOwnedBindingRequiresExactTerminalAuthority(t *testing.T) {
	const agent, uid, namespace = "example", "agent-uid", "cetirun-example"
	for _, kind := range []string{"read", "exec", "agent-reader"} {
		for _, change := range []string{"current", "subject", "owner", "role-reference", "role-rules", "role-owner", "terminating"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				role, readerRole := ReadRoles(namespace, chataccess.ServiceName(agent), agent, uid)
				role.UID, readerRole.UID = "read-role-uid", "agent-reader-role-uid"
				metadata := *role.ObjectMeta.DeepCopy()
				metadata.UID = "binding-uid"
				metadata.Labels[WorkloadLabel] = "true"
				subjects := []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: namespace, Name: role.Name}}
				roleRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}
				if kind == "exec" {
					metadata.Name += "-exec"
					roleRef.Kind, roleRef.Name = "ClusterRole", ExecRole
				}
				if kind == "agent-reader" {
					metadata = *readerRole.ObjectMeta.DeepCopy()
					metadata.UID = "binding-uid"
					roleRef.Kind, roleRef.Name = "ClusterRole", readerRole.Name
				}
				switch change {
				case "subject":
					subjects[0].Name = "foreign-account"
				case "owner":
					metadata.OwnerReferences[0].UID = "foreign-agent"
				case "role-reference":
					roleRef.Name = "cluster-admin"
				case "role-rules":
					role.Rules[0].Verbs = []string{"*"}
					readerRole.Rules[0].Verbs = []string{"*"}
				case "role-owner":
					role.OwnerReferences[0].UID = "foreign-agent"
					readerRole.OwnerReferences[0].UID = "foreign-agent"
				case "terminating":
					now := metav1.Now()
					metadata.DeletionTimestamp = &now
				}
				var object client.Object = &rbacv1.RoleBinding{ObjectMeta: metadata, Subjects: subjects, RoleRef: roleRef}
				if kind == "agent-reader" {
					object = &rbacv1.ClusterRoleBinding{ObjectMeta: metadata, Subjects: subjects, RoleRef: roleRef}
				}
				scheme := runtime.NewScheme()
				if err := rbacv1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(role, readerRole).Build()
				recognized, err := OwnedBinding(context.Background(), kube, object, agent, uid)
				if !recognized || (err == nil) != (change == "current") {
					t.Fatalf("recognized=%v err=%v", recognized, err)
				}
			})
		}
	}
}

func TestTerminalLabelDoesNotAuthorizeAnUnrelatedGrant(t *testing.T) {
	object := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "unrelated", Namespace: "cetirun-example", Labels: map[string]string{WorkloadLabel: "true"},
	}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"}}
	if recognized, err := OwnedBinding(context.Background(), nil, object, "example", "agent-uid"); recognized || err != nil {
		t.Fatal("an arbitrary labeled grant was recognized as terminal authority")
	}
}
