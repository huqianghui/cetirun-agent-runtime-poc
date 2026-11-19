// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplatform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type storageOptionsReader struct {
	client.Reader
	configReads   int
	afterConfig   func(*corev1.ConfigMap, int)
	storageReads  map[string]int
	afterStorage  func(*storagev1.StorageClass, int)
	storageErrFor func(client.ObjectKey, int) error
}

func (r *storageOptionsReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if storageClass, ok := object.(*storagev1.StorageClass); ok {
		if r.storageReads == nil {
			r.storageReads = map[string]int{}
		}
		r.storageReads[key.Name]++
		if r.storageErrFor != nil {
			if err := r.storageErrFor(key, r.storageReads[key.Name]); err != nil {
				return err
			}
		}
		if err := r.Reader.Get(ctx, key, storageClass, options...); err != nil {
			return err
		}
		if r.afterStorage != nil {
			r.afterStorage(storageClass, r.storageReads[key.Name])
		}
		return nil
	}
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if configuration, ok := object.(*corev1.ConfigMap); ok {
		r.configReads++
		if r.afterConfig != nil {
			r.afterConfig(configuration, r.configReads)
		}
	}
	return nil
}

func storageOptionsSource(t *testing.T, document Configuration, objects ...client.Object) (*KubernetesSource, *storageOptionsReader) {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	configuration := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: "platform-system", Name: "sandbox-platform", UID: "configuration-uid", ResourceVersion: "1",
	}, Data: map[string]string{ConfigurationKey: string(raw)}}
	objects = append(objects, configuration)
	reader := &storageOptionsReader{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), storageReads: map[string]int{}}
	return &KubernetesSource{APIReader: reader, Namespace: configuration.Namespace, Name: configuration.Name}, reader
}

func storageOptionsDocument(classes map[string]StoragePrerequisite) Configuration {
	if classes == nil {
		classes = map[string]StoragePrerequisite{}
	}
	return Configuration{
		Version: "v1", Registrations: map[api.SandboxType]sandbox.Registration{}, Platforms: map[api.SandboxType]sandboxpolicy.Platform{},
		RuntimeClasses: map[string]RuntimePrerequisite{}, StorageClasses: classes,
	}
}

func TestKubernetesSourceStorageOptions(t *testing.T) {
	classes := map[string]StoragePrerequisite{
		"healthy":        {UID: "healthy-uid", Provisioner: "csi.healthy.example"},
		"missing":        {UID: "missing-uid", Provisioner: "csi.missing.example"},
		"replaced":       {UID: "old-uid", Provisioner: "csi.replaced.example"},
		"provider-drift": {UID: "provider-drift-uid", Provisioner: "csi.expected.example"},
		"terminating":    {UID: "terminating-uid", Provisioner: "csi.terminating.example"},
	}
	deleting := metav1.Now()
	source, reader := storageOptionsSource(t, storageOptionsDocument(classes),
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "healthy", UID: "healthy-uid", ResourceVersion: "1"}, Provisioner: "csi.healthy.example", AllowVolumeExpansion: ptr.To(true)},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "replaced", UID: "new-uid", ResourceVersion: "1"}, Provisioner: "csi.replaced.example"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "provider-drift", UID: "provider-drift-uid", ResourceVersion: "1"}, Provisioner: "csi.actual.example"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "terminating", UID: "terminating-uid", ResourceVersion: "1", DeletionTimestamp: &deleting, Finalizers: []string{"test.example/finalizer"}}, Provisioner: "csi.terminating.example"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "unapproved", UID: "unapproved-uid", ResourceVersion: "1"}, Provisioner: "csi.unapproved.example"},
	)
	options, err := source.StorageOptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(options.StorageClasses) != 1 || options.StorageClasses[0] != (StorageOption{Name: "healthy", Provisioner: "csi.healthy.example", AllowVolumeExpansion: true}) {
		t.Fatalf("available approved options = %+v", options.StorageClasses)
	}
	if options.HomeAccessModes == nil || len(options.HomeAccessModes) != 0 {
		t.Fatalf("unexpected runtime modes: %+v", options.HomeAccessModes)
	}
	if reader.configReads != 2 {
		t.Fatalf("configuration reads = %d, want 2", reader.configReads)
	}
	if reader.storageReads["unapproved"] != 0 {
		t.Fatalf("unapproved StorageClass was read: %+v", reader.storageReads)
	}
}

