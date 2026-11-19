// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type nativeAdmissionReader struct{ client.Reader }

func coordinatedNativePodFixture(t *testing.T) (podFixture, sandboxcoordination.Store, sandboxcoordination.NativeIntent) {
	t.Helper()
	f := nativePodFixture(t)
	authority, err := sandboxcoordination.NewState(f.state.Namespace, "coordination")
	if err != nil {
		t.Fatal(err)
	}
	authority.UID = "authority-uid"
	if err := f.client.Create(context.Background(), authority); err != nil {
		t.Fatal(err)
	}
	store := sandboxcoordination.Store{Client: f.client, APIReader: f.client, Namespace: authority.Namespace,
		Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID}}
	f.handler, err = NewPodHandler(nativeAdmissionReader{f.client}, PodConfig{
		StateNamespace: f.state.Namespace, CoordinationConfigMapName: authority.Name, CoordinationConfigMapUID: string(authority.UID),
	})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := sandboxcoordination.NativeIntentFor(f.expected)
	if err != nil {
		t.Fatal(err)
	}
	return f, store, intent
}

func TestNativePodAdmissionRequiresCurrentReservationAndNeverWrites(t *testing.T) {
	for _, scenario := range []string{"current", "missing", "stale revision", "stale workload", "stopping", "pending mutation", "unavailable", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f, store, intent := coordinatedNativePodFixture(t)
			if scenario != "missing" && scenario != "pending mutation" {
				selected := intent
				switch scenario {
				case "stale revision":
					selected.Revision = "stale-revision"
				case "stale workload":
					selected.Workload.UID = "old-workload-uid"
				}
				ticket, err := store.AcquireNative(ctx, selected)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "stopping" {
					if _, err := store.BeginNativeStop(ctx, ticket); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "pending mutation" {
				change, err := store.AcquireChange(ctx, intent.Sources[1], strings.Repeat("a", 64))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.BeginChange(ctx, change); err != nil {
					t.Fatal(err)
				}
			}
			key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
			before := &corev1.ConfigMap{}
			if err := f.client.Get(ctx, key, before); err != nil {
				t.Fatal(err)
			}
			if scenario == "malformed" {
				before.Data[sandboxcoordination.DataKey] = `{"version":1,"executions":[],"changes":[],"nativeReservations":[{}]}`
				if err := f.client.Update(ctx, before); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unavailable" {
				f.handler.coordination.Identity.UID = "replaced-authority"
			}
			if f.handler.coordination.Client != nil {
				t.Fatal("admission was given an authority writer")
			}
			response := f.handler.Validate(ctx, podRequest(t, f.pod, admissionv1.Create, nil))
			if response.Allowed != (scenario == "current") {
				t.Fatalf("native reservation admission allowed=%v: %+v", response.Allowed, response.Result)
			}
			after := &corev1.ConfigMap{}
			if err := f.client.Get(ctx, key, after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("Pod admission modified coordination state")
			}
			currentState := &corev1.ConfigMap{}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.state), currentState); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.state, currentState) {
				t.Fatal("Pod admission modified its expectation")
			}
		})
	}
}

func TestNativePodAdmissionRechecksReservationAfterSourceReads(t *testing.T) {
	ctx := context.Background()
	f, store, intent := coordinatedNativePodFixture(t)
	ticket, err := store.AcquireNative(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	reader := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if err := c.Get(ctx, key, object, options...); err != nil {
				return err
			}
			if _, workload := object.(*appsv1.StatefulSet); workload && !stopped {
				stopped = true
				_, err := store.BeginNativeStop(ctx, ticket)
				return err
			}
			return nil
		},
	})
	handler, err := NewPodHandler(nativeAdmissionReader{reader}, f.handler.config)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Validate(ctx, podRequest(t, f.pod, admissionv1.Create, nil))
	if !stopped || response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "NativeReservationRequired") {
		t.Fatalf("reservation lost during validation still allowed the first Pod: %+v", response.Result)
	}
}

