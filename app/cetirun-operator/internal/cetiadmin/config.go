// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	AdmissionValidationPath       = "/validate-cetiadmin"
	AgentDefaultingPath           = "/mutate-sandbox-agent"
	ConfigKey                     = "config.json"
	TokenExpirationSeconds  int64 = 600
)

var (
	ownerAliasPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	identityPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	budgetPattern     = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$`)
)

type Config struct {
	Enabled                bool                  `json:"enabled"`
	InstanceName           string                `json:"instanceName"`
	Owner                  Owner                 `json:"owner"`
	PlatformReadNamespaces []string              `json:"platformReadNamespaces,omitempty"`
	Kubernetes             KubernetesAccess      `json:"kubernetes"`
	Model                  Model                 `json:"model"`
	Runtime                RuntimeCoordinates    `json:"runtime,omitempty"`
	AzureWorkloadIdentity  AzureWorkloadIdentity `json:"azureWorkloadIdentity"`
	SharedSkills           SharedSkills          `json:"sharedSkills"`
	Monitor                Monitor               `json:"monitor"`
	RetainedAgentUID       string                `json:"retainedAgentUID,omitempty"`
	MigrationSource        *MigrationSource      `json:"migrationSource,omitempty"`
}

type MigrationSource struct {
	Agent     ObjectIdentity `json:"agent"`
	Namespace ObjectIdentity `json:"namespace"`
}

type Owner struct {
	Alias    string `json:"alias"`
	TenantID string `json:"tenantID"`
	ObjectID string `json:"objectID"`
}

type KubernetesAccess struct {
	APIServer       string   `json:"apiServer"`
	APIAudience     string   `json:"apiAudience"`
	CAConfigMapName string   `json:"caConfigMapName"`
	APIEgressCIDRs  []string `json:"apiEgressCIDRs"`
}

type Model struct {
	Default                 string `json:"default"`
	InitialMonthlyBudgetUSD string `json:"initialMonthlyBudgetUSD"`
	GitHubCopilot           bool   `json:"githubCopilot"`
}

type RuntimeCoordinates struct {
	Version string `json:"version,omitempty"`
	Image   string `json:"image,omitempty"`
	Port    *int32 `json:"port,omitempty"`
}

type AzureWorkloadIdentity struct {
	Enabled  bool   `json:"enabled"`
	ClientID string `json:"clientId,omitempty"`
}

type SharedSkills struct {
	Enabled bool `json:"enabled"`
}

type Monitor struct {
	Enabled         bool   `json:"enabled"`
	Schedule        string `json:"schedule,omitempty"`
	Timezone        string `json:"timezone"`
	Deliver         string `json:"deliver"`
	SlackChannel    string `json:"slackChannel,omitempty"`
	SlackSecretName string `json:"slackSecretName,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Enabled: true, InstanceName: "cetiadmin",
		Kubernetes: KubernetesAccess{APIServer: "https://kubernetes.default.svc", CAConfigMapName: "kube-root-ca.crt"},
		Model:      Model{Default: "auto"},
		Monitor:    Monitor{Timezone: "UTC", Deliver: "local"},
	}
}

