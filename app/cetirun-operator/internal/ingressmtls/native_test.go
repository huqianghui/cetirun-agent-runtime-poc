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
)

func TestOpenClawNativeIngressMTLS(t *testing.T) {
	image := os.Getenv("CETIRUN_OPENCLAW_IMAGE")
	if image == "" {
		t.Skip("set CETIRUN_OPENCLAW_IMAGE to an explicitly approved cached OpenClaw 2026.9.6 image")
	}
	binary := os.Getenv("CETIRUN_ENVOY_BINARY")
	if binary == "" {
		t.Fatal("CETIRUN_ENVOY_BINARY is required")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("native-probe.cjs")
	if err != nil {
		t.Fatal(err)
	}
	ca := newTestCA(t)
	plan := Plan{Binding: Binding{Harness: "openclaw", Port: 18789, Terminal: true, ClientURI: "spiffe://example.com/gateway/agent-uid"},
		ServerName: "agent-uid.example.com"}
	_, server, serverKey := ca.issue(t, plan.ServerName, false, false)
	_, clientCert, clientKey := ca.issue(t, plan.ClientURI, true, false)
	bootstrap, err := plan.Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap = bytes.ReplaceAll(bootstrap, []byte(TLSPath), []byte("/work/tls"))
	input, err := json.Marshal(map[string]any{
		"bootstrap": json.RawMessage(bootstrap), "serverName": plan.ServerName,
		"material":   map[string]string{"tls.crt": string(server), "tls.key": string(serverKey), "ca.crt": string(ca.pem)},
		"clientCert": string(clientCert), "clientKey": string(clientKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never", "--network=none", "--read-only",
		"--user=1024:1024", "--workdir=/work", "--tmpfs=/work:rw,mode=0700,uid=1024,gid=1024",
		"--tmpfs=/home/vibe:rw,mode=0700,uid=1024,gid=1024",
		"--mount=type=bind,src="+binary+",dst=/opt/envoy,readonly",
		"--mount=type=bind,src="+script+",dst=/opt/native-probe.cjs,readonly",
		"-e", "HOME=/home/vibe", "-e", "TMPDIR=/work/run",
		"-e", "XDG_CACHE_HOME=/work/cache", "-e", "XDG_STATE_HOME=/work/state", "-e", "SQLITE_TMPDIR=/work/run",
		"-e", "OPENCLAW_HOME=/home/vibe",
		"-e", "OPENCLAW_STATE_DIR=/home/vibe/.openclaw", "-e", "OPENCLAW_CONFIG_PATH=/home/vibe/.openclaw/openclaw.json",
		"--entrypoint=node", "-i", image, "/opt/native-probe.cjs")
	command.Stdin = bytes.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cached native endpoint failed: %v: %s", err, output)
	} else {
		t.Log(strings.TrimSpace(string(output)))
	}
}
