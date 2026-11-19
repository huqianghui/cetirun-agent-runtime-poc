// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chataccess

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	networkv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	strictjson "sigs.k8s.io/json"
)

const (
	Condition          = "ChatConfigured"
	ContractAnnotation = "cetirun.microsoft.com/portal-chat"
	AgentUIDAnnotation = "cetirun.microsoft.com/agent-uid"
	WorkloadAnnotation = "cetirun.microsoft.com/custom-plan-hash"
	MTLSAPIPort        = 15443
)

type Caller struct {
	Namespace string            `json:"namespace"`
	PodLabels map[string]string `json:"podLabels"`
}

func (c Caller) Validate() error {
	if len(validation.IsDNS1123Label(c.Namespace)) != 0 || len(c.PodLabels) == 0 || len(c.PodLabels) > 16 {
		return fmt.Errorf("Chat caller requires a namespace and bounded Pod labels")
	}
	for key, value := range c.PodLabels {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 || value == "" {
			return fmt.Errorf("invalid Chat caller selector")
		}
	}
	return nil
}

type Config struct {
	Enabled bool     `json:"enabled"`
	Callers []Caller `json:"callers"`
}

func Parse(raw string) (Config, error) {
	var cfg Config
	if raw == "" {
		return cfg, nil
	}
	if len(raw) > 16384 {
		return cfg, fmt.Errorf("Chat access configuration exceeds its bound")
	}
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &cfg)
	if err != nil || len(problems) != 0 {
		return cfg, fmt.Errorf("invalid Chat access configuration")
	}
	if len(cfg.Callers) > 8 || (cfg.Enabled && len(cfg.Callers) == 0) {
		return cfg, fmt.Errorf("Chat requires one to eight explicit callers")
	}
	for _, c := range cfg.Callers {
		if err := c.Validate(); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func Digest(value any) string {
	raw, _ := json.Marshal(value)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func StorageScope(realm, subject string) string {
	if strings.TrimSpace(realm) == "" || strings.TrimSpace(subject) == "" || len(realm) > 1024 || len(subject) > 1024 {
		return ""
	}
	return "v1:" + Digest([]string{"portal-chat-v1", realm, subject})
}

func ServiceName(name string) string {
	value := "openclaw-" + name + "-cetirun"
	if len(value) > 63 {
		value = value[:63]
	}
	return strings.TrimRight(value, "-")
}

func Allows(policy *networkv1.NetworkPolicy, caller Caller, port int32) bool {
	if caller.Validate() != nil {
		return false
	}
	for _, rule := range policy.Spec.Ingress {
		matchedPort := false
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == port && p.Port.StrVal == "" && p.EndPort == nil && (p.Protocol == nil || *p.Protocol == "TCP") {
				matchedPort = true
			}
		}
		if !matchedPort {
			continue
		}
		for _, peer := range rule.From {
			if peer.IPBlock == nil && peer.NamespaceSelector != nil && peer.PodSelector != nil &&
				len(peer.NamespaceSelector.MatchExpressions) == 0 && len(peer.PodSelector.MatchExpressions) == 0 &&
				maps.Equal(peer.NamespaceSelector.MatchLabels, map[string]string{"kubernetes.io/metadata.name": caller.Namespace}) && maps.Equal(peer.PodSelector.MatchLabels, caller.PodLabels) {
				return true
			}
		}
	}
	return false
}
