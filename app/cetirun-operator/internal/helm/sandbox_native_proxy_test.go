// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	helmfixture "github.com/Azure/CetiRun/app/cetirun-operator/internal/helm/testfixture"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/chartutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func nativeProxyRenderInput() (w.Input, map[string]any) {
	input := testfixture.New("openclaw").Input
	input.Resolution.Capabilities.AuthenticatedIngress = true
	input.Helpers.Desktop = false
	input.GatewayKey = &w.ObjectIdentity{Name: "gateway-key", UID: "gateway-key-uid"}
	input.AuthorizedSecrets = append(input.AuthorizedSecrets, input.GatewayKey.Name)
	input.Controls.IngressMode, input.Resolution.AgentPolicy.Network.Ingress.Mode = api.AgentPolicyIngressAuthenticated, api.AgentPolicyIngressAuthenticated
	input.Controls.TrustedProxyIPs = []string{"192.0.2.10", "2001:db8::10"}
	values := sampleInput("openclaw", "openai", false)
	secrets := values["secrets"].(map[string]any)
	secrets["existingSecret"], secrets["apiBase"], secrets["model"] = "runtime-secret", "http://gateway.example.com/v1", "example-model"
	values["aiGateway"] = map[string]any{
		"enabled": true, "keySecretName": input.GatewayKey.Name, "keySecretKey": "api-key",
		"models": []any{map[string]any{"id": "example-model", "name": "Example"}},
	}
	values["openclaw"] = map[string]any{"config": map[string]any{"gateway": map[string]any{
		"trustedProxies": []any{"0.0.0.0/0"},
		"auth":           map[string]any{"mode": "token"},
		"controlUi":      map[string]any{"dangerouslyDisableDeviceAuth": false, "allowInsecureAuth": false},
	}}}
	return input, values
}

func TestSandboxNativeProxyTrustRendering(t *testing.T) {
	input, values := nativeProxyRenderInput()
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	files := object(object(prepared.Values["runtime"])["configFiles"])
	if err := json.Unmarshal([]byte(text(files["openclaw.json"])), &config); err != nil {
		t.Fatal(err)
	}
	gateway := object(config["gateway"])
	if !reflect.DeepEqual(gateway["trustedProxies"], []any{"192.0.2.10", "2001:db8::10"}) ||
		object(gateway["auth"])["mode"] != "token" ||
		object(gateway["controlUi"])["dangerouslyDisableDeviceAuth"] != false ||
		object(gateway["controlUi"])["allowInsecureAuth"] != false {
		t.Fatal("native proxy trust was broadened, discarded or changed authentication")
	}
	if !reflect.DeepEqual(object(prepared.Values[w.ValuesKey])["trustedProxyIPs"], gateway["trustedProxies"]) {
		t.Fatal("Go-rendered native config and protected chart hook disagree")
	}
	input.Controls.TrustedProxyIPs[0] = "192.0.2.11"
	changedBundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := PrepareSandboxChart(testChart(t), values, nil, changedBundle)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.WorkloadHash == changed.WorkloadHash || prepared.manifestHash == changed.manifestHash {
		t.Fatal("rendered proxy trust was not bound into the workload and manifest hashes")
	}
}

