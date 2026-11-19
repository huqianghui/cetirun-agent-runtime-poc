// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
)

func TestSandboxCreatePinsRuntimeBeforeFirstWrite(t *testing.T) {
	team, policy := creationPolicy()
	policy.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeOpenClaw}
	h, _ := creationPolicyHandler(team, policy)
	w := httptest.NewRecorder()
	h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"openclaw"},"team":"policy-team","modelAccess":{"sharedModels":true,"githubCopilot":false}}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	a, err := h.Kube.GetInstance(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	profile := h.Config.SandboxRuntimes["openclaw"]
	if a.Spec.Helm.Version != profile.ChartVersion || a.Spec.Agent.Image == nil || *a.Spec.Agent.Image != profile.Image ||
		a.Spec.Agent.Port == nil || *a.Spec.Agent.Port != profile.Port || a.Spec.SandboxRef != team.Spec.DefaultSandboxRef {
		t.Fatalf("creation did not persist the approved inputs: %+v", a.Spec)
	}
}

func TestSandboxCreatePreservesCustomContracts(t *testing.T) {
	for _, endpoint := range []string{"", `,"port":8080`, `,"port":8080,"webUI":true`} {
		team, policy := creationPolicy()
		policy.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeCustom}
		h, _ := creationPolicyHandler(team, policy)
		h.Config.SandboxRuntimes = nil
		h.Config.SandboxControllerDefaults = true
		w := httptest.NewRecorder()
		h.CreateInstance(w, managementRequest(http.MethodPost,
			`{"agent":{"type":"custom","image":"registry.example.com/custom@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`+
				endpoint+`},"team":"policy-team","modelAccess":{"sharedModels":true,"githubCopilot":false}}`))
		if w.Code != http.StatusCreated {
			t.Fatalf("Custom create: %d %s", w.Code, w.Body.String())
		}
		agent, err := h.Kube.GetInstance(context.Background(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		if agent.Spec.Agent.Type != api.AgentTypeCustom || agent.Spec.Agent.Image == nil ||
			agent.Spec.SandboxRef != team.Spec.DefaultSandboxRef || agent.Spec.GatewayTokenSecretRef != "" ||
			(endpoint == "") != (agent.Spec.Agent.Port == nil) {
			t.Fatal("Portal changed the Custom contract or lost policy selection")
		}
	}
}

func TestSandboxCreateUsesCommonControllerDefaults(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		t.Run(string(kind), func(t *testing.T) {
			team, policy := creationPolicy()
			policy.Spec.Harnesses.Allowed = []api.AgentType{kind}
			h, _ := creationPolicyHandler(team, policy)
			h.Config.SandboxRuntimes = nil
			h.Config.SandboxControllerDefaults = true
			w := httptest.NewRecorder()
			h.CreateInstance(w, managementRequest(http.MethodPost,
				`{"agent":{"type":"`+string(kind)+`"},"team":"policy-team","modelAccess":{"sharedModels":true,"githubCopilot":false}}`))
			if w.Code != http.StatusCreated {
				t.Fatalf("create without a redundant profile: %d %s", w.Code, w.Body.String())
			}
			agent, err := h.Kube.GetInstance(context.Background(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if agent.Spec.Helm.Version != "" || agent.Spec.Agent.Image != nil || agent.Spec.Agent.Port != nil ||
				agent.Spec.SandboxRef != team.Spec.DefaultSandboxRef {
				t.Fatal("Portal did not preserve Sandbox selection while leaving trusted defaults to the common controller")
			}
		})
	}
}

func TestSandboxCreateRequiresValidRuntimeWithoutWrites(t *testing.T) {
	for _, profile := range []config.SandboxRuntime{
		{},
		{ChartVersion: "latest", Image: "registry.example.com/hermes:latest", Port: 8642},
		{ChartVersion: "1.2.3", Image: "registry.example.com/hermes@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Port: 18789},
	} {
		for _, controllerDefaults := range []bool{false, true} {
			team, policy := creationPolicy()
			h, fc := creationPolicyHandler(team, policy)
			h.Config.SandboxRuntimes["hermes"] = profile
			h.Config.SandboxControllerDefaults = controllerDefaults
			w := httptest.NewRecorder()
			h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"hermes"},"team":"policy-team","gatewayToken":"synthetic-token","modelAccess":{"sharedModels":true,"githubCopilot":false}}`))
			if w.Code != http.StatusServiceUnavailable || errorCode(t, w) != "runtime_unavailable" {
				t.Fatalf("invalid runtime (controller defaults=%t): %d %s", controllerDefaults, w.Code, w.Body.String())
			}
			assertNoCreationWrites(t, fc)
		}
	}
}

func TestSandboxCreateWithoutProfileRequiresControllerDefaults(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		for _, profiles := range []map[string]config.SandboxRuntime{nil, {}} {
			t.Run(string(kind), func(t *testing.T) {
				team, policy := creationPolicy()
				policy.Spec.Harnesses.Allowed = []api.AgentType{kind}
				h, fc := creationPolicyHandler(team, policy)
				h.Config.SandboxRuntimes = profiles
				w := httptest.NewRecorder()
				h.CreateInstance(w, managementRequest(http.MethodPost,
					`{"agent":{"type":"`+string(kind)+`"},"team":"policy-team","modelAccess":{"sharedModels":true,"githubCopilot":false}}`))
				if w.Code != http.StatusServiceUnavailable || errorCode(t, w) != "runtime_unavailable" {
					t.Fatalf("missing runtime without controller defaults: %d %s", w.Code, w.Body.String())
				}
				assertNoCreationWrites(t, fc)
			})
		}
	}
}

func TestSandboxCreatePreservesExplicitRuntimeRequests(t *testing.T) {
	for _, controllerDefaults := range []bool{false, true} {
		team, policy := creationPolicy()
		h, _ := creationPolicyHandler(team, policy)
		h.Config.SandboxControllerDefaults = controllerDefaults
		if controllerDefaults {
			h.Config.SandboxRuntimes = nil
		}
		w := httptest.NewRecorder()
		// Admission, not Portal defaulting, decides whether the explicit image
		// and port are qualified by the selected runtime chart.
		h.CreateInstance(w, managementRequest(http.MethodPost, `{"agent":{"type":"hermes","image":"registry.example.com/request:1","port":9000},"team":"policy-team","modelAccess":{"sharedModels":true,"githubCopilot":false}}`))
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		a, err := h.Kube.GetInstance(context.Background(), "alice")
		if err != nil || a.Spec.Agent.Image == nil || *a.Spec.Agent.Image != "registry.example.com/request:1" || a.Spec.Agent.Port == nil || *a.Spec.Agent.Port != 9000 {
			t.Fatalf("explicit request was overwritten: %v", err)
		}
	}
}
