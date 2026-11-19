// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envoy

package ingressmtls

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"sigs.k8s.io/yaml"
)

func TestHermesNativeIngressMTLS(t *testing.T) {
	image := os.Getenv("CETIRUN_HERMES_IMAGE")
	if image == "" {
		t.Skip("set CETIRUN_HERMES_IMAGE to an explicitly approved cached Hermes 0.22.7 image")
	}
	binary := os.Getenv("CETIRUN_ENVOY_BINARY")
	if binary == "" {
		t.Fatal("CETIRUN_ENVOY_BINARY is required")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	chart, err := filepath.Abs("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("hermes-native-probe.cjs")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(chart, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	template, err := os.ReadFile(filepath.Join(chart, "agents/hermes/config/native.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	var bindings agents.Bindings
	bindings.NativeSandbox, bindings.ReconciledSandbox, bindings.NoSignInHelper = true, true, true
	bindings.NativeConfig = values["hermes"].(map[string]any)["config"].(map[string]any)
	bindings.ConfigTemplate = string(template)
	bindings.Model = agents.ModelBinding{
		Provider: "openai", Name: "synthetic-model", BaseURL: "http://127.0.0.1:65530/v1",
		Gateway: true, GatewayKeyEnv: "CETIRUN_GATEWAY_API_KEY",
	}
	bindings.Skills.Empty, bindings.Skills.MountPath = true, "/home/vibe/.aks/skills"
	config, err := agents.RenderConfig(agents.ResolvedAgent{Type: "hermes"}, bindings)
	if err != nil {
		t.Fatal(err)
	}
	for _, retained := range []bool{false, true} {
		name := "fresh"
		if retained {
			name = "retained-and-supervised-restart"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(configPath, []byte(config), 0444); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "managed.yaml"), []byte("webapp:\n  enabled: true\n  host: 127.0.0.1\n  port: 9120\n  public_url: https://webapp.example.com/ui/example\n"), 0444); err != nil {
				t.Fatal(err)
			}
			foreignToken := filepath.Join(dir, "foreign-token")
			if err := os.WriteFile(foreignToken, []byte(strings.Repeat("a", 43)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ca := newTestCA(t)
			plan := Plan{Binding: Binding{Harness: "hermes", Port: 8642, Terminal: true, ClientURI: "spiffe://example.com/gateway/hermes-check"},
				ServerName: "agent-hermes.example.com"}
			_, cert, key := ca.issue(t, plan.ServerName, false, false)
			_, clientCert, clientKey := ca.issue(t, plan.ClientURI, true, false)
			_, wrongCert, wrongKey := ca.issue(t, "spiffe://example.com/gateway/other-agent", true, false)
			_, expiredCert, _ := ca.issue(t, plan.ServerName, false, true)
			bootstrap, err := plan.Bootstrap()
			if err != nil {
				t.Fatal(err)
			}
			input, err := json.Marshal(map[string]any{
				"bootstrap":  json.RawMessage(bytes.ReplaceAll(bootstrap, []byte(TLSPath), []byte("/work/tls"))),
				"serverName": plan.ServerName, "retained": retained,
				"material":   map[string]string{"tls.crt": string(cert), "tls.key": string(key), "ca.crt": string(ca.pem)},
				"clientCert": string(clientCert), "clientKey": string(clientKey),
				"wrongCert": string(wrongCert), "wrongKey": string(wrongKey), "expiredCert": string(expiredCert),
			})
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"run", "--rm", "--pull=never", "--network=none", "--read-only", "--cap-drop=ALL",
				"--security-opt=no-new-privileges", "--user=1024:1024", "--workdir=/work",
				"--tmpfs=/work:rw,mode=0700,uid=1024,gid=1024", "--tmpfs=/home/vibe:rw,mode=0700,uid=1024,gid=1024",
				"--tmpfs=/candidate/foreign-home/webapp:rw,mode=0700,uid=1024,gid=1024",
				"--tmpfs=/var/run/cetirun/ingress-public:rw,mode=0700,uid=1024,gid=1024"}
			for _, mount := range [][2]string{
				{binary, "/opt/envoy"}, {script, "/candidate/probe.cjs"}, {configPath, "/candidate/config.yaml"}, {configPath, "/etc/cetirun/hermes-config.yaml"},
				{filepath.Join(chart, "agents/hermes/bootstrap/hermes-entrypoint.sh"), "/candidate/entrypoint.sh"},
				{filepath.Join(chart, "agents/hermes/bootstrap/hermes-apply-config.py"), "/etc/cetirun/hermes-apply-config.py"},
				{filepath.Join(chart, "agents/hermes/bootstrap/hermes-gateway.sh"), "/etc/cetirun/hermes-gateway.sh"},
				{filepath.Join(chart, "agents/hermes/bootstrap/hermes-runtime.py"), "/etc/cetirun/hermes-runtime.py"},
				{filepath.Join(dir, "managed.yaml"), "/etc/cetirun/hermes-managed/config.yaml"},
				{foreignToken, "/candidate/foreign-home/webapp/session-token"},
				{filepath.Join(chart, "configs/ingress-health.cjs"), "/candidate/ingress-health.cjs"},
				{filepath.Join(chart, "configs/ingress-health.cjs"), "/etc/cetirun/ingress-health.cjs"},
			} {
				args = append(args, "--mount=type=bind,src="+mount[0]+",dst="+mount[1]+",readonly")
			}
			for _, env := range []string{
				"HOME=/home/vibe", "HERMES_HOME=/home/vibe/.hermes", "TMPDIR=/work/run", "SQLITE_TMPDIR=/work/run",
				"XDG_CACHE_HOME=/home/vibe/.cache", "PYTHONDONTWRITEBYTECODE=1", "NODE_DISABLE_COMPILE_CACHE=1",
				"CETIRUN_AI_GATEWAY=true", "CETIRUN_GATEWAY_API_KEY=synthetic-model-key", "CETIRUN_GATEWAY_KEY_ENV=CETIRUN_GATEWAY_API_KEY",
				"HERMES_MANAGED_DIR=/etc/cetirun/hermes-managed", "HERMES_PYTHON=/usr/local/bin/hermes-python",
				"HERMES_WEBAPP_PUBLIC_URL=https://webapp.example.com/ui/example", "CETIRUN_AUTH_KEY=synthetic-signing-secret-independent-0123456789",
				"API_SERVER_ENABLED=true", "API_SERVER_HOST=127.0.0.1", "API_SERVER_PORT=8642", "API_SERVER_KEY=synthetic-api-key-retained-0123456789",
				"HERMES_WEBAPP_ENABLED=true", "HERMES_WEBAPP_HOST=127.0.0.1", "HERMES_WEBAPP_PORT=9120",
				"HERMES_DASHBOARD_BASIC_AUTH_USERNAME=cetirun", "HERMES_DASHBOARD_BASIC_AUTH_PASSWORD=synthetic-api-key-retained-0123456789",
				"HERMES_DASHBOARD_BASIC_AUTH_SECRET=synthetic-signing-secret-independent-0123456789", "TTYD_ENABLED=true", "TTYD_PORT=7681",
				"TTYD_SHELL=/bin/zsh", "TTYD_BASE_PATH=/term", "TAILSCALE_ENABLED=false",
				"CETIRUN_INGRESS_MTLS=true", "CETIRUN_INGRESS_SERVER_NAME=agent-hermes.example.com",
			} {
				args = append(args, "-e", env)
			}
			args = append(args, "--entrypoint=node", "-i", image, "/candidate/probe.cjs")
			ctx, cancel := context.WithTimeout(context.Background(), 170*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "docker", args...)
			command.Stdin = bytes.NewReader(input)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("native Hermes listener authority failed: %v: %s", err, output)
			}
			t.Log(strings.TrimSpace(string(output)))
		})
	}
}
