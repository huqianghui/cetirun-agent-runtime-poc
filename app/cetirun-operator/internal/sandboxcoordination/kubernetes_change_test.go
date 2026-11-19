// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func configurationObject(kind string) client.Object {
	switch kind {
	case "Team":
		return &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "team"},
			Spec: api.TeamSpec{AgentPolicyRef: "policy",
				SandboxRefs: []string{"sandbox", "spare"}, DefaultSandboxRef: "sandbox"}}
	case "Sandbox":
		resources := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("128Mi")}
		return &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox"}, Spec: api.SandboxSpec{
			Type: api.SandboxKata,
			Resources: api.SandboxResources{
				Agent:   api.SandboxContainerResources{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()},
				Helpers: api.SandboxContainerResources{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()}},
			Storage: api.SandboxStorage{
				Home:    api.SandboxHomeStorage{StorageClassName: "private", Size: resource.MustParse("1Gi"), AccessMode: corev1.ReadWriteOncePod},
				Scratch: api.SandboxScratchStorage{SizeLimit: resource.MustParse("128Mi")}}}}
	case "AgentPolicy":
		return &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "policy"},
			Spec: api.AgentPolicySpec{Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeHermes}},
				Security: api.AgentPolicySecurity{ReadOnlyRootFilesystem: ptr.To(true)},
				Network:  api.AgentPolicyNetwork{Ingress: api.AgentPolicyIngress{Mode: api.AgentPolicyIngressAuthenticated}}}}
	default:
		panic("unsupported test fixture kind")
	}
}

func desiredConfiguration(before client.Object) client.Object {
	after := before.DeepCopyObject().(client.Object)
	switch object := after.(type) {
	case *api.Team:
		object.Spec.DefaultSandboxRef = "spare"
	case *api.Sandbox:
		object.Spec.Storage.Home.Size = resource.MustParse("2Gi")
	case *api.AgentPolicy:
		object.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
	}
	after.SetAnnotations(map[string]string{"example.com/change": "desired"})
	return after
}

func configurationFixture(t *testing.T, kind string) (Store, client.WithWatch, client.Object, client.Object) {
	t.Helper()
	store, storage := fixture(t)
	if err := api.AddToScheme(storage.Scheme()); err != nil {
		t.Fatal(err)
	}
	before := configurationObject(kind)
	before.SetUID(types.UID(before.GetName() + "-uid"))
	before.SetGeneration(1)
	before.SetCreationTimestamp(metav1.NewTime(time.Unix(1700000000, 0)))
	before.SetLabels(map[string]string{"example.com/label": "original"})
	before.SetAnnotations(map[string]string{"example.com/change": "original"})
	if err := storage.Create(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	return store, storage, before, desiredConfiguration(before)
}

func configurationAdapter(t *testing.T, writer client.Client, reader client.Reader, before, after client.Object) *KubernetesChangeAdapter {
	t.Helper()
	adapter, err := NewKubernetesChangeAdapter(writer, reader, before, after)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestConfigurationNoopCannotAcquireAReservation(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, _ := configurationFixture(t, kind)
			key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
			original := &corev1.ConfigMap{}
			if err := storage.Get(ctx, key, original); err != nil {
				t.Fatal(err)
			}
			same := before.DeepCopyObject().(client.Object)
			if _, err := store.AcquireConfigurationChange(ctx, before, same); !errors.Is(err, ErrConflict) {
				t.Fatal("no-op update must be rejected before reserving an operation:", err)
			}
			current := &corev1.ConfigMap{}
			if err := storage.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, current) {
				t.Fatal("no-op update changed coordination authority")
			}
		})
	}
}

func TestConfigurationDefaultsArePinnedBeforeReservation(t *testing.T) {
	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, after := configurationFixture(t, kind)
			switch object := after.(type) {
			case *api.Sandbox:
				object.Spec.Type = ""
			case *api.AgentPolicy:
				object.Spec.Security.ReadOnlyRootFilesystem = nil
				object.Spec.Network.Ingress.Mode = ""
				object.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 8080}}
				object.Spec.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{
					{CIDR: "203.0.113.0/24", Ports: []api.AgentPolicyPort{{Port: 8443}}},
				}
			}
			original := after.DeepCopyObject()
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			canonical := after.DeepCopyObject().(client.Object)
			switch object := canonical.(type) {
			case *api.Sandbox:
				object.Spec.Type = api.SandboxKata
			case *api.AgentPolicy:
				object.Spec.Security.ReadOnlyRootFilesystem = ptr.To(true)
				object.Spec.Network.Ingress.Mode = api.AgentPolicyIngressAuthenticated
				object.Spec.Network.BlockedPorts[0].Protocol = corev1.ProtocolTCP
				object.Spec.Network.AllowedInternalCIDRs[0].Ports[0].Protocol = corev1.ProtocolTCP
			}
			same, err := store.ReadConfigurationChange(ctx, before, canonical)
			if err != nil || !reflect.DeepEqual(ticket, same) {
				t.Fatal("API-defaulted content did not identify the reserved write:", err)
			}
			adapter := configurationAdapter(t, storage, storage, before, after)
			ticket, err = store.BeginChange(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AuthorizeConfigurationUpdate(ctx, before, canonical); err != nil {
				t.Fatal("API defaults changed admission authority:", err)
			}
			if err := adapter.Submit(ctx, ticket, func(ctx context.Context) error {
				return store.CheckChange(ctx, ticket)
			}); err != nil {
				t.Fatal(err)
			}
			current := before.DeepCopyObject().(client.Object)
			if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
				t.Fatal(err)
			}
			switch object := current.(type) {
			case *api.Sandbox:
				if object.Spec.Type != api.SandboxKata {
					t.Fatal("submitted Sandbox omitted its schema default")
				}
			case *api.AgentPolicy:
				if object.Spec.Security.ReadOnlyRootFilesystem == nil || !*object.Spec.Security.ReadOnlyRootFilesystem ||
					object.Spec.Network.Ingress.Mode != api.AgentPolicyIngressAuthenticated ||
					object.Spec.Network.BlockedPorts[0].Protocol != corev1.ProtocolTCP ||
					object.Spec.Network.AllowedInternalCIDRs[0].Ports[0].Protocol != corev1.ProtocolTCP {
					t.Fatal("submitted policy omitted schema defaults")
				}
			}
			recovered, err := store.ReadConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal("original non-defaulted input could not recover the operation:", err)
			}
			if _, err := store.ReconcileChange(ctx, recovered,
				configurationAdapter(t, nil, storage, before, after)); err != nil {
				t.Fatal("matching defaulted write could not release its reservation:", err)
			}
			if !reflect.DeepEqual(original, after) {
				t.Fatal("defaulting modified caller-owned inputs")
			}
		})
	}
}

func TestConfigurationDefaultOnlyChangesCannotReserve(t *testing.T) {
	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, _ := configurationFixture(t, kind)
			after := before.DeepCopyObject().(client.Object)
			switch object := after.(type) {
			case *api.Sandbox:
				object.Spec.Type = ""
			case *api.AgentPolicy:
				object.Spec.Security.ReadOnlyRootFilesystem = nil
				object.Spec.Network.Ingress.Mode = ""
			}
			key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
			original := &corev1.ConfigMap{}
			if err := storage.Get(ctx, key, original); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, ErrConflict) {
				t.Fatal("a schema-defaulted no-op acquired a reservation:", err)
			}
			current := &corev1.ConfigMap{}
			if err := storage.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, current) {
				t.Fatal("a schema-defaulted no-op changed authority")
			}
		})
	}
}

