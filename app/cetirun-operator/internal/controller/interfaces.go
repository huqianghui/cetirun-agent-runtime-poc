// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
)

type CetiAdminBindingProvider interface {
	BindingForAgent(context.Context, *api.Agent) (cetiadmin.Binding, bool, error)
	OperatorImageReference() string
}

// HelmInstaller abstracts Helm install/upgrade/uninstall operations.
// The concrete helm.Client satisfies this interface.
type HelmInstaller interface {
	ResolveImageUpgrade(namespace, releaseName, chartVersion, agentType, image, registry string) (string, error)
	Prepare(namespace string, values map[string]any, chartVersion string) (*helm.PreparedRelease, error)
	Refresh(prepared *helm.PreparedRelease, values map[string]any) (*helm.PreparedRelease, error)
	InstallOrUpgrade(namespace, releaseName string, prepared *helm.PreparedRelease) error
	CustomApplied(namespace, releaseName string, prepared *helm.PreparedRelease) (bool, error)
	Uninstall(namespace, releaseName string) error
}
