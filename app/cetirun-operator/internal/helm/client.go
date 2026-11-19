// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ChartReference describes how to locate and load a Helm chart from an OCI registry.
type ChartReference struct {
	// RepoURL is the OCI registry URL (e.g., "oci://cetirun.azurecr.io/helm/cetirun").
	RepoURL string

	// Version is the semantic version of the chart (e.g., "0.1.0").
	Version string
}

// Validate checks whether the ChartReference configuration is valid.
func (ref *ChartReference) Validate() error {
	if ref.RepoURL == "" {
		return fmt.Errorf("RepoURL must be specified")
	}
	if !strings.HasPrefix(ref.RepoURL, "oci://") {
		return fmt.Errorf("OCI registry URL must start with oci://")
	}
	if ref.Version == "" {
		return fmt.Errorf("Version must be specified")
	}
	return nil
}

// Client wraps the Helm action configuration and provides high-level install/upgrade/uninstall operations.
type Client struct {
	chartRef ChartReference
	writesMu sync.Mutex
	writes   map[string]*releaseWriteState
}

// NewClient creates a new Helm client that loads the chart based on the provided ChartReference.
func NewClient(chartRef ChartReference) *Client {
	return &Client{chartRef: chartRef}
}

// loadChart pulls the chart from the OCI registry.
// cfg must have RegistryClient set (done by actionConfig) so that LocateChart
// can authenticate with the OCI registry.
// version overrides the default chart version; if empty, c.chartRef.Version is used.
func (c *Client) loadChart(cfg *action.Configuration, namespace, version string) (*chart.Chart, error) {
	if version == "" {
		version = c.chartRef.Version
	}
	logger := log.Log.WithName("helm").WithValues("namespace", namespace)
	chartURL := fmt.Sprintf("%s:%s", c.chartRef.RepoURL, version)
	logger.Info("Pulling chart from OCI registry", "url", chartURL)

	// action.NewInstall copies cfg.RegistryClient into ChartPathOptions.registryClient,
	// which is the private field that LocateChart checks for OCI references.
	install := action.NewInstall(cfg)
	install.Version = version

	chartPath, err := install.LocateChart(chartURL, cli.New())
	if err != nil {
		return nil, &ChartDownloadError{Chart: chartURL, Version: version, Err: err}
	}

	chrt, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("load chart from %s: %w", chartPath, err)
	}

	logger.Info("Successfully loaded chart from OCI registry",
		"chartName", chrt.Name(),
		"chartVersion", chrt.Metadata.Version)

	return chrt, nil
}

// actionConfig returns a Helm action.Configuration scoped to the given namespace.
// It initialises a registry client so that OCI chart references can be resolved.
func actionConfig(namespace string) (*action.Configuration, error) {
	cfg := new(action.Configuration)
	// Use the in-cluster REST config.
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("get in-cluster config: %w", err)
	}

	logger := log.Log.WithName("helm")
	logFn := func(format string, v ...interface{}) {
		logger.V(1).Info(fmt.Sprintf(format, v...))
	}

	if err := cfg.Init(restClientGetter(restConfig, namespace), namespace, "secret", logFn); err != nil {
		return nil, fmt.Errorf("init helm action config: %w", err)
	}

	// Initialise the OCI registry client.
	// Prefer the docker credentials file so that ACR access granted via the
	// node's managed identity (written to /root/.docker/config.json by kubelet)
	// is picked up automatically.
	registryClient, err := newRegistryClient()
	if err != nil {
		return nil, fmt.Errorf("init registry client: %w", err)
	}
	cfg.RegistryClient = registryClient

	return cfg, nil
}

// newRegistryClient creates a Helm registry client that reads credentials from
// the docker config file.  It checks the standard locations in order:
//  1. $DOCKER_CONFIG/config.json
//  2. /root/.docker/config.json (AKS kubelet default)
//  3. Helm's own registry config (~/.config/helm/registry/config.json)
func newRegistryClient(extra ...registry.ClientOption) (*registry.Client, error) {
	credentialsFile := dockerConfigFile()

	opts := []registry.ClientOption{
		registry.ClientOptEnableCache(true),
	}
	if credentialsFile != "" {
		opts = append(opts, registry.ClientOptCredentialsFile(credentialsFile))
	}

	return registry.NewClient(append(opts, extra...)...)
}

// dockerConfigFile returns the path to a docker config.json that should be
// used for registry credentials, or empty string to rely on Helm's default.
func dockerConfigFile() string {
	// Honour the standard DOCKER_CONFIG env override.
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		cfgPath := filepath.Join(dir, "config.json")
		if fi, err := os.Stat(cfgPath); err == nil && !fi.IsDir() {
			return cfgPath
		}
	}
	// AKS writes the ACR token here when the node's managed identity has
	// AcrPull permissions.
	const aksDockerConfig = "/root/.docker/config.json"
	if fi, err := os.Stat(aksDockerConfig); err == nil && !fi.IsDir() {
		return aksDockerConfig
	}
	// Fall through: registry.NewClient will use Helm's own credentials file.
	return ""
}

