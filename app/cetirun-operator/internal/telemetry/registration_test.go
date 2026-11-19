// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// stubCache answers GetInformer: kinds in installed get an informer, other
// kinds a no-match error, unless failure is set.
type stubCache struct {
	cache.Cache
	installed map[string]bool
	failure   error
	informers map[string]*stubInformer
}

type stubInformer struct {
	cache.Informer
	handlers int
}

func (i *stubInformer) AddEventHandler(toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	i.handlers++
	return nil, nil
}

func (c *stubCache) GetInformer(_ context.Context, object client.Object, _ ...cache.InformerGetOption) (cache.Informer, error) {
	if c.failure != nil {
		return nil, c.failure
	}
	name := kindName(object)
	if !c.installed[name] {
		return nil, &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "cetirun.microsoft.com", Kind: name}}
	}
	if c.informers == nil {
		c.informers = map[string]*stubInformer{}
	}
	if c.informers[name] == nil {
		c.informers[name] = &stubInformer{}
	}
	return c.informers[name], nil
}

func kindName(object client.Object) string {
	for _, kind := range kinds {
		if fmt.Sprintf("%T", kind.object) == fmt.Sprintf("%T", object) {
			return kind.logType
		}
	}
	return "event"
}

func TestRegistrationWaitsForMissingResources(t *testing.T) {
	c := &stubCache{installed: map[string]bool{}}
	r := &Recorder{Writer: NewWriter(&bytes.Buffer{}), Events: EventsNone, Log: logr.Discard()}
	if err := r.Register(context.Background(), c); err != nil {
		t.Fatalf("Register without the CetiRun resources: %v", err)
	}
	for _, kind := range kinds {
		if r.isRegistered(kind.logType) {
			t.Fatalf("%s registered without its resource", kind.logType)
		}
	}
	// A snapshot reads nothing while no kind is registered.
	if err := r.Snapshot(context.Background(), nil); err != nil {
		t.Fatalf("Snapshot without the CetiRun resources: %v", err)
	}

	for _, kind := range kinds {
		c.installed[kind.logType] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.RunRegistration(ctx, c, 10*time.Millisecond); err != nil {
		t.Fatalf("RunRegistration: %v", err)
	}
	for _, kind := range kinds {
		if !r.isRegistered(kind.logType) {
			t.Fatalf("%s not registered after its resource appeared", kind.logType)
		}
	}
	// A second pass adds no second handler.
	if _, err := r.registerInventory(ctx, c); err != nil {
		t.Fatal(err)
	}
	for name, informer := range c.informers {
		if informer.handlers != 1 {
			t.Fatalf("%s has %d handlers, want 1", name, informer.handlers)
		}
	}
}

func TestRegistrationStopsOnOtherErrors(t *testing.T) {
	c := &stubCache{failure: errors.New("forbidden")}
	r := &Recorder{Writer: NewWriter(&bytes.Buffer{}), Events: EventsNone, Log: logr.Discard()}
	if err := r.Register(context.Background(), c); err == nil {
		t.Fatal("Register hid an error that is not a missing resource")
	}
}
