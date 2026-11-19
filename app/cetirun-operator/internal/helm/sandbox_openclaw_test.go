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

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func openClawPortalSignInInputs(t *testing.T, reconciled, mtls, enabled bool) (*w.Bundle, map[string]any) {
	t.Helper()
	input, values := nativeProxyRenderInput()
	input.Reconciled = reconciled
	object(values["openclaw"])["portalSignIn"] = map[string]any{"enabled": enabled}
	values["desktop"] = map[string]any{"enabled": false}
	if mtls {
		input.Resolution.Capabilities.MutualTLS = true
		input.Resolution.AgentPolicy.Network.Ingress.RequireMTLS = true
		ref := func(namespace, name string) ingressmtls.Reference {
			return ingressmtls.Reference{Namespace: namespace, Name: name, UID: types.UID(name + "-uid"), ResourceVersion: "1"}
		}
		registration := ingressmtls.Registration{
			Image: "registry.example.com/envoy@sha256:" + strings.Repeat("a", 64),
			Bindings: []ingressmtls.Binding{{
				AgentUID: input.Resolution.Binding.AgentUID, Namespace: input.Namespace.Name,
				Harness: "openclaw", Port: 18789, Terminal: true,
				ServerSecret: ref(input.Namespace.Name, "agent-tls"),
				ClientSecret: ref("example-platform", "gateway-client"), Gateway: ref("example-platform", "ingress"),
				ClientURI: "spiffe://example.com/gateway/" + string(input.Resolution.Binding.AgentUID),
			}},
		}
		plan, err := registration.Resolve(input.Resolution.Binding.AgentUID, input.Namespace.Name, input.WorkloadName)
		if err != nil {
			t.Fatal(err)
		}
		input.Controls.IngressMTLS = plan
		resources, err := sandboxpolicy.IngressMTLSResources(sandboxpolicy.CompileInput{
			Resolution: *input.Resolution, Namespace: input.Namespace.Name, AgentName: input.AgentName,
		}, *plan, input.Controls.Revision)
		if err != nil {
			t.Fatal(err)
		}
		input.Controls.Objects = append(input.Controls.Objects, resources...)
	}
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	return bundle, values
}

func TestSandboxOpenClawPortalSignIn(t *testing.T) {
	for _, test := range []struct {
		name                 string
		reconciled, mtls, on bool
	}{
		{name: "single-start", on: true},
		{name: "plain common", reconciled: true, on: true},
		{name: "mTLS off", reconciled: true, mtls: true},
		{name: "mTLS on", reconciled: true, mtls: true, on: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle, values := openClawPortalSignInInputs(t, test.reconciled, test.mtls, test.on)
			gateway := object(object(object(values["openclaw"])["config"])["gateway"])
			gateway["mode"] = "local"
			object(gateway["auth"])["token"] = "sk-dummy-local"
			remote := map[string]any{"url": "wss://gateway.example.com", "token": "sk-dummy-remote"}
			gateway["remote"] = remote
			chrt := testChart(t)
			prepared, err := PrepareSandboxChart(chrt, values, nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			files := object(object(prepared.Values["runtime"])["configFiles"])
			var config map[string]any
			if err := json.Unmarshal([]byte(text(files["openclaw.json"])), &config); err != nil {
				t.Fatal(err)
			}
			standaloneValues, err := bundle.Apply(values)
			if err != nil {
				t.Fatal(err)
			}
			standalone := object(renderSnapshot(t, chrt, standaloneValues).Native["openclaw.json"])
			if !reflect.DeepEqual(config["gateway"], standalone["gateway"]) {
				t.Fatal("Operator and standalone Helm gateway authentication disagree")
			}
			name, mode := "OPENCLAW_GATEWAY_TOKEN", "token"
			if test.mtls && test.on {
				name, mode = "OPENCLAW_GATEWAY_PASSWORD", "trusted-proxy"
				if object(config["gateway"])["bind"] != "loopback" ||
					!reflect.DeepEqual(object(config["gateway"])["trustedProxies"], []any{"127.0.0.1"}) {
					t.Fatal("Portal sign-in widened the managed proxy boundary")
				}
			}
			if object(object(config["gateway"])["auth"])["mode"] != mode {
				t.Fatal("Sandbox sign-in did not follow the mTLS opt-in")
			}
			if test.mtls && test.on {
				remote = map[string]any{"url": "wss://gateway.example.com"}
				if _, present := object(object(config["gateway"])["auth"])["token"]; present {
					t.Fatal("Portal sign-in retained the local gateway token")
				}
			}
			if object(config["gateway"])["mode"] != "local" ||
				!reflect.DeepEqual(object(config["gateway"])["remote"], remote) {
				t.Fatal("remote gateway credentials did not follow the selected authentication mode")
			}
			template, err := prepared.SandboxTemplate()
			if err != nil {
				t.Fatal(err)
			}
			containers := append([]corev1.Container{template.Spec.Containers[0]}, template.Spec.InitContainers...)
			for _, container := range containers {
				if container.Name != "cetirun" && container.Name != "doctor" {
					continue
				}
				var env []corev1.EnvVar
				for _, item := range container.Env {
					if item.Name == "OPENCLAW_GATEWAY_TOKEN" || item.Name == "OPENCLAW_GATEWAY_PASSWORD" {
						env = append(env, item)
					}
				}
				if container.Name == "doctor" {
					if len(env) != 0 {
						t.Fatal("doctor initializer acquired gateway credentials")
					}
					continue
				}
				if len(env) != 1 || env[0].Name != name || env[0].ValueFrom == nil ||
					env[0].ValueFrom.SecretKeyRef == nil || env[0].ValueFrom.SecretKeyRef.Name != "runtime-secret" ||
					env[0].ValueFrom.SecretKeyRef.Key != "OPENCLAW_GATEWAY_TOKEN" {
					t.Fatalf("%s lost the selected authentication credential: %+v", container.Name, env)
				}
			}
		})
	}
}

