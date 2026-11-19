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

func TestDesktopNativeIngressMTLS(t *testing.T) {
	image, binary := os.Getenv("CETIRUN_DESKTOP_IMAGE"), os.Getenv("CETIRUN_ENVOY_BINARY")
	if image == "" {
		t.Skip("set CETIRUN_DESKTOP_IMAGE to an explicitly approved cached desktop image")
	}
	if binary == "" {
		t.Fatal("CETIRUN_ENVOY_BINARY is required")
	}
	absolute := func(path string) string {
		value, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	ca := newTestCA(t)
	plan := Plan{Binding: Binding{Harness: "openclaw", Port: 18789, Desktop: true,
		ClientURI: "spiffe://example.com/gateway/desktop-check"}, ServerName: "desktop.example.com"}
	_, cert, key := ca.issue(t, plan.ServerName, false, false)
	_, clientCert, clientKey := ca.issue(t, plan.ClientURI, true, false)
	_, wrongCert, wrongKey := ca.issue(t, "spiffe://example.com/gateway/other-agent", true, false)
	bootstrap, err := plan.Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{
		"bootstrap":  json.RawMessage(bytes.ReplaceAll(bootstrap, []byte(TLSPath), []byte("/work/tls"))),
		"serverName": plan.ServerName,
		"material": map[string]string{"tls.crt": string(cert), "tls.key": string(key), "ca.crt": string(ca.pem),
			"client.crt": string(clientCert), "client.key": string(clientKey), "wrong.crt": string(wrongCert), "wrong.key": string(wrongKey)},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "--rm", "--pull=never", "--network=none", "--read-only", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--user=1024:1024", "--workdir=/work",
		"--tmpfs=/work:rw,mode=0700,uid=1024,gid=1024", "--tmpfs=/home/vibe:rw,mode=0700,uid=1024,gid=1024",
		"-e", "HOME=/home/vibe", "-e", "TMPDIR=/work/run", "-e", "PYTHONDONTWRITEBYTECODE=1",
		"-e", "CETIRUN_INGRESS_MTLS=true", "-e", "NOVNC_PATH_PREFIX=example/desktop/",
		"-e", "VNC_PORT=5901", "-e", "NOVNC_PORT=6081"}
	for _, mount := range [][2]string{
		{absolute(binary), "/opt/envoy"},
		{absolute("desktop-native-probe.py"), "/candidate/probe.py"},
		{absolute("../../../cetirun-chart/configs/desktop-entrypoint.sh"), "/candidate/entrypoint.sh"},
		{absolute("../../../cetirun-chart/configs/desktop-ingress-probe.py"), "/candidate/readiness.py"},
	} {
		args = append(args, "--mount=type=bind,src="+mount[0]+",dst="+mount[1]+",readonly")
	}
	args = append(args, "--entrypoint=python3", "-i", image, "/candidate/probe.py")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native noVNC TLS traversal failed: %v: %s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
