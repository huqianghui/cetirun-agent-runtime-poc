// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/agentconfig"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/utils/ptr"
)

// SandboxRuntime describes inputs approved by the deployment's runtime chart.
// These are creation defaults, not a grant to bypass runtime admission.
type SandboxRuntime struct {
	ChartVersion string `json:"chartVersion"`
	Image        string `json:"image"`
	Port         int32  `json:"port"`
}

var runtimeDigest = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)

func (c Config) SandboxRuntimeFor(kind api.AgentType) (SandboxRuntime, bool) {
	runtime, ok := c.SandboxRuntimes[string(kind)]
	if !ok || !runtimeDigest.MatchString(runtime.Image) {
		return SandboxRuntime{}, false
	}
	if _, err := version.ParseSemantic(runtime.ChartVersion); err != nil {
		return SandboxRuntime{}, false
	}
	if (kind != api.AgentTypeOpenClaw || runtime.Port != 18789) &&
		(kind != api.AgentTypeHermes || runtime.Port != 8642) {
		return SandboxRuntime{}, false
	}
	agent := agentconfig.Defaults(api.HarnessSpec{Type: kind, Image: ptr.To(runtime.Image), Port: ptr.To(runtime.Port)})
	return runtime, agentconfig.Validate(agent) == nil
}

func loadSandboxRuntimes(raw string) map[string]SandboxRuntime {
	if len(raw) > 64*1024 {
		return nil
	}
	var runtimes map[string]SandboxRuntime
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&runtimes) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil
	}
	for kind := range runtimes {
		if _, ok := (Config{SandboxRuntimes: runtimes}).SandboxRuntimeFor(api.AgentType(kind)); !ok {
			return nil
		}
	}
	return runtimes
}