func TestConfigurationFinalizationExcludesOutstandingOperations(t *testing.T) {
	for _, operation := range []string{"none", "unrelated", "reserved change", "changing", "old UID change", "reserved execution", "starting", "running", "stopping"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, _ := configurationFixture(t, "AgentPolicy")
			before.SetFinalizers([]string{"example.com/cleanup", "example.com/retain"})
			if err := storage.Update(ctx, before); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "reserved change", "changing", "old UID change", "unrelated":
				reference := source(api.GroupVersion.String(), "AgentPolicy", "", before.GetName(), string(before.GetUID()))
				reference.ResourceVersion = before.GetResourceVersion()
				if operation == "old UID change" {
					reference.UID = "previous-policy-uid"
				}
				if operation == "unrelated" {
					reference.Name, reference.UID = "other-policy", "other-policy-uid"
				}
				change, err := store.AcquireChange(ctx, reference, strings.Repeat("b", 64))
				if err != nil {
					t.Fatal(err)
				}
				if operation == "changing" {
					if _, err := store.BeginChange(ctx, change); err != nil {
						t.Fatal(err)
					}
				}
			case "reserved execution", "starting", "running", "stopping":
				ticket := acquire(t, store, testIntent("finalization"))
				var err error
				if operation != "reserved execution" {
					ticket, err = store.BeginStart(ctx, ticket)
					if err != nil {
						t.Fatal(err)
					}
				}
				if operation == "running" || operation == "stopping" {
					ticket, err = store.MarkRunning(ctx, ticket)
					if err != nil {
						t.Fatal(err)
					}
				}
				if operation == "stopping" {
					if _, err := store.BeginStop(ctx, ticket); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := storage.Delete(ctx, before); err != nil {
				t.Fatal(err)
			}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(before), before); err != nil {
				t.Fatal(err)
			}
			after := before.DeepCopyObject().(client.Object)
			after.SetFinalizers(nil)
			key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
			original := &corev1.ConfigMap{}
			if err := storage.Get(ctx, key, original); err != nil {
				t.Fatal(err)
			}
			store.Client = nil
			err := store.AuthorizeConfigurationFinalization(ctx, before, after)
			allowed := operation == "none" || operation == "unrelated"
			if allowed && err != nil || !allowed && !errors.Is(err, ErrConflict) {
				t.Fatalf("allowed=%v: %v", allowed, err)
			}
			current := &corev1.ConfigMap{}
			if err := storage.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, current) {
				t.Fatal("finalization check changed an outstanding operation")
			}
		})
	}
}

func TestConfigurationFinalizationOnlyRemovesFinalizers(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		for _, change := range []string{"valid", "managedFields", "not terminating", "unchanged", "added", "duplicate", "labels", "annotations", "owners", "spec", "status", "UID", "RV", "generation", "deletion", "missing authority", "stale source"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				store, storage, before, _ := configurationFixture(t, kind)
				before.SetFinalizers([]string{"example.com/cleanup", "example.com/retain", "example.com/third"})
				if err := storage.Update(ctx, before); err != nil {
					t.Fatal(err)
				}
				if change != "not terminating" {
					if err := storage.Delete(ctx, before); err != nil {
						t.Fatal(err)
					}
					if err := storage.Get(ctx, client.ObjectKeyFromObject(before), before); err != nil {
						t.Fatal(err)
					}
				}
				after := before.DeepCopyObject().(client.Object)
				after.SetFinalizers([]string{"example.com/retain"})
				switch change {
				case "managedFields":
					after.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "synthetic-finalizer-controller"}})
				case "unchanged":
					after.SetFinalizers(before.GetFinalizers())
				case "added":
					after.SetFinalizers([]string{"example.com/new"})
				case "duplicate":
					after.SetFinalizers([]string{"example.com/retain", "example.com/retain"})
				case "labels":
					after.SetLabels(map[string]string{"example.com/unrelated": "changed"})
				case "annotations":
					after.SetAnnotations(map[string]string{"example.com/unrelated": "changed"})
				case "owners":
					after.SetOwnerReferences([]metav1.OwnerReference{w.AgentOwner("other", "other-uid")})
				case "spec":
					switch object := after.(type) {
					case *api.Team:
						object.Spec.DefaultSandboxRef = "other"
					case *api.Sandbox:
						object.Spec.Type = api.SandboxGVisor
					case *api.AgentPolicy:
						object.Spec.EntraID.Enabled = true
					}
				case "status":
					switch object := after.(type) {
					case *api.Team:
						object.Status.ObservedGeneration++
					case *api.Sandbox:
						object.Status.ObservedGeneration++
					case *api.AgentPolicy:
						object.Status.ObservedGeneration++
					}
				case "UID":
					after.SetUID("different")
				case "RV":
					after.SetResourceVersion("different")
				case "generation":
					after.SetGeneration(before.GetGeneration() + 1)
				case "deletion":
					after.SetDeletionTimestamp(nil)
				case "missing authority":
					store.Identity.UID = "different"
				case "stale source":
					current := before.DeepCopyObject().(client.Object)
					current.SetLabels(map[string]string{"example.com/concurrent": "changed"})
					if err := storage.Update(ctx, current); err != nil {
						t.Fatal(err)
					}
				}
				store.Client = nil
				err := store.AuthorizeConfigurationFinalization(ctx, before, after)
				allowed := change == "valid" || change == "managedFields"
				if (err == nil) != allowed {
					t.Fatalf("allowed=%v: %v", allowed, err)
				}
			})
		}
	}
}

func TestConfigurationLegacyTeamTransitionsUseWholeTeamReservation(t *testing.T) {
	for _, transition := range []struct {
		name                          string
		beforeGoverned, afterGoverned bool
	}{
		{"legacy update", false, false},
		{"add governance", false, true},
		{"remove governance", true, false},
	} {
		t.Run(transition.name, func(t *testing.T) {
			ctx := context.Background()
			store, storage, object, _ := configurationFixture(t, "Team")
			before := object.(*api.Team)
			governed := *before.Spec.DeepCopy()
			if !transition.beforeGoverned {
				before.Spec = api.TeamSpec{}
				if err := storage.Update(ctx, before); err != nil {
					t.Fatal(err)
				}
			}
			after := before.DeepCopy()
			if transition.afterGoverned {
				after.Spec = governed
			} else {
				after.Spec = api.TeamSpec{}
			}
			after.Annotations["example.com/change"] = "legacy-transition"
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal("valid legacy Team transition could not reserve a mutation:", err)
			}
			if ticket.Change.Team != nil {
				t.Fatal("legacy transition must reserve the entire Team, not a policy-class subset")
			}
			adapter := configurationAdapter(t, storage, storage, before, after)
			if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil {
				t.Fatal("legacy Team transition could not complete:", err)
			}
		})
	}
}

func configurationIntent(t *testing.T, before client.Object, suffix string) Intent {
	t.Helper()
	image, err := newConfigurationImage(before)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := w.ExecutionObjectDigest(image.object)
	if err != nil {
		t.Fatal(err)
	}
	intent := testIntent(suffix)
	for i, reference := range intent.Sources {
		if reference.Kind == image.reference.Kind {
			delete(intent.SourceDigests, w.SourceKey(reference))
			intent.Sources[i] = image.reference
			intent.SourceDigests[w.SourceKey(image.reference)] = effective
		}
	}
	switch before.(type) {
	case *api.Sandbox:
		intent.Expected.Binding.SandboxUID = before.GetUID()
	case *api.AgentPolicy:
		intent.Expected.Binding.AgentPolicyUID = before.GetUID()
	}
	return intent
}

