// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAlphaResourcePathsAndEventSelector(t *testing.T) {
	apiVersion := "cetirun.microsoft.com/v1alpha1"
	agent := cetirunv1alpha1.Agent{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiVersion, Kind: "Agent"},
		ObjectMeta: metav1.ObjectMeta{Name: "example-agent"},
	}
	team := cetirunv1alpha1.Team{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiVersion, Kind: "Team"},
		ObjectMeta: metav1.ObjectMeta{Name: "example-team"},
	}
	responses := map[string]runtime.Object{
		"/apis/cetirun.microsoft.com/v1alpha1/agents/example-agent": &agent,
		"/apis/cetirun.microsoft.com/v1alpha1/agents": &cetirunv1alpha1.AgentList{
			TypeMeta: metav1.TypeMeta{APIVersion: apiVersion, Kind: "AgentList"},
			Items:    []cetirunv1alpha1.Agent{agent},
		},
		"/apis/cetirun.microsoft.com/v1alpha1/teams/example-team": &team,
		"/apis/cetirun.microsoft.com/v1alpha1/teams": &cetirunv1alpha1.TeamList{
			TypeMeta: metav1.TypeMeta{APIVersion: apiVersion, Kind: "TeamList"},
			Items:    []cetirunv1alpha1.Team{team},
		},
		"/api/v1/events": &corev1.EventList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"},
			Items:    []corev1.Event{{Message: "Agent ready"}},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, ok := responses[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			t.Errorf("unexpected Kubernetes request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/api/v1/events" {
			selector, err := fields.ParseSelector(r.URL.Query().Get("fieldSelector"))
			if err != nil {
				t.Errorf("invalid Event selector: %v", err)
			} else {
				for key, want := range map[string]string{
					"involvedObject.name": "example-agent",
					"involvedObject.kind": "Agent",
				} {
					if got, found := selector.RequiresExactMatch(key); !found || got != want {
						t.Errorf("Event selector %s = %q, found=%v; want %q", key, got, found, want)
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode test response: %v", err)
		}
	}))
	defer server.Close()

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{cetirunv1alpha1.GroupVersion, corev1.SchemeGroupVersion})
	mapper.Add(cetirunv1alpha1.GroupVersion.WithKind("Agent"), meta.RESTScopeRoot)
	mapper.Add(cetirunv1alpha1.GroupVersion.WithKind("Team"), meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Event"), meta.RESTScopeNamespace)
	kubeClient, err := client.New(&rest.Config{Host: server.URL}, client.Options{
		Scheme: newScheme(), Mapper: mapper,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(kubeClient)
	ctx := context.Background()
	if got, err := c.GetInstance(ctx, agent.Name); err != nil || got.Name != agent.Name {
		t.Fatalf("GetInstance = %+v, err=%v", got, err)
	}
	if got, err := c.ListAllInstances(ctx); err != nil || len(got) != 1 || got[0].Name != agent.Name {
		t.Fatalf("ListAllInstances = %+v, err=%v", got, err)
	}
	if got, err := c.GetTeam(ctx, team.Name); err != nil || got.Name != team.Name {
		t.Fatalf("GetTeam = %+v, err=%v", got, err)
	}
	if got, err := c.ListTeams(ctx); err != nil || len(got) != 1 || got[0].Name != team.Name {
		t.Fatalf("ListTeams = %+v, err=%v", got, err)
	}
	if got, err := c.GetEvents(ctx, agent.Name); err != nil || len(got) != 1 {
		t.Fatalf("GetEvents = %+v, err=%v", got, err)
	}
}