func TestSandboxNativeProxyCIDRRendering(t *testing.T) {
	for _, preProxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current chart", true: "pre-proxy chart"}[preProxy], func(t *testing.T) {
			chrt := testChart(t)
			if preProxy {
				if err := helmfixture.RestorePreProxyContract(chrt); err != nil {
					t.Fatal(err)
				}
			}
			input, values := nativeProxyRenderInput()
			input.Controls.TrustedProxyIPs = []string{"10.244.0.0/16", "fd00:244::/64"}
			bundle, err := w.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := PrepareSandboxChart(chrt, values, nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			files := object(object(prepared.Values["runtime"])["configFiles"])
			if err := json.Unmarshal([]byte(text(files["openclaw.json"])), &config); err != nil {
				t.Fatal(err)
			}
			gateway := object(config["gateway"])
			if !reflect.DeepEqual(gateway["trustedProxies"], []any{"10.244.0.0/16", "fd00:244::/64"}) ||
				object(gateway["auth"])["mode"] != "token" ||
				object(gateway["controlUi"])["dangerouslyDisableDeviceAuth"] != false {
				t.Fatal("registered proxy CIDRs were lost, broadened or changed authentication")
			}
			if _, err := prepared.SandboxManifest(); err != nil {
				t.Fatal("proxy CIDRs did not produce a validated manifest:", err)
			}
			input.Controls.TrustedProxyIPs[0] = "10.244.0.0/24"
			changedBundle, err := w.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := PrepareSandboxChart(chrt, values, nil, changedBundle)
			if err != nil || prepared.WorkloadHash == changed.WorkloadHash {
				t.Fatal("proxy CIDRs are not bound into the protected workload:", err)
			}
		})
	}
}

func TestSandboxNativeProxyTrustWithPreProxyChart(t *testing.T) {
	chrt := testChart(t)
	if err := helmfixture.RestorePreProxyContract(chrt); err != nil {
		t.Fatal(err)
	}
	input, values := nativeProxyRenderInput()
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	originalSchema := slices.Clone(chrt.Schema)
	originalTemplate := slices.Clone(asset(chrt, "agents/openclaw/config/native.tpl"))
	originalValues, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := bundle.Apply(values)
	if err != nil {
		t.Fatal(err)
	}
	coalesced, err := chartutil.CoalesceValues(chrt, projected)
	if err != nil {
		t.Fatal(err)
	}
	if err := chartutil.ValidateAgainstSchema(chrt, coalesced); err == nil || !strings.Contains(err.Error(), "trustedProxyIPs") {
		t.Fatal("pre-proxy chart fixture no longer rejects the unknown protected hook:", err)
	}
	prepared, err := PrepareSandboxChart(chrt, values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ChartVersion != chrt.Metadata.Version || !bytes.Equal(chrt.Schema, originalSchema) ||
		!bytes.Equal(asset(chrt, "agents/openclaw/config/native.tpl"), originalTemplate) {
		t.Fatal("compatibility rendering changed the chart pin, schema or template")
	}
	afterValues, err := json.Marshal(values)
	if err != nil || !bytes.Equal(originalValues, afterValues) {
		t.Fatal("compatibility rendering mutated caller-owned values:", err)
	}
	if _, present := object(prepared.Values[w.ValuesKey])["trustedProxyIPs"]; present {
		t.Fatal("old chart received an unsupported protected value")
	}
	coalesced, err = chartutil.CoalesceValues(chrt, prepared.Values)
	if err != nil {
		t.Fatal(err)
	}
	if err := chartutil.ValidateAgainstSchema(chrt, coalesced); err != nil {
		t.Fatal("compatibility values bypassed the original chart schema:", err)
	}
	var config map[string]any
	files := object(object(prepared.Values["runtime"])["configFiles"])
	if err := json.Unmarshal([]byte(text(files["openclaw.json"])), &config); err != nil {
		t.Fatal(err)
	}
	gateway := object(config["gateway"])
	if !reflect.DeepEqual(gateway["trustedProxies"], []any{"192.0.2.10", "2001:db8::10"}) ||
		object(gateway["auth"])["mode"] != "token" ||
		object(gateway["controlUi"])["dangerouslyDisableDeviceAuth"] != false ||
		object(gateway["controlUi"])["allowInsecureAuth"] != false {
		t.Fatal("old pinned template discarded exact proxy trust or changed authentication")
	}
	if _, err := prepared.SandboxManifest(); err != nil {
		t.Fatal("old chart did not produce a validated, sealed manifest:", err)
	}
	input.Controls.TrustedProxyIPs[0] = "192.0.2.11"
	changedBundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := PrepareSandboxChart(chrt, values, nil, changedBundle)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.WorkloadHash == changed.WorkloadHash {
		t.Fatal("old chart proxy trust was not bound into the workload hash")
	}
	object(object(object(prepared.Values["openclaw"])["config"])["gateway"])["trustedProxies"] = []any{"0.0.0.0/0"}
	if _, err := prepared.SandboxManifest(); err == nil {
		t.Fatal("old chart accepted proxy input mutation after sealing")
	}
}

func TestSandboxPreProxyCompatibilityRetainsSchemaValidation(t *testing.T) {
	chrt := testChart(t)
	if err := helmfixture.RestorePreProxyContract(chrt); err != nil {
		t.Fatal(err)
	}
	chrt.Schema = bytes.Replace(chrt.Schema, []byte(`"executionHeld": {"const": true}`), []byte(`"executionHeld": {"const": false}`), 1)
	input, values := nativeProxyRenderInput()
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSandboxChart(chrt, values, nil, bundle); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatal("compatibility rendering bypassed an unrelated chart schema constraint:", err)
	}
}

