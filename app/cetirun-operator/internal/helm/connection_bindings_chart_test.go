// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestOperatorBrokerStorageConfiguration(t *testing.T) {
	chart, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, namespace, account, brokerURL string
		invalid                             bool
	}{
		{name: "disabled"},
		{name: "namespace only", namespace: "credentials", invalid: true},
		{name: "account only", account: "broker", invalid: true},
		{name: "cleanup URL required", namespace: "credentials", account: "broker", invalid: true},
		{name: "enabled", namespace: "credentials", account: "broker", brokerURL: "http://broker.credentials.svc.cluster.local:8080"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered, err := renderChartValues(t, chart, map[string]any{
				"cetiadmin":      map[string]any{"enabled": false},
				"connectionsMCP": map[string]any{"brokerNamespace": test.namespace, "brokerServiceAccount": test.account},
				"agentIdentity":  map[string]any{"brokerURL": test.brokerURL},
			})
			if test.invalid {
				if err == nil {
					t.Fatal("incomplete storage configuration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal([]byte(rendered["cetirun-operator/templates/deployment.yaml"]), &deployment); err != nil {
				t.Fatal(err)
			}
			vars := map[string]string{}
			for _, container := range deployment.Spec.Template.Spec.Containers {
				for _, env := range container.Env {
					vars[env.Name] = env.Value
				}
			}
			if vars["CONNECTIONS_MCP_BROKER_NAMESPACE"] != test.namespace || vars["CONNECTIONS_MCP_BROKER_SERVICE_ACCOUNT"] != test.account || vars["AGENT_ID_BROKER_URL"] != test.brokerURL {
				t.Fatal("storage configuration was not rendered")
			}
			if vars["CONNECTIONS_MCP_URL"] != "" {
				t.Fatal("storage settings activated runtime integration")
			}
		})
	}
}
