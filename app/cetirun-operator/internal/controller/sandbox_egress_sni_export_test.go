// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// An explicit output directory exports only synthetic test fixtures for the
// optional Docker transport check. Ordinary unit tests do not write artifacts.
func exportObservedSNIProof(t *testing.T, f *nativeSandboxFixture, kind string, mtls bool, spec corev1.PodSpec, proxy *corev1.ConfigMap) {
	t.Helper()
	root := os.Getenv("CETIRUN_EGRESS_PROOF_DIR")
	if root == "" {
		return
	}
	mode := "fqdn"
	if mtls {
		mode = "both"
	}
	root = filepath.Join(root, kind+"-"+mode)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write("pod.json", raw)
	for _, name := range []string{"envoy.json", "capture.sh"} {
		write(name, []byte(proxy.Data[name]))
	}
	config := &corev1.ConfigMap{}
	workload := f.workload(t)
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name + "-config"}, config); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"health-probe.cjs", "proxy-health.cjs", "ingress-health.cjs"} {
		if value := config.Data[name]; value != "" {
			write(name, []byte(value))
		}
	}
}
