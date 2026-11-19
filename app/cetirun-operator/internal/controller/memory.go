// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import "github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"

func (r *AgentReconciler) memoryOptions() helm.MemoryOptions {
	return helm.MemoryOptions{Enabled: r.MemoryEnabled || r.MemoryImage != "", Image: r.MemoryImage, Registry: r.MemoryRegistry}
}
