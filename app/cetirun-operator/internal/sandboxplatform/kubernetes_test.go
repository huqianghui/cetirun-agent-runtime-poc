// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplatform_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
)

const emptyConfigurationJSON = `{"version":"v1","registrations":{},"platforms":{},"runtimeClasses":{},"storageClasses":{}}`

type configurationReader struct {
	client.Reader
	reads int
	lists int
	after func(*corev1.ConfigMap, int)
}

func (r *configurationReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.reads++
	cm, ok := object.(*corev1.ConfigMap)
	if !ok {
		return fmt.Errorf("unexpected platform dependency %T", object)
	}
	if err := r.Reader.Get(ctx, key, cm, opts...); err != nil {
		return err
	}
	if r.after != nil {
		r.after(cm, r.reads)
	}
	return nil
}

func (r *configurationReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	r.lists++
	return errors.New("unexpected platform discovery")
}

func newKubernetesSource(t *testing.T) (*sandboxplatform.KubernetesSource, *configurationReader) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "platform-system", Name: "sandbox-platform",
			UID: "example-configuration-uid", ResourceVersion: "1",
		},
		Data: map[string]string{sandboxplatform.ConfigurationKey: emptyConfigurationJSON},
	}
	reader := &configurationReader{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()}
	source := &sandboxplatform.KubernetesSource{
		APIReader: reader, Namespace: cm.Namespace, Name: cm.Name,
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	return source, reader
}

func TestKubernetesSourceSnapshot(t *testing.T) {
	source, reader := newKubernetesSource(t)
	started := source.Now()
	clock := started
	source.Now = func() time.Time { return clock }
	reader.after = func(*corev1.ConfigMap, int) { clock = clock.Add(time.Second) }
	var inventory sandboxplatform.Source = source
	snapshot, err := inventory.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ObservedAt != started {
		t.Fatal("observation must start before authoritative reads")
	}
	if snapshot.Registrations == nil || len(snapshot.Registrations) != 0 ||
		snapshot.Platforms == nil || len(snapshot.Platforms) != 0 ||
		snapshot.RuntimeClasses == nil || len(snapshot.RuntimeClasses) != 0 ||
		snapshot.StorageClasses == nil || len(snapshot.StorageClasses) != 0 {
		t.Fatalf("empty inventory invented support or lost required maps: %+v", snapshot)
	}
	if reader.reads != 2 || reader.lists != 0 {
		t.Fatalf("snapshot must reread its selected ConfigMap without discovery: reads=%d lists=%d", reader.reads, reader.lists)
	}
}

func TestKubernetesSourceChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.ConfigMap)
	}{
		{"UID", func(cm *corev1.ConfigMap) { cm.UID = "example-replacement-uid" }},
		{"resourceVersion", func(cm *corev1.ConfigMap) { cm.ResourceVersion = "2" }},
		{"document", func(cm *corev1.ConfigMap) { cm.Data[sandboxplatform.ConfigurationKey] += " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, reader := newKubernetesSource(t)
			reader.after = func(cm *corev1.ConfigMap, reads int) {
				if reads == 2 {
					tc.mutate(cm)
				}
			}
			snapshot, err := source.Snapshot(context.Background())
			if !errors.Is(err, sandboxplatform.ErrSourceChanged) {
				t.Fatalf("concurrent change lost its shared error category: %v", err)
			}
			if !reflect.DeepEqual(snapshot, sandboxplatform.Snapshot{}) {
				t.Fatalf("concurrent change returned partial inventory: %+v", snapshot)
			}
			if reader.reads != 2 || reader.lists != 0 {
				t.Fatalf("unexpected reads=%d lists=%d", reader.reads, reader.lists)
			}
		})
	}
}
