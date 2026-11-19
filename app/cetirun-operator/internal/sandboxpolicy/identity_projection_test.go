// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/utils/ptr"
)

func entraCompilerFixture() CompileInput {
	input := compilerFixture()
	input.Resolution.AgentPolicy.EntraID.Enabled = true
	input.Resolution.Capabilities.KubernetesAccess = false
	input.Platform.Identities = []ApprovedIdentity{{
		AgentUID: input.Resolution.Binding.AgentUID, Namespace: input.Namespace,
		ServiceAccountName: "example-entra", Revision: "synthetic-approval-v1",
		Entra: &EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"},
	}}
	return input
}

func TestEntraCompilerRequiresCurrentExplicitAgentApproval(t *testing.T) {
	for name, mutate := range map[string]func(*CompileInput){
		"missing approval": func(i *CompileInput) { i.Platform.Identities = nil },
		"wrong Agent":      func(i *CompileInput) { i.Platform.Identities[0].AgentUID = "another-agent" },
		"recreated Agent":  func(i *CompileInput) { i.Resolution.Binding.AgentUID = "new-agent-uid" },
		"wrong namespace":  func(i *CompileInput) { i.Platform.Identities[0].Namespace = "another-namespace" },
		"default account":  func(i *CompileInput) { i.Platform.Identities[0].ServiceAccountName = "default" },
		"missing account":  func(i *CompileInput) { i.Platform.Identities[0].ServiceAccountName = "" },
		"invalid account":  func(i *CompileInput) { i.Platform.Identities[0].ServiceAccountName = "../example" },
		"missing revision": func(i *CompileInput) { i.Platform.Identities[0].Revision = "" },
		"blank revision":   func(i *CompileInput) { i.Platform.Identities[0].Revision = " " },
		"control revision": func(i *CompileInput) { i.Platform.Identities[0].Revision = "approval\nv2" },
		"missing Entra":    func(i *CompileInput) { i.Platform.Identities[0].Entra = nil },
		"missing client":   func(i *CompileInput) { i.Platform.Identities[0].Entra.ClientID = "" },
		"missing tenant":   func(i *CompileInput) { i.Platform.Identities[0].Entra.TenantID = "" },
		"blank tenant":     func(i *CompileInput) { i.Platform.Identities[0].Entra.TenantID = " " },
		"control client":   func(i *CompileInput) { i.Platform.Identities[0].Entra.ClientID = "client\x00other" },
		"duplicate Agent":  func(i *CompileInput) { i.Platform.Identities = append(i.Platform.Identities, i.Platform.Identities[0]) },
		"borrowed ServiceAccount": func(i *CompileInput) {
			other := i.Platform.Identities[0]
			other.AgentUID = "another-agent"
			i.Platform.Identities = append(i.Platform.Identities, other)
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := entraCompilerFixture()
			mutate(&input)
			if plan, err := Compile(input); plan != nil || !errors.Is(err, ErrUnapprovedGrant) {
				t.Fatalf("unapproved identity produced a plan: %v", err)
			}
		})
	}
}

