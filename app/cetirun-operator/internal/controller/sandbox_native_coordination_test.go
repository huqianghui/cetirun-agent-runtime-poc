// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func nativeCoordinationFixture(t *testing.T) (*nativeSandboxFixture, *w.Expectation) {
	t.Helper()
	ctx := context.Background()
	f := newNativeSandboxFixture(t, true)
	f.prepare(t)
	_, expected := f.expectation(t)
	expected.SourceDigests = map[string]string{}
	for _, source := range expected.Sources {
		if !w.EffectiveSource(source) {
			continue
		}
		value, err := f.r.nativeSourceDigest(ctx, source, f.agent.Spec.SandboxRef, true)
		if err != nil {
			t.Fatal(err)
		}
		expected.SourceDigests[w.SourceKey(source)] = value
	}
	authority, err := sandboxcoordination.NewState(f.r.OperatorNamespace, "native-coordination")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Create(ctx, authority); err != nil {
		t.Fatal(err)
	}
	f.r.SandboxNativeCoordination = &sandboxcoordination.Store{
		Client: f.kube, APIReader: f.kube, Namespace: authority.Namespace,
		Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID},
	}
	return f, expected
}

func nativePolicyChange(t *testing.T, f *nativeSandboxFixture, expected *w.Expectation) (*api.AgentPolicy, *api.AgentPolicy) {
	t.Helper()
	intent, err := sandboxcoordination.NativeIntentFor(*expected)
	if err != nil {
		t.Fatal(err)
	}
	before := &api.AgentPolicy{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: intent.Sources[2].Name}, before); err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
	return before, after
}

func TestNativeCoordinationPendingMutationBlocksStartupReservation(t *testing.T) {
	for _, phase := range []sandboxcoordination.Phase{sandboxcoordination.Reserved, sandboxcoordination.Changing} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			f, expected := nativeCoordinationFixture(t)
			before, after := nativePolicyChange(t, f, expected)
			store := f.r.SandboxNativeCoordination
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			if phase == sandboxcoordination.Changing {
				if _, err := store.BeginChange(ctx, ticket); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.r.reserveNativeSandboxSources(ctx, f.current(t), expected); !errors.Is(err, sandboxcoordination.ErrConflict) {
				t.Fatal("pending mutation did not block native startup:", err)
			}
			if err := f.r.checkNativeSandboxReservation(ctx, expected); !errors.Is(err, sandboxcoordination.ErrConflict) {
				t.Fatal("missing native reservation passed the start boundary:", err)
			}
			receipt, _, err := f.r.readSandboxPreparationReceipt(ctx, f.current(t))
			if err != nil || receipt.Annotations[nativeStartAnnotation] != "" ||
				ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || !expected.ExecutionHeld {
				t.Fatal("failed acquisition submitted native execution:", err)
			}
		})
	}
}

