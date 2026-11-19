// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package agents describes native Agent configuration without deployment
// clients, credentials, or ownership of the Agent's execution loop.
package agents

type Harness interface {
	Describe() Descriptor
	Validate(ResolvedAgent, Bindings) error
	Build(ResolvedAgent, Bindings) (Contribution, error)
}

type Descriptor struct {
	Type             string
	NativeVersion    string
	NativeCommit     string
	ConfigFile       string
	PortName         string
	DefaultPort      int32
	HealthPath       string
	ReadinessPath    string
	StartupPath      string
	WebappPort       int32
	PreserveUIPrefix bool
	NativeSecretKey  string
	EnvironmentFile  string
}

type Request struct {
	Type     string `json:"type"`
	Image    string `json:"image,omitempty"`
	Username string `json:"username,omitempty"`
	Port     *int32 `json:"port,omitempty"`
	WebUI    bool   `json:"webUI,omitempty"`
}

type Profile struct {
	// Profiles belong to the selected chart, not to a mutable global catalog.
	ID              string
	Image           string
	Port            int32
	QualifiedImages []string
}

type ResolvedAgent struct {
	Type     string `json:"type"`
	Image    string `json:"image"`
	Username string `json:"username"`
	HomeDir  string `json:"home"`
	Port     *int32 `json:"port,omitempty"`
	WebUI    bool   `json:"webUI,omitempty"`
}

// Bindings contains only non-sensitive, platform-selected inputs. ConfigTemplate
// is an asset in the selected chart, never a user-supplied Helm value.
type Bindings struct {
	ProfileID         string
	ChartVersion      string
	NativeConfig      map[string]any
	NativeOptions     map[string]any
	NativeSandbox     bool
	ReconciledSandbox bool
	IngressMTLS       bool     `json:",omitempty"`
	TrustedProxyIPs   []string `json:",omitempty"`
	Model             ModelBinding
	Skills            SkillsBinding
	Integrations      Integrations
	ConnectionsMCP    ConnectionsMCPBinding
	ConfigTemplate    string
	ReservedPorts     []PortBinding
	ContextRevision   string
	MemoryEnabled     bool
	DesktopEnabled    bool
	WebBasePath       string
	WebInstance       string
	// NoSignInHelper reports that the selected chart runs no in-agent model
	// sign-in helper: the Agent's models come through the platform AI
	// Gateway, and GitHub Copilot sign-in happens in the Portal.
	NoSignInHelper bool
}

type ModelBinding struct {
	Provider       string
	Name           string
	BaseURL        string
	AuthMode       string
	CredentialSlot string
	Revision       string
	// Gateway reports that model traffic goes through the platform AI
	// Gateway, which also replaces the in-agent model sign-in helper.
	Gateway bool
	// GatewayModels are the models the Gateway offers the Agent.
	GatewayModels []GatewayModel
	// GatewayKeyEnv names the environment variable that carries the Agent's
	// Gateway key to Hermes (aiGateway.hermes.keyEnv). The chart sets that
	// variable in Hermes' containers, so Hermes' configuration must name the
	// same one. Empty for charts that do not declare it.
	GatewayKeyEnv string
	// GatewayVisionModels are the IDs of the GatewayModels that accept
	// images (aiGateway.visionModels). Empty for charts that do not declare
	// the value.
	GatewayVisionModels []string
}

type ConnectionsMCPBinding struct {
	Enabled bool
	URL     string
}

// GatewayModel is one model the AI Gateway offers an Agent, with the limits
// the Agent should assume.
type GatewayModel struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
}

type SkillsBinding struct {
	Shared    bool
	Empty     bool `json:",omitempty"`
	MountPath string
	NPM       []string
}

// Presence flags enable native integrations without passing their credentials
// to a renderer or including those credentials in a ConfigMap.
type Integrations struct {
	ExistingSecret    bool
	Brave             bool
	Perplexity        bool
	Telegram          bool
	TelegramTokenFile string
	Slack             bool
	Discord           bool
	Feishu            bool
	Tailscale         bool
}

type Contribution struct {
	Agent       ResolvedAgent      `json:"agent"`
	ProfileID   string             `json:"profileID"`
	ConfigFiles map[string]string  `json:"configFiles"`
	Ports       []PortBinding      `json:"ports"`
	HTTP        *HTTPBinding       `json:"http,omitempty"`
	Environment []EnvironmentValue `json:"environment,omitempty"`
}

// CredentialSlot is resolved by the platform, never an adapter-selected Secret.
type EnvironmentValue struct {
	Name           string `json:"name"`
	Value          string `json:"value,omitempty"`
	CredentialSlot string `json:"credentialSlot,omitempty"`
}

type PortBinding struct {
	Name   string `json:"name"`
	Number int32  `json:"number"`
}

type HTTPBinding struct {
	PortName      string `json:"portName"`
	HealthPath    string `json:"healthPath"`
	ReadinessPath string `json:"readinessPath,omitempty"`
	StartupPath   string `json:"startupPath"`
	ChatPath      string `json:"chatPath,omitempty"`
	ModelsPath    string `json:"modelsPath,omitempty"`
}