func TestConfigurationMetadataUpdatesPreserveExecutions(t *testing.T) {
	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		for _, phase := range []Phase{Reserved, Starting, Running, Stopping} {
			t.Run(kind+"/"+string(phase), func(t *testing.T) {
				ctx := context.Background()
				store, storage, before, _ := configurationFixture(t, kind)
				execution := acquire(t, store, configurationIntent(t, before, "metadata"))
				var err error
				if phase != Reserved {
					execution, err = store.BeginStart(ctx, execution)
					if err != nil {
						t.Fatal(err)
					}
				}
				if phase == Running || phase == Stopping {
					execution, err = store.MarkRunning(ctx, execution)
					if err != nil {
						t.Fatal(err)
					}
				}
				if phase == Stopping {
					execution, err = store.BeginStop(ctx, execution)
					if err != nil {
						t.Fatal(err)
					}
				}
				after := before.DeepCopyObject().(client.Object)
				after.SetLabels(map[string]string{"example.com/description": "updated"})
				after.SetAnnotations(map[string]string{"example.com/description": "updated"})
				ticket, err := store.AcquireConfigurationChange(ctx, before, after)
				if err != nil {
					t.Fatal("unchanged effective inputs blocked an existing execution:", err)
				}
				if _, err := store.ReconcileChange(ctx, ticket,
					configurationAdapter(t, storage, storage, before, after)); err != nil {
					t.Fatal("harmless metadata update did not complete:", err)
				}
				if err := store.Check(ctx, execution); err != nil {
					t.Fatal("metadata update changed execution ownership:", err)
				}
			})
		}
	}
}

func TestConfigurationMetadataScopeStillRejectsEffectiveChanges(t *testing.T) {
	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		for _, scenario := range []string{"spec", "control label", "owner", "finalizer", "stale digest", "wrong UID"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				store, _, before, _ := configurationFixture(t, kind)
				intent := configurationIntent(t, before, "metadata")
				if scenario == "stale digest" {
					for _, reference := range intent.Sources {
						if reference.Kind == kind {
							intent.SourceDigests[w.SourceKey(reference)] = strings.Repeat("a", 64)
						}
					}
				}
				if scenario == "wrong UID" {
					for i := range intent.Sources {
						if intent.Sources[i].Kind == kind {
							intent.Sources[i].UID = "previous-uid"
						}
					}
					if kind == "Sandbox" {
						intent.Expected.Binding.SandboxUID = "previous-uid"
					} else {
						intent.Expected.Binding.AgentPolicyUID = "previous-uid"
					}
				}
				acquire(t, store, intent)
				after := before.DeepCopyObject().(client.Object)
				after.SetAnnotations(map[string]string{"example.com/description": "updated"})
				switch scenario {
				case "spec":
					after = desiredConfiguration(before)
				case "control label":
					after.SetLabels(map[string]string{"cetirun.microsoft.com/control": "changed"})
				case "owner":
					after.SetOwnerReferences([]metav1.OwnerReference{w.AgentOwner("other", "other-uid")})
				case "finalizer":
					after.SetFinalizers([]string{"example.com/retain"})
				}
				if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, ErrConflict) {
					t.Fatal("effective inputs or identity drift did not block the update:", err)
				}
			})
		}
	}
}

func TestConfigurationMetadataScopePinsFutureStartsAndWholeWrites(t *testing.T) {
	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, _ := configurationFixture(t, kind)
			after := before.DeepCopyObject().(client.Object)
			after.SetAnnotations(map[string]string{"example.com/description": "approved"})
			ticket := changingConfiguration(t, store, before, after)
			wrong := configurationIntent(t, before, "different")
			for _, reference := range wrong.Sources {
				if reference.Kind == kind {
					wrong.SourceDigests[w.SourceKey(reference)] = strings.Repeat("a", 64)
				}
			}
			if _, err := store.AcquireExecution(ctx, wrong); !errors.Is(err, ErrConflict) {
				t.Fatal("a start with a different effective digest was permitted:", err)
			}
			execution := acquire(t, store, configurationIntent(t, before, "metadata"))
			other := after.DeepCopyObject().(client.Object)
			other.SetAnnotations(map[string]string{"example.com/description": "unapproved"})
			if _, err := store.AcquireConfigurationChange(ctx, before, other); !errors.Is(err, ErrConflict) {
				t.Fatal("another metadata writer acquired the same source:", err)
			}
			if err := store.AuthorizeConfigurationUpdate(ctx, before, other); !errors.Is(err, ErrConflict) {
				t.Fatal("unchanged execution inputs bypassed full-write authorization:", err)
			}
			restarted := Store{Client: store.Client, APIReader: storage, Namespace: store.Namespace, Identity: store.Identity}
			recovered, err := restarted.ReadConfigurationChange(ctx, before, after)
			if err != nil || !reflect.DeepEqual(recovered, ticket) {
				t.Fatal("effective scope was not preserved across recovery:", err)
			}
			adapter := configurationAdapter(t, storage, storage, before, after)
			if err := adapter.Submit(ctx, recovered, func(ctx context.Context) error {
				return restarted.CheckChange(ctx, recovered)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.ReconcileChange(ctx, recovered,
				configurationAdapter(t, nil, storage, before, after)); err != nil {
				t.Fatal("metadata outcome could not recover without another write:", err)
			}
			if err := store.Check(ctx, execution); err != nil {
				t.Fatal("recovery released execution ownership:", err)
			}
		})
	}
}

func TestConfigurationMetadataRecoveryRejectsGenericRecords(t *testing.T) {
	ctx := context.Background()
	store, _, before, _ := configurationFixture(t, "AgentPolicy")
	after := before.DeepCopyObject().(client.Object)
	after.SetAnnotations(map[string]string{"example.com/description": "approved"})
	change, err := newConfigurationChange(before, after, false)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.AcquireChange(ctx, change.before.reference, change.after.digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, ErrConflict) {
		t.Fatal("concrete configuration recovery adopted a generic record:", err)
	}
	if _, err := store.AcquireExecution(ctx, configurationIntent(t, before, "conservative")); !errors.Is(err, ErrConflict) {
		t.Fatal("conservative pending change stopped excluding new executions:", err)
	}
	adapter := &syntheticChangeAdapter{outcome: completeChange}
	if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil {
		t.Fatal("generic low-level record could not complete:", err)
	}
}

func TestConfigurationMetadataScopeRejectsCorruptRecords(t *testing.T) {
	for _, scenario := range []string{"malformed digest", "wrong kind", "wrong version", "namespace", "changed digest"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, _ := configurationFixture(t, "AgentPolicy")
			after := before.DeepCopyObject().(client.Object)
			after.SetAnnotations(map[string]string{"example.com/description": "approved"})
			changingConfiguration(t, store, before, after)
			authority, state, err := store.read(ctx)
			if err != nil || state.Changes[0].UnchangedDigest == "" {
				t.Fatal("metadata reservation did not persist its effective scope:", err)
			}
			switch scenario {
			case "malformed digest":
				state.Changes[0].UnchangedDigest = "invalid"
			case "wrong kind":
				state.Changes[0].Before.Kind = "ConfigMap"
			case "wrong version":
				state.Changes[0].Before.APIVersion = "example.com/v1"
			case "namespace":
				state.Changes[0].Before.Namespace = "different"
			case "changed digest":
				state.Changes[0].UnchangedDigest = strings.Repeat("a", 64)
			}
			if scenario != "changed digest" {
				if err := validateRecord(state); err == nil {
					t.Fatal("invalid scope record was accepted")
				}
				return
			}
			if err := encode(authority, state); err != nil {
				t.Fatal(err)
			}
			if err := storage.Update(ctx, authority); err != nil {
				t.Fatal(err)
			}
			if err := store.AuthorizeConfigurationUpdate(ctx, before, after); !errors.Is(err, ErrConflict) {
				t.Fatal("recorded effective scope did not match the proposed images:", err)
			}
		})
	}
}