func TestSandboxOpenClawPortalSignInRejectsChangedChart(t *testing.T) {
	for _, test := range []struct {
		name, asset, before, after, message string
	}{
		{"auth mode", "agents/openclaw/config/native.tpl", `"mode" "trusted-proxy"`, `"mode" "none"`, "authentication contract"},
		{"identity header", "agents/openclaw/config/native.tpl", `"userHeader" "x-auth-request-email"`, `"userHeader" "x-example-user"`, "authentication contract"},
		{"loopback auth", "agents/openclaw/config/native.tpl", `"allowLoopback" true`, `"allowLoopback" false`, "authentication contract"},
		{"device approval", "agents/openclaw/config/native.tpl", `"deviceAutoApprove" (dict "enabled" true)`, `"deviceAutoApprove" (dict "enabled" false)`, "authentication contract"},
		{"retained token", "agents/openclaw/config/native.tpl", `"mode" "trusted-proxy"`, `"mode" "trusted-proxy" "token" "sk-dummy"`, "authentication contract"},
		{"remote token", "agents/openclaw/config/native.tpl", `{{- $_ := set $gateway "auth" (dict`,
			`{{- $_ := set $gateway "remote" (dict "token" "sk-dummy") }}{{- $_ := set $gateway "auth" (dict`, "authentication contract"},
		{"remote token reference", "agents/openclaw/config/native.tpl", `{{- $_ := set $gateway "auth" (dict`,
			`{{- $_ := set $gateway "remote" (dict "token" (dict "source" "env" "provider" "default" "id" "EXAMPLE_REMOTE_TOKEN")) }}{{- $_ := set $gateway "auth" (dict`, "authentication contract"},
		{"proxy trust", "agents/openclaw/config/native.tpl", `{{- $_ := set $gateway "auth" (dict`,
			`{{- $_ := set $gateway "trustedProxies" (list "127.0.0.1" "192.0.2.10") }}{{- $_ := set $gateway "auth" (dict`, "proxy trust differs"},
		{"older native asset", "agents/openclaw/config/native.tpl",
			`(or (not .Values.sandboxWorkload) (dig "ingressMTLS" false (.Values.sandboxWorkload | default dict)))`,
			`(not .Values.sandboxWorkload)`, "authentication contract"},
		{"env mode", "templates/agents/openclaw/_runtime.tpl", "- name: OPENCLAW_GATEWAY_PASSWORD", "- name: OPENCLAW_GATEWAY_TOKEN", "gateway environment"},
		{"missing env", "templates/agents/openclaw/_runtime.tpl", "- name: OPENCLAW_GATEWAY_PASSWORD", "- name: EXAMPLE_GATEWAY_PASSWORD", "gateway environment"},
		{"extra token env", "templates/agents/openclaw/_runtime.tpl", "- name: OPENCLAW_GATEWAY_PASSWORD",
			"- name: OPENCLAW_GATEWAY_TOKEN\n  value: sk-dummy\n- name: OPENCLAW_GATEWAY_PASSWORD", "gateway environment"},
		{"credential key", "templates/agents/openclaw/_runtime.tpl", "key: OPENCLAW_GATEWAY_TOKEN", "key: WRONG_GATEWAY_KEY", "gateway environment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle, values := openClawPortalSignInInputs(t, true, true, true)
			chrt := testChart(t)
			found := false
			for _, file := range append(slices.Clone(chrt.Files), chrt.Templates...) {
				if file.Name == test.asset {
					if bytes.Count(file.Data, []byte(test.before)) != 1 {
						t.Fatal("chart mutation no longer matches exactly once")
					}
					file.Data = bytes.Replace(file.Data, []byte(test.before), []byte(test.after), 1)
					found = true
				}
			}
			if !found {
				t.Fatal("chart mutation asset was not found")
			}
			if _, err := PrepareSandboxChart(chrt, values, nil, bundle); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatal("modified Portal sign-in contract was not rejected:", err)
			}
		})
	}
}

func TestSandboxOpenClawPortalSignInChangesWorkloadHash(t *testing.T) {
	bundle, values := openClawPortalSignInInputs(t, true, true, false)
	before, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	object(values["openclaw"])["portalSignIn"] = map[string]any{"enabled": true}
	after, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if before.WorkloadHash == after.WorkloadHash || before.manifestHash == after.manifestHash {
		t.Fatal("authentication changes were not bound into the rendered workload identity")
	}
}

func TestSandboxOpenClawPortalSignInCannotBypassMTLS(t *testing.T) {
	for _, reconciled := range []bool{false, true} {
		t.Run(map[bool]string{false: "single-start", true: "common"}[reconciled], func(t *testing.T) {
			bundle, values := openClawPortalSignInInputs(t, reconciled, false, true)
			object(object(object(values["openclaw"])["config"])["gateway"])["auth"] = map[string]any{"mode": "trusted-proxy"}
			if _, err := PrepareSandboxChart(testChart(t), values, nil, bundle); err == nil ||
				!strings.Contains(err.Error(), "managed ingress mTLS") {
				t.Fatal("plain Sandbox enabled trusted-proxy authentication through native preferences:", err)
			}
		})
	}
	bundle, values := openClawPortalSignInInputs(t, true, true, true)
	object(values["secrets"])["tailscaleAuthKey"] = "tskey-example"
	if _, err := PrepareSandboxChart(testChart(t), values, nil, bundle); err == nil || !strings.Contains(err.Error(), "Tailscale") {
		t.Fatal("managed mTLS accepted alternate Tailscale ingress:", err)
	}
}
