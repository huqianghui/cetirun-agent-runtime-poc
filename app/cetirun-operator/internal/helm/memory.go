// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"fmt"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chart"
	corev1 "k8s.io/api/core/v1"
)

// MemoryOptions is supplied by the controller, never decoded from Helm values.
// Defaults are resolved against the same selected chart used for rendering.
type MemoryOptions struct {
	Enabled  bool
	Image    string
	Registry string
}

func resolveMemory(input map[string]any, chrt *chart.Chart, home string) (*runtimebindings.Memory, bool, error) {
	raw, configured := input["memoryOptions"]
	if !configured {
		return nil, false, nil
	}
	options, ok := raw.(MemoryOptions)
	if !ok {
		return nil, true, fmt.Errorf("Memory options must come from the platform")
	}
	if !options.Enabled || !declaresValuePath(chrt.Values, []string{"memoryContext"}) {
		return nil, true, nil
	}
	image := options.Image
	if image == "" {
		image = runtimebindings.DefaultMemoryImage(options.Registry, chrt.Metadata.AppVersion)
	}
	binding := &runtimebindings.Memory{Image: image, Home: home}
	return binding, true, binding.Validate(home)
}

func memoryMounts() []corev1.VolumeMount {
	return runtimebindings.MemoryMounts()
}
func memoryEnvironment(home string) []corev1.EnvVar {
	return runtimebindings.MemoryEnvironment(home)
}
func memoryVolumes() []corev1.Volume {
	return runtimebindings.MemoryVolumes()
}
func memoryInit(m *runtimebindings.Memory) corev1.Container {
	return runtimebindings.MemoryInit(m)
}
