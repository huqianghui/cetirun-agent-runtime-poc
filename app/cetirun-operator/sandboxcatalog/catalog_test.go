// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcatalog

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
)

func TestReaderStorageOptions(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	configuration := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: "platform-system", Name: "sandbox-platform", UID: "configuration-uid", ResourceVersion: "1",
	}, Data: map[string]string{sandboxplatform.ConfigurationKey: `{"version":"v1","registrations":{},"platforms":{},"runtimeClasses":{},"storageClasses":{"example-csi":{"UID":"storage-uid","Provisioner":"csi.example.com"}}}`}}
	storageClass := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "example-csi", UID: "storage-uid", ResourceVersion: "1"}, Provisioner: "csi.example.com", AllowVolumeExpansion: ptr.To(true)}
	reader := Reader{APIReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(configuration, storageClass).Build(), Namespace: "platform-system", Name: "sandbox-platform"}
	options, err := reader.StorageOptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(options.StorageClasses) != 1 || options.StorageClasses[0].Name != "example-csi" || !options.StorageClasses[0].AllowVolumeExpansion {
		t.Fatalf("catalog options = %+v", options)
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"storageClasses":[{"name":"example-csi","provisioner":"csi.example.com","allowVolumeExpansion":true}],"homeAccessModes":{}}` {
		t.Fatalf("catalog JSON = %s", encoded)
	}
}
