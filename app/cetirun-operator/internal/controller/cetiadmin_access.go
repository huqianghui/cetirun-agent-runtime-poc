// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GrantInstallationAccess is the production GrantAccess callback. It never grants
// before admission is ready, and withdraws partially written grants on error.
func (r *CetiAdminReconciler) GrantInstallationAccess(ctx context.Context, binding cetiadmin.Binding, state *cetiadmin.Installation) (grantErr error) {
	if state == nil {
		return cetiadmin.ErrUnbound
	}
	defer func() {
		if grantErr != nil {
			grantErr = errors.Join(grantErr, r.RevokeInstallationAccess(ctx, state))
		}
	}()
	if binding.ServiceAccountUsername() == "" || binding.Record() != state.Record || r.AdmissionReady == nil {
		return cetiadmin.ErrUnbound
	}
	if err := r.AdmissionReady(ctx); err != nil {
		return fmt.Errorf("AdmissionUnavailable: %w", err)
	}
	if _, err := r.currentAccessBinding(ctx, state); err != nil {
		return err
	}
	plan, err := r.planInstallationAccess(binding, state)
	if err != nil {
		return err
	}
	for _, desired := range plan {
		if err := r.ensureAccessResource(ctx, state, desired); err != nil {
			return err
		}
	}
	return r.verifyInstallationAccess(ctx, binding, state)
}

// RevokeInstallationAccess is the production RevokeAccess callback. Binding deletion
// is UID/RV conditional and must be observed before roles are removed.
func (r *CetiAdminReconciler) RevokeInstallationAccess(ctx context.Context, state *cetiadmin.Installation) error {
	if state == nil {
		return cetiadmin.ErrUnbound
	}
	state.Hold = true
	runtimeErr := r.revokeNamespaceAccess(ctx, state)
	withdrawErr := r.withdrawResources(ctx, state)
	checkpoint, err := r.checkpointObject(ctx, state)
	if err != nil {
		return errors.Join(runtimeErr, withdrawErr, err)
	}
	return errors.Join(runtimeErr, withdrawErr, r.saveState(ctx, checkpoint, state))
}

func (r *CetiAdminReconciler) currentAccessBinding(ctx context.Context, state *cetiadmin.Installation) (cetiadmin.Binding, error) {
	_, current, err := r.readState(ctx)
	if err != nil {
		return cetiadmin.Binding{}, err
	}
	if current.RecordUID != state.RecordUID || current.Record != state.Record || state.OperatorImage != r.OperatorImage || state.RuntimeVersion != r.ChartVersion {
		return cetiadmin.Binding{}, cetiadmin.ErrConflict
	}
	source := &corev1.ConfigMap{}
	if err := r.reader().Get(ctx, r.Source, source); err != nil {
		return cetiadmin.Binding{}, err
	}
	config, err := cetiadmin.ParseConfig(source.Data[cetiadmin.ConfigKey])
	if err != nil {
		return cetiadmin.Binding{}, err
	}
	if r.Runtime == nil {
		return cetiadmin.Binding{}, cetiadmin.ErrUnbound
	}
	if err := config.ValidateMCPAudience(r.Runtime.ConnectionsMCPAudience); err != nil {
		return cetiadmin.Binding{}, err
	}
	observed, err := r.observe(ctx, source, state.Record)
	if err != nil {
		return cetiadmin.Binding{}, err
	}
	if err := r.validateRetainedObjects(ctx, state); err != nil {
		return cetiadmin.Binding{}, err
	}
	if err := r.validateInstalled(state, observed); err != nil {
		return cetiadmin.Binding{}, err
	}
	if state.Bootstrapped && observed.Agent.Spec.Suspended {
		return cetiadmin.Binding{}, cetiadmin.ErrUnbound
	}
	if err := r.migrationSourceStopped(ctx, config); err != nil {
		return cetiadmin.Binding{}, err
	}
	return cetiadmin.ResolveBinding(config, &state.Record, observed)
}

func (r *CetiAdminReconciler) verifyInstallationAccess(ctx context.Context, binding cetiadmin.Binding, state *cetiadmin.Installation) error {
	if r.AdmissionReady == nil {
		return cetiadmin.ErrUnbound
	}
	if err := r.AdmissionReady(ctx); err != nil {
		return err
	}
	current, err := r.currentAccessBinding(ctx, state)
	if err != nil {
		return err
	}
	if current.Record() != binding.Record() {
		return cetiadmin.ErrConflict
	}
	plan, err := r.planInstallationAccess(current, state)
	if err != nil {
		return err
	}
	if state.Creation != nil || len(state.Resources) != len(plan) {
		return cetiadmin.ErrUnbound
	}
	for _, desired := range plan {
		resource := accessResource(state, desired)
		if resource == nil {
			return cetiadmin.ErrUnbound
		}
		if err := r.verifyAccessResource(ctx, state, *resource, desired); err != nil {
			return err
		}
	}
	return nil
}

