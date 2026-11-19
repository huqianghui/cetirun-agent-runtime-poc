// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

func TestCustomSharedPlatformObjectsMatchAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	namespace := "custom-platform-validation"
	namespaces := coreClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	if _, err := namespaces.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer namespaces.Delete(ctx, namespace, metav1.DeleteOptions{})

	ref := func(name string) runtimebindings.ObjectReference {
		return runtimebindings.ObjectReference{Name: name, UID: name + "-uid", Revision: "1"}
	}
	image := "registry.example.com/agent:1"
	bindings := runtimebindings.Custom{
		AgentName: "example", AgentUID: "example-uid", Generation: 1, Namespace: ref(namespace),
		ApprovedType: "custom", ApprovedImage: image, PolicyRevision: "1",
		Home: runtimebindings.Home{Name: "data-custom-platform-example-0", FSGroup: 2000,
			Claim: &runtimebindings.HomeClaim{StorageClass: "default", StorageSize: "100Gi", AccessMode: "ReadWriteOnce"}},
		Model: runtimebindings.ModelAccess{ObjectReference: ref("model"), Key: "key", BaseURL: "http://gateway.models.svc/v1"},
		Context: runtimebindings.Context{ObjectReference: ref("context"), Files: map[string]string{
			"canonical": "AGENTS.md", "memory": "skills/cetirun-memory/SKILL.md",
		}},
		Network: runtimebindings.Network{Revision: "1", GatewayPort: 8080,
			Gateway: runtimebindings.Peer{Namespace: "models", PodLabels: map[string]string{"app": "gateway"}},
			DNS:     runtimebindings.Peer{Namespace: "kube-system", PodLabels: map[string]string{"k8s-app": "kube-dns"}},
		},
	}
	ch, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := helm.PrepareChart(ch, map[string]any{
		"agentType": "custom", "customBindings": bindings, "fullnameOverride": "custom-platform-example",
		"agentRequest":        map[string]any{"type": "custom", "image": image, "username": "worker", "releaseName": "example"},
		"ttyd":                map[string]any{"enabled": false},
		"podDisruptionBudget": map[string]any{"enabled": true},
		"autoscaling":         map[string]any{"enabled": true, "minReplicas": 1, "maxReplicas": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	capabilities := chartutil.DefaultCapabilities.Copy()
	capabilities.KubeVersion = chartutil.KubeVersion{Version: "v1.37.0", Major: "1", Minor: "37"}
	values, err := chartutil.ToRenderValues(ch, prepared.Values, chartutil.ReleaseOptions{
		Name: "example", Namespace: namespace, IsInstall: true, Revision: 1,
	}, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(ch, values)
	if err != nil {
		t.Fatal(err)
	}
	resources := map[string]string{
		"StatefulSet": "statefulsets", "Service": "services", "ServiceAccount": "serviceaccounts",
		"ConfigMap": "configmaps", "NetworkPolicy": "networkpolicies", "PodDisruptionBudget": "poddisruptionbudgets",
		"HorizontalPodAutoscaler":   "horizontalpodautoscalers",
		"ValidatingAdmissionPolicy": "validatingadmissionpolicies", "ValidatingAdmissionPolicyBinding": "validatingadmissionpolicybindings",
	}
	count := 0
	for filename, content := range rendered {
		if strings.HasSuffix(filename, "NOTES.txt") {
			continue
		}
		for _, document := range strings.Split(content, "\n---") {
			var obj map[string]any
			if err := yaml.Unmarshal([]byte(document), &obj); err != nil {
				t.Fatal(err)
			}
			if len(obj) == 0 {
				continue
			}
			item := &unstructured.Unstructured{Object: obj}
			kind := item.GetKind()
			resourceName, ok := resources[kind]
			if !ok {
				t.Fatalf("unexpected rendered kind %s", kind)
			}
			resource := coreClient.Resource(item.GroupVersionKind().GroupVersion().WithResource(resourceName))
			var target dynamic.ResourceInterface = resource
			if kind != "ValidatingAdmissionPolicy" && kind != "ValidatingAdmissionPolicyBinding" {
				target = resource.Namespace(namespace)
			}
			if _, err := target.Create(ctx, item, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
				t.Fatalf("%s rejected by API: %v", kind, err)
			}
			count++
		}
	}
	if count != len(resources) {
		t.Fatalf("validated %d kinds, want %d", count, len(resources))
	}
}
