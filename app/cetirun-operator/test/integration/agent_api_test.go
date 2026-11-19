// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

var (
	instances  dynamic.ResourceInterface
	coreClient dynamic.Interface
	nextID     atomic.Int64
)

func TestMain(m *testing.M) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			fmt.Fprintln(os.Stderr, "envtest requires preinstalled binaries in KUBEBUILDER_ASSETS")
			os.Exit(1)
		}
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory:    assets,
		UseExistingCluster:       ptr.To(false),
		CRDDirectoryPaths:        []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing:    true,
		ControlPlaneStartTimeout: 30 * time.Second,
		ControlPlaneStopTimeout:  30 * time.Second,
	}
	cfg, err := env.Start()
	if err != nil {
		_ = env.Stop()
		fmt.Fprintln(os.Stderr, "start isolated API server:", err)
		os.Exit(1)
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		_ = env.Stop()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	instances = client.Resource(schema.GroupVersionResource{
		Group: "cetirun.microsoft.com", Version: "v1alpha1", Resource: "agents",
	})
	coreClient = client
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stop isolated API server:", err)
		code = 1
	}
	os.Exit(code)
}

func instance(agentType string) *unstructured.Unstructured {
	spec := map[string]any{
		"user":       map[string]any{"alias": "alice", "displayName": "Example User"},
		"agent":      map[string]any{"type": agentType},
		"team":       "example-team",
		"sandboxRef": "standard",
	}
	if agentType == "custom" {
		spec["agent"].(map[string]any)["image"] = "registry.example.com/team/agent:1"
	} else {
		spec["gatewayTokenSecretRef"] = "gateway-token"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cetirun.microsoft.com/v1alpha1", "kind": "Agent",
		"metadata": map[string]any{"name": fmt.Sprintf("agent-%d", nextID.Add(1))},
		"spec":     spec,
	}}
}

func agent(obj *unstructured.Unstructured) map[string]any {
	return obj.Object["spec"].(map[string]any)["agent"].(map[string]any)
}

func create(t *testing.T, obj *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := instances.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := instances.Delete(ctx, got.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Error(err)
		}
	})
	return got
}

func TestAgentDefaults(t *testing.T) {
	t.Run("Custom Web UI uses the declared port", func(t *testing.T) {
		obj := instance("custom")
		agent(obj)["webUI"] = true
		agent(obj)["port"] = int64(8080)
		got := create(t, obj)
		if agent(got)["webUI"] != true || agent(got)["port"] != int64(8080) {
			t.Fatal("Web UI must keep its flag and shared port")
		}
	})
	for _, typ := range []string{"openclaw", "hermes", "custom"} {
		t.Run(typ, func(t *testing.T) {
			got := create(t, instance(typ))
			if agent(got)["username"] != "vibe" || agent(got)["type"] != typ {
				t.Fatalf("unexpected defaults: %v", agent(got))
			}
			if agent(got)["webUI"] != false {
				t.Fatal("Web UI must default to false")
			}
			if _, found := agent(got)["port"]; found {
				t.Fatal("schema must not set a version-dependent port")
			}
			if _, found := agent(got)["image"]; found && typ != "custom" {
				t.Fatal("schema must not select built-in images")
			}
		})
	}
	t.Run("null username defaults like omission", func(t *testing.T) {
		obj := instance("custom")
		agent(obj)["username"] = nil
		if got := create(t, obj); agent(got)["username"] != "vibe" {
			t.Fatal("non-nullable username must be defaulted")
		}
	})
	t.Run("null Web UI defaults to false", func(t *testing.T) {
		obj := instance("custom")
		agent(obj)["webUI"] = nil
		if got := create(t, obj); agent(got)["webUI"] != false {
			t.Fatal("null Web UI must reset to false")
		}
	})
	for _, null := range []bool{false, true} {
		t.Run(fmt.Sprintf("default Hermes null=%v", null), func(t *testing.T) {
			obj := instance("hermes")
			if null {
				agent(obj)["type"] = nil
			} else {
				delete(agent(obj), "type")
			}
			got := create(t, obj)
			if agent(got)["type"] != "hermes" || agent(got)["username"] != "vibe" {
				t.Fatalf("unexpected defaults: %v", agent(got))
			}
		})
	}
}

func TestAgentExamples(t *testing.T) {
	for _, file := range []string{
		"../config/sample-alice.yaml", "../config/sample-bob.yaml", "../config/sample-custom.yaml",
		"../../../../examples/vscode-server/agent.yaml",
	} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := yaml.UnmarshalStrict(raw, &object); err != nil {
				t.Fatal(err)
			}
			create(t, &unstructured.Unstructured{Object: object})
		})
	}
}

func TestWebsiteAgentExamples(t *testing.T) {
	for _, path := range []string{
		"getting-started/first-instance.md",
	} {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../../../../site/content/en/docs", path))
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, section := range strings.Split(string(raw), "```yaml\n")[1:] {
				block, _, _ := strings.Cut(section, "```")
				if !strings.Contains(block, "kind: Agent\n") {
					continue
				}
				var object map[string]any
				if err := yaml.UnmarshalStrict([]byte(block), &object); err != nil {
					t.Fatal(err)
				}
				create(t, &unstructured.Unstructured{Object: object})
				count++
			}
			if count == 0 {
				t.Fatal("no Agent examples found")
			}
		})
	}
}