func changingConfiguration(t *testing.T, store Store, before, after client.Object) ChangeTicket {
	t.Helper()
	ticket, err := store.AcquireConfigurationChange(context.Background(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = store.BeginChange(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestKubernetesConfigurationUpdate(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, after := configurationFixture(t, kind)
			originalBefore, originalAfter := before.DeepCopyObject(), after.DeepCopyObject()
			updates := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					updates++
					if object.GetUID() != before.GetUID() || object.GetResourceVersion() != before.GetResourceVersion() {
						t.Fatal("Update lost original UID/resourceVersion preconditions")
					}
					options := &client.UpdateOptions{}
					for _, option := range opts {
						option.ApplyToUpdate(options)
					}
					if options.FieldValidation != metav1.FieldValidationStrict {
						t.Fatal("Update did not request strict field validation")
					}
					return c.Update(ctx, object, opts...)
				},
			})
			adapter := configurationAdapter(t, writer, storage, before, after)
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "Team" {
				expected, err := w.TeamMutationDigest(after.(*api.Team))
				if err != nil || ticket.Change.Team == nil || ticket.Change.DesiredDigest != expected {
					t.Fatal("Team update did not reuse the complete mutation digest and scoped reservation:", err)
				}
			}
			completed, err := store.ReconcileChange(ctx, ticket, adapter)
			if err != nil || updates != 1 {
				t.Fatalf("single Update did not complete: updates=%d error=%v", updates, err)
			}
			if err := store.CheckChange(ctx, completed); !errors.Is(err, ErrConflict) {
				t.Fatal("verified outcome did not release its reservation:", err)
			}
			if !reflect.DeepEqual(before, originalBefore) || !reflect.DeepEqual(after, originalAfter) {
				t.Fatal("adapter or store mutated caller images")
			}
			if err := adapter.Submit(ctx, completed, func(context.Context) error { return nil }); !errors.Is(err, ErrConflict) || updates != 1 {
				t.Fatal("same adapter submitted twice:", err)
			}
		})
	}
}

func TestConfigurationTeamAffectedSelections(t *testing.T) {
	store, storage, oldObject, _ := configurationFixture(t, "Team")
	before := oldObject.(*api.Team)
	acquire(t, store, teamIntent(t, before, "existing"))
	after := before.DeepCopy()
	after.Spec.AgentPolicyRef = "new-policy"
	if _, err := store.AcquireConfigurationChange(context.Background(), before, after); !errors.Is(err, ErrConflict) {
		t.Fatal("Team policy mutation acquired a reservation:", err)
	}

	store, storage, oldObject, _ = configurationFixture(t, "Team")
	before = oldObject.(*api.Team)
	acquire(t, store, teamIntent(t, before, "existing"))
	after = before.DeepCopy()
	after.Spec.SandboxRefs = append(after.Spec.SandboxRefs, "another-spare")
	ticket, err := store.AcquireConfigurationChange(context.Background(), before, after)
	if err != nil || ticket.Change.Team == nil {
		t.Fatal("unselected Sandbox mutation was unnecessarily blocked:", err)
	}
	if _, err := store.ReconcileChange(context.Background(), ticket,
		configurationAdapter(t, storage, storage, before, after)); err != nil {
		t.Fatal("unselected Sandbox update failed:", err)
	}
}

func TestConfigurationLostReplyRecoveryNeverResubmits(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, after := configurationFixture(t, kind)
			updates := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					updates++
					if err := c.Update(ctx, object, opts...); err != nil {
						return err
					}
					return errors.New("synthetic lost response after commit")
				},
			})
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			ticket, err = store.ReconcileChange(ctx, ticket, configurationAdapter(t, writer, storage, before, after))
			if err == nil || ticket.Change.Phase != Changing || updates != 1 {
				t.Fatal("uncertain response lost its reservation:", err)
			}
			readOnly := store
			readOnly.Client = nil
			recovered, err := readOnly.ReadConfigurationChange(ctx, before, after)
			if err != nil || !reflect.DeepEqual(recovered, ticket) {
				t.Fatal("read-only lookup failed after source advancement:", err)
			}
			observer := configurationAdapter(t, nil, storage, before, after)
			outcome, err := observer.ObserveChange(ctx, recovered)
			if err != nil || !outcome.Applied || !outcome.NoPendingSubmissions || outcome.Aborted {
				t.Fatal("committed matching content was not observed:", err)
			}
			if err := store.CheckChange(ctx, recovered); err != nil {
				t.Fatal("observation itself released the reservation:", err)
			}
			restarted := Store{Client: store.Client, APIReader: storage, Namespace: store.Namespace, Identity: store.Identity}
			again, err := restarted.AcquireConfigurationChange(ctx, before, after)
			if err != nil || !reflect.DeepEqual(again, recovered) {
				t.Fatal("exact journaled operation could not recover with old input RV:", err)
			}
			if _, err := restarted.ReconcileChange(ctx, again, observer); err != nil || updates != 1 {
				t.Fatal("recovery resubmitted or failed to finish:", err)
			}
		})
	}
}

func TestConfigurationUnknownOutcomesStayLocked(t *testing.T) {
	for _, scenario := range []string{"failed", "never submitted", "missing", "replaced", "wrong spec", "wrong label", "wrong annotation", "wrong owners", "wrong finalizers", "terminating"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, after := configurationFixture(t, "AgentPolicy")
			ticket := changingConfiguration(t, store, before, after)
			updates := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					updates++
					return errors.New("synthetic request failure")
				},
			})
			adapter := configurationAdapter(t, writer, storage, before, after)
			if scenario == "failed" {
				if err := adapter.Submit(ctx, ticket, func(ctx context.Context) error { return store.CheckChange(ctx, ticket) }); err == nil {
					t.Fatal("failed request succeeded")
				}
			} else if scenario == "missing" {
				if err := storage.Delete(ctx, before); err != nil {
					t.Fatal(err)
				}
			} else if scenario != "never submitted" {
				current := after.DeepCopyObject().(*api.AgentPolicy)
				switch scenario {
				case "replaced":
					current.UID = "replacement-uid"
				case "wrong spec":
					current.Spec.EntraID.Enabled = true
				case "wrong label":
					current.Labels["example.com/label"] = "other"
				case "wrong annotation":
					current.Annotations["example.com/change"] = "other"
				case "wrong owners":
					current.OwnerReferences = []metav1.OwnerReference{w.AgentOwner("other", "other-uid")}
				case "wrong finalizers":
					current.Finalizers = []string{"example.com/other"}
				case "terminating":
					current.Finalizers = []string{"example.com/retain"}
				}
				if err := storage.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
				if scenario == "terminating" {
					if err := storage.Delete(ctx, current); err != nil {
						t.Fatal(err)
					}
				}
			}
			recovered, err := store.ReadConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := store.ReconcileChange(ctx, recovered, configurationAdapter(t, writer, storage, before, after)); err == nil {
					t.Fatal("unknown outcome was considered terminal")
				}
				if err := store.CheckChange(ctx, ticket); err != nil {
					t.Fatal("unknown outcome stopped excluding writes:", err)
				}
			}
			expectedUpdates := 0
			if scenario == "failed" {
				expectedUpdates = 1
			}
			if updates != expectedUpdates {
				t.Fatal("observation retried an Update")
			}
			other := desiredConfiguration(before)
			other.SetLabels(map[string]string{"example.com/label": "different"})
			if _, err := store.AcquireConfigurationChange(ctx, before, other); !errors.Is(err, ErrConflict) {
				t.Fatal("uncertain reservation was adopted for another desired write:", err)
			}
		})
	}
}

