// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSandboxBootstrapRoleUpgradeAPIServer(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
			t.Run(kind+"/"+string(backend), func(t *testing.T) {
				h := newNativeStartAPIHarnessWithRuntime(t, true, true, corev1.ReadWriteOncePod, kind, backend, nil)
				h = startReconciledAPIHarness(t, h)
				_, receipt, err := h.r.readSandboxBootstrap(h.ctx, h.current(t))
				if err != nil {
					t.Fatal(err)
				}
				role := &rbacv1.Role{}
				key := client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.Objects[1].Name}
				if err := h.admin.Get(h.ctx, key, role); err != nil {
					t.Fatal(err)
				}
				role.Rules = readHistoricalBootstrapRBAC(t).Role.Rules
				if err := h.admin.Update(h.ctx, role); err != nil {
					t.Fatal("seed the recorded pre-mTLS Role:", err)
				}
				workload := h.workload(t)
				h.assertAccess(t, h.kube, authorizationv1.ResourceAttributes{
					Group: "rbac.authorization.k8s.io", Resource: "roles", Namespace: receipt.Namespace, Name: role.Name, Verb: "patch",
				}, true)
				if err := h.r.bootstrapReconciledSandbox(h.ctx, h.current(t)); err != nil {
					t.Fatal("operator could not migrate the recorded Role through the API:", err)
				}
				if err := h.admin.Get(h.ctx, key, role); err != nil {
					t.Fatal(err)
				}
				desired, _ := h.r.operatorNamespaceRBAC(receipt.Namespace)
				if role.UID != receipt.Objects[1].UID || !reflect.DeepEqual(role.Rules, desired.Rules) {
					t.Fatal("API migration replaced the Role or lost the target rules")
				}
				version := role.ResourceVersion
				if err := h.r.bootstrapReconciledSandbox(h.ctx, h.current(t)); err != nil {
					t.Fatal(err)
				}
				if err := h.admin.Get(h.ctx, key, role); err != nil || role.ResourceVersion != version {
					t.Fatal("steady migration wrote the Role again:", err)
				}
				_, current, err := h.r.readSandboxBootstrap(h.ctx, h.current(t))
				if err != nil || !reflect.DeepEqual(current, receipt) ||
					h.workload(t).UID != workload.UID || !reflect.DeepEqual(h.workload(t).Spec, workload.Spec) {
					t.Fatal("API migration changed the retained lifetime:", err)
				}
			})
		}
	}
}