func TestAgentCreateValidation(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*unstructured.Unstructured)
		field string
	}{
		{"missing spec", func(o *unstructured.Unstructured) { delete(o.Object, "spec") }, "spec"},
		{"missing agent", func(o *unstructured.Unstructured) { delete(o.Object["spec"].(map[string]any), "agent") }, "agent"},
		{"empty type", func(o *unstructured.Unstructured) { agent(o)["type"] = "" }, "type"},
		{"unknown type", func(o *unstructured.Unstructured) { agent(o)["type"] = "unknown" }, "type"},
		{"custom without image", func(o *unstructured.Unstructured) { delete(agent(o), "image") }, "image"},
		{"custom null image", func(o *unstructured.Unstructured) { agent(o)["image"] = nil }, "image"},
		{"empty image", func(o *unstructured.Unstructured) { agent(o)["image"] = "" }, "image"},
		{"long image", func(o *unstructured.Unstructured) { agent(o)["image"] = strings.Repeat("x", 2049) }, "image"},
		{"empty username", func(o *unstructured.Unstructured) { agent(o)["username"] = "" }, "username"},
		{"root", func(o *unstructured.Unstructured) { agent(o)["username"] = "root" }, "username"},
		{"path traversal", func(o *unstructured.Unstructured) { agent(o)["username"] = "../other" }, "username"},
		{"uppercase username", func(o *unstructured.Unstructured) { agent(o)["username"] = "Alice" }, "username"},
		{"long username", func(o *unstructured.Unstructured) { agent(o)["username"] = strings.Repeat("a", 33) }, "username"},
		{"zero port", func(o *unstructured.Unstructured) { agent(o)["port"] = int64(0) }, "port"},
		{"large port", func(o *unstructured.Unstructured) { agent(o)["port"] = int64(65536) }, "port"},
		{"string port", func(o *unstructured.Unstructured) { agent(o)["port"] = "8080" }, "port"},
		{"web UI missing port", func(o *unstructured.Unstructured) { agent(o)["webUI"] = true }, "port"},
		{"web UI zero port", func(o *unstructured.Unstructured) {
			agent(o)["webUI"] = true
			agent(o)["port"] = int64(0)
		}, "port"},
		{"web UI string", func(o *unstructured.Unstructured) { agent(o)["webUI"] = "true" }, "webUI"},
		{"web UI on built-in", func(o *unstructured.Unstructured) {
			agent(o)["type"] = "hermes"
			agent(o)["webUI"] = true
			agent(o)["port"] = int64(8080)
			o.Object["spec"].(map[string]any)["gatewayTokenSecretRef"] = "gateway-token"
		}, "webUI"},
		{"missing ref name", func(o *unstructured.Unstructured) { agent(o)["registrySecretRef"] = map[string]any{} }, "name"},
		{"empty ref name", func(o *unstructured.Unstructured) { agent(o)["registrySecretRef"] = map[string]any{"name": ""} }, "name"},
		{"invalid ref name", func(o *unstructured.Unstructured) {
			agent(o)["registrySecretRef"] = map[string]any{"name": "Other/secret"}
		}, "name"},
		{"empty DNS label", func(o *unstructured.Unstructured) { agent(o)["registrySecretRef"] = map[string]any{"name": "a..b"} }, "name"},
		{"long ref name", func(o *unstructured.Unstructured) {
			agent(o)["registrySecretRef"] = map[string]any{"name": strings.Repeat("a", 254)}
		}, "name"},
		{"custom with mixed secret", func(o *unstructured.Unstructured) {
			o.Object["spec"].(map[string]any)["gatewayTokenSecretRef"] = "gateway-token"
		}, "gatewayTokenSecretRef"},
		{"built-in without secret", func(o *unstructured.Unstructured) {
			agent(o)["type"] = "hermes"
		}, "gatewayTokenSecretRef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := instance("custom")
			tt.edit(obj)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := instances.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"})
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("want Invalid on %s, got %v", tt.field, err)
			}
		})
	}
}

func TestAgentStrictFields(t *testing.T) {
	for _, path := range [][]string{
		{"spec", "agentType"},
		{"spec", "agent", "uid"},
		{"spec", "agent", "env"},
		{"spec", "agent", "model"},
		{"spec", "agent", "webUI", "port"},
		{"spec", "agent", "registryAuth"},
		{"spec", "registryAuth"},
		{"spec", "agent", "registrySecretRef", "namespace"},
		{"spec", "agent", "registrySecretRef", "key"},
		{"spec", "agent", "registrySecretRef", "password"},
	} {
		t.Run(strings.Join(path, "."), func(t *testing.T) {
			obj := instance("custom")
			agent(obj)["registrySecretRef"] = map[string]any{"name": "registry-agent"}
			if err := unstructured.SetNestedField(obj.Object, "not-allowed", path...); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := instances.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"})
			if !apierrors.IsBadRequest(err) || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("want strict unknown-field error, got %v", err)
			}
		})
	}
	t.Run("legacy only without strict validation", func(t *testing.T) {
		obj := instance("openclaw")
		spec := obj.Object["spec"].(map[string]any)
		delete(spec, "agent")
		spec["agentType"] = "openclaw"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := instances.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Ignore"})
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "agent") {
			t.Fatalf("legacy-only request must fail without agent, got %v", err)
		}
	})
}