func TestConfigurationGuardsAndOriginalCAS(t *testing.T) {
	for _, scenario := range []string{"canceled", "initial guard", "final guard", "guard cancels", "authority replaced", "source races"} {
		t.Run(scenario, func(t *testing.T) {
			store, storage, before, after := configurationFixture(t, "Sandbox")
			ticket := changingConfiguration(t, store, before, after)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			updates, guards := 0, 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					updates++
					if scenario == "source races" {
						current := before.DeepCopyObject().(client.Object)
						current.SetLabels(map[string]string{"example.com/race": "different"})
						if err := c.Update(ctx, current); err != nil {
							t.Fatal(err)
						}
					}
					return c.Update(ctx, object, opts...)
				},
			})
			reader := interceptor.NewClient(storage, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					if scenario == "authority replaced" {
						authority, state, err := store.read(ctx)
						if err != nil {
							t.Fatal(err)
						}
						authority.UID = "replacement"
						if err := encode(authority, state); err != nil {
							t.Fatal(err)
						}
						if err := c.Update(ctx, authority); err != nil {
							t.Fatal(err)
						}
					}
					return nil
				},
			})
			if scenario == "canceled" {
				cancel()
			}
			guard := func(ctx context.Context) error {
				guards++
				if (scenario == "initial guard" && guards == 1) || (scenario == "final guard" && guards == 2) {
					return ErrConflict
				}
				if scenario == "guard cancels" && guards == 2 {
					cancel()
					return nil
				}
				return store.CheckChange(ctx, ticket)
			}
			adapter := configurationAdapter(t, writer, reader, before, after)
			err := adapter.Submit(ctx, ticket, guard)
			if err == nil {
				t.Fatal("guard/race failed open")
			}
			if scenario == "source races" {
				if updates != 1 || !apierrors.IsConflict(err) || guards != 2 {
					t.Fatal("stale CAS was rebased or retried:", err)
				}
			} else if updates != 0 {
				t.Fatal("invalidated guard authorized an Update")
			}
		})
	}
}

func TestConfigurationRequiresAuthoritativeBeforeImage(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		for _, field := range []string{"UID", "RV", "generation", "spec", "label", "annotation", "status", "creation"} {
			t.Run(kind+"/"+field, func(t *testing.T) {
				store, _, before, _ := configurationFixture(t, kind)
				switch field {
				case "UID":
					before.SetUID("different")
				case "RV":
					before.SetResourceVersion("different")
				case "generation":
					before.SetGeneration(2)
				case "spec":
					before = desiredConfiguration(before)
				case "label":
					before.SetLabels(map[string]string{"example.com/label": "false"})
				case "annotation":
					before.SetAnnotations(map[string]string{"example.com/change": "false"})
				case "status":
					switch object := before.(type) {
					case *api.Team:
						object.Status.ObservedGeneration = 99
					case *api.Sandbox:
						object.Status.ObservedGeneration = 99
					case *api.AgentPolicy:
						object.Status.ObservedGeneration = 99
					}
				case "creation":
					before.SetCreationTimestamp(metav1.NewTime(time.Unix(1800000000, 0)))
				}
				if _, err := store.AcquireConfigurationChange(context.Background(), before, desiredConfiguration(before)); !errors.Is(err, ErrConflict) {
					t.Fatal("non-authoritative before image acquired a ticket:", err)
				}
				_, state, err := store.read(context.Background())
				if err != nil || len(state.Changes) != 0 {
					t.Fatal("rejected before image changed the journal:", err)
				}
			})
		}
	}
}

func TestConfigurationRejectsServerFieldEditsAndUnsupportedInputs(t *testing.T) {
	store, storage, before, after := configurationFixture(t, "Team")
	for _, field := range []string{"name", "namespace", "UID", "RV", "generation", "creation", "generateName", "selfLink", "deletion", "grace", "managedFields", "status", "kind", "version", "bound"} {
		t.Run(field, func(t *testing.T) {
			changed := after.DeepCopyObject().(*api.Team)
			switch field {
			case "name":
				changed.Name = "another"
			case "namespace":
				changed.Namespace = "another"
			case "UID":
				changed.UID = "another"
			case "RV":
				changed.ResourceVersion = "another"
			case "generation":
				changed.Generation++
			case "creation":
				changed.CreationTimestamp = metav1.NewTime(time.Unix(1800000000, 0))
			case "generateName":
				changed.GenerateName = "generated-"
			case "selfLink":
				changed.SelfLink = "/different"
			case "deletion":
				changed.DeletionTimestamp = ptr.To(metav1.Now())
			case "grace":
				changed.DeletionGracePeriodSeconds = ptr.To(int64(1))
			case "managedFields":
				changed.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "different"}}
			case "status":
				changed.Status.ObservedGeneration++
			case "kind":
				changed.Kind = "Secret"
			case "version":
				changed.APIVersion = "example.com/v1"
			case "bound":
				changed.Annotations["example.com/large"] = strings.Repeat("x", MaxConfigurationBytes)
			}
			if _, err := NewKubernetesChangeAdapter(storage, storage, before, changed); err == nil {
				t.Fatal("invalid input was retained by adapter")
			}
			if _, err := store.AcquireConfigurationChange(context.Background(), before, changed); err == nil {
				t.Fatal("invalid input acquired a ticket")
			}
		})
	}
	var nilTeam *api.Team
	for _, object := range []client.Object{nil, nilTeam, &corev1.Secret{}, &corev1.ConfigMap{}, &api.Agent{},
		&metav1.PartialObjectMetadata{}, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": api.GroupVersion.String(), "kind": "Team"}}} {
		if _, err := NewKubernetesChangeAdapter(storage, storage, object, object); err == nil {
			t.Fatalf("unsupported source %T accepted", object)
		}
	}
	unpersisted := before.DeepCopyObject().(client.Object)
	unpersisted.SetUID("")
	if _, err := NewKubernetesChangeAdapter(storage, storage, unpersisted, unpersisted); err == nil {
		t.Fatal("unpersisted object accepted")
	}
	if _, err := NewKubernetesChangeAdapter(storage, nil, before, after); err == nil {
		t.Fatal("missing reader accepted")
	}
}

func TestConfigurationCopiesInputsAndPinsWholeWrite(t *testing.T) {
	ctx := context.Background()
	store, storage, before, after := configurationFixture(t, "Team")
	team := after.(*api.Team)
	team.Spec.MemberContext.Description = []string{"synthetic desired"}
	team.OwnerReferences = []metav1.OwnerReference{w.AgentOwner("owner", "owner-uid")}
	team.Finalizers = []string{"example.com/retain"}
	ticket := changingConfiguration(t, store, before, after)
	adapter := configurationAdapter(t, storage, storage, before, after)
	desiredDigest := ticket.Change.DesiredDigest
	before.GetLabels()["example.com/label"] = "caller-only"
	team.Spec.MemberContext.Description[0] = "caller-only"
	team.OwnerReferences[0].Name = "caller-only"
	team.Finalizers[0] = "example.com/caller-only"
	team.Annotations["example.com/change"] = "caller-only"
	if err := adapter.Submit(ctx, ticket, func(ctx context.Context) error { return store.CheckChange(ctx, ticket) }); err != nil {
		t.Fatal("caller changed privately retained payload:", err)
	}
	outcome, err := adapter.ObserveChange(ctx, ticket)
	if err != nil || outcome.DesiredDigest != desiredDigest {
		t.Fatal("observed payload was not the original complete desired write:", err)
	}
}

