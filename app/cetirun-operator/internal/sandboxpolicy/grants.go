// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"fmt"
	"reflect"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

// VerifyPolicyGrants checks the requested approvals at the trusted snapshot
// boundary using authoritative ClusterRole reads (the manager's APIReader, not
// its cache). Rule and field order/duplicates are normalized, but role UID and
// resourceVersion must match exactly. It never changes an approval or bans
// explicitly approved privileges. Consumers must repeat verification when
// refreshing evidence; this observation is not a future execution permit.
// Compile deliberately remains a pure consumer of trusted snapshot contents.
func VerifyPolicyGrants(ctx context.Context, reader client.Reader, policy *api.AgentPolicy, inventory []ApprovedGrant) error {
	if policy == nil || policy.UID == "" || !policy.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: a persisted non-terminating policy is required", ErrUnavailable)
	}
	if err := validateControlInput(policy.Spec, Platform{Grants: inventory}); err != nil {
		return err
	}
	normalized, errs := sandbox.NormalizeAgentPolicy(policy.Spec)
	if len(errs) != 0 {
		return errs.ToAggregate()
	}
	grants, err := approvedGrants(policy.UID, normalized.KubernetesAccess, inventory)
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		return nil
	}
	if reader == nil {
		return fmt.Errorf("%w: an authoritative role reader is required", ErrUnavailable)
	}
	roles := make(map[string]*rbacv1.ClusterRole)
	for _, grant := range grants {
		role, observed := roles[grant.RoleRef.Name]
		if !observed {
			role = &rbacv1.ClusterRole{}
			if err := reader.Get(ctx, client.ObjectKey{Name: grant.RoleRef.Name}, role); err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("%w: approved ClusterRole is missing: %w", ErrUnapprovedGrant, err)
				}
				return fmt.Errorf("%w: read approved ClusterRole: %w", ErrUnavailable, err)
			}
			roles[grant.RoleRef.Name] = role
		}
		if !role.DeletionTimestamp.IsZero() || role.UID != grant.RoleUID || role.ResourceVersion != grant.RoleResourceVersion {
			return fmt.Errorf("%w: ClusterRole %q no longer matches its approved UID and resourceVersion", ErrUnapprovedGrant, grant.RoleRef.Name)
		}
		rules, err := canonicalRoleRules(role.Rules)
		if err != nil {
			return fmt.Errorf("%w: normalize observed ClusterRole rules: %w", ErrUnavailable, err)
		}
		if !reflect.DeepEqual(rules, grant.Rules) {
			return fmt.Errorf("%w: ClusterRole %q no longer matches its approved rules", ErrUnapprovedGrant, grant.RoleRef.Name)
		}
	}
	return nil
}
