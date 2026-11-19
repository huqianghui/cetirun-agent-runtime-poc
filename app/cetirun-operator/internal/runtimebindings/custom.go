// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package runtimebindings defines receipts from platform capability owners.
// These are internal inputs, not fields accepted from an Agent or Helm values.
package runtimebindings

import (
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"k8s.io/apimachinery/pkg/util/validation"
)

type ObjectReference struct {
	Name     string `json:"name"`
	UID      string `json:"uid"`
	Revision string `json:"revision"`
}

type ModelAccess struct {
	ObjectReference
	BaseURL string `json:"baseURL"`
	Key     string `json:"key"`
}

// CopilotProxy contains platform-owned references only. Its token and proxy key
// are never mounted into the Agent or included in the Agent model binding.
type CopilotProxy struct {
	Image            string   `json:"image"`
	KeyID            string   `json:"keyID"`
	SignInGeneration string   `json:"signInGeneration,omitempty"`
	ResponsesModels  []string `json:"responsesModels,omitempty"`
}

type Home struct {
	Name    string     `json:"name"`
	FSGroup int64      `json:"fsGroup"`
	Claim   *HomeClaim `json:"claim"`
}

// Context is an immutable ConfigMap snapshot. Files maps keys to paths under
// HOME/.agents. Its owner supplies approved content, not executable init hooks.
type Context struct {
	ObjectReference
	Files map[string]string `json:"files"`
}

type Peer struct {
	Namespace string            `json:"namespace"`
	PodLabels map[string]string `json:"podLabels"`
}

type Network struct {
	Revision    string              `json:"revision"`
	Callers     []Peer              `json:"callers"`
	Gateway     Peer                `json:"gateway"`
	GatewayPort int32               `json:"gatewayPort"`
	DNS         Peer                `json:"dns"`
	APIServer   []APIServerEndpoint `json:"apiServer,omitempty"`
}

type APIServerEndpoint struct {
	IP   string `json:"ip"`
	Port int32  `json:"port"`
}

// Custom is returned only after policy and capability owners approve these
// exact objects for the requested Agent UID, generation, type and image.
type Custom struct {
	AgentName        string          `json:"agentName"`
	AgentUID         string          `json:"agentUID"`
	Generation       int64           `json:"generation"`
	RestartRequestID string          `json:"restartRequestID,omitempty"`
	Namespace        ObjectReference `json:"namespace"`
	ApprovedType     string          `json:"approvedType"`
	ApprovedImage    string          `json:"approvedImage"`
	PolicyRevision   string          `json:"policyRevision"`
	Model            ModelAccess     `json:"model"`
	Copilot          *CopilotProxy   `json:"copilot,omitempty"`
	Home             Home            `json:"home"`
	Context          Context         `json:"context"`
	Network          Network         `json:"network"`
	// Only the Registry synchronizer supplies this stable, namespace-local name.
	PullSecretName string         `json:"pullSecretName,omitempty"`
	Memory         *Memory        `json:"memory,omitempty"`
	AgentIdentity  *AgentIdentity `json:"agentIdentity,omitempty"`
}

func (b Custom) HarnessBindings() agents.Bindings {
	bindings := agents.Bindings{ContextRevision: b.Context.Revision,
		WebBasePath: "/ui/" + b.AgentName + "/", WebInstance: b.AgentUID}
	bindings.Model = agents.ModelBinding{BaseURL: b.Model.BaseURL, CredentialSlot: "model", Revision: b.Model.Revision}
	return bindings
}

func validReference(ref ObjectReference) bool {
	return len(validation.IsDNS1123Subdomain(ref.Name)) == 0 && ref.UID != "" && ref.Revision != ""
}

func validPeer(peer Peer) bool {
	if len(validation.IsDNS1123Label(peer.Namespace)) != 0 || len(peer.PodLabels) == 0 {
		return false
	}
	for key, value := range peer.PodLabels {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return false
		}
	}
	return true
}