func TestConfigurationTicketTampering(t *testing.T) {
	for _, field := range []string{"authority", "operation", "phase", "before", "digest", "scope before", "scope after", "no scope"} {
		t.Run(field, func(t *testing.T) {
			store, storage, before, after := configurationFixture(t, "Team")
			ticket := changingConfiguration(t, store, before, after)
			altered := ticket.DeepCopy()
			switch field {
			case "authority":
				altered.AuthorityUID = "other"
			case "operation":
				altered.Change.ID = "other"
			case "phase":
				altered.Change.Phase = Reserved
			case "before":
				altered.Change.Before.ResourceVersion = "other"
			case "digest":
				altered.Change.DesiredDigest = strings.Repeat("a", 64)
			case "scope before":
				altered.Change.Team.Before.Policy = "other"
			case "scope after":
				altered.Change.Team.After.Sandboxes[0] = "other"
			case "no scope":
				altered.Change.Team = nil
			}
			if _, err := store.ReconcileChange(context.Background(), altered,
				configurationAdapter(t, storage, storage, before, after)); err == nil {
				t.Fatal("foreign ticket was accepted")
			}
			if err := store.CheckChange(context.Background(), ticket); err != nil {
				t.Fatal("altered ticket damaged reservation:", err)
			}
		})
	}
}

