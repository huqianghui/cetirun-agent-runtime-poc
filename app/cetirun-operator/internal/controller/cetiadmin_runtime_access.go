// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Keep controller-local names while the strict cross-package contract is
// owned by cetiadmin. Diagnostics and legacy focused tests use these helpers.
const (
	cetiAdminGrantAgent        = cetiadmin.AnnotationRuntimeGrantAgent
	cetiAdminGrantAgentUID     = cetiadmin.AnnotationRuntimeGrantAgentUID
	cetiAdminGrantNamespace    = cetiadmin.AnnotationRuntimeGrantNamespace
	cetiAdminGrantNamespaceUID = cetiadmin.AnnotationRuntimeGrantNamespaceUID
)

func cetiAdminAccessName(state *cetiadmin.Installation) string {
	return cetiadmin.AccessName(state.RecordUID)
}

func (r *CetiAdminReconciler) EnqueueRuntimeAccess(ctx context.Context, object client.Object) []reconcile.Request {
	key := client.ObjectKeyFromObject(object)
	if key != r.Source && key != r.stateKey() {
		return nil
	}
	agents := &api.AgentList{}
	if err := r.reader().List(ctx, agents); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot enqueue Agent management access")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(agents.Items))
	for i := range agents.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: agents.Items[i].Name}})
	}
	return requests
}

// ReconcileRuntimeAccess belongs to the ordinary Agent queue. Its permission
// objects do not enter the Admin installation checkpoint or runtime hash.
func (r *CetiAdminReconciler) ReconcileRuntimeAccess(ctx context.Context, name string) (resultErr error) {
	_, state, err := r.readState(ctx)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if name == state.Agent.Name {
		return nil
	}
	if !state.CanRun() {
		return r.revokeRuntimeNamespace(ctx, state, name)
	}
	binding, err := r.currentAccessBinding(ctx, state)
	if err != nil {
		return errors.Join(err, r.revokeRuntimeNamespace(ctx, state, name))
	}
	agent, target, err := r.runtimeTarget(ctx, binding.Config(), state.Record, name)
	if err != nil {
		revokeErr := r.revokeRuntimeNamespace(ctx, state, name)
		if apierrors.IsNotFound(err) || errors.Is(err, cetiadmin.ErrUnbound) {
			return revokeErr
		}
		return errors.Join(err, revokeErr)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, r.revokeRuntimeNamespace(ctx, state, name))
		}
	}()
	namespace := &corev1.Namespace{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: target.Namespace.Name}, namespace); err != nil {
		return err
	}
	if !identityMatches(target.Namespace, namespace) {
		return cetiadmin.ErrConflict
	}
	if namespace.Labels["app.kubernetes.io/managed-by"] != "cetirun-operator" {
		before := namespace.DeepCopy()
		namespace.Labels = maps.Clone(namespace.Labels)
		if namespace.Labels == nil {
			namespace.Labels = map[string]string{}
		}
		namespace.Labels["app.kubernetes.io/managed-by"] = "cetirun-operator"
		if err := r.Patch(ctx, namespace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	for _, desired := range r.runtimeAccessObjects(state, agent, target) {
		if err := r.ensureRuntimeAccessResource(ctx, desired); err != nil {
			return err
		}
	}
	if target.Protected {
		err = r.RevokeExecAccess(ctx, state.Record, target)
	} else {
		err = r.GrantExecAccess(ctx, binding, target)
	}
	if err != nil {
		return err
	}
	_, current, err := r.readState(ctx)
	if err != nil || !current.CanRun() || current.Record != state.Record {
		return errors.Join(err, cetiadmin.ErrUnbound)
	}
	_, err = r.currentAccessBinding(ctx, current)
	return err
}

func (r *CetiAdminReconciler) runtimeTarget(ctx context.Context, config cetiadmin.Config, self cetiadmin.Record, name string) (*api.Agent, cetiadmin.TargetIdentity, error) {
	agent, target, err := cetiadmin.ResolveRuntimeTarget(ctx, r.reader(), config, self, name, "")
	if err != nil {
		return nil, target, err
	}
	if r.Runtime == nil || slices.Contains([]string{r.Runtime.OperatorNamespace, r.Runtime.registryNamespace(), r.Runtime.ConnectionsMCPBrokerNamespace}, target.Namespace.Name) {
		return nil, target, cetiadmin.ErrUnbound
	}
	return agent, target, nil
}

func (r *CetiAdminReconciler) runtimeAccessObjects(state *cetiadmin.Installation, agent *api.Agent, target cetiadmin.TargetIdentity) []client.Object {
	name := cetiadmin.AccessName(state.RecordUID)
	metadata := func(namespace, name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}, Annotations: map[string]string{
			cetiadmin.AnnotationSourceUID: string(state.Source.UID), cetiadmin.AnnotationInstallationUID: string(state.RecordUID),
			cetiadmin.AnnotationRuntimeGrantAgent: agent.Name, cetiadmin.AnnotationRuntimeGrantAgentUID: string(agent.UID), cetiadmin.AnnotationRuntimeGrantNamespace: target.Namespace.Name, cetiadmin.AnnotationRuntimeGrantNamespaceUID: string(target.Namespace.UID),
		}}
	}
	capability := "runtime"
	if target.Protected {
		capability = "protected"
	}
	admin := []rbacv1.Subject{{Kind: "ServiceAccount", Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}}
	reader := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "cetirun-sandbox-admission", Namespace: state.Source.Namespace}}
	objects := []client.Object{
		&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metadata(target.Namespace.Name, name+"-runtime"), Subjects: admin,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name + "-" + capability}},
		&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metadata(target.Namespace.Name, name+"-reader"), Subjects: reader,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name + "-reader"}},
	}
	hash := sha256.Sum256([]byte(agent.UID))
	receiptName := name + "-" + hex.EncodeToString(hash[:8])
	var names []string
	if !target.Protected {
		names = append(names, r.Runtime.maintenanceKey(agent).Name)
	}
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		names = append(names, r.Runtime.ownershipKey(agent).Name)
	}
	var receiptRules []rbacv1.PolicyRule
	if len(names) != 0 {
		receiptRules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: names, Verbs: []string{"get"}}}
	}
	objects = append(objects,
		&rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"}, ObjectMeta: metadata(state.Source.Namespace, receiptName), Rules: receiptRules},
		&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metadata(state.Source.Namespace, receiptName), Subjects: reader,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: receiptName}},
	)
	return objects
}

