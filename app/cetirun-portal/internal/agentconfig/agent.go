// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"k8s.io/utils/ptr"
)

// Field preserves the distinction between omission, null and a concrete value.
type Field[T any] struct {
	Set   bool
	Null  bool
	Value T
}

func (f *Field[T]) UnmarshalJSON(data []byte) error {
	f.Set = true
	f.Null = bytes.Equal(bytes.TrimSpace(data), []byte("null"))
	if f.Null {
		return nil
	}
	return json.Unmarshal(data, &f.Value)
}

func (f Field[T]) IsZero() bool { return !f.Set }

func (f Field[T]) MarshalJSON() ([]byte, error) {
	if f.Null || !f.Set {
		return []byte("null"), nil
	}
	return json.Marshal(f.Value)
}

func Value[T any](value T) Field[T] { return Field[T]{Set: true, Value: value} }

// Input deliberately excludes platform-owned Registry references.
type Input struct {
	Type     Field[api.AgentType] `json:"type,omitzero"`
	Image    Field[string]        `json:"image,omitzero"`
	Username Field[string]        `json:"username,omitzero"`
	Port     Field[int32]         `json:"port,omitzero"`
	WebUI    Field[bool]          `json:"webUI,omitzero"`
}

func (in Input) Empty() bool {
	return !in.Type.Set && !in.Image.Set && !in.Username.Set && !in.Port.Set && !in.WebUI.Set
}

func (in Input) Create() (api.HarnessSpec, error) {
	agent := Defaults(api.HarnessSpec{})
	if in.Image.Set && in.Image.Null {
		return api.HarnessSpec{}, fmt.Errorf("agent.image must not be null on creation")
	}
	if err := in.merge(&agent); err != nil {
		return api.HarnessSpec{}, err
	}
	return agent, Validate(agent)
}

func (in Input) Apply(current api.HarnessSpec) (api.HarnessSpec, error) {
	agent := Defaults(current)
	if err := in.merge(&agent); err != nil {
		return api.HarnessSpec{}, err
	}
	previous := Defaults(current)
	if agent.Type != previous.Type {
		return api.HarnessSpec{}, fmt.Errorf("agent.type cannot change after creation")
	}
	if *agent.Username != *previous.Username {
		return api.HarnessSpec{}, fmt.Errorf("agent.username cannot change after creation")
	}
	if current.Image != nil && agent.Image == nil {
		return api.HarnessSpec{}, fmt.Errorf("a pinned agent.image cannot be cleared")
	}
	return agent, Validate(agent)
}

func (in Input) merge(agent *api.HarnessSpec) error {
	if in.Type.Set {
		if in.Type.Null || in.Type.Value == "" {
			return fmt.Errorf("agent.type must be hermes, openclaw or custom")
		}
		agent.Type = in.Type.Value
	}
	if in.Username.Set {
		if in.Username.Null {
			return fmt.Errorf("agent.username must not be null")
		}
		agent.Username = ptr.To(in.Username.Value)
	}
	if in.Image.Set {
		agent.Image = nil
		if !in.Image.Null {
			agent.Image = ptr.To(in.Image.Value)
		}
	}
	if in.Port.Set {
		agent.Port = nil
		if !in.Port.Null {
			agent.Port = ptr.To(in.Port.Value)
		}
	}
	if in.WebUI.Set {
		agent.WebUI = !in.WebUI.Null && in.WebUI.Value
	}
	return nil
}

func Defaults(agent api.HarnessSpec) api.HarnessSpec {
	agent = *agent.DeepCopy()
	if agent.Type == "" {
		agent.Type = api.DefaultAgentType
	}
	if agent.Username == nil {
		agent.Username = ptr.To(api.DefaultAgentUsername)
	}
	return agent
}

var username = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var imageReference = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+(([._]|__|[-]+)[a-z0-9]+)*)*(:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127})?(@sha256:[a-f0-9]{64})?$`)

func Validate(agent api.HarnessSpec) error {
	switch agent.Type {
	case api.AgentTypeHermes, api.AgentTypeOpenClaw, api.AgentTypeCustom:
	default:
		return fmt.Errorf("agent.type must be hermes, openclaw or custom")
	}
	if agent.Username == nil || !username.MatchString(*agent.Username) || *agent.Username == "root" {
		return fmt.Errorf("agent.username must name a non-root image user, using 1-32 lowercase letters, digits, underscores or hyphens")
	}
	if agent.Type != api.AgentTypeCustom && *agent.Username != api.DefaultAgentUsername {
		return fmt.Errorf("built-in agents require username vibe")
	}
	if agent.Type == api.AgentTypeCustom && agent.Image == nil {
		return fmt.Errorf("custom agents require agent.image")
	}
	if agent.Image != nil && (len(*agent.Image) > 2048 || !imageReference.MatchString(*agent.Image)) {
		return fmt.Errorf("agent.image must be an OCI image reference without a URL scheme or whitespace")
	}
	if agent.Port != nil && (*agent.Port < 1 || *agent.Port > 65535) {
		return fmt.Errorf("agent.port must be between 1 and 65535")
	}
	if agent.WebUI && (agent.Type != api.AgentTypeCustom || agent.Port == nil) {
		return fmt.Errorf("agent.webUI requires a custom agent with port")
	}
	return nil
}
