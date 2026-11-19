// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

type agentMutationHelm struct{ HelmInstaller }

func (*agentMutationHelm) ResolveImageUpgrade(_, _, _, _, _, _ string) (string, error) {
	return "registry.example.com/agent:2", nil
}

func (*agentMutationHelm) Prepare(string, map[string]any, string) (*helm.PreparedRelease, error) {
	return &helm.PreparedRelease{Agent: agents.ResolvedAgent{
		Type: "hermes", Image: "registry.example.com/agent:1", Port: ptr.To[int32](18789),
	}}, nil
}

func (*agentMutationHelm) Uninstall(string, string) error { return nil }

type concurrentAgentWriter struct{ client.Client }

func (c concurrentAgentWriter) beforeWrite(ctx context.Context, object client.Object) error {
	if _, ok := object.(*api.Agent); ok {
		latest := &unstructured.Unstructured{}
		latest.SetAPIVersion(api.GroupVersion.String())
		latest.SetKind("Agent")
		latest.SetName(object.GetName())
		concurrent := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"futureConfiguration":"concurrent"}}`))
		if err := c.Client.Patch(ctx, latest, concurrent); err != nil {
			return err
		}
	}
	return nil
}

func (c concurrentAgentWriter) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if err := c.beforeWrite(ctx, object); err != nil {
		return err
	}
	return c.Client.Update(ctx, object, options...)
}

func (c concurrentAgentWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if err := c.beforeWrite(ctx, object); err != nil {
		return err
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func TestAgentSpecAndFinalizerPatchesPreserveFutureFields(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Fatal("KUBEBUILDER_ASSETS must name local envtest binaries")
	}
	data, err := os.ReadFile("../../config/crd/bases/cetirun.microsoft.com_agents.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var definition apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &definition); err != nil {
		t.Fatal(err)
	}
	spec := definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	spec.Properties["futureConfiguration"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	for _, parent := range []string{"agent", "helm"} {
		nested := spec.Properties[parent]
		nested.Properties["futureConfiguration"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
		spec.Properties[parent] = nested
	}
	definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
	// Deletion queries an absent VirtualService; only its API discovery is needed.
	virtualServices := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "virtualservices.networking.istio.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "networking.istio.io", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "virtualservices", Kind: "VirtualService"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1beta1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: ptr.To(true),
				}},
			}},
		},
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDs:                     []*apiextensionsv1.CustomResourceDefinition{&definition, virtualServices},
		ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 30 * time.Second,
	}
	cfg, err := environment.Start()
	if err != nil {
		_ = environment.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, istiov1beta1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const operatorNamespace = "spec-patch-test"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: operatorNamespace}}); err != nil {
		t.Fatal(err)
	}
	reconcile := func(r *AgentReconciler, agent *api.Agent) error {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
		return err
	}
	for _, test := range []struct {
		name     string
		deleting bool
		prepare  func(*api.Agent)
		run      func(*AgentReconciler, *api.Agent) error
		want     func(*api.Agent)
	}{
		{"upgrade-default-image", false, func(*api.Agent) {},
			func(r *AgentReconciler, a *api.Agent) error {
				a.Status.HelmReleaseName = helmReleaseName(a.Name)
				_, err := r.upgradeDefaultImage(ctx, a)
				return err
			}, func(a *api.Agent) { a.Spec.Agent.Image = ptr.To("registry.example.com/agent:2") }},
		{"remove-finalizer", true, func(*api.Agent) {}, reconcile, func(a *api.Agent) { a.Finalizers = []string{"example.com/retain"} }},
		{"custom-ownership-finalizer", false, func(a *api.Agent) {
			a.Spec.Agent.Type, a.Spec.GatewayTokenSecretRef = api.AgentTypeCustom, ""
		},
			func(r *AgentReconciler, a *api.Agent) error {
				bindings := runtimebindings.Custom{
					Namespace: runtimebindings.ObjectReference{Name: instanceNamespace(a.Name), UID: "synthetic-namespace"},
					Home: runtimebindings.Home{Name: "home", Claim: &runtimebindings.HomeClaim{
						StorageClass: "test-home", StorageSize: "1Gi", AccessMode: "ReadWriteOnce",
					}},
				}
				return r.ensureCustomOwnership(ctx, a, bindings, nil)
			}, func(a *api.Agent) { a.Finalizers = append(a.Finalizers, customOwnershipFinalizer) }},
	} {
		for _, concurrent := range []bool{false, true} {
			name := test.name
			if concurrent {
				name += "-conflict"
			}
			t.Run(name, func(t *testing.T) {
				agent := &api.Agent{
					ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{api.Finalizer, "example.com/retain"}},
					Spec: api.AgentSpec{
						Agent: api.HarnessSpec{Type: api.AgentTypeHermes, Image: ptr.To("registry.example.com/agent:1"), Port: ptr.To[int32](18789)},
						User:  api.UserSpec{Alias: "patch-user", DisplayName: "Example User"},
						Team:  "example-team", SandboxRef: "standard",
						Helm: api.HelmSpec{Version: "1.0.0"}, GatewayTokenSecretRef: "synthetic-token",
					},
				}
				test.prepare(agent)
				object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(agent)
				if err != nil {
					t.Fatal(err)
				}
				raw := &unstructured.Unstructured{Object: object}
				raw.SetAPIVersion(api.GroupVersion.String())
				raw.SetKind("Agent")
				paths := [][]string{{"spec", "futureConfiguration"}, {"spec", "agent", "futureConfiguration"}, {"spec", "helm", "futureConfiguration"}}
				for _, path := range paths {
					if err := unstructured.SetNestedField(raw.Object, "preserve-me", path...); err != nil {
						t.Fatal(err)
					}
				}
				if err := cl.Create(ctx, raw, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
					t.Fatal(err)
				}
				if test.deleting {
					if err := cl.Delete(ctx, raw); err != nil {
						t.Fatal(err)
					}
				}
				key := client.ObjectKeyFromObject(agent)
				if err := cl.Get(ctx, key, agent); err != nil {
					t.Fatal(err)
				}
				before := agent.DeepCopy()
				r := &AgentReconciler{Client: cl, APIReader: cl, Scheme: scheme, HelmClient: &agentMutationHelm{},
					EventRecorder: record.NewFakeRecorder(10), ChartVersion: "1.0.0", OperatorNamespace: operatorNamespace}
				if concurrent {
					r.Client = concurrentAgentWriter{cl}
				}
				err = test.run(r, agent)
				if concurrent {
					if !apierrors.IsConflict(err) {
						t.Fatalf("stale Agent write must conflict: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if err := cl.Get(ctx, key, raw); err != nil {
					t.Fatal(err)
				}
				for i, path := range paths {
					want := "preserve-me"
					if concurrent && i == 0 {
						want = "concurrent"
					}
					value, found, err := unstructured.NestedString(raw.Object, path...)
					if err != nil || !found || value != want {
						t.Errorf("Agent write lost future field %v: value=%q found=%v err=%v", path, value, found, err)
					}
				}
				if err := cl.Get(ctx, key, agent); err != nil {
					t.Fatal(err)
				}
				want := before.DeepCopy()
				if !concurrent {
					test.want(want)
				}
				if agent.UID != before.UID || !reflect.DeepEqual(agent.Spec, want.Spec) || !slices.Equal(agent.Finalizers, want.Finalizers) {
					t.Fatalf("Agent write changed its identity or unexpected known fields: spec=%+v finalizers=%v", agent.Spec, agent.Finalizers)
				}
				generation := before.Generation
				if concurrent || !reflect.DeepEqual(before.Spec, want.Spec) {
					generation++
				}
				if agent.Generation != generation {
					t.Fatalf("generation=%d, want %d", agent.Generation, generation)
				}
			})
		}
	}
}
