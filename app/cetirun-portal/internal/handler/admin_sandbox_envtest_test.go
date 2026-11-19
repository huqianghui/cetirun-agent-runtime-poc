// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestSandboxDefaultsThroughPortalAndServedAPI(t *testing.T) {
	if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
		t.Fatal("Sandbox default regression refuses an existing cluster")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, name))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatal("set KUBEBUILDER_ASSETS to preinstalled isolated test binaries")
		}
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../../cetirun-operator/config/crd/bases"}, ErrorIfCRDPathMissing: true,
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: 30 * time.Second,
	}
	config, err := environment.Start()
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kc, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	handler := &AdminSandboxHandler{Kube: kube.NewClient(kc)}
	for index, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
		for _, runtimeType := range []struct {
			name    string
			present bool
			value   any
			want    api.SandboxType
		}{
			{name: "omitted", want: api.SandboxKata},
			{name: "null", present: true, want: api.SandboxKata},
			{name: "kata", present: true, value: "kata", want: api.SandboxKata},
			{name: "gvisor", present: true, value: "gvisor", want: api.SandboxGVisor},
			{name: "container", present: true, value: "container", want: api.SandboxContainer},
		} {
			t.Run(fmt.Sprintf("%s/%s", mode, runtimeType.name), func(t *testing.T) {
				expected := adminSandboxFixture()
				expected.Type = runtimeType.want
				expected.Storage.Home.AccessMode = mode
				spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&expected)
				if err != nil {
					t.Fatal(err)
				}
				delete(spec, "type")
				if runtimeType.present {
					spec["type"] = runtimeType.value
				}
				name := fmt.Sprintf("runtime-%d-%s", index, runtimeType.name)
				direct := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": api.GroupVersion.String(), "kind": "Sandbox",
					"metadata": map[string]any{"name": name}, "spec": spec,
				}}
				if err := kc.Create(ctx, direct, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
					t.Fatal(err)
				}
				stored := &api.Sandbox{}
				if err := kc.Get(ctx, client.ObjectKey{Name: name}, stored); err != nil {
					t.Fatal(err)
				}
				if !equality.Semantic.DeepEqual(stored.Spec, expected) {
					t.Fatal("served schema did not preserve the defaulted complete Sandbox")
				}

				portalBody := maps.Clone(spec)
				portalBody["name"] = "portal-" + name
				response := httptest.NewRecorder()
				handler.CreateSandbox(response, sandboxRequest(t, http.MethodPost, "", portalBody).WithContext(ctx))
				if response.Code != http.StatusCreated {
					t.Fatalf("Portal default: %d %s", response.Code, response.Body.String())
				}
				var dto SandboxDTO
				if err := json.Unmarshal(response.Body.Bytes(), &dto); err != nil {
					t.Fatal(err)
				}
				if !equality.Semantic.DeepEqual(dto.SandboxSpec, expected) {
					t.Fatal("Portal and served API defaulted different resource/storage fields")
				}
				expected.Storage.Home.Size = resource.MustParse("12Gi")
				update := SandboxUpsertRequest{Name: dto.Name, UID: dto.UID, ResourceVersion: dto.ResourceVersion, SandboxSpec: expected}
				if runtimeType.want == api.SandboxKata {
					update.Type = ""
				}
				response = httptest.NewRecorder()
				handler.UpdateSandbox(response, sandboxRequest(t, http.MethodPut, dto.Name, update).WithContext(ctx))
				if response.Code != http.StatusOK {
					t.Fatalf("Portal defaulted update: %d %s", response.Code, response.Body.String())
				}
				response = httptest.NewRecorder()
				handler.UpdateSandbox(response, sandboxRequest(t, http.MethodPut, dto.Name, update).WithContext(ctx))
				if response.Code != http.StatusConflict {
					t.Fatal("defaulting bypassed stale-version protection")
				}
				invalid := direct.DeepCopy()
				invalid.SetName("invalid-" + name)
				invalid.SetUID("")
				invalid.SetResourceVersion("")
				if err := unstructured.SetNestedField(invalid.Object, "", "spec", "type"); err != nil {
					t.Fatal(err)
				}
				if err := kc.Create(ctx, invalid); !apierrors.IsInvalid(err) {
					t.Fatal("served schema accepted an explicitly invalid type:", err)
				}
			})
		}
	}
}
