// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package ingressmtls describes the opt-in managed TLS endpoint. It does not
// issue certificates or replace the guest-external network boundary.
package ingressmtls

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/Azure/CetiRun/app/internal/chataccess"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	ContainerName          = "ingress-mtls"
	ConfigName             = "cetirun-ingress-mtls"
	ConfigDigestAnnotation = "cetirun.microsoft.com/ingress-mtls-config"
	TLSPath                = "/var/run/cetirun/ingress-tls"
	PublicPath             = "/var/run/cetirun/ingress-public"
	ConfigPath             = "/etc/cetirun-ingress/envoy.json"
	AdminPort              = 15440
	APIPort                = chataccess.MTLSAPIPort
	WebappPort             = 15444
	TerminalPort           = 15445
	DesktopPort            = 15446
	MaxConnectionSeconds   = 300
)

// Reference approves an existing object, not a name to discover or adopt.
type Reference struct {
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
}

type Registration struct {
	// No default image or tag-only fallback is permitted.
	Image    string    `json:"image"`
	Bindings []Binding `json:"bindings"`
}

type Binding struct {
	AgentUID     types.UID    `json:"agentUID"`
	Namespace    string       `json:"namespace"`
	Harness      string       `json:"harness"`
	Port         int32        `json:"port"`
	Terminal     bool         `json:"terminal"`
	Desktop      bool         `json:"desktop,omitempty"`
	ServerSecret Reference    `json:"serverSecret"`
	ClientSecret Reference    `json:"clientSecret"`
	Gateway      Reference    `json:"gateway"`
	ClientURI    string       `json:"clientURI"`
	ChatClients  []ChatClient `json:"chatClients,omitempty"`
}

// ChatClient approves a Portal mesh workload and its own client credential.
// Gateway client keys are never shared with the Portal.
type ChatClient struct {
	Secret    Reference         `json:"secret"`
	URI       string            `json:"uri"`
	PodLabels map[string]string `json:"podLabels"`
}

type Plan struct {
	Binding
	Image       string `json:"image"`
	ServerName  string `json:"serverName"`
	ServiceHost string `json:"serviceHost"`
}

type Port struct {
	Name    string
	Backend int32
	TLS     int32
}

