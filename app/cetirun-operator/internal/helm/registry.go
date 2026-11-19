// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
)

func nativeNoopUpgrade(previous *release.Release, desired *chart.Chart, values map[string]any) bool {
	return previous != nil && previous.Info != nil && previous.Info.Status == release.StatusDeployed &&
		sameJSON(previous.Chart, desired) && sameJSON(previous.Config, values)
}

// Same runtime inputs, different pull references: update future Pods without
// rolling the healthy writer. A later image/config upgrade restores the chart's
// normal update strategy. Secret content is never present in either values map.
func registryBindingOnly(previous *release.Release, chrt *chart.Chart, next map[string]any) bool {
	if previous == nil || !sameJSON(previous.Chart, chrt) {
		return false
	}
	withoutPull := func(values map[string]any) (map[string]any, string) {
		raw, err := json.Marshal(values)
		if err != nil {
			return nil, ""
		}
		var copy map[string]any
		if json.Unmarshal(raw, &copy) != nil {
			return nil, ""
		}
		runtime := object(copy["runtime"])
		pull, _ := json.Marshal(runtime["imagePullSecrets"])
		delete(runtime, "imagePullSecrets")
		delete(runtime, "preserveRunningPod")
		return copy, string(pull)
	}
	old, oldPull := withoutPull(previous.Config)
	desired, newPull := withoutPull(next)
	return old != nil && desired != nil &&
		(oldPull != newPull || object(previous.Config["runtime"])["preserveRunningPod"] == true) && sameJSON(old, desired)
}
