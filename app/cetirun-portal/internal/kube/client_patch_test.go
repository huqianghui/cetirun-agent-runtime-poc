// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPortalPatchesPreserveFutureFields(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		team        bool
		addRegistry bool
		registry    bool
		copilot     bool
		strict      bool
		wantSpec    string
		run         func(*Client, client.Object) (client.Object, error)
	}{
		{
			name: "instance-display-name", strict: true, wantSpec: `{"user":{"displayName":"Renamed"}}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				return c.UpdateInstanceByName(ctx, object.GetName(), UpdateInstanceOpts{
					ExpectedUID: object.GetUID(), ExpectedResourceVersion: object.GetResourceVersion(), DisplayName: ptr.To("Renamed"),
				})
			},
		},
		{
			name: "instance-agent-port", strict: true, wantSpec: `{"agent":{"port":9090}}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				agent := object.(*api.Agent).Spec.Agent.DeepCopy()
				agent.Port = ptr.To[int32](9090)
				return c.UpdateInstanceByName(ctx, object.GetName(), UpdateInstanceOpts{Agent: agent})
			},
		},
		{
			name: "instance-agent-image", strict: true, wantSpec: `{"agent":{"image":"registry.example.com/agent:2"}}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				agent := object.(*api.Agent).Spec.Agent.DeepCopy()
				agent.Image = ptr.To("registry.example.com/agent:2")
				return c.UpdateInstanceByName(ctx, object.GetName(), UpdateInstanceOpts{
					Agent: agent, ExpectedUID: object.GetUID(), ExpectedResourceVersion: object.GetResourceVersion(),
				})
			},
		},
		{
			name: "instance-clear-agent-port", strict: true, wantSpec: `{"agent":{"port":null}}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				agent := object.(*api.Agent).Spec.Agent.DeepCopy()
				agent.Port = nil
				return c.UpdateInstanceByName(ctx, object.GetName(), UpdateInstanceOpts{Agent: agent})
			},
		},
		{
			name: "instance-upgrade", strict: true, wantSpec: `{"helm":{"version":"2.0.0"}}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				return c.UpgradeInstanceByName(ctx, object.GetName(), InstanceVersion{UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()})
			},
		},
		{
			name: "legacy-instance-upgrade", wantSpec: `{"helm":{"version":"2.0.0"}}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				return c.UpgradeInstance(ctx, object.GetName())
			},
		},
		{
			name: "team-editable-fields", team: true, strict: true,
			wantSpec: `{"memberContext":{"role":"Reviewer"},"skills":["new-skill"],"skillStorage":{"storageSize":"2Gi"}}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				spec := object.(*api.Team).Spec.DeepCopy()
				spec.MemberContext.Role = "Reviewer"
				spec.Skills, spec.SkillStorage.StorageSize = []string{"new-skill"}, "2Gi"
				return c.UpdateTeamSpec(ctx, object.GetName(), *spec, ConfigurationVersion{UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()})
			},
		},
		{
			name: "team-clear-editable-fields", team: true, strict: true,
			wantSpec: `{"memberContext":{"role":null},"skills":null,"skillStorage":null}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				spec := object.(*api.Team).Spec.DeepCopy()
				spec.MemberContext, spec.Skills, spec.SkillStorage = api.TeamContextSpec{}, nil, nil
				return c.UpdateTeamSpec(ctx, object.GetName(), *spec, ConfigurationVersion{UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()})
			},
		},
		{
			name: "registry-add", addRegistry: true, registry: true,
			wantSpec: `{"agent":{"registrySecretRef":{"name":"registry-source-OPERATION"}}}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				return c.BeginRegistryUpdate(ctx, object.(*api.Agent), false)
			},
		},
		{
			name: "registry-rotate", registry: true,
			run: func(c *Client, object client.Object) (client.Object, error) {
				return c.BeginRegistryUpdate(ctx, object.(*api.Agent), false)
			},
		},
		{
			name: "registry-remove", registry: true, wantSpec: `{"agent":{"registrySecretRef":null}}`,
			run: func(c *Client, object client.Object) (client.Object, error) {
				return c.BeginRegistryUpdate(ctx, object.(*api.Agent), true)
			},
		},
		{
			// The GitHub Copilot sign-in records the account's models.
			name: "copilot-models", copilot: true,
			run: func(c *Client, object client.Object) (client.Object, error) {
				if err := c.SetCopilotModels(ctx, object.GetName(), object.GetUID(), []api.CopilotModel{{ID: "gpt-5.5"}}, "generation-1", nil); err != nil {
					return nil, err
				}
				return c.GetInstanceByName(ctx, object.GetName())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, resource := "Agent", "agents"
			var expected client.Object = &api.Agent{}
			spec := map[string]any{
				"agent": map[string]any{
					"type": "hermes", "image": "registry.example.com/agent:1", "username": "vibe", "port": 18789,
					"registrySecretRef": map[string]any{"name": "registry-source-existing"}, "futureAgent": "keep-agent",
				},
				"user": map[string]any{"alias": "example", "displayName": "Example User", "futureUser": "keep-user"},
				"helm": map[string]any{
					"version": "1.0.0", "futureHelm": "keep-helm", "values": map[string]any{"futureValues": map[string]any{"enabled": true}},
				},
				"gatewayTokenSecretRef": "synthetic-token",
			}
			if tc.addRegistry {
				delete(spec["agent"].(map[string]any), "registrySecretRef")
			}
			if tc.team {
				kind, resource, expected = "Team", "teams", &api.Team{}
				spec = map[string]any{
					"agentPolicyRef": "policy",
					"sandboxRefs":    []string{"example-sandbox"}, "defaultSandboxRef": "example-sandbox",
					"memberContext": map[string]any{"role": "Engineer", "futureContext": "keep-context"},
					"skills":        []string{"old-skill"},
					"skillStorage": map[string]any{
						"resourceGroup": "example-group", "storageAccount": "examplestorage", "containerName": "skills",
						"storageSize": "1Gi", "futureStorage": "keep-storage",
					},
				}
			}
			spec["futureConfiguration"] = map[string]any{"mode": "preserve", "items": []string{"one", "two"}}
			initial := patchTestJSON(t, map[string]any{
				"apiVersion": api.GroupVersion.String(), "kind": kind, "spec": spec,
				"metadata": map[string]any{
					"name": "example", "uid": "synthetic-agent-uid", "resourceVersion": "7",
					"annotations": map[string]string{registryauth.Operation: "previous-operation", "example.com/keep": "unchanged"},
					"finalizers":  []string{"example.com/retain"},
				},
				"status": map[string]any{"futureStatus": map[string]any{"receipt": "preserve"}},
			})
			var mu sync.Mutex
			stored := initial
			var written []byte
			writes := 0
			endpoint := "/apis/" + api.GroupVersion.String() + "/" + resource + "/example"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != endpoint {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if r.Method != http.MethodGet {
					writes++
					if r.Method != http.MethodPatch || r.Header.Get("Content-Type") != "application/merge-patch+json" {
						t.Errorf("want a merge patch, got %s %s", r.Method, r.Header.Get("Content-Type"))
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					if tc.strict && r.URL.Query().Get("fieldValidation") != metav1.FieldValidationStrict {
						t.Error("instance patch lost strict field validation")
					}
					payload, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					written = payload
					stored, err = jsonpatch.MergePatch(stored, payload)
					if err == nil {
						stored, err = jsonpatch.MergePatch(stored, []byte(`{"metadata":{"resourceVersion":"8"}}`))
					}
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				if _, err := w.Write(stored); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{api.GroupVersion})
			mapper.Add(api.GroupVersion.WithKind(kind), meta.RESTScopeRoot)
			transport, err := client.New(&rest.Config{Host: server.URL}, client.Options{Scheme: newScheme(), Mapper: mapper})
			if err != nil {
				t.Fatal(err)
			}
			portal := NewClient(transport)
			portal.chartVersionVal, portal.chartVersionExp = "2.0.0", time.Now().Add(time.Minute)
			if err := transport.Get(ctx, client.ObjectKey{Name: "example"}, expected); err != nil {
				t.Fatal(err)
			}
			before := expected.DeepCopyObject()
			result, err := tc.run(portal, expected)
			if err != nil {
				t.Fatal(err)
			}
			if result.GetUID() != expected.GetUID() || result.GetResourceVersion() != "8" {
				t.Fatalf("write did not return the persisted version: uid=%s resourceVersion=%s", result.GetUID(), result.GetResourceVersion())
			}
			if !reflect.DeepEqual(before, expected) {
				t.Fatal("write mutated the caller's object")
			}
			wantMetadata := map[string]any{"resourceVersion": "7"}
			if tc.registry {
				operation := result.GetAnnotations()[registryauth.Operation]
				if len(operation) != 32 || operation == "previous-operation" {
					t.Fatalf("missing new registry operation: %q", operation)
				}
				wantMetadata["annotations"] = map[string]any{registryauth.Operation: operation}
				tc.wantSpec = strings.ReplaceAll(tc.wantSpec, "OPERATION", operation)
			}
			if tc.copilot {
				annotations := result.GetAnnotations()
				signedIn := annotations[api.AnnotationCopilotSignedInAt]
				if annotations[api.AnnotationCopilotModels] != `[{"id":"gpt-5.5"}]` || signedIn == "" {
					t.Fatalf("missing the Copilot annotations: %v", annotations)
				}
				wantMetadata["annotations"] = map[string]any{api.AnnotationCopilotModels: `[{"id":"gpt-5.5"}]`, api.AnnotationCopilotSignedInAt: signedIn,
					api.AnnotationCopilotSignInGeneration: "generation-1"}
			}
			wantPatch := map[string]any{"metadata": wantMetadata}
			if tc.wantSpec != "" {
				var specPatch map[string]any
				if err := json.Unmarshal([]byte(tc.wantSpec), &specPatch); err != nil {
					t.Fatal(err)
				}
				wantPatch["spec"] = specPatch
			}
			mu.Lock()
			defer mu.Unlock()
			if writes != 1 {
				t.Fatalf("writes=%d, want one patch", writes)
			}
			if !jsonpatch.Equal(written, patchTestJSON(t, wantPatch)) {
				t.Fatalf("patch changed unexpected fields or lost its version lock:\n%s\nwant: %+v", written, wantPatch)
			}
			wantMetadata["resourceVersion"] = "8"
			want, err := jsonpatch.MergePatch(initial, patchTestJSON(t, wantPatch))
			if err != nil {
				t.Fatal(err)
			}
			if !jsonpatch.Equal(stored, want) {
				t.Fatal("write lost stored fields outside the requested changes")
			}
		})
	}
}

func patchTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
