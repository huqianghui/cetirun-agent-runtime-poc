// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

type Phase string

const (
	PhaseInstalling Phase = "Installing"
	PhaseStopping   Phase = "Stopping"
	PhaseActive     Phase = "Active"
	PhaseReady      Phase = "Ready"
	PhaseDisabled   Phase = "Disabled"
	PhaseBlocked    Phase = "Blocked"
)

type ManagedResource struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	ObjectIdentity
	CreationID string `json:"creationID,omitempty"`
}

type TargetIdentity struct {
	Agent      ObjectIdentity    `json:"agent"`
	Namespace  ObjectIdentity    `json:"namespace"`
	Runtime    string            `json:"runtime"`
	Release    string            `json:"release"`
	Team       string            `json:"team,omitempty"`
	Protected  bool              `json:"protected"`
	AuthSecret string            `json:"authSecret,omitempty"`
	Resources  []ManagedResource `json:"resources,omitempty"`
}

const (
	AnnotationInstallationUID = "cetirun.microsoft.com/cetiadmin-installation-uid"
	AnnotationCreationID      = "cetirun.microsoft.com/cetiadmin-creation-id"
	AnnotationRecovery        = "cetirun.microsoft.com/cetiadmin-recovery"
)

// CreationIntent is saved before CREATE. Its random ID and exact request let a
// retry recognize a lost response without adopting an object found by name.
type CreationIntent struct {
	Resource ManagedResource `json:"resource"`
	Object   json.RawMessage `json:"object"`
}

// RecoveryIdentity identifies the retained Agent when its protected record is
// unreadable. Admission protects this reciprocal receipt too; its Agent UID
// must match the current object, never just the name or source marker.
type RecoveryIdentity struct {
	Record         ObjectIdentity    `json:"record"`
	Source         ObjectIdentity    `json:"source"`
	Agent          ObjectIdentity    `json:"agent"`
	Namespace      ObjectIdentity    `json:"namespace"`
	ServiceAccount ObjectIdentity    `json:"serviceAccount"`
	Resources      []ManagedResource `json:"resources,omitempty"`
}

// Installation is one protected installation checkpoint, not a per-command log.
type Installation struct {
	Record
	Configuration           Config             `json:"configuration"`
	Phase                   Phase              `json:"phase"`
	Reason                  string             `json:"reason,omitempty"`
	PendingCreate           string             `json:"pendingCreate,omitempty"`
	Creation                *CreationIntent    `json:"creation,omitempty"`
	RecordUID               types.UID          `json:"recordUID,omitempty"`
	Hold                    bool               `json:"hold"`
	Bootstrapped            bool               `json:"bootstrapped"`
	BootstrapReleasePending bool               `json:"bootstrapReleasePending,omitempty"`
	OperatorImage           string             `json:"operatorImage"`
	RuntimeVersion          string             `json:"runtimeVersion"`
	ResolvedRuntime         RuntimeCoordinates `json:"resolvedRuntime,omitempty"`
	PreviousSource          *ObjectIdentity    `json:"previousSource,omitempty"`
	Resources               []ManagedResource  `json:"resources,omitempty"`
	legacyAuthorization     bool
}

func InstallationName(namespace, sourceName string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + sourceName))
	return "cetiadmin-binding-" + hex.EncodeToString(sum[:12])
}

func ParseInstallation(raw string) (Installation, error) {
	var state Installation
	type currentInstallation Installation
	stored := struct {
		*currentInstallation
		Configuration  json.RawMessage `json:"configuration"`
		Targets        json.RawMessage `json:"targets,omitempty"`
		ReadNamespaces json.RawMessage `json:"readNamespaces,omitempty"`
		AccessHash     json.RawMessage `json:"accessHash,omitempty"`
	}{currentInstallation: (*currentInstallation)(&state)}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return state, fmt.Errorf("invalid CetiAdmin installation record")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return state, fmt.Errorf("invalid CetiAdmin installation record")
	}
	if len(stored.Configuration) != 0 {
		var legacy bool
		var err error
		state.Configuration, legacy, err = storedConfiguration(stored.Configuration)
		if err != nil {
			return state, err
		}
		state.legacyAuthorization = legacy
	}
	state.legacyAuthorization = state.legacyAuthorization || len(stored.Targets) != 0 || len(stored.ReadNamespaces) != 0 || len(stored.AccessHash) != 0
	if state.SchemaVersion != 1 || state.Source.Name == "" || state.Source.Namespace == "" || state.Source.UID == "" {
		return state, fmt.Errorf("CetiAdmin installation source identity is missing")
	}
	switch state.Phase {
	case PhaseInstalling, PhaseStopping, PhaseActive, PhaseReady, PhaseDisabled, PhaseBlocked:
	default:
		return state, fmt.Errorf("unsupported CetiAdmin installation phase")
	}
	return state, nil
}

func (s Installation) CanRun() bool {
	return !s.legacyAuthorization && !s.Hold && (s.Phase == PhaseActive || s.Phase == PhaseReady)
}

// Old installation records are readable for withdrawal during a normal
// upgrade. Their target inventory never becomes current runtime authority.
func storedConfiguration(raw json.RawMessage) (Config, bool, error) {
	var config Config
	type storedConfig Config
	stored := struct {
		*storedConfig
		Scope *struct {
			Mode                   string   `json:"mode"`
			TeamNames              []string `json:"teamNames,omitempty"`
			PlatformReadNamespaces []string `json:"platformReadNamespaces,omitempty"`
		} `json:"scope,omitempty"`
	}{storedConfig: (*storedConfig)(&config)}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return config, false, fmt.Errorf("invalid stored CetiAdmin configuration")
	}
	if stored.Scope != nil {
		if len(config.PlatformReadNamespaces) != 0 || (stored.Scope.Mode != "all-managed-agents" && stored.Scope.Mode != "teams") {
			return config, true, fmt.Errorf("invalid legacy CetiAdmin configuration")
		}
		config.PlatformReadNamespaces = stored.Scope.PlatformReadNamespaces
	}
	return config, stored.Scope != nil, nil
}

// ReadSourceConfig accepts the old wire shape only to finish stopping the same
// retained installation. New source writes still use strict ParseConfig.
func ReadSourceConfig(source *corev1.ConfigMap, record *Record) (Config, error) {
	if source == nil {
		return Config{}, ErrUnbound
	}
	config, err := ParseConfig(source.Data[ConfigKey])
	if err == nil {
		return config, nil
	}
	if record == nil || !record.Source.matches(source) {
		return Config{}, err
	}
	var marker struct {
		Enabled *bool `json:"enabled"`
	}
	if json.Unmarshal([]byte(source.Data[ConfigKey]), &marker) != nil || marker.Enabled == nil || *marker.Enabled {
		return Config{}, err
	}
	previous, legacy, readErr := storedConfiguration(json.RawMessage(source.Data[ConfigKey]))
	if readErr != nil || !legacy || previous.Enabled || previous.InstanceName != record.Agent.Name || record.Agent.UID == "" {
		return Config{}, err
	}
	return previous, nil
}