func TestEntraCompilerApprovalRevisionAndPrivilegeSeparation(t *testing.T) {
	input := entraCompilerFixture()
	original := mustCompile(t, input)
	if len(original.Identity.RoleBindings)+len(original.Identity.ClusterRoleBindings)+len(original.Identity.Bindings) != 0 ||
		len(original.Identity.Volumes) != 1 || original.Identity.Volumes[0].Name != "sandbox-entra-exchange" ||
		original.Identity.ServiceAccount.Name != input.Platform.Identities[0].ServiceAccountName {
		t.Fatal("Entra-only compilation did not preserve its explicit identity without Kubernetes RBAC")
	}
	for name, mutate := range map[string]func(*CompileInput){
		"approval revision": func(i *CompileInput) { i.Platform.Identities[0].Revision = "synthetic-approval-v2" },
		"client":            func(i *CompileInput) { i.Platform.Identities[0].Entra.ClientID = "replacement-client" },
		"tenant":            func(i *CompileInput) { i.Platform.Identities[0].Entra.TenantID = "replacement-tenant" },
		"ServiceAccount":    func(i *CompileInput) { i.Platform.Identities[0].ServiceAccountName = "replacement-account" },
		"token lifetime":    func(i *CompileInput) { i.Platform.TokenExpirationSeconds = 3600 },
		"disable":           func(i *CompileInput) { i.Resolution.AgentPolicy.EntraID.Enabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			current := entraCompilerFixture()
			mutate(&current)
			if mustCompile(t, current).Revision == original.Revision {
				t.Fatal("changed identity intent or approval reused the old control revision")
			}
		})
	}
	input.Platform.Identities[0].Kubernetes = true
	input.Platform.Grants = adminFixture().Platform.Grants
	if got := mustCompile(t, input); got.Revision != original.Revision || !reflect.DeepEqual(got.Identity, original.Identity) {
		t.Fatal("unrequested Kubernetes approval granted privileges or changed effective Entra controls")
	}
	input.Resolution.AgentPolicy.EntraID.Enabled = false
	disabled := mustCompile(t, input)
	if disabled.Identity.ServiceAccountName != "default" || disabled.Identity.ServiceAccount != nil ||
		len(disabled.Identity.Volumes)+len(disabled.Identity.VolumeMounts)+len(disabled.Identity.Env) != 0 {
		t.Fatal("disabled Entra retained its identity projection")
	}
	kubernetes := adminFixture()
	before := mustCompile(t, kubernetes)
	kubernetes.Platform.Identities[0].Entra = &EntraIdentity{ClientID: "unrequested-client", TenantID: "unrequested-tenant"}
	if got := mustCompile(t, kubernetes); got.Revision != before.Revision || !reflect.DeepEqual(got.Identity, before.Identity) {
		t.Fatal("Kubernetes-only identity inherited Azure privileges")
	}
}

func TestEntraProjectionRejectsAlteredCompilerContract(t *testing.T) {
	for name, mutate := range map[string]func(*IdentityPlan){
		"automount":            func(p *IdentityPlan) { p.AutomountServiceAccountToken = true },
		"account automount":    func(p *IdentityPlan) { p.ServiceAccount.AutomountServiceAccountToken = ptr.To(true) },
		"missing account":      func(p *IdentityPlan) { p.ServiceAccount = nil },
		"account substitution": func(p *IdentityPlan) { p.ServiceAccount.Name = "other-account" },
		"empty account name":   func(p *IdentityPlan) { p.ServiceAccount.Name, p.ServiceAccountName = "", "" },
		"invalid account name": func(p *IdentityPlan) {
			p.ServiceAccount.Name, p.ServiceAccountName = "Invalid_Account", "Invalid_Account"
		},
		"missing namespace":   func(p *IdentityPlan) { p.ServiceAccount.Namespace = "" },
		"missing client":      func(p *IdentityPlan) { delete(p.ServiceAccount.Annotations, entraClientAnnotation) },
		"client substitution": func(p *IdentityPlan) { p.Env[0].Value = "another-client" },
		"tenant substitution": func(p *IdentityPlan) { p.Env[1].Value = "another-tenant" },
		"token file":          func(p *IdentityPlan) { p.Env[2].Value = "/home/vibe/borrowed-token" },
		"missing environment": func(p *IdentityPlan) { p.Env = p.Env[:2] },
		"extra environment": func(p *IdentityPlan) {
			p.Env = append(p.Env, corev1.EnvVar{Name: "AZURE_CLIENT_SECRET", Value: "synthetic"})
		},
		"environment source": func(p *IdentityPlan) { p.Env[0].ValueFrom = &corev1.EnvVarSource{} },
		"missing volume":     func(p *IdentityPlan) { p.Volumes = nil },
		"extra volume":       func(p *IdentityPlan) { p.Volumes = append(p.Volumes, p.Volumes[0]) },
		"wrong audience": func(p *IdentityPlan) {
			p.Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience = "https://kubernetes.default.svc"
		},
		"token path": func(p *IdentityPlan) { p.Volumes[0].Projected.Sources[0].ServiceAccountToken.Path = "other-token" },
		"unbounded token": func(p *IdentityPlan) {
			p.Volumes[0].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = ptr.To[int64](3601)
		},
		"implicit lifetime":    func(p *IdentityPlan) { p.Volumes[0].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = nil },
		"world-readable token": func(p *IdentityPlan) { p.Volumes[0].Projected.DefaultMode = ptr.To[int32](0444) },
		"extra source": func(p *IdentityPlan) {
			p.Volumes[0].Projected.Sources = append(p.Volumes[0].Projected.Sources, corev1.VolumeProjection{
				Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "unapproved"}},
			})
		},
		"writable mount": func(p *IdentityPlan) { p.VolumeMounts[0].ReadOnly = false },
		"mount path":     func(p *IdentityPlan) { p.VolumeMounts[0].MountPath = "/home/vibe" },
		"subpath":        func(p *IdentityPlan) { p.VolumeMounts[0].SubPath = "azure-identity-token" },
		"missing mount":  func(p *IdentityPlan) { p.VolumeMounts = nil },
		"Kubernetes binding": func(p *IdentityPlan) {
			p.RoleBindings = []rbacv1.RoleBinding{{}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := entraCompilerFixture()
			plan := mustCompile(t, input).Identity
			if err := ValidateIdentityProjection(input.Resolution.AgentPolicy, plan); err != nil {
				t.Fatal("unmodified compiler projection was rejected:", err)
			}
			mutate(&plan)
			if err := ValidateIdentityProjection(input.Resolution.AgentPolicy, plan); !errors.Is(err, ErrUnapprovedGrant) {
				t.Fatal("altered Entra projection accepted:", err)
			}
		})
	}
}