// ChartIdentity names a chart version, or the default one, by its repository
// and version.
func (c *Client) ChartIdentity(version string) string {
	if version == "" {
		version = c.chartRef.Version
	}
	return c.chartRef.RepoURL + ":" + version
}

// chartInspectionTimeout bounds reading a chart for InspectChart. Helm's
// registry client sets no timeout and pulls with a background context, so a
// registry that stalls after accepting a connection would otherwise hold the
// caller indefinitely.
var chartInspectionTimeout = 30 * time.Second

// InspectChart loads a chart version, or the default one, and returns the
// values it declares, without preparing a release. Loading needs only the
// registry, not the cluster. Every registry request ends when ctx is done or
// after chartInspectionTimeout, whichever comes first.
func (c *Client) InspectChart(ctx context.Context, version string) (*ChartValues, error) {
	ctx, cancel := context.WithTimeout(ctx, chartInspectionTimeout)
	defer cancel()
	registryClient, err := newRegistryClient(registry.ClientOptHTTPClient(&http.Client{
		Transport: contextTransport{ctx: ctx, base: &plainHTTPFallback{base: registry.NewTransport(false)}},
	}))
	if err != nil {
		return nil, fmt.Errorf("init registry client: %w", err)
	}
	chrt, err := c.loadChart(&action.Configuration{RegistryClient: registryClient}, "", version)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("read chart %s: %w", c.ChartIdentity(version), ctx.Err())
		}
		return nil, err
	}
	return NewChartValues(chrt), nil
}

// Prepare loads the selected chart and builds its native Agent configuration.
func (c *Client) Prepare(namespace string, values map[string]any, chartVersion string) (*PreparedRelease, error) {
	cfg, err := actionConfig(namespace)
	if err != nil {
		return nil, err
	}

	chrt, err := c.loadChart(cfg, namespace, chartVersion)
	if err != nil {
		return nil, err
	}
	prepared, err := prepareChart(chrt, values, false)
	if err != nil {
		return nil, configurationError(err)
	}
	prepared.namespace = namespace
	if prepared.custom {
		if err := prepared.prepareCustomAction(cfg, namespace, prepared.releaseName); err != nil {
			return nil, err
		}
		config, err := cfg.RESTClientGetter.ToRESTConfig()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := prepared.validateCustomAPI(ctx, config); err != nil {
			return nil, err
		}
	}
	return prepared, nil
}

// InstallOrUpgrade applies the already-prepared chart without waiting for Pod readiness.
func (c *Client) InstallOrUpgrade(namespace, releaseName string, prepared *PreparedRelease) (err error) {
	state, done, err := c.beginReleaseWrite(namespace, releaseName)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, done()) }()
	return c.installOrUpgrade(namespace, releaseName, prepared, func(namespace string) (*action.Configuration, error) {
		cfg, err := actionConfig(namespace)
		if err != nil {
			return nil, err
		}
		trackReleaseWrites(cfg, state)
		return cfg, nil
	})
}

