// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *AgentReconciler) sandboxGrantJournal() *sandboxpolicy.GrantJournalStore {
	source := client.ObjectKey{}
	if provider, ok := r.CetiAdmin.(interface{ CetiAdminSource() client.ObjectKey }); ok {
		source = provider.CetiAdminSource()
	}
	return &sandboxpolicy.GrantJournalStore{Client: r.Client, APIReader: r.customReader(), Namespace: r.OperatorNamespace,
		PlatformAccessVerifier: cetiadmin.RuntimeAccessBindingVerifier(source)}
}

func (r *AgentReconciler) sandboxPreparationStateNeeded(agent *api.Agent) bool {
	// A namespace alone does not enable preparation. During deletion the hold
	// also protects retries after the preparation marker has been released.
	// Reconciled lifetimes retain their grant journal through bootstrap ownership.
	cleanupPending := !agent.DeletionTimestamp.IsZero() &&
		(controllerutil.ContainsFinalizer(agent, sandbox.HoldFinalizer) || controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer))
	return controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) ||
		(r.OperatorNamespace != "" && (r.SandboxPreparation != nil || cleanupPending ||
			controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer)))
}

func (r *AgentReconciler) revokeSandboxPreparationGrants(ctx context.Context, agent *api.Agent) error {
	current := &api.Agent{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
		return err
	}
	if current.UID != agent.UID {
		return fmt.Errorf("Sandbox Agent UID changed before grant cleanup")
	}
	agent = current
	if !r.sandboxPreparationStateNeeded(agent) {
		return nil
	}
	journal := r.sandboxGrantJournal()
	read := journal.Read
	if !agent.DeletionTimestamp.IsZero() {
		read = journal.ReadForCleanup
	}
	bindings, err := read(ctx, agent.Name, agent.UID)
	if apierrors.IsNotFound(err) && !controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) {
		return r.checkMissingReconciledGrantJournal(ctx, agent)
	}
	if err != nil {
		return fmt.Errorf("Sandbox grant cleanup inventory is unavailable: %w", err)
	}
	if len(bindings) == 0 {
		return nil
	}
	if err := r.prepareReconciledGrantRevocation(ctx, agent, bindings); err != nil {
		confirmed, readErr := read(ctx, agent.Name, agent.UID)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		confirmed = slices.DeleteFunc(confirmed, func(ref sandboxpolicy.BindingReference) bool {
			return ref.RoleRef.Kind == "ClusterRole" && ref.UID == ""
		})
		return errors.Join(err, journal.Revoke(ctx, agent.Name, agent.UID, confirmed))
	}
	return journal.Revoke(ctx, agent.Name, agent.UID, bindings)
}