func (r *CetiAdminReconciler) ensureRuntimeAccessResource(ctx context.Context, desired client.Object) error {
	current := desired.DeepCopyObject().(client.Object)
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		desired.GetAnnotations()[cetiadmin.AnnotationCreationID] = uuid.NewString()
		writeErr := r.Create(ctx, desired)
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
			return errors.Join(writeErr, err)
		}
		if current.GetUID() == "" || !sameCetiAdminCreation(desired, current) {
			return errors.Join(writeErr, cetiadmin.ErrConflict)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if current.GetUID() == "" || current.GetDeletionTimestamp() != nil || current.GetAnnotations()[cetiadmin.AnnotationCreationID] == "" ||
		!reflect.DeepEqual(current.GetOwnerReferences(), desired.GetOwnerReferences()) {
		return cetiadmin.ErrConflict
	}
	for key, value := range desired.GetAnnotations() {
		if current.GetAnnotations()[key] != value {
			return cetiadmin.ErrConflict
		}
	}
	desired.GetAnnotations()[cetiadmin.AnnotationCreationID] = current.GetAnnotations()[cetiadmin.AnnotationCreationID]
	if sameCetiAdminCreation(desired, current) {
		return nil
	}
	if wanted, ok := desired.(*rbacv1.RoleBinding); ok {
		actual := current.(*rbacv1.RoleBinding)
		if !reflect.DeepEqual(actual.Subjects, wanted.Subjects) {
			return cetiadmin.ErrConflict
		}
		// RoleRef is immutable. A policy transition withdraws this namespace's
		// old binding before granting its new capability.
		if actual.RoleRef != wanted.RoleRef {
			if err := r.deleteRuntimeAccessResource(ctx, current); err != nil {
				return err
			}
			delete(desired.GetAnnotations(), cetiadmin.AnnotationCreationID)
			return r.ensureRuntimeAccessResource(ctx, desired)
		}
	}
	want := current.DeepCopyObject().(client.Object)
	switch object := want.(type) {
	case *rbacv1.Role:
		object.Rules = desired.(*rbacv1.Role).Rules
	default:
		return cetiadmin.ErrConflict
	}
	if err := r.Patch(ctx, want, client.MergeFromWithOptions(current, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		return err
	}
	if !sameCetiAdminCreation(desired, current) {
		return cetiadmin.ErrConflict
	}
	return nil
}

func (r *CetiAdminReconciler) revokeRuntimeNamespace(ctx context.Context, state *cetiadmin.Installation, agentName string) error {
	var failures []error
	bindings := &rbacv1.RoleBindingList{}
	if err := r.reader().List(ctx, bindings); err != nil {
		return err
	}
	roles := &rbacv1.RoleList{}
	if err := r.reader().List(ctx, roles); err != nil {
		return err
	}
	var objects []client.Object
	for i := range bindings.Items {
		objects = append(objects, &bindings.Items[i])
	}
	for i := range roles.Items {
		objects = append(objects, &roles.Items[i])
	}
	for _, object := range objects {
		annotations := object.GetAnnotations()
		if annotations[cetiadmin.AnnotationInstallationUID] != string(state.RecordUID) || annotations[cetiadmin.AnnotationRuntimeGrantAgent] == "" ||
			(agentName != "" && annotations[cetiadmin.AnnotationRuntimeGrantAgent] != agentName) {
			continue
		}
		owners := object.GetOwnerReferences()
		if annotations[cetiadmin.AnnotationSourceUID] != string(state.Source.UID) || annotations[cetiadmin.AnnotationRuntimeGrantNamespaceUID] == "" ||
			annotations[cetiadmin.AnnotationRuntimeGrantNamespace] == "" || annotations[cetiadmin.AnnotationCreationID] == "" || len(owners) != 1 ||
			owners[0].APIVersion != api.GroupVersion.String() || owners[0].Kind != "Agent" || owners[0].Name != annotations[cetiadmin.AnnotationRuntimeGrantAgent] ||
			string(owners[0].UID) != annotations[cetiadmin.AnnotationRuntimeGrantAgentUID] || owners[0].UID == "" {
			failures = append(failures, cetiadmin.ErrConflict)
			continue
		}
		if binding, ok := object.(*rbacv1.RoleBinding); ok {
			if !cetiAdminSubjectsOwned(binding.Subjects, state) {
				failures = append(failures, cetiadmin.ErrConflict)
				continue
			}
		}
		failures = append(failures, r.deleteRuntimeAccessResource(ctx, object))
	}
	failures = append(failures, r.revokeRuntimeExec(ctx, state.Record, agentName))
	return errors.Join(failures...)
}

func (r *CetiAdminReconciler) revokeNamespaceAccess(ctx context.Context, state *cetiadmin.Installation) error {
	return errors.Join(r.revokeRuntimeNamespace(ctx, state, ""), r.revokeDiagnosticAccess(ctx, state))
}

func (r *CetiAdminReconciler) deleteRuntimeAccessResource(ctx context.Context, object client.Object) error {
	uid, version := object.GetUID(), object.GetResourceVersion()
	if uid == "" || version == "" {
		return cetiadmin.ErrConflict
	}
	writeErr := r.Delete(ctx, object, client.Preconditions{UID: &uid, ResourceVersion: &version})
	current := object.DeepCopyObject().(client.Object)
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(object), current)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return errors.Join(writeErr, err, fmt.Errorf("CetiAdmin namespace access withdrawal has not been observed"))
}
