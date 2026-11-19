// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

func (p *PreparedRelease) sandboxOpenClawPortalSignIn() bool {
	input := p.sandboxBundle.Identity()
	return input.AgentType == "openclaw" && input.Reconciled && input.Controls.IngressMTLS != nil &&
		object(object(p.Values["openclaw"])["portalSignIn"])["enabled"] == true
}

func (p *PreparedRelease) validateSandboxOpenClawConfig(config corev1.ConfigMap) error {
	input := p.sandboxBundle.Identity()
	if input.AgentType != "openclaw" {
		return nil
	}
	var native struct {
		Gateway struct {
			Bind           string         `json:"bind"`
			TrustedProxies []string       `json:"trustedProxies"`
			Auth           map[string]any `json:"auth"`
			Remote         map[string]any `json:"remote"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal([]byte(config.Data["openclaw.json"]), &native); err != nil || len(config.BinaryData) != 0 {
		return fmt.Errorf("rendered Sandbox OpenClaw configuration is invalid")
	}
	proxies := input.Controls.TrustedProxyIPs
	if input.Controls.IngressMTLS != nil {
		proxies = []string{"127.0.0.1"}
	}
	if len(proxies) != 0 && !slices.Equal(native.Gateway.TrustedProxies, proxies) {
		return fmt.Errorf("rendered OpenClaw proxy trust differs from the approved native ingress IPs")
	}
	if p.sandboxOpenClawPortalSignIn() {
		auth := map[string]any{
			"mode": "trusted-proxy",
			"trustedProxy": map[string]any{
				"userHeader": "x-auth-request-email", "allowLoopback": true,
				"deviceAutoApprove": map[string]any{"enabled": true},
			},
		}
		_, hasRemoteToken := native.Gateway.Remote["token"]
		if native.Gateway.Bind != "loopback" || !equality.Semantic.DeepEqual(native.Gateway.Auth, auth) || hasRemoteToken {
			return fmt.Errorf("rendered OpenClaw Portal sign-in differs from the managed mTLS authentication contract")
		}
	} else if native.Gateway.Auth["mode"] == "trusted-proxy" {
		return fmt.Errorf("Sandbox OpenClaw trusted-proxy authentication requires Portal sign-in with managed ingress mTLS")
	}
	return nil
}

func (p *PreparedRelease) validateSandboxOpenClawEnvironment(spec corev1.PodSpec) error {
	if p.sandboxBundle.Identity().AgentType != "openclaw" {
		return nil
	}
	if len(spec.Containers) == 0 {
		return fmt.Errorf("Sandbox OpenClaw gateway environment requires the main container")
	}
	name := "OPENCLAW_GATEWAY_TOKEN"
	if p.sandboxOpenClawPortalSignIn() {
		name = "OPENCLAW_GATEWAY_PASSWORD"
	}
	want := corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: text(object(p.Values["secrets"])["existingSecret"])},
			Key:                  "OPENCLAW_GATEWAY_TOKEN",
		},
	}}
	containers := []corev1.Container{spec.Containers[0]}
	for _, container := range spec.InitContainers {
		if container.Name == "doctor" {
			containers = append(containers, container)
		}
	}
	for index, container := range containers {
		found := false
		for _, env := range container.Env {
			if env.Name != "OPENCLAW_GATEWAY_TOKEN" && env.Name != "OPENCLAW_GATEWAY_PASSWORD" {
				continue
			}
			if index != 0 || found || !equality.Semantic.DeepEqual(env, want) {
				return fmt.Errorf("Sandbox OpenClaw gateway environment differs from the selected authentication mode")
			}
			found = true
		}
		if index == 0 && !found {
			return fmt.Errorf("Sandbox OpenClaw gateway environment is missing its authorized credential")
		}
	}
	return nil
}