func (r *AgentReconciler) sandboxSourceAccess(attempt *sandboxPreparationAttempt, pods ...corev1.PodSpec) (*rbacv1.Role, *rbacv1.RoleBinding, sandboxpolicy.BindingReference, error) {
	account := r.SandboxPreparation.AdmissionServiceAccount
	if len(validation.IsDNS1123Subdomain(account)) != 0 || len(validation.IsDNS1123Label(r.OperatorNamespace)) != 0 ||
		attempt.input.Namespace.Name == r.OperatorNamespace {
		return nil, nil, sandboxpolicy.BindingReference{}, fmt.Errorf("Sandbox admission source access requires an explicit separate platform ServiceAccount")
	}
	names := map[string][]string{}
	// Pinned bootstrap authentication is rechecked by admission even when the
	// Custom main does not mount it. Grant only the approved, exact sources.
	for _, source := range attempt.sources {
		if source.Kind != "Secret" {
			continue
		}
		matchesTLSReference := func(ref ingressmtls.Reference) bool {
			return source.APIVersion == "v1" && source.ValidIdentity() &&
				source.Namespace == ref.Namespace && source.Name == ref.Name &&
				source.UID == ref.UID && source.ResourceVersion == ref.ResourceVersion
		}
		endpointSecret := false
		if plan := attempt.input.Controls.IngressMTLS; plan != nil {
			if slices.ContainsFunc(plan.ClientSecrets(), matchesTLSReference) {
				continue // Client credentials have their own namespace-scoped read grants.
			}
			endpointSecret = matchesTLSReference(plan.ServerSecret)
		}
		approved := slices.Contains(attempt.input.AuthorizedSecrets, source.Name) ||
			(attempt.input.ImagePullSecret != nil && source.Name == attempt.input.ImagePullSecret.Name &&
				source.UID == attempt.input.ImagePullSecret.UID) || endpointSecret
		if source.APIVersion != "v1" || source.Namespace != attempt.input.Namespace.Name || !source.ValidIdentity() || !approved {
			return nil, nil, sandboxpolicy.BindingReference{}, fmt.Errorf("Sandbox source-read grant contains an unapproved Secret source")
		}
		names["secrets"] = append(names["secrets"], source.Name)
	}
	for _, pod := range pods {
		for _, source := range sandboxworkload.PodSourceReferences(pod, attempt.input.Namespace.Name) {
			switch source.Kind {
			case "ConfigMap":
				names["configmaps"] = append(names["configmaps"], source.Name)
			case "Secret":
				endpointSecret := attempt.input.Controls.IngressMTLS != nil &&
					source.Name == attempt.input.Controls.IngressMTLS.ServerSecret.Name
				if !slices.Contains(attempt.input.AuthorizedSecrets, source.Name) &&
					(attempt.input.ImagePullSecret == nil || source.Name != attempt.input.ImagePullSecret.Name) && !endpointSecret {
					return nil, nil, sandboxpolicy.BindingReference{}, fmt.Errorf("Sandbox source-read grant contains an unapproved Secret")
				}
				names["secrets"] = append(names["secrets"], source.Name)
			}
		}
	}
	var rules []rbacv1.PolicyRule
	for _, resource := range []string{"configmaps", "secrets"} {
		if len(names[resource]) != 0 {
			rules = append(rules, rbacv1.PolicyRule{
				APIGroups: []string{""}, Resources: []string{resource},
				ResourceNames: uniqueReferences(names[resource]), Verbs: []string{"get"},
			})
		}
	}
	if len(rules) == 0 {
		return nil, nil, sandboxpolicy.BindingReference{}, fmt.Errorf("Sandbox native source inventory is empty")
	}
	if attempt.reconciled {
		rules = append(rules,
			rbacv1.PolicyRule{APIGroups: []string{"apps"}, Resources: []string{"deployments", "replicasets"}, Verbs: []string{"get"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"get"}},
		)
	}
	revision, err := sandboxPreparationDigest(struct {
		Namespace sandboxworkload.ObjectIdentity
		Agent     sandboxworkload.ObjectIdentity
		Account   string
		Rules     []rbacv1.PolicyRule
	}{attempt.input.Namespace, sandboxworkload.ObjectIdentity{Name: attempt.agent.Name, UID: attempt.agent.UID}, account, rules})
	if err != nil {
		return nil, nil, sandboxpolicy.BindingReference{}, err
	}
	metadata := metav1.ObjectMeta{
		Name: "cetirun-sandbox-sources", Namespace: attempt.input.Namespace.Name,
		OwnerReferences: []metav1.OwnerReference{sandboxworkload.AgentOwner(attempt.agent.Name, attempt.agent.UID)},
		Labels:          map[string]string{sandboxpolicy.AgentUIDLabel: string(attempt.agent.UID)},
		Annotations:     map[string]string{sandboxpolicy.GrantRevisionAnnotation: revision},
	}
	role := &rbacv1.Role{ObjectMeta: *metadata.DeepCopy(), Rules: rules}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: *metadata.DeepCopy(),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: r.OperatorNamespace, Name: account}},
	}
	ref := sandboxpolicy.BindingReference{
		Kind: "RoleBinding", Namespace: binding.Namespace, Name: binding.Name,
		AgentName: attempt.agent.Name, AgentUID: attempt.agent.UID,
		RoleRef: binding.RoleRef, ServiceAccountName: account, ServiceAccountNamespace: r.OperatorNamespace,
		GrantRevision: revision,
	}
	return role, binding, ref, nil
}

