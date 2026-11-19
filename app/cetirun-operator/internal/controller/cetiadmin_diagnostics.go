// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Diagnostic failures are returned after installation reconciliation, without
// revoking unrelated management access or changing runtime eligibility.
func (r *CetiAdminReconciler) reconcileDiagnosticAccess(ctx context.Context, state *cetiadmin.Installation) error {
	binding, err := r.currentAccessBinding(ctx, state)
	if err != nil {
		return errors.Join(err, r.revokeDiagnosticAccess(ctx, state))
	}
	config := binding.Config()
	name := cetiAdminAccessName(state) + "-diagnostics"
	owner := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name, UID: state.Agent.UID}}
	seen := map[string]bool{}
	var failures []error
	for _, namespaceName := range config.PlatformReadNamespaces {
		if seen[namespaceName] || namespaceName == state.Namespace.Name ||
			(config.MigrationSource != nil && namespaceName == config.MigrationSource.Namespace.Name) {
			continue
		}
		seen[namespaceName] = true
		namespace := &corev1.Namespace{}
		err := r.reader().Get(ctx, client.ObjectKey{Name: namespaceName}, namespace)
		if err == nil && (namespace.UID == "" || namespace.DeletionTimestamp != nil) {
			err = cetiadmin.ErrConflict
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("diagnostic namespace %s: %w", namespaceName, err), r.withdrawDiagnosticBinding(ctx, state, namespaceName))
			continue
		}
		desired := &rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name, OwnerReferences: []metav1.OwnerReference{registryauth.Owner(owner)}, Annotations: map[string]string{
				cetiadmin.AnnotationSourceUID: string(state.Source.UID), cetiadmin.AnnotationInstallationUID: string(state.RecordUID),
				cetiAdminGrantNamespace: namespace.Name, cetiAdminGrantNamespaceUID: string(namespace.UID),
			}},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}}
		if err := r.ensureRuntimeAccessResource(ctx, desired); err != nil {
			failures = append(failures, fmt.Errorf("diagnostic namespace %s: %w", namespaceName, err))
		}
	}
	return errors.Join(failures...)
}

func (r *CetiAdminReconciler) withdrawDiagnosticBinding(ctx context.Context, state *cetiadmin.Installation, namespace string) error {
	binding := &rbacv1.RoleBinding{}
	key := client.ObjectKey{Name: cetiAdminAccessName(state) + "-diagnostics", Namespace: namespace}
	if err := r.reader().Get(ctx, key, binding); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !diagnosticBindingOwned(binding, state) {
		return cetiadmin.ErrConflict
	}
	return r.deleteRuntimeAccessResource(ctx, binding)
}

func diagnosticBindingOwned(binding *rbacv1.RoleBinding, state *cetiadmin.Installation) bool {
	name := cetiAdminAccessName(state) + "-diagnostics"
	owner := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name, UID: state.Agent.UID}}
	return binding.Name == name && binding.Namespace != "" && binding.Annotations[cetiadmin.AnnotationInstallationUID] == string(state.RecordUID) &&
		binding.Annotations[cetiadmin.AnnotationSourceUID] == string(state.Source.UID) && binding.Annotations[cetiadmin.AnnotationCreationID] != "" &&
		binding.Annotations[cetiAdminGrantNamespace] == binding.Namespace && binding.Annotations[cetiAdminGrantNamespaceUID] != "" &&
		reflect.DeepEqual(binding.OwnerReferences, []metav1.OwnerReference{registryauth.Owner(owner)}) &&
		reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}}) &&
		binding.RoleRef == (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name})
}

func (r *CetiAdminReconciler) revokeDiagnosticAccess(ctx context.Context, state *cetiadmin.Installation) error {
	bindings := &rbacv1.RoleBindingList{}
	if err := r.reader().List(ctx, bindings); err != nil {
		return err
	}
	var failures []error
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		if binding.Name != cetiAdminAccessName(state)+"-diagnostics" || binding.Annotations[cetiadmin.AnnotationInstallationUID] != string(state.RecordUID) {
			continue
		}
		if !diagnosticBindingOwned(binding, state) {
			failures = append(failures, cetiadmin.ErrConflict)
			continue
		}
		if err := r.deleteRuntimeAccessResource(ctx, binding); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