func accessResource(state *cetiadmin.Installation, object client.Object) *cetiadmin.ManagedResource {
	for i := range state.Resources {
		ref := &state.Resources[i]
		if ref.Name == object.GetName() && ref.Namespace == object.GetNamespace() && ref.Kind == object.GetObjectKind().GroupVersionKind().Kind {
			return ref
		}
	}
	return nil
}

func (r *CetiAdminReconciler) verifyAccessResource(ctx context.Context, state *cetiadmin.Installation, ref cetiadmin.ManagedResource, desired client.Object) error {
	actual := desired.DeepCopyObject().(client.Object)
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(desired), actual); err != nil {
		return err
	}
	if !identityMatches(ref.ObjectIdentity, actual) || ref.CreationID == "" || actual.GetAnnotations()[cetiadmin.AnnotationCreationID] != ref.CreationID ||
		actual.GetAnnotations()[cetiadmin.AnnotationInstallationUID] != string(state.RecordUID) || !sameCetiAdminCreation(desired, actual) {
		return cetiadmin.ErrConflict
	}
	return nil
}

func (r *CetiAdminReconciler) ensureAccessResource(ctx context.Context, state *cetiadmin.Installation, desired client.Object) error {
	if ref := accessResource(state, desired); ref != nil {
		return r.verifyAccessResource(ctx, state, *ref, desired)
	}
	checkpoint, err := r.checkpointObject(ctx, state)
	if err != nil {
		return err
	}
	var identity cetiadmin.ObjectIdentity
	_, err = r.ensureCreated(ctx, checkpoint, state, desired.GetObjectKind().GroupVersionKind().Kind, desired, &identity)
	return err
}

func (r *CetiAdminReconciler) planInstallationAccess(binding cetiadmin.Binding, state *cetiadmin.Installation) ([]client.Object, error) {
	if binding.ServiceAccountUsername() == "" || r.Runtime == nil {
		return nil, cetiadmin.ErrUnbound
	}
	clusterRules, namespaceRules, err := r.diagnosticRules()
	if err != nil {
		return nil, err
	}
	clusterRules = append(clusterRules, cetiadmin.ClusterOperationRules()...)
	name := cetiadmin.AccessName(state.RecordUID)
	ownerAgent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name, UID: state.Agent.UID}}
	metadata := func(namespace, objectName string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: objectName, Namespace: namespace, OwnerReferences: []metav1.OwnerReference{registryauth.Owner(ownerAgent)},
			Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(state.Source.UID), cetiadmin.AnnotationInstallationUID: string(state.RecordUID)}}
	}
	subjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}}
	var objects, bindings []client.Object
	objects = append(objects, &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"}, ObjectMeta: metadata("", name), Rules: clusterRules})
	bindings = append(bindings, &rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metadata("", name), Subjects: subjects,
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}})
	for _, capability := range []struct {
		suffix    string
		protected bool
	}{{"runtime", false}, {"protected", true}} {
		rules := append(slices.Clone(namespaceRules), cetiadmin.NamespaceOperationRules(capability.protected)...)
		objects = append(objects, &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"}, ObjectMeta: metadata("", name+"-"+capability.suffix), Rules: rules})
	}
	objects = append(objects, &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"}, ObjectMeta: metadata("", name+"-reader"), Rules: cetiadmin.RuntimeReaderRules()})
	objects = append(objects, &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"}, ObjectMeta: metadata("", name+"-diagnostics"), Rules: namespaceRules})
	readerName := name + "-admission"
	objects = append(objects, &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"}, ObjectMeta: metadata("", readerName), Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"persistentvolumes"}, Verbs: []string{"get"}},
		{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"clusterrolebindings"}, Verbs: []string{"get"}},
		{APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"}, Verbs: []string{"get"}},
	}})
	bindings = append(bindings, &rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metadata("", readerName),
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "cetirun-sandbox-admission", Namespace: state.Source.Namespace}},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: readerName}})
	return append(objects, bindings...), nil
}

func (r *CetiAdminReconciler) diagnosticRules() ([]rbacv1.PolicyRule, []rbacv1.PolicyRule, error) {
	var cluster, namespaces []rbacv1.PolicyRule
	for _, resource := range cetiadmin.DiagnosticResources() {
		mapping, err := r.RESTMapper().RESTMapping(schema.GroupKind{Group: resource.Group, Kind: resource.Kind})
		if meta.IsNoMatchError(err) {
			if resource.Group == "" || resource.Group == api.GroupVersion.Group || resource.Group == "apps" || resource.Group == "batch" || resource.Group == rbacv1.GroupName {
				return nil, nil, fmt.Errorf("required diagnostic API is unavailable: %s/%s", resource.Group, resource.Resource)
			}
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("diagnostic discovery failed: %w", err)
		}
		if mapping == nil || mapping.Scope == nil || mapping.GroupVersionKind.GroupKind() != (schema.GroupKind{Group: resource.Group, Kind: resource.Kind}) ||
			mapping.Resource.Group != resource.Group || mapping.Resource.Resource == "" || strings.ContainsAny(mapping.Resource.Resource, "/*") ||
			(mapping.Scope.Name() != meta.RESTScopeNameNamespace && mapping.Scope.Name() != meta.RESTScopeNameRoot) {
			return nil, nil, fmt.Errorf("diagnostic resource mapping changed: %s/%s", resource.Group, resource.Resource)
		}
		namespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace
		rule := cetiadmin.ReadRule(resource.Group, mapping.Resource.Resource)
		if namespaced {
			namespaces = append(namespaces, rule)
		} else {
			cluster = append(cluster, rule)
		}
	}
	// Platform logs are read-only; exec never appears in this role.
	namespaces = append(namespaces, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}})
	return cluster, namespaces, nil
}

