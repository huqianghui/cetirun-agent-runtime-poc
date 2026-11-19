// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

func TestAgentStatusPatchPreservesFutureFields(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Fatal("KUBEBUILDER_ASSETS must name local envtest binaries")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
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
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, apiextensionsv1.AddToScheme} {
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
	var definition apiextensionsv1.CustomResourceDefinition
	if err := cl.Get(ctx, client.ObjectKey{Name: "agents." + api.GroupVersion.Group}, &definition); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../test/fixtures/registry-credentials-status.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var field apiextensionsv1.JSONSchemaProps
	if err := yaml.UnmarshalStrict(data, &field); err != nil {
		t.Fatal(err)
	}
	statusSchema := definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	statusSchema.Properties["futureRegistryCredentials"] = field
	identitySchema := statusSchema.Properties["agentIdentity"]
	identitySchema.Properties["futureObservation"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	statusSchema.Properties["agentIdentity"] = identitySchema
	definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"] = statusSchema
	if err := cl.Update(ctx, &definition); err != nil {
		t.Fatal(err)
	}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "status-compatibility"}, Spec: api.AgentSpec{
		Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: api.UserSpec{Alias: "status-user", DisplayName: "Example User"},
		Team: "example-team", SandboxRef: "standard",
		GatewayTokenSecretRef: "synthetic-token",
	}}
	if err := cl.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(agent)
	get := func() *unstructured.Unstructured {
		t.Helper()
		object := &unstructured.Unstructured{}
		object.SetAPIVersion(api.GroupVersion.String())
		object.SetKind("Agent")
		if err := cl.Get(ctx, key, object); err != nil {
			t.Fatal(err)
		}
		return object
	}
	future := map[string]any{"sourceUID": "synthetic-source", "sourceResourceVersion": "7", "pullSecretUID": "synthetic-pull"}
	if err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		object := get()
		if err := unstructured.SetNestedMap(object.Object, future, "status", "futureRegistryCredentials"); err != nil {
			return false, err
		}
		if err := unstructured.SetNestedMap(object.Object, map[string]any{"phase": "Disabled", "futureObservation": "preserve-me"}, "status", "agentIdentity"); err != nil {
			return false, err
		}
		err := cl.Status().Update(ctx, object, client.FieldValidation(metav1.FieldValidationStrict))
		return err == nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	assertFuture := func() {
		t.Helper()
		object := get()
		value, found, err := unstructured.NestedMap(object.Object, "status", "futureRegistryCredentials")
		if err != nil || !found || !reflect.DeepEqual(value, future) {
			t.Fatal("status patch lost the unknown top-level field", value, err)
		}
		nested, _, err := unstructured.NestedString(object.Object, "status", "agentIdentity", "futureObservation")
		if err != nil || nested != "preserve-me" {
			t.Fatal("status patch lost an unknown nested field", nested, err)
		}
	}
	r := &AgentReconciler{Client: cl, APIReader: cl}
	if err := cl.Get(ctx, key, agent); err != nil {
		t.Fatal(err)
	}
	agent.Status.Phase = api.PhaseReady
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	assertFuture()
	if get().GetUID() != agent.UID {
		t.Fatal("status patch replaced the Agent")
	}

	t.Run("stale status cannot overwrite a concurrent update", func(t *testing.T) {
		stale := agent.DeepCopy()
		object := get()
		if err := cl.Patch(ctx, object, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"example.com/concurrent":"write"}}}`))); err != nil {
			t.Fatal(err)
		}
		stale.Status.Phase = api.PhaseFailed
		if err := r.updateStatus(ctx, stale); !apierrors.IsConflict(err) {
			t.Fatalf("stale revision must conflict: %v", err)
		}
		phase, _, _ := unstructured.NestedString(get().Object, "status", "phase")
		if phase != api.PhaseReady {
			t.Fatal("stale status was persisted")
		}
		assertFuture()
	})
	t.Run("missing revision cannot claim the latest status", func(t *testing.T) {
		candidate := agent.DeepCopy()
		candidate.ResourceVersion = ""
		if err := r.updateStatus(ctx, candidate); !apierrors.IsConflict(err) {
			t.Fatalf("missing revision must conflict: %v", err)
		}
	})
	t.Run("identity failure checkpoint preserves unknown status", func(t *testing.T) {
		if err := cl.Get(ctx, key, agent); err != nil {
			t.Fatal(err)
		}
		agent.Status.AgentIdentity.Reason = "CreateFailed"
		if err := r.persistDefiniteCreateFailure(ctx, agent); err != nil {
			t.Fatal(err)
		}
		assertFuture()
	})
}