func TestNativeCoordinationRechecksACompletedMutationBeforeStart(t *testing.T) {
	ctx := context.Background()
	f, expected := nativeCoordinationFixture(t)
	before, after := nativePolicyChange(t, f, expected)
	store := f.r.SandboxNativeCoordination
	change, err := store.AcquireConfigurationChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(f.kube, f.kube, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileChange(ctx, change, adapter); err != nil {
		t.Fatal(err)
	}
	if err := f.r.reserveNativeSandboxSources(ctx, f.current(t), expected); !errors.Is(err, errSandboxSourceChanged) {
		t.Fatal("capture/acquisition race authorized stale sources:", err)
	}
	ticket, found, err := store.ReadNative(ctx, expected.Agent.UID)
	if err != nil || !found || ticket.Reservation.Phase != sandboxcoordination.Reserved {
		t.Fatal("failed source recheck discarded its reservation:", err)
	}
	if err := f.r.stopNativeSandboxReservation(ctx, f.current(t)); err != nil {
		t.Fatal("unstarted held lifetime could not finish observed quiescence:", err)
	}
	if _, found, err := store.ReadNative(ctx, expected.Agent.UID); err != nil || found {
		t.Fatal("quiescent failed startup retained its reservation:", err)
	}
}

func TestNativeCoordinationStopWaitsForObservedQuiescence(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		name := "hold"
		if deleting {
			name = "delete"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, expected := nativeCoordinationFixture(t)
			if err := f.r.reserveNativeSandboxSources(ctx, f.current(t), expected); err != nil {
				t.Fatal(err)
			}
			before, after := nativePolicyChange(t, f, expected)
			store := f.r.SandboxNativeCoordination
			if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
				t.Fatal("selected mutation overlapped the native reservation:", err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Namespace: expected.Namespace.Name, Name: "terminating-native-pod",
				Finalizers: []string{"example.com/ordinary-termination"},
			}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			if err := f.kube.Create(ctx, pod); err != nil {
				t.Fatal(err)
			}
			if deleting {
				if err := f.kube.Delete(ctx, f.current(t)); err != nil {
					t.Fatal(err)
				}
			}
			// Disabling the opt-in must not discard existing cleanup obligations.
			f.r.SandboxPreparation.NativeExecution = false
			for _, terminate := range []bool{false, true} {
				if terminate {
					if err := f.kube.Delete(ctx, pod); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.r.stopSandboxBuiltin(ctx, f.current(t)); err == nil {
					t.Fatal("visible or terminating Pod was treated as quiescence")
				}
				if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
					t.Fatal("zero replicas or Pod deletion released the native reservation:", err)
				}
			}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
				t.Fatal(err)
			}
			pod.Finalizers = nil
			if err := f.kube.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Fatal("fixture did not complete ordinary Pod termination:", err)
			}
			if err := f.r.stopSandboxBuiltin(ctx, f.current(t)); err != nil {
				t.Fatal("quiescent native cleanup could not release:", err)
			}
			if err := f.r.stopSandboxBuiltin(ctx, f.current(t)); err != nil {
				t.Fatal("completed release was not idempotent:", err)
			}
			if _, err := store.AcquireConfigurationChange(ctx, before, after); err != nil {
				t.Fatal("selected mutation stayed blocked after quiescence:", err)
			}
		})
	}
}

func TestNativeCoordinationCannotReleaseWithRunningOrUnobservedResources(t *testing.T) {
	for _, failure := range []string{"running workload", "Pod list outage", "replaced HOME", "missing namespace"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f, expected := nativeCoordinationFixture(t)
			if err := f.r.reserveNativeSandboxSources(ctx, f.current(t), expected); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "running workload":
				workload := f.workload(t)
				workload.Spec.Replicas = ptr.To[int32](1)
				if err := f.kube.Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
			case "Pod list outage":
				f.r.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
					List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
						return errors.New("synthetic Pod-list outage")
					},
				})
			case "replaced HOME":
				home := &corev1.PersistentVolumeClaim{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Home.Name}, home); err != nil {
					t.Fatal(err)
				}
				home.UID = "replacement-home-uid"
				if err := f.kube.Update(ctx, home); err != nil {
					t.Fatal(err)
				}
			case "missing namespace":
				f.r.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if _, ok := object.(*corev1.Namespace); ok {
							return apierrors.NewNotFound(corev1.Resource("namespaces"), key.Name)
						}
						return c.Get(ctx, key, object, options...)
					},
				})
			}
			if err := f.r.stopNativeSandboxReservation(ctx, f.current(t)); err == nil {
				t.Fatal("unconfirmed quiescence released configuration")
			}
			store := f.r.SandboxNativeCoordination
			ticket, found, err := store.ReadNative(ctx, expected.Agent.UID)
			if err != nil || !found || ticket.Reservation.Phase != sandboxcoordination.Stopping {
				t.Fatal("failed quiescence lost the stop reservation:", err)
			}
			if err := f.r.checkNativeSandboxReservation(ctx, expected); !errors.Is(err, sandboxcoordination.ErrConflict) {
				t.Fatal("stopping native lifetime could restart:", err)
			}
		})
	}
}