func TestSandboxPreProxyCompatibilityRejectsModifiedTemplate(t *testing.T) {
	chrt := testChart(t)
	if err := helmfixture.RestorePreProxyContract(chrt); err != nil {
		t.Fatal(err)
	}
	for _, file := range chrt.Files {
		if file.Name == "agents/openclaw/config/native.tpl" {
			file.Data = bytes.Replace(file.Data, []byte(`{{- $config := deepCopy .Values.openclaw.config }}`),
				[]byte(`{{- $config := deepCopy .Values.openclaw.config }}
{{- $_ := set (dig "gateway" (dict) $config) "trustedProxies" (list "127.0.0.6") }}`), 1)
		}
	}
	input, values := nativeProxyRenderInput()
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSandboxChart(chrt, values, nil, bundle); err == nil ||
		!strings.Contains(err.Error(), "proxy trust differs") {
		t.Fatal("compatibility rendering accepted a template that discarded the approved proxy list:", err)
	}
}

func TestSandboxNativeProxyTrustRejectsMissingOrModifiedRender(t *testing.T) {
	input, _ := nativeProxyRenderInput()
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	prepared := &PreparedRelease{sandboxBundle: bundle}
	for _, proxies := range [][]string{
		input.Controls.TrustedProxyIPs, nil, {"0.0.0.0/0"}, {"::/0"}, {"127.0.0.6"},
		{"192.0.2.10"}, {"192.0.2.10", "192.0.2.11", "2001:db8::10"},
	} {
		raw, err := json.Marshal(map[string]any{"gateway": map[string]any{"trustedProxies": proxies}})
		if err != nil {
			t.Fatal(err)
		}
		object := unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": input.WorkloadName + "-config"},
			"data":     map[string]any{"openclaw.json": string(raw)},
		}}
		err = prepared.validateSandboxNative(object)
		if (err == nil) != slices.Equal(proxies, input.Controls.TrustedProxyIPs) {
			t.Fatalf("render validation mismatch for %v: %v", proxies, err)
		}
	}
}

func TestSandboxProxyTrustAbsentHookPreservesHeldAndLegacyRendering(t *testing.T) {
	held, _, _ := sandboxRender(t, "openclaw", true, false)
	legacy, err := PrepareChart(testChart(t), sampleInput("openclaw", "openai", false))
	if err != nil {
		t.Fatal(err)
	}
	for mode, prepared := range map[string]*PreparedRelease{"held": held, "legacy": legacy} {
		if _, present := object(prepared.Values[w.ValuesKey])["trustedProxyIPs"]; present {
			t.Fatalf("%s rendering gained a native proxy binding", mode)
		}
		var config map[string]any
		files := object(object(prepared.Values["runtime"])["configFiles"])
		if err := json.Unmarshal([]byte(text(files["openclaw.json"])), &config); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(object(config["gateway"])["trustedProxies"], []any{"127.0.0.6"}) {
			t.Fatalf("%s rendering changed proxy trust without a native hook", mode)
		}
	}
}