func (r *AgentReconciler) applySandboxSourceAccess(ctx context.Context, agent *api.Agent, desiredRole *rbacv1.Role, desiredBinding *rbacv1.RoleBinding, access sandboxpolicy.BindingReference) error {
	currentRole := &rbacv1.Role{}
	err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(desiredRole), currentRole)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desiredRole); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if !sandboxPreparationOwned(currentRole, agent) {
			return fmt.Errorf("Sandbox source-read Role has foreign or terminating ownership")
		}
		if !equality.Semantic.DeepEqual(currentRole.Rules, desiredRole.Rules) ||
			currentRole.Annotations[sandboxpolicy.GrantRevisionAnnotation] != desiredRole.Annotations[sandboxpolicy.GrantRevisionAnnotation] {
			base := currentRole.DeepCopy()
			currentRole.Rules = desiredRole.Rules
			if currentRole.Annotations == nil {
				currentRole.Annotations = map[string]string{}
			}
			currentRole.Annotations[sandboxpolicy.GrantRevisionAnnotation] = desiredRole.Annotations[sandboxpolicy.GrantRevisionAnnotation]
			if err := r.Patch(ctx, currentRole, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	}
	journal := r.sandboxGrantJournal()
	recorded, err := journal.Read(ctx, agent.Name, agent.UID)
	if err != nil {
		return err
	}
	var recordedBinding *sandboxpolicy.BindingReference
	for i := range recorded {
		expected := recorded[i]
		expected.UID = ""
		if expected == access {
			recordedBinding = &recorded[i]
			break
		}
	}
	if recordedBinding == nil {
		return fmt.Errorf("Sandbox source-read RoleBinding has no matching durable grant intent")
	}
	currentBinding := &rbacv1.RoleBinding{}
	err = r.APIReader.Get(ctx, client.ObjectKeyFromObject(desiredBinding), currentBinding)
	if apierrors.IsNotFound(err) {
		if recordedBinding.UID != "" {
			if err := journal.RefreshBindingIntents(ctx, agent.Name, agent.UID, []sandboxpolicy.BindingReference{access}); err != nil {
				return err
			}
		}
		return r.Create(ctx, desiredBinding)
	}
	if err != nil {
		return err
	}
	if !sandboxPreparationOwned(currentBinding, agent) ||
		(recordedBinding.UID != "" && currentBinding.UID != recordedBinding.UID) ||
		currentBinding.RoleRef != desiredBinding.RoleRef ||
		!equality.Semantic.DeepEqual(currentBinding.Subjects, desiredBinding.Subjects) ||
		currentBinding.Annotations[sandboxpolicy.GrantRevisionAnnotation] != desiredBinding.Annotations[sandboxpolicy.GrantRevisionAnnotation] {
		return fmt.Errorf("Sandbox source-read RoleBinding differs from its recorded identity")
	}
	return nil
}

func sandboxPreparationOwned(object client.Object, agent *api.Agent) bool {
	return object.GetUID() != "" && object.GetResourceVersion() != "" && object.GetDeletionTimestamp().IsZero() &&
		object.GetLabels()[sandboxpolicy.AgentUIDLabel] == string(agent.UID) &&
		equality.Semantic.DeepEqual(object.GetOwnerReferences(), []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)})
}

func (r *AgentReconciler) sandboxBindingUIDs(ctx context.Context, desired []sandboxpolicy.BindingReference) ([]sandboxpolicy.BindingReference, error) {
	result := slices.Clone(desired)
	for i := range result {
		ref := &result[i]
		var object client.Object
		switch ref.Kind {
		case "RoleBinding":
			object = &rbacv1.RoleBinding{}
		case "ClusterRoleBinding":
			object = &rbacv1.ClusterRoleBinding{}
		default:
			return nil, fmt.Errorf("invalid Sandbox binding kind")
		}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, object); err != nil {
			return nil, err
		}
		agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: ref.AgentName, UID: ref.AgentUID}}
		if !sandboxPreparationOwned(object, agent) || object.GetAnnotations()[sandboxpolicy.GrantRevisionAnnotation] != ref.GrantRevision {
			return nil, fmt.Errorf("Sandbox grant changed before recording its UID")
		}
		var role rbacv1.RoleRef
		var subjects []rbacv1.Subject
		switch binding := object.(type) {
		case *rbacv1.RoleBinding:
			role, subjects = binding.RoleRef, binding.Subjects
		case *rbacv1.ClusterRoleBinding:
			role, subjects = binding.RoleRef, binding.Subjects
		}
		wanted := []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: ref.ServiceAccountNamespace, Name: ref.ServiceAccountName}}
		if role != ref.RoleRef || !equality.Semantic.DeepEqual(subjects, wanted) {
			return nil, fmt.Errorf("Sandbox grant contents differ from durable inventory")
		}
		ref.UID = object.GetUID()
	}
	return result, nil
}