func (b Custom) Validate(agent agents.ResolvedAgent) error {
	if b.RestartRequestID != "" && !api.ValidRestartRequestID(b.RestartRequestID) {
		return fmt.Errorf("invalid Agent restart request ID")
	}
	if b.AgentIdentity != nil {
		return fmt.Errorf("UnsupportedFeature: Custom does not support Agent Identity")
	}
	if err := b.Memory.Validate(agent.HomeDir); err != nil {
		return err
	}
	if b.PullSecretName != "" && (len(validation.IsDNS1123Label(b.PullSecretName)) != 0 ||
		b.PullSecretName != "registry-pull-"+strings.ReplaceAll(b.AgentUID, "-", "")) {
		return fmt.Errorf("RegistryBindingInvalid: pull Secret must belong to this Agent UID")
	}
	if len(validation.IsDNS1123Subdomain(b.AgentName)) != 0 || b.AgentUID == "" || b.Generation < 1 || !validReference(b.Namespace) ||
		b.ApprovedType != "custom" || b.ApprovedImage != agent.Image || b.PolicyRevision == "" {
		return fmt.Errorf("PolicyBindingPending: exact Agent and image approval is required")
	}
	if !validReference(b.Model.ObjectReference) || len(validation.IsConfigMapKey(b.Model.Key)) != 0 {
		return fmt.Errorf("ModelBindingPending: versioned instance Secret reference is required")
	}
	if proxy := b.Copilot; proxy != nil {
		if proxy.Image == "" || strings.ContainsAny(proxy.Image, " \t\r\n\"'") ||
			!regexp.MustCompile(`^[a-f0-9]{12}$`).MatchString(proxy.KeyID) ||
			(proxy.SignInGeneration != "" && !regexp.MustCompile(`^[a-f0-9]{1,64}$`).MatchString(proxy.SignInGeneration)) || len(proxy.ResponsesModels) > 128 {
			return fmt.Errorf("CopilotBindingInvalid: approved image and credential identities are required")
		}
		for _, model := range proxy.ResponsesModels {
			if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(model) {
				return fmt.Errorf("CopilotBindingInvalid: invalid model identifier")
			}
		}
	}
	if len(validation.IsDNS1123Subdomain(b.Home.Name)) != 0 || b.Home.FSGroup < 1 || b.Home.FSGroup > 2147483647 {
		return fmt.Errorf("HomeBindingPending: approved HOME name and volume permissions are required")
	}
	if b.Home.Claim == nil {
		return fmt.Errorf("HomeBindingPending: approved HOME claim template is required")
	}
	if err := b.Home.Claim.Validate(); err != nil {
		return err
	}
	if !validReference(b.Context.ObjectReference) {
		return fmt.Errorf("ContextBindingPending: approved immutable context is required")
	}
	if err := ValidateContextFiles(b.Context.Files); err != nil {
		return err
	}
	if b.Network.Revision == "" || !validPeer(b.Network.DNS) ||
		!validPeer(b.Network.Gateway) || b.Network.GatewayPort < 1 || b.Network.GatewayPort > 65535 {
		return fmt.Errorf("NetworkBindingPending: approved Gateway and DNS peers are required")
	}
	for _, peer := range b.Network.Callers {
		if !validPeer(peer) {
			return fmt.Errorf("NetworkBindingInvalid: callers require a namespace and pod selector")
		}
	}
	if len(b.Network.APIServer) > 64 {
		return fmt.Errorf("NetworkBindingInvalid: too many API server endpoints")
	}
	for _, endpoint := range b.Network.APIServer {
		address, err := netip.ParseAddr(endpoint.IP)
		if err != nil || address.IsUnspecified() || address.IsMulticast() ||
			endpoint.Port < 1 || endpoint.Port > 65535 {
			return fmt.Errorf("NetworkBindingInvalid: exact API server addresses and ports are required")
		}
	}
	return agents.ValidatePortableBindings(b.HarnessBindings())
}

