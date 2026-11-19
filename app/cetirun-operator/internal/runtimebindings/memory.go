// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"fmt"
	"regexp"
	"strings"
)

const MemoryAssetsDir = "/opt/cetirun-memory"

var memoryImagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./:_-]*(@sha256:[a-f0-9]{64}|:[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$`)

func DefaultMemoryImage(registry, version string) string {
	if registry == "" {
		registry = "mcr.microsoft.com/aks/ai-runtime/cetirun"
	}
	return strings.TrimSuffix(registry, "/") + "/cetirun-memory-context:" + version
}

// Memory identifies the approved assets and private storage path.
type Memory struct {
	Image string `json:"image"`
	Home  string `json:"home"`
}

func (m *Memory) Validate(home string) error {
	if m == nil {
		return nil
	}
	if !memoryImagePattern.MatchString(m.Image) || m.Home != home {
		return fmt.Errorf("MemoryBindingInvalid: a versioned or digest-pinned image and the resolved HOME are required")
	}
	return nil
}
func (m *Memory) Values() map[string]any {
	if m == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "image": m.Image, "home": m.Home}
}
