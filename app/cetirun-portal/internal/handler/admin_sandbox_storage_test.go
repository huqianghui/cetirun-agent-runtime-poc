// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func storageOptionsFixture() (*corev1.ConfigMap, *storagev1.StorageClass, config.Config) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-platform", Namespace: "operator-system", UID: "platform-uid"},
		Data: map[string]string{"platform.json": `{"version":"v1","registrations":{},"platforms":{},"runtimeClasses":{},"storageClasses":{"example-csi":{"UID":"storage-uid","Provisioner":"csi.example.com"}}}`}}
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "example-csi", UID: "storage-uid"}, Provisioner: "csi.example.com", AllowVolumeExpansion: ptr.To(true)}
	cfg := config.Config{SandboxStoragePlatformNamespace: cm.Namespace, SandboxStoragePlatformConfigMapName: cm.Name, SandboxStorageDefaultClassName: sc.Name}
	return cm, sc, cfg
}

func TestAdminSandboxStorageOptions(t *testing.T) {
	for _, tc := range []struct {
		name, defaultName, wantDefault string
		missing                        bool
	}{
		{name: "approved default", defaultName: "example-csi", wantDefault: "example-csi"},
		{name: "no default"},
		{name: "unapproved default", defaultName: "unapproved"},
		{name: "missing default", defaultName: "example-csi", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cm, sc, cfg := storageOptionsFixture()
			cfg.SandboxStorageDefaultClassName = tc.defaultName
			objects := []client.Object{cm}
			if !tc.missing {
				objects = append(objects, sc)
			}
			h := &AdminSandboxHandler{Kube: kube.NewClient(newFakeClient(objects...)), Config: cfg}
			w := httptest.NewRecorder()
			h.GetStorageOptions(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/sandbox-storage-options", nil))
			if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
			}
			var dto sandboxStorageOptionsDTO
			if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
				t.Fatal(err)
			}
			if dto.DefaultStorageClassName != tc.wantDefault || dto.StorageClasses == nil || dto.HomeAccessModes == nil {
				t.Fatalf("unexpected options: %s", w.Body.String())
			}
			if tc.missing {
				if len(dto.StorageClasses) != 0 {
					t.Fatal("missing class remains selectable")
				}
			} else if len(dto.StorageClasses) != 1 || dto.StorageClasses[0].Name != sc.Name || dto.StorageClasses[0].Provisioner != sc.Provisioner || !dto.StorageClasses[0].AllowVolumeExpansion {
				t.Fatalf("storage details not projected: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "storage-uid") || strings.Contains(w.Body.String(), "platform.json") {
				t.Fatalf("internal configuration leaked: %s", w.Body.String())
			}
		})
	}
}

func TestAdminSandboxStorageOptionsUnavailable(t *testing.T) {
	for _, problem := range []string{"unconfigured", "missing config", "malformed config", "read failure"} {
		t.Run(problem, func(t *testing.T) {
			cm, sc, cfg := storageOptionsFixture()
			if problem == "malformed config" {
				cm.Data["platform.json"] = `{"unexpected":"diagnostic-example"}`
			}
			objects := []client.Object{sc}
			if problem != "missing config" {
				objects = append(objects, cm)
			}
			if problem == "unconfigured" {
				cfg = config.Config{}
			}
			kc := newFakeClient(objects...)
			if problem == "read failure" {
				kc = interceptor.NewClient(kc.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return fmt.Errorf("diagnostic-example: backend read failed")
				}})
			}
			h := &AdminSandboxHandler{Kube: kube.NewClient(kc), Config: cfg}
			w := httptest.NewRecorder()
			h.GetStorageOptions(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/sandbox-storage-options", nil))
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "diagnostic-example") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAdminSandboxStorageOptionsRefreshesApproval(t *testing.T) {
	cm, sc, cfg := storageOptionsFixture()
	kc := newFakeClient(cm, sc)
	h := &AdminSandboxHandler{Kube: kube.NewClient(kc), Config: cfg}
	for _, wantCount := range []int{1, 0} {
		w := httptest.NewRecorder()
		h.GetStorageOptions(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/sandbox-storage-options", nil))
		var dto sandboxStorageOptionsDTO
		if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil || w.Code != http.StatusOK || len(dto.StorageClasses) != wantCount {
			t.Fatalf("stale options: status=%d body=%s error=%v", w.Code, w.Body.String(), err)
		}
		if wantCount == 1 {
			current := &corev1.ConfigMap{}
			if err := kc.Get(context.Background(), client.ObjectKeyFromObject(cm), current); err != nil {
				t.Fatal(err)
			}
			current.Data["platform.json"] = `{"version":"v1","registrations":{},"platforms":{},"runtimeClasses":{},"storageClasses":{}}`
			if err := kc.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
		}
	}
}