func TestKubernetesSourceStorageOptionsRejectsInvalidConfiguration(t *testing.T) {
	source, _ := storageOptionsSource(t, storageOptionsDocument(nil))
	configuration, err := source.readConfiguration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	configuration.Data[ConfigurationKey] = `{"version":"v1","registrations":{},"platforms":{},"runtimeClasses":{},"storageClasses":{},"extra":true}`
	// A reader that returns the malformed configuration verifies the same strict
	// decoder is used by the narrow catalog path.
	source.APIReader = &staticConfigReader{configuration: configuration}
	if _, err := source.StorageOptions(context.Background()); !errors.Is(err, sandboxpolicy.ErrUnavailable) {
		t.Fatalf("invalid platform document error = %v", err)
	}
}

type staticConfigReader struct{ configuration *corev1.ConfigMap }

func (r *staticConfigReader) Get(_ context.Context, _ client.ObjectKey, object client.Object, _ ...client.GetOption) error {
	configuration, ok := object.(*corev1.ConfigMap)
	if !ok {
		return fmt.Errorf("unexpected object %T", object)
	}
	r.configuration.DeepCopyInto(configuration)
	return nil
}

func (r *staticConfigReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("unexpected list")
}

func TestKubernetesSourceStorageOptionsFailsOnStorageRead(t *testing.T) {
	source, reader := storageOptionsSource(t, storageOptionsDocument(map[string]StoragePrerequisite{
		"healthy": {UID: "healthy-uid", Provisioner: "csi.healthy.example"},
	}))
	reader.storageErrFor = func(client.ObjectKey, int) error { return errors.New("forbidden") }
	if _, err := source.StorageOptions(context.Background()); err == nil {
		t.Fatal("storage read failure was accepted")
	}
}

func TestKubernetesSourceStorageOptionsRejectsStorageClassDrift(t *testing.T) {
	source, reader := storageOptionsSource(t, storageOptionsDocument(map[string]StoragePrerequisite{
		"healthy": {UID: "healthy-uid", Provisioner: "csi.healthy.example"},
	}), &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "healthy", UID: "healthy-uid", ResourceVersion: "1"}, Provisioner: "csi.healthy.example"})
	reader.afterStorage = func(storageClass *storagev1.StorageClass, reads int) {
		if reads == 2 {
			storageClass.UID = "replacement-uid"
		}
	}
	if _, err := source.StorageOptions(context.Background()); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("StorageClass drift error = %v", err)
	}
}

func TestKubernetesSourceStorageOptionsFailsWhenStorageClassRecheckFails(t *testing.T) {
	source, reader := storageOptionsSource(t, storageOptionsDocument(map[string]StoragePrerequisite{
		"healthy": {UID: "healthy-uid", Provisioner: "csi.healthy.example"},
	}), &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "healthy", UID: "healthy-uid", ResourceVersion: "1"}, Provisioner: "csi.healthy.example"})
	reader.storageErrFor = func(_ client.ObjectKey, reads int) error {
		if reads == 2 {
			return errors.New("recheck failed")
		}
		return nil
	}
	if _, err := source.StorageOptions(context.Background()); err == nil {
		t.Fatal("StorageClass recheck failure was accepted")
	}
}

func TestKubernetesSourceStorageOptionsRejectsConfigurationDrift(t *testing.T) {
	source, reader := storageOptionsSource(t, storageOptionsDocument(map[string]StoragePrerequisite{
		"healthy": {UID: "healthy-uid", Provisioner: "csi.healthy.example"},
	}), &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "healthy", UID: "healthy-uid", ResourceVersion: "1"}, Provisioner: "csi.healthy.example"})
	reader.afterConfig = func(configuration *corev1.ConfigMap, reads int) {
		if reads == 2 {
			configuration.ResourceVersion = "2"
		}
	}
	if _, err := source.StorageOptions(context.Background()); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("configuration drift error = %v", err)
	}
}

func TestStorageOptionsFromConfigurationUsesAvailableRuntimeModes(t *testing.T) {
	document := storageOptionsDocument(nil)
	document.Registrations = map[api.SandboxType]sandbox.Registration{
		api.SandboxKata:   {Available: true, Capabilities: sandbox.Capabilities{HomeAccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteMany, corev1.ReadWriteOnce}}},
		api.SandboxGVisor: {Available: false, Capabilities: sandbox.Capabilities{HomeAccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}}},
	}
	options := storageOptionsFromConfiguration(document)
	want := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce}
	if got := options.HomeAccessModes[api.SandboxKata]; !slices.Equal(got, want) {
		t.Fatalf("kata HOME modes = %v, want %v", got, want)
	}
	if _, exists := options.HomeAccessModes[api.SandboxGVisor]; exists {
		t.Fatalf("unavailable runtime was included: %+v", options.HomeAccessModes)
	}
}
