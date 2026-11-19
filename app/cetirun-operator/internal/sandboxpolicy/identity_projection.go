// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	entraClientAnnotation = "azure.workload.identity/client-id"
	entraTenantAnnotation = "azure.workload.identity/tenant-id"
)

// projectIdentity is shared by compilation and structural projection validation.
// Only the compiler selects approved identities and Kubernetes audiences.
func projectIdentity(plan *IdentityPlan, kubernetes bool, entra *EntraIdentity, audience string, expiration int64) {
	mode := int32(0440)
	if kubernetes {
		plan.Volumes = append(plan.Volumes, corev1.Volume{
			Name: "sandbox-kubernetes-api",
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: &mode,
				Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Audience: audience, ExpirationSeconds: &expiration, Path: "token",
					}},
					{ConfigMap: &corev1.ConfigMapProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
						Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
					}},
					{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{
						Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
					}}}},
				},
			}},
		})
		plan.VolumeMounts = append(plan.VolumeMounts, corev1.VolumeMount{
			Name: "sandbox-kubernetes-api", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true,
		})
	}
	if entra != nil {
		plan.Volumes = append(plan.Volumes, corev1.Volume{
			Name: "sandbox-entra-exchange",
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: &mode, Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Audience: EntraAudience, ExpirationSeconds: &expiration, Path: "azure-identity-token",
					},
				}},
			}},
		})
		plan.VolumeMounts = append(plan.VolumeMounts, corev1.VolumeMount{
			Name: "sandbox-entra-exchange", MountPath: "/var/run/secrets/azure/tokens", ReadOnly: true,
		})
		plan.Env = []corev1.EnvVar{
			{Name: "AZURE_CLIENT_ID", Value: entra.ClientID},
			{Name: "AZURE_TENANT_ID", Value: entra.TenantID},
			{Name: "AZURE_FEDERATED_TOKEN_FILE", Value: "/var/run/secrets/azure/tokens/azure-identity-token"},
		}
		plan.ServiceAccount.Annotations[entraClientAnnotation] = entra.ClientID
		plan.ServiceAccount.Annotations[entraTenantAnnotation] = entra.TenantID
	}
}

// ValidateIdentityProjection validates the compiler's token, mount and environment
// contract before rendering. It neither approves identifiers found on a
// ServiceAccount nor observes federation, cloud permissions or current sources.
// Callers must separately verify ownership, current approval and source identity.
func ValidateIdentityProjection(policy api.AgentPolicySpec, plan IdentityPlan) error {
	kubernetes, entra := policy.KubernetesAccess != nil, policy.EntraID.Enabled
	if plan.AutomountServiceAccountToken {
		return fmt.Errorf("%w: identity projection cannot enable token automount", ErrUnapprovedGrant)
	}
	if !kubernetes && !entra {
		if plan.ServiceAccountName != "default" || plan.ServiceAccount != nil ||
			len(plan.Volumes)+len(plan.VolumeMounts)+len(plan.Env)+
				len(plan.RoleBindings)+len(plan.ClusterRoleBindings)+len(plan.Bindings) != 0 {
			return fmt.Errorf("%w: disabled identity cannot project credentials", ErrUnapprovedGrant)
		}
		return nil
	}
	account := plan.ServiceAccount
	if account == nil || account.Name == "default" || account.Name != plan.ServiceAccountName ||
		len(validation.IsDNS1123Subdomain(account.Name)) != 0 || len(validation.IsDNS1123Label(account.Namespace)) != 0 ||
		account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken ||
		len(account.Secrets)+len(account.ImagePullSecrets) != 0 ||
		(kubernetes && policy.KubernetesAccess.ServiceAccountName != account.Name) {
		return fmt.Errorf("%w: identity projection requires the bound dedicated ServiceAccount", ErrUnapprovedGrant)
	}
	var approved *EntraIdentity
	if entra {
		approved = &EntraIdentity{
			ClientID: account.Annotations[entraClientAnnotation], TenantID: account.Annotations[entraTenantAnnotation],
		}
		if !identityValue(approved.ClientID) || !identityValue(approved.TenantID) {
			return fmt.Errorf("%w: Entra projection requires explicit ServiceAccount identifiers", ErrUnapprovedGrant)
		}
	} else if _, present := account.Annotations[entraClientAnnotation]; present {
		return fmt.Errorf("%w: Kubernetes-only identity cannot carry Entra annotations", ErrUnapprovedGrant)
	} else if _, present := account.Annotations[entraTenantAnnotation]; present {
		return fmt.Errorf("%w: Kubernetes-only identity cannot carry Entra annotations", ErrUnapprovedGrant)
	}
	if !kubernetes && len(plan.RoleBindings)+len(plan.ClusterRoleBindings)+len(plan.Bindings) != 0 {
		return fmt.Errorf("%w: Entra-only identity cannot carry Kubernetes bindings", ErrUnapprovedGrant)
	}
	var expiration int64
	audience := ""
	for _, volume := range plan.Volumes {
		projection := volume.Projected
		if projection == nil || len(projection.Sources) == 0 || projection.Sources[0].ServiceAccountToken == nil {
			return fmt.Errorf("%w: identity requires the compiler token projection", ErrUnapprovedGrant)
		}
		token := projection.Sources[0].ServiceAccountToken
		if token.ExpirationSeconds == nil || *token.ExpirationSeconds < 600 || *token.ExpirationSeconds > 3600 ||
			(expiration != 0 && expiration != *token.ExpirationSeconds) {
			return fmt.Errorf("%w: identity token lifetimes must be bounded and consistent", ErrUnapprovedGrant)
		}
		expiration = *token.ExpirationSeconds
		if volume.Name == "sandbox-kubernetes-api" {
			audience = token.Audience
		}
	}
	if kubernetes && (!identityValue(audience) || audience == EntraAudience) {
		return fmt.Errorf("%w: Kubernetes identity requires a distinct API audience", ErrUnapprovedGrant)
	}
	expected := IdentityPlan{ServiceAccount: &corev1.ServiceAccount{}}
	expected.ServiceAccount.Annotations = map[string]string{}
	projectIdentity(&expected, kubernetes, approved, audience, expiration)
	if !equality.Semantic.DeepEqual(plan.Volumes, expected.Volumes) ||
		!equality.Semantic.DeepEqual(plan.VolumeMounts, expected.VolumeMounts) ||
		!equality.Semantic.DeepEqual(plan.Env, expected.Env) {
		return fmt.Errorf("%w: identity projection differs from the requested compiler contract", ErrUnapprovedGrant)
	}
	return nil
}
