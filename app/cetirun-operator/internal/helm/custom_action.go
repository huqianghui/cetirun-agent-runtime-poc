// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// prepareCustomAction uses the real action's release revision and capabilities.
// Dry-run disables writes, hooks, CRDs and remote template lookup. Only its
// validated output is sealed; apply never executes the source templates again.
func (p *PreparedRelease) prepareCustomAction(cfg *action.Configuration, namespace, name string) error {
	if len(p.chart.CRDObjects()) != 0 {
		return fmt.Errorf("Custom charts cannot contain CRDs")
	}
	last, err := cfg.Releases.Last(name)
	if err != nil && err != driver.ErrReleaseNotFound {
		return err
	}
	var planned *release.Release
	if err == driver.ErrReleaseNotFound {
		install := action.NewInstall(cfg)
		install.Namespace, install.ReleaseName = namespace, name
		install.DryRun, install.DryRunOption = true, "client"
		install.SkipCRDs, install.DisableHooks = true, true
		planned, err = install.Run(p.chart, p.Values)
	} else {
		if last.Info != nil && last.Info.Status.IsPending() {
			return ErrReleaseOperationPending
		}
		if !p.ownsRelease(last, namespace, name) || last.Info == nil || last.Info.Status.IsPending() ||
			(last.Info.Status != release.StatusDeployed && last.Info.Status != release.StatusFailed) {
			return fmt.Errorf("Custom release is unowned or not ready for an update")
		}
		upgrade := action.NewUpgrade(cfg)
		upgrade.Namespace, upgrade.ReuseValues, upgrade.DisableHooks = namespace, false, true
		upgrade.DryRun, upgrade.DryRunOption = true, "client"
		planned, err = upgrade.Run(name, p.chart, p.Values)
	}
	if err != nil {
		return fmt.Errorf("Custom action preflight failed: %w", err)
	}
	if len(planned.Hooks) != 0 {
		return fmt.Errorf("Custom charts cannot contain hooks")
	}
	p.setCustomCapabilities(cfg.Capabilities)
	if err := p.validateCustomManifest(namespace, name, []byte(planned.Manifest)); err != nil {
		return err
	}
	p.actionRevision = planned.Version
	p.sealedInput = p.inputDigest()
	return nil
}

// Files.Get does not recursively evaluate its payload. Keeping the source
// chart's templates, dependencies or CRDs here would reopen the render boundary.
func (p *PreparedRelease) frozenChart() *chart.Chart {
	metadata := *p.chart.Metadata
	metadata.Dependencies = nil
	return &chart.Chart{
		Metadata:  &metadata,
		Files:     []*chart.File{{Name: "approved.yaml", Data: append([]byte(nil), p.manifest...)}},
		Templates: []*chart.File{{Name: "templates/approved.yaml", Data: []byte(`{{ .Files.Get "approved.yaml" }}`)}},
		Values:    map[string]any{},
	}
}

type customPostRenderer struct {
	prepared  *PreparedRelease
	namespace string
	name      string
}

func (r customPostRenderer) Run(buffer *bytes.Buffer) (*bytes.Buffer, error) {
	if err := r.prepared.validateCustomManifest(r.namespace, r.name, buffer.Bytes()); err != nil {
		return nil, err
	}
	return bytes.NewBuffer(r.prepared.manifest), nil
}

func (p *PreparedRelease) ownsRelease(rel *release.Release, namespace, name string) bool {
	if rel == nil {
		return false
	}
	expected := object(object(p.Values["runtime"])["bindings"])
	actual := object(object(rel.Config["runtime"])["bindings"])
	return expected["agentUID"] != nil && rel.Name == name && rel.Namespace == namespace &&
		actual["agentUID"] == expected["agentUID"] && actual["agentName"] == expected["agentName"] &&
		object(actual["namespace"])["uid"] == object(expected["namespace"])["uid"] &&
		object(actual["home"])["name"] == object(expected["home"])["name"]
}

func (p *PreparedRelease) installOrUpgradeCustom(cfg *action.Configuration, namespace, name string) error {
	if p.actionRevision == 0 || p.sealedInput == "" {
		return fmt.Errorf("Custom application requires sealed action preflight")
	}
	if err := p.ValidateCustom(namespace, name); err != nil {
		return err
	}
	last, err := cfg.Releases.Last(name)
	installing := err == driver.ErrReleaseNotFound
	if err != nil && !installing {
		return err
	}
	if installing {
		if p.actionRevision != 1 {
			return fmt.Errorf("Custom release changed after preflight")
		}
		install := action.NewInstall(cfg)
		install.Namespace, install.ReleaseName = namespace, name
		install.SkipCRDs, install.DisableHooks = true, true
		install.PostRenderer = customPostRenderer{p, namespace, name}
		_, err = install.Run(p.frozenChart(), p.Values)
	} else {
		if !p.ownsRelease(last, namespace, name) || last.Info == nil || last.Info.Status.IsPending() ||
			(last.Info.Status != release.StatusDeployed && last.Info.Status != release.StatusFailed) ||
			last.Version+1 != p.actionRevision {
			return fmt.Errorf("Custom release changed after preflight")
		}
		upgrade := action.NewUpgrade(cfg)
		upgrade.Namespace, upgrade.ReuseValues, upgrade.DisableHooks = namespace, false, true
		upgrade.MaxHistory = 10
		upgrade.PostRenderer = customPostRenderer{p, namespace, name}
		_, err = upgrade.Run(name, p.frozenChart(), p.Values)
	}
	// An existing-name conflict is not proof that this exact plan succeeded.
	return err
}

func (c *Client) CustomApplied(namespace, name string, prepared *PreparedRelease) (bool, error) {
	if err := prepared.ValidateCustom(namespace, name); err != nil {
		return false, err
	}
	cfg, err := actionConfig(namespace)
	if err != nil {
		return false, err
	}
	return prepared.customApplied(cfg, namespace, name)
}

func (p *PreparedRelease) customApplied(cfg *action.Configuration, namespace, name string) (bool, error) {
	last, err := cfg.Releases.Last(name)
	if err == driver.ErrReleaseNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !p.ownsRelease(last, namespace, name) || last.Info == nil || last.Info.Status != release.StatusDeployed ||
		len(last.Hooks) != 0 || object(last.Config["runtime"])["planHash"] != p.PlanHash {
		return false, nil
	}
	// Verify durable Helm state, not just a workload annotation or old Agent
	// status. A changed manifest must not become a successful application proof.
	copy := *p
	if err := copy.validateCustomManifest(namespace, name, []byte(last.Manifest)); err != nil {
		return false, nil
	}
	return true, nil
}
