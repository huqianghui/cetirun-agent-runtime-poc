// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"errors"
	"fmt"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/builtin"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// ResolveImageUpgrade moves a built-in default only when its deployed chart
// differs from the requested chart. Explicit non-default images stay pinned.
func (c *Client) ResolveImageUpgrade(namespace, releaseName, chartVersion, agentType, image, registry string) (string, error) {
	if agentType != "openclaw" && agentType != "hermes" || image == "" {
		return image, nil
	}
	cfg, err := actionConfig(namespace)
	if err != nil {
		return "", err
	}
	previous, err := cfg.Releases.Deployed(releaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) || errors.Is(err, driver.ErrNoDeployedReleases) {
		return image, nil
	}
	if err != nil {
		return "", fmt.Errorf("read deployed chart for image upgrade: %w", err)
	}
	return resolveImageUpgrade(previous, chartVersion, agentType, image, registry, func() (*chart.Chart, error) {
		return c.loadChart(cfg, namespace, chartVersion)
	})
}

func resolveImageUpgrade(previous *release.Release, version, kind, image, registry string, loadTarget func() (*chart.Chart, error)) (string, error) {
	if previous == nil || previous.Chart == nil || previous.Chart.Metadata == nil ||
		previous.Chart.Metadata.Version == version || kind != "openclaw" && kind != "hermes" {
		return image, nil
	}
	deployed := object(object(previous.Config["runtime"])["agent"])
	if text(deployed["type"]) != kind || text(deployed["image"]) != image {
		return image, nil
	}
	// Historical default images do not require today's native runtime profile.
	oldImage, err := defaultRuntimeImage(previous.Chart, kind, registry)
	if err != nil {
		return "", err
	}
	if image != oldImage {
		return image, nil
	}
	harness, err := builtin.NewRegistry().Lookup(kind)
	if err != nil {
		return "", err
	}
	target, err := loadTarget()
	if err != nil {
		return "", err
	}
	profile, err := loadRuntimeProfile(target, harness.Describe(), registry)
	if err != nil {
		return "", err
	}
	return profile.Image, nil
}
