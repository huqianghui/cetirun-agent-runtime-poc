// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

func nativeKubernetesFixture() CompileInput {
	input := adminFixture()
	input.NativeExecution = true
	input.Resolution.Capabilities.Profile = sandbox.ProfileNativeSingleStart
	input.Resolution.Capabilities.EnforcementObservation = false
	input.Resolution.Capabilities.RestrictiveFailure = false
	input.Resolution.Capabilities.EntraID = false
	input.Platform.Adapter.Name, input.Platform.Adapter.Qualified = Cilium, false
	return input
}

func TestNativeKubernetesAccessRequiresExactApprovals(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		t.Run(string(kind), func(t *testing.T) {
			input := nativeKubernetesFixture()
			input.Resolution.AgentPolicy.Harnesses.Allowed = []api.AgentType{kind}
			plan := mustCompile(t, input)
			if plan.Identity.ServiceAccount == nil || plan.Identity.ServiceAccount.Name != "example-dedicated" ||
				ptr.Deref(plan.Identity.ServiceAccount.AutomountServiceAccountToken, true) ||
				len(plan.Identity.RoleBindings) != 1 || len(plan.Identity.ClusterRoleBindings) != 1 ||
				len(plan.Identity.Bindings) != 2 || len(plan.Identity.Volumes) != 1 || len(plan.Identity.VolumeMounts) != 1 {
				t.Fatal("native Kubernetes access lost its dedicated identity, exact grants or projection")
			}
			mount := plan.Identity.VolumeMounts[0]
			projection := plan.Identity.Volumes[0].Projected
			if !mount.ReadOnly || mount.MountPath != "/var/run/secrets/kubernetes.io/serviceaccount" ||
				projection == nil || len(projection.Sources) != 3 ||
				projection.Sources[0].ServiceAccountToken.Audience != input.Platform.KubernetesAudience ||
				ptr.Deref(projection.Sources[0].ServiceAccountToken.ExpirationSeconds, 0) != 600 {
				t.Fatal("Kubernetes projection is not bounded and audience-specific")
			}
			if !syntheticEgressAllowed(plan.Network.Policy.Spec, "203.0.113.10", nil, 6443, corev1.ProtocolTCP) ||
				syntheticEgressAllowed(plan.Network.Policy.Spec, "203.0.113.10", nil, 443, corev1.ProtocolTCP) {
				t.Fatal("Kubernetes network grant is not limited to the registered API port")
			}
		})
	}
	for name, mutate := range map[string]func(*CompileInput){
		"undeclared capability": func(i *CompileInput) { i.Resolution.Capabilities.KubernetesAccess = false },
		"missing grants":        func(i *CompileInput) { i.Platform.Grants = nil },
		"foreign policy":        func(i *CompileInput) { i.Platform.Grants[0].PolicyUID = "other-policy" },
		"wrong scope":           func(i *CompileInput) { i.Platform.Grants[0].Namespace = "other-services" },
		"missing role pin":      func(i *CompileInput) { i.Platform.Grants[0].RoleUID = "" },
		"missing role version":  func(i *CompileInput) { i.Platform.Grants[0].RoleResourceVersion = "" },
		"missing rules":         func(i *CompileInput) { i.Platform.Grants[0].Rules = nil },
		"shared role alias":     func(i *CompileInput) { i.Platform.Grants[0].RoleRef.Kind = "Role" },
		"duplicate approval":    func(i *CompileInput) { i.Platform.Grants = append(i.Platform.Grants, i.Platform.Grants[0]) },
		"missing identity":      func(i *CompileInput) { i.Platform.Identities = nil },
		"foreign Agent":         func(i *CompileInput) { i.Platform.Identities[0].AgentUID = "another-agent" },
		"foreign namespace":     func(i *CompileInput) { i.Platform.Identities[0].Namespace = "another-namespace" },
		"unapproved identity":   func(i *CompileInput) { i.Platform.Identities[0].Kubernetes = false },
		"default account":       func(i *CompileInput) { i.Platform.Identities[0].ServiceAccountName = "default" },
		"wrong account":         func(i *CompileInput) { i.Platform.Identities[0].ServiceAccountName = "other-account" },
		"unbounded token":       func(i *CompileInput) { i.Platform.TokenExpirationSeconds = 3601 },
		"missing audience":      func(i *CompileInput) { i.Platform.KubernetesAudience = "" },
		"missing API":           func(i *CompileInput) { i.Platform.Topology.KubernetesAPI = nil },
		"blocked API": func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedCIDRs = []string{"203.0.113.10/32"}
		},
		"blocked API port": func(i *CompileInput) {
			i.Resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 6443}}
		},
		"Entra policy": func(i *CompileInput) { i.Resolution.AgentPolicy.EntraID.Enabled = true },
		"Entra registration": func(i *CompileInput) {
			i.Platform.Identities[0].Entra = &EntraIdentity{ClientID: "synthetic-client", TenantID: "synthetic-tenant"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := nativeKubernetesFixture()
			mutate(&input)
			if _, err := Compile(input); err == nil {
				t.Fatal("native execution accepted missing, foreign or unsupported authorization")
			}
		})
	}
}