func ValidateContextFiles(files map[string]string) error {
	if len(files) > 256 {
		return fmt.Errorf("ContextBindingInvalid: too many context files")
	}
	seen := map[string]bool{}
	for key, file := range files {
		if len(validation.IsConfigMapKey(key)) != 0 || file == "" || len(file) > 512 ||
			path.Clean(file) != file || strings.ContainsAny(file, "\\\x00\r\n") ||
			(file != "AGENTS.md" && !strings.HasPrefix(file, "skills/")) ||
			seen[file] || strings.HasSuffix(file, "/") {
			return fmt.Errorf("ContextBindingInvalid: invalid or duplicate context path")
		}
		seen[file] = true
	}
	for file := range seen {
		for parent := path.Dir(file); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return fmt.Errorf("ContextBindingInvalid: file shadows a context directory")
			}
		}
	}
	if !seen["AGENTS.md"] || !seen["skills/cetirun-memory/SKILL.md"] {
		return fmt.Errorf("ContextBindingPending: canonical AGENTS.md and cetirun-memory are required")
	}
	return nil
}

// nativePlatformKeys are operator-wide settings that only native harnesses
// consume. Custom HOME comes from its approved binding, so these are dropped
// from the shared values instead of rejected; Agents still cannot set them.
var nativePlatformKeys = map[string]bool{"persistence": true}

// SharedValues returns the operator-wide Helm values that apply to Custom.
func SharedValues(values map[string]any) map[string]any {
	shared := make(map[string]any, len(values))
	for key, value := range values {
		if !nativePlatformKeys[key] {
			shared[key] = value
		}
	}
	return shared
}

// ValidateOverrides deliberately keeps the Custom override surface small.
// Bindings, identity, routing, hooks and volumes never come from Helm overrides.
func ValidateOverrides(values map[string]any) error {
	for key := range values {
		if _, supported := customPlatformKeys[key]; !supported {
			return fmt.Errorf("UnsupportedFeature: helm.values.%s is not supported for Custom", key)
		}
		switch key {
		case "resources":
			if values[key] == nil {
				continue
			}
			resources, ok := values[key].(map[string]any)
			if !ok {
				return fmt.Errorf("invalid Custom resources")
			}
			for field := range resources {
				if field != "requests" && field != "limits" {
					return fmt.Errorf("UnsupportedFeature: Custom resources accepts only requests and limits")
				}
			}
		}
	}
	if err := ValidateCustomSkills(values["skills"]); err != nil {
		return err
	}
	return ValidateScheduling(values)
}

// The value states whether a supported setting changes the Pod's inputs.
var customPlatformKeys = map[string]bool{
	"resources": true, "runtimeClassName": true, "nodeSelector": true, "affinity": true,
	"tolerations": true, "topologySpreadConstraints": true, "desktop": true, "podAnnotations": true, "global": true,
	"networkPolicy": false, "sidecarScope": false, "execRestriction": false,
	"podDisruptionBudget": false, "autoscaling": false, "ttyd": false,
	"skills": true,
}

// PlatformValues selects supported shared settings for the Custom plan hash.
// Native configuration and credentials remain outside the Custom runtime.
func PlatformValues(values map[string]any) map[string]any {
	selected := map[string]any{}
	for key := range customPlatformKeys {
		if key == "ttyd" && !terminalEnabled(values) {
			continue
		}
		if value, ok := values[key]; ok {
			selected[key] = value
		}
	}
	return selected
}

func WorkloadValues(values map[string]any) map[string]any {
	selected := map[string]any{}
	for key, workload := range customPlatformKeys {
		if workload {
			if value, ok := values[key]; ok {
				selected[key] = value
			}
		}
	}
	desktop, _ := values["desktop"].(map[string]any)
	if desktop["enabled"] != true {
		delete(selected, "desktop")
		delete(selected, "global")
	}
	skills, _ := values["skills"].(map[string]any)
	if skills["shared"] != true {
		delete(selected, "skills")
	} else {
		mounted := map[string]any{}
		for _, key := range []string{"shared", "storageClassName", "protocol", "secretName", "mountOptions",
			"resourceGroup", "storageAccount", "containerName", "storageSize"} {
			mounted[key] = skills[key]
		}
		selected["skills"] = mounted
	}
	return selected
}

func terminalEnabled(values map[string]any) bool {
	terminal, _ := values["ttyd"].(map[string]any)
	return terminal["enabled"] == true
}