func TestNativePodReservationBlocksSelectedTeamPolicyMutation(t *testing.T) {
	ctx := context.Background()
	f, store, intent := coordinatedNativePodFixture(t)
	if _, err := store.AcquireNative(ctx, intent); err != nil {
		t.Fatal(err)
	}
	before := &api.Team{}
	if err := f.client.Get(ctx, client.ObjectKey{Name: intent.Sources[0].Name}, before); err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Spec.AgentPolicyRef = "other-policy"
	if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatalf("selected Team policy mutation was not blocked: %v", err)
	}
}

func TestNativeCoordinationPreservesDefaultAdmissionAndCleanup(t *testing.T) {
	ctx := context.Background()
	for _, native := range []bool{false, true} {
		f := newPodFixture(t)
		if native {
			f = nativePodFixture(t)
		}
		response := f.handler.Validate(ctx, podRequest(t, f.pod, admissionv1.Create, nil))
		if !response.Allowed {
			t.Fatalf("unconfigured native=%v admission changed: %+v", native, response.Result)
		}
	}
	legacy := newPodFixture(t)
	config := PodConfig{StateNamespace: legacy.state.Namespace, CoordinationConfigMapName: "absent", CoordinationConfigMapUID: "absent-uid"}
	reader := interceptor.NewClient(legacy.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if key.Name == "absent" {
				t.Error("non-native or cleanup admission consulted native coordination")
				return errors.New("unavailable native authority")
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	handler, err := NewPodHandler(nativeAdmissionReader{reader}, config)
	if err != nil {
		t.Fatal(err)
	}
	if response := handler.Validate(ctx, podRequest(t, legacy.pod, admissionv1.Create, nil)); !response.Allowed {
		t.Fatalf("configured native coordination changed non-native behavior: %+v", response.Result)
	}
	f := nativePodFixture(t)
	f.handler, err = NewPodHandler(nativeAdmissionReader{f.client}, config)
	if err != nil {
		t.Fatal(err)
	}
	pod := f.pod.DeepCopy()
	pod.UID = "first-pod-uid"
	if err := f.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.expected.ExecutionHeld, f.expected.PodUID = true, pod.UID
	saveNativePodExpectation(t, &f)
	pod.APIVersion, pod.Kind = "v1", "Pod"
	next := pod.DeepCopy()
	next.Finalizers = nil
	if response := f.handler.Validate(ctx, podRequest(t, next, admissionv1.Update, pod)); !response.Allowed {
		t.Fatalf("missing reservation stranded recorded native finalization: %+v", response.Result)
	}
	if response := f.handler.Validate(ctx, podRequest(t, pod, admissionv1.Delete, nil)); !response.Allowed {
		t.Fatalf("coordination stranded ordinary native Pod deletion: %+v", response.Result)
	}
}

func TestPodCoordinationConfigRequiresExactSeparateAuthority(t *testing.T) {
	for _, config := range []PodConfig{
		{CoordinationConfigMapName: "coordination"},
		{CoordinationConfigMapUID: "authority-uid"},
		{CoordinationConfigMapName: "Bad/Name", CoordinationConfigMapUID: "authority-uid"},
		{CoordinationConfigMapName: "coordination", CoordinationConfigMapUID: " uid "},
		{CoordinationConfigMapName: "coordination", CoordinationConfigMapUID: strings.Repeat("u", 129)},
		{CoordinationConfigMapName: "coordination", CoordinationConfigMapUID: "authority-uid", PlacementConfigMapName: "coordination"},
	} {
		config.StateNamespace = "operator-state"
		if _, err := NewPodHandler(&unavailablePodReader{}, config); err == nil {
			t.Fatal("invalid native authority was accepted")
		}
	}
	config := Config{
		ValidationMode: ValidationStrict, AdminUsers: []string{"admin@example.com"},
		ControllerUsername:        "system:serviceaccount:operator-state:controller",
		CoordinationConfigMapName: "coordination", CoordinationConfigMapUID: "authority-uid",
	}
	for _, namespace := range []string{"", "other-state"} {
		if err := Run(context.Background(), nil, config, ServerOptions{PodStateNamespace: namespace}); err == nil ||
			!strings.Contains(err.Error(), "coordination requires Pod admission") {
			t.Fatal("serving configuration could separate Pod and configuration coordination:", err)
		}
	}
}
