// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package terminal provides the platform-owned browser terminal for Custom
// Agents. It never runs a local user shell or accepts a client-selected target.
package terminal

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	BasePath = "/term"
	ExecRole = terminalpolicy.ExecRole
)

type Config struct {
	AgentName   string
	AgentUID    string
	Namespace   string
	StatefulSet string
	Container   string
	Port        int
	BindHost    string
	SessionTTL  time.Duration
}

func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		AgentName:   getenv("CETIRUN_TERMINAL_AGENT_NAME"),
		AgentUID:    getenv("CETIRUN_TERMINAL_AGENT_UID"),
		Namespace:   getenv("CETIRUN_TERMINAL_NAMESPACE"),
		StatefulSet: getenv("CETIRUN_TERMINAL_STATEFULSET"),
		Container:   getenv("CETIRUN_TERMINAL_CONTAINER"),
		BindHost:    getenv("CETIRUN_BIND_HOST"),
		Port:        7681, SessionTTL: 30 * time.Minute,
	}
	if raw := getenv("PORT"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return cfg, fmt.Errorf("invalid terminal PORT")
		}
		cfg.Port = value
	}
	if raw := getenv("CETIRUN_TERMINAL_SESSION_SECONDS"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 60 || value > 3600 {
			return cfg, fmt.Errorf("terminal session duration must be 60–3600 seconds")
		}
		cfg.SessionTTL = time.Duration(value) * time.Second
	}
	return cfg, cfg.Validate()
}

func (c Config) PodName() string { return c.StatefulSet + "-0" }

var uidPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func (c Config) Validate() error {
	if len(validation.IsDNS1123Subdomain(c.AgentName)) != 0 || !uidPattern.MatchString(c.AgentUID) ||
		len(validation.IsDNS1123Label(c.Namespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(c.StatefulSet)) != 0 ||
		len(validation.IsDNS1123Label(c.Container)) != 0 ||
		c.Port < 1024 || c.Port > 65535 || c.SessionTTL < time.Minute || c.SessionTTL > time.Hour ||
		(c.BindHost != "" && c.BindHost != "127.0.0.1") {
		return fmt.Errorf("terminal requires a fixed Agent UID, namespace, StatefulSet, container and valid port")
	}
	return nil
}