func TestEntraProjectionModesUseCompilerShape(t *testing.T) {
	for _, mode := range []string{"disabled", "entra", "kubernetes", "combined"} {
		for _, lifetime := range []int64{0, 600, 3600} {
			t.Run(fmt.Sprintf("%s/%d", mode, lifetime), func(t *testing.T) {
				input := compilerFixture()
				if mode == "kubernetes" || mode == "combined" {
					input = adminFixture()
				}
				if mode == "entra" {
					input = entraCompilerFixture()
				} else if mode == "combined" {
					input.Resolution.AgentPolicy.EntraID.Enabled = true
					input.Platform.Identities[0].Entra = &EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"}
				}
				input.Platform.TokenExpirationSeconds = lifetime
				plan := mustCompile(t, input)
				if err := ValidateIdentityProjection(input.Resolution.AgentPolicy, plan.Identity); err != nil {
					t.Fatal("compiler output rejected:", err)
				}
				if mode == "kubernetes" {
					plan.Identity.Env = []corev1.EnvVar{{Name: "AZURE_CLIENT_ID", Value: "unrequested-client"}}
					if err := ValidateIdentityProjection(input.Resolution.AgentPolicy, plan.Identity); err == nil {
						t.Fatal("Kubernetes-only workload acquired Entra environment")
					}
					plan.Identity.Env = nil
					plan.Identity.ServiceAccount.Annotations[entraTenantAnnotation] = "unrequested-tenant"
					if err := ValidateIdentityProjection(input.Resolution.AgentPolicy, plan.Identity); err == nil {
						t.Fatal("Kubernetes-only workload acquired Entra annotation")
					}
				}
				if mode == "combined" {
					plan.Identity.Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience = EntraAudience
					if err := ValidateIdentityProjection(input.Resolution.AgentPolicy, plan.Identity); err == nil {
						t.Fatal("Kubernetes projection accepted the Entra exchange audience")
					}
				}
			})
		}
	}
}

func TestNativeEntraRequiresDeclaredCapabilityAndBootstrapPin(t *testing.T) {
	for _, declare := range []bool{false, true} {
		input := nativeCompilerFixture()
		input.Resolution.Capabilities.EntraID = declare
		input.Resolution.AgentPolicy.EntraID.Enabled = true
		input.Platform.Identities = entraCompilerFixture().Platform.Identities
		input.Platform.KubernetesAudience = "https://kubernetes.default.svc"
		want := ErrUnsupportedControl
		if declare {
			want = ErrUnapprovedGrant
		}
		if plan, err := Compile(input); plan != nil || !errors.Is(err, want) {
			t.Fatal("native identity did not require both support and a recorded pin:", err)
		}
		if declare {
			input.IdentityPin = &IdentityPin{Name: "example-entra", UID: "account-uid", BootstrapUID: "bootstrap-uid"}
			plan := mustCompile(t, input)
			if !plan.Identity.BootstrapServiceAccount || plan.Identity.ServiceAccount.UID != input.IdentityPin.UID {
				t.Fatal("compiler lost the bootstrap identity pin")
			}
			for _, object := range plan.Resources {
				if object.GetKind() == "ServiceAccount" {
					t.Fatal("bootstrap ServiceAccount was handed to Helm")
				}
			}
		}
	}
}
