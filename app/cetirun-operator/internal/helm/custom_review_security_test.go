// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	corev1 "k8s.io/api/core/v1"
)

func TestCustomDecodeRejectsCaseVariants(t *testing.T) {
	for _, input := range []map[string]any{
		{"allowPrivilegeEscalation": true, "allowPrivilegeescalation": false},
		{"readOnlyRootFilesystem": false, "readOnlyRootfilesystem": true},
		{"seccompProfile": map[string]any{"type": "Unconfined"}, "seccompprofile": map[string]any{"type": "RuntimeDefault"}},
	} {
		if err := decodeObject(input, &corev1.SecurityContext{}); err == nil {
			t.Fatalf("case variant accepted: %v", input)
		}
	}
	if err := decodeObject(map[string]any{"allowPrivilegeEscalation": false, "runAsNonRoot": true}, &corev1.SecurityContext{}); err != nil {
		t.Fatalf("legitimate security context rejected: %v", err)
	}
	if err := decodeObject(map[string]any{"hostPath": map[string]any{"path": "/synthetic"},
		"hostpath": nil, "persistentVolumeclaim": map[string]any{"claimName": "home"}}, &corev1.VolumeSource{}); err == nil {
		t.Fatal("volume alias accepted")
	}
	if err := decodeObject(map[string]any{"secretKeyRef": map[string]any{"name": "other", "key": "key"},
		"secretKeyref": map[string]any{"name": "model", "key": "key"}}, &corev1.EnvVarSource{}); err == nil {
		t.Fatal("credential alias accepted")
	}
}

func TestCustomYAMLKeyCoercionCollisionsAreRejected(t *testing.T) {
	for _, test := range []struct{ key, alias string }{{"1", "1"}, {"true", "true"}, {"true", "yes"}} {
		t.Run(test.alias, func(t *testing.T) {
			input := customInput(8080)
			bindings := input["customBindings"].(runtimebindings.Custom)
			bindings.Network.Callers = []runtimebindings.Peer{{Namespace: "callers", PodLabels: map[string]string{test.key: "approved"}}}
			input["customBindings"] = bindings
			p, err := prepareChart(testChart(t), input, false)
			if err != nil {
				t.Fatal(err)
			}
			p.setCustomCapabilities(chartutil.DefaultCapabilities)
			values, err := chartutil.ToRenderValues(p.chart, p.Values, chartutil.ReleaseOptions{Name: p.releaseName, Namespace: "cetirun-example", IsInstall: true}, chartutil.DefaultCapabilities)
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := engine.Render(p.chart, values)
			if err != nil {
				t.Fatal(err)
			}
			var original bytes.Buffer
			for filename, content := range rendered {
				if !strings.HasSuffix(filename, "NOTES.txt") && strings.TrimSpace(content) != "" {
					original.WriteString("---\n" + content + "\n")
				}
			}
			pattern := regexp.MustCompile(`(?m)^([ \t]*)"` + test.key + `": approved$`)
			if !pattern.Match(original.Bytes()) {
				t.Fatal("coercion mutation did not match")
			}
			payload := pattern.ReplaceAllString(original.String(), `${1}"`+test.key+`": approved`+"\n"+`${1}`+test.alias+`: other`)
			for i := 0; i < 64; i++ {
				candidate := *p
				if err := candidate.validateCustomManifest("cetirun-example", p.releaseName, []byte(payload)); err == nil {
					t.Fatal("ambiguous YAML key conversion passed validation")
				}
			}
			control := *p
			if err := control.validateCustomManifest("cetirun-example", p.releaseName, original.Bytes()); err != nil {
				t.Fatalf("quoted label key rejected: %v", err)
			}
		})
	}
}

func TestCustomCaseVariantCannotBeSealed(t *testing.T) {
	ch := testChart(t)
	for _, file := range ch.Templates {
		if file.Name == "templates/agents/custom/_runtime.tpl" {
			file.Data = []byte(strings.Replace(string(file.Data), "allowPrivilegeEscalation: false",
				"allowPrivilegeEscalation: true\nallowPrivilegeescalation: false", 1))
		}
	}
	p := customActionPlan(t, ch)
	if err := p.prepareCustomAction(customActionConfig(), "cetirun-example", p.releaseName); err == nil {
		t.Fatal("case variant passed action preflight")
	}
}

func TestCustomDuplicateYAMLKeysAreRejected(t *testing.T) {
	p, err := PrepareChart(testChart(t), customInput(nil))
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Replace(string(p.manifest), `"kind":"StatefulSet"`, `"kind":"StatefulSet","kind":"StatefulSet"`, 1)
	if payload == string(p.manifest) {
		t.Fatal("duplicate key mutation did not match")
	}
	if err := p.validateCustomManifest("cetirun-example", p.releaseName, []byte(payload)); err == nil {
		t.Fatal("duplicate YAML key passed final validation")
	}
}

func TestCustomHTTPRequiresNetworkIsolation(t *testing.T) {
	for _, port := range []any{nil, 8080} {
		input := customInput(port)
		input["networkPolicy"] = map[string]any{"enabled": false}
		_, err := PrepareChart(testChart(t), input)
		if (err != nil) != (port != nil) {
			t.Fatalf("port=%v error=%v", port, err)
		}
	}
}
