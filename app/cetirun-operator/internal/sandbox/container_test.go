// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func containerCapabilities() Capabilities {
	capabilities := nativeCapabilities()
	capabilities.RuntimeClassName = "example-container"
	capabilities.PerContainerResourceLimits = true
	capabilities.RequirePerContainerResourceLimits = true
	return capabilities
}

func containerResolveInput() ResolveInput {
	input := resolverFixture()
	input.NativeExecution = true
	input.Resources.Sandboxes["standard"].Spec.Type = api.SandboxContainer
	input.Registrations = map[api.SandboxType]Registration{
		api.SandboxContainer: {Capabilities: containerCapabilities(), Available: true},
	}
	return input
}

func TestContainerRuntimeResolution(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes, api.AgentTypeCustom} {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
			t.Run(string(kind)+"/"+string(mode), func(t *testing.T) {
				input := containerResolveInput()
				input.Agent.Spec.Agent.Type = kind
				input.Resources.Sandboxes["standard"].Spec.Storage.Home.AccessMode = mode
				registration := input.Registrations[api.SandboxContainer]
				registration.Capabilities.HomeAccessModes = []corev1.PersistentVolumeAccessMode{mode}
				input.Registrations[api.SandboxContainer] = registration
				resolved := mustResolve(t, input)
				if resolved.Sandbox.Type != api.SandboxContainer || resolved.Binding != input.AuthorizedBinding ||
					resolved.Capabilities.RuntimeClassName != "example-container" ||
					!resolved.Capabilities.PerContainerResourceLimits || resolved.Capabilities.EnforcementObservation ||
					resolved.Capabilities.RestrictiveFailure || resolved.Sandbox.Storage.Home.AccessMode != mode {
					t.Fatal("container resolution changed the binding, runtime or guarantees")
				}
				kata := resolverFixture()
				if mustResolve(t, kata).Sandbox.Type != api.SandboxKata {
					t.Fatal("container support changed the omitted Kata default")
				}
				if resolved.Revision == mustResolve(t, kata).Revision {
					t.Fatal("different backends reused an execution revision")
				}
			})
		}
	}
}

func TestContainerRuntimeFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*ResolveInput){
		"no native executor":   func(i *ResolveInput) { i.NativeExecution = false },
		"missing registration": func(i *ResolveInput) { delete(i.Registrations, api.SandboxContainer) },
		"no AgentPolicy":       func(i *ResolveInput) { delete(i.Resources.AgentPolicies, "normal") },
		"unauthorized binding": func(i *ResolveInput) { i.AuthorizedBinding.AgentPolicyUID = "another-policy" },
		"unknown harness":      func(i *ResolveInput) { i.Agent.Spec.Agent.Type = api.AgentType("unknown") },
		"unregistered HOME mode": func(i *ResolveInput) {
			i.Resources.Sandboxes["standard"].Spec.Storage.Home.AccessMode = corev1.ReadWriteOnce
		},
		"FQDN": func(i *ResolveInput) {
			i.Resources.AgentPolicies["normal"].Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
		},
		"mTLS without declared support": func(i *ResolveInput) { i.Resources.AgentPolicies["normal"].Spec.Network.Ingress.RequireMTLS = true },
		"Entra":                         func(i *ResolveInput) { i.Resources.AgentPolicies["normal"].Spec.EntraID.Enabled = true },
		"Kubernetes": func(i *ResolveInput) {
			i.Resources.AgentPolicies["normal"].Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "operations",
				RoleBindings:       []api.AgentPolicyRoleBinding{{Namespace: "services", ClusterRoleName: "reader"}},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := containerResolveInput()
			mutate(&input)
			if _, err := Resolve(input); err == nil {
				t.Fatal("container execution accepted an unavailable or unsupported dependency")
			}
		})
	}
	input := containerResolveInput()
	registration := input.Registrations[api.SandboxContainer]
	registration.Available = false
	input.Registrations[api.SandboxContainer] = registration
	if _, err := Resolve(input); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatal("unavailable ordinary runtime did not fail closed:", err)
	}
}