func TestConfigurationReadOnlyAuthorization(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, _, before, after := configurationFixture(t, kind)
			readOnly := store
			readOnly.Client = nil
			if err := readOnly.AuthorizeConfigurationUpdate(ctx, before, after); !errors.Is(err, ErrConflict) {
				t.Fatal("unknown write was authorized:", err)
			}
			if _, err := readOnly.ReadConfigurationChange(ctx, before, after); !errors.Is(err, ErrUnavailable) {
				t.Fatal("read-only lookup acquired a ticket:", err)
			}
			if _, err := readOnly.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, ErrUnavailable) {
				t.Fatal("missing writer acquired a new reservation:", err)
			}
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			if err := readOnly.AuthorizeConfigurationUpdate(ctx, before, after); !errors.Is(err, ErrConflict) {
				t.Fatal("reserved ticket authorized an Update:", err)
			}
			if _, err := readOnly.BeginChange(ctx, ticket); !errors.Is(err, ErrUnavailable) {
				t.Fatal("read-only store advanced a ticket:", err)
			}
			ticket, err = store.BeginChange(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			if err := readOnly.AuthorizeConfigurationUpdate(ctx, before, after); err != nil {
				t.Fatal("exact changing write was denied with a read-only Store:", err)
			}
			admitted := after.DeepCopyObject().(client.Object)
			admitted.SetGeneration(before.GetGeneration() + 1)
			admitted.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "configuration-test",
				Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: api.GroupVersion.String(),
				FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{}}`)}}})
			if err := readOnly.AuthorizeConfigurationUpdate(ctx, before, admitted); err != nil {
				t.Fatal("API-managed generation/field ownership prevented admission:", err)
			}
			admitted.SetGeneration(before.GetGeneration() + 2)
			if err := readOnly.AuthorizeConfigurationUpdate(ctx, before, admitted); !errors.Is(err, ErrConflict) {
				t.Fatal("unexpected generation was allowed:", err)
			}
			tampered := after.DeepCopyObject().(client.Object)
			tampered.SetAnnotations(map[string]string{"example.com/not-an-effective-control": "unapproved"})
			if err := readOnly.AuthorizeConfigurationUpdate(ctx, before, tampered); !errors.Is(err, ErrConflict) {
				t.Fatal("non-effective metadata escaped the whole-write digest:", err)
			}
			if err := readOnly.ReleaseChange(ctx, ticket); !errors.Is(err, ErrUnavailable) {
				t.Fatal("read-only Store released a ticket:", err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if err := readOnly.AuthorizeConfigurationUpdate(canceled, before, after); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled admission was authorized:", err)
			}
		})
	}
}

func TestConfigurationAuthorizationRejectsAuthorityAndSourceDrift(t *testing.T) {
	for _, scenario := range []string{"reader missing", "authority missing", "authority replaced", "corrupt", "digest", "scope before", "scope after", "stale source", "replaced source", "before scope", "after scope"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, after := configurationFixture(t, "Team")
			changingConfiguration(t, store, before, after)
			switch scenario {
			case "reader missing":
				store.APIReader = nil
			case "authority missing":
				store.Identity.Name = "missing"
			case "authority replaced":
				store.Identity.UID = "other"
			case "corrupt", "digest", "scope before", "scope after":
				authority, state, err := store.read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "digest":
					state.Changes[0].DesiredDigest = strings.Repeat("a", 64)
				case "scope before":
					state.Changes[0].Team.Before.Policy = "other"
				case "scope after":
					state.Changes[0].Team.After.Policy = "other"
				}
				if err := encode(authority, state); err != nil {
					t.Fatal(err)
				}
				if scenario == "corrupt" {
					authority.Data[DataKey] = `{"version":1,"changes":null,"executions":[]}`
				}
				if err := storage.Update(ctx, authority); err != nil {
					t.Fatal(err)
				}
			case "stale source", "replaced source":
				current := before.DeepCopyObject().(client.Object)
				if scenario == "replaced source" {
					current.SetUID("replacement")
				}
				current.SetAnnotations(map[string]string{"example.com/later": "changed"})
				if err := storage.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
			case "before scope":
				before.(*api.Team).Spec.AgentPolicyRef = "other"
			case "after scope":
				after.(*api.Team).Spec.AgentPolicyRef = "other"
			}
			store.Client = nil
			if err := store.AuthorizeConfigurationUpdate(ctx, before, after); err == nil {
				t.Fatal("invalid authority/source authorized an update")
			}
		})
	}
}

func TestConfigurationEmptyMetadataAndMetadataOnlyGeneration(t *testing.T) {
	ctx := context.Background()
	store, storage, before, _ := configurationFixture(t, "Team")
	after := before.DeepCopyObject().(client.Object)
	after.SetLabels(map[string]string{})
	after.SetAnnotations(map[string]string{})
	after.SetOwnerReferences([]metav1.OwnerReference{})
	after.SetFinalizers([]string{})
	ticket := changingConfiguration(t, store, before, after)
	admitted := after.DeepCopyObject().(client.Object)
	admitted.SetGeneration(before.GetGeneration() + 1)
	if err := store.AuthorizeConfigurationUpdate(ctx, before, admitted); err != nil {
		t.Fatal("exact admission increment was rejected for an effective metadata-only update:", err)
	}
	if err := store.AuthorizeConfigurationUpdate(ctx, before, after); err != nil {
		t.Fatal(err)
	}
	adapter := configurationAdapter(t, storage, storage, before, after)
	if err := adapter.Submit(ctx, ticket, func(ctx context.Context) error { return store.CheckChange(ctx, ticket) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil {
		t.Fatal("empty metadata could not round trip:", err)
	}
}

func TestConfigurationMetadataAdmissionGenerationBounds(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		for _, tc := range []struct {
			name          string
			before, after int64
			allowed       bool
		}{
			{"unchanged", 1, 1, true},
			{"increment", 1, 2, true},
			{"zero increment", 0, 1, true},
			{"regression", 1, 0, false},
			{"jump", 1, 3, false},
			{"maximum unchanged", math.MaxInt64, math.MaxInt64, true},
			{"maximum regression", math.MaxInt64, math.MaxInt64 - 1, false},
			{"overflow", math.MaxInt64, math.MinInt64, false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				store, storage, before, _ := configurationFixture(t, kind)
				before.SetGeneration(tc.before)
				if err := storage.Update(ctx, before); err != nil {
					t.Fatal(err)
				}
				after := before.DeepCopyObject().(client.Object)
				after.SetLabels(map[string]string{"example.com/change": "metadata"})
				ticket := changingConfiguration(t, store, before, after)
				admitted := after.DeepCopyObject().(client.Object)
				admitted.SetGeneration(tc.after)
				store.Client = nil
				if err := store.AuthorizeConfigurationUpdate(ctx, before, admitted); (err == nil) != tc.allowed {
					t.Fatalf("admission generation %d -> %d: allowed=%t err=%v", tc.before, tc.after, tc.allowed, err)
				}
				if tc.after != tc.before {
					if _, err := NewKubernetesChangeAdapter(storage, storage, before, admitted); err == nil {
						t.Fatal("file/adapter inputs were allowed to alter server generation")
					}
				}
				if tc.allowed {
					admitted.SetLabels(map[string]string{"example.com/change": "not-reserved"})
					if err := store.AuthorizeConfigurationUpdate(ctx, before, admitted); !errors.Is(err, ErrConflict) {
						t.Fatal("generation normalization authorized an unreserved mutation:", err)
					}
				}
				if err := store.CheckChange(ctx, ticket); err != nil {
					t.Fatal("admission changed the reservation:", err)
				}
			})
		}
	}
}

func TestConfigurationMetadataRequestRetainsAuthoritativeSpec(t *testing.T) {
	for _, editSpec := range []bool{false, true} {
		name := "metadata-only"
		if editSpec {
			name = "actual-spec-edit"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, _ := configurationFixture(t, "AgentPolicy")
			after := before.DeepCopyObject().(*api.AgentPolicy)
			after.Labels = map[string]string{"example.com/change": "metadata"}
			after.Annotations = map[string]string{"example.com/description": "updated"}
			after.Finalizers = []string{"example.com/retain"}
			after.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(),
				Kind: "Team", Name: "owner", UID: "owner-uid"}}
			if editSpec {
				after.Spec.ModelAccess.Copilot.Allowed = true
			}
			reader := interceptor.NewClient(storage, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					if raw, ok := object.(*unstructured.Unstructured); ok && raw.GetKind() == "AgentPolicy" {
						unstructured.RemoveNestedField(raw.Object, "spec", "agentIdentity")
					}
					return nil
				},
			})
			var submitted client.Object
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					submitted = object.DeepCopyObject().(client.Object)
					return c.Update(ctx, object, opts...)
				},
			})
			store.APIReader = reader
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			adapter := configurationAdapter(t, writer, reader, before, after)
			if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil {
				t.Fatal(err)
			}
			if submitted == nil || submitted.GetUID() != before.GetUID() ||
				submitted.GetResourceVersion() != before.GetResourceVersion() || submitted.GetGeneration() != before.GetGeneration() ||
				!reflect.DeepEqual(submitted.GetLabels(), after.Labels) ||
				!reflect.DeepEqual(submitted.GetAnnotations(), after.Annotations) ||
				!reflect.DeepEqual(submitted.GetFinalizers(), after.Finalizers) ||
				!reflect.DeepEqual(submitted.GetOwnerReferences(), after.OwnerReferences) {
				t.Fatal("wire preservation changed reserved metadata or original conditional-write identities")
			}
			encoded, err := json.Marshal(submitted)
			if err != nil {
				t.Fatal(err)
			}
			raw := &unstructured.Unstructured{}
			if err := raw.UnmarshalJSON(encoded); err != nil {
				t.Fatal(err)
			}
			if editSpec {
				value, found, err := unstructured.NestedBool(raw.Object, "spec", "modelAccess", "copilot", "allowed")
				if err != nil || !found || !value {
					t.Fatal("wire preservation discarded an actual requested spec edit:", err)
				}
			} else if _, found, err := unstructured.NestedFieldNoCopy(raw.Object, "spec", "agentIdentity"); err != nil || found {
				t.Fatal("metadata-only update materialized an omitted zero-value object:", err)
			}
		})
	}
}

func TestConfigurationManagedFieldsFormattingPreservesUpdates(t *testing.T) {
	const compact = `{"f:metadata":{"f:labels":{"f:example.com/label":{}}},"f:spec":{}}`
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(compact), "", "  "); err != nil {
		t.Fatal(err)
	}
	const reordered = `{"f:spec":{},"f:metadata":{"f:labels":{"f:example.com/label":{}}}}`
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		for _, format := range []struct{ name, before, after string }{
			{"pretty", pretty.String(), pretty.String()},
			{"reordered", reordered, reordered},
			{"different formatting", pretty.String(), compact},
		} {
			t.Run(kind+"/"+format.name, func(t *testing.T) {
				ctx := context.Background()
				store, storage, before, _ := configurationFixture(t, kind)
				before.SetManagedFields([]metav1.ManagedFieldsEntry{{
					Manager: "example-controller", Operation: metav1.ManagedFieldsOperationUpdate,
					APIVersion: api.GroupVersion.String(), FieldsType: "FieldsV1",
					FieldsV1: &metav1.FieldsV1{Raw: []byte(compact)},
				}})
				authority := &corev1.ConfigMap{}
				if err := storage.Get(ctx, client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}, authority); err != nil {
					t.Fatal(err)
				}
				storage = fake.NewClientBuilder().WithScheme(storage.Scheme()).
					WithObjects(authority, before).WithReturnManagedFields().Build()
				store.Client, store.APIReader = storage, storage
				before.GetManagedFields()[0].FieldsV1.Raw = []byte(format.before)
				after := desiredConfiguration(before)
				after.GetManagedFields()[0].FieldsV1.Raw = []byte(format.after)
				originalBefore, originalAfter := before.DeepCopyObject(), after.DeepCopyObject()
				ticket, err := store.AcquireConfigurationChange(ctx, before, after)
				if err != nil {
					t.Fatal("equivalent managed fields rejected:", err)
				}
				adapter := configurationAdapter(t, storage, storage, before, after)
				if string(adapter.change.before.object.GetManagedFields()[0].FieldsV1.Raw) != format.before ||
					string(adapter.change.after.object.GetManagedFields()[0].FieldsV1.Raw) != format.after {
					t.Fatal("comparison normalization rewrote the retained submission images")
				}
				if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil {
					t.Fatal("equivalent managed fields prevented the guarded update:", err)
				}
				_, state, err := store.read(ctx)
				if err != nil || len(state.Changes) != 0 {
					t.Fatal("completed update retained its reservation:", err)
				}
				if !reflect.DeepEqual(before, originalBefore) || !reflect.DeepEqual(after, originalAfter) {
					t.Fatal("canonicalization changed caller-owned inputs")
				}
			})
		}
	}
}

func TestConfigurationManagedFieldsContentRemainsProtected(t *testing.T) {
	for _, change := range []string{"field set", "manager", "operation", "apiVersion", "fieldsType", "time", "subresource", "removed"} {
		t.Run(change, func(t *testing.T) {
			store, storage, before, _ := configurationFixture(t, "Sandbox")
			before.SetManagedFields([]metav1.ManagedFieldsEntry{{
				Manager: "example-controller", Operation: metav1.ManagedFieldsOperationUpdate,
				APIVersion: api.GroupVersion.String(), FieldsType: "FieldsV1",
				Time:     ptr.To(metav1.NewTime(time.Unix(1700000000, 0))),
				FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{}}`)},
			}})
			after := desiredConfiguration(before)
			field := &after.GetManagedFields()[0]
			switch change {
			case "field set":
				field.FieldsV1.Raw = []byte(`{"f:status":{}}`)
			case "manager":
				field.Manager = "different-controller"
			case "operation":
				field.Operation = metav1.ManagedFieldsOperationApply
			case "apiVersion":
				field.APIVersion = "example.com/v1"
			case "fieldsType":
				field.FieldsType = "different"
			case "time":
				field.Time = ptr.To(metav1.NewTime(time.Unix(1800000000, 0)))
			case "subresource":
				field.Subresource = "status"
			case "removed":
				after.SetManagedFields(nil)
			}
			if _, err := NewKubernetesChangeAdapter(storage, storage, before, after); !errors.Is(err, ErrConflict) {
				t.Fatal("server metadata change was not rejected:", err)
			}
			if _, err := store.AcquireConfigurationChange(context.Background(), before, after); !errors.Is(err, ErrConflict) {
				t.Fatal("server metadata change acquired a reservation:", err)
			}
		})
	}
}