var imageDigest = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$`)
var ErrApprovalPending = errors.New("mTLS has no current per-Agent certificate approval")

func (r Reference) Validate() error {
	if len(validation.IsDNS1123Label(r.Namespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(r.Name)) != 0 || r.UID == "" ||
		len(validation.IsValidLabelValue(string(r.UID))) != 0 ||
		r.ResourceVersion == "" || strings.TrimSpace(r.ResourceVersion) != r.ResourceVersion {
		return fmt.Errorf("mTLS references require explicit namespace, name, UID and resourceVersion")
	}
	return nil
}

func (r Registration) Validate() error {
	if !imageDigest.MatchString(r.Image) || len(r.Bindings) > 128 {
		return fmt.Errorf("mTLS registration requires a digest-pinned image and bounded bindings")
	}
	agents, secrets := map[types.UID]bool{}, map[string]bool{}
	for _, binding := range r.Bindings {
		if err := binding.Validate(); err != nil {
			return err
		}
		if agents[binding.AgentUID] {
			return fmt.Errorf("mTLS Agent bindings must be unique")
		}
		agents[binding.AgentUID] = true
		for _, ref := range append([]Reference{binding.ServerSecret}, binding.ClientSecrets()...) {
			key := ref.Namespace + "/" + ref.Name
			if secrets[key] {
				return fmt.Errorf("mTLS Secret approvals must be distinct and per-Agent")
			}
			secrets[key] = true
		}
	}
	return nil
}

func (b Binding) Validate() error {
	if b.AgentUID == "" || len(validation.IsValidLabelValue(string(b.AgentUID))) != 0 ||
		len(validation.IsDNS1123Label(b.Namespace)) != 0 ||
		!slices.Contains([]string{"openclaw", "hermes", "custom"}, b.Harness) ||
		b.Port < 1024 || b.Port > 65535 ||
		slices.Contains([]int32{AdminPort, APIPort, WebappPort, TerminalPort, 7681, 9119}, b.Port) ||
		(b.Desktop && slices.Contains([]int32{DesktopPort, 5900, 6080, 18800}, b.Port)) {
		return fmt.Errorf("mTLS requires a per-Agent supported harness and nonconflicting primary port")
	}
	if b.Harness == "hermes" && b.Port == 9120 {
		return fmt.Errorf("Hermes API port conflicts with its Webapp port")
	}
	if b.Harness == "custom" && b.Desktop || b.Harness != "custom" && len(b.ChatClients) != 0 || len(b.ChatClients) > 8 {
		return fmt.Errorf("mTLS Custom supports declared HTTP and terminal endpoints, not desktop or unrelated Chat clients")
	}
	for _, ref := range []Reference{b.ServerSecret, b.ClientSecret, b.Gateway} {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	if b.ServerSecret.Namespace != b.Namespace || b.ClientSecret.Namespace != b.Gateway.Namespace ||
		b.Gateway.Namespace == b.Namespace {
		return fmt.Errorf("mTLS server credentials belong to the Agent; client credentials belong only to its external Gateway")
	}
	if err := validateClientURI(b.ClientURI, b.AgentUID); err != nil {
		return err
	}
	namespaces, uris := map[string]bool{}, map[string]bool{b.ClientURI: true}
	for _, client := range b.ChatClients {
		if err := client.Secret.Validate(); err != nil {
			return err
		}
		if err := validateClientURI(client.URI, b.AgentUID); err != nil {
			return err
		}
		if client.Secret.Namespace == b.Namespace ||
			namespaces[client.Secret.Namespace] || uris[client.URI] || len(client.PodLabels) == 0 || len(client.PodLabels) > 16 {
			return fmt.Errorf("mTLS Chat requires distinct per-namespace client identities and explicit Pod selectors")
		}
		for key, value := range client.PodLabels {
			if len(validation.IsQualifiedName(key)) != 0 || value == "" || len(validation.IsValidLabelValue(value)) != 0 {
				return fmt.Errorf("mTLS Chat client selector is invalid")
			}
		}
		namespaces[client.Secret.Namespace], uris[client.URI] = true, true
	}
	return nil
}

func validateClientURI(value string, agentUID types.UID) error {
	uri, err := url.Parse(value)
	if err != nil || uri.Scheme != "spiffe" || uri.Host == "" || uri.User != nil ||
		uri.RawQuery != "" || uri.Fragment != "" || uri.RawPath != "" || uri.Path == "" ||
		len(value) > 2048 || strings.ContainsAny(value, "*\r\n\t ") ||
		!strings.HasSuffix(uri.Path, "/"+string(agentUID)) {
		return fmt.Errorf("mTLS requires an exact client SPIFFE URI SAN ending in the Agent UID")
	}
	return nil
}

func (b Binding) ClientSecrets() []Reference {
	refs := []Reference{b.ClientSecret}
	for _, client := range b.ChatClients {
		refs = append(refs, client.Secret)
	}
	return refs
}

func (p Plan) TerminalEndpoint() Plan {
	p.Harness, p.Port, p.Terminal, p.Desktop, p.ChatClients = "custom-terminal", 7681, false, false, nil
	service, _, _ := strings.Cut(p.ServiceHost, ".")
	p.ServiceHost = terminalpolicy.WorkloadName(service) + "." + p.Namespace + ".svc.cluster.local"
	return p
}

func (p Plan) Ports() []Port {
	if p.Harness == "custom-terminal" {
		return []Port{{Name: "terminal", Backend: p.Port, TLS: TerminalPort}}
	}
	name := "gateway"
	if p.Harness == "hermes" {
		name = "api"
	}
	if p.Harness == "custom" {
		name = "http"
	}
	ports := []Port{{Name: name, Backend: p.Port, TLS: APIPort}}
	if p.Harness == "hermes" {
		ports = append(ports, Port{Name: "webapp", Backend: 9120, TLS: WebappPort})
	}
	if p.Terminal && p.Harness != "custom" {
		ports = append(ports, Port{Name: "ttyd", Backend: 7681, TLS: TerminalPort})
	}
	if p.Desktop {
		ports = append(ports, Port{Name: "novnc", Backend: 6080, TLS: DesktopPort})
	}
	return ports
}

func (r Registration) Resolve(agentUID types.UID, namespace, service string) (*Plan, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	for _, binding := range r.Bindings {
		if binding.AgentUID == agentUID && binding.Namespace == namespace {
			host := service + "." + namespace + ".svc.cluster.local"
			hash := sha256.Sum256([]byte(agentUID))
			return &Plan{Binding: binding, Image: r.Image, ServiceHost: host, ServerName: "agent-" + hex.EncodeToString(hash[:16]) + "." + host}, nil
		}
	}
	return nil, ErrApprovalPending
}
