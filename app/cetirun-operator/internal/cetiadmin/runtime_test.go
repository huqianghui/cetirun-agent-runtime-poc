// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

func TestRuntimeProjectionHasNoManagementListFields(t *testing.T) {
	config, record, observed := bindingFixture(t)
	config.PlatformReadNamespaces = []string{"platform-diagnostics-only"}
	source, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	observed.Source.Data[ConfigKey] = string(source)
	record.ConfigHash, err = ConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := ResolveBinding(config, &record, observed)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := binding.RuntimeProjection("registry.example.com/operator:dev", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scope", "teamNames", "targets", "accessHash", "platformReadNamespaces"} {
		if _, exists := fields[name]; exists {
			t.Errorf("runtime projection contains unused %s", name)
		}
	}
	if strings.Contains(string(data), "platform-diagnostics-only") {
		t.Fatal("platform diagnostics leaked into runtime inputs")
	}
	if err := projection.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeProjectionUsesTheBoundIdentity(t *testing.T) {
	config, record, observed := bindingFixture(t)
	binding, err := ResolveBinding(config, &record, observed)
	if err != nil {
		t.Fatal(err)
	}
	image := "registry.example.com/cetirun-operator:dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	projection, err := binding.RuntimeProjection(image, "api://connections")
	if err != nil {
		t.Fatal(err)
	}
	if projection.OperatorImage != image || projection.AgentUID != string(record.Agent.UID) || projection.NamespaceUID != string(record.Namespace.UID) || projection.ServiceAccountName != record.ServiceAccount.Name {
		t.Fatal("projection replaced the recorded identity or Operator image")
	}
	loaded, err := clientcmd.Load([]byte(projection.Kubeconfig))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CurrentContext != "cetiadmin" || loaded.Contexts["cetiadmin"].Namespace != record.Namespace.Name {
		t.Fatal("kubeconfig did not use the explicit context/namespace")
	}
	cluster, auth := loaded.Clusters["cetiadmin"], loaded.AuthInfos["cetiadmin"]
	if cluster.Server != config.Kubernetes.APIServer || cluster.CertificateAuthority != "/var/run/cetirun/kubernetes/ca.crt" || cluster.InsecureSkipTLSVerify {
		t.Fatal("kubeconfig did not preserve verified TLS inputs")
	}
	if auth.Token != "" || auth.TokenFile != "/var/run/cetirun/kubernetes/token" || auth.Exec != nil || auth.AuthProvider != nil || auth.ClientKey != "" || len(auth.ClientKeyData) != 0 {
		t.Fatal("kubeconfig embedded credentials or added an external login")
	}
	var runtime struct {
		Instance ObjectIdentity `json:"instance"`
		Monitor  Monitor        `json:"monitor"`
	}
	if err := json.Unmarshal([]byte(projection.RuntimeConfig), &runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.Instance.UID != record.Agent.UID || runtime.Instance.Namespace != record.Namespace.Name || runtime.Monitor.Deliver != "local" {
		t.Fatal("native helper did not receive the bound instance and configured monitor")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(projection.RuntimeConfig), &fields); err != nil || len(fields) != 2 || fields["instance"] == nil || fields["monitor"] == nil {
		t.Fatal("monitor input drifted from the native helper contract")
	}
}

func TestRuntimeProjectionRejectsUnboundInputs(t *testing.T) {
	if _, err := (Binding{}).RuntimeProjection("operator:dev", "mcp"); err == nil {
		t.Fatal("zero binding produced runtime inputs")
	}
	config, record, observed := bindingFixture(t)
	binding, err := ResolveBinding(config, &record, observed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.RuntimeProjection("", "mcp"); err == nil {
		t.Fatal("missing Operator image accepted")
	}
	if _, err := binding.RuntimeProjection("operator:dev", config.Kubernetes.APIAudience); err == nil {
		t.Fatal("shared Kubernetes/MCP audience accepted")
	}
}

func TestRuntimeProjectionRejectsExtraCredentialConfiguration(t *testing.T) {
	config, record, observed := bindingFixture(t)
	binding, err := ResolveBinding(config, &record, observed)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*RuntimeProjection)
	}{
		{"monitor trailing JSON", func(p *RuntimeProjection) { p.RuntimeConfig += `{}` }},
		{"kubeconfig trailing JSON", func(p *RuntimeProjection) { p.Kubeconfig += `{}` }},
		{"proxy override", func(p *RuntimeProjection) {
			p.Kubeconfig = strings.Replace(p.Kubeconfig, `"server":`, `"proxy-url":"https://unapproved.example.com","server":`, 1)
		}},
		{"embedded token", func(p *RuntimeProjection) {
			p.Kubeconfig = strings.Replace(p.Kubeconfig, `"tokenFile":`, `"token":"synthetic-not-a-real-key","tokenFile":`, 1)
		}},
		{"impersonation", func(p *RuntimeProjection) {
			p.Kubeconfig = strings.Replace(p.Kubeconfig, `"tokenFile":`, `"as":"another-user","tokenFile":`, 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := binding.RuntimeProjection("registry.example.com/operator:dev", "mcp")
			if err != nil {
				t.Fatal(err)
			}
			test.edit(&p)
			if p.Validate() == nil {
				t.Fatal("extra credential or control input accepted")
			}
		})
	}
}