func TestAgentTransitions(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*unstructured.Unstructured)
		edit    func(*unstructured.Unstructured)
		valid   bool
	}{
		{"type immutable", nil, func(o *unstructured.Unstructured) { agent(o)["type"] = "hermes" }, false},
		{"omit nondefault type", nil, func(o *unstructured.Unstructured) { delete(agent(o), "type") }, false},
		{"omit default type", func(o *unstructured.Unstructured) { agent(o)["type"] = "hermes" },
			func(o *unstructured.Unstructured) { delete(agent(o), "type") }, true},
		{"default username immutable", nil, func(o *unstructured.Unstructured) { agent(o)["username"] = "alice" }, false},
		{"omit default username", nil, func(o *unstructured.Unstructured) { delete(agent(o), "username") }, true},
		{"omit nondefault username", func(o *unstructured.Unstructured) { agent(o)["username"] = "alice" },
			func(o *unstructured.Unstructured) { delete(agent(o), "username") }, false},
		{"add pinned image", nil, func(o *unstructured.Unstructured) { agent(o)["image"] = "registry.example.com/agent:2" }, true},
		{"update image", func(o *unstructured.Unstructured) { agent(o)["image"] = "registry.example.com/agent:1" },
			func(o *unstructured.Unstructured) { agent(o)["image"] = "registry.example.com/agent:2" }, true},
		{"remove pinned image", func(o *unstructured.Unstructured) { agent(o)["image"] = "registry.example.com/agent:1" },
			func(o *unstructured.Unstructured) { delete(agent(o), "image") }, false},
		{"null pinned image", func(o *unstructured.Unstructured) { agent(o)["image"] = "registry.example.com/agent:1" },
			func(o *unstructured.Unstructured) { agent(o)["image"] = nil }, false},
		{"update port", nil, func(o *unstructured.Unstructured) { agent(o)["port"] = int64(65535) }, true},
		{"remove port", func(o *unstructured.Unstructured) { agent(o)["port"] = int64(1) },
			func(o *unstructured.Unstructured) { delete(agent(o), "port") }, true},
		{"add registry reference", nil, func(o *unstructured.Unstructured) {
			agent(o)["registrySecretRef"] = map[string]any{"name": "registry-agent"}
		}, true},
		{"remove registry reference", func(o *unstructured.Unstructured) {
			agent(o)["registrySecretRef"] = map[string]any{"name": "registry-agent"}
		},
			func(o *unstructured.Unstructured) { delete(agent(o), "registrySecretRef") }, true},
		{"model stays independent", nil, func(o *unstructured.Unstructured) { o.Object["spec"].(map[string]any)["model"] = "another-model" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := instance("openclaw")
			if tt.prepare != nil {
				tt.prepare(obj)
			}
			obj = create(t, obj)
			tt.edit(obj)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := instances.Update(ctx, obj, metav1.UpdateOptions{FieldValidation: "Strict"})
			if tt.valid && err != nil {
				t.Fatal(err)
			}
			if !tt.valid && !apierrors.IsInvalid(err) {
				t.Fatalf("want Invalid transition, got %v", err)
			}
		})
	}
	t.Run("custom can become headless", func(t *testing.T) {
		obj := instance("custom")
		agent(obj)["port"] = int64(8080)
		obj = create(t, obj)
		delete(agent(obj), "port")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := instances.Update(ctx, obj, metav1.UpdateOptions{FieldValidation: "Strict"}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCustomWebUITransitions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(map[string]any)
		valid bool
		webUI bool
	}{
		{"port change", func(a map[string]any) { a["port"] = int64(9090) }, true, true},
		{"port required while enabled", func(a map[string]any) { delete(a, "port") }, false, true},
		{"disable keeps HTTP port", func(a map[string]any) { a["webUI"] = false }, true, false},
		{"reset flag", func(a map[string]any) { a["webUI"] = nil }, true, false},
		{"headless", func(a map[string]any) { a["webUI"] = false; delete(a, "port") }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := instance("custom")
			agent(obj)["port"] = int64(8080)
			agent(obj)["webUI"] = true
			obj = create(t, obj)
			tc.edit(agent(obj))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, err := instances.Update(ctx, obj, metav1.UpdateOptions{FieldValidation: "Strict"})
			if tc.valid {
				if err != nil || agent(got)["webUI"] != tc.webUI {
					t.Fatalf("Web UI transition failed: %v", err)
				}
			} else if !apierrors.IsInvalid(err) {
				t.Fatalf("want Invalid transition, got %v", err)
			}
		})
	}
}