func TestConfigurationManagedFieldsRejectAmbiguousJSON(t *testing.T) {
	for _, raw := range []string{"null", "[]", `"value"`, "{", `{"f:spec":{},"f:spec":{}}`, `{"f:spec":{"f:value":{},"f:value":{}}}`} {
		t.Run(raw, func(t *testing.T) {
			_, _, before, _ := configurationFixture(t, "Sandbox")
			before.SetManagedFields([]metav1.ManagedFieldsEntry{{
				Manager: "example-controller", FieldsType: "FieldsV1",
				FieldsV1: &metav1.FieldsV1{Raw: []byte(raw)},
			}})
			if _, err := newConfigurationImage(before); !errors.Is(err, ErrUnavailable) {
				t.Fatal("ambiguous or non-object field set was accepted:", err)
			}
		})
	}
}

func TestConfigurationRequiresGuardAndWriterOnlyForSubmission(t *testing.T) {
	ctx := context.Background()
	store, storage, before, after := configurationFixture(t, "Sandbox")
	ticket := changingConfiguration(t, store, before, after)
	guard := func(ctx context.Context) error { return store.CheckChange(ctx, ticket) }
	observer := configurationAdapter(t, nil, storage, before, after)
	if err := observer.Submit(ctx, ticket, guard); !errors.Is(err, ErrUnavailable) {
		t.Fatal("read-only adapter submitted an Update:", err)
	}
	adapter := configurationAdapter(t, storage, storage, before, after)
	if err := adapter.Submit(ctx, ticket, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unguarded adapter submitted an Update:", err)
	}
	var missing *KubernetesChangeAdapter
	if _, err := missing.ObserveChange(ctx, ticket); !errors.Is(err, ErrUnavailable) {
		t.Fatal("nil adapter was usable:", err)
	}
	if _, err := adapter.ObserveChange(ctx, ChangeTicket{}); !errors.Is(err, ErrConflict) {
		t.Fatal("observation adopted an unknown ticket:", err)
	}
	if err := store.CheckChange(ctx, ticket); err != nil {
		t.Fatal("rejected submissions damaged the reservation:", err)
	}
}

func TestConfigurationRejectsUnknownAuthoritativeContent(t *testing.T) {
	for _, field := range []string{"spec", "status", "metadata", "bound"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, after := configurationFixture(t, "AgentPolicy")
			reader := interceptor.NewClient(storage, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					if live, ok := object.(*unstructured.Unstructured); ok {
						var err error
						if field == "bound" {
							err = unstructured.SetNestedField(live.Object, strings.Repeat("x", MaxConfigurationBytes), "metadata", "annotations", "example.com/large")
						} else {
							err = unstructured.SetNestedField(live.Object, "future field", field, "unrecognized")
						}
						if err != nil {
							t.Fatal(err)
						}
					}

					return nil
				},
			})
			checked := store
			checked.APIReader = reader
			if _, err := checked.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, ErrUnavailable) {
				t.Fatal("lossy typed read accepted unknown authoritative content:", err)
			}
			ticket := changingConfiguration(t, store, before, after)
			if err := checked.AuthorizeConfigurationUpdate(ctx, before, after); !errors.Is(err, ErrUnavailable) {
				t.Fatal("admission ignored unknown authoritative content:", err)
			}
			adapter := configurationAdapter(t, storage, reader, before, after)
			if err := adapter.Submit(ctx, ticket, func(ctx context.Context) error { return store.CheckChange(ctx, ticket) }); !errors.Is(err, ErrUnavailable) {
				t.Fatal("submission ignored unknown authoritative content:", err)
			}
			if err := storage.Update(ctx, after.DeepCopyObject().(client.Object)); err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.ObserveChange(ctx, ticket); !errors.Is(err, ErrUnavailable) {
				t.Fatal("recovery ignored unknown authoritative content:", err)
			}
			if err := store.CheckChange(ctx, ticket); err != nil {
				t.Fatal("unknown content released the reservation:", err)
			}
		})
	}
}

func TestConfigurationGenerationIsAuthoritativelyPinned(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind+"/persisted zero", func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, _ := configurationFixture(t, kind)
			before.SetGeneration(0)
			if err := storage.Update(ctx, before); err != nil {
				t.Fatal(err)
			}
			after := desiredConfiguration(before)
			ticket := changingConfiguration(t, store, before, after)
			readOnly := store
			readOnly.Client = nil
			if err := readOnly.AuthorizeConfigurationUpdate(ctx, before, after); err != nil {
				t.Fatal("authoritative zero-generation fixture was rejected:", err)
			}
			admitted := after.DeepCopyObject().(client.Object)
			admitted.SetGeneration(1)
			if err := readOnly.AuthorizeConfigurationUpdate(ctx, before, admitted); err != nil {
				t.Fatal("exact admission increment was rejected:", err)
			}
			if _, err := NewKubernetesChangeAdapter(storage, storage, before, admitted); !errors.Is(err, ErrConflict) {
				t.Fatal("file/adapter inputs could change generation:", err)
			}
			adapter := configurationAdapter(t, storage, storage, before, after)
			if err := adapter.Submit(ctx, ticket, func(ctx context.Context) error { return store.CheckChange(ctx, ticket) }); err != nil {
				t.Fatal("zero-generation fake persisted object could not be updated:", err)
			}
			if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil {
				t.Fatal("zero-generation fake persisted object could not be observed:", err)
			}
		})
		t.Run(kind+"/omitted nonzero", func(t *testing.T) {
			ctx := context.Background()
			store, _, before, after := configurationFixture(t, kind)
			forgedBefore, forgedAfter := before.DeepCopyObject().(client.Object), after.DeepCopyObject().(client.Object)
			forgedBefore.SetGeneration(0)
			forgedAfter.SetGeneration(0)
			if _, err := store.AcquireConfigurationChange(ctx, forgedBefore, forgedAfter); !errors.Is(err, ErrConflict) {
				t.Fatal("omitting the real generation bypassed authoritative comparison:", err)
			}
			changingConfiguration(t, store, before, after)
			store.Client = nil
			if err := store.AuthorizeConfigurationUpdate(ctx, forgedBefore, forgedAfter); !errors.Is(err, ErrConflict) {
				t.Fatal("admission accepted a non-authoritative zero generation:", err)
			}
			forgedBefore.SetGeneration(-1)
			forgedAfter.SetGeneration(-1)
			if _, err := store.ReadConfigurationChange(ctx, forgedBefore, forgedAfter); !errors.Is(err, ErrUnavailable) {
				t.Fatal("negative generation was accepted:", err)
			}
		})
	}
}
