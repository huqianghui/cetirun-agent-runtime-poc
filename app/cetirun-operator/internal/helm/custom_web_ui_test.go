// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chartutil"
)

func TestCustomWebUIRendering(t *testing.T) {
	for _, desktop := range []bool{false, true} {
		input := customInput(8088)
		object(input["agentRequest"])["webUI"] = true
		input["desktop"] = map[string]any{"enabled": desktop}
		prepared, err := PrepareChart(testChart(t), input)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Agent.Port == nil || *prepared.Agent.Port != 8088 || !prepared.Agent.WebUI {
			t.Fatal("browser became chat")
		}
		if err := prepared.ValidateCustom("cetirun-example", "openclaw-example"); err != nil {
			t.Fatal(err)
		}
		ports := prepared.customServicePorts()
		if ports[0].Name != "http" || ports[0].Port != 8088 {
			t.Fatalf("missing browser service: %#v", ports)
		}
		objects := platformRender(t, prepared, chartutil.DefaultCapabilities)
		ingress := array(object(objects["NetworkPolicy"]["spec"])["ingress"])
		if len(ingress) != 1 {
			t.Fatal("Web UI must not inherit internal Chat callers")
		}
		chart := testChart(t)
		for _, file := range chart.Templates {
			file.Data = []byte(strings.ReplaceAll(string(file.Data), "path: {{ .healthPath | quote }}", "path: /wrong"))
		}
		if _, err := PrepareChart(chart, input); err == nil {
			t.Fatal("changed UI probe accepted")
		}
		input["networkPolicy"] = map[string]any{"enabled": false}
		if _, err := PrepareChart(testChart(t), input); err == nil {
			t.Fatal("unprotected browser endpoint accepted")
		}
	}
}

func TestWebUIFlagChangesPreparedContract(t *testing.T) {
	input := customInput(8088)
	chat, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	object(input["agentRequest"])["webUI"] = true
	ui, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	if chat.PlanHash == ui.PlanHash || chat.WorkloadHash == ui.WorkloadHash {
		t.Fatal("protocol change must invalidate both the plan and workload")
	}
	object(input["agentRequest"])["port"] = nil
	if _, err := PrepareChart(testChart(t), input); err == nil {
		t.Fatal("Web UI without port was prepared")
	}
	object(input["agentRequest"])["webUI"] = map[string]any{"port": 8088}
	if _, err := PrepareChart(testChart(t), input); err == nil {
		t.Fatal("old Web UI object was prepared")
	}
}
