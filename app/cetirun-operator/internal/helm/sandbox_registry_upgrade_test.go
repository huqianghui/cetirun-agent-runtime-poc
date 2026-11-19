// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"context"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/action"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestReconciledRegistryUpgradePreservesSealedInputs(t *testing.T) {
	for _, adding := range []bool{true, false} {
		t.Run(map[bool]string{true: "add", false: "remove"}[adding], func(t *testing.T) {
			source, _, _ := sandboxSDKKubernetesAccessPlans(t)
			prepare := func(pull bool) *PreparedRelease {
				input := source.sandboxBundle.Identity()
				input.Reconciled = true
				values := sampleInput(input.AgentType, "openai", false)
				object(values["secrets"])["existingSecret"] = "runtime-secret"
				if pull {
					input.ImagePullSecret = &w.ObjectIdentity{Name: "registry-pull-example", UID: "pull-secret-uid"}
					object(values["agentRequest"])["pullSecretName"] = input.ImagePullSecret.Name
				}
				bundle, err := w.Build(input)
				if err != nil {
					t.Fatal(err)
				}
				guard := func(context.Context) error { return nil }
				prepared, err := source.ReconcileSandboxWithPreflight(values, bundle, guard, guard, guard,
					func(context.Context, unstructured.Unstructured) (func(context.Context) error, error) {
						return guard, nil
					}, nil)
				if err != nil {
					t.Fatal(err)
				}
				return prepared
			}
			first, next := prepare(!adding), prepare(adding)
			cfg, _, _ := sandboxSDKConfig()
			configure := func(string) (*action.Configuration, error) { return cfg, nil }
			namespace := first.sandboxReconcile.Namespace.Name
			if err := (&Client{}).installOrUpgrade(namespace, first.releaseName, first, configure); err != nil {
				t.Fatal(err)
			}
			previous, err := cfg.Releases.Deployed(first.releaseName)
			if err != nil {
				t.Fatal(err)
			}
			if !registryBindingOnly(previous, next.chart, next.Values) {
				t.Fatal("fixture does not exercise the legacy pull-binding classification")
			}
			digest, manifest := next.inputDigest(), bytes.Clone(next.manifest)
			if err := (&Client{}).installOrUpgrade(namespace, next.releaseName, next, configure); err != nil {
				t.Fatal("registry reference change invalidated its sealed Sandbox render:", err)
			}
			if next.inputDigest() != digest || next.sealedInput != digest || !bytes.Equal(next.manifest, manifest) {
				t.Fatal("SDK upgrade changed sealed inputs or manifest")
			}
		})
	}
}