func ParseConfig(raw string) (Config, error) {
	if text := strings.TrimSpace(raw); text == "" || text[0] != '{' {
		return Config{}, fmt.Errorf("CetiAdmin configuration must be a JSON object")
	}
	config := DefaultConfig()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode CetiAdmin configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, fmt.Errorf("CetiAdmin configuration must contain one JSON object")
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if len(validation.IsDNS1123Label(c.InstanceName)) != 0 || len(c.InstanceName) > 44 {
		return fmt.Errorf("instanceName must be a DNS label of at most 44 characters for the openclaw- Helm release")
	}
	if !c.Enabled {
		return nil
	}
	if c.MigrationSource != nil {
		source := c.MigrationSource
		if source.Agent.Name == c.InstanceName || source.Agent.Namespace != "" || source.Namespace.Namespace != "" ||
			len(validation.IsDNS1123Subdomain(source.Agent.Name)) != 0 || len(validation.IsDNS1123Label(source.Namespace.Name)) != 0 ||
			source.Agent.UID == "" || source.Namespace.UID == "" {
			return fmt.Errorf("migrationSource requires distinct source Agent and namespace names with their exact UIDs")
		}
	}
	if !ownerAliasPattern.MatchString(c.Owner.Alias) {
		return fmt.Errorf("owner.alias must be a valid existing owner alias")
	}
	for _, field := range []struct{ name, value string }{
		{"owner.tenantID", c.Owner.TenantID}, {"owner.objectID", c.Owner.ObjectID},
	} {
		if !validIdentity(field.value) {
			return fmt.Errorf("%s must be a nonzero UUID", field.name)
		}
	}
	for _, namespace := range c.PlatformReadNamespaces {
		if len(validation.IsDNS1123Label(namespace)) != 0 {
			return fmt.Errorf("platformReadNamespaces must contain valid namespace names")
		}
	}
	if err := c.Kubernetes.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Model.Default) == "" || len(c.Model.Default) > 256 {
		return fmt.Errorf("model.default must name a model")
	}
	budget, err := strconv.ParseFloat(c.Model.InitialMonthlyBudgetUSD, 64)
	if !budgetPattern.MatchString(c.Model.InitialMonthlyBudgetUSD) || err != nil || math.IsInf(budget, 0) {
		return fmt.Errorf("model.initialMonthlyBudgetUSD must be a nonnegative decimal with at most six fractional digits")
	}
	if c.Runtime.Port != nil && (*c.Runtime.Port < 1024 || *c.Runtime.Port > 65535) {
		return fmt.Errorf("runtime.port must be between 1024 and 65535")
	}
	if c.AzureWorkloadIdentity.Enabled && !validIdentity(c.AzureWorkloadIdentity.ClientID) {
		return fmt.Errorf("azureWorkloadIdentity.clientId must name an existing managed identity")
	}
	return c.Monitor.validate()
}

func (c Config) ValidateMCPAudience(audience string) error {
	if c.Enabled && audience != "" && c.Kubernetes.APIAudience == audience {
		return fmt.Errorf("kubernetes.apiAudience must differ from the Connections MCP audience")
	}
	return nil
}

func (k KubernetesAccess) validate() error {
	server, err := url.Parse(k.APIServer)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" || server.User != nil ||
		(server.Path != "" && server.Path != "/") || server.RawQuery != "" || server.ForceQuery || server.Fragment != "" {
		return fmt.Errorf("kubernetes.apiServer must be an HTTPS origin without credentials")
	}
	if strings.EqualFold(server.Hostname(), "localhost") {
		return fmt.Errorf("kubernetes.apiServer cannot select a loopback address")
	}
	if address, err := netip.ParseAddr(server.Hostname()); err == nil && forbiddenAddress(address) {
		return fmt.Errorf("kubernetes.apiServer cannot select loopback, link-local or multicast addresses")
	}
	if strings.TrimSpace(k.APIAudience) == "" {
		return fmt.Errorf("kubernetes.apiAudience is required")
	}
	if len(validation.IsDNS1123Subdomain(k.CAConfigMapName)) != 0 {
		return fmt.Errorf("kubernetes.caConfigMapName must name the cluster CA ConfigMap")
	}
	if len(k.APIEgressCIDRs) == 0 {
		return fmt.Errorf("kubernetes.apiEgressCIDRs must contain verified API addresses")
	}
	for _, value := range k.APIEgressCIDRs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Bits() != prefix.Addr().BitLen() || forbiddenAddress(prefix.Addr()) {
			return fmt.Errorf("kubernetes.apiEgressCIDRs must contain single-host CIDRs excluding loopback, link-local and multicast addresses")
		}
	}
	return nil
}

func forbiddenAddress(address netip.Addr) bool {
	address = address.Unmap()
	return address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified()
}

func validIdentity(value string) bool {
	return identityPattern.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}

func (m Monitor) validate() error {
	if m.Deliver != "local" && m.Deliver != "slack" {
		return fmt.Errorf("monitor.deliver must be local or slack")
	}
	if _, err := time.LoadLocation(m.Timezone); err != nil || m.Timezone == "" || m.Timezone == "Local" {
		return fmt.Errorf("monitor.timezone must be an IANA timezone")
	}
	if !m.Enabled {
		return nil
	}
	if strings.TrimSpace(m.Schedule) == "" {
		return fmt.Errorf("monitor.schedule is required when monitoring is enabled")
	}
	if m.Deliver == "slack" {
		if strings.TrimSpace(m.SlackChannel) == "" || strings.ContainsAny(m.SlackChannel, " \t\r\n") {
			return fmt.Errorf("monitor.slackChannel must name the explicit Slack destination")
		}
		if len(validation.IsDNS1123Subdomain(m.SlackSecretName)) != 0 {
			return fmt.Errorf("monitor.slackSecretName is required for Slack delivery")
		}
	}
	return nil
}
