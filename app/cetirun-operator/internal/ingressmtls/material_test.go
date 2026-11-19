// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envoy

package ingressmtls

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEnvoyMissingMaterialFailsClosed(t *testing.T) {
	binary := os.Getenv("CETIRUN_ENVOY_BINARY")
	if binary == "" {
		t.Fatal("CETIRUN_ENVOY_BINARY must name the pinned Envoy binary")
	}
	ca := newTestCA(t)
	_, cert, key := ca.issue(t, "agent.example.com", false, false)
	for _, missing := range []string{"tls.crt", "tls.key", "ca.crt"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			for file, data := range map[string][]byte{"tls.crt": cert, "tls.key": key, "ca.crt": ca.pem} {
				if file != missing {
					if err := os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			config, err := (Plan{Binding: Binding{Harness: "openclaw", Port: 18789, ClientURI: "spiffe://example.com/gateway/agent-uid"}}).Bootstrap()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "envoy.json")
			if err := os.WriteFile(path, bytes.ReplaceAll(config, []byte(TLSPath), []byte(dir)), 0600); err != nil {
				t.Fatal(err)
			}
			if err := exec.Command(binary, "--mode", "validate", "--disable-hot-restart", "-c", path).Run(); err == nil {
				t.Fatal("Envoy accepted missing mandatory TLS material")
			}
		})
	}
}
