// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAgentSourceReferencePinsIntentNotStatus(t *testing.T) {
	fixture := testfixture.New("openclaw")
	expected, err := w.AgentSourceReference(fixture.Agent)
	if err != nil || !expected.ValidIdentity() || expected.ResourceVersion != "" {
		t.Fatalf("invalid intent identity: %+v %v", expected, err)
	}
	legacy, err := json.Marshal(struct {
		Spec        api.AgentSpec
		Annotations map[string]string
		Held        bool
		Prepared    bool
	}{
		Spec: fixture.Agent.Spec,
		Annotations: map[string]string{
			sandboxbinding.AuthorizationAnnotation: fixture.Agent.Annotations[sandboxbinding.AuthorizationAnnotation],
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(legacy)
	if expected.IntentDigest != hex.EncodeToString(sum[:]) {
		t.Fatal("Agents without bootstrap no longer match their persisted intent digest")
	}
	for name, mutate := range map[string]func(*api.Agent){
		"resource version": func(a *api.Agent) { a.ResourceVersion = "42" },
		"status":           func(a *api.Agent) { a.Status.Phase = api.PhaseFailed },
		"label":            func(a *api.Agent) { a.Labels = map[string]string{"example.com/note": "updated"} },
		"annotation":       func(a *api.Agent) { a.Annotations["example.com/note"] = "updated" },
		"other finalizer":  func(a *api.Agent) { a.Finalizers = append(a.Finalizers, "example.com/retain") },
	} {
		t.Run(name, func(t *testing.T) {
			agent := fixture.Agent.DeepCopy()
			mutate(agent)
			got, err := w.AgentSourceReference(agent)
			if err != nil || got != expected {
				t.Fatalf("unrelated metadata or status invalidated intent: %+v %v", got, err)
			}
		})
	}
	for name, mutate := range map[string]func(*api.Agent){
		"UID":                     func(a *api.Agent) { a.UID = "replacement-agent" },
		"generation":              func(a *api.Agent) { a.Generation++ },
		"spec without generation": func(a *api.Agent) { a.Spec.Helm.Version = "changed" },
		"suspension":              func(a *api.Agent) { a.Spec.Suspended = true },
		"hold":                    func(a *api.Agent) { a.Finalizers = append(a.Finalizers, sandbox.HoldFinalizer) },
		"preparation marker":      func(a *api.Agent) { a.Finalizers = append(a.Finalizers, sandbox.PreparationFinalizer) },
		"bootstrap marker":        func(a *api.Agent) { a.Finalizers = append(a.Finalizers, sandbox.BootstrapFinalizer) },
		"reauthorization":         func(a *api.Agent) { a.Annotations[sandboxbinding.ReauthorizeAnnotation] = "different-request" },
	} {
		t.Run(name, func(t *testing.T) {
			agent := fixture.Agent.DeepCopy()
			mutate(agent)
			got, err := w.AgentSourceReference(agent)
			if err != nil || got == expected {
				t.Fatalf("changed intent was missed: %+v %v", got, err)
			}
		})
	}
	for name, mutate := range map[string]func(*api.Agent){
		"removed authorization": func(a *api.Agent) { delete(a.Annotations, sandboxbinding.AuthorizationAnnotation) },
		"unpersisted":           func(a *api.Agent) { a.ResourceVersion = "" },
		"missing generation":    func(a *api.Agent) { a.Generation = 0 },
		"terminating": func(a *api.Agent) {
			now := metav1.Now()
			a.DeletionTimestamp = &now
		},
	} {
		t.Run(name, func(t *testing.T) {
			agent := fixture.Agent.DeepCopy()
			mutate(agent)
			if _, err := w.AgentSourceReference(agent); err == nil {
				t.Fatal("invalid Agent source was accepted")
			}
		})
	}
}

func TestSourceReferenceIdentityFormsAreUnambiguous(t *testing.T) {
	agent, err := w.AgentSourceReference(testfixture.New("hermes").Agent)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := w.SourceReference{APIVersion: "v1", Kind: "Secret", Name: "example", Namespace: "example", UID: "secret-uid", ResourceVersion: "1"}
	if !ordinary.ValidIdentity() || !agent.ValidIdentity() {
		t.Fatal("valid source rejected")
	}
	for name, mutate := range map[string]func(*w.SourceReference){
		"legacy RV only":   func(s *w.SourceReference) { s.ResourceVersion, s.Generation, s.IntentDigest = "1", 0, "" },
		"mixed versions":   func(s *w.SourceReference) { s.ResourceVersion = "1" },
		"empty digest":     func(s *w.SourceReference) { s.IntentDigest = "" },
		"short digest":     func(s *w.SourceReference) { s.IntentDigest = "abcd" },
		"uppercase digest": func(s *w.SourceReference) { s.IntentDigest = strings.ToUpper(s.IntentDigest) },
		"no generation":    func(s *w.SourceReference) { s.Generation = 0 },
		"no UID":           func(s *w.SourceReference) { s.UID = "" },
		"wrong kind":       func(s *w.SourceReference) { s.Kind = "Team" },
	} {
		t.Run(name, func(t *testing.T) {
			source := agent
			mutate(&source)
			if source.ValidIdentity() {
				t.Fatal("ambiguous or incomplete source accepted")
			}
		})
	}
	ordinary.IntentDigest = agent.IntentDigest
	if ordinary.ValidIdentity() {
		t.Fatal("ordinary source can bypass exact resourceVersion pinning")
	}
}