func (r *CetiAdminReconciler) recoverPendingAccess(ctx context.Context, state *cetiadmin.Installation) error {
	if state.Creation == nil {
		return nil
	}
	kind := state.Creation.Resource.Kind
	if kind == "Agent" || kind == "Team" || kind == "Namespace" || kind == "ServiceAccount" {
		return nil
	}
	request, err := r.creationObject(state)
	if err != nil {
		return err
	}
	actual := request.DeepCopyObject().(client.Object)
	err = r.reader().Get(ctx, client.ObjectKeyFromObject(request), actual)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if !sameCetiAdminCreation(request, actual) || actual.GetUID() == "" {
			return cetiadmin.ErrConflict
		}
		ref := state.Creation.Resource
		ref.ObjectIdentity = identityOf(actual)
		state.Resources = append(state.Resources, ref)
	}
	state.Creation, state.PendingCreate = nil, ""
	return nil
}

func (r *CetiAdminReconciler) withdrawResources(ctx context.Context, state *cetiadmin.Installation) error {
	var errs []error
	if err := r.recoverPendingAccess(ctx, state); err != nil {
		errs = append(errs, err)
	}
	for _, bindingsOnly := range []bool{true, false} {
		remaining := make([]cetiadmin.ManagedResource, 0, len(state.Resources))
		for _, ref := range state.Resources {
			isBinding := ref.Kind == "RoleBinding" || ref.Kind == "ClusterRoleBinding"
			if isBinding != bindingsOnly {
				remaining = append(remaining, ref)
				continue
			}
			if err := r.deleteAccessResource(ctx, state, ref); err != nil {
				errs = append(errs, err)
				remaining = append(remaining, ref)
			}
		}
		state.Resources = remaining
		if len(errs) != 0 {
			break
		}
	}
	return errors.Join(errs...)
}

func (r *CetiAdminReconciler) deleteAccessResource(ctx context.Context, state *cetiadmin.Installation, ref cetiadmin.ManagedResource) error {
	if ref.UID == "" || ref.CreationID == "" || state.RecordUID == "" || state.Agent.UID == "" {
		return cetiadmin.ErrConflict
	}
	switch ref.Kind {
	case "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
	case "ConfigMap":
		if ref.Name != "cetiadmin-targets" || ref.Namespace != state.Namespace.Name {
			return cetiadmin.ErrConflict
		}
	default:
		return cetiadmin.ErrConflict
	}
	object, gvk, err := installationObject(ref.Kind)
	if err != nil || ref.APIVersion != gvk.GroupVersion().String() {
		return cetiadmin.ErrConflict
	}
	key := client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}
	if err := r.reader().Get(ctx, key, object); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name, UID: state.Agent.UID}}
	if object.GetUID() != ref.UID || object.GetAnnotations()[cetiadmin.AnnotationCreationID] != ref.CreationID ||
		object.GetAnnotations()[cetiadmin.AnnotationInstallationUID] != string(state.RecordUID) ||
		object.GetAnnotations()[cetiadmin.AnnotationSourceUID] != string(state.Source.UID) || !hasAgentOwner(object, agent) {
		return fmt.Errorf("%w: refusing to delete foreign %s %s", cetiadmin.ErrConflict, ref.Kind, ref.Name)
	}
	switch binding := object.(type) {
	case *rbacv1.RoleBinding:
		if !cetiAdminSubjectsOwned(binding.Subjects, state) {
			return cetiadmin.ErrConflict
		}
	case *rbacv1.ClusterRoleBinding:
		if !cetiAdminSubjectsOwned(binding.Subjects, state) {
			return cetiadmin.ErrConflict
		}
	}
	version := object.GetResourceVersion()
	deleteErr := r.Delete(ctx, object, client.Preconditions{UID: &ref.UID, ResourceVersion: &version})
	readErr := r.reader().Get(ctx, key, object)
	if apierrors.IsNotFound(readErr) {
		return nil
	}
	if readErr != nil {
		return errors.Join(deleteErr, readErr)
	}
	return errors.Join(deleteErr, fmt.Errorf("withdrawal of %s %s has not been observed", ref.Kind, ref.Name))
}

func cetiAdminSubjectsOwned(subjects []rbacv1.Subject, state *cetiadmin.Installation) bool {
	admin := rbacv1.Subject{Kind: "ServiceAccount", Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}
	reader := rbacv1.Subject{Kind: "ServiceAccount", Name: "cetirun-sandbox-admission", Namespace: state.Source.Namespace}
	return len(subjects) == 1 && (subjects[0] == admin || subjects[0] == reader)
}
