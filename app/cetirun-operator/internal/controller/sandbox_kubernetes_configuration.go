// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"

const (
	SandboxPlatformConfigurationKey      = sandboxplatform.ConfigurationKey
	MaxSandboxPlatformConfigurationBytes = sandboxplatform.MaxConfigurationBytes
)

type SandboxPlatformConfiguration = sandboxplatform.Configuration

type KubernetesSandboxConfigurationSource = sandboxplatform.KubernetesSource
