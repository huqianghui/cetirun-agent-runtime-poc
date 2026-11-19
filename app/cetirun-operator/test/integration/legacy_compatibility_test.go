// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package integration_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRemovedDirectProviderFieldsAreRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	teams := coreClient.Resource(schema.GroupVersionResource{Group: "cetirun.microsoft.com", Version: "v1alpha1", Resource: "teams"})
	team := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cetirun.microsoft.com/v1alpha1", "kind": "Team",
		"metadata": map[string]any{"name": "gateway-schema-team"},
		"spec": map[string]any{
			"memberContext": map[string]any{"role": "Engineer"},
		},
	}}
	storedTeam, err := teams.Create(ctx, team, metav1.CreateOptions{FieldValidation: "Strict"})
	if err != nil {
		t.Fatal("current Team schema rejected:", err)
	}
	if err := unstructured.SetNestedField(storedTeam.Object, "Reviewer", "spec", "memberContext", "role"); err != nil {
		t.Fatal(err)
	}
	if _, err := teams.Update(ctx, storedTeam, metav1.UpdateOptions{FieldValidation: "Strict"}); err != nil {
		t.Fatal("current Team update rejected:", err)
	}
	removed := team.DeepCopy()
	removed.SetName("removed-provider-team")
	if err := unstructured.SetNestedField(removed.Object, map[string]any{"provider": "openai"}, "spec", "defaults"); err != nil {
		t.Fatal(err)
	}
	if _, err := teams.Create(ctx, removed, metav1.CreateOptions{FieldValidation: "Strict"}); err == nil {
		t.Fatal("removed Team defaults.provider was accepted")
	}

	for _, typ := range []string{"openclaw", "hermes", "custom"} {
		t.Run(typ, func(t *testing.T) {
			obj := instance(typ)
			if _, err := instances.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"}); err != nil {
				t.Fatal("current Agent schema rejected:", err)
			}
			removedProvider := instance(typ)
			removedProvider.SetName("removed-provider-" + typ)
			if err := unstructured.SetNestedField(removedProvider.Object, "openai", "spec", "provider"); err != nil {
				t.Fatal(err)
			}
			if _, err := instances.Create(ctx, removedProvider, metav1.CreateOptions{FieldValidation: "Strict"}); err == nil {
				t.Fatal("removed Agent provider field was accepted")
			}
		})
	}
}