func TestNativeCoordinationChecksNeverAcquireOrRepair(t *testing.T) {
	ctx := context.Background()
	f, expected := nativeCoordinationFixture(t)
	if err := f.r.checkNativeSandboxReservation(ctx, expected); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("missing reservation was reconstructed by validation:", err)
	}
	if err := f.r.reserveNativeSandboxSources(ctx, f.current(t), expected); err != nil {
		t.Fatal(err)
	}
	store := f.r.SandboxNativeCoordination
	store.Client = nil
	if err := f.r.checkNativeSandboxReservation(ctx, expected); err != nil {
		t.Fatal("read-only native startup check required writes:", err)
	}
	stale := *expected
	stale.Revision = "different-revision"
	if err := f.r.checkNativeSandboxReservation(ctx, &stale); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("stale reservation passed startup validation:", err)
	}
	store.Identity.UID = "replacement-authority"
	if err := f.r.checkNativeSandboxReservation(ctx, expected); !errors.Is(err, sandboxcoordination.ErrUnavailable) {
		t.Fatal("replaced coordination authority passed startup validation:", err)
	}
}

func TestNativeCoordinationConfigurationUsesOnlyExplicitAuthority(t *testing.T) {
	ctx := context.Background()
	disabled := &AgentReconciler{}
	if err := disabled.reserveNativeSandboxSources(ctx, nil, nil); err != nil {
		t.Fatal("unconfigured default acquired native coordination:", err)
	}
	if err := disabled.checkNativeSandboxReservation(ctx, nil); err != nil {
		t.Fatal("unconfigured default required native coordination:", err)
	}
	if err := disabled.stopNativeSandboxReservation(ctx, nil); err != nil {
		t.Fatal("unconfigured default changed cleanup:", err)
	}
	store, err := NewNativeSandboxCoordination(SandboxSourcesConfig{}, nil, nil, "")
	if err != nil || store != nil {
		t.Fatal("default factory installed a coordinator:", err)
	}
	config, err := ParseSandboxSourcesConfig(strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`,
		`"enabled":true,"coordinationConfigMapName":"coordination","coordinationConfigMapUID":"authority-uid"`, 1))
	if err != nil {
		t.Fatal(err)
	}
	_, kube, _ := aiGatewayTestReconciler(t)
	// NativeExecution is false, but cleanup still needs the configured authority.
	store, err = NewNativeSandboxCoordination(config, kube, kube, "operator-state")
	if err != nil || store == nil || store.Client != kube || store.APIReader != kube ||
		store.Namespace != "operator-state" || store.Identity != (w.ObjectIdentity{Name: "coordination", UID: "authority-uid"}) {
		t.Fatalf("factory did not preserve the existing authority identity: %+v %v", store, err)
	}
	if _, err := NewNativeSandboxCoordination(config, nil, kube, "operator-state"); err == nil {
		t.Fatal("factory accepted a missing controller writer")
	}
	if _, err := NewNativeSandboxCoordination(config, kube, nil, "operator-state"); err == nil {
		t.Fatal("factory accepted a missing authoritative reader")
	}
	for _, raw := range []string{
		`{"coordinationConfigMapName":"coordination","coordinationConfigMapUID":"authority-uid"}`,
		strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"coordinationConfigMapName":"coordination"`, 1),
		strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"coordinationConfigMapUID":"authority-uid"`, 1),
		strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"coordinationConfigMapName":"Bad/Name","coordinationConfigMapUID":"authority-uid"`, 1),
		strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"coordinationConfigMapName":"sandbox-platform","coordinationConfigMapUID":"authority-uid"`, 1),
		strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"coordinationConfigMapName":"coordination","coordinationConfigMapUID":" uid "`, 1),
	} {
		if _, err := ParseSandboxSourcesConfig(raw); err == nil {
			t.Fatal("invalid coordination source configuration was accepted")
		}
	}
}