func TestContainerRuntimeMTLSRequiresDeclaredCapability(t *testing.T) {
	input := containerResolveInput()
	input.Resources.AgentPolicies["normal"].Spec.Network.Ingress.RequireMTLS = true
	if _, err := Resolve(input); err == nil {
		t.Fatal("mTLS intent was accepted without declared runtime support")
	}
	registration := input.Registrations[api.SandboxContainer]
	registration.Capabilities.MutualTLS = true
	input.Registrations[api.SandboxContainer] = registration
	resolved := mustResolve(t, input)
	if resolved.Sandbox.Type != api.SandboxContainer || !resolved.Capabilities.MutualTLS ||
		!resolved.Capabilities.PerContainerResourceLimits || !resolved.AgentPolicy.Network.Ingress.RequireMTLS {
		t.Fatal("container resolution lost its resource semantics or declared mTLS intent")
	}
}

func TestContainerRuntimeEntraRequiresDeclaredCapability(t *testing.T) {
	input := containerResolveInput()
	input.Resources.AgentPolicies["normal"].Spec.EntraID.Enabled = true
	if _, err := Resolve(input); err == nil {
		t.Fatal("Entra intent was accepted without declared runtime support")
	}
	registration := input.Registrations[api.SandboxContainer]
	registration.Capabilities.EntraID = true
	input.Registrations[api.SandboxContainer] = registration
	resolved := mustResolve(t, input)
	if resolved.Sandbox.Type != api.SandboxContainer || !resolved.Capabilities.EntraID ||
		!resolved.Capabilities.PerContainerResourceLimits || !resolved.AgentPolicy.EntraID.Enabled {
		t.Fatal("container resolution lost its resource semantics or declared Entra intent")
	}
}

func TestContainerRuntimeCapabilities(t *testing.T) {
	for name, mutate := range map[string]func(*Capabilities){
		"missing RuntimeClass":     func(c *Capabilities) { c.RuntimeClassName = "" },
		"missing revision":         func(c *Capabilities) { c.Revision = "" },
		"missing host limits":      func(c *Capabilities) { c.HostResourceLimits = false },
		"missing container limits": func(c *Capabilities) { c.PerContainerResourceLimits = false },
		"qualified profile":        func(c *Capabilities) { c.Profile = "" },
		"external observer claim":  func(c *Capabilities) { c.EnforcementObservation = true },
		"physical fencing claim":   func(c *Capabilities) { c.RestrictiveFailure = true },
	} {
		t.Run(name, func(t *testing.T) {
			input := containerResolveInput()
			capabilities := containerCapabilities()
			mutate(&capabilities)
			if errs := CheckCompatibility(input.Resources.Sandboxes["standard"].Spec, input.Resources.AgentPolicies["normal"].Spec, capabilities); len(errs) == 0 {
				t.Fatal("container registration accepted missing or overstated support")
			}
		})
	}
	for _, kind := range []api.SandboxType{api.SandboxKata, api.SandboxGVisor} {
		if len(CheckRuntimeCapabilities(kind, containerCapabilities())) == 0 {
			t.Fatalf("ordinary container quotas were attributed to %s", kind)
		}
	}
}

func TestNativeObservedSNICapabilityRequiresExplicitRegistration(t *testing.T) {
	for _, kind := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		t.Run(string(kind), func(t *testing.T) {
			input := resolverFixture()
			box := input.Resources.Sandboxes["standard"].Spec
			box.Type = kind
			policy := input.Resources.AgentPolicies["normal"].Spec
			policy.Network.BlockedFQDNs = []string{"blocked.example.com"}
			capabilities := nativeCapabilities()
			if kind == api.SandboxContainer {
				capabilities = containerCapabilities()
			}
			for _, registered := range []bool{false, true} {
				capabilities.BlockedFQDNs = registered
				errs := CheckCompatibility(box, policy, capabilities)
				if (len(errs) == 0) != registered {
					t.Fatalf("unexpected observed-SNI compatibility with registered=%t: %v", registered, errs)
				}
			}
		})
	}
}