func TestNativeKubernetesNilAccessRemainsIsolated(t *testing.T) {
	input := nativeKubernetesFixture()
	input.Resolution.AgentPolicy.KubernetesAccess = nil
	plan := mustCompile(t, input)
	if plan.Identity.ServiceAccountName != "default" || plan.Identity.ServiceAccount != nil ||
		len(plan.Identity.Bindings)+len(plan.Identity.Volumes)+len(plan.Identity.VolumeMounts)+len(plan.Identity.Env) != 0 {
		t.Fatal("an unrequested registered identity granted access")
	}
	if syntheticEgressAllowed(plan.Network.Policy.Spec, "203.0.113.10", nil, 6443, corev1.ProtocolTCP) ||
		slices.ContainsFunc(plan.Resources, func(object unstructured.Unstructured) bool {
			return object.GetKind() == "RoleBinding" || object.GetKind() == "ClusterRoleBinding"
		}) {
		t.Fatal("nil Kubernetes access acquired API network access or RBAC")
	}
}

func TestNativeKubernetesAndEntraPrivilegesRemainIndependent(t *testing.T) {
	for _, mode := range []string{"neither", "kubernetes", "entra", "combined"} {
		t.Run(mode, func(t *testing.T) {
			input := nativeKubernetesFixture()
			input.Resolution.Capabilities.EntraID = true
			input.Platform.Identities[0].Entra = &EntraIdentity{ClientID: "example-client", TenantID: "example-tenant"}
			input.IdentityPin = &IdentityPin{
				Name: input.Platform.Identities[0].ServiceAccountName, UID: "example-account-uid", BootstrapUID: "example-bootstrap-uid",
			}
			kubernetes, entra := mode == "kubernetes" || mode == "combined", mode == "entra" || mode == "combined"
			if !kubernetes {
				input.Resolution.AgentPolicy.KubernetesAccess = nil
			}
			input.Resolution.AgentPolicy.EntraID.Enabled = entra
			plan := mustCompile(t, input)
			if err := ValidateIdentityProjection(input.Resolution.AgentPolicy, plan.Identity); err != nil {
				t.Fatal(err)
			}
			bindings, volumes, environment := 0, 0, 0
			if kubernetes {
				bindings, volumes = 2, 1
			}
			if entra {
				volumes++
				environment = 3
			}
			if len(plan.Identity.Bindings) != bindings || len(plan.Identity.Volumes) != volumes ||
				len(plan.Identity.Env) != environment {
				t.Fatal("an unrequested registered identity projected privileges")
			}
			if kubernetes || entra {
				if !plan.Identity.BootstrapServiceAccount || plan.Identity.ServiceAccount.UID != input.IdentityPin.UID {
					t.Fatal("combined registration lost its retained ServiceAccount pin")
				}
			}
			if syntheticEgressAllowed(plan.Network.Policy.Spec, "203.0.113.10", nil, 6443, corev1.ProtocolTCP) != kubernetes {
				t.Fatal("Entra approval changed the Kubernetes API network grant")
			}
			for _, capability := range []string{"kubernetes", "entra"} {
				t.Run("missing-"+capability+"-capability", func(t *testing.T) {
					current := input
					if capability == "kubernetes" {
						current.Resolution.Capabilities.KubernetesAccess = false
					} else {
						current.Resolution.Capabilities.EntraID = false
					}
					if _, err := Compile(current); err == nil {
						t.Fatal("identity inventory was accepted without its corresponding capability")
					}
				})
			}
		})
	}
}
