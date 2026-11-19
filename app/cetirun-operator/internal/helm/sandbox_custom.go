// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"fmt"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/chart"
	"k8s.io/utils/ptr"
)

func prepareSandboxCustomChart(chrt *chart.Chart, input map[string]any) (*PreparedRelease, error) {
	values, err := prepareRuntimeValues(chrt, input)
	if err != nil {
		return nil, err
	}
	protected := object(values[w.ValuesKey])
	runtime := object(values["runtime"])
	identity := object(runtime["agent"])
	request := agents.Request{
		Type: "custom", Image: text(identity["image"]), Username: text(identity["username"]),
		WebUI: identity["webUI"] == true,
	}
	if raw, present := identity["port"]; present && raw != nil {
		request.Port = ptr.To(number(raw))
	}
	agent, err := agents.ResolveCustom(request)
	if err != nil {
		return nil, err
	}
	if values["agentType"] != "custom" || protected["reconciled"] != true ||
		text(identity["home"]) != agent.HomeDir || text(object(runtime["model"])["apiBase"]) == "" ||
		text(object(runtime["model"])["keySecretName"]) == "" || text(object(runtime["model"])["keySecretKey"]) != "api-key" {
		return nil, fmt.Errorf("Custom requires a complete reconciled Sandbox runtime contribution")
	}
	return &PreparedRelease{Values: values, Agent: agent, ChartVersion: chrt.Metadata.Version, chart: chrt}, nil
}