func (c *Client) installOrUpgrade(namespace, releaseName string, prepared *PreparedRelease,
	configure func(string) (*action.Configuration, error),
) error {
	if prepared == nil || prepared.chart == nil {
		return fmt.Errorf("release must be prepared before installation")
	}
	ctx := context.Background()
	if prepared.maintenance != nil {
		ctx = prepared.maintenance.ctx
		if err := prepared.maintenance.check(); err != nil {
			return err
		}
	}
	if (prepared.sandboxBundle != nil || prepared.Values[sandboxworkload.ValuesKey] != nil) && prepared.sandboxReconcile == nil {
		return sandboxworkload.ErrExecutionHeld
	}
	if prepared.sandboxReconcile != nil {
		if prepared.sandboxBundle == nil || namespace != prepared.sandboxBundle.Identity().Namespace.Name ||
			releaseName != prepared.sandboxBundle.Identity().ReleaseName {
			return fmt.Errorf("Sandbox namespace or release mismatch")
		}
		if err := prepared.CheckSandboxApply(context.Background()); err != nil {
			return err
		}
	} else if err := prepared.ValidateCustom(namespace, releaseName); err != nil {
		return err
	}
	if prepared.custom && !prepared.apiValidated {
		return fmt.Errorf("Custom application requires successful API preflight")
	}
	cfg, err := configure(namespace)
	if err != nil {
		return err
	}
	if prepared.custom && prepared.maintenance == nil {
		return prepared.installOrUpgradeCustom(cfg, namespace, releaseName)
	}
	if prepared.maintenance != nil {
		cfg, err = prepared.maintenance.prepare(cfg, prepared)
		if err != nil {
			return err
		}
	}
	if prepared.sandboxReconcile != nil {
		guarded := *cfg
		guarded.KubeClient = &reconciledGrantKube{Interface: cfg.KubeClient, prepared: prepared}
		cfg = &guarded
	}
	chrt, values := prepared.chart, prepared.Values

	exists, err := releaseExists(cfg, releaseName)
	if err != nil {
		return err
	}

	if exists {
		previous, err := cfg.Releases.Deployed(releaseName)
		if err != nil {
			previous, err = cfg.Releases.Last(releaseName)
		}
		if err != nil {
			return fmt.Errorf("read previous release for registry rollout classification")
		}
		pullOnly := prepared.maintenance == nil && registryBindingOnly(previous, chrt, values)
		noHooks := pullOnly || nativeNoopUpgrade(previous, chrt, values)
		// The common Sandbox renderer must apply its already-authorized
		// manifest, not a late mutation of the sealed input values.
		if pullOnly && prepared.sandboxReconcile == nil {
			ensureObject(values, "runtime")["preserveRunningPod"] = true
		}
		upgrade := action.NewUpgrade(cfg)
		upgrade.Namespace = namespace
		upgrade.ReuseValues = false
		upgrade.Wait = false
		upgrade.MaxHistory = 10
		upgrade.DisableHooks = noHooks
		if prepared.maintenance != nil {
			upgrade.DisableHooks = true
			upgrade.MaxHistory = 0
		}
		if prepared.sandboxReconcile != nil {
			upgrade.DisableHooks = true
			upgrade.PostRenderer = reconciledSandboxRenderer{prepared}
		}
		if _, err := upgrade.RunWithContext(ctx, releaseName, chrt, values); err != nil {
			return fmt.Errorf("helm upgrade %s: %w", releaseName, err)
		}
	} else {
		install := action.NewInstall(cfg)
		install.Namespace = namespace
		install.ReleaseName = releaseName
		install.CreateNamespace = false
		install.Wait = false
		if prepared.maintenance != nil {
			// Maintenance requires an existing revision; never turn a lost read
			// or release deletion into a fresh installation.
			return fmt.Errorf("Helm maintenance release disappeared before apply")
		}
		if prepared.sandboxReconcile != nil {
			install.DisableHooks = true
			install.PostRenderer = reconciledSandboxRenderer{prepared}
		}
		_, err := install.RunWithContext(ctx, chrt, values)
		if err != nil {
			// Another reconcile loop may have raced us to the install.
			// Treat "already exists" as success — the release is there.
			if prepared.sandboxReconcile == nil && strings.Contains(err.Error(), "already exists") {
				return nil
			}
			return fmt.Errorf("helm install %s: %w", releaseName, err)
		}
	}
	if prepared.maintenance != nil {
		return prepared.maintenance.verify(cfg)
	}
	return nil
}

func (c *Client) Refresh(prepared *PreparedRelease, values map[string]any) (*PreparedRelease, error) {
	return prepared.Rebuild(values)
}

// Uninstall removes a Helm release.
func (c *Client) Uninstall(namespace, releaseName string) error {
	_, done, err := c.beginReleaseWrite(namespace, releaseName)
	if err != nil {
		return err
	}
	defer done()
	cfg, err := actionConfig(namespace)
	if err != nil {
		return err
	}

	exists, err := c.ReleaseExists(namespace, releaseName)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	uninstall := action.NewUninstall(cfg)
	if _, err := uninstall.Run(releaseName); err != nil {
		return fmt.Errorf("helm uninstall %s: %w", releaseName, err)
	}
	return nil
}

// ReleaseExists reports whether a Helm release with releaseName exists in namespace.
// A failed release is considered to exist (it will be upgraded, not re-installed).
func (c *Client) ReleaseExists(namespace, releaseName string) (bool, error) {
	cfg, err := actionConfig(namespace)
	if err != nil {
		return false, err
	}
	return releaseExists(cfg, releaseName)
}

func releaseExists(cfg *action.Configuration, releaseName string) (bool, error) {
	histClient := action.NewHistory(cfg)
	histClient.Max = 1
	releases, err := histClient.Run(releaseName)
	if err != nil {
		if err == driver.ErrReleaseNotFound {
			return false, nil
		}
		return false, fmt.Errorf("helm history %s: %w", releaseName, err)
	}
	if len(releases) == 0 {
		return false, nil
	}
	status := releases[len(releases)-1].Info.Status
	// Treat uninstalled as not existing; everything else (deployed, failed, pending) as existing.
	return status != release.StatusUninstalled, nil
}
